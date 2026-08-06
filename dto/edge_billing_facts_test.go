package dto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEdgeBillingFactsValidateGenericToolCalls(t *testing.T) {
	facts := EdgeBillingFactsV1{ToolCalls: map[string]int{
		"google_search": 2,
		"custom_fn":     3,
	}}
	require.NoError(t, facts.Validate(EdgeBillingModeRatioV1))

	facts.ToolCalls["custom_fn"] = -1
	require.Error(t, facts.Validate(EdgeBillingModeRatioV1))
}
