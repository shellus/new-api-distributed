package edge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	coreservice "github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	defaultEdgeBalanceSettlementFloorQuota = -10_000_000
	edgeBalanceSettlementFloorQuotaEnv     = "EDGE_BALANCE_SETTLEMENT_FLOOR_QUOTA"
	legacyEdgeBalanceNegativeFloorQuotaEnv = "EDGE_BALANCE_NEGATIVE_FLOOR_QUOTA"
)

// BillingSessionFactory connects the shared BillingSession to the durable
// edge-local replicated balance ledger. It never contacts master on the user
// request path.
func BillingSessionFactory(c *gin.Context, preConsumedQuota int, relayInfo *relaycommon.RelayInfo) (*coreservice.BillingSession, *types.NewAPIError) {
	if c == nil || relayInfo == nil || model.DB == nil {
		return nil, edgeFundingAPIError(errors.New("edge billing is not ready"), http.StatusServiceUnavailable)
	}
	if preConsumedQuota < 0 || preConsumedQuota > common.MaxQuota {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("invalid edge pre-consume quota %d", preConsumedQuota),
			types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry(),
		)
	}
	if relayInfo.EdgePricingPolicy == nil {
		return nil, edgeFundingAPIError(errors.New("edge pricing policy is not pinned"), http.StatusServiceUnavailable)
	}
	floor, err := edgeBalanceSettlementFloorQuota()
	if err != nil {
		return nil, edgeFundingAPIError(err, http.StatusServiceUnavailable)
	}
	reservationID := "reservation-" + uuid.NewString()
	funding := &EdgeBalanceFunding{
		db: model.DB, responseStatus: func() int { return c.Writer.Status() },
		relayInfo: relayInfo, pricing: *relayInfo.EdgePricingPolicy,
		reservationID: reservationID, requestID: edgeUsageRequestID(relayInfo.RequestId, reservationID),
		settlementFloorQuota: floor,
	}
	session, apiErr := coreservice.NewBillingSessionWithFunding(c, relayInfo, preConsumedQuota, funding, coreservice.NoopTokenQuotaAccounting{})
	if apiErr == nil {
		relayInfo.EdgeReservationID = reservationID
		ReleaseEdgeRequestPolicy(c)
	}
	return session, apiErr
}

func edgeBalanceSettlementFloorQuota() (int64, error) {
	legacyValue := common.GetEnvOrDefault(legacyEdgeBalanceNegativeFloorQuotaEnv, defaultEdgeBalanceSettlementFloorQuota)
	value := int64(common.GetEnvOrDefault(edgeBalanceSettlementFloorQuotaEnv, legacyValue))
	if value < -int64(common.MaxQuota) || value > 0 {
		return 0, errors.New("EDGE_BALANCE_SETTLEMENT_FLOOR_QUOTA must be between -common.MaxQuota and 0")
	}
	return value, nil
}

func edgeUsageRequestID(sourceRequestID, reservationID string) string {
	digest := sha256.Sum256([]byte(sourceRequestID + "\x00" + reservationID))
	return "request-" + hex.EncodeToString(digest[:])[:56]
}

type EdgeBalanceFunding struct {
	db                   *gorm.DB
	responseStatus       func() int
	relayInfo            *relaycommon.RelayInfo
	pricing              dto.EdgePricingPolicyV1
	reservationID        string
	requestID            string
	settlementFloorQuota int64
	reservation          *model.EdgeLocalQuotaReservation
	reservedQuota        int64
	hasReservation       bool
	settledEvent         *dto.EdgeUsageEventV1
}

func (f *EdgeBalanceFunding) Source() string { return coreservice.BillingSourceEdgeBalance }

func (f *EdgeBalanceFunding) SyncBillingRelayInfo(info *relaycommon.RelayInfo) {
	if f == nil || info == nil || f.reservation == nil {
		return
	}
	info.EdgeSnapshotID = f.reservation.SnapshotID
	info.EdgeSnapshotRevision = f.reservation.SnapshotRevision
	info.EdgePricingRevision = f.reservation.PricingRevision
	switch f.reservation.FundingAccountType {
	case model.EdgeBalanceAccountTypeWallet:
		info.BillingSource = coreservice.BillingSourceWallet
		info.SubscriptionId = 0
		info.SubscriptionPreConsumed = 0
	case model.EdgeBalanceAccountTypeSubscription:
		info.BillingSource = coreservice.BillingSourceSubscription
		info.SubscriptionId = int(f.reservation.FundingAccountID)
		info.SubscriptionPreConsumed = f.reservedQuota
	}
}

func (f *EdgeBalanceFunding) PreConsume(amount int) error {
	if f == nil || f.db == nil || f.relayInfo == nil {
		return edgeFundingAPIError(errors.New("edge balance funding is incomplete"), http.StatusServiceUnavailable)
	}
	if amount < 0 || amount > common.MaxQuota {
		return edgeFundingAPIError(errors.New("edge balance reservation quota is invalid"), http.StatusBadRequest)
	}
	if EdgeAccountingSubjectQuarantined(int64(f.relayInfo.UserId), int64(f.relayInfo.TokenId)) {
		return edgeFundingAPIError(errEdgeAccountingSubjectQuarantined, http.StatusServiceUnavailable)
	}
	if f.hasReservation {
		if f.reservedQuota == int64(amount) {
			return nil
		}
		return model.ErrEdgeLocalReservationConflict
	}
	reservation, err := model.ReserveEdgeLocalBalance(f.db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: f.reservationID, RequestID: f.requestID,
		UserID: int64(f.relayInfo.UserId), TokenID: int64(f.relayInfo.TokenId), Quota: int64(amount),
		SettlementFloorQuota: f.settlementFloorQuota, NowUnixMilli: time.Now().UnixMilli(),
	})
	if err != nil {
		return edgeFundingAPIError(err, edgeFundingStatus(err))
	}
	f.reservation = reservation
	f.reservedQuota = reservation.ReservedQuota
	f.hasReservation = true
	return nil
}

func (f *EdgeBalanceFunding) Reserve(delta int) error {
	if f == nil || !f.hasReservation || f.reservation == nil {
		return model.ErrEdgeLocalQuotaInsufficient
	}
	target := f.reservedQuota + int64(delta)
	if target < 0 || target > int64(common.MaxQuota) {
		return errors.New("edge balance reserve adjustment exceeds the supported quota range")
	}
	adjusted, err := model.AdjustEdgeLocalBalanceReservation(f.db, f.reservationID, target, time.Now().UnixMilli())
	if err != nil {
		return edgeFundingAPIError(err, edgeFundingStatus(err))
	}
	f.reservation = adjusted
	f.reservedQuota = adjusted.ReservedQuota
	return nil
}

func (f *EdgeBalanceFunding) Settle(delta int) (settlementErr error) {
	staged := false
	defer func() {
		if settlementErr != nil {
			var reservation *model.EdgeLocalQuotaReservation
			if f != nil {
				reservation = f.reservation
			}
			MarkEdgeAccountingReservationFailure(staged, reservation)
		}
	}()
	if f == nil || !f.hasReservation || f.reservation == nil {
		return model.ErrEdgeLocalQuotaInsufficient
	}
	actualQuota := f.reservedQuota + int64(delta)
	if actualQuota < 0 || actualQuota > int64(common.MaxQuota) {
		return errors.New("edge balance settlement quota exceeds the supported range")
	}
	// A request that produced neither billable quota nor metered usage has no
	// settlement payload to persist. Release the hold with the ledger's refund
	// transition instead of turning an expected upstream/client-abort path into
	// an unrecoverable active reservation.
	if actualQuota == 0 && f.relayInfo.SettlementUsage == nil {
		return f.Refund()
	}
	if f.settledEvent == nil {
		event, err := f.buildUsageEvent(actualQuota)
		if err != nil {
			return err
		}
		f.settledEvent = event
	}
	if f.settledEvent.Billing.ChargedQuota != actualQuota {
		return model.ErrEdgeLocalSettlementConflict
	}
	if err := model.StageEdgeLocalReservationSettlement(f.db, f.reservationID, *f.settledEvent); err != nil {
		return err
	}
	staged = true
	settled, err := model.SettleStagedEdgeLocalReservation(f.db, f.reservationID)
	if err != nil {
		return err
	}
	f.settledEvent = settled
	f.reservedQuota = settled.Billing.ReservedQuota
	f.hasReservation = false
	refreshEdgeAccountingReadiness(f.db)
	return nil
}

func (f *EdgeBalanceFunding) Refund() error {
	if f == nil || !f.hasReservation {
		return nil
	}
	if err := model.RefundEdgeLocalReservation(f.db, f.reservationID, time.Now().UnixMilli()); err != nil {
		MarkEdgeAccountingReservationFailure(errors.Is(err, model.ErrEdgeLocalSettlementStaged), f.reservation)
		return err
	}
	f.hasReservation = false
	f.reservedQuota = 0
	return nil
}

func (f *EdgeBalanceFunding) HasReservation() bool { return f != nil && f.hasReservation }

func (f *EdgeBalanceFunding) buildUsageEvent(actualQuota int64) (*dto.EdgeUsageEventV1, error) {
	endpoint := edgeEndpointForRelayInfo(f.relayInfo)
	if f.relayInfo.SettlementUsage == nil && endpoint != dto.EdgeEndpointTaskV1 && endpoint != dto.EdgeEndpointMidjourneyV1 {
		return nil, errors.New("edge settlement usage is unavailable")
	}
	status := http.StatusOK
	if f.responseStatus != nil {
		status = f.responseStatus()
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		status = http.StatusOK
	}
	startedAt := f.relayInfo.StartTime.UnixMilli()
	finishedAt := time.Now().UnixMilli()
	if finishedAt < startedAt {
		finishedAt = startedAt
	}
	var firstResponseAtUnixMilli *int64
	if f.relayInfo.HasSendResponse() && f.relayInfo.FirstResponseTime.After(f.relayInfo.StartTime) {
		firstResponseAt := f.relayInfo.FirstResponseTime.UnixMilli()
		if firstResponseAt > startedAt && firstResponseAt <= finishedAt {
			firstResponseAtUnixMilli = &firstResponseAt
		}
	}
	consumeLogSnapshot, err := dto.CloneEdgeConsumeLogSnapshotV1(f.relayInfo.EdgeConsumeLogSnapshot)
	if err != nil {
		return nil, fmt.Errorf("clone edge consume-log snapshot: %w", err)
	}
	if consumeLogSnapshot != nil && consumeLogSnapshot.Other != nil {
		delete(consumeLogSnapshot.Other, "frt")
	}
	facts := dto.EdgeBillingFactsV1{}
	if f.relayInfo.EdgeBillingFacts != nil {
		facts = *f.relayInfo.EdgeBillingFacts
		if len(f.relayInfo.EdgeBillingFacts.ToolCalls) > 0 {
			facts.ToolCalls = make(map[string]int, len(f.relayInfo.EdgeBillingFacts.ToolCalls))
			for name, count := range f.relayInfo.EdgeBillingFacts.ToolCalls {
				facts.ToolCalls[name] = count
			}
		}
		if f.relayInfo.EdgeBillingFacts.TieredQuotaBeforeGroup != nil {
			value := *f.relayInfo.EdgeBillingFacts.TieredQuotaBeforeGroup
			facts.TieredQuotaBeforeGroup = &value
		}
	}
	return &dto.EdgeUsageEventV1{
		EventID: "event-" + uuid.NewString(), ChannelID: int64(f.relayInfo.ChannelId),
		Endpoint: endpoint, Streaming: f.relayInfo.IsStream, Model: f.relayInfo.OriginModelName,
		Group: f.relayInfo.UsingGroup, StartedAtUnixMilli: startedAt,
		FirstResponseAtUnixMilli: firstResponseAtUnixMilli, FinishedAtUnixMilli: finishedAt,
		Outcome: dto.EdgeUsageOutcomeSuccessV1, HTTPStatus: &status,
		Usage: dto.CloneBillingUsage(f.relayInfo.SettlementUsage), ConsumeLogSnapshot: consumeLogSnapshot,
		Billing: dto.EdgeUsageBillingV1{
			PricingPolicyID: f.pricing.PolicyID, PricingPolicyVersion: f.pricing.Version,
			BillingMode: f.pricing.BillingMode, GroupRatio: f.relayInfo.PriceData.GroupRatioInfo.GroupRatio,
			AppliedRatios:         f.relayInfo.PriceData.OtherRatios(),
			BillingExpressionHash: f.pricing.BillingExpressionHash,
			MatchedTier:           f.relayInfo.EdgeBillingMatchedTier,
			Facts:                 facts,
			ChargedQuota:          actualQuota,
		},
	}, nil
}

func edgeEndpointForRelayInfo(info *relaycommon.RelayInfo) dto.EdgeEndpointV1 {
	if info == nil {
		return dto.EdgeEndpointDataPlaneV1
	}
	switch info.RelayFormat {
	case types.RelayFormatClaude:
		return dto.EdgeEndpointClaudeMessagesV1
	case types.RelayFormatOpenAIResponses:
		return dto.EdgeEndpointOpenAIResponsesV1
	case types.RelayFormatOpenAIResponsesCompaction:
		return dto.EdgeEndpointOpenAIResponsesCompactV1
	case types.RelayFormatOpenAIImage:
		return dto.EdgeEndpointOpenAIImagesV1
	case types.RelayFormatEmbedding:
		return dto.EdgeEndpointOpenAIEmbeddingsV1
	case types.RelayFormatOpenAIAudio:
		return dto.EdgeEndpointOpenAIAudioV1
	case types.RelayFormatRerank:
		return dto.EdgeEndpointOpenAIRerankV1
	case types.RelayFormatGemini:
		return dto.EdgeEndpointGeminiV1
	case types.RelayFormatOpenAIRealtime:
		return dto.EdgeEndpointOpenAIRealtimeV1
	case types.RelayFormatTask:
		return dto.EdgeEndpointTaskV1
	case types.RelayFormatMjProxy:
		return dto.EdgeEndpointMidjourneyV1
	case types.RelayFormatOpenAI:
		if info.RelayMode == relayconstant.RelayModeCompletions {
			return dto.EdgeEndpointOpenAICompletionsV1
		}
		return dto.EdgeEndpointOpenAIChatCompletionsV1
	default:
		return dto.EdgeEndpointDataPlaneV1
	}
}

func edgeFundingStatus(err error) int {
	if errors.Is(err, model.ErrEdgeLocalQuotaInsufficient) {
		return http.StatusForbidden
	}
	if errors.Is(err, model.ErrEdgeLocalSnapshotMismatch) || errors.Is(err, gorm.ErrRecordNotFound) {
		return http.StatusServiceUnavailable
	}
	return http.StatusServiceUnavailable
}

func edgeFundingAPIError(err error, status int) *types.NewAPIError {
	code := types.ErrorCodeUpdateDataError
	options := []types.NewAPIErrorOptions{types.ErrOptionWithSkipRetry()}
	if status == http.StatusForbidden {
		code = types.ErrorCodeInsufficientUserQuota
		options = append(options, types.ErrOptionWithNoRecordErrorLog())
	}
	return types.NewErrorWithStatusCode(err, code, status, options...)
}
