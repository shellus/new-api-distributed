package edge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
)

const (
	defaultConsumeLogOutboxBatchSize = 100
	maxConsumeLogOutboxBatchSize     = 1000
	consumeLogOutboxClaimTTL         = 30 * time.Second
	consumeLogOutboxBaseBackoff      = 2 * time.Second
	consumeLogOutboxMaxBackoff       = 5 * time.Minute
)

// RunMasterConsumeLogOutbox materializes authoritative settlement events into
// the existing consume-log table. Claims are durable and expire, so another
// master can resume after a crash without losing or duplicating a log row.
func RunMasterConsumeLogOutbox(ctx context.Context) {
	intervalSeconds := common.GetEnvOrDefault("EDGE_CONSUME_LOG_OUTBOX_INTERVAL_SECONDS", 2)
	if intervalSeconds < 1 {
		intervalSeconds = 1
	}
	batchSize := common.GetEnvOrDefault("EDGE_CONSUME_LOG_OUTBOX_BATCH_SIZE", defaultConsumeLogOutboxBatchSize)
	if batchSize < 1 {
		batchSize = 1
	}
	if batchSize > maxConsumeLogOutboxBatchSize {
		batchSize = maxConsumeLogOutboxBatchSize
	}

	drain := func() {
		for {
			processed, err := PublishMasterConsumeLogOutboxBatch(ctx, time.Time{}, batchSize)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					common.SysError("edge consume-log outbox publish failed: " + err.Error())
				}
				return
			}
			if processed < batchSize {
				return
			}
		}
	}
	drain()
	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			drain()
		}
	}
}

// PublishMasterConsumeLogOutboxBatch processes at most batchSize ready rows.
// It is exported for deterministic recovery and integration tests.
func PublishMasterConsumeLogOutboxBatch(ctx context.Context, now time.Time, batchSize int) (int, error) {
	if ctx == nil {
		return 0, errors.New("edge consume-log outbox context is nil")
	}
	if batchSize < 1 || batchSize > maxConsumeLogOutboxBatchSize {
		return 0, fmt.Errorf("edge consume-log outbox batch size must be between 1 and %d", maxConsumeLogOutboxBatchSize)
	}
	useWallClock := now.IsZero()
	processed := 0
	var publishErrors []error
	for processed < batchSize {
		if err := ctx.Err(); err != nil {
			return processed, errors.Join(append(publishErrors, err)...)
		}
		claimNow := now
		if useWallClock {
			claimNow = time.Now()
		}
		claim, err := model.ClaimEdgeConsumeLogOutbox(ctx, claimNow, consumeLogOutboxClaimTTL)
		if err != nil {
			publishErrors = append(publishErrors, err)
			break
		}
		if claim == nil {
			break
		}
		processed++
		if err := publishMasterConsumeLogClaim(ctx, claim); err != nil {
			markNow := now
			if useWallClock {
				markNow = time.Now()
			}
			retryAt := markNow.Add(consumeLogOutboxBackoff(claim.Attempts))
			if markErr := model.MarkEdgeConsumeLogOutboxFailed(ctx, claim, err, retryAt, markNow); markErr != nil {
				err = errors.Join(err, markErr)
			}
			publishErrors = append(publishErrors, fmt.Errorf("event %s: %w", claim.EventUID, err))
			continue
		}
		markNow := now
		if useWallClock {
			markNow = time.Now()
		}
		if err := model.MarkEdgeConsumeLogOutboxPublished(ctx, claim, markNow); err != nil {
			publishErrors = append(publishErrors, fmt.Errorf("event %s: %w", claim.EventUID, err))
		}
	}
	return processed, errors.Join(publishErrors...)
}

func publishMasterConsumeLogClaim(ctx context.Context, claim *model.EdgeConsumeLogOutbox) error {
	if claim == nil {
		return errors.New("edge consume-log outbox claim is nil")
	}
	var payload edgeConsumeLogOutboxPayload
	if err := common.UnmarshalJsonStr(claim.Payload, &payload); err != nil {
		return fmt.Errorf("decode outbox payload: %w", err)
	}

	db := model.DB.WithContext(ctx)
	var stored model.EdgeUsageEvent
	if err := db.First(&stored, claim.EventID).Error; err != nil {
		return fmt.Errorf("load edge usage event: %w", err)
	}
	var node model.EdgeNode
	if err := db.First(&node, stored.NodeID).Error; err != nil {
		return fmt.Errorf("load edge usage node: %w", err)
	}
	var snapshot model.EdgeCompiledSnapshot
	if err := db.First(&snapshot, stored.SnapshotID).Error; err != nil {
		return fmt.Errorf("load edge usage snapshot: %w", err)
	}
	billingEventKey, err := validateConsumeLogOutboxProjection(claim, payload, stored, node)
	if err != nil {
		return err
	}

	var usage *dto.BillingUsage
	if err := common.UnmarshalJsonStr(stored.UsagePayload, &usage); err != nil {
		return fmt.Errorf("decode stored edge usage: %w", err)
	}
	var billing dto.EdgeUsageBillingV1
	if err := common.UnmarshalJsonStr(stored.BillingPayload, &billing); err != nil {
		return fmt.Errorf("decode stored edge billing: %w", err)
	}
	httpStatus := (*int)(nil)
	if stored.HTTPStatus != 0 {
		status := stored.HTTPStatus
		httpStatus = &status
	}
	event := dto.EdgeUsageEventV1{
		EventID: payload.EventID, Sequence: stored.Sequence,
		ReservationID: stored.ReservationUID, RequestID: stored.RequestUID,
		UserID: int64(stored.UserID), TokenID: int64(stored.TokenID),
		SnapshotID: snapshot.SnapshotUID, SnapshotRevision: stored.SnapshotRevision,
		PricingRevision: stored.PricingRevision, BalanceRevision: stored.BalanceRevision,
		FundingSource: stored.FundingSource, UserSubscriptionID: int64(stored.UserSubscriptionID),
		TokenUnlimitedQuota: stored.TokenUnlimitedQuota, ChannelID: int64(stored.ChannelID),
		Endpoint: dto.EdgeEndpointV1(stored.Endpoint), Streaming: stored.Streaming,
		Model: stored.Model, Group: stored.Group, StartedAtUnixMilli: stored.StartedAtUnixMilli,
		FirstResponseAtUnixMilli: stored.FirstResponseAtUnixMilli,
		FinishedAtUnixMilli:      stored.FinishedAtUnixMilli, Outcome: dto.EdgeUsageOutcomeV1(stored.Outcome),
		HTTPStatus: httpStatus, ErrorCode: stored.ErrorCode, Usage: usage, Billing: billing,
	}
	if err := event.Validate(); err != nil {
		return fmt.Errorf("validate stored edge usage event: %w", err)
	}
	if !common.LogConsumeEnabled {
		return nil
	}

	username := ""
	if err := db.Unscoped().Model(&model.User{}).Select("username").Where("id = ?", stored.UserID).Scan(&username).Error; err != nil {
		return fmt.Errorf("load edge usage username: %w", err)
	}
	tokenName := ""
	if err := db.Unscoped().Model(&model.Token{}).Select("name").Where("id = ? AND user_id = ?", stored.TokenID, stored.UserID).Scan(&tokenName).Error; err != nil {
		return fmt.Errorf("load edge usage token name: %w", err)
	}
	requestPath := "/v1/chat/completions"
	if event.Endpoint == dto.EdgeEndpointOpenAIResponsesV1 {
		requestPath = "/v1/responses"
	}
	adminInfo := map[string]interface{}{
		"edge_event_id":        event.EventID,
		"edge_node_id":         payload.NodeID,
		"edge_node_generation": payload.NodeGeneration,
		"edge_funding_source":  event.FundingSource,
		"edge_outcome":         event.Outcome,
		"edge_endpoint":        event.Endpoint,
	}
	if event.HTTPStatus != nil {
		adminInfo["edge_http_status"] = *event.HTTPStatus
	}
	if event.ErrorCode != "" {
		adminInfo["edge_error_code"] = event.ErrorCode
	}
	if event.Usage != nil {
		adminInfo["edge_usage"] = event.Usage
	}
	other := map[string]interface{}{
		"request_path":           requestPath,
		"edge_settlement":        true,
		"pricing_policy_id":      billing.PricingPolicyID,
		"pricing_policy_version": billing.PricingPolicyVersion,
		"billing_mode":           billing.BillingMode,
		"group_ratio":            billing.GroupRatio,
		"applied_ratios":         billing.AppliedRatios,
		"admin_info":             adminInfo,
	}
	if event.FirstResponseAtUnixMilli != nil {
		other["frt"] = float64(*event.FirstResponseAtUnixMilli - event.StartedAtUnixMilli)
	}
	durationSeconds := (event.FinishedAtUnixMilli - event.StartedAtUnixMilli) / 1000
	if durationSeconds > math.MaxInt32 {
		durationSeconds = math.MaxInt32
	}
	log := &model.Log{
		UserId: stored.UserID, Username: username, CreatedAt: event.FinishedAtUnixMilli / 1000,
		Type: model.LogTypeConsume, Content: "Edge settlement usage",
		PromptTokens: stored.PromptTokens, CompletionTokens: stored.CompletionTokens,
		TokenName: tokenName, ModelName: stored.Model, Quota: int(stored.ChargedQuota),
		ChannelId: stored.ChannelID, TokenId: stored.TokenID, UseTime: int(durationSeconds),
		IsStream: stored.Streaming, Group: stored.Group, RequestId: stored.RequestUID,
		Other: common.MapToJsonStr(other),
	}
	if _, err = model.CreateEdgeConsumeLogOnce(ctx, log, billingEventKey); err != nil {
		return err
	}
	if common.DataExportEnabled {
		_, err = model.RecordEdgeQuotaDataOnce(ctx, billingEventKey, model.QuotaDataLogParams{
			UserID: stored.UserID, Username: username, ModelName: stored.Model,
			Quota: int(stored.ChargedQuota), CreatedAt: event.FinishedAtUnixMilli / 1000,
			TokenUsed: stored.PromptTokens + stored.CompletionTokens, UseGroup: stored.Group,
			TokenID: stored.TokenID, ChannelID: stored.ChannelID, NodeName: node.NodeUID,
		})
	}
	return err
}

func validateConsumeLogOutboxProjection(claim *model.EdgeConsumeLogOutbox, payload edgeConsumeLogOutboxPayload, stored model.EdgeUsageEvent, node model.EdgeNode) (string, error) {
	billingEventKey, err := model.EdgeConsumeLogBillingEventKey(node.NodeUID, stored.NodeGeneration, stored.EventUID)
	if err != nil {
		return "", fmt.Errorf("derive edge consume-log billing event key: %w", err)
	}
	// Rows created before the master-global billing key was introduced stored
	// the raw edge event ID in EventUID. Accept them for upgrade compatibility,
	// but always publish with the derived global key.
	if claim.EventUID != billingEventKey && claim.EventUID != stored.EventUID {
		return "", errors.New("edge consume-log outbox event identity is invalid")
	}
	if stored.ID != claim.EventID || payload.EventID != stored.EventUID ||
		payload.NodeID != node.NodeUID || payload.NodeGeneration != stored.NodeGeneration || stored.NodeID != node.ID ||
		payload.RequestID != stored.RequestUID || payload.UserID != stored.UserID || payload.TokenID != stored.TokenID ||
		payload.ChannelID != stored.ChannelID || string(payload.Endpoint) != stored.Endpoint || payload.Streaming != stored.Streaming ||
		payload.Model != stored.Model || payload.Group != stored.Group || string(payload.Outcome) != stored.Outcome ||
		payload.HTTPStatus != stored.HTTPStatus || payload.ErrorCode != stored.ErrorCode ||
		payload.PromptTokens != stored.PromptTokens || payload.CompletionTokens != stored.CompletionTokens ||
		payload.Quota != stored.ChargedQuota || payload.StartedAtUnixMilli != stored.StartedAtUnixMilli ||
		(payload.FirstResponseAtUnixMilli == nil) != (stored.FirstResponseAtUnixMilli == nil) ||
		(payload.FirstResponseAtUnixMilli != nil && *payload.FirstResponseAtUnixMilli != *stored.FirstResponseAtUnixMilli) ||
		payload.FinishedAtUnixMilli != stored.FinishedAtUnixMilli {
		return "", errors.New("edge consume-log outbox payload does not match the authoritative usage event")
	}
	return billingEventKey, nil
}

func consumeLogOutboxBackoff(attempts int) time.Duration {
	if attempts <= 1 {
		return consumeLogOutboxBaseBackoff
	}
	shift := attempts - 1
	if shift > 8 {
		shift = 8
	}
	backoff := consumeLogOutboxBaseBackoff * time.Duration(1<<shift)
	if backoff > consumeLogOutboxMaxBackoff {
		return consumeLogOutboxMaxBackoff
	}
	return backoff
}
