package edgesettlement

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDigestBlockV1IsDeterministicAndBindsNodeGenerationAndEvents(t *testing.T) {
	request := settlementDigestRequestForTest()
	digest, err := DigestBlockV1("edge.digest", 1, request)
	require.NoError(t, err)
	assert.Len(t, digest, 64)

	repeated, err := DigestBlockV1("edge.digest", 1, request)
	require.NoError(t, err)
	assert.Equal(t, digest, repeated)

	otherNode, err := DigestBlockV1("edge.other", 1, request)
	require.NoError(t, err)
	assert.NotEqual(t, digest, otherNode)
	otherGeneration, err := DigestBlockV1("edge.digest", 2, request)
	require.NoError(t, err)
	assert.NotEqual(t, digest, otherGeneration)

	tampered := request
	tampered.Events = append([]dto.EdgeUsageEventV1(nil), request.Events...)
	tampered.Events[0].Billing.ChargedQuota++
	tampered.Events[0].Billing.ReservedQuota++
	tamperedDigest, err := DigestBlockV1("edge.digest", 1, tampered)
	require.NoError(t, err)
	assert.NotEqual(t, digest, tamperedDigest)
}

func TestDigestBlockV1CanonicalizesAppliedRatioMapOrder(t *testing.T) {
	left := settlementDigestRequestForTest()
	left.Events[0].Billing.AppliedRatios = map[string]float64{"alpha": 2, "beta": 3}
	right := settlementDigestRequestForTest()
	right.Events[0].Billing.AppliedRatios = map[string]float64{"beta": 3, "alpha": 2}

	leftDigest, err := DigestBlockV1("edge.digest", 1, left)
	require.NoError(t, err)
	rightDigest, err := DigestBlockV1("edge.digest", 1, right)
	require.NoError(t, err)
	assert.Equal(t, leftDigest, rightDigest)
}

func TestDigestBlockV1ExcludesFirstResponseTime(t *testing.T) {
	withoutFirstResponse := settlementDigestRequestForTest()
	withoutDigest, err := DigestBlockV1("edge.digest", 1, withoutFirstResponse)
	require.NoError(t, err)

	withFirstResponse := settlementDigestRequestForTest()
	firstResponseAt := withFirstResponse.Events[0].StartedAtUnixMilli + 500
	withFirstResponse.Events[0].FirstResponseAtUnixMilli = &firstResponseAt
	withDigest, err := DigestBlockV1("edge.digest", 1, withFirstResponse)
	require.NoError(t, err)

	assert.Equal(t, withoutDigest, withDigest)
}

func settlementDigestRequestForTest() dto.EdgeSettlementBlockRequestV1 {
	status := 200
	return dto.EdgeSettlementBlockRequestV1{
		Meta:    dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV1, RequestID: "settlement-digest-1"},
		BlockID: "block-1", FirstSequence: 1, LastSequence: 1,
		CreatedAtUnixMilli: 1_784_145_630_000, BlockDigest: strings.Repeat("0", 64),
		Events: []dto.EdgeUsageEventV1{{
			EventID: "event-1", Sequence: 1, ReservationID: "reservation-1", RequestID: "request-1",
			UserID: 1, TokenID: 1, SnapshotID: "snapshot-1", SnapshotRevision: 1,
			PricingRevision: 1, BalanceRevision: 1, FundingSource: "wallet",
			ChannelID: 1, Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1,
			Model: "gpt-test", Group: "default", StartedAtUnixMilli: 1_784_145_600_000,
			FinishedAtUnixMilli: 1_784_145_620_000, Outcome: dto.EdgeUsageOutcomeSuccessV1, HTTPStatus: &status,
			Usage: dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}),
			Billing: dto.EdgeUsageBillingV1{
				PricingPolicyID: "price-1", PricingPolicyVersion: "v1", BillingMode: dto.EdgeBillingModeRatioV1,
				GroupRatio: 1, ReservedQuota: 20, ChargedQuota: 14,
			},
		}},
	}
}
