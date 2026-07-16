package model

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeQuotaLeaseEnforcesLifecycleAndFullTerminalAccounting(t *testing.T) {
	truncateTables(t)
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	lease := &EdgeQuotaLease{
		LeaseUID: "lease-lifecycle", NodeID: 1, NodeGeneration: 1, UserID: 1, TokenID: 1,
		SnapshotID: 1, SnapshotUID: "snapshot-1", SnapshotRevision: 1, PricingRevision: 1,
		RequestIdempotencyKey: "lease-lifecycle-request", RequestHash: strings.Repeat("a", 64),
		FundingSource: EdgeLeaseFundingSourceWallet, GrantedQuota: 100, RenewAfterRemainingQuota: 25,
		IssuedAtUnixMilli: now.UnixMilli(), ExpiresAtUnixMilli: now.Add(time.Minute).UnixMilli(),
	}
	require.NoError(t, DB.Create(lease).Error)

	lease.Status = EdgeQuotaLeaseStatusClosed
	lease.ReturnedQuota = 100
	lease.ClosedAtUnixMilli = now.Add(time.Second).UnixMilli()
	lease.Version++
	err := DB.Save(lease).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEdgeQuotaLeaseTransition)

	require.NoError(t, DB.First(lease, lease.ID).Error)
	lease.Status = EdgeQuotaLeaseStatusClosing
	lease.CloseAfterEventSequence = 1
	lease.Version++
	require.NoError(t, DB.Save(lease).Error)

	lease.Status = EdgeQuotaLeaseStatusClosed
	lease.ReturnedQuota = 100
	lease.ClosedAtUnixMilli = now.Add(2 * time.Second).UnixMilli()
	lease.Version++
	require.NoError(t, DB.Save(lease).Error)
	assert.Zero(t, lease.RemainingQuota())

	lease.Status = EdgeQuotaLeaseStatusActive
	lease.ClosedAtUnixMilli = 0
	err = DB.Save(lease).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEdgeQuotaLeaseTransition)
}

func TestEdgeUsageEventUniqueSequenceEventAndReservation(t *testing.T) {
	truncateTables(t)
	base := &EdgeUsageEvent{
		NodeID: 1, NodeGeneration: 1, BlockID: 1, LeaseID: 1,
		EventUID: "event-1", ReservationUID: "reservation-1", RequestUID: "request-1", Sequence: 1,
		UserID: 1, TokenID: 1, ChannelID: 1, Endpoint: "openai_chat_completions",
		Model: "gpt-test", Group: "default", Outcome: "success",
		StartedAtUnixMilli: 1, FinishedAtUnixMilli: 1,
		ReservedQuota: 10, ChargedQuota: 10, PricingPolicyID: "price-1", PricingPolicyVersion: "v1",
		UsagePayload: "{}", BillingPayload: "{}",
	}
	require.NoError(t, DB.Create(base).Error)

	duplicateReservation := *base
	duplicateReservation.ID = 0
	duplicateReservation.EventUID = "event-2"
	duplicateReservation.RequestUID = "request-2"
	duplicateReservation.Sequence = 2
	require.Error(t, DB.Create(&duplicateReservation).Error)

	duplicateSequence := *base
	duplicateSequence.ID = 0
	duplicateSequence.EventUID = "event-3"
	duplicateSequence.ReservationUID = "reservation-3"
	duplicateSequence.RequestUID = "request-3"
	require.Error(t, DB.Create(&duplicateSequence).Error)
}

func TestEdgeQuotaLeaseAllowsAuditableZeroGrant(t *testing.T) {
	truncateTables(t)
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	lease := &EdgeQuotaLease{
		LeaseUID: "lease-free", NodeID: 1, NodeGeneration: 1, UserID: 1, TokenID: 1,
		SnapshotID: 1, SnapshotUID: "snapshot-1", SnapshotRevision: 1, PricingRevision: 1,
		RequestIdempotencyKey: "lease-free-request", RequestHash: strings.Repeat("b", 64),
		FundingSource: EdgeLeaseFundingSourceWallet, GrantedQuota: 0, RenewAfterRemainingQuota: 0,
		IssuedAtUnixMilli: now.UnixMilli(), ExpiresAtUnixMilli: now.Add(time.Minute).UnixMilli(),
	}
	require.NoError(t, DB.Create(lease).Error)
	require.NoError(t, DB.Create(&EdgeLeaseFunding{
		LeaseID: lease.ID, Source: EdgeLeaseFundingSourceWallet, UserID: 1,
		ReservedQuota: 0, Status: EdgeLeaseFundingStatusReserved,
	}).Error)
	assert.Zero(t, lease.RemainingQuota())
}
