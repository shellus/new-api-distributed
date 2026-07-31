package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrEdgeConsumeLogOutboxClaimLost = errors.New("edge consume-log outbox claim was lost")

const maxEdgeConsumeLogOutboxClaimAttempts = 16

// ClaimEdgeConsumeLogOutbox leases one ready row using a compare-and-swap on
// the existing status/attempt/available tuple. It returns nil, nil when no row
// is ready. Keeping the lease in AvailableAt avoids a separate process-local
// ownership flag and lets another master recover the row after a publisher crash.
func ClaimEdgeConsumeLogOutbox(ctx context.Context, now time.Time, leaseDuration time.Duration) (*EdgeConsumeLogOutbox, error) {
	if DB == nil {
		return nil, errors.New("database is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now()
	}
	if leaseDuration <= 0 {
		return nil, errors.New("edge consume-log outbox lease duration must be positive")
	}
	nowUnix := now.Unix()
	if nowUnix <= 0 {
		return nil, errors.New("edge consume-log outbox claim time must be positive")
	}
	leaseUntil := now.Add(leaseDuration).Unix()
	if leaseUntil <= nowUnix {
		leaseUntil = nowUnix + 1
	}
	db := DB.WithContext(ctx)

	for attempt := 0; attempt < maxEdgeConsumeLogOutboxClaimAttempts; attempt++ {
		var candidate EdgeConsumeLogOutbox
		query := db.Where("status IN ? AND available_at <= ?", []EdgeConsumeLogOutboxStatus{
			EdgeConsumeLogOutboxStatusPending,
			EdgeConsumeLogOutboxStatusFailed,
		}, nowUnix).Order("id ASC").Limit(1).Find(&candidate)
		if query.Error != nil {
			return nil, query.Error
		}
		if query.RowsAffected == 0 {
			return nil, nil
		}

		result := db.Model(&EdgeConsumeLogOutbox{}).
			Where("id = ? AND status = ? AND attempts = ? AND available_at = ?",
				candidate.ID, candidate.Status, candidate.Attempts, candidate.AvailableAt).
			UpdateColumns(map[string]any{
				"attempts":     candidate.Attempts + 1,
				"available_at": leaseUntil,
				"updated_at":   nowUnix,
			})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		candidate.Attempts++
		candidate.AvailableAt = leaseUntil
		candidate.UpdatedAt = nowUnix
		return &candidate, nil
	}
	return nil, ErrEdgeConsumeLogOutboxClaimLost
}

func MarkEdgeConsumeLogOutboxPublished(ctx context.Context, claim *EdgeConsumeLogOutbox, now time.Time) error {
	if DB == nil {
		return errors.New("database is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateEdgeConsumeLogOutboxClaim(claim); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now()
	}
	nowUnix := now.Unix()
	if nowUnix <= 0 {
		return errors.New("edge consume-log outbox publish time must be positive")
	}
	db := DB.WithContext(ctx)
	result := db.Model(&EdgeConsumeLogOutbox{}).
		Where("id = ? AND status IN ? AND attempts = ? AND available_at = ?", claim.ID,
			[]EdgeConsumeLogOutboxStatus{EdgeConsumeLogOutboxStatusPending, EdgeConsumeLogOutboxStatusFailed},
			claim.Attempts, claim.AvailableAt).
		UpdateColumns(map[string]any{
			"status":       EdgeConsumeLogOutboxStatusPublished,
			"last_error":   "",
			"published_at": nowUnix,
			"updated_at":   nowUnix,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var current EdgeConsumeLogOutbox
	if err := db.Select("status").First(&current, claim.ID).Error; err == nil && current.Status == EdgeConsumeLogOutboxStatusPublished {
		return nil
	}
	return ErrEdgeConsumeLogOutboxClaimLost
}

func MarkEdgeConsumeLogOutboxFailed(ctx context.Context, claim *EdgeConsumeLogOutbox, publishErr error, retryAt time.Time, now time.Time) error {
	if DB == nil {
		return errors.New("database is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateEdgeConsumeLogOutboxClaim(claim); err != nil {
		return err
	}
	if publishErr == nil {
		return errors.New("edge consume-log outbox publish error is nil")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if !retryAt.After(now) {
		return errors.New("edge consume-log outbox retry time must be in the future")
	}
	if now.Unix() <= 0 || retryAt.Unix() <= 0 {
		return errors.New("edge consume-log outbox failure timestamps must be positive")
	}
	errorText := strings.TrimSpace(publishErr.Error())
	if errorText == "" {
		errorText = "unknown publish error"
	}
	if len(errorText) > 4096 {
		errorText = errorText[:4096]
	}
	result := DB.WithContext(ctx).Model(&EdgeConsumeLogOutbox{}).
		Where("id = ? AND status IN ? AND attempts = ? AND available_at = ?", claim.ID,
			[]EdgeConsumeLogOutboxStatus{EdgeConsumeLogOutboxStatusPending, EdgeConsumeLogOutboxStatusFailed},
			claim.Attempts, claim.AvailableAt).
		UpdateColumns(map[string]any{
			"status":       EdgeConsumeLogOutboxStatusFailed,
			"last_error":   errorText,
			"available_at": retryAt.Unix(),
			"updated_at":   now.Unix(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeConsumeLogOutboxClaimLost
	}
	return nil
}

func MarkEdgeConsumeLogOutboxQuarantined(ctx context.Context, claim *EdgeConsumeLogOutbox, publishErr error, now time.Time) error {
	if DB == nil {
		return errors.New("database is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateEdgeConsumeLogOutboxClaim(claim); err != nil {
		return err
	}
	if publishErr == nil {
		return errors.New("edge consume-log outbox quarantine error is nil")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if now.Unix() <= 0 {
		return errors.New("edge consume-log outbox quarantine time must be positive")
	}
	errorText := strings.TrimSpace(publishErr.Error())
	if errorText == "" {
		errorText = "unknown permanent projection error"
	}
	if len(errorText) > 4096 {
		errorText = errorText[:4096]
	}
	result := DB.WithContext(ctx).Model(&EdgeConsumeLogOutbox{}).
		Where("id = ? AND status IN ? AND attempts = ? AND available_at = ?", claim.ID,
			[]EdgeConsumeLogOutboxStatus{EdgeConsumeLogOutboxStatusPending, EdgeConsumeLogOutboxStatusFailed},
			claim.Attempts, claim.AvailableAt).
		UpdateColumns(map[string]any{
			"status":     EdgeConsumeLogOutboxStatusQuarantined,
			"last_error": errorText,
			"updated_at": now.Unix(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEdgeConsumeLogOutboxClaimLost
	}
	return nil
}

func validateEdgeConsumeLogOutboxClaim(claim *EdgeConsumeLogOutbox) error {
	if claim == nil || claim.ID <= 0 || claim.Attempts <= 0 || claim.AvailableAt <= 0 {
		return fmt.Errorf("%w: invalid claim", ErrEdgeConsumeLogOutboxClaimLost)
	}
	if claim.Status != EdgeConsumeLogOutboxStatusPending && claim.Status != EdgeConsumeLogOutboxStatusFailed {
		return fmt.Errorf("%w: status %q is not claimable", ErrEdgeConsumeLogOutboxClaimLost, claim.Status)
	}
	return nil
}
