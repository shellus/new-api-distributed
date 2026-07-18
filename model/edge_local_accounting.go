package model

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgesettlement"

	"gorm.io/gorm"
)

// StageEdgeLocalReservationSettlement persists the exact metered usage before
// mutating balance overlays. A staged reservation cannot be refunded, so a crash
// or a transient settlement failure can never silently turn completed usage
// into free usage.
func StageEdgeLocalReservationSettlement(db *gorm.DB, reservationID string, event dto.EdgeUsageEventV1) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge local settlement staging requires SQLite")
	}
	if err := validateEdgeLocalIdentifier(reservationID); err != nil {
		return err
	}
	if err := validateEdgeLocalIdentifier(event.EventID); err != nil {
		return fmt.Errorf("event ID: %w", err)
	}
	chargedQuota := event.Billing.ChargedQuota
	if err := validateEdgeLocalQuota(chargedQuota, true); err != nil {
		return err
	}
	if event.FinishedAtUnixMilli <= 0 {
		return errors.New("usage event finish time must be positive")
	}
	canonicalizeEdgeLocalStagedUsageEvent(&event)
	payload, err := common.Marshal(event)
	if err != nil {
		return err
	}

	return db.Transaction(func(tx *gorm.DB) error {
		var reservation EdgeLocalQuotaReservation
		if err := tx.Where("reservation_id = ?", reservationID).First(&reservation).Error; err != nil {
			return err
		}
		switch reservation.Status {
		case EdgeLocalReservationStatusRefunded:
			return ErrEdgeLocalReservationFinalized
		case EdgeLocalReservationStatusSettled:
			var stored EdgeLocalUsageEvent
			if err := tx.Where("reservation_id = ?", reservationID).First(&stored).Error; err != nil {
				return ErrEdgeLocalAccountingCorruption
			}
			candidate := event
			normalizeEdgeLocalUsageEvent(&candidate, reservation, stored.Sequence)
			candidatePayload, err := common.Marshal(candidate)
			if err != nil {
				return err
			}
			if !bytes.Equal(candidatePayload, []byte(stored.Payload)) {
				return ErrEdgeLocalSettlementConflict
			}
			return nil
		case EdgeLocalReservationStatusActive:
		default:
			return ErrEdgeLocalAccountingCorruption
		}

		validationReservation := reservation
		if chargedQuota > validationReservation.ReservedQuota {
			validationReservation.ReservedQuota = chargedQuota
		}
		candidate := event
		normalizeEdgeLocalUsageEvent(&candidate, validationReservation, 1)
		if err := candidate.Validate(); err != nil {
			return err
		}
		staged, err := edgeLocalReservationSettlementStaged(reservation)
		if err != nil {
			return err
		}
		if staged {
			if reservation.StagedEventID != event.EventID || !bytes.Equal([]byte(reservation.StagedEventPayload), payload) {
				return ErrEdgeLocalSettlementConflict
			}
			return nil
		}
		result := tx.Model(&EdgeLocalQuotaReservation{}).
			Where("reservation_id = ? AND status = ? AND staged_event_payload = ''", reservationID, EdgeLocalReservationStatusActive).
			Updates(map[string]any{
				"staged_event_id":       event.EventID,
				"staged_event_payload":  string(payload),
				"staged_at_unix_milli":  event.FinishedAtUnixMilli,
				"updated_at_unix_milli": event.FinishedAtUnixMilli,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalReservationConflict
		}
		return nil
	})
}

// SettleEdgeLocalReservation first stages the exact usage payload, then
// finalizes the reservation and appends its immutable usage event and outbox
// entry in one SQLite transaction. If finalization fails, the staged payload
// remains durable for recovery.
func SettleEdgeLocalReservation(db *gorm.DB, reservationID string, event dto.EdgeUsageEventV1) (*dto.EdgeUsageEventV1, error) {
	if err := StageEdgeLocalReservationSettlement(db, reservationID, event); err != nil {
		return nil, err
	}
	return SettleStagedEdgeLocalReservation(db, reservationID)
}

// SettleStagedEdgeLocalReservation retries finalization using only the payload
// already committed to the edge-local database.
func SettleStagedEdgeLocalReservation(db *gorm.DB, reservationID string) (*dto.EdgeUsageEventV1, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local settlement requires SQLite")
	}
	if err := validateEdgeLocalIdentifier(reservationID); err != nil {
		return nil, err
	}

	var settled *dto.EdgeUsageEventV1
	err := db.Transaction(func(tx *gorm.DB) error {
		var reservation EdgeLocalQuotaReservation
		if err := tx.Where("reservation_id = ?", reservationID).First(&reservation).Error; err != nil {
			return err
		}
		if reservation.Status == EdgeLocalReservationStatusRefunded {
			return ErrEdgeLocalReservationFinalized
		}
		if reservation.Status == EdgeLocalReservationStatusSettled {
			var stored EdgeLocalUsageEvent
			if err := tx.Where("reservation_id = ?", reservationID).First(&stored).Error; err != nil {
				return ErrEdgeLocalAccountingCorruption
			}
			var durable dto.EdgeUsageEventV1
			if err := common.Unmarshal([]byte(stored.Payload), &durable); err != nil {
				return err
			}
			if err := durable.Validate(); err != nil {
				return ErrEdgeLocalAccountingCorruption
			}
			settled = &durable
			return nil
		}
		if reservation.Status != EdgeLocalReservationStatusActive {
			return ErrEdgeLocalAccountingCorruption
		}
		staged, err := edgeLocalReservationSettlementStaged(reservation)
		if err != nil {
			return err
		}
		if !staged {
			return ErrEdgeLocalSettlementStaged
		}
		var event dto.EdgeUsageEventV1
		if err := common.Unmarshal([]byte(reservation.StagedEventPayload), &event); err != nil {
			return ErrEdgeLocalAccountingCorruption
		}
		canonicalizeEdgeLocalStagedUsageEvent(&event)
		if event.EventID != reservation.StagedEventID {
			return ErrEdgeLocalAccountingCorruption
		}
		chargedQuota := event.Billing.ChargedQuota
		if err := validateEdgeLocalQuota(chargedQuota, true); err != nil {
			return ErrEdgeLocalAccountingCorruption
		}
		originalReservedQuota := reservation.ReservedQuota
		var control EdgeLocalControlState
		if err := tx.First(&control, edgeLocalControlStateID).Error; err != nil {
			return err
		}
		if control.NextEventSequence <= 0 || control.NextEventSequence == int64(^uint64(0)>>1) {
			return ErrEdgeLocalAccountingCorruption
		}
		sequence := control.NextEventSequence
		normalizeEdgeLocalUsageEvent(&event, reservation, sequence)
		if err := event.Validate(); err != nil {
			return err
		}
		payload, err := common.Marshal(event)
		if err != nil {
			return err
		}

		if err := settleStagedEdgeLocalBalanceReservationTx(tx, &reservation, &event); err != nil {
			return err
		}
		result := tx.Model(&EdgeLocalQuotaReservation{}).
			Where("reservation_id = ? AND status = ?", reservationID, EdgeLocalReservationStatusActive).
			Updates(map[string]any{
				"status":                  EdgeLocalReservationStatusSettled,
				"reserved_quota":          originalReservedQuota,
				"charged_quota":           chargedQuota,
				"event_id":                event.EventID,
				"event_sequence":          sequence,
				"updated_at_unix_milli":   event.FinishedAtUnixMilli,
				"finalized_at_unix_milli": event.FinishedAtUnixMilli,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalReservationConflict
		}
		storedEvent := EdgeLocalUsageEvent{
			EventID: event.EventID, Sequence: sequence,
			ReservationID: reservation.ReservationID, RequestID: reservation.RequestID,
			Payload: string(payload), CreatedAtUnixMilli: event.FinishedAtUnixMilli,
		}
		if err := tx.Create(&storedEvent).Error; err != nil {
			return err
		}
		outbox := EdgeLocalOutbox{
			EventID: event.EventID, Sequence: sequence, Status: EdgeLocalOutboxStatusPending,
			Payload: string(payload), CreatedAtUnixMilli: event.FinishedAtUnixMilli,
			UpdatedAtUnixMilli: event.FinishedAtUnixMilli,
		}
		if err := tx.Create(&outbox).Error; err != nil {
			return err
		}
		result = tx.Model(&EdgeLocalControlState{}).Where("id = ? AND next_event_sequence = ?", edgeLocalControlStateID, sequence).
			Updates(map[string]any{"next_event_sequence": sequence + 1, "updated_at_unix_milli": event.FinishedAtUnixMilli})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalSettlementOutOfOrder
		}
		settled = &event
		return nil
	})
	if err != nil {
		return nil, err
	}
	return settled, nil
}

// ListStagedEdgeLocalReservationIDs returns active settlements in durable
// staging order so background recovery can retry deterministically.
func ListStagedEdgeLocalReservationIDs(db *gorm.DB, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, errors.New("edge local staged settlement limit must be positive")
	}
	return listStagedEdgeLocalReservationIDs(db, limit)
}

// ListAllStagedEdgeLocalReservationIDs returns the complete active recovery
// frontier. Admission is already closed while it is used, so collecting the
// finite in-flight frontier avoids a blocked early page hiding a later event
// that would return quota needed by that page.
func ListAllStagedEdgeLocalReservationIDs(db *gorm.DB) ([]string, error) {
	return listStagedEdgeLocalReservationIDs(db, 0)
}

func listStagedEdgeLocalReservationIDs(db *gorm.DB, limit int) ([]string, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local staged settlement query requires SQLite")
	}
	var reservations []EdgeLocalQuotaReservation
	query := db.Select("reservation_id", "staged_event_id", "staged_event_payload", "staged_at_unix_milli").
		Where("status = ? AND (staged_event_id <> '' OR staged_event_payload <> '' OR staged_at_unix_milli <> 0)", EdgeLocalReservationStatusActive).
		Order("staged_at_unix_milli asc, reservation_id asc")
	if limit > 0 {
		query = query.Limit(limit)
	}
	if err := query.Find(&reservations).Error; err != nil {
		return nil, err
	}
	ids := make([]string, len(reservations))
	for index := range reservations {
		staged, err := edgeLocalReservationSettlementStaged(reservations[index])
		if err != nil {
			return nil, err
		}
		if !staged {
			return nil, ErrEdgeLocalAccountingCorruption
		}
		ids[index] = reservations[index].ReservationID
	}
	return ids, nil
}

func edgeLocalReservationSettlementStaged(reservation EdgeLocalQuotaReservation) (bool, error) {
	hasID := reservation.StagedEventID != ""
	hasPayload := reservation.StagedEventPayload != ""
	hasTime := reservation.StagedAtUnixMilli > 0
	if !hasID && !hasPayload && reservation.StagedAtUnixMilli == 0 {
		return false, nil
	}
	if hasID && hasPayload && hasTime {
		return true, nil
	}
	return false, ErrEdgeLocalAccountingCorruption
}

func canonicalizeEdgeLocalStagedUsageEvent(event *dto.EdgeUsageEventV1) {
	event.Sequence = 0
	event.ReservationID = ""
	event.RequestID = ""
	event.UserID = 0
	event.TokenID = 0
	event.SnapshotID = ""
	event.SnapshotRevision = 0
	event.PricingRevision = 0
	event.BalanceRevision = 0
	event.FundingSource = ""
	event.UserSubscriptionID = 0
	event.TokenUnlimitedQuota = false
	event.Billing.ReservedQuota = 0
}

func normalizeEdgeLocalUsageEvent(event *dto.EdgeUsageEventV1, reservation EdgeLocalQuotaReservation, sequence int64) {
	event.Sequence = sequence
	event.ReservationID = reservation.ReservationID
	event.RequestID = reservation.RequestID
	event.UserID = reservation.UserID
	event.TokenID = reservation.TokenID
	event.SnapshotID = reservation.SnapshotID
	event.SnapshotRevision = reservation.SnapshotRevision
	event.PricingRevision = reservation.PricingRevision
	event.BalanceRevision = reservation.BalanceRevision
	event.FundingSource, event.UserSubscriptionID, _ = edgeLocalBalanceFundingSource(reservation)
	event.TokenUnlimitedQuota = reservation.TokenUnlimitedQuota
	event.Billing.ReservedQuota = reservation.ReservedQuota
}

// BuildEdgeLocalSettlementBlock returns the already-durable pending block on
// retry. Otherwise it atomically assigns the earliest contiguous events to a
// new block and persists the exact request payload before returning it.
func BuildEdgeLocalSettlementBlock(
	db *gorm.DB,
	meta dto.EdgeControlRequestMetaV1,
	blockID string,
	maxEvents int,
	createdAtUnixMilli int64,
	settlementCircuitEpoch int64,
) (*dto.EdgeSettlementBlockRequestV1, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local settlement block requires SQLite")
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	if err := validateEdgeLocalIdentifier(blockID); err != nil {
		return nil, fmt.Errorf("block ID: %w", err)
	}
	if maxEvents <= 0 || maxEvents > dto.EdgeControlMaxSettlementEventsV1 {
		return nil, fmt.Errorf("settlement block event limit must be between 1 and %d", dto.EdgeControlMaxSettlementEventsV1)
	}
	if createdAtUnixMilli <= 0 {
		return nil, errors.New("settlement block creation time must be positive")
	}
	if settlementCircuitEpoch < 0 {
		return nil, errors.New("settlement circuit epoch must not be negative")
	}

	var request *dto.EdgeSettlementBlockRequestV1
	err := db.Transaction(func(tx *gorm.DB) error {
		var pending []EdgeLocalSettlementBlock
		if err := tx.Where("status = ?", EdgeLocalSettlementBlockStatusPending).Order("first_sequence asc").Limit(2).Find(&pending).Error; err != nil {
			return err
		}
		if len(pending) > 1 {
			return ErrEdgeLocalAccountingCorruption
		}
		if len(pending) == 1 {
			var durable dto.EdgeSettlementBlockRequestV1
			if err := common.Unmarshal([]byte(pending[0].Payload), &durable); err != nil {
				return err
			}
			if err := durable.Validate(); err != nil {
				return ErrEdgeLocalAccountingCorruption
			}
			request = &durable
			return nil
		}

		var control EdgeLocalControlState
		if err := tx.First(&control, edgeLocalControlStateID).Error; err != nil {
			return err
		}
		var storedEvents []EdgeLocalUsageEvent
		if err := tx.Where("acknowledged = ? AND block_id = ?", false, "").Order("sequence asc").Limit(maxEvents).Find(&storedEvents).Error; err != nil {
			return err
		}
		if len(storedEvents) == 0 {
			return ErrEdgeLocalNoPendingUsageEvents
		}
		expectedFirst := control.LastAckedSequence + 1
		if storedEvents[0].Sequence != expectedFirst {
			return ErrEdgeLocalSettlementOutOfOrder
		}
		events := make([]dto.EdgeUsageEventV1, 0, len(storedEvents))
		for i := range storedEvents {
			expected := expectedFirst + int64(i)
			if storedEvents[i].Sequence != expected {
				return ErrEdgeLocalSettlementOutOfOrder
			}
			var event dto.EdgeUsageEventV1
			if err := common.Unmarshal([]byte(storedEvents[i].Payload), &event); err != nil {
				return err
			}
			if event.Sequence != storedEvents[i].Sequence || event.EventID != storedEvents[i].EventID || event.FinishedAtUnixMilli > createdAtUnixMilli {
				return ErrEdgeLocalAccountingCorruption
			}
			events = append(events, event)
		}
		if control.NodeID == "" || control.NodeGeneration <= 0 {
			return ErrEdgeLocalAccountingCorruption
		}

		built := dto.EdgeSettlementBlockRequestV1{
			Meta: meta, BlockID: blockID,
			PreviousBlockID: control.LastAckedBlockID, PreviousBlockDigest: control.LastAckedBlockDigest,
			FirstSequence: events[0].Sequence, LastSequence: events[len(events)-1].Sequence,
			CreatedAtUnixMilli: createdAtUnixMilli, Events: events,
		}
		if err := edgesettlement.SetBlockDigestV1(control.NodeID, control.NodeGeneration, &built); err != nil {
			return err
		}
		if err := built.Validate(); err != nil {
			return err
		}
		payload, err := common.Marshal(built)
		if err != nil {
			return err
		}
		block := EdgeLocalSettlementBlock{
			BlockID: blockID, NodeID: control.NodeID, NodeGeneration: control.NodeGeneration,
			PreviousBlockID: built.PreviousBlockID, PreviousBlockDigest: built.PreviousBlockDigest,
			FirstSequence: built.FirstSequence, LastSequence: built.LastSequence, EventCount: len(events),
			BlockDigest: built.BlockDigest, Status: EdgeLocalSettlementBlockStatusPending,
			RequestCircuitEpoch: settlementCircuitEpoch,
			Payload:             string(payload), CreatedAtUnixMilli: createdAtUnixMilli,
		}
		if err := tx.Create(&block).Error; err != nil {
			return err
		}
		result := tx.Model(&EdgeLocalUsageEvent{}).
			Where("sequence >= ? AND sequence <= ? AND acknowledged = ? AND block_id = ?", built.FirstSequence, built.LastSequence, false, "").
			Update("block_id", blockID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(len(events)) {
			return ErrEdgeLocalSettlementConflict
		}
		result = tx.Model(&EdgeLocalOutbox{}).
			Where("sequence >= ? AND sequence <= ? AND status = ?", built.FirstSequence, built.LastSequence, EdgeLocalOutboxStatusPending).
			Updates(map[string]any{
				"block_id": blockID, "status": EdgeLocalOutboxStatusInBlock,
				"updated_at_unix_milli": createdAtUnixMilli,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(len(events)) {
			return ErrEdgeLocalSettlementConflict
		}
		request = &built
		return nil
	})
	if err != nil {
		return nil, err
	}
	return request, nil
}

func RefreshEdgeLocalSettlementRequest(
	db *gorm.DB,
	blockID string,
	meta dto.EdgeControlRequestMetaV1,
	settlementCircuitEpoch int64,
) (*dto.EdgeSettlementBlockRequestV1, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local settlement request refresh requires SQLite")
	}
	if err := validateEdgeLocalIdentifier(blockID); err != nil {
		return nil, err
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	if settlementCircuitEpoch < 0 {
		return nil, errors.New("settlement circuit epoch must not be negative")
	}
	var refreshed *dto.EdgeSettlementBlockRequestV1
	err := db.Transaction(func(tx *gorm.DB) error {
		var block EdgeLocalSettlementBlock
		if err := tx.Where("block_id = ?", blockID).First(&block).Error; err != nil {
			return err
		}
		if block.Status != EdgeLocalSettlementBlockStatusPending {
			return ErrEdgeLocalSettlementConflict
		}
		var request dto.EdgeSettlementBlockRequestV1
		if err := common.UnmarshalJsonStr(block.Payload, &request); err != nil {
			return ErrEdgeLocalAccountingCorruption
		}
		if settlementCircuitEpoch <= block.RequestCircuitEpoch {
			refreshed = &request
			return nil
		}
		request.Meta = meta
		if err := request.Validate(); err != nil {
			return err
		}
		digest, err := edgesettlement.DigestBlockV1(block.NodeID, block.NodeGeneration, request)
		if err != nil {
			return err
		}
		if digest != block.BlockDigest || digest != request.BlockDigest {
			return ErrEdgeLocalAccountingCorruption
		}
		payload, err := common.Marshal(request)
		if err != nil {
			return err
		}
		result := tx.Model(&EdgeLocalSettlementBlock{}).
			Where("block_id = ? AND status = ? AND request_circuit_epoch < ?", blockID, EdgeLocalSettlementBlockStatusPending, settlementCircuitEpoch).
			Updates(map[string]any{"payload": string(payload), "request_circuit_epoch": settlementCircuitEpoch})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalSettlementConflict
		}
		refreshed = &request
		return nil
	})
	if err != nil {
		return nil, err
	}
	return refreshed, nil
}

func AcknowledgeEdgeLocalSettlementBlock(db *gorm.DB, ack dto.EdgeSettlementAckV1) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge local settlement acknowledgement requires SQLite")
	}
	if err := ack.Validate(); err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var block EdgeLocalSettlementBlock
		if err := tx.Where("block_id = ?", ack.BlockID).First(&block).Error; err != nil {
			return err
		}
		if block.NodeID != ack.NodeID || block.NodeGeneration != ack.NodeGeneration ||
			block.LastSequence != ack.AckedThroughSequence || ack.NextExpectedSequence != block.LastSequence+1 ||
			ack.AcceptedEventCount != block.EventCount {
			return ErrEdgeLocalSettlementConflict
		}
		if block.Status == EdgeLocalSettlementBlockStatusAcked {
			return nil
		}
		if block.Status != EdgeLocalSettlementBlockStatusPending {
			return ErrEdgeLocalAccountingCorruption
		}
		var control EdgeLocalControlState
		if err := tx.First(&control, edgeLocalControlStateID).Error; err != nil {
			return err
		}
		if control.LastAckedSequence+1 != block.FirstSequence || control.LastAckedBlockID != block.PreviousBlockID ||
			control.LastAckedBlockDigest != block.PreviousBlockDigest {
			return ErrEdgeLocalSettlementOutOfOrder
		}
		ackPayload, err := common.Marshal(ack)
		if err != nil {
			return err
		}
		result := tx.Model(&EdgeLocalUsageEvent{}).
			Where("block_id = ? AND sequence >= ? AND sequence <= ? AND acknowledged = ?", block.BlockID, block.FirstSequence, block.LastSequence, false).
			Updates(map[string]any{"acknowledged": true, "acknowledged_at_unix_milli": ack.AcknowledgedAtUnixMilli})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(block.EventCount) {
			return ErrEdgeLocalSettlementConflict
		}
		result = tx.Model(&EdgeLocalOutbox{}).
			Where("block_id = ? AND status = ?", block.BlockID, EdgeLocalOutboxStatusInBlock).
			Updates(map[string]any{
				"status": EdgeLocalOutboxStatusAcked, "updated_at_unix_milli": ack.AcknowledgedAtUnixMilli,
				"acknowledged_at_unix_milli": ack.AcknowledgedAtUnixMilli,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(block.EventCount) {
			return ErrEdgeLocalSettlementConflict
		}
		result = tx.Model(&EdgeLocalSettlementBlock{}).
			Where("block_id = ? AND status = ?", block.BlockID, EdgeLocalSettlementBlockStatusPending).
			Updates(map[string]any{
				"status": EdgeLocalSettlementBlockStatusAcked, "ack_payload": string(ackPayload),
				"acknowledged_at_unix_milli": ack.AcknowledgedAtUnixMilli,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalSettlementConflict
		}
		result = tx.Model(&EdgeLocalControlState{}).
			Where("id = ? AND last_acked_sequence = ?", edgeLocalControlStateID, control.LastAckedSequence).
			Updates(map[string]any{
				"last_acked_sequence":     block.LastSequence,
				"last_acked_block_id":     block.BlockID,
				"last_acked_block_digest": block.BlockDigest,
				"updated_at_unix_milli":   ack.AcknowledgedAtUnixMilli,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalSettlementOutOfOrder
		}
		return nil
	})
}

func GetEdgeLocalPendingSettlementBlock(db *gorm.DB) (*dto.EdgeSettlementBlockRequestV1, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local settlement query requires SQLite")
	}
	var block EdgeLocalSettlementBlock
	if err := db.Where("status = ?", EdgeLocalSettlementBlockStatusPending).Order("first_sequence asc").First(&block).Error; err != nil {
		return nil, err
	}
	var request dto.EdgeSettlementBlockRequestV1
	if err := common.Unmarshal([]byte(block.Payload), &request); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, ErrEdgeLocalAccountingCorruption
	}
	return &request, nil
}

func GetEdgeLocalSettlementState(db *gorm.DB) (*dto.EdgeSettlementStateV1, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local settlement state requires SQLite")
	}
	var control EdgeLocalControlState
	if err := db.First(&control, edgeLocalControlStateID).Error; err != nil {
		return nil, err
	}
	var pendingEvents int64
	if err := db.Model(&EdgeLocalUsageEvent{}).Where("acknowledged = ?", false).Count(&pendingEvents).Error; err != nil {
		return nil, err
	}
	var pendingBlocks int64
	if err := db.Model(&EdgeLocalSettlementBlock{}).Where("status = ?", EdgeLocalSettlementBlockStatusPending).Count(&pendingBlocks).Error; err != nil {
		return nil, err
	}
	var oldest int64
	if pendingEvents > 0 {
		if err := db.Model(&EdgeLocalUsageEvent{}).Where("acknowledged = ?", false).Select("MIN(created_at_unix_milli)").Scan(&oldest).Error; err != nil {
			return nil, err
		}
	}
	return &dto.EdgeSettlementStateV1{
		LastAckedSequence:      control.LastAckedSequence,
		LastAckedBlockID:       control.LastAckedBlockID,
		NextEventSequence:      control.NextEventSequence,
		PendingEventCount:      pendingEvents,
		PendingBlockCount:      pendingBlocks,
		OldestPendingUnixMilli: oldest,
	}, nil
}
