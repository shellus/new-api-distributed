package dto

import relaydto "github.com/QuantumNous/new-api/relaykit/dto"

// Billing usage is defined in RelayKit. These aliases keep the distributed
// control-plane protocol source-compatible while its wire schema remains in
// the host application.
type BillingUsage = relaydto.BillingUsage
type Usage = relaydto.Usage
type ClaudeUsage = relaydto.ClaudeUsage
type GeminiUsageMetadata = relaydto.GeminiUsageMetadata
type GeminiPromptTokensDetails = relaydto.GeminiPromptTokensDetails
type InputTokenDetails = relaydto.InputTokenDetails
type OutputTokenDetails = relaydto.OutputTokenDetails
type VertexKeyType = relaydto.VertexKeyType
type AwsKeyType = relaydto.AwsKeyType
type ChannelSettings = relaydto.ChannelSettings
type ChannelOtherSettings = relaydto.ChannelOtherSettings
type AdvancedCustomConfig = relaydto.AdvancedCustomConfig
type AdvancedCustomRoute = relaydto.AdvancedCustomRoute
type UserSetting = relaydto.UserSetting

const (
	BillingUsageSourceClaudeMessages = relaydto.BillingUsageSourceClaudeMessages
	BillingUsageSourceGeminiChat     = relaydto.BillingUsageSourceGeminiChat
	BillingUsageSourceOAIChat        = relaydto.BillingUsageSourceOAIChat
	BillingUsageSourceOAIResponses   = relaydto.BillingUsageSourceOAIResponses
	BillingUsageSemanticAnthropic    = relaydto.BillingUsageSemanticAnthropic
	BillingUsageSemanticGemini       = relaydto.BillingUsageSemanticGemini
	BillingUsageSemanticOpenAI       = relaydto.BillingUsageSemanticOpenAI
)

var (
	CloneBillingUsage              = relaydto.CloneBillingUsage
	NewOpenAIChatBillingUsage      = relaydto.NewOpenAIChatBillingUsage
	NewOpenAIResponsesBillingUsage = relaydto.NewOpenAIResponsesBillingUsage
	NewGeminiChatBillingUsage      = relaydto.NewGeminiChatBillingUsage
)
