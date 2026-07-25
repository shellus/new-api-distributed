package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestPruneEdgeLocalAccountingHistoryRequiresAckAndBalanceWatermark(t *testing.T) {
	db := openEdgeLocalTestDB(t, "accounting-prune.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(1)))
	now := time.UnixMilli(edgeLocalTestNow + 50_000)
	control := dto.EdgeNodeControlConfigV1{NodeID: "edge.accounting-prune", NodeGeneration: 1}
	require.NoError(t, ApplyEdgeLocalBalanceDelta(db, control, dto.EdgeBalanceDeltaV2{
		Dataset: dto.EdgeBalanceDatasetBalancesV2, BaseRevision: 0, Revision: 1, Full: true,
		Wallets: []dto.EdgeWalletBalanceV2{{UserID: 7, RemainQuota: 1_000}},
		Tokens:  []dto.EdgeTokenBalanceV2{{TokenID: 11, UserID: 7, RemainQuota: 1_000}},
		Subscriptions: []dto.EdgeSubscriptionBalanceV2{{
			SubscriptionID: 21, UserID: 7, TotalQuota: 1_000, RemainQuota: 1_000,
			ExpiresAtUnixMilli: now.Add(time.Hour).UnixMilli(), AllowWalletOverflow: true,
		}},
	}, now.UnixMilli()))

	for sequence := 1; sequence <= 3; sequence++ {
		reservationID := fmt.Sprintf("reservation-prune-%d", sequence)
		reservation, err := ReserveEdgeLocalBalance(db, EdgeLocalBalanceReservationRequest{
			ReservationID: reservationID, RequestID: fmt.Sprintf("request-prune-%d", sequence),
			UserID: 7, TokenID: 11, Quota: 10, SettlementFloorQuota: -20,
			NowUnixMilli: now.Add(time.Duration(sequence) * time.Second).UnixMilli(),
		})
		require.NoError(t, err)
		if sequence == 3 {
			require.NoError(t, BindEdgeLocalReservationOwner(db, reservationID, "task", "task-prune-owner", now.Add(4*time.Second).UnixMilli()))
		}
		_, err = SettleEdgeLocalReservation(db, reservation.ReservationID,
			edgeLocalBalanceUsageEvent(fmt.Sprintf("event-prune-%d", sequence), 10, now.Add(time.Duration(sequence)*time.Second)))
		require.NoError(t, err)
	}

	block, err := BuildEdgeLocalSettlementBlock(db, dto.EdgeControlRequestMetaV1{
		ProtocolVersion: dto.EdgeControlProtocolVersionV2, RequestID: "settlement-prune",
	}, "block-prune", 100, now.Add(10*time.Second).UnixMilli(), 0)
	require.NoError(t, err)
	require.NoError(t, AcknowledgeEdgeLocalSettlementBlock(db, dto.EdgeSettlementAckV1{
		Status: dto.EdgeSettlementAckAcceptedV1, NodeID: control.NodeID, NodeGeneration: control.NodeGeneration,
		BlockID: block.BlockID, AckedThroughSequence: block.LastSequence, NextExpectedSequence: block.LastSequence + 1,
		AcceptedEventCount: len(block.Events), AcknowledgedAtUnixMilli: now.Add(11 * time.Second).UnixMilli(),
	}))

	result, err := PruneEdgeLocalAccountingHistory(db, 0, 100)
	require.NoError(t, err)
	assert.Zero(t, result.DeletedRows(), "master acknowledgement alone must not prune overlay evidence")

	require.NoError(t, ApplyEdgeLocalBalanceDelta(db, control, dto.EdgeBalanceDeltaV2{
		Dataset: dto.EdgeBalanceDatasetBalancesV2, BaseRevision: 1, Revision: 2,
		SettlementAppliedThroughSequence: block.LastSequence,
		Tokens:                           []dto.EdgeTokenBalanceV2{{TokenID: 11, UserID: 7, RemainQuota: 970}},
		Subscriptions: []dto.EdgeSubscriptionBalanceV2{{
			SubscriptionID: 21, UserID: 7, TotalQuota: 1_000, RemainQuota: 970,
			ExpiresAtUnixMilli: now.Add(time.Hour).UnixMilli(), AllowWalletOverflow: true,
		}},
	}, now.Add(12*time.Second).UnixMilli()))

	result, err = PruneEdgeLocalAccountingHistory(db, 1, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.ThroughSequence)
	assert.Equal(t, int64(2), result.Reservations)
	assert.Equal(t, int64(2), result.UsageEvents)
	assert.Equal(t, int64(2), result.OutboxEntries)
	assert.Zero(t, result.SettlementBlocks, "a block spanning the retained tail must remain intact")

	assertEdgeLocalAccountingHistoryCounts(t, db, 1, 1, 1, 1)
	result, err = PruneEdgeLocalAccountingHistory(db, 0, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(3), result.ThroughSequence)
	assert.Zero(t, result.Reservations, "owned task reservation must remain as recovery evidence")
	assert.Equal(t, int64(1), result.UsageEvents)
	assert.Equal(t, int64(1), result.OutboxEntries)
	assert.Equal(t, int64(1), result.SettlementBlocks)
	assertEdgeLocalAccountingHistoryCounts(t, db, 1, 0, 0, 0)
}

func assertEdgeLocalAccountingHistoryCounts(t *testing.T, db *gorm.DB, reservations, usageEvents, outboxEntries, settlementBlocks int64) {
	t.Helper()
	for _, item := range []struct {
		model any
		want  int64
	}{
		{model: &EdgeLocalQuotaReservation{}, want: reservations},
		{model: &EdgeLocalUsageEvent{}, want: usageEvents},
		{model: &EdgeLocalOutbox{}, want: outboxEntries},
		{model: &EdgeLocalSettlementBlock{}, want: settlementBlocks},
	} {
		var count int64
		require.NoError(t, db.Model(item.model).Count(&count).Error)
		assert.Equal(t, item.want, count)
	}
}
