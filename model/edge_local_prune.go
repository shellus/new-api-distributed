package model

import (
	"errors"

	"gorm.io/gorm"
)

type EdgeLocalAccountingPruneResult struct {
	ThroughSequence  int64
	Reservations     int64
	UsageEvents      int64
	OutboxEntries    int64
	SettlementBlocks int64
}

func (result EdgeLocalAccountingPruneResult) DeletedRows() int64 {
	return result.Reservations + result.UsageEvents + result.OutboxEntries + result.SettlementBlocks
}

// PruneEdgeLocalAccountingHistory removes only history that is both accepted
// by master and already reflected in the local replicated-balance watermark.
// The newest retainEvents sequences stay available for local audit. Task-owned
// reservations remain as crash-recovery evidence.
func PruneEdgeLocalAccountingHistory(db *gorm.DB, retainEvents int64, batchSize int) (EdgeLocalAccountingPruneResult, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return EdgeLocalAccountingPruneResult{}, errors.New("edge local accounting pruning requires SQLite")
	}
	if retainEvents < 0 {
		return EdgeLocalAccountingPruneResult{}, errors.New("edge local accounting retention must not be negative")
	}
	if batchSize <= 0 {
		return EdgeLocalAccountingPruneResult{}, errors.New("edge local accounting prune batch size must be positive")
	}

	result := EdgeLocalAccountingPruneResult{}
	err := withEdgeLocalTransaction(db, "prune accounting history", func(tx *gorm.DB) error {
		var control EdgeLocalControlState
		if err := tx.First(&control, edgeLocalControlStateID).Error; err != nil {
			return err
		}
		safeThrough := min(control.LastAckedSequence, control.BalanceSettlementSequence)
		pruneThrough := safeThrough - retainEvents
		if pruneThrough <= 0 {
			return nil
		}

		attempt := EdgeLocalAccountingPruneResult{ThroughSequence: pruneThrough}
		var reservationIDs []string
		if err := tx.Model(&EdgeLocalQuotaReservation{}).
			Where("status = ? AND owner_kind = '' AND event_sequence > 0 AND event_sequence <= ?", EdgeLocalReservationStatusSettled, pruneThrough).
			Order("event_sequence asc").Limit(batchSize).Pluck("reservation_id", &reservationIDs).Error; err != nil {
			return err
		}
		if len(reservationIDs) > 0 {
			deleted := tx.Where("reservation_id IN ?", reservationIDs).Delete(&EdgeLocalQuotaReservation{})
			if deleted.Error != nil {
				return deleted.Error
			}
			attempt.Reservations = deleted.RowsAffected
		}

		var usageEventIDs []string
		if err := tx.Model(&EdgeLocalUsageEvent{}).
			Where("acknowledged = ? AND sequence <= ?", true, pruneThrough).
			Order("sequence asc").Limit(batchSize).Pluck("event_id", &usageEventIDs).Error; err != nil {
			return err
		}
		if len(usageEventIDs) > 0 {
			deleted := tx.Where("event_id IN ?", usageEventIDs).Delete(&EdgeLocalUsageEvent{})
			if deleted.Error != nil {
				return deleted.Error
			}
			attempt.UsageEvents = deleted.RowsAffected
		}

		var outboxIDs []int64
		if err := tx.Model(&EdgeLocalOutbox{}).
			Where("status = ? AND sequence <= ?", EdgeLocalOutboxStatusAcked, pruneThrough).
			Order("sequence asc").Limit(batchSize).Pluck("id", &outboxIDs).Error; err != nil {
			return err
		}
		if len(outboxIDs) > 0 {
			deleted := tx.Where("id IN ?", outboxIDs).Delete(&EdgeLocalOutbox{})
			if deleted.Error != nil {
				return deleted.Error
			}
			attempt.OutboxEntries = deleted.RowsAffected
		}

		var blockIDs []string
		if err := tx.Model(&EdgeLocalSettlementBlock{}).
			Where("status = ? AND last_sequence <= ?", EdgeLocalSettlementBlockStatusAcked, pruneThrough).
			Order("last_sequence asc").Limit(batchSize).Pluck("block_id", &blockIDs).Error; err != nil {
			return err
		}
		if len(blockIDs) > 0 {
			deleted := tx.Where("block_id IN ?", blockIDs).Delete(&EdgeLocalSettlementBlock{})
			if deleted.Error != nil {
				return deleted.Error
			}
			attempt.SettlementBlocks = deleted.RowsAffected
		}

		result = attempt
		return nil
	})
	return result, err
}
