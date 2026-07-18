package edge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	coreservice "github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const defaultEdgeBalanceNegativeFloorQuota = -10_000_000

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
	floor, err := edgeBalanceNegativeFloorQuota()
	if err != nil {
		return nil, edgeFundingAPIError(err, http.StatusServiceUnavailable)
	}
	reservationID := "reservation-" + uuid.NewString()
	funding := &EdgeBalanceFunding{
		db: model.DB, responseStatus: func() int { return c.Writer.Status() },
		relayInfo: relayInfo, pricing: *relayInfo.EdgePricingPolicy,
		reservationID: reservationID, requestID: edgeUsageRequestID(relayInfo.RequestId, reservationID),
		negativeFloorQuota: floor,
	}
	session, apiErr := coreservice.NewBillingSessionWithFunding(c, relayInfo, preConsumedQuota, funding, coreservice.NoopTokenQuotaAccounting{})
	if apiErr == nil {
		ReleaseEdgeRequestPolicy(c)
	}
	return session, apiErr
}

func edgeBalanceNegativeFloorQuota() (int64, error) {
	value := int64(common.GetEnvOrDefault("EDGE_BALANCE_NEGATIVE_FLOOR_QUOTA", defaultEdgeBalanceNegativeFloorQuota))
	if value < -int64(common.MaxQuota) || value > 0 {
		return 0, errors.New("EDGE_BALANCE_NEGATIVE_FLOOR_QUOTA must be between -common.MaxQuota and 0")
	}
	return value, nil
}

func edgeUsageRequestID(sourceRequestID, reservationID string) string {
	digest := sha256.Sum256([]byte(sourceRequestID + "\x00" + reservationID))
	return "request-" + hex.EncodeToString(digest[:])[:56]
}

type EdgeBalanceFunding struct {
	db                 *gorm.DB
	responseStatus     func() int
	relayInfo          *relaycommon.RelayInfo
	pricing            dto.EdgePricingPolicyV1
	reservationID      string
	requestID          string
	negativeFloorQuota int64
	reservation        *model.EdgeLocalQuotaReservation
	reservedQuota      int64
	hasReservation     bool
	settledEvent       *dto.EdgeUsageEventV1
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
	if f.hasReservation {
		if f.reservedQuota == int64(amount) {
			return nil
		}
		return model.ErrEdgeLocalReservationConflict
	}
	reservation, err := model.ReserveEdgeLocalBalance(f.db, model.EdgeLocalBalanceReservationRequest{
		ReservationID: f.reservationID, RequestID: f.requestID,
		UserID: int64(f.relayInfo.UserId), TokenID: int64(f.relayInfo.TokenId), Quota: int64(amount),
		NegativeFloorQuota: f.negativeFloorQuota, NowUnixMilli: time.Now().UnixMilli(),
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
			MarkEdgeAccountingFailure(staged)
		}
	}()
	if f == nil || !f.hasReservation || f.reservation == nil {
		return model.ErrEdgeLocalQuotaInsufficient
	}
	actualQuota := f.reservedQuota + int64(delta)
	if actualQuota < 0 || actualQuota > int64(common.MaxQuota) {
		return errors.New("edge balance settlement quota exceeds the supported range")
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
		MarkEdgeAccountingFailure(errors.Is(err, model.ErrEdgeLocalSettlementStaged))
		return err
	}
	f.hasReservation = false
	f.reservedQuota = 0
	return nil
}

func (f *EdgeBalanceFunding) HasReservation() bool { return f != nil && f.hasReservation }

func (f *EdgeBalanceFunding) buildUsageEvent(actualQuota int64) (*dto.EdgeUsageEventV1, error) {
	if f.relayInfo.SettlementUsage == nil {
		return nil, errors.New("edge settlement usage is unavailable")
	}
	if len(f.relayInfo.PriceData.OtherRatios()) != 0 {
		return nil, errors.New("edge settlement v2 does not support request-specific billing multipliers")
	}
	if f.pricing.BillingMode == dto.EdgeBillingModeTieredExprV1 {
		return nil, errors.New("tiered billing is not supported by edge settlement v2")
	}
	endpoint := dto.EdgeEndpointOpenAIChatCompletionsV1
	if strings.HasPrefix(f.relayInfo.RequestURLPath, "/v1/responses") {
		endpoint = dto.EdgeEndpointOpenAIResponsesV1
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
	return &dto.EdgeUsageEventV1{
		EventID: "event-" + uuid.NewString(), ChannelID: int64(f.relayInfo.ChannelId),
		Endpoint: endpoint, Streaming: f.relayInfo.IsStream, Model: f.relayInfo.OriginModelName,
		Group: f.relayInfo.UsingGroup, StartedAtUnixMilli: startedAt, FinishedAtUnixMilli: finishedAt,
		Outcome: dto.EdgeUsageOutcomeSuccessV1, HTTPStatus: &status,
		Usage: dto.CloneBillingUsage(f.relayInfo.SettlementUsage),
		Billing: dto.EdgeUsageBillingV1{
			PricingPolicyID: f.pricing.PolicyID, PricingPolicyVersion: f.pricing.Version,
			BillingMode: f.pricing.BillingMode, GroupRatio: f.relayInfo.PriceData.GroupRatioInfo.GroupRatio,
			ChargedQuota: actualQuota,
		},
	}, nil
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
