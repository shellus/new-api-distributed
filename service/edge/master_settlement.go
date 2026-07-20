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
	coreservice "github.com/QuantumNous/new-api/service"

	"gorm.io/gorm"
)

var (
	ErrMasterSettlementOutOfOrder      = errors.New("edge settlement block is out of order")
	ErrMasterSettlementConflict        = errors.New("edge settlement block conflicts with authoritative state")
	ErrMasterDynamicPricingUnsupported = errors.New("dynamic billing expressions are unsupported by edge settlement")
)

type MasterSettlementCommand struct {
	Request        dto.EdgeSettlementBlockRequestV1
	IdempotencyKey string
	RequestHash    string
	Now            time.Time
}

// SettlementSequenceError exposes the authoritative cursor so the control
// handler can return expected.next_settlement_sequence without parsing text.
type SettlementSequenceError struct {
	Expected int64
	Received int64
}

type SettlementCircuitError struct {
	Epoch  int64
	Reason string
}

func (e *SettlementCircuitError) Error() string {
	if e == nil {
		return "edge settlement circuit is open"
	}
	return fmt.Sprintf("edge settlement circuit is open: epoch=%d reason=%s", e.Epoch, e.Reason)
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
	authentication  map[int64]dto.EdgeTokenAuthRecordV1
	users           map[int64]dto.EdgeUserPolicyV1
	groups          map[string]dto.EdgeGroupPolicyV1
	models          map[string]dto.EdgeModelPolicyV1
	channels        map[int64]dto.EdgeChannelProjectionV1
	pricing         map[string]dto.EdgePricingPolicyV1
	pricingRevision int64
}

type edgeConsumeLogOutboxPayload struct {
	EventID                  string                 `json:"event_id"`
	NodeID                   string                 `json:"node_id"`
	NodeGeneration           int64                  `json:"node_generation"`
	RequestID                string                 `json:"request_id"`
	UserID                   int                    `json:"user_id"`
	TokenID                  int                    `json:"token_id"`
	ChannelID                int                    `json:"channel_id"`
	Endpoint                 dto.EdgeEndpointV1     `json:"endpoint"`
	Streaming                bool                   `json:"streaming"`
	Model                    string                 `json:"model"`
	Group                    string                 `json:"group"`
	Outcome                  dto.EdgeUsageOutcomeV1 `json:"outcome"`
	HTTPStatus               int                    `json:"http_status,omitempty"`
	ErrorCode                string                 `json:"error_code,omitempty"`
	PromptTokens             int                    `json:"prompt_tokens"`
	CompletionTokens         int                    `json:"completion_tokens"`
	Quota                    int64                  `json:"quota"`
	StartedAtUnixMilli       int64                  `json:"started_at_unix_milli"`
	FirstResponseAtUnixMilli *int64                 `json:"first_response_at_unix_milli,omitempty"`
	FinishedAtUnixMilli      int64                  `json:"finished_at_unix_milli"`
}

type masterSettlementCharge struct {
	event             *dto.EdgeUsageEventV1
	snapshotID        int64
	chargedQuota      int64
	normalizedUsage   *dto.Usage
	billingPreference string
}

// SettleMasterUsageBlockTx accepts one contiguous block exactly once. It
// recomputes every charge from the immutable snapshot, checks the node event-time
// window, charges authoritative funding/token balances, and writes usage plus
// the consume-log outbox in the same transaction.
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
	if node.SettlementCircuitOpen {
		return nil, &SettlementCircuitError{Epoch: node.SettlementCircuitEpoch, Reason: node.SettlementCircuitReason}
	}

	expectedSequence := node.LastEventSeq + 1
	if command.Request.FirstSequence != expectedSequence {
		return nil, &SettlementSequenceError{Expected: expectedSequence, Received: command.Request.FirstSequence}
	}
	if err := validateMasterSettlementChainTx(tx, node, &command.Request); err != nil {
		return nil, err
	}

	charges := make([]masterSettlementCharge, 0, len(command.Request.Events))
	policyCache := make(map[string]*masterSnapshotPolicies)
	snapshotIDs := make(map[string]int64)
	for i := range command.Request.Events {
		event := &command.Request.Events[i]
		if err := rejectDuplicateMasterUsageEventTx(tx, node, event); err != nil {
			return nil, err
		}
		cacheKey := fmt.Sprintf("%s\\x00%d", event.SnapshotID, event.SnapshotRevision)
		policies := policyCache[cacheKey]
		snapshotID := snapshotIDs[cacheKey]
		if policies == nil {
			snapshot, err := model.GetEdgeCompiledSnapshotForSettlementTx(tx, event.SnapshotID, event.SnapshotRevision)
			if err != nil {
				return nil, err
			}
			policies, err = loadMasterSnapshotPoliciesTx(tx, snapshot.ID, true)
			if err != nil {
				return nil, err
			}
			policyCache[cacheKey] = policies
			snapshotID = snapshot.ID
			snapshotIDs[cacheKey] = snapshot.ID
		}
		if event.PricingRevision != policies.pricingRevision {
			return nil, fmt.Errorf("%w: event %s pricing revision does not match snapshot", ErrMasterSettlementConflict, event.EventID)
		}
		if err := validateMasterSettlementSubject(policies, event); err != nil {
			return nil, err
		}
		chargedQuota, normalizedUsage, err := recomputeMasterUsageQuota(policies, int(event.UserID), event)
		if err != nil {
			return nil, err
		}
		if chargedQuota != event.Billing.ChargedQuota {
			return nil, fmt.Errorf("%w: event %s reported charge=%d master=%d", ErrMasterSettlementConflict, event.EventID, event.Billing.ChargedQuota, chargedQuota)
		}
		charges = append(charges, masterSettlementCharge{
			event: event, snapshotID: snapshotID, chargedQuota: chargedQuota, normalizedUsage: normalizedUsage,
			billingPreference: policies.users[event.UserID].Setting.BillingPreference,
		})
	}
	windowSeconds, windowQuota, err := masterSettlementWindowConfig()
	if err != nil {
		return nil, err
	}
	exceeded, reason, err := masterSettlementWindowExceededTx(tx, node, charges, windowSeconds, windowQuota)
	if err != nil {
		return nil, err
	}
	if exceeded {
		node.SettlementCircuitOpen = true
		node.SettlementCircuitOpenedAt = now.Unix()
		node.SettlementCircuitReason = reason
		node.SettlementCircuitEpoch++
		node.UpdatedAt = now.Unix()
		if err := tx.Save(node).Error; err != nil {
			return nil, err
		}
		return nil, &SettlementCircuitError{Epoch: node.SettlementCircuitEpoch, Reason: reason}
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

	for _, charge := range charges {
		event := charge.event
		chargeResult, err := model.ApplyEdgeSettlementChargeTx(
			tx, int(event.UserID), int(event.TokenID), event.FundingSource, int(event.UserSubscriptionID),
			event.TokenUnlimitedQuota, charge.chargedQuota,
		)
		if err != nil {
			return nil, err
		}
		promptTokens, completionTokens := masterUsageTokenTotals(charge.normalizedUsage)
		usagePayload, err := common.Marshal(event.Usage)
		if err != nil {
			return nil, err
		}
		billingPayload, err := common.Marshal(event.Billing)
		if err != nil {
			return nil, err
		}
		var consumeLogSnapshotPayload *string
		if event.ConsumeLogSnapshot != nil {
			payload, err := common.Marshal(event.ConsumeLogSnapshot)
			if err != nil {
				return nil, err
			}
			value := string(payload)
			consumeLogSnapshotPayload = &value
		}
		settlementFacts := coreservice.TextConsumeLogSettlementFacts{
			BillingSource: event.FundingSource, BillingPreference: charge.billingPreference,
		}
		if event.FundingSource == coreservice.BillingSourceSubscription {
			settlementFacts.SubscriptionID = chargeResult.SubscriptionID
			settlementFacts.SubscriptionPreConsumed = event.Billing.ReservedQuota
			settlementFacts.SubscriptionPostDelta = charge.chargedQuota - event.Billing.ReservedQuota
			settlementFacts.SubscriptionPlanID = chargeResult.SubscriptionPlanID
			settlementFacts.SubscriptionPlanTitle = chargeResult.SubscriptionPlanTitle
			settlementFacts.SubscriptionTotal = chargeResult.SubscriptionTotal
			settlementFacts.SubscriptionUsed = chargeResult.SubscriptionUsed
		}
		settlementPayload, err := common.Marshal(settlementFacts)
		if err != nil {
			return nil, err
		}
		settlementValue := string(settlementPayload)
		httpStatus := 0
		if event.HTTPStatus != nil {
			httpStatus = *event.HTTPStatus
		}
		storedEvent := &model.EdgeUsageEvent{
			NodeID: node.ID, NodeGeneration: node.Generation, BlockID: block.ID,
			EventUID: event.EventID, ReservationUID: event.ReservationID, RequestUID: event.RequestID,
			Sequence: event.Sequence, UserID: int(event.UserID), TokenID: int(event.TokenID),
			SnapshotID: charge.snapshotID, SnapshotRevision: event.SnapshotRevision,
			PricingRevision: event.PricingRevision, BalanceRevision: event.BalanceRevision,
			FundingSource: event.FundingSource, UserSubscriptionID: int(event.UserSubscriptionID),
			TokenUnlimitedQuota: event.TokenUnlimitedQuota,
			ChannelID:           int(event.ChannelID), Endpoint: string(event.Endpoint), Streaming: event.Streaming,
			Model: event.Model, Group: event.Group, Outcome: string(event.Outcome), HTTPStatus: httpStatus,
			ErrorCode: event.ErrorCode, StartedAtUnixMilli: event.StartedAtUnixMilli,
			FirstResponseAtUnixMilli: event.FirstResponseAtUnixMilli,
			FinishedAtUnixMilli:      event.FinishedAtUnixMilli, PromptTokens: promptTokens,
			CompletionTokens: completionTokens, ReservedQuota: event.Billing.ReservedQuota,
			ChargedQuota: charge.chargedQuota, PricingPolicyID: event.Billing.PricingPolicyID,
			PricingPolicyVersion: event.Billing.PricingPolicyVersion,
			UsagePayload:         string(usagePayload), BillingPayload: string(billingPayload),
			ConsumeLogSnapshotPayload:   consumeLogSnapshotPayload,
			ConsumeLogSettlementPayload: &settlementValue,
		}
		if err := tx.Create(storedEvent).Error; err != nil {
			return nil, err
		}
		if promptTokens+completionTokens > 0 {
			if err := model.AddEdgeSettlementStatsTx(tx, int(event.UserID), int(event.TokenID), int(event.ChannelID), charge.chargedQuota, event.FinishedAtUnixMilli/1000); err != nil {
				return nil, err
			}
		}
		outboxPayload, err := common.Marshal(edgeConsumeLogOutboxPayload{
			EventID: event.EventID, NodeID: node.NodeUID, NodeGeneration: node.Generation,
			RequestID: event.RequestID, UserID: int(event.UserID), TokenID: int(event.TokenID),
			ChannelID: int(event.ChannelID), Endpoint: event.Endpoint, Streaming: event.Streaming,
			Model: event.Model, Group: event.Group, Outcome: event.Outcome, HTTPStatus: httpStatus,
			ErrorCode: event.ErrorCode, PromptTokens: promptTokens, CompletionTokens: completionTokens,
			Quota: charge.chargedQuota, StartedAtUnixMilli: event.StartedAtUnixMilli,
			FirstResponseAtUnixMilli: event.FirstResponseAtUnixMilli,
			FinishedAtUnixMilli:      event.FinishedAtUnixMilli,
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
	maxTaskDurationSeconds := common.GetEnvOrDefault("EDGE_MAX_TASK_DURATION_SECONDS", 604800)
	if maxTaskDurationSeconds < maxDurationSeconds || maxTaskDurationSeconds > 2_592_000 {
		return errors.New("edge maximum task duration configuration is invalid")
	}
	maxTaskDurationMillis := int64(maxTaskDurationSeconds) * int64(time.Second/time.Millisecond)
	for i := range request.Events {
		event := request.Events[i]
		eventMaxDurationMillis := maxDurationMillis
		if event.Endpoint == dto.EdgeEndpointTaskV1 || event.Endpoint == dto.EdgeEndpointMidjourneyV1 {
			eventMaxDurationMillis = maxTaskDurationMillis
		}
		if event.StartedAtUnixMilli > latestAllowed || event.FinishedAtUnixMilli > latestAllowed ||
			event.FinishedAtUnixMilli > request.CreatedAtUnixMilli ||
			event.FinishedAtUnixMilli-event.StartedAtUnixMilli > eventMaxDurationMillis {
			return fmt.Errorf("%w: event %s has an invalid settlement timeline", ErrMasterSettlementConflict, event.EventID)
		}
	}
	return nil
}
