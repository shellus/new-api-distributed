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
		UserID: 7, TokenID: 11, Quota: 40, NegativeFloorQuota: -10_000_000, NowUnixMilli: now.UnixMilli(),
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

func TestEdgeAccountingStartupBlocksOnOrphanedBalanceReservation(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	_, err := model.ReserveEdgeLocalBalance(db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-accounting-orphan", RequestID: "request-accounting-orphan",
		UserID: 7, TokenID: 11, Quota: 40, NegativeFloorQuota: -10_000_000, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)

	require.NoError(t, InitializeEdgeAccountingReadiness(context.Background(), db))
	assert.True(t, edgeAccountingBlock.Load())
	assert.False(t, edgeAccountingReady.Load())
	assert.ErrorIs(t, RecoverEdgeStagedSettlements(context.Background(), db), errEdgeAccountingRecoveryBlocked)
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
