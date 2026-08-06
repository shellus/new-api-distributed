package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMasterWalletBillingReserveSettleAndRefundLifecycle(t *testing.T) {
	truncate(t)
	ctx := newMasterBillingTestContext(1_000)
	seedUser(t, 11_001, 1_000)
	seedToken(t, 11_002, 11_001, "wallet-lifecycle", 1_000)
	info := masterBillingRelayInfo(11_001, 11_002, "wallet-lifecycle", "wallet_only")

	session, apiErr := NewBillingSession(ctx, info, 100)
	require.Nil(t, apiErr)
	require.NotNil(t, session)
	assert.Equal(t, BillingSourceWallet, info.BillingSource)
	assert.Equal(t, 100, info.FinalPreConsumedQuota)
	assert.Equal(t, 900, getUserQuota(t, info.UserId))
	assert.Equal(t, 900, getTokenRemainQuota(t, info.TokenId))

	require.NoError(t, session.Reserve(150))
	assert.Equal(t, 150, info.FinalPreConsumedQuota)
	assert.Equal(t, 850, getUserQuota(t, info.UserId))
	assert.Equal(t, 850, getTokenRemainQuota(t, info.TokenId))

	require.NoError(t, session.Settle(120))
	assert.Equal(t, 880, getUserQuota(t, info.UserId))
	assert.Equal(t, 880, getTokenRemainQuota(t, info.TokenId))
	assert.Equal(t, 120, getTokenUsedQuota(t, info.TokenId))

	// A late error path cannot refund an already committed request.
	session.Refund(ctx)
	assert.Equal(t, 880, getUserQuota(t, info.UserId))
	assert.Equal(t, 880, getTokenRemainQuota(t, info.TokenId))
}

func TestMasterWalletBillingRefundRestoresAllReservationsOnce(t *testing.T) {
	truncate(t)
	ctx := newMasterBillingTestContext(1_000)
	seedUser(t, 11_011, 1_000)
	seedToken(t, 11_012, 11_011, "wallet-refund", 1_000)
	info := masterBillingRelayInfo(11_011, 11_012, "wallet-refund", "wallet_only")

	session, apiErr := NewBillingSession(ctx, info, 100)
	require.Nil(t, apiErr)
	require.NoError(t, session.Reserve(150))
	session.Refund(ctx)
	session.Refund(ctx)

	assert.Equal(t, 1_000, getUserQuota(t, info.UserId))
	assert.Equal(t, 1_000, getTokenRemainQuota(t, info.TokenId))
	assert.Equal(t, 0, getTokenUsedQuota(t, info.TokenId))
	assert.False(t, session.NeedsRefund())
}

func TestMasterWalletTrustPlaygroundAndUnlimitedTokenContracts(t *testing.T) {
	trustQuota := common.GetTrustQuota()
	tests := []struct {
		name                  string
		initialQuota          int
		contextTokenQuota     int
		forcePreConsume       bool
		playground            bool
		unlimited             bool
		expectedPreConsumed   int
		expectedUserAfterPre  int
		expectedTokenAfterPre int
		expectedUserFinal     int
		expectedTokenFinal    int
	}{
		{
			name:         "trusted wallet defers both deductions until settlement",
			initialQuota: trustQuota + 1_000, contextTokenQuota: trustQuota + 1_000,
			expectedPreConsumed: 0, expectedUserAfterPre: trustQuota + 1_000, expectedTokenAfterPre: trustQuota + 1_000,
			expectedUserFinal: trustQuota + 880, expectedTokenFinal: trustQuota + 880,
		},
		{
			name:         "force pre-consume disables trust bypass",
			initialQuota: trustQuota + 1_000, contextTokenQuota: trustQuota + 1_000, forcePreConsume: true,
			expectedPreConsumed: 100, expectedUserAfterPre: trustQuota + 900, expectedTokenAfterPre: trustQuota + 900,
			expectedUserFinal: trustQuota + 880, expectedTokenFinal: trustQuota + 880,
		},
		{
			name:         "playground skips token mutations but still bills wallet",
			initialQuota: 1_000, contextTokenQuota: 1_000, playground: true,
			expectedPreConsumed: 100, expectedUserAfterPre: 900, expectedTokenAfterPre: 1_000,
			expectedUserFinal: 880, expectedTokenFinal: 1_000,
		},
		{
			name:         "unlimited token bypasses balance check while tracking usage",
			initialQuota: 1_000, contextTokenQuota: 0, unlimited: true,
			expectedPreConsumed: 100, expectedUserAfterPre: 900, expectedTokenAfterPre: -100,
			expectedUserFinal: 880, expectedTokenFinal: -120,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)
			userID := 11_100 + index*10
			tokenID := userID + 1
			key := "wallet-contract-" + test.name
			ctx := newMasterBillingTestContext(test.contextTokenQuota)
			seedUser(t, userID, test.initialQuota)
			tokenQuota := test.initialQuota
			if test.unlimited {
				tokenQuota = 0
			}
			seedToken(t, tokenID, userID, key, tokenQuota)
			if test.unlimited {
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).Update("unlimited_quota", true).Error)
			}
			info := masterBillingRelayInfo(userID, tokenID, key, "wallet_only")
			info.ForcePreConsume = test.forcePreConsume
			info.IsPlayground = test.playground
			info.TokenUnlimited = test.unlimited

			session, apiErr := NewBillingSession(ctx, info, 100)
			require.Nil(t, apiErr)
			assert.Equal(t, test.expectedPreConsumed, session.GetPreConsumedQuota())
			assert.Equal(t, test.expectedUserAfterPre, getUserQuota(t, userID))
			assert.Equal(t, test.expectedTokenAfterPre, getTokenRemainQuota(t, tokenID))

			require.NoError(t, session.Settle(120))
			assert.Equal(t, test.expectedUserFinal, getUserQuota(t, userID))
			assert.Equal(t, test.expectedTokenFinal, getTokenRemainQuota(t, tokenID))
		})
	}
}

func TestMasterSubscriptionBillingReserveSettleAndRefundLifecycle(t *testing.T) {
	truncate(t)
	ctx := newMasterBillingTestContext(1_000)
	seedUser(t, 12_001, 500)
	seedToken(t, 12_002, 12_001, "subscription-lifecycle", 1_000)
	seedBillingSubscription(t, 12_003, 12_004, 12_001, 1_000, 0, true)
	info := masterBillingRelayInfo(12_001, 12_002, "subscription-lifecycle", "subscription_only")
	info.RequestId = "subscription-lifecycle-request"

	session, apiErr := NewBillingSession(ctx, info, 100)
	require.Nil(t, apiErr)
	assert.Equal(t, BillingSourceSubscription, info.BillingSource)
	assert.Equal(t, int64(100), info.SubscriptionPreConsumed)
	assert.Equal(t, int64(100), getSubscriptionUsed(t, 12_004))
	assert.Equal(t, 900, getTokenRemainQuota(t, info.TokenId))
	assert.Equal(t, 500, getUserQuota(t, info.UserId))

	require.NoError(t, session.Reserve(150))
	assert.Equal(t, int64(150), info.SubscriptionPreConsumed)
	assert.Equal(t, int64(150), getSubscriptionUsed(t, 12_004))
	assertSubscriptionReservation(t, info.RequestId, 150, "consumed")

	require.NoError(t, session.Settle(120))
	assert.Equal(t, int64(120), getSubscriptionUsed(t, 12_004))
	assert.Equal(t, 880, getTokenRemainQuota(t, info.TokenId))
	assert.Equal(t, int64(-30), info.SubscriptionPostDelta)
	// The request record retains the original hold for idempotency/audit, but a
	// settled session can no longer use it to refund committed consumption.
	assertSubscriptionReservation(t, info.RequestId, 150, "consumed")
	session.Refund(ctx)
	assert.Equal(t, int64(120), getSubscriptionUsed(t, 12_004))
}

func TestMasterSubscriptionRefundRestoresBaseAndExtraReserveOnce(t *testing.T) {
	truncate(t)
	ctx := newMasterBillingTestContext(1_000)
	seedUser(t, 12_011, 500)
	seedToken(t, 12_012, 12_011, "subscription-refund", 1_000)
	seedBillingSubscription(t, 12_013, 12_014, 12_011, 1_000, 0, true)
	info := masterBillingRelayInfo(12_011, 12_012, "subscription-refund", "subscription_only")
	info.RequestId = "subscription-refund-request"

	session, apiErr := NewBillingSession(ctx, info, 100)
	require.Nil(t, apiErr)
	require.NoError(t, session.Reserve(150))
	session.Refund(ctx)
	session.Refund(ctx)

	assert.Equal(t, int64(0), getSubscriptionUsed(t, 12_014))
	assert.Equal(t, 1_000, getTokenRemainQuota(t, info.TokenId))
	assertSubscriptionReservation(t, info.RequestId, 150, "refunded")
	assert.False(t, session.NeedsRefund())
}

func TestMasterSubscriptionZeroActualReleasesMinimumHold(t *testing.T) {
	truncate(t)
	ctx := newMasterBillingTestContext(1_000)
	seedUser(t, 12_021, 500)
	seedToken(t, 12_022, 12_021, "subscription-zero", 1_000)
	seedBillingSubscription(t, 12_023, 12_024, 12_021, 1_000, 0, true)
	info := masterBillingRelayInfo(12_021, 12_022, "subscription-zero", "subscription_only")
	info.RequestId = "subscription-zero-request"

	session, apiErr := NewBillingSession(ctx, info, 0)
	require.Nil(t, apiErr)
	assert.Equal(t, 1, session.GetPreConsumedQuota())
	assert.Equal(t, int64(1), getSubscriptionUsed(t, 12_024))
	assert.Equal(t, 999, getTokenRemainQuota(t, info.TokenId))

	require.NoError(t, session.Settle(0))
	assert.Equal(t, int64(0), getSubscriptionUsed(t, 12_024))
	assert.Equal(t, 1_000, getTokenRemainQuota(t, info.TokenId))
	assert.Equal(t, int64(-1), info.SubscriptionPostDelta)
}

func TestMasterBillingPreferenceFallbacksDoNotDoubleConsumeToken(t *testing.T) {
	t.Run("wallet first falls back to subscription before token deduction", func(t *testing.T) {
		truncate(t)
		ctx := newMasterBillingTestContext(1_000)
		seedUser(t, 13_001, 50)
		seedToken(t, 13_002, 13_001, "wallet-first", 1_000)
		seedBillingSubscription(t, 13_003, 13_004, 13_001, 1_000, 0, true)
		info := masterBillingRelayInfo(13_001, 13_002, "wallet-first", "wallet_first")
		info.RequestId = "wallet-first-request"

		session, apiErr := NewBillingSession(ctx, info, 100)
		require.Nil(t, apiErr)
		assert.Equal(t, BillingSourceSubscription, info.BillingSource)
		assert.Equal(t, 50, getUserQuota(t, info.UserId))
		assert.Equal(t, int64(100), getSubscriptionUsed(t, 13_004))
		assert.Equal(t, 900, getTokenRemainQuota(t, info.TokenId))
		session.Refund(ctx)
		assert.Equal(t, int64(0), getSubscriptionUsed(t, 13_004))
		assert.Equal(t, 1_000, getTokenRemainQuota(t, info.TokenId))
	})

	t.Run("subscription first rolls back failed subscription token hold before wallet fallback", func(t *testing.T) {
		truncate(t)
		ctx := newMasterBillingTestContext(1_000)
		seedUser(t, 13_011, 500)
		seedToken(t, 13_012, 13_011, "subscription-first-overflow", 1_000)
		seedBillingSubscription(t, 13_013, 13_014, 13_011, 100, 50, true)
		info := masterBillingRelayInfo(13_011, 13_012, "subscription-first-overflow", "subscription_first")
		info.RequestId = "subscription-first-overflow-request"

		session, apiErr := NewBillingSession(ctx, info, 100)
		require.Nil(t, apiErr)
		assert.Equal(t, BillingSourceWallet, info.BillingSource)
		assert.Equal(t, 400, getUserQuota(t, info.UserId))
		assert.Equal(t, int64(50), getSubscriptionUsed(t, 13_014))
		assert.Equal(t, 900, getTokenRemainQuota(t, info.TokenId))
		var records int64
		require.NoError(t, model.DB.Model(&model.SubscriptionPreConsumeRecord{}).Count(&records).Error)
		assert.Zero(t, records)
		session.Refund(ctx)
		assert.Equal(t, 500, getUserQuota(t, info.UserId))
		assert.Equal(t, 1_000, getTokenRemainQuota(t, info.TokenId))
	})

	t.Run("strict subscription prevents wallet fallback and restores token hold", func(t *testing.T) {
		truncate(t)
		ctx := newMasterBillingTestContext(1_000)
		seedUser(t, 13_021, 500)
		seedToken(t, 13_022, 13_021, "subscription-first-strict", 1_000)
		seedBillingSubscription(t, 13_023, 13_024, 13_021, 100, 50, false)
		info := masterBillingRelayInfo(13_021, 13_022, "subscription-first-strict", "subscription_first")
		info.RequestId = "subscription-first-strict-request"

		session, apiErr := NewBillingSession(ctx, info, 100)
		assert.Nil(t, session)
		require.NotNil(t, apiErr)
		assert.Equal(t, types.ErrorCodeInsufficientUserQuota, apiErr.GetErrorCode())
		assert.Equal(t, 500, getUserQuota(t, info.UserId))
		assert.Equal(t, int64(50), getSubscriptionUsed(t, 13_024))
		assert.Equal(t, 1_000, getTokenRemainQuota(t, info.TokenId))
	})
}

func newMasterBillingTestContext(tokenQuota int) *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("token_quota", tokenQuota)
	return ctx
}

func masterBillingRelayInfo(userID, tokenID int, tokenKey, preference string) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		UserId:          userID,
		TokenId:         tokenID,
		TokenKey:        tokenKey,
		OriginModelName: "billing-test-model",
		RequestId:       "billing-test-request",
		UserSetting: dto.UserSetting{
			BillingPreference: preference,
		},
	}
}

func seedBillingSubscription(t *testing.T, planID, subscriptionID, userID int, total, used int64, allowWalletOverflow bool) {
	t.Helper()
	plan := &model.SubscriptionPlan{
		Id: planID, Title: "billing test plan", DurationUnit: model.SubscriptionDurationMonth,
		DurationValue: 1, Enabled: true, TotalAmount: total, QuotaResetPeriod: model.SubscriptionResetNever,
	}
	require.NoError(t, model.DB.Create(plan).Error)
	subscription := &model.UserSubscription{
		Id: subscriptionID, UserId: userID, PlanId: planID,
		AmountTotal: total, AmountUsed: used, Status: "active",
		StartTime: time.Now().Add(-time.Hour).Unix(), EndTime: time.Now().Add(24 * time.Hour).Unix(),
		AllowWalletOverflow: allowWalletOverflow,
	}
	require.NoError(t, model.DB.Create(subscription).Error)
}

func assertSubscriptionReservation(t *testing.T, requestID string, preConsumed int64, status string) {
	t.Helper()
	var record model.SubscriptionPreConsumeRecord
	require.NoError(t, model.DB.Where("request_id = ?", requestID).First(&record).Error)
	assert.Equal(t, preConsumed, record.PreConsumed)
	assert.Equal(t, status, record.Status)
}
