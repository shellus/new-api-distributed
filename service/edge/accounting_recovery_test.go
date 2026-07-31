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
)

func TestEdgeAccountingReadinessRecoversDurableBalanceSettlement(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	_, err := model.ReserveEdgeLocalBalance(db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-accounting-recovery", RequestID: "request-accounting-recovery",
		UserID: 7, TokenID: 11, Quota: 40, SettlementFloorQuota: -10_000_000, NowUnixMilli: now.UnixMilli(),
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
}

func TestEdgeAccountingStartupQuarantinesOnlyOrphanedReservationSubject(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	_, err := model.ReserveEdgeLocalBalance(db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-accounting-orphan", RequestID: "request-accounting-orphan",
		UserID: 7, TokenID: 11, Quota: 40, SettlementFloorQuota: -10_000_000, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)

	require.NoError(t, InitializeEdgeAccountingReadiness(context.Background(), db))
	assert.False(t, edgeAccountingBlock.Load())
	assert.True(t, edgeAccountingReady.Load())
	assert.True(t, EdgeServingReady(), "an orphaned subject must not make the whole edge unavailable")
	assert.True(t, EdgeAccountingSubjectQuarantined(7, 11))
	assert.False(t, EdgeAccountingSubjectQuarantined(8, 12))
	assert.Equal(t, 1, EdgeAccountingQuarantinedReservationCount())

	blockedFunding := newEdgeBalanceFundingForTest(db, now, "reservation-accounting-blocked", "request-accounting-blocked")
	err = blockedFunding.PreConsume(1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), errEdgeAccountingSubjectQuarantined.Error())
	assert.False(t, blockedFunding.HasReservation())

	require.NoError(t, model.RefundEdgeLocalReservation(db, "reservation-accounting-orphan", now.Add(time.Second).UnixMilli()))
	require.NoError(t, ReconcileEdgeAccountingQuarantine(context.Background(), db))
	assert.False(t, EdgeAccountingSubjectQuarantined(7, 11))
	assert.Zero(t, EdgeAccountingQuarantinedReservationCount())

	recoveredFunding := newEdgeBalanceFundingForTest(db, now, "reservation-accounting-recovered", "request-accounting-recovered")
	require.NoError(t, recoveredFunding.PreConsume(1))
	require.NoError(t, recoveredFunding.Refund())
}

func TestEdgeAccountingRuntimeFailureQuarantinesReservationWithoutClosingReadiness(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	reservation, err := model.ReserveEdgeLocalBalance(db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-accounting-runtime", RequestID: "request-accounting-runtime",
		UserID: 7, TokenID: 11, Quota: 40, SettlementFloorQuota: -10_000_000, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)

	MarkEdgeAccountingReservationFailure(false, reservation)
	assert.False(t, edgeAccountingBlock.Load())
	assert.True(t, edgeAccountingReady.Load())
	assert.True(t, EdgeServingReady())
	assert.True(t, EdgeAccountingSubjectQuarantined(7, 11))
}

func TestEdgeAccountingManualQuarantineRetainsUnknownStatusUntilTerminal(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	reservation, err := model.ReserveEdgeLocalBalance(db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-accounting-invalid-status", RequestID: "request-accounting-invalid-status",
		UserID: 7, TokenID: 11, Quota: 40, SettlementFloorQuota: -10_000_000, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	require.NoError(t, edgeAccountingQuarantine.addManual(*reservation, false))
	require.NoError(t, db.Model(&model.EdgeLocalQuotaReservation{}).
		Where("reservation_id = ?", reservation.ReservationID).
		UpdateColumn("status", model.EdgeLocalReservationStatus("invalid")).Error)

	require.NoError(t, ReconcileEdgeAccountingQuarantine(context.Background(), db))
	assert.True(t, EdgeAccountingSubjectQuarantined(7, 11))
	assert.Equal(t, 1, EdgeAccountingQuarantinedReservationCount())

	require.NoError(t, db.Model(&model.EdgeLocalQuotaReservation{}).
		Where("reservation_id = ?", reservation.ReservationID).
		UpdateColumn("status", model.EdgeLocalReservationStatusRefunded).Error)
	require.NoError(t, ReconcileEdgeAccountingQuarantine(context.Background(), db))
	assert.False(t, EdgeAccountingSubjectQuarantined(7, 11))
	assert.Zero(t, EdgeAccountingQuarantinedReservationCount())
}

func TestEdgeAccountingQuarantinePromotesDurablyStagedReservationToGlobalRecovery(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	_, err := model.ReserveEdgeLocalBalance(db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-accounting-promote", RequestID: "request-accounting-promote",
		UserID: 7, TokenID: 11, Quota: 40, SettlementFloorQuota: -10_000_000, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	require.NoError(t, InitializeEdgeAccountingReadiness(context.Background(), db))
	assert.True(t, EdgeAccountingSubjectQuarantined(7, 11))

	require.NoError(t, model.StageEdgeLocalReservationSettlement(
		db, "reservation-accounting-promote", edgeAccountingTestUsageEvent("event-accounting-promote", 40, now),
	))
	require.NoError(t, ReconcileEdgeAccountingQuarantine(context.Background(), db))
	assert.False(t, edgeAccountingReady.Load())
	assert.False(t, EdgeAccountingSubjectQuarantined(7, 11))

	require.NoError(t, RecoverEdgeStagedSettlements(context.Background(), db))
	assert.True(t, edgeAccountingReady.Load())
}

func edgeAccountingTestUsageEvent(eventID string, chargedQuota int64, now time.Time) dto.EdgeUsageEventV1 {
	status := http.StatusOK
	return dto.EdgeUsageEventV1{
		EventID: eventID, ChannelID: 31, Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1,
		Model: "gpt-test", Group: "default", StartedAtUnixMilli: now.Add(-time.Second).UnixMilli(),
		FinishedAtUnixMilli: now.UnixMilli(), Outcome: dto.EdgeUsageOutcomeSuccessV1, HTTPStatus: &status,
		Billing: dto.EdgeUsageBillingV1{
			PricingPolicyID: "pricing-runtime", PricingPolicyVersion: "v1",
			BillingMode: dto.EdgeBillingModeFixedPriceV1, GroupRatio: 1, ChargedQuota: chargedQuota,
		},
	}
}
