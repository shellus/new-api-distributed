package edge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayhelper "github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEdgeLeaseFundingAcquiresSynchronouslyAndReusesDurableIntent(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	requests := make([]dto.EdgeLeaseAcquireRequestV1, 0, 2)
	client := newEdgeRuntimeTestControlClient(t, edgeRuntimeRoundTripper(func(request *http.Request) (*http.Response, error) {
		require.Equal(t, "/control/v1/lease/acquire", request.URL.Path)
		var acquire dto.EdgeLeaseAcquireRequestV1
		decodeEdgeRuntimeRequest(t, request, &acquire)
		requests = append(requests, acquire)
		if len(requests) == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		lease := edgeRuntimeTestLease(now, "lease-acquired", acquire.Subject.UserID, acquire.Subject.TokenID, acquire.RequestedQuota)
		lease.SnapshotID = acquire.SnapshotID
		lease.SnapshotRevision = acquire.SnapshotRevision
		lease.PricingRevision = acquire.SnapshotRevision
		return edgeRuntimeJSONResponse(t, http.StatusOK, dto.EdgeLeaseAcquireResponseV1{
			Meta: edgeRuntimeResponseMeta(acquire.Meta.RequestID), Lease: lease,
		}), nil
	}))
	installEdgeRuntimeTestClient(t, client)

	first := &EdgeLeaseFunding{
		db: db, requestContext: context.Background(),
		relayInfo:     &relaycommon.RelayInfo{UserId: 7, TokenId: 11},
		reservationID: "reservation-acquire-first", requestID: "request-acquire-first",
	}
	err := first.PreConsume(100)
	require.Error(t, err)
	assert.False(t, first.HasReservation())
	durable, err := model.GetEdgeLocalLeaseAcquireIntent(db, 7, 11)
	require.NoError(t, err)
	assert.Equal(t, requests[0], *durable)

	second := &EdgeLeaseFunding{
		db: db, requestContext: context.Background(),
		relayInfo:     &relaycommon.RelayInfo{UserId: 7, TokenId: 11},
		reservationID: "reservation-acquire-second", requestID: "request-acquire-second",
	}
	require.NoError(t, second.PreConsume(100))
	require.True(t, second.HasReservation())
	require.Len(t, requests, 2)
	assert.Equal(t, requests[0], requests[1], "retry must reuse the exact durable request and idempotency key")
	_, err = model.GetEdgeLocalLeaseAcquireIntent(db, 7, 11)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)

	lease := requireEdgeRuntimeLease(t, db, "lease-acquired")
	assert.Equal(t, int64(100), lease.ReservedQuota)
	assert.Equal(t, lease.GrantedQuota-100, lease.RemainingQuota)
	reservation := activeEdgeRuntimeReservation(t, db, "reservation-acquire-second")
	assert.Equal(t, "lease-acquired", reservation.LeaseID)
}

func TestEdgeUsageRequestIDCanonicalizesProcessRequestIDForDurableProtocol(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	lease := edgeRuntimeTestLease(now, "lease-request-id", 7, 11, 100)
	require.NoError(t, model.InstallEdgeLocalLease(db, lease))

	requestID := edgeUsageRequestID("202607152104520537832988268d9d6tytKhT2g", "reservation-request-id")
	assert.Len(t, requestID, 64)
	assert.Equal(t, "request-", requestID[:8])
	assert.Equal(t, requestID, edgeUsageRequestID("202607152104520537832988268d9d6tytKhT2g", "reservation-request-id"))
	assert.NotEqual(t, requestID, edgeUsageRequestID("202607152104520537832988268d9d6tytKhT2g", "reservation-request-id-other"))
	_, err := model.ReserveEdgeLocalQuota(db, model.EdgeLocalReservationRequest{
		ReservationID: "reservation-request-id", RequestID: requestID, LeaseID: lease.LeaseID,
		Quota: 1, NowUnixMilli: now.UnixMilli(),
	})
	require.NoError(t, err, "mixed-case process IDs must map to a protocol-valid durable request ID")
}

func TestEdgeLeaseFundingRefundFailureClosesAccountingAdmission(t *testing.T) {
	previousReady := edgeAccountingReady.Load()
	previousBlock := edgeAccountingBlock.Load()
	edgeAccountingReady.Store(true)
	edgeAccountingBlock.Store(false)
	t.Cleanup(func() {
		edgeAccountingReady.Store(previousReady)
		edgeAccountingBlock.Store(previousBlock)
	})

	funding := &EdgeLeaseFunding{hasReservation: true, reservationID: "reservation-refund-failure"}
	err := funding.Refund()
	require.Error(t, err)
	assert.False(t, edgeAccountingReady.Load())
	assert.True(t, edgeAccountingBlock.Load(), "an unstaged refund failure must require operator recovery")
}

func TestEdgeLeaseFundingSettlementSafelySupplementsActualQuota(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	enableEdgeRuntimeServing(t)
	pricing := dto.EdgePricingPolicyV1{
		PolicyID: "pricing-runtime", Version: "v1", Model: "gpt-test",
		BillingMode: dto.EdgeBillingModeFixedPriceV1,
	}
	newFunding := func(userID, tokenID int, reservationID, requestID string) *EdgeLeaseFunding {
		return &EdgeLeaseFunding{
			db: db, requestContext: context.Background(), responseStatus: func() int { return http.StatusOK },
			relayInfo: &relaycommon.RelayInfo{
				UserId: userID, TokenId: tokenID, UsingGroup: "default", StartTime: now.Add(-time.Second),
				OriginModelName: "gpt-test", RequestURLPath: "/v1/chat/completions",
				SettlementUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}),
				PriceData:       types.PriceData{GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}},
				ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: 31},
			},
			pricing: pricing, reservationID: reservationID, requestID: requestID,
		}
	}

	successLease := edgeRuntimeTestLease(now, "lease-supplement-success", 7, 11, 200)
	require.NoError(t, model.InstallEdgeLocalLease(db, successLease))
	success := newFunding(7, 11, "reservation-supplement-success", "request-supplement-success")
	require.NoError(t, success.PreConsume(100))
	require.NoError(t, success.Settle(50))
	assert.False(t, success.HasReservation())
	lease := requireEdgeRuntimeLease(t, db, successLease.LeaseID)
	assert.Equal(t, int64(50), lease.RemainingQuota)
	assert.Zero(t, lease.ReservedQuota)
	assert.Equal(t, int64(150), lease.ConsumedQuota)
	reservation, err := model.GetEdgeLocalReservation(db, "reservation-supplement-success")
	require.NoError(t, err)
	assert.Equal(t, model.EdgeLocalReservationStatusSettled, reservation.Status)
	assert.Equal(t, int64(150), reservation.ReservedQuota)
	assert.Equal(t, int64(150), reservation.ChargedQuota)
	require.NotNil(t, success.settledEvent)
	assert.Equal(t, int64(150), success.settledEvent.Billing.ReservedQuota)
	assert.Equal(t, int64(150), success.settledEvent.Billing.ChargedQuota)

	insufficientLease := edgeRuntimeTestLease(now, "lease-supplement-insufficient", 8, 12, 120)
	require.NoError(t, model.InstallEdgeLocalLease(db, insufficientLease))
	insufficient := newFunding(8, 12, "reservation-supplement-insufficient", "request-supplement-insufficient")
	require.NoError(t, insufficient.PreConsume(100))
	err = insufficient.Settle(50)
	assert.True(t, errors.Is(err, model.ErrEdgeLocalQuotaInsufficient))
	assert.False(t, EdgeServingReady(), "a durable staged settlement must close admission until recovery succeeds")
	assert.True(t, insufficient.HasReservation())
	lease = requireEdgeRuntimeLease(t, db, insufficientLease.LeaseID)
	assert.Equal(t, int64(20), lease.RemainingQuota)
	assert.Equal(t, int64(100), lease.ReservedQuota)
	assert.Zero(t, lease.ConsumedQuota)
	reservation = activeEdgeRuntimeReservation(t, db, "reservation-supplement-insufficient")
	assert.Equal(t, int64(100), reservation.ReservedQuota)
	assert.Zero(t, reservation.ChargedQuota)
}

func TestEdgeLeaseFundingAcquiresZeroGrantForSignedFreePricing(t *testing.T) {
	zero := 0.0
	nonzero := 0.001
	tests := []struct {
		name       string
		groupRatio float64
		pricing    dto.EdgePricingPolicyV1
	}{
		{
			name: "fixed price zero", groupRatio: 1,
			pricing: dto.EdgePricingPolicyV1{
				PolicyID: "pricing-free", Version: "v1", Model: "gpt-test",
				BillingMode: dto.EdgeBillingModeFixedPriceV1, ModelPrice: &zero, QuotaPerUnit: 500_000,
			},
		},
		{
			name: "ratio zero", groupRatio: 1,
			pricing: dto.EdgePricingPolicyV1{
				PolicyID: "pricing-free", Version: "v1", Model: "gpt-test",
				BillingMode: dto.EdgeBillingModeRatioV1, ModelRatio: &zero, QuotaPerUnit: 500_000,
			},
		},
		{
			name: "group ratio zero", groupRatio: 0,
			pricing: dto.EdgePricingPolicyV1{
				PolicyID: "pricing-free", Version: "v1", Model: "gpt-test",
				BillingMode: dto.EdgeBillingModeFixedPriceV1, ModelPrice: &nonzero, QuotaPerUnit: 500_000,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, now := newEdgeRuntimeTestDB(t, "")
			client := newEdgeRuntimeTestControlClient(t, edgeRuntimeRoundTripper(func(request *http.Request) (*http.Response, error) {
				var acquire dto.EdgeLeaseAcquireRequestV1
				decodeEdgeRuntimeRequest(t, request, &acquire)
				assert.Zero(t, acquire.RequestedQuota)
				assert.Zero(t, acquire.MinimumAcceptableQuota)
				lease := edgeRuntimeTestLease(now, "lease-free", acquire.Subject.UserID, acquire.Subject.TokenID, 0)
				lease.RenewAfterRemainingQuota = 0
				lease.SnapshotID = acquire.SnapshotID
				lease.SnapshotRevision = acquire.SnapshotRevision
				lease.PricingRevision = acquire.SnapshotRevision
				return edgeRuntimeJSONResponse(t, http.StatusOK, dto.EdgeLeaseAcquireResponseV1{
					Meta: edgeRuntimeResponseMeta(acquire.Meta.RequestID), Lease: lease,
				}), nil
			}))
			installEdgeRuntimeTestClient(t, client)
			funding := &EdgeLeaseFunding{
				db: db, requestContext: context.Background(), responseStatus: func() int { return http.StatusOK },
				relayInfo: &relaycommon.RelayInfo{
					UserId: 7, TokenId: 11, UsingGroup: "default", OriginModelName: "gpt-test",
					RequestURLPath: "/v1/chat/completions", StartTime: now.Add(-time.Second),
					SettlementUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 1, TotalTokens: 1}),
					ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: 31},
					PriceData: types.PriceData{
						FreeModel: true,
						GroupRatioInfo: types.GroupRatioInfo{
							GroupRatio: test.groupRatio,
						},
					},
				},
				pricing: test.pricing, reservationID: "reservation-free", requestID: "request-free",
			}
			require.NoError(t, funding.PreConsume(0))
			assert.True(t, funding.HasReservation())
			lease := requireEdgeRuntimeLease(t, db, "lease-free")
			assert.Zero(t, lease.GrantedQuota)
			assert.Zero(t, lease.RemainingQuota)
			assert.Zero(t, lease.ReservedQuota)
			reservation := activeEdgeRuntimeReservation(t, db, "reservation-free")
			assert.Zero(t, reservation.ReservedQuota)
			require.NoError(t, funding.Settle(0))
			assert.False(t, funding.HasReservation())
			lease = requireEdgeRuntimeLease(t, db, "lease-free")
			assert.Zero(t, lease.RemainingQuota)
			assert.Zero(t, lease.ReservedQuota)
			assert.Zero(t, lease.ConsumedQuota)
			var events int64
			require.NoError(t, db.Model(&model.EdgeLocalUsageEvent{}).Count(&events).Error)
			assert.Equal(t, int64(1), events)
		})
	}
}

func TestEdgeFreeRatioSnapshotFlowsThroughPricingLeaseAndUsageEvent(t *testing.T) {
	db, now := newEdgeRuntimeTestDB(t, "")
	previousMode := common.CurrentRuntimeMode()
	require.NoError(t, common.SetRuntimeMode(common.RuntimeModeEdge))
	t.Cleanup(func() { require.NoError(t, common.SetRuntimeMode(previousMode)) })

	zero := 0.0
	pricing, err := projectEdgeSnapshotPricing("gpt-test", edgeSnapshotPricingInput{
		Mode: billing_setting.BillingModeRatio, ModelRatio: &zero, QuotaPerUnit: 500_000,
	})
	require.NoError(t, err)
	payload, err := common.Marshal(pricing)
	require.NoError(t, err)
	require.NoError(t, db.Where("model = ?", "gpt-test").Delete(&model.EdgeLocalPricingProjection{}).Error)
	require.NoError(t, db.Create(&model.EdgeLocalPricingProjection{
		PolicyID: pricing.PolicyID, Version: pricing.Version, Model: pricing.Model, Payload: string(payload),
	}).Error)

	gin.SetMode(gin.TestMode)
	requestContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(requestContext, constant.ContextKeyEdgeGroupRatio, 1.0)
	relayInfo := &relaycommon.RelayInfo{
		RequestId: "request-free-e2e", UserId: 7, TokenId: 11,
		UsingGroup: "default", OriginModelName: "gpt-test",
		RequestURLPath: "/v1/chat/completions", StartTime: now.Add(-time.Second),
		SettlementUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}),
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: 31},
	}
	priceData, err := relayhelper.ModelPriceHelper(requestContext, relayInfo, 100, &types.TokenCountMeta{MaxTokens: 50})
	require.NoError(t, err)
	assert.True(t, priceData.FreeModel)
	assert.Zero(t, priceData.QuotaToPreConsume)
	require.NotNil(t, relayInfo.EdgePricingPolicy)
	require.NotNil(t, relayInfo.EdgePricingPolicy.ModelRatio)
	assert.Zero(t, *relayInfo.EdgePricingPolicy.ModelRatio)

	client := newEdgeRuntimeTestControlClient(t, edgeRuntimeRoundTripper(func(request *http.Request) (*http.Response, error) {
		var acquire dto.EdgeLeaseAcquireRequestV1
		decodeEdgeRuntimeRequest(t, request, &acquire)
		assert.Zero(t, acquire.RequestedQuota)
		assert.Zero(t, acquire.MinimumAcceptableQuota)
		lease := edgeRuntimeTestLease(now, "lease-free-e2e", acquire.Subject.UserID, acquire.Subject.TokenID, 0)
		lease.RenewAfterRemainingQuota = 0
		lease.SnapshotID = acquire.SnapshotID
		lease.SnapshotRevision = acquire.SnapshotRevision
		lease.PricingRevision = acquire.SnapshotRevision
		return edgeRuntimeJSONResponse(t, http.StatusOK, dto.EdgeLeaseAcquireResponseV1{
			Meta: edgeRuntimeResponseMeta(acquire.Meta.RequestID), Lease: lease,
		}), nil
	}))
	installEdgeRuntimeTestClient(t, client)
	funding := &EdgeLeaseFunding{
		db: db, requestContext: context.Background(), responseStatus: func() int { return http.StatusOK },
		relayInfo: relayInfo, pricing: *relayInfo.EdgePricingPolicy,
		reservationID: "reservation-free-e2e", requestID: relayInfo.RequestId,
	}
	require.NoError(t, funding.PreConsume(priceData.QuotaToPreConsume))
	assert.Equal(t, "lease-free-e2e", funding.leaseID)
	require.NoError(t, funding.Settle(0))

	var stored model.EdgeLocalUsageEvent
	require.NoError(t, db.First(&stored).Error)
	var event dto.EdgeUsageEventV1
	require.NoError(t, common.UnmarshalJsonStr(stored.Payload, &event))
	assert.Equal(t, dto.EdgeBillingModeRatioV1, event.Billing.BillingMode)
	assert.Equal(t, pricing.PolicyID, event.Billing.PricingPolicyID)
	assert.Equal(t, 1.0, event.Billing.GroupRatio)
	assert.Zero(t, event.Billing.ReservedQuota)
	assert.Zero(t, event.Billing.ChargedQuota)
	require.NotNil(t, event.Usage)
}
