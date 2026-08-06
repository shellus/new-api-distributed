package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildAndFinalizeTextConsumeLogSnapshotPreservesRichOtherContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:4321"
	ctx.Set("username", "request-user")
	ctx.Set("token_name", "request-token")
	ctx.Set(common.RequestIdKey, "request-visible-1")
	ctx.Set(common.UpstreamRequestIdKey, "upstream-1")
	ctx.Set("use_channel", []string{"31", "32"})
	common.SetContextKey(ctx, constant.ContextKeySystemPromptOverride, true)
	common.SetContextKey(ctx, constant.ContextKeyLocalCountTokens, true)
	ctx.Set(ginKeyChannelAffinityLogInfo, map[string]interface{}{
		"reason": "sticky", "channel_id": 31, "key_fp": "fingerprint",
	})

	start := time.UnixMilli(1_784_145_600_000)
	streamStatus := relaycommon.NewStreamStatus()
	streamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, assert.AnError)
	streamStatus.RecordError("soft stream error")
	relayInfo := &relaycommon.RelayInfo{
		UserId: 7, TokenId: 11, UsingGroup: "default", UserGroup: "vip",
		OriginModelName: "gpt-4o-gizmo-request", IsStream: true,
		StartTime: start, FirstResponseTime: start.Add(250 * time.Millisecond),
		ReasoningEffort: "high", UserSetting: dto.UserSetting{BillingPreference: "subscription_first", RecordIpLog: true},
		RequestConversionChain:  []relaytypes.RelayFormat{relaytypes.RelayFormatClaude, relaytypes.RelayFormatOpenAIResponses},
		FinalRequestRelayFormat: relaytypes.RelayFormatOpenAIResponses,
		ParamOverrideAudit:      []string{"set temperature = 0.5"},
		StreamStatus:            streamStatus,
		PriceData: hosttypes.PriceData{
			ModelRatio: 1.5, CompletionRatio: 2, CacheRatio: 0.1, ModelPrice: 0,
			CacheCreationRatio: 1.25, CacheCreation5mRatio: 1.25, CacheCreation1hRatio: 2,
			GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 0.8, GroupSpecialRatio: 0.8, HasSpecialRatio: true},
		},
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 31, IsModelMapped: true, UpstreamModelName: "claude-upstream"},
		QuotaClamp:  &common.QuotaClamp{Op: "QuotaRound", Kind: common.QuotaClampOverflow, Original: 1e20, Clamped: common.MaxQuota},
	}
	usage := &dto.Usage{
		PromptTokens: 100, CompletionTokens: 20, InputTokens: 115, UsageSource: dto.BillingUsageSourceClaudeMessages,
		PromptTokensDetails:         dto.InputTokenDetails{CachedTokens: 10, CachedCreationTokens: 5},
		ClaudeCacheCreation5mTokens: 2, ClaudeCacheCreation1hTokens: 3,
	}
	summary := textQuotaSummary{
		ModelName: "gpt-4o-gizmo-request", TokenName: "request-token", UseTimeSeconds: 3,
		PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
		CacheTokens: 10, CacheCreationTokens: 5, CacheCreationTokens5m: 2, CacheCreationTokens1h: 3,
		ModelRatio: 1.5, GroupRatio: 0.8, CompletionRatio: 2, CacheRatio: 0.1,
		CacheCreationRatio: 1.25, CacheCreationRatio5m: 1.25, CacheCreationRatio1h: 2,
		UsageSemantic: "anthropic", IsClaudeUsageSemantic: true,
	}

	snapshot := buildTextConsumeLogSnapshot(ctx, relayInfo, summary, usage, usage, []string{"cache hit"}, "policy-note", false, nil)
	require.NotNil(t, snapshot)
	assert.Equal(t, "request-user", snapshot.Username)
	assert.Equal(t, "request-token", snapshot.TokenName)
	assert.Equal(t, "gpt-4o-gizmo-*", snapshot.ModelName)
	assert.Equal(t, "cache hit, 模型 gpt-4o-gizmo-request", snapshot.Content)
	assert.Equal(t, "203.0.113.10", snapshot.IP)
	assert.Equal(t, "request-visible-1", snapshot.RequestID)
	assert.Equal(t, "upstream-1", snapshot.UpstreamRequestID)
	require.NotNil(t, snapshot.UseTimeSeconds)
	assert.Equal(t, int64(3), *snapshot.UseTimeSeconds)

	for _, key := range []string{
		"model_ratio", "group_ratio", "completion_ratio", "cache_tokens", "cache_ratio", "model_price", "user_group_ratio", "frt",
		"reasoning_effort", "is_model_mapped", "upstream_model_name", "is_system_prompt_overwritten", "request_path", "request_conversion",
		"claude", "po", "stream_status", "usage_semantic", "reject_reason", "cache_creation_tokens", "cache_creation_ratio",
		"cache_creation_tokens_5m", "cache_creation_ratio_5m", "cache_creation_tokens_1h", "cache_creation_ratio_1h",
		"cache_write_tokens", "input_tokens_total", "admin_info",
	} {
		assert.Contains(t, snapshot.Other, key)
	}

	facts := TextConsumeLogSettlementFacts{
		BillingSource: "subscription", BillingPreference: "subscription_first",
		SubscriptionID: 21, SubscriptionPreConsumed: 100, SubscriptionPostDelta: 20,
		SubscriptionPlanID: 4, SubscriptionPlanTitle: "Pro",
		SubscriptionTotal: 1_000, SubscriptionUsed: 320,
	}
	localFinal, err := FinalizeTextConsumeLogSnapshot(snapshot, facts)
	require.NoError(t, err)
	edgeObservation, err := dto.CloneEdgeConsumeLogSnapshotV1(snapshot)
	require.NoError(t, err)
	delete(edgeObservation.Other, "frt")
	edgeFinal, err := FinalizeTextConsumeLogSnapshot(edgeObservation, facts)
	require.NoError(t, err)
	edgeFinal.Other["frt"] = snapshot.Other["frt"]

	assert.Equal(t, localFinal, edgeFinal, "the complete snapshot must round-trip without a hand-maintained key list")
	assert.Equal(t, int64(120), localFinal.Other["subscription_consumed"])
	assert.Equal(t, int64(680), localFinal.Other["subscription_remain"])
	assert.Equal(t, 0, localFinal.Other["wallet_quota_deducted"])
}

func TestBuildEdgeTextSettlementUsageKeepsZeroUsageVariant(t *testing.T) {
	summary := textQuotaSummary{}
	relayInfo := &relaycommon.RelayInfo{RequestURLPath: "/v1/responses"}

	usage := buildEdgeTextSettlementUsage(relayInfo, nil, nil, summary)
	require.NotNil(t, usage)
	assert.Equal(t, dto.BillingUsageSourceOAIResponses, usage.Source)
	require.NotNil(t, usage.OpenAIUsage)
	assert.Zero(t, usage.OpenAIUsage.TotalTokens)
}

func TestBuildEdgeTextSettlementUsageCarriesAuthoritativeClaudePromptCount(t *testing.T) {
	originUsage := &dto.Usage{BillingUsage: &dto.BillingUsage{
		Source:   dto.BillingUsageSourceClaudeMessages,
		Semantic: dto.BillingUsageSemanticAnthropic,
		ClaudeUsage: &dto.ClaudeUsage{
			InputTokens:          2_604,
			OutputTokens:         383,
			CacheReadInputTokens: 2_432,
		},
	}}
	summary := textQuotaSummary{PromptTokens: 172, CompletionTokens: 383, TotalTokens: 555}

	usage := buildEdgeTextSettlementUsage(&relaycommon.RelayInfo{}, originUsage, nil, summary)
	require.NotNil(t, usage)
	require.NotNil(t, usage.ClaudeUsage)
	assert.Equal(t, 172, usage.ClaudeUsage.InputTokens)
	assert.Equal(t, 383, usage.ClaudeUsage.OutputTokens)
	assert.Equal(t, 2_432, usage.ClaudeUsage.CacheReadInputTokens)
}

func TestBuildAndFinalizeTextConsumeLogSnapshotFixedWalletAndIPDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx.Request.RemoteAddr = "203.0.113.20:1234"
	ctx.Set("username", "wallet-user")
	ctx.Set("token_name", "wallet-token")
	relayInfo := &relaycommon.RelayInfo{
		RequestId: "wallet-request", OriginModelName: "gpt-fixed", RequestURLPath: "/v1/chat/completions",
		StartTime: time.Now(), FirstResponseTime: time.Now().Add(-time.Second),
		UserSetting: dto.UserSetting{BillingPreference: "wallet_only", RecordIpLog: false},
		PriceData:   hosttypes.PriceData{UsePrice: true, ModelPrice: 0.02, GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 1, GroupSpecialRatio: -1}},
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 31},
	}
	summary := textQuotaSummary{
		ModelName: "gpt-fixed", TokenName: "wallet-token", UseTimeSeconds: 1,
		PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, ModelPrice: 0.02, GroupRatio: 1,
	}

	snapshot := buildTextConsumeLogSnapshot(ctx, relayInfo, summary, &dto.Usage{PromptTokens: 1, CompletionTokens: 1}, nil, nil, "", false, nil)
	require.NotNil(t, snapshot)
	assert.Empty(t, snapshot.IP)
	assert.Equal(t, 0.02, snapshot.Other["model_price"])
	snapshot.Other["subscription_id"] = 99
	finalized, err := FinalizeTextConsumeLogSnapshot(snapshot, TextConsumeLogSettlementFacts{
		BillingSource: "wallet", BillingPreference: "wallet_only",
	})
	require.NoError(t, err)
	assert.Equal(t, "wallet", finalized.Other["billing_source"])
	assert.Equal(t, "wallet_only", finalized.Other["billing_preference"])
	assert.NotContains(t, finalized.Other, "subscription_id")
}

func TestFinalizeTextConsumeLogSnapshotNormalizesEffectiveBillingPreference(t *testing.T) {
	baseSnapshot := &dto.EdgeConsumeLogSnapshotV1{Other: map[string]interface{}{
		"request_path": "/v1/chat/completions",
	}}
	for _, tc := range []struct {
		name       string
		preference string
		expected   string
	}{
		{name: "empty uses effective default", preference: "", expected: "subscription_first"},
		{name: "whitespace uses effective default", preference: "   ", expected: "subscription_first"},
		{name: "invalid uses effective default", preference: "invalid", expected: "subscription_first"},
		{name: "explicit subscription first", preference: "subscription_first", expected: "subscription_first"},
		{name: "explicit subscription only", preference: "subscription_only", expected: "subscription_only"},
		{name: "explicit wallet first", preference: "wallet_first", expected: "wallet_first"},
		{name: "explicit wallet only", preference: "wallet_only", expected: "wallet_only"},
		{name: "explicit wallet only is trimmed", preference: " wallet_only ", expected: "wallet_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			masterFacts := TextConsumeLogSettlementFactsFromRelayInfo(&relaycommon.RelayInfo{
				BillingSource: BillingSourceWallet,
				UserSetting:   dto.UserSetting{BillingPreference: tc.preference},
			})
			edgeFacts := TextConsumeLogSettlementFacts{
				BillingSource:     BillingSourceWallet,
				BillingPreference: common.NormalizeBillingPreference(tc.preference),
			}

			masterFinal, err := FinalizeTextConsumeLogSnapshot(baseSnapshot, masterFacts)
			require.NoError(t, err)
			edgeFinal, err := FinalizeTextConsumeLogSnapshot(baseSnapshot, edgeFacts)
			require.NoError(t, err)

			assert.Equal(t, tc.expected, masterFinal.Other["billing_preference"])
			assert.Equal(t, masterFinal.Other, edgeFinal.Other)
		})
	}
}
