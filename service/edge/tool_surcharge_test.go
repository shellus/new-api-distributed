package edge

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestMasterToolSurchargeQuotaUsesGenericToolFacts(t *testing.T) {
	policy := dto.EdgePricingPolicyV1{ToolPrices: map[string]float64{
		"google_search": 2,
		"custom_fn":     4,
	}}
	facts := dto.EdgeBillingFactsV1{ToolCalls: map[string]int{
		"google_search": 3,
		"custom_fn":     2,
	}}

	quota := masterToolSurchargeQuota(policy, facts, decimal.NewFromFloat(0.5), decimal.NewFromInt(500_000))
	require.True(t, quota.Equal(decimal.NewFromInt(3_500)))
}

func TestMasterToolSurchargeQuotaKeepsLegacyFactsCompatible(t *testing.T) {
	policy := dto.EdgePricingPolicyV1{ToolPrices: map[string]float64{"web_search_preview": 2}}
	facts := dto.EdgeBillingFactsV1{WebSearchPreviewCalls: 3}

	quota := masterToolSurchargeQuota(policy, facts, decimal.NewFromFloat(0.5), decimal.NewFromInt(500_000))
	require.True(t, quota.Equal(decimal.NewFromInt(1_500)))
}
