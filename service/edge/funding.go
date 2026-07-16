package edge

import (
	"context"
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
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

const (
	defaultEdgeLeaseRequestQuota = 100_000
	defaultEdgeLeaseMinimumQuota = 1_000
)

var edgeLeaseAcquireGroup singleflight.Group

// BillingSessionFactory connects the shared BillingSession to the durable
// edge-local lease reservation state. The only synchronous master call is a
// lease acquisition after local authorization succeeds and no usable lease can
// fund the request.
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
	reservationID := "reservation-" + uuid.NewString()
	funding := &EdgeLeaseFunding{
		db: model.DB, requestContext: c.Request.Context(), responseStatus: func() int { return c.Writer.Status() },
		relayInfo: relayInfo, pricing: *relayInfo.EdgePricingPolicy,
		reservationID: reservationID, requestID: edgeUsageRequestID(relayInfo.RequestId, reservationID),
	}
	session, apiErr := coreservice.NewBillingSessionWithFunding(c, relayInfo, preConsumedQuota, funding, coreservice.NoopTokenQuotaAccounting{})
	if apiErr == nil {
		ReleaseEdgeRequestPolicy(c)
	}
	return session, apiErr
}

// edgeUsageRequestID maps the process-level request ID into the strict,
// lowercase identifier alphabet used by the durable edge settlement protocol.
// The reservation ID is included so even an absent or reused source request ID
// cannot collide with another local reservation.
func edgeUsageRequestID(sourceRequestID, reservationID string) string {
	digest := sha256.Sum256([]byte(sourceRequestID + "\x00" + reservationID))
	return "request-" + hex.EncodeToString(digest[:])[:56]
}

type EdgeLeaseFunding struct {
	db             *gorm.DB
	requestContext context.Context
	responseStatus func() int
	relayInfo      *relaycommon.RelayInfo
	pricing        dto.EdgePricingPolicyV1
	reservationID  string
	requestID      string
	leaseID        string
	reservedQuota  int64
	hasReservation bool
	settledEvent   *dto.EdgeUsageEventV1
}

func (f *EdgeLeaseFunding) Source() string { return coreservice.BillingSourceEdgeLease }

func (f *EdgeLeaseFunding) PreConsume(amount int) error {
	if f == nil || f.db == nil || f.relayInfo == nil {
		return edgeFundingAPIError(errors.New("edge lease funding is incomplete"), http.StatusServiceUnavailable)
	}
	if amount < 0 || amount > common.MaxQuota {
		return edgeFundingAPIError(errors.New("edge lease reservation quota is invalid"), http.StatusBadRequest)
	}
	if f.hasReservation {
		if f.reservedQuota == int64(amount) {
			return nil
		}
		return model.ErrEdgeLocalReservationConflict
	}
	leaseID, err := f.reserveFromLocalLeases(int64(amount))
	if err == nil {
		f.leaseID = leaseID
		f.reservedQuota = int64(amount)
		f.hasReservation = true
		return nil
	}
	if !edgeLeaseNeedsAcquisition(err) {
		return edgeFundingAPIError(err, edgeFundingStatus(err))
	}
	if err := f.acquireLease(int64(amount), false); err != nil {
		return err
	}
	leaseID, err = f.reserveFromLocalLeases(int64(amount))
	if err != nil {
		return edgeFundingAPIError(err, edgeFundingStatus(err))
	}
	f.leaseID = leaseID
	f.reservedQuota = int64(amount)
	f.hasReservation = true
	return nil
}

func (f *EdgeLeaseFunding) Reserve(delta int) error {
	if f == nil || !f.hasReservation {
		return model.ErrEdgeLocalLeaseUnavailable
	}
	target := f.reservedQuota + int64(delta)
	if target < 0 || target > int64(common.MaxQuota) {
		return errors.New("edge lease reserve adjustment exceeds the supported quota range")
	}
	adjusted, err := model.AdjustEdgeLocalReservation(f.db, f.reservationID, target, time.Now().UnixMilli())
	if err != nil {
		return edgeFundingAPIError(err, edgeFundingStatus(err))
	}
	f.reservedQuota = adjusted.ReservedQuota
	return nil
}

func (f *EdgeLeaseFunding) Settle(delta int) (settlementErr error) {
	staged := false
	defer func() {
		if settlementErr != nil {
			MarkEdgeAccountingFailure(staged)
		}
	}()
	if f == nil || !f.hasReservation {
		return model.ErrEdgeLocalLeaseUnavailable
	}
	actualQuota := f.reservedQuota + int64(delta)
	if actualQuota < 0 || actualQuota > int64(common.MaxQuota) {
		return errors.New("edge lease settlement quota exceeds the supported range")
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

func (f *EdgeLeaseFunding) Refund() error {
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

func (f *EdgeLeaseFunding) HasReservation() bool {
	return f != nil && f.hasReservation
}

func (f *EdgeLeaseFunding) reserveFromLocalLeases(quota int64) (string, error) {
	now := time.Now().UnixMilli()
	var leases []model.EdgeLocalQuotaLease
	if err := f.db.Where("user_id = ? AND token_id = ? AND status = ? AND expires_at_unix_milli > ?",
		f.relayInfo.UserId, f.relayInfo.TokenId, dto.EdgeLeaseStatusActiveV1, now).
		Order("issued_at_unix_milli desc, lease_id asc").Find(&leases).Error; err != nil {
		return "", err
	}
	if len(leases) == 0 {
		return "", model.ErrEdgeLocalLeaseUnavailable
	}
	var lastErr error = model.ErrEdgeLocalQuotaInsufficient
	for _, lease := range leases {
		_, err := model.ReserveEdgeLocalQuota(f.db, model.EdgeLocalReservationRequest{
			ReservationID: f.reservationID, RequestID: f.requestID, LeaseID: lease.LeaseID,
			Quota: quota, NowUnixMilli: now,
		})
		if err == nil {
			f.relayInfo.EdgeLeaseSnapshotID = lease.SnapshotID
			f.relayInfo.EdgeLeaseSnapshotRevision = lease.SnapshotRevision
			f.relayInfo.EdgeLeasePricingRevision = lease.PricingRevision
			return lease.LeaseID, nil
		}
		if errors.Is(err, model.ErrEdgeLocalReservationConflict) || errors.Is(err, model.ErrEdgeLocalReservationFinalized) {
			return "", err
		}
		lastErr = err
	}
	return "", lastErr
}

func (f *EdgeLeaseFunding) acquireLease(minimumQuota int64, force bool) error {
	client, ok := ActiveEdgeControlClient()
	if !ok {
		return edgeFundingAPIError(errors.New("master control connection is unavailable and no local lease can fund the request"), http.StatusServiceUnavailable)
	}
	key := fmt.Sprintf("%d:%d", f.relayInfo.UserId, f.relayInfo.TokenId)
	result, err, _ := edgeLeaseAcquireGroup.Do(key, func() (any, error) {
		// Another waiter may have installed enough quota while this call waited.
		if !force {
			if _, reserveErr := f.findFundableLease(minimumQuota); reserveErr == nil {
				return nil, nil
			}
		}
		snapshot, err := model.GetEdgeLocalSnapshotState(f.db)
		if err != nil {
			return nil, err
		}
		expiresAt, err := model.GetEdgeLocalSnapshotExpiry(f.db)
		if err != nil {
			return nil, err
		}
		if snapshot.SnapshotID == "" || snapshot.Revision <= 0 || expiresAt <= time.Now().UnixMilli() {
			return nil, errors.New("edge snapshot is unavailable or expired")
		}
		allowZeroGrant := !force && minimumQuota == 0 && f.freePricingPolicy()
		requestedQuota, minimumAcceptable, err := edgeLeaseQuotaRequest(minimumQuota, allowZeroGrant)
		if err != nil {
			return nil, err
		}
		meta, err := client.NewRequestMeta("lease-acquire")
		if err != nil {
			return nil, err
		}
		request := dto.EdgeLeaseAcquireRequestV1{
			Meta: meta, Subject: dto.EdgeLeaseSubjectV1{UserID: int64(f.relayInfo.UserId), TokenID: int64(f.relayInfo.TokenId)},
			RequestedQuota: requestedQuota, MinimumAcceptableQuota: minimumAcceptable,
			SnapshotID: snapshot.SnapshotID, SnapshotRevision: snapshot.Revision,
		}
		if latest, latestErr := f.latestActiveLease(); latestErr == nil {
			request.ExistingLeaseID = latest.LeaseID
		}
		durable, err := model.GetOrCreateEdgeLocalLeaseAcquireIntent(f.db, request, time.Now().UnixMilli())
		if err != nil {
			return nil, err
		}
		response, err := client.AcquireLease(f.requestContext, *durable)
		if err != nil {
			var remote *EdgeControlRemoteError
			if errors.As(err, &remote) {
				if discardErr := model.DiscardEdgeLocalLeaseAcquireIntent(f.db, durable.Meta.RequestID); discardErr != nil {
					return nil, errors.Join(err, discardErr)
				}
			}
			return nil, err
		}
		if err := model.InstallEdgeLocalLeaseFromAcquireIntent(f.db, durable.Meta.RequestID, response.Lease); err != nil {
			return nil, err
		}
		return response.Lease.LeaseID, nil
	})
	_ = result
	if err != nil {
		return edgeFundingAPIError(err, edgeFundingStatus(err))
	}
	return nil
}

func (f *EdgeLeaseFunding) findFundableLease(quota int64) (*model.EdgeLocalQuotaLease, error) {
	var lease model.EdgeLocalQuotaLease
	err := f.db.Where("user_id = ? AND token_id = ? AND status = ? AND expires_at_unix_milli > ? AND remaining_quota >= ?",
		f.relayInfo.UserId, f.relayInfo.TokenId, dto.EdgeLeaseStatusActiveV1, time.Now().UnixMilli(), quota).
		Order("issued_at_unix_milli desc, lease_id asc").First(&lease).Error
	if err != nil {
		return nil, err
	}
	return &lease, nil
}

func (f *EdgeLeaseFunding) latestActiveLease() (*model.EdgeLocalQuotaLease, error) {
	return model.FindEdgeLocalActiveLease(f.db, int64(f.relayInfo.UserId), int64(f.relayInfo.TokenId), time.Now().UnixMilli())
}

func (f *EdgeLeaseFunding) buildUsageEvent(actualQuota int64) (*dto.EdgeUsageEventV1, error) {
	if f.relayInfo.SettlementUsage == nil {
		return nil, errors.New("edge settlement usage is unavailable")
	}
	if len(f.relayInfo.PriceData.OtherRatios()) != 0 {
		return nil, errors.New("edge settlement v1 does not support request-specific billing multipliers")
	}
	if f.pricing.BillingMode == dto.EdgeBillingModeTieredExprV1 {
		return nil, errors.New("tiered billing is not supported by edge settlement v1")
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
	event := &dto.EdgeUsageEventV1{
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
	}
	return event, nil
}

func (f *EdgeLeaseFunding) freePricingPolicy() bool {
	if f == nil || f.relayInfo == nil || !f.relayInfo.PriceData.FreeModel {
		return false
	}
	if f.relayInfo.PriceData.GroupRatioInfo.GroupRatio == 0 {
		return true
	}
	switch f.pricing.BillingMode {
	case dto.EdgeBillingModeFixedPriceV1:
		return f.pricing.ModelPrice != nil && *f.pricing.ModelPrice == 0
	case dto.EdgeBillingModeRatioV1:
		return f.pricing.ModelRatio != nil && *f.pricing.ModelRatio == 0
	default:
		return false
	}
}

func edgeLeaseQuotaRequest(minimum int64, allowZeroGrant bool) (int64, int64, error) {
	if allowZeroGrant {
		if minimum != 0 {
			return 0, 0, errors.New("zero edge lease cannot satisfy a positive minimum")
		}
		return 0, 0, nil
	}
	requested := int64(common.GetEnvOrDefault("EDGE_LEASE_REQUEST_QUOTA", defaultEdgeLeaseRequestQuota))
	configuredMinimum := int64(common.GetEnvOrDefault("EDGE_LEASE_MINIMUM_QUOTA", defaultEdgeLeaseMinimumQuota))
	if requested <= 0 || requested > int64(common.MaxQuota) || configuredMinimum < 0 || configuredMinimum > int64(common.MaxQuota) {
		return 0, 0, errors.New("edge lease quota configuration is invalid")
	}
	if minimum < 0 || minimum > int64(common.MaxQuota) {
		return 0, 0, errors.New("edge lease minimum exceeds the supported range")
	}
	if requested < minimum {
		requested = minimum
	}
	minimumAcceptable := configuredMinimum
	if minimumAcceptable < minimum {
		minimumAcceptable = minimum
	}
	if minimumAcceptable > requested {
		minimumAcceptable = requested
	}
	return requested, minimumAcceptable, nil
}

func edgeLeaseNeedsAcquisition(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound) ||
		errors.Is(err, model.ErrEdgeLocalLeaseUnavailable) ||
		errors.Is(err, model.ErrEdgeLocalLeaseExpired) ||
		errors.Is(err, model.ErrEdgeLocalSnapshotMismatch) ||
		errors.Is(err, model.ErrEdgeLocalQuotaInsufficient)
}

func edgeFundingStatus(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	var remote *EdgeControlRemoteError
	if errors.As(err, &remote) {
		if remote.StatusCode == http.StatusForbidden || remote.StatusCode == http.StatusConflict {
			return http.StatusForbidden
		}
		if remote.StatusCode >= 500 || remote.Retryable() {
			return http.StatusServiceUnavailable
		}
		return remote.StatusCode
	}
	if errors.Is(err, model.ErrEdgeLocalQuotaInsufficient) {
		return http.StatusForbidden
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
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
