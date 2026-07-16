package edge

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgesettlement"

	"gorm.io/gorm"
)

const (
	defaultMasterLeaseTTL          = 15 * time.Minute
	defaultMasterLeaseRenewDivisor = int64(4)
	maxMasterLeaseTTL              = 24 * time.Hour
)

var (
	ErrMasterLeaseUnavailable          = errors.New("edge quota lease is unavailable")
	ErrMasterLeaseConflict             = errors.New("edge quota lease conflicts with authoritative state")
	ErrMasterSettlementOutOfOrder      = errors.New("edge settlement block is out of order")
	ErrMasterSettlementConflict        = errors.New("edge settlement block conflicts with authoritative state")
	ErrMasterDynamicPricingUnsupported = errors.New("dynamic billing expressions are unsupported by edge control v1 settlement")
	ErrMasterLeaseQuotaExceeded        = errors.New("edge usage exceeds the remaining lease quota")
)

type MasterLeasePolicy struct {
	TTL           time.Duration
	MaxLeaseQuota int64
	RenewDivisor  int64
}

type MasterLeaseAcquireCommand struct {
	Request        dto.EdgeLeaseAcquireRequestV1
	IdempotencyKey string
	RequestHash    string
	Now            time.Time
	Policy         MasterLeasePolicy
}

type MasterSettlementCommand struct {
	Request        dto.EdgeSettlementBlockRequestV1
	IdempotencyKey string
	RequestHash    string
	Now            time.Time
}

type MasterLeaseCloseCommand struct {
	Request dto.EdgeLeaseCloseRequestV1
	Now     time.Time
}

// SettlementSequenceError exposes the authoritative cursor so the control
// handler can return expected.next_settlement_sequence without parsing text.
type SettlementSequenceError struct {
	Expected int64
	Received int64
}

func (e *SettlementSequenceError) Error() string {
	if e == nil {
		return ErrMasterSettlementOutOfOrder.Error()
	}
	return fmt.Sprintf("%s: expected=%d received=%d", ErrMasterSettlementOutOfOrder, e.Expected, e.Received)
}

func (e *SettlementSequenceError) Unwrap() error {
	return ErrMasterSettlementOutOfOrder
}

type masterSnapshotPolicies struct {
	authentication map[int64]dto.EdgeTokenAuthRecordV1
	users          map[int64]dto.EdgeUserPolicyV1
	groups         map[string]dto.EdgeGroupPolicyV1
	models         map[string]dto.EdgeModelPolicyV1
	channels       map[int64]dto.EdgeChannelProjectionV1
	pricing        map[string]dto.EdgePricingPolicyV1
}

type selectedMasterLeaseFunding struct {
	source         model.EdgeLeaseFundingSource
	quota          int64
	subscriptionID int
}

type edgeConsumeLogOutboxPayload struct {
	EventID             string                 `json:"event_id"`
	NodeID              string                 `json:"node_id"`
	NodeGeneration      int64                  `json:"node_generation"`
	LeaseID             string                 `json:"lease_id"`
	RequestID           string                 `json:"request_id"`
	UserID              int                    `json:"user_id"`
	TokenID             int                    `json:"token_id"`
	ChannelID           int                    `json:"channel_id"`
	Endpoint            dto.EdgeEndpointV1     `json:"endpoint"`
	Streaming           bool                   `json:"streaming"`
	Model               string                 `json:"model"`
	Group               string                 `json:"group"`
	Outcome             dto.EdgeUsageOutcomeV1 `json:"outcome"`
	HTTPStatus          int                    `json:"http_status,omitempty"`
	ErrorCode           string                 `json:"error_code,omitempty"`
	PromptTokens        int                    `json:"prompt_tokens"`
	CompletionTokens    int                    `json:"completion_tokens"`
	Quota               int64                  `json:"quota"`
	StartedAtUnixMilli  int64                  `json:"started_at_unix_milli"`
	FinishedAtUnixMilli int64                  `json:"finished_at_unix_milli"`
}

// AcquireMasterQuotaLeaseTx must run inside ExecuteControlMutation's
// transaction. That outer layer locks the authenticated identity and durable
// request receipt; this function revalidates the identity, serializes node
// risk with the node row lock and performs all authoritative reservations in
// the same database transaction.
func AcquireMasterQuotaLeaseTx(tx *gorm.DB, identity *model.EdgeControlIdentity, command MasterLeaseAcquireCommand) (*model.EdgeQuotaLease, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if err := command.Request.Validate(); err != nil {
		return nil, err
	}
	if err := edgeauth.ValidateIdempotencyKey(strings.TrimSpace(command.IdempotencyKey)); err != nil {
		return nil, err
	}
	if err := validateMasterRequestHash(command.RequestHash); err != nil {
		return nil, err
	}
	now := command.Now
	if now.IsZero() {
		now = time.Now()
	}
	zeroGrant := command.Request.RequestedQuota == 0
	lockedIdentity, err := lockMasterControlIdentityTx(tx, identity, now)
	if err != nil {
		return nil, err
	}
	node := lockedIdentity.Node
	if !node.CanIssueLease() {
		return nil, ErrMasterLeaseUnavailable
	}

	var replay model.EdgeQuotaLease
	replayQuery := tx.Where("node_id = ? AND node_generation = ? AND request_idempotency_key = ?",
		node.ID, node.Generation, command.IdempotencyKey).Limit(1).Find(&replay)
	if replayQuery.Error != nil {
		return nil, replayQuery.Error
	}
	if replayQuery.RowsAffected == 1 {
		if replay.RequestHash != command.RequestHash || replay.UserID != int(command.Request.Subject.UserID) ||
			replay.TokenID != int(command.Request.Subject.TokenID) || replay.SnapshotUID != command.Request.SnapshotID ||
			replay.SnapshotRevision != command.Request.SnapshotRevision {
			return nil, ErrMasterLeaseConflict
		}
		return &replay, nil
	}

	if command.Request.ExistingLeaseID != "" {
		existing, err := model.LockEdgeQuotaLeaseByUIDTx(tx, node.ID, node.Generation, command.Request.ExistingLeaseID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrMasterLeaseConflict
			}
			return nil, err
		}
		if existing.UserID != int(command.Request.Subject.UserID) || existing.TokenID != int(command.Request.Subject.TokenID) ||
			existing.Status != model.EdgeQuotaLeaseStatusActive {
			return nil, ErrMasterLeaseConflict
		}
	}

	snapshot, err := model.GetPublishedEdgeCompiledSnapshotForLeaseTx(
		tx, command.Request.SnapshotID, command.Request.SnapshotRevision, now.Unix(),
	)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: snapshot is not currently published", ErrMasterLeaseUnavailable)
		}
		return nil, err
	}
	policies, err := loadMasterSnapshotPoliciesTx(tx, snapshot.ID, true)
	if err != nil {
		return nil, err
	}

	user, token, err := model.LockEdgeLeaseSubjectTx(tx, int(command.Request.Subject.UserID), int(command.Request.Subject.TokenID))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrMasterLeaseUnavailable
		}
		return nil, err
	}
	if err := validateMasterLeaseSubject(user, token, policies, now, zeroGrant); err != nil {
		return nil, err
	}

	policy, err := normalizeMasterLeasePolicy(command.Policy)
	if err != nil {
		return nil, err
	}
	desiredQuota := command.Request.RequestedQuota
	if desiredQuota > policy.MaxLeaseQuota {
		desiredQuota = policy.MaxLeaseQuota
	}
	minimumQuota := command.Request.MinimumAcceptableQuota
	if minimumQuota == 0 && !zeroGrant {
		minimumQuota = 1
	}

	funding := &selectedMasterLeaseFunding{source: model.EdgeLeaseFundingSourceWallet}
	if !zeroGrant {
		outstanding, err := edgeNodeOutstandingQuotaTx(tx, node.ID, node.Generation)
		if err != nil {
			return nil, err
		}
		nodeAvailable := node.MaxOutstandingQuota - outstanding
		if nodeAvailable < desiredQuota {
			desiredQuota = nodeAvailable
		}
		if !token.UnlimitedQuota && int64(token.RemainQuota) < desiredQuota {
			desiredQuota = int64(token.RemainQuota)
		}
		if desiredQuota < minimumQuota || desiredQuota <= 0 {
			return nil, ErrMasterLeaseUnavailable
		}

		funding, err = selectMasterLeaseFundingTx(tx, user, desiredQuota, minimumQuota, now.Unix())
		if err != nil {
			return nil, err
		}
		if funding.quota < minimumQuota || funding.quota <= 0 {
			return nil, ErrMasterLeaseUnavailable
		}

		switch funding.source {
		case model.EdgeLeaseFundingSourceWallet:
			if err := model.ReserveEdgeLeaseWalletTx(tx, user.Id, funding.quota); err != nil {
				if errors.Is(err, model.ErrEdgeLeaseWalletQuotaInsufficient) {
					return nil, fmt.Errorf("%w: %v", ErrMasterLeaseUnavailable, err)
				}
				return nil, err
			}
		case model.EdgeLeaseFundingSourceSubscription:
			if err := model.ReserveEdgeLeaseSubscriptionTx(tx, funding.subscriptionID, now.Unix(), funding.quota); err != nil {
				if errors.Is(err, model.ErrEdgeLeaseSubscriptionUnavailable) {
					return nil, fmt.Errorf("%w: %v", ErrMasterLeaseUnavailable, err)
				}
				return nil, err
			}
		default:
			return nil, model.ErrInvalidEdgeLeaseFundingSource
		}
		if err := model.ReserveEdgeLeaseTokenTx(tx, token.Id, funding.quota, token.UnlimitedQuota); err != nil {
			if errors.Is(err, model.ErrEdgeLeaseTokenQuotaInsufficient) {
				return nil, fmt.Errorf("%w: %v", ErrMasterLeaseUnavailable, err)
			}
			return nil, err
		}
	}

	leaseUID, err := newMasterLeaseUID()
	if err != nil {
		return nil, err
	}
	issuedAt := now.UnixMilli()
	expiresAt := now.Add(policy.TTL).UnixMilli()
	snapshotExpiry := time.Unix(snapshot.ExpiresAt, 0).UnixMilli()
	if expiresAt > snapshotExpiry {
		expiresAt = snapshotExpiry
	}
	if expiresAt <= issuedAt {
		return nil, ErrMasterLeaseUnavailable
	}
	renewAfter := int64(0)
	if funding.quota > 0 {
		renewAfter = funding.quota / policy.RenewDivisor
		if renewAfter >= funding.quota {
			renewAfter = funding.quota - 1
		}
	}
	lease := &model.EdgeQuotaLease{
		LeaseUID:                 leaseUID,
		NodeID:                   node.ID,
		NodeGeneration:           node.Generation,
		UserID:                   user.Id,
		TokenID:                  token.Id,
		SnapshotID:               snapshot.ID,
		SnapshotUID:              snapshot.SnapshotUID,
		SnapshotRevision:         snapshot.Revision,
		PricingRevision:          snapshot.Revision,
		RequestIdempotencyKey:    command.IdempotencyKey,
		RequestHash:              command.RequestHash,
		Status:                   model.EdgeQuotaLeaseStatusActive,
		FundingSource:            funding.source,
		TokenUnlimited:           token.UnlimitedQuota,
		GrantedQuota:             funding.quota,
		RenewAfterRemainingQuota: renewAfter,
		IssuedAtUnixMilli:        issuedAt,
		ExpiresAtUnixMilli:       expiresAt,
	}
	if err := tx.Create(lease).Error; err != nil {
		return nil, err
	}
	leaseFunding := &model.EdgeLeaseFunding{
		LeaseID:            lease.ID,
		Source:             funding.source,
		UserID:             user.Id,
		UserSubscriptionID: funding.subscriptionID,
		ReservedQuota:      funding.quota,
		Status:             model.EdgeLeaseFundingStatusReserved,
	}
	if err := tx.Create(leaseFunding).Error; err != nil {
		return nil, err
	}
	return lease, nil
}

// SettleMasterUsageBlockTx accepts one contiguous block exactly once. It
// recomputes every charge from the lease's immutable snapshot, updates only
// consumption statistics (never wallet/token remain quota a second time), and
// writes the consume-log outbox in the same transaction.
func SettleMasterUsageBlockTx(tx *gorm.DB, identity *model.EdgeControlIdentity, command MasterSettlementCommand) (*dto.EdgeSettlementAckV1, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if err := command.Request.Validate(); err != nil {
		return nil, err
	}
	if err := edgeauth.ValidateIdempotencyKey(strings.TrimSpace(command.IdempotencyKey)); err != nil {
		return nil, err
	}
	if err := validateMasterRequestHash(command.RequestHash); err != nil {
		return nil, err
	}
	now := command.Now
	if now.IsZero() {
		now = time.Now()
	}
	lockedIdentity, err := lockMasterControlIdentityTx(tx, identity, now)
	if err != nil {
		return nil, err
	}
	node := lockedIdentity.Node
	if !node.CanAcceptSettlement() {
		return nil, ErrMasterSettlementConflict
	}
	if err := validateMasterSettlementTimes(command.Request, now); err != nil {
		return nil, err
	}
	expectedBlockDigest, err := edgesettlement.DigestBlockV1(node.NodeUID, node.Generation, command.Request)
	if err != nil {
		return nil, err
	}
	if command.Request.BlockDigest != expectedBlockDigest {
		return nil, fmt.Errorf("%w: settlement block digest does not match canonical content", ErrMasterSettlementConflict)
	}

	duplicate, err := findMasterSettlementReplayTx(tx, node, command)
	if err != nil {
		return nil, err
	}
	if duplicate != nil {
		return duplicate, nil
	}

	expectedSequence := node.LastEventSeq + 1
	if command.Request.FirstSequence != expectedSequence {
		return nil, &SettlementSequenceError{Expected: expectedSequence, Received: command.Request.FirstSequence}
	}
	if err := validateMasterSettlementChainTx(tx, node, &command.Request); err != nil {
		return nil, err
	}

	block := &model.EdgeSettlementBlock{
		NodeID:                  node.ID,
		NodeGeneration:          node.Generation,
		BlockUID:                command.Request.BlockID,
		IdempotencyKey:          command.IdempotencyKey,
		RequestHash:             command.RequestHash,
		BlockOrdinal:            node.LastBlockSeq + 1,
		PreviousBlockUID:        command.Request.PreviousBlockID,
		PreviousBlockDigest:     command.Request.PreviousBlockDigest,
		FirstSequence:           command.Request.FirstSequence,
		LastSequence:            command.Request.LastSequence,
		EventCount:              len(command.Request.Events),
		BlockDigest:             command.Request.BlockDigest,
		Status:                  model.EdgeSettlementBlockStatusAccepted,
		EdgeCreatedAtUnixMilli:  command.Request.CreatedAtUnixMilli,
		AcknowledgedAtUnixMilli: now.UnixMilli(),
	}
	if err := tx.Create(block).Error; err != nil {
		return nil, err
	}

	leases := make(map[string]*model.EdgeQuotaLease)
	fundings := make(map[int64]*model.EdgeLeaseFunding)
	policyCache := make(map[int64]*masterSnapshotPolicies)
	for i := range command.Request.Events {
		event := &command.Request.Events[i]
		lease := leases[event.LeaseID]
		if lease == nil {
			lease, err = model.LockEdgeQuotaLeaseByUIDTx(tx, node.ID, node.Generation, event.LeaseID)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil, ErrMasterSettlementConflict
				}
				return nil, err
			}
			leases[event.LeaseID] = lease
		}
		if err := validateMasterSettlementLeaseEvent(lease, event); err != nil {
			return nil, err
		}

		policies := policyCache[lease.SnapshotID]
		if policies == nil {
			if _, err := model.GetEdgeCompiledSnapshotForSettlementTx(tx, lease.SnapshotID, lease.SnapshotUID, lease.SnapshotRevision); err != nil {
				return nil, err
			}
			policies, err = loadMasterSnapshotPoliciesTx(tx, lease.SnapshotID, true)
			if err != nil {
				return nil, err
			}
			policyCache[lease.SnapshotID] = policies
		}
		chargedQuota, normalizedUsage, err := recomputeMasterUsageQuota(policies, lease.UserID, event)
		if err != nil {
			return nil, err
		}
		if chargedQuota != event.Billing.ChargedQuota {
			return nil, fmt.Errorf("%w: event %s reported charge=%d master=%d", ErrMasterSettlementConflict, event.EventID, event.Billing.ChargedQuota, chargedQuota)
		}
		if chargedQuota > lease.RemainingQuota() {
			return nil, ErrMasterLeaseQuotaExceeded
		}
		if err := rejectDuplicateMasterUsageEventTx(tx, node, event); err != nil {
			return nil, err
		}

		funding := fundings[lease.ID]
		if funding == nil {
			funding, err = model.LockEdgeLeaseFundingTx(tx, lease.ID)
			if err != nil {
				return nil, err
			}
			fundings[lease.ID] = funding
		}
		lease.ConsumedQuota += chargedQuota
		lease.UpdatedAt = now.Unix()
		funding.ConsumedQuota += chargedQuota
		funding.UpdatedAt = now.Unix()
		if err := tx.Save(lease).Error; err != nil {
			return nil, err
		}
		if err := tx.Save(funding).Error; err != nil {
			return nil, err
		}

		promptTokens, completionTokens := masterUsageTokenTotals(normalizedUsage)
		usagePayload, err := common.Marshal(event.Usage)
		if err != nil {
			return nil, err
		}
		billingPayload, err := common.Marshal(event.Billing)
		if err != nil {
			return nil, err
		}
		httpStatus := 0
		if event.HTTPStatus != nil {
			httpStatus = *event.HTTPStatus
		}
		storedEvent := &model.EdgeUsageEvent{
			NodeID:               node.ID,
			NodeGeneration:       node.Generation,
			BlockID:              block.ID,
			LeaseID:              lease.ID,
			EventUID:             event.EventID,
			ReservationUID:       event.ReservationID,
			RequestUID:           event.RequestID,
			Sequence:             event.Sequence,
			UserID:               int(event.UserID),
			TokenID:              int(event.TokenID),
			ChannelID:            int(event.ChannelID),
			Endpoint:             string(event.Endpoint),
			Streaming:            event.Streaming,
			Model:                event.Model,
			Group:                event.Group,
			Outcome:              string(event.Outcome),
			HTTPStatus:           httpStatus,
			ErrorCode:            event.ErrorCode,
			StartedAtUnixMilli:   event.StartedAtUnixMilli,
			FinishedAtUnixMilli:  event.FinishedAtUnixMilli,
			PromptTokens:         promptTokens,
			CompletionTokens:     completionTokens,
			ReservedQuota:        event.Billing.ReservedQuota,
			ChargedQuota:         chargedQuota,
			PricingPolicyID:      event.Billing.PricingPolicyID,
			PricingPolicyVersion: event.Billing.PricingPolicyVersion,
			UsagePayload:         string(usagePayload),
			BillingPayload:       string(billingPayload),
		}
		if err := tx.Create(storedEvent).Error; err != nil {
			return nil, err
		}
		if err := model.AddEdgeLeaseSettlementStatsTx(tx, lease.UserID, lease.TokenID, int(event.ChannelID), chargedQuota, event.FinishedAtUnixMilli/1000); err != nil {
			return nil, err
		}
		outboxPayload, err := common.Marshal(edgeConsumeLogOutboxPayload{
			EventID: event.EventID, NodeID: node.NodeUID, NodeGeneration: node.Generation,
			LeaseID: lease.LeaseUID, RequestID: event.RequestID, UserID: lease.UserID, TokenID: lease.TokenID,
			ChannelID: int(event.ChannelID), Endpoint: event.Endpoint, Streaming: event.Streaming,
			Model: event.Model, Group: event.Group, Outcome: event.Outcome, HTTPStatus: httpStatus,
			ErrorCode: event.ErrorCode, PromptTokens: promptTokens, CompletionTokens: completionTokens,
			Quota: chargedQuota, StartedAtUnixMilli: event.StartedAtUnixMilli, FinishedAtUnixMilli: event.FinishedAtUnixMilli,
		})
		if err != nil {
			return nil, err
		}
		billingEventKey, err := model.EdgeConsumeLogBillingEventKey(node.NodeUID, node.Generation, event.EventID)
		if err != nil {
			return nil, err
		}
		if err := tx.Create(&model.EdgeConsumeLogOutbox{
			EventID: storedEvent.ID, EventUID: billingEventKey, Payload: string(outboxPayload),
			Status: model.EdgeConsumeLogOutboxStatusPending, AvailableAt: now.Unix(),
		}).Error; err != nil {
			return nil, err
		}
	}

	node.LastEventSeq = command.Request.LastSequence
	node.LastBlockSeq = block.BlockOrdinal
	node.UpdatedAt = now.Unix()
	if err := tx.Save(node).Error; err != nil {
		return nil, err
	}
	if err := finalizeMasterClosableLeasesTx(tx, node, now); err != nil {
		return nil, err
	}
	return &dto.EdgeSettlementAckV1{
		Status:                  dto.EdgeSettlementAckAcceptedV1,
		NodeID:                  node.NodeUID,
		NodeGeneration:          node.Generation,
		BlockID:                 block.BlockUID,
		AckedThroughSequence:    block.LastSequence,
		NextExpectedSequence:    block.LastSequence + 1,
		AcceptedEventCount:      block.EventCount,
		AcknowledgedAtUnixMilli: block.AcknowledgedAtUnixMilli,
	}, nil
}

func validateMasterSettlementTimes(request dto.EdgeSettlementBlockRequestV1, now time.Time) error {
	skewSeconds := common.GetEnvOrDefault("EDGE_CONTROL_CLOCK_SKEW_TOLERANCE_SECONDS", 120)
	if skewSeconds < 1 || skewSeconds > int(dto.EdgeControlMaxClockSkewToleranceSecondsV1) {
		return errors.New("edge control clock skew configuration is invalid")
	}
	maxDurationSeconds := common.GetEnvOrDefault("EDGE_MAX_INFLIGHT_REQUEST_SECONDS", 3600)
	if maxDurationSeconds < 1 || maxDurationSeconds > 86400 {
		return errors.New("edge maximum in-flight request duration configuration is invalid")
	}
	latestAllowed := now.Add(time.Duration(skewSeconds) * time.Second).UnixMilli()
	if request.CreatedAtUnixMilli > latestAllowed {
		return fmt.Errorf("%w: settlement block was created in the future", ErrMasterSettlementConflict)
	}
	maxDurationMillis := int64(maxDurationSeconds) * int64(time.Second/time.Millisecond)
	for i := range request.Events {
		event := request.Events[i]
		if event.StartedAtUnixMilli > latestAllowed || event.FinishedAtUnixMilli > latestAllowed ||
			event.FinishedAtUnixMilli > request.CreatedAtUnixMilli ||
			event.FinishedAtUnixMilli-event.StartedAtUnixMilli > maxDurationMillis {
			return fmt.Errorf("%w: event %s has an invalid settlement timeline", ErrMasterSettlementConflict, event.EventID)
		}
	}
	return nil
}

func CloseMasterQuotaLeaseTx(tx *gorm.DB, identity *model.EdgeControlIdentity, command MasterLeaseCloseCommand) (*dto.EdgeLeaseCloseResponseV1, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if err := command.Request.Validate(); err != nil {
		return nil, err
	}
	now := command.Now
	if now.IsZero() {
		now = time.Now()
	}
	lockedIdentity, err := lockMasterControlIdentityTx(tx, identity, now)
	if err != nil {
		return nil, err
	}
	node := lockedIdentity.Node
	if !node.CanAcceptSettlement() {
		return nil, ErrMasterLeaseConflict
	}
	lease, err := model.LockEdgeQuotaLeaseByUIDTx(tx, node.ID, node.Generation, command.Request.LeaseID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrMasterLeaseConflict
		}
		return nil, err
	}
	if lease.Version != command.Request.LeaseVersion {
		return nil, ErrMasterLeaseConflict
	}
	if lease.Status.Terminal() {
		return masterLeaseCloseResponse(lease), nil
	}
	if lease.Status == model.EdgeQuotaLeaseStatusActive {
		lease.Status = model.EdgeQuotaLeaseStatusClosing
		lease.Version++
	}
	if lease.Status != model.EdgeQuotaLeaseStatusClosing && lease.Status != model.EdgeQuotaLeaseStatusRevoked {
		return nil, ErrMasterLeaseConflict
	}
	if lease.CloseAfterEventSequence != 0 && lease.CloseAfterEventSequence != command.Request.FinalEventSequence {
		return nil, ErrMasterLeaseConflict
	}
	lease.CloseAfterEventSequence = command.Request.FinalEventSequence
	lease.UpdatedAt = now.Unix()
	if err := tx.Save(lease).Error; err != nil {
		return nil, err
	}
	if node.LastEventSeq >= lease.CloseAfterEventSequence {
		if err := finalizeMasterLeaseTx(tx, lease, now, false); err != nil {
			return nil, err
		}
	}
	return masterLeaseCloseResponse(lease), nil
}

// RevokeMasterQuotaLeaseTx preserves already reserved quota until the edge's
// declared final durable watermark is accepted. It therefore follows
// active -> revoked -> closed and shares the normal confirmed-unused refund.
func RevokeMasterQuotaLeaseTx(tx *gorm.DB, nodeID int64, generation int64, leaseUID string, finalEventSequence int64, now time.Time) (*model.EdgeQuotaLease, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if finalEventSequence < 0 {
		return nil, ErrMasterLeaseConflict
	}
	node, err := model.LockEdgeNodeByIDTx(tx, nodeID)
	if err != nil {
		return nil, err
	}
	if node.Generation != generation {
		return nil, ErrMasterLeaseConflict
	}
	lease, err := model.LockEdgeQuotaLeaseByUIDTx(tx, nodeID, generation, leaseUID)
	if err != nil {
		return nil, err
	}
	if lease.Status.Terminal() {
		return lease, nil
	}
	if lease.Status != model.EdgeQuotaLeaseStatusActive && lease.Status != model.EdgeQuotaLeaseStatusRevoked {
		return nil, ErrMasterLeaseConflict
	}
	if lease.Status == model.EdgeQuotaLeaseStatusActive {
		lease.Status = model.EdgeQuotaLeaseStatusRevoked
		lease.Version++
	}
	if lease.CloseAfterEventSequence != 0 && lease.CloseAfterEventSequence != finalEventSequence {
		return nil, ErrMasterLeaseConflict
	}
	lease.CloseAfterEventSequence = finalEventSequence
	lease.UpdatedAt = now.Unix()
	if err := tx.Save(lease).Error; err != nil {
		return nil, err
	}
	if node.LastEventSeq >= finalEventSequence {
		if err := finalizeMasterLeaseTx(tx, lease, now, false); err != nil {
			return nil, err
		}
	}
	return lease, nil
}

// ForceCloseMasterQuotaLeaseTx is an administrative loss-finalization path.
// The unreported remainder is deliberately forfeited and never refunded.
func ForceCloseMasterQuotaLeaseTx(tx *gorm.DB, nodeID int64, generation int64, leaseUID string, now time.Time) (*model.EdgeQuotaLease, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if _, err := model.LockEdgeNodeByIDTx(tx, nodeID); err != nil {
		return nil, err
	}
	lease, err := model.LockEdgeQuotaLeaseByUIDTx(tx, nodeID, generation, leaseUID)
	if err != nil {
		return nil, err
	}
	if lease.Status.Terminal() {
		return lease, nil
	}
	if err := finalizeMasterLeaseTx(tx, lease, now, true); err != nil {
		return nil, err
	}
	return lease, nil
}

// InvalidateMasterLeaseSubjectCaches is intentionally separate from the
// transaction functions. Call it only after the surrounding transaction has
// committed, so Redis cannot advertise a reservation that later rolled back.
func InvalidateMasterLeaseSubjectCaches(userID int) {
	if userID <= 0 {
		return
	}
	_ = model.InvalidateUserCache(userID)
	_ = model.InvalidateUserTokensCache(userID)
}
