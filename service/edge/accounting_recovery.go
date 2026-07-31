package edge

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
)

var errEdgeAccountingRecoveryBlocked = errors.New("edge accounting recovery requires operator intervention")

// InitializeEdgeAccountingReadiness restores the fail-closed accounting gate
// from durable local state before the edge starts accepting requests.
func InitializeEdgeAccountingReadiness(ctx context.Context, db *gorm.DB) error {
	edgeAccountingReady.Store(false)
	edgeAccountingBlock.Store(false)
	edgeAccountingQuarantine.reset()
	if db == nil {
		edgeAccountingBlock.Store(true)
		return errors.New("edge accounting database is unavailable")
	}
	ids, err := model.ListStagedEdgeLocalReservationIDs(db.WithContext(ctx), 1)
	if err != nil {
		edgeAccountingBlock.Store(true)
		return err
	}
	var orphanedReservations []model.EdgeLocalQuotaReservation
	if err := db.WithContext(ctx).
		Where("status = ? AND owner_kind = '' AND staged_event_id = '' AND staged_event_payload = '' AND staged_at_unix_milli = 0",
			model.EdgeLocalReservationStatusActive).
		Order("created_at_unix_milli asc, reservation_id asc").Find(&orphanedReservations).Error; err != nil {
		edgeAccountingBlock.Store(true)
		return err
	}
	for _, reservation := range orphanedReservations {
		if err := edgeAccountingQuarantine.add(reservation); err != nil {
			edgeAccountingBlock.Store(true)
			return err
		}
	}
	if len(orphanedReservations) > 0 {
		common.SysError(fmt.Sprintf("edge accounting quarantined %d orphaned reservations; unaffected subjects remain available", len(orphanedReservations)))
	}
	if len(ids) == 0 {
		edgeAccountingReady.Store(true)
	}
	return nil
}

// MarkEdgeAccountingFailure closes admission after a completed upstream request
// could not be finalized locally. Recoverable failures have a durable staged
// event; failures before staging stay latched until restart/operator review.
func MarkEdgeAccountingFailure(recoverable bool) {
	edgeAccountingReady.Store(false)
	if !recoverable {
		edgeAccountingBlock.Store(true)
	}
}

// MarkEdgeAccountingReservationFailure isolates an unstaged, unknowable
// request to its user and token. If the reservation identity is unavailable,
// the safer global accounting block remains in force.
func MarkEdgeAccountingReservationFailure(recoverable bool, reservation *model.EdgeLocalQuotaReservation) {
	if recoverable {
		MarkEdgeAccountingFailure(true)
		return
	}
	if reservation == nil || edgeAccountingQuarantine.addManual(*reservation, false) != nil {
		MarkEdgeAccountingFailure(false)
		return
	}
	common.SysError(fmt.Sprintf(
		"edge accounting quarantined reservation %s for user %d token %d; operator review is required",
		reservation.ReservationID, reservation.UserID, reservation.TokenID,
	))
}

// QuarantineEdgeTaskReservation isolates a single task subject. Missing
// reservation references remain quarantined until restart/operator repair;
// existing reservation rows clear automatically after reaching a terminal
// accounting state.
func QuarantineEdgeTaskReservation(
	_ context.Context,
	reservation *model.EdgeLocalQuotaReservation,
	retainWhenMissing bool,
	cause error,
) {
	if reservation == nil || edgeAccountingQuarantine.addManual(*reservation, retainWhenMissing) != nil {
		MarkEdgeAccountingFailure(false)
		return
	}
	reason := "unknown task accounting anomaly"
	if cause != nil {
		reason = cause.Error()
	}
	common.SysError(fmt.Sprintf(
		"edge task accounting quarantined reservation %s for user %d token %d: %s",
		reservation.ReservationID, reservation.UserID, reservation.TokenID, reason,
	))
}

// RecoverEdgeStagedSettlements drains the finite durable recovery frontier in
// deterministic order. Admission reopens only after no staged reservation remains.
func RecoverEdgeStagedSettlements(ctx context.Context, db *gorm.DB) error {
	if edgeAccountingBlock.Load() {
		return errEdgeAccountingRecoveryBlocked
	}
	if db == nil {
		MarkEdgeAccountingFailure(false)
		return errors.New("edge accounting database is unavailable")
	}
	edgeAccountingReady.Store(false)
	for {
		ids, err := model.ListAllStagedEdgeLocalReservationIDs(db.WithContext(ctx))
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			edgeAccountingReady.Store(true)
			return nil
		}
		settledAny := false
		var quotaBlockedErr error
		for _, reservationID := range ids {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := model.SettleStagedEdgeLocalReservation(db.WithContext(ctx), reservationID); err != nil {
				wrapped := fmt.Errorf("recover staged edge reservation %s: %w", reservationID, err)
				if errors.Is(err, model.ErrEdgeLocalQuotaInsufficient) {
					if quotaBlockedErr == nil {
						quotaBlockedErr = wrapped
					}
					continue
				}
				if edgeAccountingRecoveryRequiresIntervention(err) {
					MarkEdgeAccountingFailure(false)
				}
				return wrapped
			}
			settledAny = true
		}
		if settledAny {
			// A later reservation may have released balance overlay that an
			// earlier staged settlement needs. Re-scan from the deterministic
			// head before declaring recovery blocked.
			continue
		}
		if quotaBlockedErr != nil {
			return quotaBlockedErr
		}
	}
}

func edgeAccountingRecoveryRequiresIntervention(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound) ||
		errors.Is(err, model.ErrEdgeLocalSnapshotMismatch) ||
		errors.Is(err, model.ErrEdgeLocalReservationConflict) ||
		errors.Is(err, model.ErrEdgeLocalReservationFinalized) ||
		errors.Is(err, model.ErrEdgeLocalSettlementStaged) ||
		errors.Is(err, model.ErrEdgeLocalSettlementConflict) ||
		errors.Is(err, model.ErrEdgeLocalSettlementOutOfOrder) ||
		errors.Is(err, model.ErrEdgeLocalAccountingCorruption)
}

func refreshEdgeAccountingReadiness(db *gorm.DB) {
	if db == nil || edgeAccountingBlock.Load() {
		return
	}
	ids, err := model.ListStagedEdgeLocalReservationIDs(db, 1)
	if err != nil {
		MarkEdgeAccountingFailure(false)
		return
	}
	edgeAccountingReady.Store(len(ids) == 0)
}
