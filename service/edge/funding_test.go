package edge

import (
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	coreservice "github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEdgeBalanceFundingWorksWithoutMasterClient(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	require.Nil(t, activeEdgeControlClient.Load())
	funding := newEdgeBalanceFundingForTest(db, now, "reservation-offline", "request-offline")

	require.NoError(t, funding.PreConsume(100))
	assert.True(t, funding.HasReservation())
	require.NoError(t, funding.Settle(0))
	assert.False(t, funding.HasReservation())

	var event model.EdgeLocalUsageEvent
	require.NoError(t, db.First(&event).Error)
	var payload dto.EdgeUsageEventV1
	require.NoError(t, common.UnmarshalJsonStr(event.Payload, &payload))
	assert.Equal(t, "subscription", payload.FundingSource)
	assert.Equal(t, int64(21), payload.UserSubscriptionID)
	assert.Equal(t, int64(1), payload.BalanceRevision)
}

func TestEdgeBalanceFundingSettlementCanExceedPreConsumeWithinFloor(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	funding := newEdgeBalanceFundingForTest(db, now, "reservation-supplement", "request-supplement")
	funding.negativeFloorQuota = -1_000_000

	require.NoError(t, funding.PreConsume(100))
	require.NoError(t, funding.Settle(50))
	reservation, err := model.GetEdgeLocalReservation(db, funding.reservationID)
	require.NoError(t, err)
	assert.Equal(t, int64(100), reservation.ReservedQuota)
	assert.Equal(t, int64(150), reservation.ChargedQuota)
	account := model.EdgeLocalBalanceAccount{}
	require.NoError(t, db.Where("account_type = ? AND account_id = ?", model.EdgeBalanceAccountTypeSubscription, 21).First(&account).Error)
	assert.Zero(t, account.ReservedQuota)
	assert.Equal(t, int64(150), account.UnsettledQuota)
}

func TestEdgeUsageRequestIDCanonicalizesProcessRequestIDForDurableProtocol(t *testing.T) {
	requestID := edgeUsageRequestID("202607152104520537832988268d9d6tytKhT2g", "reservation-request-id")
	assert.Len(t, requestID, 64)
	assert.Equal(t, "request-", requestID[:8])
	assert.Equal(t, requestID, edgeUsageRequestID("202607152104520537832988268d9d6tytKhT2g", "reservation-request-id"))
	assert.NotEqual(t, requestID, edgeUsageRequestID("202607152104520537832988268d9d6tytKhT2g", "reservation-request-id-other"))
}

func newEdgeBalanceFundingForTest(db *gorm.DB, now time.Time, reservationID, requestID string) *EdgeBalanceFunding {
	return &EdgeBalanceFunding{
		db: db, responseStatus: func() int { return http.StatusOK },
		relayInfo: &relaycommon.RelayInfo{
			UserId: 7, TokenId: 11, UsingGroup: "default", StartTime: now.Add(-time.Second),
			OriginModelName: "gpt-test", RequestURLPath: "/v1/chat/completions",
			SettlementUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}),
			PriceData:       types.PriceData{GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}},
			ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: 31},
		},
		pricing: dto.EdgePricingPolicyV1{
			PolicyID: "pricing-runtime", Version: "v1", Model: "gpt-test",
			BillingMode: dto.EdgeBillingModeFixedPriceV1,
		},
		reservationID: reservationID, requestID: requestID,
		negativeFloorQuota: defaultEdgeBalanceNegativeFloorQuota,
	}
}

var _ coreservice.FundingSource = (*EdgeBalanceFunding)(nil)
