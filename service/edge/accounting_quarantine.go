package edge

import (
	"context"
	"errors"
	"sync"

	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
)

var errEdgeAccountingSubjectQuarantined = errors.New("edge accounting subject requires operator review")

var edgeAccountingQuarantine = newEdgeAccountingQuarantineState()

type edgeAccountingQuarantineSubject struct {
	userID  int64
	tokenID int64
}

type edgeAccountingQuarantineState struct {
	mu           sync.RWMutex
	reservations map[string]edgeAccountingQuarantineSubject
}

func newEdgeAccountingQuarantineState() *edgeAccountingQuarantineState {
	return &edgeAccountingQuarantineState{reservations: make(map[string]edgeAccountingQuarantineSubject)}
}

func (state *edgeAccountingQuarantineState) reset() {
	state.mu.Lock()
	state.reservations = make(map[string]edgeAccountingQuarantineSubject)
	state.mu.Unlock()
}

func (state *edgeAccountingQuarantineState) add(reservation model.EdgeLocalQuotaReservation) error {
	if reservation.ReservationID == "" || reservation.UserID <= 0 || reservation.TokenID <= 0 {
		return errors.New("edge accounting quarantine reservation identity is incomplete")
	}
	state.mu.Lock()
	state.reservations[reservation.ReservationID] = edgeAccountingQuarantineSubject{
		userID: reservation.UserID, tokenID: reservation.TokenID,
	}
	state.mu.Unlock()
	return nil
}

func (state *edgeAccountingQuarantineState) blocked(userID, tokenID int64) bool {
	state.mu.RLock()
	defer state.mu.RUnlock()
	for _, subject := range state.reservations {
		if (userID > 0 && subject.userID == userID) || (tokenID > 0 && subject.tokenID == tokenID) {
			return true
		}
	}
	return false
}

func (state *edgeAccountingQuarantineState) count() int {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return len(state.reservations)
}

func (state *edgeAccountingQuarantineState) reservationIDs() []string {
	state.mu.RLock()
	defer state.mu.RUnlock()
	ids := make([]string, 0, len(state.reservations))
	for reservationID := range state.reservations {
		ids = append(ids, reservationID)
	}
	return ids
}

func (state *edgeAccountingQuarantineState) reconcile(reservationIDs []string, reservations []model.EdgeLocalQuotaReservation) (bool, error) {
	retained := make(map[string]edgeAccountingQuarantineSubject, len(reservations))
	stagedFound := false
	for _, reservation := range reservations {
		requiresQuarantine, staged, err := edgeReservationAccountingQuarantineState(reservation)
		if err != nil {
			return false, err
		}
		if staged {
			stagedFound = true
		}
		if !requiresQuarantine {
			continue
		}
		if reservation.ReservationID == "" || reservation.UserID <= 0 || reservation.TokenID <= 0 {
			return false, errors.New("edge accounting quarantine reservation identity is incomplete")
		}
		retained[reservation.ReservationID] = edgeAccountingQuarantineSubject{
			userID: reservation.UserID, tokenID: reservation.TokenID,
		}
	}
	state.mu.Lock()
	for _, reservationID := range reservationIDs {
		delete(state.reservations, reservationID)
	}
	for reservationID, subject := range retained {
		state.reservations[reservationID] = subject
	}
	state.mu.Unlock()
	return stagedFound, nil
}

func edgeReservationAccountingQuarantineState(reservation model.EdgeLocalQuotaReservation) (bool, bool, error) {
	if reservation.Status != model.EdgeLocalReservationStatusActive || reservation.OwnerKind != "" {
		return false, false, nil
	}
	hasStagedID := reservation.StagedEventID != ""
	hasStagedPayload := reservation.StagedEventPayload != ""
	hasStagedTime := reservation.StagedAtUnixMilli > 0
	if !hasStagedID && !hasStagedPayload && reservation.StagedAtUnixMilli == 0 {
		return true, false, nil
	}
	if hasStagedID && hasStagedPayload && hasStagedTime {
		return false, true, nil
	}
	return false, false, model.ErrEdgeLocalAccountingCorruption
}

func EdgeAccountingSubjectQuarantined(userID, tokenID int64) bool {
	return edgeAccountingQuarantine.blocked(userID, tokenID)
}

func EdgeAccountingQuarantinedReservationCount() int {
	return edgeAccountingQuarantine.count()
}

func ReconcileEdgeAccountingQuarantine(ctx context.Context, db *gorm.DB) error {
	if ctx == nil {
		return errors.New("edge accounting quarantine context is nil")
	}
	if db == nil {
		return errors.New("edge accounting database is unavailable")
	}
	reservationIDs := edgeAccountingQuarantine.reservationIDs()
	if len(reservationIDs) == 0 {
		return nil
	}
	var reservations []model.EdgeLocalQuotaReservation
	if err := db.WithContext(ctx).Where("reservation_id IN ?", reservationIDs).Find(&reservations).Error; err != nil {
		return err
	}
	stagedFound, err := edgeAccountingQuarantine.reconcile(reservationIDs, reservations)
	if err != nil {
		return err
	}
	if stagedFound {
		MarkEdgeAccountingFailure(true)
	}
	return nil
}
