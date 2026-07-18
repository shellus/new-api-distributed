package edge

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const (
	defaultMasterSettlementWindowSeconds = int64(300)
	defaultMasterSettlementWindowQuota   = int64(50_000_000)
)

func validateMasterRequestHash(value string) error {
	value = strings.TrimSpace(value)
	decoded, err := hex.DecodeString(value)
	if err != nil || len(value) != 64 || len(decoded) != 32 || value != strings.ToLower(value) {
		return errors.New("edge control request hash must be a lowercase SHA-256 digest")
	}
	return nil
}

func lockMasterControlIdentityTx(tx *gorm.DB, identity *model.EdgeControlIdentity, now time.Time) (*model.EdgeControlIdentity, error) {
	if identity == nil || identity.Node == nil || identity.Credential == nil {
		return nil, errors.New("edge control identity is missing")
	}
	locked, err := model.LockEdgeControlIdentityTx(tx, identity.Node.NodeUID, identity.Node.Generation, identity.Credential.CredentialUID)
	if err != nil {
		return nil, err
	}
	if locked.Node.ID != identity.Node.ID || locked.Credential.ID != identity.Credential.ID ||
		locked.Credential.Fingerprint != identity.Credential.Fingerprint {
		return nil, ErrControlAuthentication
	}
	if err := locked.Credential.ValidateAt(now.Unix()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrControlAuthentication, err)
	}
	return locked, nil
}

func loadMasterSnapshotPoliciesTx(tx *gorm.DB, snapshotID int64, includePricing bool) (*masterSnapshotPolicies, error) {
	policies := &masterSnapshotPolicies{
		authentication: make(map[int64]dto.EdgeTokenAuthRecordV1),
		users:          make(map[int64]dto.EdgeUserPolicyV1),
		groups:         make(map[string]dto.EdgeGroupPolicyV1),
		models:         make(map[string]dto.EdgeModelPolicyV1),
		channels:       make(map[int64]dto.EdgeChannelProjectionV1),
		pricing:        make(map[string]dto.EdgePricingPolicyV1),
	}
	datasets := []dto.EdgeSnapshotDatasetV1{
		dto.EdgeSnapshotDatasetAuthenticationV1,
		dto.EdgeSnapshotDatasetUsersV1,
		dto.EdgeSnapshotDatasetGroupsV1,
	}
	if includePricing {
		datasets = append(datasets,
			dto.EdgeSnapshotDatasetModelsV1,
			dto.EdgeSnapshotDatasetChannelsV1,
			dto.EdgeSnapshotDatasetPricingV1,
		)
	}
	for _, dataset := range datasets {
		if dataset == dto.EdgeSnapshotDatasetPricingV1 {
			var stored model.EdgeCompiledSnapshotDataset
			if err := tx.Where("snapshot_id = ? AND dataset = ?", snapshotID, dataset).First(&stored).Error; err != nil {
				return nil, err
			}
			policies.pricingRevision = stored.Revision
		}
		pages, err := model.GetEdgeCompiledSnapshotDatasetPagesTx(tx, snapshotID, dataset)
		if err != nil {
			return nil, err
		}
		for i := range pages {
			if pages[i].ItemCount <= 0 || pages[i].ItemCount > int64(dto.EdgeControlMaxSnapshotPageLimitV1) {
				return nil, model.ErrEdgeCompiledSnapshotIncomplete
			}
			var payload dto.EdgeSnapshotPagePayloadV1
			if err := common.UnmarshalJsonStr(pages[i].Payload, &payload); err != nil {
				return nil, err
			}
			if err := payload.Validate(dataset, int(pages[i].ItemCount)); err != nil {
				return nil, err
			}
			switch dataset {
			case dto.EdgeSnapshotDatasetAuthenticationV1:
				for _, record := range payload.Authentication {
					if _, exists := policies.authentication[record.TokenID]; exists {
						return nil, model.ErrEdgeCompiledSnapshotIncomplete
					}
					policies.authentication[record.TokenID] = record
				}
			case dto.EdgeSnapshotDatasetUsersV1:
				for _, record := range payload.Users {
					if _, exists := policies.users[record.UserID]; exists {
						return nil, model.ErrEdgeCompiledSnapshotIncomplete
					}
					policies.users[record.UserID] = record
				}
			case dto.EdgeSnapshotDatasetGroupsV1:
				for _, record := range payload.Groups {
					if _, exists := policies.groups[record.UserGroup]; exists {
						return nil, model.ErrEdgeCompiledSnapshotIncomplete
					}
					policies.groups[record.UserGroup] = record
				}
			case dto.EdgeSnapshotDatasetModelsV1:
				for _, record := range payload.Models {
					if _, exists := policies.models[record.Model]; exists {
						return nil, model.ErrEdgeCompiledSnapshotIncomplete
					}
					policies.models[record.Model] = record
				}
			case dto.EdgeSnapshotDatasetChannelsV1:
				for _, record := range payload.Channels {
					if _, exists := policies.channels[record.ChannelID]; exists {
						return nil, model.ErrEdgeCompiledSnapshotIncomplete
					}
					policies.channels[record.ChannelID] = record
				}
			case dto.EdgeSnapshotDatasetPricingV1:
				for _, record := range payload.Pricing {
					key := masterPricingKey(record.PolicyID, record.Version, record.Model)
					if _, exists := policies.pricing[key]; exists {
						return nil, model.ErrEdgeCompiledSnapshotIncomplete
					}
					policies.pricing[key] = record
				}
			}
		}
	}
	return policies, nil
}

func findMasterSettlementReplayTx(tx *gorm.DB, node *model.EdgeNode, command MasterSettlementCommand) (*dto.EdgeSettlementAckV1, error) {
	var byBlock model.EdgeSettlementBlock
	query := tx.Where("node_id = ? AND node_generation = ? AND block_uid = ?", node.ID, node.Generation, command.Request.BlockID).
		Limit(1).Find(&byBlock)
	if query.Error != nil {
		return nil, query.Error
	}
	if query.RowsAffected == 1 {
		if byBlock.IdempotencyKey != command.IdempotencyKey || byBlock.RequestHash != command.RequestHash ||
			byBlock.BlockDigest != command.Request.BlockDigest || byBlock.FirstSequence != command.Request.FirstSequence ||
			byBlock.LastSequence != command.Request.LastSequence {
			return nil, ErrMasterSettlementConflict
		}
		return &dto.EdgeSettlementAckV1{
			Status: dto.EdgeSettlementAckDuplicateV1, NodeID: node.NodeUID, NodeGeneration: node.Generation,
			BlockID: byBlock.BlockUID, AckedThroughSequence: byBlock.LastSequence,
			NextExpectedSequence: byBlock.LastSequence + 1, AcceptedEventCount: byBlock.EventCount,
			AcknowledgedAtUnixMilli: byBlock.AcknowledgedAtUnixMilli,
		}, nil
	}
	var byIdempotency model.EdgeSettlementBlock
	query = tx.Where("node_id = ? AND node_generation = ? AND idempotency_key = ?", node.ID, node.Generation, command.IdempotencyKey).
		Limit(1).Find(&byIdempotency)
	if query.Error != nil {
		return nil, query.Error
	}
	if query.RowsAffected == 1 {
		return nil, ErrMasterSettlementConflict
	}
	return nil, nil
}

func validateMasterSettlementChainTx(tx *gorm.DB, node *model.EdgeNode, request *dto.EdgeSettlementBlockRequestV1) error {
	if node.LastBlockSeq == 0 {
		if request.PreviousBlockID != "" || request.PreviousBlockDigest != "" {
			return ErrMasterSettlementConflict
		}
		return nil
	}
	var previous model.EdgeSettlementBlock
	if err := tx.Where("node_id = ? AND node_generation = ? AND block_ordinal = ?", node.ID, node.Generation, node.LastBlockSeq).
		First(&previous).Error; err != nil {
		return err
	}
	if request.PreviousBlockID != previous.BlockUID || request.PreviousBlockDigest != previous.BlockDigest {
		return ErrMasterSettlementConflict
	}
	return nil
}

func masterSettlementWindowConfig() (int64, int64, error) {
	windowSeconds := int64(common.GetEnvOrDefault("EDGE_NODE_SETTLEMENT_WINDOW_SECONDS", int(defaultMasterSettlementWindowSeconds)))
	if windowSeconds < 10 || windowSeconds > 86_400 {
		return 0, 0, errors.New("EDGE_NODE_SETTLEMENT_WINDOW_SECONDS must be between 10 and 86400")
	}
	windowQuota := int64(common.GetEnvOrDefault("EDGE_NODE_SETTLEMENT_WINDOW_QUOTA", int(defaultMasterSettlementWindowQuota)))
	if windowQuota < 1 || windowQuota > int64(common.MaxQuota) {
		return 0, 0, errors.New("EDGE_NODE_SETTLEMENT_WINDOW_QUOTA must be between 1 and common.MaxQuota")
	}
	return windowSeconds, windowQuota, nil
}

func masterSettlementWindowExceededTx(
	tx *gorm.DB,
	node *model.EdgeNode,
	charges []masterSettlementCharge,
	windowSeconds int64,
	windowQuota int64,
) (bool, string, error) {
	if tx == nil || node == nil || len(charges) == 0 || windowSeconds <= 0 || windowQuota <= 0 {
		return false, "", errors.New("invalid edge settlement window check")
	}
	if charges[0].event == nil {
		return false, "", ErrMasterSettlementConflict
	}
	minFinished := charges[0].event.FinishedAtUnixMilli
	maxFinished := minFinished
	for _, charge := range charges {
		if charge.event == nil || charge.chargedQuota < 0 || charge.chargedQuota > int64(common.MaxQuota) {
			return false, "", ErrMasterSettlementConflict
		}
		if charge.event.FinishedAtUnixMilli < minFinished {
			minFinished = charge.event.FinishedAtUnixMilli
		}
		if charge.event.FinishedAtUnixMilli > maxFinished {
			maxFinished = charge.event.FinishedAtUnixMilli
		}
	}
	windowMillis := windowSeconds * int64(time.Second/time.Millisecond)
	type usagePoint struct {
		FinishedAtUnixMilli int64
		ChargedQuota        int64
	}
	points := make([]usagePoint, 0, len(charges))
	var historical []usagePoint
	if err := tx.Model(&model.EdgeUsageEvent{}).
		Select("finished_at_unix_milli", "charged_quota").
		Where("node_id = ? AND node_generation = ? AND finished_at_unix_milli > ? AND finished_at_unix_milli <= ?",
			node.ID, node.Generation, minFinished-windowMillis, maxFinished).
		Find(&historical).Error; err != nil {
		return false, "", err
	}
	for _, point := range historical {
		if point.ChargedQuota < 0 || point.ChargedQuota > int64(common.MaxQuota) {
			return false, "", ErrMasterSettlementConflict
		}
	}
	points = append(points, historical...)
	for _, charge := range charges {
		points = append(points, usagePoint{FinishedAtUnixMilli: charge.event.FinishedAtUnixMilli, ChargedQuota: charge.chargedQuota})
	}
	sort.Slice(points, func(i, j int) bool {
		return points[i].FinishedAtUnixMilli < points[j].FinishedAtUnixMilli
	})
	left := 0
	current := int64(0)
	for right := range points {
		for left < right && points[left].FinishedAtUnixMilli <= points[right].FinishedAtUnixMilli-windowMillis {
			current -= points[left].ChargedQuota
			left++
		}
		if points[right].ChargedQuota > windowQuota-current {
			return true, fmt.Sprintf("event-time settlement window exceeded quota=%d window_seconds=%d", windowQuota, windowSeconds), nil
		}
		current += points[right].ChargedQuota
	}
	return false, "", nil
}

func validateMasterSettlementSubject(policies *masterSnapshotPolicies, event *dto.EdgeUsageEventV1) error {
	if policies == nil || event == nil {
		return ErrMasterSettlementConflict
	}
	auth, exists := policies.authentication[event.TokenID]
	if !exists || !auth.Enabled || auth.UserID != event.UserID || auth.TokenID != event.TokenID {
		return ErrMasterSettlementConflict
	}
	user, exists := policies.users[event.UserID]
	if !exists || !user.Enabled {
		return ErrMasterSettlementConflict
	}
	if _, exists := policies.groups[user.DefaultGroup]; !exists {
		return ErrMasterSettlementConflict
	}
	return nil
}

func rejectDuplicateMasterUsageEventTx(tx *gorm.DB, node *model.EdgeNode, event *dto.EdgeUsageEventV1) error {
	var count int64
	if err := tx.Model(&model.EdgeUsageEvent{}).
		Where("node_id = ? AND node_generation = ? AND (sequence = ? OR event_uid = ? OR reservation_uid = ?)",
			node.ID, node.Generation, event.Sequence, event.EventID, event.ReservationID).
		Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return ErrMasterSettlementConflict
	}
	return nil
}

func masterPricingKey(policyID string, version string, modelName string) string {
	return policyID + "\x00" + version + "\x00" + modelName
}

func masterContainsEndpoint(values []dto.EdgeEndpointV1, target dto.EdgeEndpointV1) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func masterContainsInt64(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func masterContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func recomputeMasterUsageQuota(policies *masterSnapshotPolicies, userID int, event *dto.EdgeUsageEventV1) (int64, *dto.Usage, error) {
	if policies == nil || event == nil {
		return 0, nil, ErrMasterSettlementConflict
	}
	policy, exists := policies.pricing[masterPricingKey(event.Billing.PricingPolicyID, event.Billing.PricingPolicyVersion, event.Model)]
	if !exists || policy.BillingMode != event.Billing.BillingMode {
		return 0, nil, ErrMasterSettlementConflict
	}
	if len(event.Billing.AppliedRatios) != 0 {
		// V1 intentionally has no snapshot representation that would let master
		// independently derive request-specific multipliers.
		return 0, nil, ErrMasterSettlementConflict
	}
	modelPolicy, exists := policies.models[event.Model]
	if !exists || !modelPolicy.Enabled || !masterContainsEndpoint(modelPolicy.Endpoints, event.Endpoint) ||
		!masterContainsInt64(modelPolicy.ChannelIDs, event.ChannelID) {
		return 0, nil, ErrMasterSettlementConflict
	}
	channelPolicy, exists := policies.channels[event.ChannelID]
	if !exists || !channelPolicy.Enabled || !masterContainsString(channelPolicy.Models, event.Model) ||
		!masterContainsString(channelPolicy.Groups, event.Group) {
		return 0, nil, ErrMasterSettlementConflict
	}
	userPolicy, exists := policies.users[int64(userID)]
	if !exists || !userPolicy.Enabled {
		return 0, nil, ErrMasterSettlementConflict
	}
	groupPolicy, exists := policies.groups[userPolicy.DefaultGroup]
	if !exists {
		return 0, nil, ErrMasterSettlementConflict
	}
	expectedGroupRatio := 0.0
	groupFound := false
	for _, usingGroup := range groupPolicy.UsingGroups {
		if usingGroup.Enabled && usingGroup.Group == event.Group {
			expectedGroupRatio = usingGroup.Ratio
			groupFound = true
			break
		}
	}
	if !groupFound || math.Abs(expectedGroupRatio-event.Billing.GroupRatio) > 1e-12 {
		return 0, nil, ErrMasterSettlementConflict
	}
	if event.Usage == nil && event.Outcome == dto.EdgeUsageOutcomeSuccessV1 {
		return 0, nil, ErrMasterSettlementConflict
	}
	usage, err := normalizeMasterBillingUsage(event.Usage)
	if err != nil {
		return 0, nil, err
	}
	if usage.PromptTokensDetails.ImageTokens != 0 || usage.PromptTokensDetails.AudioTokens != 0 ||
		usage.CompletionTokenDetails.ImageTokens != 0 || usage.CompletionTokenDetails.AudioTokens != 0 {
		return 0, nil, ErrMasterSettlementConflict
	}

	totalTokens := int64(usage.PromptTokens) + int64(usage.CompletionTokens)
	if totalTokens == 0 {
		return 0, usage, nil
	}
	groupRatio := decimal.NewFromFloat(expectedGroupRatio)
	quotaPerUnit := decimal.NewFromFloat(policy.QuotaPerUnit)
	var quotaDecimal decimal.Decimal
	switch policy.BillingMode {
	case dto.EdgeBillingModeFixedPriceV1:
		if policy.ModelPrice == nil {
			return 0, nil, ErrMasterSettlementConflict
		}
		quotaDecimal = decimal.NewFromFloat(*policy.ModelPrice).Mul(quotaPerUnit).Mul(groupRatio)
	case dto.EdgeBillingModeRatioV1:
		if policy.ModelRatio == nil {
			return 0, nil, ErrMasterSettlementConflict
		}
		completionRatio := masterOptionalRatio(policy.CompletionRatio, 1)
		cacheReadRatio := masterOptionalRatio(policy.CacheReadRatio, 1)
		cacheCreationRatio := masterOptionalRatio(policy.CacheCreationRatio, 1)
		cacheCreation1hRatio := masterOptionalRatio(policy.CacheCreation1hRatio, cacheCreationRatio)

		promptTokens := int64(usage.PromptTokens)
		cachedTokens := int64(usage.PromptTokensDetails.CachedTokens)
		cacheCreationTokens := int64(usage.PromptTokensDetails.CacheCreationTokensTotal())
		cacheCreation1hTokens := int64(usage.ClaudeCacheCreation1hTokens)
		if cacheCreation1hTokens > cacheCreationTokens {
			cacheCreation1hTokens = cacheCreationTokens
		}
		cacheCreationDefaultTokens := cacheCreationTokens - cacheCreation1hTokens
		basePromptTokens := promptTokens
		if usage.UsageSemantic != dto.BillingUsageSemanticAnthropic {
			basePromptTokens -= cachedTokens + cacheCreationTokens
			if basePromptTokens < 0 {
				basePromptTokens = 0
			}
		}
		promptCost := decimal.NewFromInt(basePromptTokens).
			Add(decimal.NewFromInt(cachedTokens).Mul(decimal.NewFromFloat(cacheReadRatio))).
			Add(decimal.NewFromInt(cacheCreationDefaultTokens).Mul(decimal.NewFromFloat(cacheCreationRatio))).
			Add(decimal.NewFromInt(cacheCreation1hTokens).Mul(decimal.NewFromFloat(cacheCreation1hRatio)))
		completionCost := decimal.NewFromInt(int64(usage.CompletionTokens)).Mul(decimal.NewFromFloat(completionRatio))
		quotaDecimal = promptCost.Add(completionCost).
			Mul(decimal.NewFromFloat(*policy.ModelRatio)).Mul(groupRatio)
	case dto.EdgeBillingModeTieredExprV1:
		return 0, nil, ErrMasterDynamicPricingUnsupported
	default:
		return 0, nil, ErrMasterSettlementConflict
	}
	quota, clamp := common.QuotaFromDecimalChecked(quotaDecimal)
	if clamp != nil {
		return 0, nil, clamp
	}
	if quota < 0 {
		return 0, nil, ErrMasterSettlementConflict
	}
	if policy.BillingMode == dto.EdgeBillingModeRatioV1 && policy.ModelRatio != nil &&
		*policy.ModelRatio != 0 && expectedGroupRatio != 0 && quota == 0 {
		quota = 1
	}
	return int64(quota), usage, nil
}

func masterOptionalRatio(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

func normalizeMasterBillingUsage(billingUsage *dto.BillingUsage) (*dto.Usage, error) {
	if billingUsage == nil {
		return &dto.Usage{}, nil
	}
	switch billingUsage.Source {
	case dto.BillingUsageSourceOAIChat, dto.BillingUsageSourceOAIResponses:
		if billingUsage.OpenAIUsage == nil || billingUsage.Semantic != dto.BillingUsageSemanticOpenAI {
			return nil, ErrMasterSettlementConflict
		}
		usage := *billingUsage.OpenAIUsage
		if usage.PromptTokens == 0 && usage.InputTokens > 0 {
			usage.PromptTokens = usage.InputTokens
		}
		if usage.CompletionTokens == 0 && usage.OutputTokens > 0 {
			usage.CompletionTokens = usage.OutputTokens
		}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		usage.UsageSemantic = dto.BillingUsageSemanticOpenAI
		usage.UsageSource = billingUsage.Source
		usage.BillingUsage = nil
		return &usage, nil
	case dto.BillingUsageSourceClaudeMessages:
		if billingUsage.ClaudeUsage == nil || billingUsage.Semantic != dto.BillingUsageSemanticAnthropic {
			return nil, ErrMasterSettlementConflict
		}
		claudeUsage := billingUsage.ClaudeUsage
		cacheCreation5m := claudeUsage.GetCacheCreation5mTokens()
		if cacheCreation5m == 0 {
			cacheCreation5m = claudeUsage.ClaudeCacheCreation5mTokens
		}
		cacheCreation1h := claudeUsage.GetCacheCreation1hTokens()
		if cacheCreation1h == 0 {
			cacheCreation1h = claudeUsage.ClaudeCacheCreation1hTokens
		}
		usage := &dto.Usage{
			PromptTokens: claudeUsage.InputTokens, CompletionTokens: claudeUsage.OutputTokens,
			TotalTokens:   claudeUsage.InputTokens + claudeUsage.OutputTokens,
			UsageSemantic: dto.BillingUsageSemanticAnthropic, UsageSource: dto.BillingUsageSourceClaudeMessages,
			ClaudeCacheCreation5mTokens: cacheCreation5m, ClaudeCacheCreation1hTokens: cacheCreation1h,
		}
		usage.PromptTokensDetails.CachedTokens = claudeUsage.CacheReadInputTokens
		usage.PromptTokensDetails.CachedCreationTokens = claudeUsage.GetCacheCreationTotalTokens()
		return usage, nil
	case dto.BillingUsageSourceGeminiChat:
		if billingUsage.GeminiUsageMetadata == nil || billingUsage.Semantic != dto.BillingUsageSemanticGemini {
			return nil, ErrMasterSettlementConflict
		}
		metadata := billingUsage.GeminiUsageMetadata
		usage := &dto.Usage{
			PromptTokens:     metadata.PromptTokenCount + metadata.ToolUsePromptTokenCount,
			CompletionTokens: metadata.CandidatesTokenCount + metadata.ThoughtsTokenCount,
			TotalTokens:      metadata.TotalTokenCount,
			UsageSemantic:    dto.BillingUsageSemanticGemini, UsageSource: dto.BillingUsageSourceGeminiChat,
		}
		usage.PromptTokensDetails.CachedTokens = metadata.CachedContentTokenCount
		for _, detail := range metadata.PromptTokensDetails {
			addMasterGeminiInputDetail(&usage.PromptTokensDetails, detail)
		}
		for _, detail := range metadata.ToolUsePromptTokensDetails {
			addMasterGeminiInputDetail(&usage.PromptTokensDetails, detail)
		}
		for _, detail := range metadata.CandidatesTokensDetails {
			switch detail.Modality {
			case "IMAGE":
				usage.CompletionTokenDetails.ImageTokens += detail.TokenCount
			case "AUDIO":
				usage.CompletionTokenDetails.AudioTokens += detail.TokenCount
			case "TEXT":
				usage.CompletionTokenDetails.TextTokens += detail.TokenCount
			}
		}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		return usage, nil
	default:
		return nil, ErrMasterSettlementConflict
	}
}

func addMasterGeminiInputDetail(details *dto.InputTokenDetails, detail dto.GeminiPromptTokensDetails) {
	switch detail.Modality {
	case "AUDIO":
		details.AudioTokens += detail.TokenCount
	case "IMAGE":
		details.ImageTokens += detail.TokenCount
	case "TEXT":
		details.TextTokens += detail.TokenCount
	}
}

func masterUsageTokenTotals(usage *dto.Usage) (int, int) {
	if usage == nil {
		return 0, 0
	}
	return usage.PromptTokens, usage.CompletionTokens
}
