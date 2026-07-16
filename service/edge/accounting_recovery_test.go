package edge

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEdgeAccountingReadinessRecoversDurableStagedUsage(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	lease := edgeRuntimeTestLease(now, "lease-accounting-recovery", 7, 11, 100)
	require.NoError(t, model.InstallEdgeLocalLease(db, lease))
	_, err := model.ReserveEdgeLocalQuota(db, model.EdgeLocalReservationRequest{
		ReservationID: "reservation-accounting-recovery", RequestID: "request-accounting-recovery",
		LeaseID: lease.LeaseID, Quota: 40, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	require.NoError(t, model.StageEdgeLocalReservationSettlement(
		db, "reservation-accounting-recovery", edgeAccountingTestUsageEvent("event-accounting-recovery", 40, now),
	))

	require.NoError(t, InitializeEdgeAccountingReadiness(context.Background(), db))
	assert.False(t, edgeAccountingReady.Load())
	assert.False(t, EdgeServingReady())
	require.NoError(t, RecoverEdgeStagedSettlements(context.Background(), db))
	assert.True(t, edgeAccountingReady.Load())
	assert.True(t, EdgeServingReady())

	reservation, err := model.GetEdgeLocalReservation(db, "reservation-accounting-recovery")
	require.NoError(t, err)
	assert.Equal(t, model.EdgeLocalReservationStatusSettled, reservation.Status)
	var outboxCount int64
	require.NoError(t, db.Model(&model.EdgeLocalOutbox{}).Where("event_id = ?", "event-accounting-recovery").Count(&outboxCount).Error)
	assert.Equal(t, int64(1), outboxCount)
}

func TestEdgeAccountingRecoveryStaysClosedUntilQuotaReturns(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	lease := edgeRuntimeTestLease(now, "lease-accounting-blocked", 7, 11, 100)
	require.NoError(t, model.InstallEdgeLocalLease(db, lease))
	_, err := model.ReserveEdgeLocalQuota(db, model.EdgeLocalReservationRequest{
		ReservationID: "reservation-accounting-blocked", RequestID: "request-accounting-blocked",
		LeaseID: lease.LeaseID, Quota: 40, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	_, err = model.ReserveEdgeLocalQuota(db, model.EdgeLocalReservationRequest{
		ReservationID: "reservation-accounting-holder", RequestID: "request-accounting-holder",
		LeaseID: lease.LeaseID, Quota: 60, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	_, err = model.SettleEdgeLocalReservation(
		db, "reservation-accounting-blocked", edgeAccountingTestUsageEvent("event-accounting-blocked", 60, now),
	)
	assert.ErrorIs(t, err, model.ErrEdgeLocalQuotaInsufficient)

	MarkEdgeAccountingFailure(true)
	err = RecoverEdgeStagedSettlements(context.Background(), db)
	assert.ErrorIs(t, err, model.ErrEdgeLocalQuotaInsufficient)
	assert.False(t, EdgeServingReady())
	require.NoError(t, model.RefundEdgeLocalReservation(db, "reservation-accounting-holder", now.Add(time.Second).UnixMilli()))
	require.NoError(t, RecoverEdgeStagedSettlements(context.Background(), db))
	assert.True(t, EdgeServingReady())
}

func TestEdgeAccountingRecoverySettlesQuotaReleasingEventsBeforeRetryingBlockedOnes(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	lease := edgeRuntimeTestLease(now, "lease-accounting-reorder", 7, 11, 100)
	require.NoError(t, model.InstallEdgeLocalLease(db, lease))
	for _, reservation := range []model.EdgeLocalReservationRequest{
		{
			ReservationID: "reservation-accounting-needs-quota", RequestID: "request-accounting-needs-quota",
			LeaseID: lease.LeaseID, Quota: 40, NowUnixMilli: now.UnixMilli(),
		},
		{
			ReservationID: "reservation-accounting-returns-quota", RequestID: "request-accounting-returns-quota",
			LeaseID: lease.LeaseID, Quota: 60, NowUnixMilli: now.UnixMilli(),
		},
	} {
		_, err := model.ReserveEdgeLocalQuota(db, reservation)
		require.NoError(t, err)
	}
	blockedEvent := edgeAccountingTestUsageEvent("event-accounting-needs-quota", 60, now)
	blockedEvent.FinishedAtUnixMilli = now.UnixMilli()
	require.NoError(t, model.StageEdgeLocalReservationSettlement(db, "reservation-accounting-needs-quota", blockedEvent))
	releasingEvent := edgeAccountingTestUsageEvent("event-accounting-returns-quota", 10, now.Add(time.Millisecond))
	require.NoError(t, model.StageEdgeLocalReservationSettlement(db, "reservation-accounting-returns-quota", releasingEvent))

	require.NoError(t, InitializeEdgeAccountingReadiness(context.Background(), db))
	require.NoError(t, RecoverEdgeStagedSettlements(context.Background(), db))
	assert.True(t, EdgeServingReady())
	for _, reservationID := range []string{"reservation-accounting-needs-quota", "reservation-accounting-returns-quota"} {
		reservation, err := model.GetEdgeLocalReservation(db, reservationID)
		require.NoError(t, err)
		assert.Equal(t, model.EdgeLocalReservationStatusSettled, reservation.Status)
	}
	storedLease, err := model.GetEdgeLocalLease(db, lease.LeaseID)
	require.NoError(t, err)
	assert.Equal(t, int64(30), storedLease.RemainingQuota)
	assert.Zero(t, storedLease.ReservedQuota)
	assert.Equal(t, int64(70), storedLease.ConsumedQuota)
}

func TestEdgeAccountingRecoveryLatchesDurableCorruption(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	lease := edgeRuntimeTestLease(now, "lease-accounting-corrupt", 7, 11, 100)
	require.NoError(t, model.InstallEdgeLocalLease(db, lease))
	_, err := model.ReserveEdgeLocalQuota(db, model.EdgeLocalReservationRequest{
		ReservationID: "reservation-accounting-corrupt", RequestID: "request-accounting-corrupt",
		LeaseID: lease.LeaseID, Quota: 40, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	require.NoError(t, model.StageEdgeLocalReservationSettlement(
		db, "reservation-accounting-corrupt", edgeAccountingTestUsageEvent("event-accounting-corrupt", 40, now),
	))
	require.NoError(t, db.Where("lease_id = ?", lease.LeaseID).Delete(&model.EdgeLocalQuotaLease{}).Error)

	require.NoError(t, InitializeEdgeAccountingReadiness(context.Background(), db))
	err = RecoverEdgeStagedSettlements(context.Background(), db)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	assert.False(t, EdgeServingReady())
	err = RecoverEdgeStagedSettlements(context.Background(), db)
	assert.ErrorIs(t, err, errEdgeAccountingRecoveryBlocked)
}

func TestEdgeAccountingStartupBlocksOnOrphanedUnstagedReservation(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	lease := edgeRuntimeTestLease(now, "lease-accounting-orphan", 7, 11, 100)
	require.NoError(t, model.InstallEdgeLocalLease(db, lease))
	_, err := model.ReserveEdgeLocalQuota(db, model.EdgeLocalReservationRequest{
		ReservationID: "reservation-accounting-orphan", RequestID: "request-accounting-orphan",
		LeaseID: lease.LeaseID, Quota: 40, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)

	require.NoError(t, InitializeEdgeAccountingReadiness(context.Background(), db))
	assert.False(t, EdgeServingReady())
	assert.True(t, edgeAccountingBlock.Load())
	err = RecoverEdgeStagedSettlements(context.Background(), db)
	assert.ErrorIs(t, err, errEdgeAccountingRecoveryBlocked)
	reservation, err := model.GetEdgeLocalReservation(db, "reservation-accounting-orphan")
	require.NoError(t, err)
	assert.Equal(t, model.EdgeLocalReservationStatusActive, reservation.Status, "ambiguous usage must not be guessed as a refund or charge")
}

func TestEdgeAccountingUnrecoverableFailureStaysLatched(t *testing.T) {
	db, _ := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	MarkEdgeAccountingFailure(false)
	assert.False(t, EdgeServingReady())
	err := RecoverEdgeStagedSettlements(context.Background(), db)
	assert.ErrorIs(t, err, errEdgeAccountingRecoveryBlocked)
	assert.False(t, EdgeServingReady())

	require.NoError(t, InitializeEdgeAccountingReadiness(context.Background(), db), "restart initialization clears only the in-memory latch after checking durable state")
	assert.True(t, EdgeServingReady())
}

func edgeAccountingTestUsageEvent(eventID string, chargedQuota int64, now time.Time) dto.EdgeUsageEventV1 {
	status := http.StatusOK
	return dto.EdgeUsageEventV1{
		EventID: eventID, ChannelID: 31,
		Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1, Model: "gpt-test", Group: "default",
		StartedAtUnixMilli: now.Add(-time.Second).UnixMilli(), FinishedAtUnixMilli: now.UnixMilli(),
		Outcome: dto.EdgeUsageOutcomeSuccessV1, HTTPStatus: &status,
		Billing: dto.EdgeUsageBillingV1{
			PricingPolicyID: "pricing-runtime", PricingPolicyVersion: "v1",
			BillingMode: dto.EdgeBillingModeFixedPriceV1, GroupRatio: 1, ChargedQuota: chargedQuota,
		},
	}
}
