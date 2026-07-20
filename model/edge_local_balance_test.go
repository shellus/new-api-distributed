package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEdgeLocalBalanceReservationSettlementAndOverlayConverge(t *testing.T) {
	db := openEdgeLocalTestDB(t, "balance-ledger.db")
	snapshot := edgeLocalTestSnapshot(1)
	require.NoError(t, ApplyEdgeLocalSnapshot(db, snapshot))
	now := time.UnixMilli(edgeLocalTestNow + 10_000)
	control := dto.EdgeNodeControlConfigV1{NodeID: "edge.balance-test", NodeGeneration: 1}
	full := dto.EdgeBalanceDeltaV2{
		Dataset:      dto.EdgeBalanceDatasetBalancesV2,
		BaseRevision: 0,
		Revision:     1,
		Full:         true,
		Wallets:      []dto.EdgeWalletBalanceV2{{UserID: 7, RemainQuota: 100}},
		Tokens:       []dto.EdgeTokenBalanceV2{{TokenID: 11, UserID: 7, RemainQuota: 60}},
		Subscriptions: []dto.EdgeSubscriptionBalanceV2{{
			SubscriptionID: 21, UserID: 7, TotalQuota: 100, RemainQuota: 80,
			ExpiresAtUnixMilli: now.Add(time.Hour).UnixMilli(), AllowWalletOverflow: true,
		}},
	}
	require.NoError(t, ApplyEdgeLocalBalanceDelta(db, control, full, now.UnixMilli()))

	reservation, err := ReserveEdgeLocalBalance(db, EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-balance-1", RequestID: "request-balance-1",
		UserID: 7, TokenID: 11, Quota: 50, NegativeFloorQuota: -20, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	assert.Equal(t, EdgeBalanceAccountTypeSubscription, reservation.FundingAccountType)
	assert.Equal(t, int64(21), reservation.FundingAccountID)

	adjusted, err := AdjustEdgeLocalBalanceReservation(db, reservation.ReservationID, 70, now.Add(time.Second).UnixMilli())
	require.NoError(t, err)
	assert.Equal(t, int64(70), adjusted.ReservedQuota)
	_, err = AdjustEdgeLocalBalanceReservation(db, reservation.ReservationID, 81, now.Add(2*time.Second).UnixMilli())
	assert.ErrorIs(t, err, ErrEdgeLocalQuotaInsufficient)

	event := edgeLocalBalanceUsageEvent("event-balance-1", 70, now)
	require.NoError(t, StageEdgeLocalReservationSettlement(db, reservation.ReservationID, event))
	settled, err := SettleStagedEdgeLocalReservation(db, reservation.ReservationID)
	require.NoError(t, err)
	assert.Equal(t, int64(70), settled.Billing.ChargedQuota)

	subscription := requireEdgeLocalBalanceAccount(t, db, EdgeBalanceAccountTypeSubscription, 21)
	token := requireEdgeLocalBalanceAccount(t, db, EdgeBalanceAccountTypeToken, 11)
	assert.Equal(t, int64(70), subscription.UnsettledQuota)
	assert.Equal(t, int64(70), token.UnsettledQuota)
	assert.Zero(t, subscription.ReservedQuota)

	masterApplied := dto.EdgeBalanceDeltaV2{
		Dataset:                          dto.EdgeBalanceDatasetBalancesV2,
		BaseRevision:                     1,
		Revision:                         2,
		SettlementAppliedThroughSequence: settled.Sequence,
		Tokens:                           []dto.EdgeTokenBalanceV2{{TokenID: 11, UserID: 7, RemainQuota: -10}},
		Subscriptions: []dto.EdgeSubscriptionBalanceV2{{
			SubscriptionID: 21, UserID: 7, TotalQuota: 100, RemainQuota: 10,
			ExpiresAtUnixMilli: now.Add(time.Hour).UnixMilli(), AllowWalletOverflow: true,
		}},
	}
	require.NoError(t, ApplyEdgeLocalBalanceDelta(db, control, masterApplied, now.Add(3*time.Second).UnixMilli()))
	subscription = requireEdgeLocalBalanceAccount(t, db, EdgeBalanceAccountTypeSubscription, 21)
	token = requireEdgeLocalBalanceAccount(t, db, EdgeBalanceAccountTypeToken, 11)
	assert.Zero(t, subscription.UnsettledQuota)
	assert.Zero(t, token.UnsettledQuota)
	assert.Equal(t, int64(10), subscription.ReplicatedQuota)
	assert.Equal(t, int64(-10), token.ReplicatedQuota)

	require.NoError(t, ApplyEdgeLocalBalanceDelta(db, control, masterApplied, now.Add(4*time.Second).UnixMilli()))
	state, err := GetEdgeLocalBalanceState(db)
	require.NoError(t, err)
	assert.True(t, state.Initialized)
	assert.Equal(t, int64(2), state.Revision)
	assert.Equal(t, settled.Sequence, state.SettlementSequence)
}

func TestEdgeLocalBalanceWorksOfflineUntilNegativeFloor(t *testing.T) {
	db := openEdgeLocalTestDB(t, "balance-offline.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(1)))
	now := time.UnixMilli(edgeLocalTestNow + 20_000)
	control := dto.EdgeNodeControlConfigV1{NodeID: "edge.balance-offline", NodeGeneration: 1}
	require.NoError(t, ApplyEdgeLocalBalanceDelta(db, control, dto.EdgeBalanceDeltaV2{
		Dataset: dto.EdgeBalanceDatasetBalancesV2, BaseRevision: 0, Revision: 1, Full: true,
		Wallets: []dto.EdgeWalletBalanceV2{{UserID: 7, RemainQuota: 100}},
		Tokens:  []dto.EdgeTokenBalanceV2{{TokenID: 11, UserID: 7, RemainQuota: 60}},
		Subscriptions: []dto.EdgeSubscriptionBalanceV2{{
			SubscriptionID: 21, UserID: 7, TotalQuota: 100, RemainQuota: 80,
			ExpiresAtUnixMilli: now.Add(time.Hour).UnixMilli(), AllowWalletOverflow: true,
		}},
	}, now.UnixMilli()))

	reservation, err := ReserveEdgeLocalBalance(db, EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-offline-1", RequestID: "request-offline-1",
		UserID: 7, TokenID: 11, Quota: 80, NegativeFloorQuota: -20, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	event := edgeLocalBalanceUsageEvent("event-offline-1", 80, now)
	require.NoError(t, StageEdgeLocalReservationSettlement(db, reservation.ReservationID, event))
	_, err = SettleStagedEdgeLocalReservation(db, reservation.ReservationID)
	require.NoError(t, err)

	_, err = ReserveEdgeLocalBalance(db, EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-offline-2", RequestID: "request-offline-2",
		UserID: 7, TokenID: 11, Quota: 1, NegativeFloorQuota: -20, NowUnixMilli: now.Add(time.Second).UnixMilli(),
	})
	assert.ErrorIs(t, err, ErrEdgeLocalQuotaInsufficient)
}

func TestEdgeLocalSettlementRequestIDRefreshesAfterCircuitEpochAdvance(t *testing.T) {
	db := openEdgeLocalTestDB(t, "balance-circuit-retry.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(1)))
	now := time.UnixMilli(edgeLocalTestNow + 30_000)
	control := dto.EdgeNodeControlConfigV1{NodeID: "edge.balance-retry", NodeGeneration: 1}
	require.NoError(t, ApplyEdgeLocalBalanceDelta(db, control, dto.EdgeBalanceDeltaV2{
		Dataset: dto.EdgeBalanceDatasetBalancesV2, BaseRevision: 0, Revision: 1, Full: true,
		Wallets: []dto.EdgeWalletBalanceV2{{UserID: 7, RemainQuota: 100}},
		Tokens:  []dto.EdgeTokenBalanceV2{{TokenID: 11, UserID: 7, RemainQuota: 100}},
	}, now.UnixMilli()))
	reservation, err := ReserveEdgeLocalBalance(db, EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-circuit-retry", RequestID: "request-circuit-retry",
		UserID: 7, TokenID: 11, Quota: 10, NegativeFloorQuota: -20, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	_, err = SettleEdgeLocalReservation(db, reservation.ReservationID, edgeLocalBalanceUsageEvent("event-circuit-retry", 10, now))
	require.NoError(t, err)
	built, err := BuildEdgeLocalSettlementBlock(db, dto.EdgeControlRequestMetaV1{
		ProtocolVersion: dto.EdgeControlProtocolVersionV2, RequestID: "settlement-at-epoch-0",
	}, "block-circuit-retry", 100, now.Add(time.Second).UnixMilli(), 0)
	require.NoError(t, err)
	originalDigest := built.BlockDigest

	unchanged, err := RefreshEdgeLocalSettlementRequest(db, built.BlockID, dto.EdgeControlRequestMetaV1{
		ProtocolVersion: dto.EdgeControlProtocolVersionV2, RequestID: "settlement-still-epoch-0",
	}, 0)
	require.NoError(t, err)
	assert.Equal(t, "settlement-at-epoch-0", unchanged.Meta.RequestID)

	refreshed, err := RefreshEdgeLocalSettlementRequest(db, built.BlockID, dto.EdgeControlRequestMetaV1{
		ProtocolVersion: dto.EdgeControlProtocolVersionV2, RequestID: "settlement-at-epoch-2",
	}, 2)
	require.NoError(t, err)
	assert.Equal(t, "settlement-at-epoch-2", refreshed.Meta.RequestID)
	assert.Equal(t, originalDigest, refreshed.BlockDigest)
	assert.Equal(t, built.Events, refreshed.Events)
}

func TestEdgeLocalReservationOwnerAndTaskLookupIncludeZeroQuota(t *testing.T) {
	db := openEdgeLocalTestDB(t, "balance-task-owner.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(1)))
	now := time.UnixMilli(edgeLocalTestNow + 40_000)
	control := dto.EdgeNodeControlConfigV1{NodeID: "edge.balance-task-owner", NodeGeneration: 1}
	require.NoError(t, ApplyEdgeLocalBalanceDelta(db, control, dto.EdgeBalanceDeltaV2{
		Dataset: dto.EdgeBalanceDatasetBalancesV2, BaseRevision: 0, Revision: 1, Full: true,
		Wallets: []dto.EdgeWalletBalanceV2{{UserID: 7, RemainQuota: 100}},
		Tokens:  []dto.EdgeTokenBalanceV2{{TokenID: 11, UserID: 7, RemainQuota: 100}},
	}, now.UnixMilli()))

	reservation, err := ReserveEdgeLocalBalance(db, EdgeLocalBalanceReservationRequest{
		ReservationID: "reservation-task-owner", RequestID: "request-task-owner",
		UserID: 7, TokenID: 11, Quota: 0, NegativeFloorQuota: -20, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err)
	require.NoError(t, BindEdgeLocalReservationOwner(db, reservation.ReservationID, "task", "task-owner-test", now.Add(time.Second).UnixMilli()))

	previousDB := DB
	DB = db
	t.Cleanup(func() { DB = previousDB })
	task := &Task{
		TaskID: "task-owner-test", UserId: 7, ChannelId: 31, Status: TaskStatusSubmitted,
		PrivateData: TaskPrivateData{TokenId: 11, EdgeReservationID: reservation.ReservationID},
	}
	require.NoError(t, task.Insert())

	owned, err := ListActiveEdgeLocalOwnedReservations(db, "task")
	require.NoError(t, err)
	require.Len(t, owned, 1)
	assert.Equal(t, "task-owner-test", owned[0].OwnerID)
	tasks, err := GetEdgeTasksWithReservations()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, reservation.ReservationID, tasks[0].PrivateData.EdgeReservationID)

	require.NoError(t, RefundEdgeLocalReservation(db, reservation.ReservationID, now.Add(2*time.Second).UnixMilli()))
	owned, err = ListActiveEdgeLocalOwnedReservations(db, "task")
	require.NoError(t, err)
	assert.Empty(t, owned)
}

func edgeLocalBalanceUsageEvent(eventID string, charged int64, now time.Time) dto.EdgeUsageEventV1 {
	status := 200
	return dto.EdgeUsageEventV1{
		EventID: eventID, ChannelID: 31, Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1,
		Model: "gpt-4o-mini", Group: "default", StartedAtUnixMilli: now.UnixMilli(),
		FinishedAtUnixMilli: now.Add(time.Second).UnixMilli(), Outcome: dto.EdgeUsageOutcomeSuccessV1,
		HTTPStatus: &status, Billing: dto.EdgeUsageBillingV1{
			PricingPolicyID: "pricing-1", PricingPolicyVersion: "v1",
			BillingMode: dto.EdgeBillingModeFixedPriceV1, GroupRatio: 1, ChargedQuota: charged,
		},
	}
}

func requireEdgeLocalBalanceAccount(t *testing.T, db *gorm.DB, accountType EdgeBalanceAccountType, accountID int64) EdgeLocalBalanceAccount {
	t.Helper()
	var account EdgeLocalBalanceAccount
	require.NoError(t, db.Where("account_type = ? AND account_id = ?", accountType, accountID).First(&account).Error)
	return account
}
