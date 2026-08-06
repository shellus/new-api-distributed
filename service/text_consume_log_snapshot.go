package service

import (
	"math"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

// TextConsumeLogSettlementFacts contains the authoritative post-settlement
// fields that are finalized after the request-time log snapshot was captured.
type TextConsumeLogSettlementFacts struct {
	BillingSource           string `json:"billing_source,omitempty"`
	BillingPreference       string `json:"billing_preference,omitempty"`
	SubscriptionID          int    `json:"subscription_id,omitempty"`
	SubscriptionPreConsumed int64  `json:"subscription_pre_consumed,omitempty"`
	SubscriptionPostDelta   int64  `json:"subscription_post_delta,omitempty"`
	SubscriptionPlanID      int    `json:"subscription_plan_id,omitempty"`
	SubscriptionPlanTitle   string `json:"subscription_plan_title,omitempty"`
	SubscriptionTotal       int64  `json:"subscription_total,omitempty"`
	SubscriptionUsed        int64  `json:"subscription_used,omitempty"`
}

func TextConsumeLogSettlementFactsFromRelayInfo(relayInfo *relaycommon.RelayInfo) TextConsumeLogSettlementFacts {
	if relayInfo == nil {
		return TextConsumeLogSettlementFacts{}
	}
	usedFinal := relayInfo.SubscriptionAmountUsedAfterPreConsume + relayInfo.SubscriptionPostDelta
	if usedFinal < 0 {
		usedFinal = 0
	}
	return TextConsumeLogSettlementFacts{
		BillingSource:           relayInfo.BillingSource,
		BillingPreference:       relayInfo.UserSetting.BillingPreference,
		SubscriptionID:          relayInfo.SubscriptionId,
		SubscriptionPreConsumed: relayInfo.SubscriptionPreConsumed,
		SubscriptionPostDelta:   relayInfo.SubscriptionPostDelta,
		SubscriptionPlanID:      relayInfo.SubscriptionPlanId,
		SubscriptionPlanTitle:   relayInfo.SubscriptionPlanTitle,
		SubscriptionTotal:       relayInfo.SubscriptionAmountTotal,
		SubscriptionUsed:        usedFinal,
	}
}

// FinalizeTextConsumeLogSnapshot applies authoritative post-settlement billing
// state to the request-time snapshot. Both direct master logging and edge
// settlement projection call this function.
func FinalizeTextConsumeLogSnapshot(snapshot *dto.EdgeConsumeLogSnapshotV1, facts TextConsumeLogSettlementFacts) (*dto.EdgeConsumeLogSnapshotV1, error) {
	finalized, err := dto.CloneEdgeConsumeLogSnapshotV1(snapshot)
	if err != nil {
		return nil, err
	}
	if finalized == nil {
		finalized = &dto.EdgeConsumeLogSnapshotV1{}
	}
	if finalized.Other == nil {
		finalized.Other = make(map[string]interface{})
	}
	for _, key := range []string{
		"billing_source", "billing_preference", "subscription_id", "subscription_pre_consumed",
		"subscription_post_delta", "subscription_plan_id", "subscription_plan_title", "subscription_total",
		"subscription_used", "subscription_remain", "subscription_consumed", "wallet_quota_deducted",
	} {
		delete(finalized.Other, key)
	}
	if facts.BillingSource != "" {
		finalized.Other["billing_source"] = facts.BillingSource
	}
	// Logs describe the effective billing policy used by BillingSession, not
	// the incidental raw user-setting string. Edge snapshots normalize this
	// value before transport; normalizing again at the shared finalization
	// boundary keeps direct and projected logs identical.
	finalized.Other["billing_preference"] = common.NormalizeBillingPreference(facts.BillingPreference)
	if facts.BillingSource != BillingSourceSubscription {
		return finalized, nil
	}
	if facts.SubscriptionID != 0 {
		finalized.Other["subscription_id"] = facts.SubscriptionID
	}
	if facts.SubscriptionPreConsumed > 0 {
		finalized.Other["subscription_pre_consumed"] = facts.SubscriptionPreConsumed
	}
	if facts.SubscriptionPostDelta != 0 {
		finalized.Other["subscription_post_delta"] = facts.SubscriptionPostDelta
	}
	if facts.SubscriptionPlanID != 0 {
		finalized.Other["subscription_plan_id"] = facts.SubscriptionPlanID
	}
	if facts.SubscriptionPlanTitle != "" {
		finalized.Other["subscription_plan_title"] = facts.SubscriptionPlanTitle
	}
	consumed := facts.SubscriptionPreConsumed + facts.SubscriptionPostDelta
	if consumed < 0 {
		consumed = 0
	}
	used := facts.SubscriptionUsed
	if used < 0 {
		used = 0
	}
	if facts.SubscriptionTotal > 0 {
		remain := facts.SubscriptionTotal - used
		if remain < 0 {
			remain = 0
		}
		finalized.Other["subscription_total"] = facts.SubscriptionTotal
		finalized.Other["subscription_used"] = used
		finalized.Other["subscription_remain"] = remain
	}
	if consumed > 0 {
		finalized.Other["subscription_consumed"] = consumed
	}
	finalized.Other["wallet_quota_deducted"] = 0
	return finalized, nil
}

func buildTextConsumeLogSnapshot(
	ctx *gin.Context,
	relayInfo *relaycommon.RelayInfo,
	summary textQuotaSummary,
	originUsage *dto.Usage,
	billingUsage *dto.Usage,
	extraContent []string,
	adminRejectReason string,
	tieredBillingApplied bool,
	tieredResult *billingexpr.TieredResult,
) *dto.EdgeConsumeLogSnapshotV1 {
	logModel := summary.ModelName
	if strings.HasPrefix(logModel, "gpt-4-gizmo") {
		logModel = "gpt-4-gizmo-*"
		extraContent = append(extraContent, "模型 "+summary.ModelName)
	}
	if strings.HasPrefix(logModel, "gpt-4o-gizmo") {
		logModel = "gpt-4o-gizmo-*"
		extraContent = append(extraContent, "模型 "+summary.ModelName)
	}

	var other map[string]interface{}
	if summary.IsClaudeUsageSemantic {
		other = GenerateClaudeOtherInfo(ctx, relayInfo,
			summary.ModelRatio, summary.GroupRatio, summary.CompletionRatio,
			summary.CacheTokens, summary.CacheRatio,
			summary.CacheCreationTokens, summary.CacheCreationRatio,
			summary.CacheCreationTokens5m, summary.CacheCreationRatio5m,
			summary.CacheCreationTokens1h, summary.CacheCreationRatio1h,
			summary.ModelPrice, relayInfo.PriceData.GroupRatioInfo.GroupSpecialRatio)
		other["usage_semantic"] = "anthropic"
	} else {
		other = GenerateTextOtherInfo(ctx, relayInfo, summary.ModelRatio, summary.GroupRatio, summary.CompletionRatio, summary.CacheTokens, summary.CacheRatio, summary.ModelPrice, relayInfo.PriceData.GroupRatioInfo.GroupSpecialRatio)
	}
	appendUsageBillingPathForLog(other, common.GetContextKeyBool(ctx, constant.ContextKeyLocalCountTokens), originUsage)
	if adminRejectReason != "" {
		other["reject_reason"] = adminRejectReason
	}
	if summary.ImageTokens != 0 {
		other["image"] = true
		other["image_ratio"] = summary.ImageRatio
		other["image_output"] = summary.ImageTokens
	}
	appendToolSurchargeLogInfo(other, summary.ToolSurchargeItems)
	if summary.AudioInputPrice > 0 && summary.AudioTokens > 0 {
		other["audio_input_seperate_price"] = true
		other["audio_input_token_count"] = summary.AudioTokens
		other["audio_input_price"] = summary.AudioInputPrice
	}
	if summary.CacheCreationTokens > 0 {
		other["cache_creation_tokens"] = summary.CacheCreationTokens
		other["cache_creation_ratio"] = summary.CacheCreationRatio
	}
	if summary.CacheCreationTokens5m > 0 {
		other["cache_creation_tokens_5m"] = summary.CacheCreationTokens5m
		other["cache_creation_ratio_5m"] = summary.CacheCreationRatio5m
	}
	if summary.CacheCreationTokens1h > 0 {
		other["cache_creation_tokens_1h"] = summary.CacheCreationTokens1h
		other["cache_creation_ratio_1h"] = summary.CacheCreationRatio1h
	}
	if cacheWriteTokens := cacheWriteTokensTotal(summary); cacheWriteTokens > 0 {
		other["cache_write_tokens"] = cacheWriteTokens
	}
	if relayInfo.GetFinalRequestRelayFormat() != types.RelayFormatClaude && billingUsage != nil && billingUsage.UsageSource != "" && billingUsage.InputTokens > 0 {
		other["input_tokens_total"] = billingUsage.InputTokens
	}
	if tieredBillingApplied {
		InjectTieredBillingInfo(other, relayInfo, tieredResult)
	}
	attachQuotaSaturation(ctx, relayInfo, other)

	useTime := summary.UseTimeSeconds
	if useTime < 0 {
		useTime = 0
	}
	if useTime > math.MaxInt32 {
		useTime = math.MaxInt32
	}
	requestID := ctx.GetString(common.RequestIdKey)
	if requestID == "" {
		requestID = relayInfo.RequestId
	}
	ip := ""
	if relayInfo.UserSetting.RecordIpLog {
		ip = ctx.ClientIP()
	}
	return &dto.EdgeConsumeLogSnapshotV1{
		Username:          ctx.GetString("username"),
		TokenName:         summary.TokenName,
		ModelName:         logModel,
		Content:           strings.Join(extraContent, ", "),
		UseTimeSeconds:    &useTime,
		IP:                ip,
		RequestID:         requestID,
		UpstreamRequestID: ctx.GetString(common.UpstreamRequestIdKey),
		Other:             other,
	}
}

func buildEdgeTextSettlementUsage(relayInfo *relaycommon.RelayInfo, originUsage, billingUsage *dto.Usage, summary textQuotaSummary) *dto.BillingUsage {
	if originUsage != nil && originUsage.BillingUsage != nil {
		result := dto.CloneBillingUsage(originUsage.BillingUsage)
		if result.ClaudeUsage != nil {
			// calculateTextQuotaSummary applies the existing OpenRouter/Claude
			// cache separation before settlement. Carry those authoritative token
			// counts in the durable usage fact so master recomputation and the
			// direct path consume exactly the same normalized input.
			result.ClaudeUsage.InputTokens = summary.PromptTokens
			result.ClaudeUsage.OutputTokens = summary.CompletionTokens
		}
		return result
	}
	settlementUsage := billingUsage
	if settlementUsage == nil {
		settlementUsage = &dto.Usage{
			PromptTokens: summary.PromptTokens, CompletionTokens: summary.CompletionTokens, TotalTokens: summary.TotalTokens,
		}
	}
	responses := relayInfo != nil && strings.HasPrefix(relayInfo.RequestURLPath, "/v1/responses")
	var result *dto.BillingUsage
	if responses {
		result = dto.NewOpenAIResponsesBillingUsage(settlementUsage)
	} else {
		result = dto.NewOpenAIChatBillingUsage(settlementUsage)
	}
	if result != nil {
		return result
	}
	source := dto.BillingUsageSourceOAIChat
	if responses {
		source = dto.BillingUsageSourceOAIResponses
	}
	return &dto.BillingUsage{Source: source, Semantic: dto.BillingUsageSemanticOpenAI, OpenAIUsage: &dto.Usage{}}
}
