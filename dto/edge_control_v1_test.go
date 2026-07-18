package dto

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgetoken"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func edgeTestRequestMetaV1(requestID string) EdgeControlRequestMetaV1 {
	return EdgeControlRequestMetaV1{
		ProtocolVersion: EdgeControlProtocolVersionV1,
		RequestID:       requestID,
	}
}

func edgeTestResponseMetaV1(requestID string) EdgeControlResponseMetaV1 {
	serverRequestID := "server-" + requestID
	if requestID == "" {
		serverRequestID = "server-request"
	}
	return EdgeControlResponseMetaV1{
		ProtocolVersion:     EdgeControlProtocolVersionV1,
		RequestID:           requestID,
		ServerRequestID:     serverRequestID,
		ServerTimeUnixMilli: 1_700_000_000_100,
	}
}

func edgeTestDigestV1(character string) string {
	return strings.Repeat(character, 64)
}

func edgeTestDetachedSignatureV1(digest string) EdgeDetachedContentSignatureV1 {
	return EdgeDetachedContentSignatureV1{
		Algorithm:     edgeauth.Algorithm,
		KeyID:         "snapshot-signing-key-test",
		PayloadDigest: digest,
		Value:         "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQg==",
	}
}

func edgeTestManifestV1() EdgeSnapshotManifestV1 {
	return EdgeSnapshotManifestV1{
		SnapshotID:         "snapshot-test-7",
		Revision:           7,
		CreatedAtUnixMilli: 1_700_000_000_100,
		ExpiresAtUnixMilli: 1_700_003_600_100,
		HashAlgorithm:      "sha256",
		Digest:             edgeTestDigestV1("a"),
		TokenFingerprint: EdgeTokenFingerprintSchemeV1{
			Algorithm: edgetoken.FingerprintAlgorithm,
			Version:   edgetoken.FingerprintVersion,
		},
		Datasets: []EdgeSnapshotDatasetManifestV1{
			{
				Dataset:           EdgeSnapshotDatasetAuthenticationV1,
				Revision:          7,
				ItemCount:         1,
				PageCount:         1,
				Digest:            edgeTestDigestV1("b"),
				DetachedSignature: edgeTestDetachedSignatureV1(edgeTestDigestV1("b")),
			},
			{
				Dataset:           EdgeSnapshotDatasetPricingV1,
				Revision:          4,
				ItemCount:         1,
				PageCount:         1,
				Digest:            edgeTestDigestV1("c"),
				DetachedSignature: edgeTestDetachedSignatureV1(edgeTestDigestV1("c")),
			},
			{
				Dataset:           EdgeSnapshotDatasetRoutingV1,
				Revision:          5,
				ItemCount:         1,
				PageCount:         1,
				Digest:            edgeTestDigestV1("d"),
				DetachedSignature: edgeTestDetachedSignatureV1(edgeTestDigestV1("d")),
			},
		},
	}
}

func edgeTestSnapshotVerificationKeyV1(t *testing.T, keyID string, seed byte) EdgeSnapshotVerificationKeyV1 {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	publicKey, err := edgeauth.EncodePublicKey(privateKey.Public().(ed25519.PublicKey))
	require.NoError(t, err)
	return EdgeSnapshotVerificationKeyV1{
		KeyID:              keyID,
		Algorithm:          edgeauth.Algorithm,
		PublicKey:          publicKey,
		NotBeforeUnixMilli: 1_700_000_000_000,
		ExpiresAtUnixMilli: 1_800_000_000_000,
	}
}

func edgeTestControlConfigV1(t *testing.T) EdgeNodeControlConfigV1 {
	t.Helper()
	return EdgeNodeControlConfigV1{
		NodeID:                      "node-test-1",
		NodeGeneration:              3,
		Enabled:                     true,
		HeartbeatIntervalSeconds:    15,
		SnapshotPollIntervalSeconds: 30,
		SnapshotPageLimit:           500,
		SettlementMaxEvents:         100,
		SettlementMaxDelaySeconds:   5,
		ClockSkewToleranceSeconds:   60,
		SnapshotVerificationKeys: []EdgeSnapshotVerificationKeyV1{
			edgeTestSnapshotVerificationKeyV1(t, "snapshot-key-1", 0x42),
		},
	}
}

func edgeTestSettlementAckV1() EdgeSettlementAckV1 {
	return EdgeSettlementAckV1{
		Status:                  EdgeSettlementAckAcceptedV1,
		NodeID:                  "node-test-1",
		NodeGeneration:          3,
		BlockID:                 "block-test-9",
		AckedThroughSequence:    10,
		NextExpectedSequence:    11,
		AcceptedEventCount:      2,
		AcknowledgedAtUnixMilli: 1_700_000_001_000,
	}
}

func TestEdgeControlV1ContractsRoundTripWithCommonJSON(t *testing.T) {
	expiresAt := int64(1_700_003_600_000)
	httpStatus := 200
	modelRatio := 1.25
	completionRatio := 2.0
	cacheReadRatio := 0.1
	cacheCreationRatio := 1.25
	cacheCreation1hRatio := 2.0
	retryAfter := int64(3)
	expectedGeneration := int64(4)
	expectedRevision := int64(8)
	expectedSequence := int64(10)

	declaration := EdgeNodeDeclarationV1{
		Name:               "edge-test",
		Region:             "test-region",
		PublicURL:          "https://edge.invalid",
		SoftwareVersion:    "v-test",
		StartedAtUnixMilli: 1_700_000_000_000,
		Capabilities: []EdgeEndpointCapabilityV1{
			{Endpoint: EdgeEndpointOpenAIChatCompletionsV1, Streaming: true},
			{Endpoint: EdgeEndpointOpenAIResponsesV1, Streaming: true},
		},
	}
	snapshotState := EdgeSnapshotStateV1{
		SnapshotID:         "snapshot-test-7",
		Revision:           7,
		AppliedAtUnixMilli: 1_700_000_000_500,
		Datasets: []EdgeSnapshotDatasetStateV1{
			{Dataset: EdgeSnapshotDatasetAuthenticationV1, Revision: 7},
			{Dataset: EdgeSnapshotDatasetPricingV1, Revision: 4},
		},
	}
	settlementState := EdgeSettlementStateV1{
		LastAckedSequence:      8,
		LastAckedBlockID:       "block-test-8",
		NextEventSequence:      10,
		PendingEventCount:      1,
		PendingBlockCount:      1,
		OldestPendingUnixMilli: 1_700_000_000_600,
	}
	manifest := edgeTestManifestV1()
	ack := edgeTestSettlementAckV1()

	pageResponses := []EdgeSnapshotPageResponseV1{
		{
			Meta:       edgeTestResponseMetaV1("request-page-authentication"),
			SnapshotID: manifest.SnapshotID,
			Dataset:    EdgeSnapshotDatasetAuthenticationV1,
			Revision:   7,
			NextCursor: "cursor-authentication-2",
			ItemCount:  1,
			Digest:     edgeTestDigestV1("d"),
			Payload: EdgeSnapshotPagePayloadV1{
				Authentication: []EdgeTokenAuthRecordV1{
					{
						TokenFingerprint:   edgeTestDigestV1("f"),
						TokenID:            11,
						UserID:             21,
						Enabled:            true,
						ExpiresAtUnixMilli: &expiresAt,
						Group:              "default",
						ModelLimitEnabled:  true,
						AllowedModels:      []string{"gpt-text-test"},
						AllowedCIDRs:       []string{"192.0.2.0/24"},
						CrossGroupRetry:    true,
					},
				},
			},
		},
		{
			Meta:       edgeTestResponseMetaV1("request-page-users"),
			SnapshotID: manifest.SnapshotID,
			Dataset:    EdgeSnapshotDatasetUsersV1,
			Revision:   7,
			ItemCount:  1,
			Digest:     edgeTestDigestV1("e"),
			Payload: EdgeSnapshotPagePayloadV1{
				Users: []EdgeUserPolicyV1{
					{
						UserID:       21,
						Enabled:      true,
						Username:     "edge-user-test",
						Email:        "edge-user@example.invalid",
						DefaultGroup: "default",
						Setting: EdgeUserSettingV1{
							AcceptUnsetRatioModel: false,
							Language:              "en",
							BillingPreference:     "subscription_first",
						},
					},
				},
			},
		},
		{
			Meta:       edgeTestResponseMetaV1("request-page-groups"),
			SnapshotID: manifest.SnapshotID,
			Dataset:    EdgeSnapshotDatasetGroupsV1,
			Revision:   6,
			ItemCount:  1,
			Digest:     edgeTestDigestV1("f"),
			Payload: EdgeSnapshotPagePayloadV1{
				Groups: []EdgeGroupPolicyV1{
					{
						UserGroup: "default",
						UsingGroups: []EdgeUsingGroupPolicyV1{
							{Group: "default", Enabled: true, Ratio: 1.0},
						},
					},
				},
			},
		},
		{
			Meta:       edgeTestResponseMetaV1("request-page-models"),
			SnapshotID: manifest.SnapshotID,
			Dataset:    EdgeSnapshotDatasetModelsV1,
			Revision:   5,
			ItemCount:  1,
			Digest:     edgeTestDigestV1("a"),
			Payload: EdgeSnapshotPagePayloadV1{
				Models: []EdgeModelPolicyV1{
					{
						Model:      "gpt-text-test",
						Enabled:    true,
						Endpoints:  []EdgeEndpointV1{EdgeEndpointOpenAIChatCompletionsV1, EdgeEndpointOpenAIResponsesV1},
						Streaming:  true,
						ChannelIDs: []int64{31},
					},
				},
			},
		},
		{
			Meta:       edgeTestResponseMetaV1("request-page-channels"),
			SnapshotID: manifest.SnapshotID,
			Dataset:    EdgeSnapshotDatasetChannelsV1,
			Revision:   3,
			ItemCount:  1,
			Digest:     edgeTestDigestV1("b"),
			Payload: EdgeSnapshotPagePayloadV1{
				Channels: []EdgeChannelProjectionV1{
					{
						ChannelID:         31,
						Type:              99,
						Name:              "text-channel-test",
						Enabled:           true,
						Groups:            []string{"default"},
						Models:            []string{"gpt-text-test"},
						ModelMapping:      map[string]string{"gpt-text-test": "upstream-text-test"},
						Priority:          10,
						Weight:            20,
						LocalService:      EdgeLocalServiceCPAPro20x4V1,
						StatusCodeMapping: map[string]int{"429": 503},
						TextPolicy: EdgeTextRequestPolicyV1{
							ForceFormat:             true,
							ThinkingToContent:       true,
							PassThroughBodyEnabled:  false,
							SystemPrompt:            "test system prompt",
							SystemPromptOverride:    false,
							AllowServiceTier:        true,
							AllowInferenceGeo:       false,
							AllowSpeed:              true,
							DisableStore:            true,
							AllowSafetyIdentifier:   false,
							AllowIncludeObfuscation: false,
						},
					},
				},
			},
		},
		{
			Meta:       edgeTestResponseMetaV1("request-page-pricing"),
			SnapshotID: manifest.SnapshotID,
			Dataset:    EdgeSnapshotDatasetPricingV1,
			Revision:   4,
			ItemCount:  1,
			Digest:     edgeTestDigestV1("c"),
			Payload: EdgeSnapshotPagePayloadV1{
				Pricing: []EdgePricingPolicyV1{
					{
						PolicyID:                 "pricing-test-1",
						Version:                  "pricing-version-test-4",
						Model:                    "gpt-text-test",
						BillingMode:              EdgeBillingModeTieredExprV1,
						ModelRatio:               &modelRatio,
						CompletionRatio:          &completionRatio,
						CacheReadRatio:           &cacheReadRatio,
						CacheCreationRatio:       &cacheCreationRatio,
						CacheCreation1hRatio:     &cacheCreation1hRatio,
						BillingExpression:        `v1:tier("base", p * 1.25 + c * 2)`,
						BillingExpressionHash:    edgeTestDigestV1("d"),
						BillingExpressionVersion: 1,
						QuotaPerUnit:             500_000,
					},
				},
			},
		},
		{
			Meta:       edgeTestResponseMetaV1("request-page-routing"),
			SnapshotID: manifest.SnapshotID,
			Dataset:    EdgeSnapshotDatasetRoutingV1,
			Revision:   5,
			ItemCount:  1,
			Digest:     edgeTestDigestV1("d"),
			Payload: EdgeSnapshotPagePayloadV1{
				Routing: []EdgeRoutingPolicyV1{edgeValidRoutingPolicyV1()},
			},
		},
	}

	billingUsage := NewOpenAIResponsesBillingUsage(&Usage{
		PromptTokens:     120,
		CompletionTokens: 30,
		TotalTokens:      150,
		PromptTokensDetails: InputTokenDetails{
			CachedTokens: 20,
			TextTokens:   120,
		},
		CompletionTokenDetails: OutputTokenDetails{
			TextTokens:      30,
			ReasoningTokens: 5,
		},
	})
	require.NotNil(t, billingUsage)
	chatBillingUsage := NewOpenAIChatBillingUsage(&Usage{
		PromptTokens:     40,
		CompletionTokens: 10,
		TotalTokens:      50,
	})
	require.NotNil(t, chatBillingUsage)

	usageEvent := EdgeUsageEventV1{
		EventID:             "event-test-9",
		Sequence:            9,
		ReservationID:       "reservation-test-9",
		RequestID:           "relay-request-test-9",
		UserID:              21,
		TokenID:             11,
		SnapshotID:          manifest.SnapshotID,
		SnapshotRevision:    manifest.Revision,
		PricingRevision:     4,
		BalanceRevision:     2,
		FundingSource:       "wallet",
		ChannelID:           31,
		Endpoint:            EdgeEndpointOpenAIResponsesV1,
		Streaming:           true,
		Model:               "gpt-text-test",
		Group:               "default",
		StartedAtUnixMilli:  1_700_000_000_600,
		FinishedAtUnixMilli: 1_700_000_000_900,
		Outcome:             EdgeUsageOutcomeSuccessV1,
		HTTPStatus:          &httpStatus,
		Usage:               billingUsage,
		Billing: EdgeUsageBillingV1{
			PricingPolicyID:       "pricing-test-1",
			PricingPolicyVersion:  "pricing-version-test-4",
			BillingMode:           EdgeBillingModeTieredExprV1,
			GroupRatio:            1.0,
			AppliedRatios:         map[string]float64{"speed": 2.0},
			BillingExpressionHash: edgeTestDigestV1("d"),
			MatchedTier:           "base",
			ReservedQuota:         100,
			ChargedQuota:          80,
		},
	}
	chatUsageEvent := usageEvent
	chatUsageEvent.EventID = "event-test-10"
	chatUsageEvent.Sequence = 10
	chatUsageEvent.ReservationID = "reservation-test-10"
	chatUsageEvent.RequestID = "relay-request-test-10"
	chatUsageEvent.Endpoint = EdgeEndpointOpenAIChatCompletionsV1
	chatUsageEvent.Streaming = false
	chatUsageEvent.Usage = chatBillingUsage
	chatUsageEvent.Billing.ReservedQuota = 30
	chatUsageEvent.Billing.ChargedQuota = 20

	cases := []struct {
		name   string
		input  any
		output any
	}{
		{
			name: "bootstrap request",
			input: &EdgeBootstrapRequestV1{
				Meta:                      edgeTestRequestMetaV1("request-bootstrap"),
				SupportedProtocolVersions: []string{EdgeControlProtocolVersionV1},
				Declaration:               declaration,
				Snapshot:                  snapshotState,
				Settlement:                settlementState,
			},
			output: &EdgeBootstrapRequestV1{},
		},
		{
			name: "bootstrap response",
			input: &EdgeBootstrapResponseV1{
				Meta:          edgeTestResponseMetaV1("request-bootstrap"),
				Control:       edgeTestControlConfigV1(t),
				Snapshot:      manifest,
				SettlementAck: &ack,
			},
			output: &EdgeBootstrapResponseV1{},
		},
		{
			name: "snapshot manifest request",
			input: &EdgeSnapshotManifestRequestV1{
				Meta:    edgeTestRequestMetaV1("request-manifest"),
				Current: snapshotState,
			},
			output: &EdgeSnapshotManifestRequestV1{},
		},
		{
			name: "snapshot manifest response",
			input: &EdgeSnapshotManifestResponseV1{
				Meta:     edgeTestResponseMetaV1("request-manifest"),
				Changed:  true,
				Snapshot: &manifest,
			},
			output: &EdgeSnapshotManifestResponseV1{},
		},
		{
			name: "snapshot page request",
			input: &EdgeSnapshotPageRequestV1{
				Meta:       edgeTestRequestMetaV1("request-page"),
				SnapshotID: manifest.SnapshotID,
				Dataset:    EdgeSnapshotDatasetAuthenticationV1,
				Cursor:     "cursor-authentication-1",
				Limit:      500,
			},
			output: &EdgeSnapshotPageRequestV1{},
		},
		{
			name:   "authentication snapshot page",
			input:  &pageResponses[0],
			output: &EdgeSnapshotPageResponseV1{},
		},
		{
			name:   "user snapshot page",
			input:  &pageResponses[1],
			output: &EdgeSnapshotPageResponseV1{},
		},
		{
			name:   "group snapshot page",
			input:  &pageResponses[2],
			output: &EdgeSnapshotPageResponseV1{},
		},
		{
			name:   "model snapshot page",
			input:  &pageResponses[3],
			output: &EdgeSnapshotPageResponseV1{},
		},
		{
			name:   "channel snapshot page",
			input:  &pageResponses[4],
			output: &EdgeSnapshotPageResponseV1{},
		},
		{
			name:   "pricing snapshot page",
			input:  &pageResponses[5],
			output: &EdgeSnapshotPageResponseV1{},
		},
		{
			name:   "routing snapshot page",
			input:  &pageResponses[6],
			output: &EdgeSnapshotPageResponseV1{},
		},
		{
			name: "settlement block request",
			input: &EdgeSettlementBlockRequestV1{
				Meta:                edgeTestRequestMetaV1("request-settlement"),
				BlockID:             "block-test-9",
				PreviousBlockID:     "block-test-8",
				PreviousBlockDigest: edgeTestDigestV1("e"),
				FirstSequence:       9,
				LastSequence:        10,
				CreatedAtUnixMilli:  1_700_000_001_000,
				BlockDigest:         edgeTestDigestV1("f"),
				Events:              []EdgeUsageEventV1{usageEvent, chatUsageEvent},
			},
			output: &EdgeSettlementBlockRequestV1{},
		},
		{
			name: "settlement block response",
			input: &EdgeSettlementBlockResponseV1{
				Meta: edgeTestResponseMetaV1("request-settlement"),
				Ack:  ack,
			},
			output: &EdgeSettlementBlockResponseV1{},
		},
		{
			name: "heartbeat request",
			input: &EdgeHeartbeatRequestV1{
				Meta:        edgeTestRequestMetaV1("request-heartbeat"),
				Declaration: declaration,
				Snapshot:    snapshotState,
				Settlement:  settlementState,
				Runtime: EdgeRuntimeStatusV1{
					UptimeSeconds:      600,
					InFlightRequests:   2,
					RecentRequestCount: 40,
					RecentErrorCount:   1,
					Draining:           false,
				},
				CPA: []EdgeCPAStatusV1{
					{
						LocalService:        EdgeLocalServiceCPAPro20x4V1,
						Healthy:             true,
						LatencyMilliseconds: 12,
						AvailableModels:     []string{"gpt-text-test"},
						CheckedAtUnixMilli:  1_700_000_001_000,
					},
				},
			},
			output: &EdgeHeartbeatRequestV1{},
		},
		{
			name: "heartbeat response",
			input: &EdgeHeartbeatResponseV1{
				Meta:          edgeTestResponseMetaV1("request-heartbeat"),
				Control:       edgeTestControlConfigV1(t),
				Snapshot:      &manifest,
				SettlementAck: &ack,
			},
			output: &EdgeHeartbeatResponseV1{},
		},
		{
			name: "unified error response",
			input: &EdgeControlErrorResponseV1{
				Meta: edgeTestResponseMetaV1("request-settlement"),
				Error: EdgeControlErrorV1{
					Code:              EdgeControlErrorCodeSettlementOutOfOrderV1,
					Message:           "settlement sequence is out of order",
					Retryable:         true,
					RetryAfterSeconds: &retryAfter,
					Expected: &EdgeControlExpectedStateV1{
						ProtocolVersions:       []string{EdgeControlProtocolVersionV1},
						NodeGeneration:         &expectedGeneration,
						SnapshotID:             "snapshot-test-8",
						SnapshotRevision:       &expectedRevision,
						NextSettlementSequence: &expectedSequence,
					},
				},
			},
			output: &EdgeControlErrorResponseV1{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := common.Marshal(tc.input)
			require.NoError(t, err)
			require.NoError(t, common.Unmarshal(encoded, tc.output))
			assert.Equal(t, tc.input, tc.output)
		})
	}
}

func TestEdgeControlV1SchemaExcludesSecretsAndTransportAuthFields(t *testing.T) {
	roots := []reflect.Type{
		reflect.TypeOf(EdgeBootstrapRequestV1{}),
		reflect.TypeOf(EdgeBootstrapResponseV1{}),
		reflect.TypeOf(EdgeSnapshotManifestRequestV1{}),
		reflect.TypeOf(EdgeSnapshotManifestResponseV1{}),
		reflect.TypeOf(EdgeSnapshotPageRequestV1{}),
		reflect.TypeOf(EdgeSnapshotPageResponseV1{}),
		reflect.TypeOf(EdgeSettlementBlockRequestV1{}),
		reflect.TypeOf(EdgeSettlementBlockResponseV1{}),
		reflect.TypeOf(EdgeHeartbeatRequestV1{}),
		reflect.TypeOf(EdgeHeartbeatResponseV1{}),
		reflect.TypeOf(EdgeControlErrorResponseV1{}),
	}

	jsonFields := make(map[string]struct{})
	visited := make(map[reflect.Type]struct{})
	var visit func(reflect.Type)
	visit = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
			typ = typ.Elem()
		}
		if typ.Kind() == reflect.Map {
			visit(typ.Elem())
			return
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		if _, ok := visited[typ]; ok {
			return
		}
		visited[typ] = struct{}{}

		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
			if jsonName != "" && jsonName != "-" {
				jsonFields[jsonName] = struct{}{}
			}
			visit(field.Type)
		}
	}
	for _, root := range roots {
		visit(root)
	}

	forbiddenFields := []string{
		"token",
		"api_key",
		"api_token",
		"base_url",
		"authorization",
		"password",
		"private_key",
		"access_token",
		"refresh_token",
		"client_secret",
		"oauth_client_secret",
		"webhook_secret",
		"payment_secret",
		"payment_key",
		"oauth_secret",
		"smtp_password",
		"smtp_secret",
		"proxy",
		"header_override",
		"param_override",
		"credentials",
		"auth",
		"signature",
		"idempotency_key",
		"nonce",
		"sent_at_unix_milli",
		"role",
		"unused_quota",
	}
	for _, field := range forbiddenFields {
		assert.NotContains(t, jsonFields, field)
	}

	assert.Contains(t, jsonFields, "token_fingerprint")
	assert.Contains(t, jsonFields, "local_service")
	assert.Contains(t, jsonFields, "request_id")
	assert.Contains(t, jsonFields, "server_request_id")
	assert.Contains(t, jsonFields, "snapshot_verification_keys")
	assert.Contains(t, jsonFields, "public_key")
	assert.Contains(t, jsonFields, "username")
	assert.Contains(t, jsonFields, "detached_signature")
	assert.Contains(t, jsonFields, "payload_digest")
	assert.Contains(t, jsonFields, "routing")
	assert.Contains(t, jsonFields, "channel_affinity")
	assert.Contains(t, jsonFields, "key")
	assert.Contains(t, jsonFields, "path")
	assert.Contains(t, jsonFields, "pass_headers")
	assert.Contains(t, jsonFields, "keep_origin")
	assert.Contains(t, jsonFields, "skip_retry")
}

func TestEdgeControlV1ErrorCodesAreUnique(t *testing.T) {
	codes := []EdgeControlErrorCodeV1{
		EdgeControlErrorCodeInvalidRequestV1,
		EdgeControlErrorCodeUnsupportedProtocolV1,
		EdgeControlErrorCodeAuthenticationFailedV1,
		EdgeControlErrorCodeInvalidSignatureV1,
		EdgeControlErrorCodeReplayDetectedV1,
		EdgeControlErrorCodeNodeDisabledV1,
		EdgeControlErrorCodeIdempotencyConflictV1,
		EdgeControlErrorCodeSnapshotNotFoundV1,
		EdgeControlErrorCodeSnapshotCursorStaleV1,
		EdgeControlErrorCodeSettlementOutOfOrderV1,
		EdgeControlErrorCodeSettlementConflictV1,
		EdgeControlErrorCodeRateLimitedV1,
		EdgeControlErrorCodeTemporarilyUnavailableV1,
		EdgeControlErrorCodeInternalV1,
	}

	seen := make(map[EdgeControlErrorCodeV1]struct{}, len(codes))
	for _, code := range codes {
		assert.NotEmpty(t, code)
		_, duplicate := seen[code]
		assert.False(t, duplicate, "duplicate edge control error code: %s", code)
		seen[code] = struct{}{}
	}
	assert.Len(t, seen, len(codes))
}

func TestEdgeControlV1InitialEndpointBoundary(t *testing.T) {
	capabilities := []EdgeEndpointCapabilityV1{
		{Endpoint: EdgeEndpointOpenAIChatCompletionsV1, Streaming: true},
		{Endpoint: EdgeEndpointOpenAIResponsesV1, Streaming: true},
	}

	encoded, err := common.Marshal(capabilities)
	require.NoError(t, err)
	assert.JSONEq(t, `[
		{"endpoint":"openai_chat_completions","streaming":true},
		{"endpoint":"openai_responses","streaming":true}
	]`, string(encoded))
}

func TestEdgeControlV1IdentifierLimit(t *testing.T) {
	assert.Equal(t, 64, EdgeControlMaxIdentifierLengthV1)
	assert.Equal(t, 7, EdgeControlMaxSnapshotDatasetsV1)
}

func edgeValidDeclarationForValidationV1() EdgeNodeDeclarationV1 {
	return EdgeNodeDeclarationV1{
		Name:               "edge shanghai 1",
		Region:             "cn-east-1",
		PublicURL:          "https://edge.example.invalid/api/prefix",
		SoftwareVersion:    "v1.2.3-test",
		StartedAtUnixMilli: 1_700_000_000_000,
		Capabilities: []EdgeEndpointCapabilityV1{
			{Endpoint: EdgeEndpointOpenAIChatCompletionsV1, Streaming: true},
			{Endpoint: EdgeEndpointOpenAIResponsesV1, Streaming: true},
		},
	}
}

func edgeValidSnapshotStateForValidationV1() EdgeSnapshotStateV1 {
	return EdgeSnapshotStateV1{
		SnapshotID:         "snapshot-7",
		Revision:           7,
		AppliedAtUnixMilli: 1_700_000_000_100,
		Datasets: []EdgeSnapshotDatasetStateV1{
			{Dataset: EdgeSnapshotDatasetAuthenticationV1, Revision: 7},
			{Dataset: EdgeSnapshotDatasetModelsV1, Revision: 6},
		},
	}
}

func edgeValidSettlementStateForValidationV1() EdgeSettlementStateV1 {
	return EdgeSettlementStateV1{
		LastAckedSequence:      8,
		LastAckedBlockID:       "block-8",
		NextEventSequence:      10,
		PendingEventCount:      1,
		PendingBlockCount:      1,
		OldestPendingUnixMilli: 1_700_000_000_200,
	}
}

func edgeValidBootstrapRequestForValidationV1() EdgeBootstrapRequestV1 {
	return EdgeBootstrapRequestV1{
		Meta:                      edgeTestRequestMetaV1("request-bootstrap-1"),
		SupportedProtocolVersions: []string{EdgeControlProtocolVersionV1},
		Declaration:               edgeValidDeclarationForValidationV1(),
		Snapshot:                  edgeValidSnapshotStateForValidationV1(),
		Settlement:                edgeValidSettlementStateForValidationV1(),
	}
}

func edgeValidCPAStatusForValidationV1() EdgeCPAStatusV1 {
	return EdgeCPAStatusV1{
		LocalService:        EdgeLocalServiceCPAPro20x4V1,
		Healthy:             true,
		LatencyMilliseconds: 12,
		AvailableModels:     []string{"gpt-5.1", "claude-sonnet-4"},
		CheckedAtUnixMilli:  1_700_000_000_300,
	}
}

func edgeValidHeartbeatRequestForValidationV1() EdgeHeartbeatRequestV1 {
	return EdgeHeartbeatRequestV1{
		Meta:        edgeTestRequestMetaV1("request-heartbeat-1"),
		Declaration: edgeValidDeclarationForValidationV1(),
		Snapshot:    edgeValidSnapshotStateForValidationV1(),
		Settlement:  edgeValidSettlementStateForValidationV1(),
		Runtime: EdgeRuntimeStatusV1{
			UptimeSeconds:      60,
			InFlightRequests:   1,
			RecentRequestCount: 10,
			RecentErrorCount:   1,
		},
		CPA: []EdgeCPAStatusV1{edgeValidCPAStatusForValidationV1()},
	}
}

func TestEdgeControlRequestMetaV1Validate(t *testing.T) {
	require.NoError(t, edgeTestRequestMetaV1("request.a_b:c-1").Validate())

	cases := []struct {
		name string
		meta EdgeControlRequestMetaV1
	}{
		{name: "wrong protocol", meta: EdgeControlRequestMetaV1{ProtocolVersion: "edge-control.v3", RequestID: "request-1"}},
		{name: "empty request id", meta: edgeTestRequestMetaV1("")},
		{name: "uppercase request id", meta: edgeTestRequestMetaV1("Request-1")},
		{name: "non ascii request id", meta: edgeTestRequestMetaV1("请求-1")},
		{name: "slash request id", meta: edgeTestRequestMetaV1("request/1")},
		{name: "request id too long", meta: edgeTestRequestMetaV1(strings.Repeat("a", EdgeControlMaxIdentifierLengthV1+1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Error(t, tc.meta.Validate())
		})
	}
}

func TestEdgeNodeDeclarationV1Validate(t *testing.T) {
	require.NoError(t, edgeValidDeclarationForValidationV1().Validate())

	cases := []struct {
		name   string
		mutate func(*EdgeNodeDeclarationV1)
	}{
		{name: "empty name", mutate: func(value *EdgeNodeDeclarationV1) { value.Name = "" }},
		{name: "surrounding name whitespace", mutate: func(value *EdgeNodeDeclarationV1) { value.Name = " edge" }},
		{name: "uppercase region", mutate: func(value *EdgeNodeDeclarationV1) { value.Region = "CN-East" }},
		{name: "relative public url", mutate: func(value *EdgeNodeDeclarationV1) { value.PublicURL = "/edge" }},
		{name: "unsupported public url scheme", mutate: func(value *EdgeNodeDeclarationV1) { value.PublicURL = "ftp://edge.example.invalid" }},
		{name: "public url userinfo", mutate: func(value *EdgeNodeDeclarationV1) { value.PublicURL = "https://user@edge.example.invalid/prefix" }},
		{name: "public url query", mutate: func(value *EdgeNodeDeclarationV1) { value.PublicURL = "https://edge.example.invalid/prefix?a=1" }},
		{name: "empty public url query", mutate: func(value *EdgeNodeDeclarationV1) { value.PublicURL = "https://edge.example.invalid/prefix?" }},
		{name: "public url fragment", mutate: func(value *EdgeNodeDeclarationV1) { value.PublicURL = "https://edge.example.invalid/prefix#status" }},
		{name: "public url too long", mutate: func(value *EdgeNodeDeclarationV1) {
			value.PublicURL = "https://" + strings.Repeat("a", EdgeControlMaxPublicURLLengthV1)
		}},
		{name: "zero started timestamp", mutate: func(value *EdgeNodeDeclarationV1) { value.StartedAtUnixMilli = 0 }},
		{name: "started timestamp before year 2000", mutate: func(value *EdgeNodeDeclarationV1) { value.StartedAtUnixMilli = edgeControlMinUnixMilliV1 - 1 }},
		{name: "timestamp beyond year 9999", mutate: func(value *EdgeNodeDeclarationV1) { value.StartedAtUnixMilli = edgeControlMaxUnixMilliV1 + 1 }},
		{name: "empty capabilities", mutate: func(value *EdgeNodeDeclarationV1) { value.Capabilities = nil }},
		{name: "unknown capability", mutate: func(value *EdgeNodeDeclarationV1) { value.Capabilities[0].Endpoint = "openai_images" }},
		{name: "duplicate capability", mutate: func(value *EdgeNodeDeclarationV1) { value.Capabilities[1] = value.Capabilities[0] }},
		{name: "too many capabilities", mutate: func(value *EdgeNodeDeclarationV1) {
			value.Capabilities = make([]EdgeEndpointCapabilityV1, EdgeControlMaxCapabilitiesV1+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeValidDeclarationForValidationV1()
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func TestEdgeSnapshotAndSettlementStateV1Validate(t *testing.T) {
	require.NoError(t, (EdgeSnapshotStateV1{}).Validate())
	require.NoError(t, edgeValidSnapshotStateForValidationV1().Validate())
	require.NoError(t, (EdgeSettlementStateV1{}).Validate())
	require.NoError(t, edgeValidSettlementStateForValidationV1().Validate())

	snapshotCases := []struct {
		name   string
		mutate func(*EdgeSnapshotStateV1)
	}{
		{name: "uppercase snapshot id", mutate: func(value *EdgeSnapshotStateV1) { value.SnapshotID = "Snapshot-7" }},
		{name: "negative revision", mutate: func(value *EdgeSnapshotStateV1) { value.Revision = -1 }},
		{name: "negative applied timestamp", mutate: func(value *EdgeSnapshotStateV1) { value.AppliedAtUnixMilli = -1 }},
		{name: "unknown dataset", mutate: func(value *EdgeSnapshotStateV1) { value.Datasets[0].Dataset = "options" }},
		{name: "negative dataset revision", mutate: func(value *EdgeSnapshotStateV1) { value.Datasets[0].Revision = -1 }},
		{name: "duplicate dataset", mutate: func(value *EdgeSnapshotStateV1) { value.Datasets[1] = value.Datasets[0] }},
		{name: "too many datasets", mutate: func(value *EdgeSnapshotStateV1) {
			value.Datasets = make([]EdgeSnapshotDatasetStateV1, EdgeControlMaxSnapshotDatasetsV1+1)
		}},
	}
	for _, tc := range snapshotCases {
		t.Run("snapshot "+tc.name, func(t *testing.T) {
			value := edgeValidSnapshotStateForValidationV1()
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}

	settlementCases := []struct {
		name   string
		mutate func(*EdgeSettlementStateV1)
	}{
		{name: "uppercase block id", mutate: func(value *EdgeSettlementStateV1) { value.LastAckedBlockID = "Block-8" }},
		{name: "negative acked sequence", mutate: func(value *EdgeSettlementStateV1) { value.LastAckedSequence = -1 }},
		{name: "negative next event sequence", mutate: func(value *EdgeSettlementStateV1) { value.NextEventSequence = -1 }},
		{name: "negative pending event count", mutate: func(value *EdgeSettlementStateV1) { value.PendingEventCount = -1 }},
		{name: "negative pending block count", mutate: func(value *EdgeSettlementStateV1) { value.PendingBlockCount = -1 }},
		{name: "negative oldest pending timestamp", mutate: func(value *EdgeSettlementStateV1) { value.OldestPendingUnixMilli = -1 }},
	}
	for _, tc := range settlementCases {
		t.Run("settlement "+tc.name, func(t *testing.T) {
			value := edgeValidSettlementStateForValidationV1()
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func TestEdgeBootstrapRequestV1Validate(t *testing.T) {
	require.NoError(t, edgeValidBootstrapRequestForValidationV1().Validate())

	cases := []struct {
		name   string
		mutate func(*EdgeBootstrapRequestV1)
	}{
		{name: "invalid meta", mutate: func(value *EdgeBootstrapRequestV1) { value.Meta.ProtocolVersion = "v1" }},
		{name: "empty supported versions", mutate: func(value *EdgeBootstrapRequestV1) { value.SupportedProtocolVersions = nil }},
		{name: "missing v1", mutate: func(value *EdgeBootstrapRequestV1) { value.SupportedProtocolVersions = []string{"edge-control.v2"} }},
		{name: "duplicate supported version", mutate: func(value *EdgeBootstrapRequestV1) {
			value.SupportedProtocolVersions = []string{EdgeControlProtocolVersionV1, EdgeControlProtocolVersionV1}
		}},
		{name: "noncanonical supported version", mutate: func(value *EdgeBootstrapRequestV1) {
			value.SupportedProtocolVersions = []string{EdgeControlProtocolVersionV1, "Edge-Control.v2"}
		}},
		{name: "too many supported versions", mutate: func(value *EdgeBootstrapRequestV1) {
			value.SupportedProtocolVersions = make([]string, EdgeControlMaxSupportedProtocolVersionsV1+1)
		}},
		{name: "invalid declaration", mutate: func(value *EdgeBootstrapRequestV1) { value.Declaration.PublicURL = "http://user@edge.invalid" }},
		{name: "invalid snapshot", mutate: func(value *EdgeBootstrapRequestV1) { value.Snapshot.Revision = -1 }},
		{name: "invalid settlement", mutate: func(value *EdgeBootstrapRequestV1) { value.Settlement.PendingBlockCount = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeValidBootstrapRequestForValidationV1()
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func TestEdgeCPAStatusV1Validate(t *testing.T) {
	require.NoError(t, edgeValidCPAStatusForValidationV1().Validate())
	for _, service := range []EdgeLocalServiceV1{
		EdgeLocalServiceCPAVIPV1,
		EdgeLocalServiceCPAPro20x4V1,
		EdgeLocalServiceCPAPro20x5V1,
		EdgeLocalServiceCPAPro20x6V1,
	} {
		status := edgeValidCPAStatusForValidationV1()
		status.LocalService = service
		assert.NoError(t, status.Validate())
	}

	cpaCases := []struct {
		name   string
		mutate func(*EdgeCPAStatusV1)
	}{
		{name: "invalid local service", mutate: func(value *EdgeCPAStatusV1) { value.LocalService = "CPA unknown" }},
		{name: "negative latency", mutate: func(value *EdgeCPAStatusV1) { value.LatencyMilliseconds = -1 }},
		{name: "duplicate model", mutate: func(value *EdgeCPAStatusV1) { value.AvailableModels = []string{"gpt-5", "gpt-5"} }},
		{name: "empty model", mutate: func(value *EdgeCPAStatusV1) { value.AvailableModels = []string{""} }},
		{name: "model too long", mutate: func(value *EdgeCPAStatusV1) {
			value.AvailableModels = []string{strings.Repeat("m", EdgeControlMaxModelLengthV1+1)}
		}},
		{name: "too many models", mutate: func(value *EdgeCPAStatusV1) {
			value.AvailableModels = make([]string, EdgeControlMaxAvailableModelsV1+1)
		}},
		{name: "zero checked timestamp", mutate: func(value *EdgeCPAStatusV1) { value.CheckedAtUnixMilli = 0 }},
	}
	for _, tc := range cpaCases {
		t.Run("cpa "+tc.name, func(t *testing.T) {
			value := edgeValidCPAStatusForValidationV1()
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func TestEdgeHeartbeatRequestV1Validate(t *testing.T) {
	require.NoError(t, edgeValidHeartbeatRequestForValidationV1().Validate())

	cases := []struct {
		name   string
		mutate func(*EdgeHeartbeatRequestV1)
	}{
		{name: "negative uptime", mutate: func(value *EdgeHeartbeatRequestV1) { value.Runtime.UptimeSeconds = -1 }},
		{name: "negative in flight requests", mutate: func(value *EdgeHeartbeatRequestV1) { value.Runtime.InFlightRequests = -1 }},
		{name: "negative recent request count", mutate: func(value *EdgeHeartbeatRequestV1) { value.Runtime.RecentRequestCount = -1 }},
		{name: "negative recent error count", mutate: func(value *EdgeHeartbeatRequestV1) { value.Runtime.RecentErrorCount = -1 }},
		{name: "duplicate cpa", mutate: func(value *EdgeHeartbeatRequestV1) { value.CPA = append(value.CPA, value.CPA[0]) }},
		{name: "too many cpa statuses", mutate: func(value *EdgeHeartbeatRequestV1) {
			value.CPA = make([]EdgeCPAStatusV1, EdgeControlMaxHeartbeatCPAStatusesV1+1)
		}},
		{name: "invalid cpa", mutate: func(value *EdgeHeartbeatRequestV1) { value.CPA[0].LocalService = "cpa/unknown" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeValidHeartbeatRequestForValidationV1()
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func TestEdgeChannelAndModelProjectionV1Validate(t *testing.T) {
	channel := EdgeChannelProjectionV1{
		ChannelID:    1,
		Type:         99,
		Name:         "CPA text channel",
		Enabled:      true,
		Groups:       []string{"default"},
		Models:       []string{"gpt-5.1", "claude-sonnet-4"},
		ModelMapping: map[string]string{"gpt-5.1": "gpt-5.1-upstream"},
		Weight:       10,
		LocalService: EdgeLocalServiceCPAPro20x5V1,
	}
	require.NoError(t, channel.Validate())

	invalidChannel := channel
	invalidChannel.LocalService = "cpa/unknown"
	assert.Error(t, invalidChannel.Validate())
	invalidChannel = channel
	invalidChannel.Models = []string{"gpt-5.1", "gpt-5.1"}
	assert.Error(t, invalidChannel.Validate())
	invalidChannel = channel
	invalidChannel.Groups = []string{"default", "default"}
	assert.Error(t, invalidChannel.Validate())

	model := EdgeModelPolicyV1{
		Model:      "gpt-5.1",
		Enabled:    true,
		Endpoints:  []EdgeEndpointV1{EdgeEndpointOpenAIChatCompletionsV1, EdgeEndpointOpenAIResponsesV1},
		Streaming:  true,
		ChannelIDs: []int64{1, 2},
	}
	require.NoError(t, model.Validate())
	model.Endpoints[1] = model.Endpoints[0]
	assert.Error(t, model.Validate())
	model = EdgeModelPolicyV1{
		Model:      strings.Repeat("m", EdgeControlMaxModelLengthV1+1),
		Endpoints:  []EdgeEndpointV1{EdgeEndpointOpenAIResponsesV1},
		ChannelIDs: []int64{1},
	}
	assert.Error(t, model.Validate())
}

func TestEdgeControlV1StableMetadataAndKeyJSON(t *testing.T) {
	responseMeta := edgeTestResponseMetaV1("")
	require.NoError(t, responseMeta.Validate())
	encoded, err := common.Marshal(responseMeta)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"protocol_version":"edge-control.v1",
		"server_request_id":"server-request",
		"server_time_unix_milli":1700000000100
	}`, string(encoded))
	assert.NotContains(t, string(encoded), `"request_id":`)

	responseMeta = edgeTestResponseMetaV1("request-1")
	require.NoError(t, responseMeta.Validate())
	encoded, err = common.Marshal(responseMeta)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"protocol_version":"edge-control.v1",
		"request_id":"request-1",
		"server_request_id":"server-request-1",
		"server_time_unix_milli":1700000000100
	}`, string(encoded))

	fingerprint := EdgeTokenFingerprintSchemeV1{
		Algorithm: edgetoken.FingerprintAlgorithm,
		Version:   edgetoken.FingerprintVersion,
	}
	require.NoError(t, fingerprint.Validate())
	encoded, err = common.Marshal(fingerprint)
	require.NoError(t, err)
	assert.JSONEq(t, `{"algorithm":"sha256","version":1}`, string(encoded))
	assert.NotContains(t, string(encoded), "key_id")

	control := edgeTestControlConfigV1(t)
	require.NoError(t, control.Validate())
	encoded, err = common.Marshal(control)
	require.NoError(t, err)
	verificationKey := control.SnapshotVerificationKeys[0]
	expectedControl := fmt.Sprintf(`{
		"node_id":"node-test-1",
		"node_generation":3,
		"enabled":true,
		"heartbeat_interval_seconds":15,
		"snapshot_poll_interval_seconds":30,
		"snapshot_page_limit":500,
		"settlement_max_events":100,
		"settlement_max_delay_seconds":5,
		"settlement_circuit_open":false,
		"settlement_circuit_epoch":0,
		"clock_skew_tolerance_seconds":60,
		"snapshot_verification_keys":[{
			"key_id":"snapshot-key-1",
			"algorithm":"ed25519",
			"public_key":%q,
			"not_before_unix_milli":1700000000000,
			"expires_at_unix_milli":1800000000000
		}]
	}`, verificationKey.PublicKey)
	assert.JSONEq(t, expectedControl, string(encoded))
}

func TestEdgeControlResponseMetaV1Validate(t *testing.T) {
	require.NoError(t, edgeTestResponseMetaV1("request-1").Validate())
	require.NoError(t, edgeTestResponseMetaV1("").Validate())

	cases := []struct {
		name   string
		mutate func(*EdgeControlResponseMetaV1)
	}{
		{name: "wrong protocol", mutate: func(value *EdgeControlResponseMetaV1) { value.ProtocolVersion = "edge-control.v3" }},
		{name: "uppercase client request id", mutate: func(value *EdgeControlResponseMetaV1) { value.RequestID = "Request-1" }},
		{name: "missing server request id", mutate: func(value *EdgeControlResponseMetaV1) { value.ServerRequestID = "" }},
		{name: "uppercase server request id", mutate: func(value *EdgeControlResponseMetaV1) { value.ServerRequestID = "Server-1" }},
		{name: "zero server time", mutate: func(value *EdgeControlResponseMetaV1) { value.ServerTimeUnixMilli = 0 }},
		{name: "server time beyond year 9999", mutate: func(value *EdgeControlResponseMetaV1) { value.ServerTimeUnixMilli = edgeControlMaxUnixMilliV1 + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeTestResponseMetaV1("request-1")
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func TestEdgeTokenFingerprintSchemeV1Validate(t *testing.T) {
	valid := EdgeTokenFingerprintSchemeV1{
		Algorithm: edgetoken.FingerprintAlgorithm,
		Version:   edgetoken.FingerprintVersion,
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name   string
		mutate func(*EdgeTokenFingerprintSchemeV1)
	}{
		{name: "wrong algorithm", mutate: func(value *EdgeTokenFingerprintSchemeV1) { value.Algorithm = "sha512" }},
		{name: "wrong version", mutate: func(value *EdgeTokenFingerprintSchemeV1) { value.Version = 2 }},
		{name: "key id present", mutate: func(value *EdgeTokenFingerprintSchemeV1) { value.KeyID = "fingerprint-key-1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := valid
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func TestEdgeSnapshotVerificationKeyV1Validate(t *testing.T) {
	require.NoError(t, edgeTestSnapshotVerificationKeyV1(t, "snapshot-key-1", 0x42).Validate())

	cases := []struct {
		name   string
		mutate func(*EdgeSnapshotVerificationKeyV1)
	}{
		{name: "uppercase key id", mutate: func(value *EdgeSnapshotVerificationKeyV1) { value.KeyID = "Snapshot-Key-1" }},
		{name: "wrong algorithm", mutate: func(value *EdgeSnapshotVerificationKeyV1) { value.Algorithm = "rsa" }},
		{name: "invalid public key", mutate: func(value *EdgeSnapshotVerificationKeyV1) { value.PublicKey = "not-base64" }},
		{name: "noncanonical public key", mutate: func(value *EdgeSnapshotVerificationKeyV1) { value.PublicKey = strings.TrimRight(value.PublicKey, "=") }},
		{name: "zero not before", mutate: func(value *EdgeSnapshotVerificationKeyV1) { value.NotBeforeUnixMilli = 0 }},
		{name: "zero expiry", mutate: func(value *EdgeSnapshotVerificationKeyV1) { value.ExpiresAtUnixMilli = 0 }},
		{name: "expiry equals not before", mutate: func(value *EdgeSnapshotVerificationKeyV1) { value.ExpiresAtUnixMilli = value.NotBeforeUnixMilli }},
		{name: "expiry before not before", mutate: func(value *EdgeSnapshotVerificationKeyV1) { value.ExpiresAtUnixMilli = value.NotBeforeUnixMilli - 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeTestSnapshotVerificationKeyV1(t, "snapshot-key-1", 0x42)
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func TestEdgeNodeControlConfigV1Validate(t *testing.T) {
	require.NoError(t, edgeTestControlConfigV1(t).Validate())

	cases := []struct {
		name   string
		mutate func(*EdgeNodeControlConfigV1)
	}{
		{name: "uppercase node id", mutate: func(value *EdgeNodeControlConfigV1) { value.NodeID = "Node-1" }},
		{name: "zero node generation", mutate: func(value *EdgeNodeControlConfigV1) { value.NodeGeneration = 0 }},
		{name: "zero heartbeat interval", mutate: func(value *EdgeNodeControlConfigV1) { value.HeartbeatIntervalSeconds = 0 }},
		{name: "heartbeat interval over limit", mutate: func(value *EdgeNodeControlConfigV1) {
			value.HeartbeatIntervalSeconds = EdgeControlMaxHeartbeatIntervalSecondsV1 + 1
		}},
		{name: "zero snapshot poll interval", mutate: func(value *EdgeNodeControlConfigV1) { value.SnapshotPollIntervalSeconds = 0 }},
		{name: "snapshot poll interval over limit", mutate: func(value *EdgeNodeControlConfigV1) {
			value.SnapshotPollIntervalSeconds = EdgeControlMaxSnapshotPollIntervalSecondsV1 + 1
		}},
		{name: "zero snapshot page limit", mutate: func(value *EdgeNodeControlConfigV1) { value.SnapshotPageLimit = 0 }},
		{name: "snapshot page limit over limit", mutate: func(value *EdgeNodeControlConfigV1) { value.SnapshotPageLimit = EdgeControlMaxSnapshotPageLimitV1 + 1 }},
		{name: "zero settlement events", mutate: func(value *EdgeNodeControlConfigV1) { value.SettlementMaxEvents = 0 }},
		{name: "settlement events over limit", mutate: func(value *EdgeNodeControlConfigV1) { value.SettlementMaxEvents = EdgeControlMaxSettlementEventsV1 + 1 }},
		{name: "zero settlement delay", mutate: func(value *EdgeNodeControlConfigV1) { value.SettlementMaxDelaySeconds = 0 }},
		{name: "settlement delay over limit", mutate: func(value *EdgeNodeControlConfigV1) {
			value.SettlementMaxDelaySeconds = EdgeControlMaxSettlementDelaySecondsV1 + 1
		}},
		{name: "zero clock skew", mutate: func(value *EdgeNodeControlConfigV1) { value.ClockSkewToleranceSeconds = 0 }},
		{name: "clock skew over limit", mutate: func(value *EdgeNodeControlConfigV1) {
			value.ClockSkewToleranceSeconds = EdgeControlMaxClockSkewToleranceSecondsV1 + 1
		}},
		{name: "empty snapshot verification keys", mutate: func(value *EdgeNodeControlConfigV1) { value.SnapshotVerificationKeys = nil }},
		{name: "too many snapshot verification keys", mutate: func(value *EdgeNodeControlConfigV1) {
			value.SnapshotVerificationKeys = make([]EdgeSnapshotVerificationKeyV1, EdgeControlMaxSnapshotVerificationKeysV1+1)
		}},
		{name: "invalid snapshot verification key", mutate: func(value *EdgeNodeControlConfigV1) { value.SnapshotVerificationKeys[0].Algorithm = "rsa" }},
		{name: "duplicate snapshot key id", mutate: func(value *EdgeNodeControlConfigV1) {
			value.SnapshotVerificationKeys = append(value.SnapshotVerificationKeys, edgeTestSnapshotVerificationKeyV1(t, "snapshot-key-1", 0x43))
		}},
		{name: "duplicate snapshot public key", mutate: func(value *EdgeNodeControlConfigV1) {
			duplicate := value.SnapshotVerificationKeys[0]
			duplicate.KeyID = "snapshot-key-2"
			value.SnapshotVerificationKeys = append(value.SnapshotVerificationKeys, duplicate)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeTestControlConfigV1(t)
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func edgeFloat64PtrV1(value float64) *float64 {
	return &value
}

func edgeValidPricingPolicyV1(mode EdgeBillingModeV1) EdgePricingPolicyV1 {
	policy := EdgePricingPolicyV1{
		PolicyID:             "pricing-policy-1",
		Version:              "pricing-version-1",
		Model:                "gpt-edge-test",
		BillingMode:          mode,
		CompletionRatio:      edgeFloat64PtrV1(2),
		CacheReadRatio:       edgeFloat64PtrV1(0.1),
		CacheCreationRatio:   edgeFloat64PtrV1(1.25),
		CacheCreation1hRatio: edgeFloat64PtrV1(2),
		QuotaPerUnit:         500_000,
	}
	switch mode {
	case EdgeBillingModeRatioV1:
		policy.ModelRatio = edgeFloat64PtrV1(1.25)
	case EdgeBillingModeFixedPriceV1:
		policy.ModelPrice = edgeFloat64PtrV1(0.01)
	case EdgeBillingModeTieredExprV1:
		policy.ModelRatio = edgeFloat64PtrV1(1.25)
		policy.BillingExpression = `v1:tier("base", p * 1.25 + c * 2)`
		policy.BillingExpressionHash = billingexpr.ExprHashString(policy.BillingExpression)
		policy.BillingExpressionVersion = billingexpr.DefaultExprVersion
	}
	return policy
}

func edgeValidRoutingPolicyV1() EdgeRoutingPolicyV1 {
	return EdgeRoutingPolicyV1{
		ChannelAffinity: EdgeChannelAffinityPolicyV1{
			Enabled:               true,
			SwitchOnSuccess:       true,
			KeepOnChannelDisabled: false,
			MaxEntries:            100_000,
			DefaultTTLSeconds:     3600,
			Rules: []EdgeChannelAffinityRuleV1{
				{
					Name:       "codex cli trace",
					ModelRegex: []string{"^gpt-.*$"},
					PathRegex:  []string{"^/v1/responses$"},
					KeySources: []EdgeChannelAffinityKeySourceV1{
						{Type: EdgeChannelAffinityKeySourceGJSONV1, Path: "prompt_cache_key"},
						{Type: EdgeChannelAffinityKeySourceRequestHeaderV1, Key: "X-Client-Request-Id"},
					},
					ValueRegex:        "^[[:graph:]]+$",
					TTLSeconds:        0,
					PassHeaders:       []string{"Originator", "Session_id", "User-Agent"},
					KeepOrigin:        true,
					SkipRetry:         true,
					IncludeUsingGroup: true,
					IncludeRuleName:   true,
				},
			},
		},
	}
}

func edgeValidSnapshotPageResponseV1(dataset EdgeSnapshotDatasetV1) EdgeSnapshotPageResponseV1 {
	response := EdgeSnapshotPageResponseV1{
		Meta:       edgeTestResponseMetaV1("request-page-validation"),
		SnapshotID: "snapshot-validation-7",
		Dataset:    dataset,
		Revision:   7,
		NextCursor: "cursor-next-2",
		ItemCount:  1,
		Digest:     edgeTestDigestV1("d"),
	}
	switch dataset {
	case EdgeSnapshotDatasetAuthenticationV1:
		response.Payload.Authentication = []EdgeTokenAuthRecordV1{{
			TokenFingerprint:  edgeTestDigestV1("a"),
			TokenID:           11,
			UserID:            21,
			Enabled:           true,
			Group:             "default",
			ModelLimitEnabled: true,
			AllowedModels:     []string{"gpt-edge-test"},
			AllowedCIDRs:      []string{"192.0.2.0/24"},
		}}
	case EdgeSnapshotDatasetUsersV1:
		response.Payload.Users = []EdgeUserPolicyV1{{
			UserID:       21,
			Enabled:      true,
			Username:     "edge user",
			Email:        "edge-user@example.invalid",
			DefaultGroup: "default",
			Setting:      EdgeUserSettingV1{Language: "en", BillingPreference: "subscription_first"},
		}}
	case EdgeSnapshotDatasetGroupsV1:
		response.Payload.Groups = []EdgeGroupPolicyV1{{
			UserGroup: "default",
			UsingGroups: []EdgeUsingGroupPolicyV1{{
				Group: "default",
				Ratio: 1,
			}},
		}}
	case EdgeSnapshotDatasetModelsV1:
		response.Payload.Models = []EdgeModelPolicyV1{{
			Model:      "gpt-edge-test",
			Enabled:    true,
			Endpoints:  []EdgeEndpointV1{EdgeEndpointOpenAIChatCompletionsV1, EdgeEndpointOpenAIResponsesV1},
			Streaming:  true,
			ChannelIDs: []int64{31},
		}}
	case EdgeSnapshotDatasetChannelsV1:
		response.Payload.Channels = []EdgeChannelProjectionV1{{
			ChannelID:    31,
			Type:         99,
			Name:         "edge channel",
			Enabled:      true,
			Groups:       []string{"default"},
			Models:       []string{"gpt-edge-test"},
			Weight:       1,
			LocalService: EdgeLocalServiceCPAPro20x4V1,
		}}
	case EdgeSnapshotDatasetPricingV1:
		response.Payload.Pricing = []EdgePricingPolicyV1{edgeValidPricingPolicyV1(EdgeBillingModeTieredExprV1)}
	case EdgeSnapshotDatasetRoutingV1:
		response.Payload.Routing = []EdgeRoutingPolicyV1{edgeValidRoutingPolicyV1()}
	}
	return response
}

func edgeValidUsageEventV1() EdgeUsageEventV1 {
	httpStatus := 200
	return EdgeUsageEventV1{
		EventID:             "event-validation-9",
		Sequence:            9,
		ReservationID:       "reservation-validation-9",
		RequestID:           "relay-request-validation-9",
		UserID:              21,
		TokenID:             11,
		SnapshotID:          "snapshot-validation-7",
		SnapshotRevision:    7,
		PricingRevision:     4,
		BalanceRevision:     2,
		FundingSource:       "wallet",
		ChannelID:           31,
		Endpoint:            EdgeEndpointOpenAIResponsesV1,
		Streaming:           true,
		Model:               "gpt-edge-test",
		Group:               "default",
		StartedAtUnixMilli:  1_700_000_000_600,
		FinishedAtUnixMilli: 1_700_000_000_900,
		Outcome:             EdgeUsageOutcomeSuccessV1,
		HTTPStatus:          &httpStatus,
		Usage: NewOpenAIResponsesBillingUsage(&Usage{
			PromptTokens:     120,
			CompletionTokens: 30,
			TotalTokens:      150,
			PromptTokensDetails: InputTokenDetails{
				CachedTokens: 20,
				TextTokens:   100,
			},
			CompletionTokenDetails: OutputTokenDetails{
				TextTokens:      30,
				ReasoningTokens: 5,
			},
		}),
		Billing: EdgeUsageBillingV1{
			PricingPolicyID:       "pricing-policy-1",
			PricingPolicyVersion:  "pricing-version-1",
			BillingMode:           EdgeBillingModeTieredExprV1,
			GroupRatio:            1,
			AppliedRatios:         map[string]float64{"speed": 2},
			BillingExpressionHash: edgeTestDigestV1("d"),
			MatchedTier:           "base",
			ReservedQuota:         100,
			ChargedQuota:          80,
		},
	}
}

func edgeValidSettlementBlockV1() EdgeSettlementBlockRequestV1 {
	event := edgeValidUsageEventV1()
	return EdgeSettlementBlockRequestV1{
		Meta:                edgeTestRequestMetaV1("request-settlement-validation"),
		BlockID:             "block-validation-9",
		PreviousBlockID:     "block-validation-8",
		PreviousBlockDigest: edgeTestDigestV1("e"),
		FirstSequence:       event.Sequence,
		LastSequence:        event.Sequence,
		CreatedAtUnixMilli:  1_700_000_001_000,
		BlockDigest:         edgeTestDigestV1("f"),
		Events:              []EdgeUsageEventV1{event},
	}
}

func TestEdgeSnapshotManifestAndDatasetV1Validate(t *testing.T) {
	valid := edgeTestManifestV1()
	require.NoError(t, valid.Validate())
	require.NoError(t, valid.Datasets[0].Validate())

	cases := []struct {
		name   string
		mutate func(*EdgeSnapshotManifestV1)
	}{
		{name: "zero revision", mutate: func(value *EdgeSnapshotManifestV1) { value.Revision = 0 }},
		{name: "expiry not after creation", mutate: func(value *EdgeSnapshotManifestV1) { value.ExpiresAtUnixMilli = value.CreatedAtUnixMilli }},
		{name: "wrong hash algorithm", mutate: func(value *EdgeSnapshotManifestV1) { value.HashAlgorithm = "sha512" }},
		{name: "uppercase digest", mutate: func(value *EdgeSnapshotManifestV1) { value.Digest = strings.ToUpper(value.Digest) }},
		{name: "invalid token scheme", mutate: func(value *EdgeSnapshotManifestV1) { value.TokenFingerprint.Version++ }},
		{name: "empty datasets", mutate: func(value *EdgeSnapshotManifestV1) { value.Datasets = nil }},
		{name: "duplicate dataset", mutate: func(value *EdgeSnapshotManifestV1) { value.Datasets[1] = value.Datasets[0] }},
		{name: "out of order datasets", mutate: func(value *EdgeSnapshotManifestV1) {
			value.Datasets[0], value.Datasets[1] = value.Datasets[1], value.Datasets[0]
		}},
		{name: "dataset revision beyond snapshot", mutate: func(value *EdgeSnapshotManifestV1) { value.Datasets[0].Revision = value.Revision + 1 }},
		{name: "negative item count", mutate: func(value *EdgeSnapshotManifestV1) { value.Datasets[0].ItemCount = -1 }},
		{name: "zero items with page", mutate: func(value *EdgeSnapshotManifestV1) { value.Datasets[0].ItemCount = 0 }},
		{name: "items without page", mutate: func(value *EdgeSnapshotManifestV1) { value.Datasets[0].PageCount = 0 }},
		{name: "pages exceed items", mutate: func(value *EdgeSnapshotManifestV1) { value.Datasets[0].PageCount = 2 }},
		{name: "wrong signature algorithm", mutate: func(value *EdgeSnapshotManifestV1) { value.Datasets[0].DetachedSignature.Algorithm = "rsa" }},
		{name: "signature digest mismatch", mutate: func(value *EdgeSnapshotManifestV1) {
			value.Datasets[0].DetachedSignature.PayloadDigest = edgeTestDigestV1("c")
		}},
		{name: "malformed signature", mutate: func(value *EdgeSnapshotManifestV1) { value.Datasets[0].DetachedSignature.Value = "not-base64" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeTestManifestV1()
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}
}

func TestEdgeSnapshotManifestRequestAndResponseV1Validate(t *testing.T) {
	manifest := edgeTestManifestV1()
	request := EdgeSnapshotManifestRequestV1{
		Meta:    edgeTestRequestMetaV1("request-manifest-validation"),
		Current: edgeValidSnapshotStateForValidationV1(),
	}
	require.NoError(t, request.Validate())
	request.Current.Revision = -1
	assert.Error(t, request.Validate())

	response := EdgeSnapshotManifestResponseV1{
		Meta:     edgeTestResponseMetaV1("request-manifest-validation"),
		Changed:  true,
		Snapshot: &manifest,
	}
	require.NoError(t, response.Validate())
	response.Changed = false
	assert.Error(t, response.Validate())
	response.Snapshot = nil
	require.NoError(t, response.Validate())
	response.Changed = true
	assert.Error(t, response.Validate())
}

func TestEdgeSnapshotPageRequestPayloadAndResponseV1Validate(t *testing.T) {
	request := EdgeSnapshotPageRequestV1{
		Meta:       edgeTestRequestMetaV1("request-page-validation"),
		SnapshotID: "snapshot-validation-7",
		Dataset:    EdgeSnapshotDatasetAuthenticationV1,
		Cursor:     "cursor-authentication-1",
		Limit:      500,
	}
	require.NoError(t, request.Validate())
	request.Limit = 0
	assert.Error(t, request.Validate())
	request.Limit = 500
	request.Cursor = "Cursor-1"
	assert.Error(t, request.Validate())

	for _, dataset := range []EdgeSnapshotDatasetV1{
		EdgeSnapshotDatasetAuthenticationV1,
		EdgeSnapshotDatasetUsersV1,
		EdgeSnapshotDatasetGroupsV1,
		EdgeSnapshotDatasetModelsV1,
		EdgeSnapshotDatasetChannelsV1,
		EdgeSnapshotDatasetPricingV1,
		EdgeSnapshotDatasetRoutingV1,
	} {
		t.Run(string(dataset), func(t *testing.T) {
			response := edgeValidSnapshotPageResponseV1(dataset)
			require.NoError(t, response.Validate())
		})
	}

	response := edgeValidSnapshotPageResponseV1(EdgeSnapshotDatasetUsersV1)
	response.Payload.Models = edgeValidSnapshotPageResponseV1(EdgeSnapshotDatasetModelsV1).Payload.Models
	assert.Error(t, response.Validate())

	response = edgeValidSnapshotPageResponseV1(EdgeSnapshotDatasetUsersV1)
	response.ItemCount = 2
	assert.Error(t, response.Validate())

	response = edgeValidSnapshotPageResponseV1(EdgeSnapshotDatasetUsersV1)
	response.Payload.Users = append(response.Payload.Users, response.Payload.Users[0])
	response.ItemCount = 2
	assert.Error(t, response.Validate())

	response = edgeValidSnapshotPageResponseV1(EdgeSnapshotDatasetUsersV1)
	response.Cursor = response.NextCursor
	assert.Error(t, response.Validate())

	response = edgeValidSnapshotPageResponseV1(EdgeSnapshotDatasetUsersV1)
	response.Digest = strings.Repeat("g", 64)
	assert.Error(t, response.Validate())
}

func TestEdgeSnapshotGroupPolicyAllowsZeroRatioAndRejectsInvalidRatios(t *testing.T) {
	tests := []struct {
		name    string
		ratio   float64
		wantErr bool
	}{
		{name: "zero", ratio: 0},
		{name: "negative", ratio: -1, wantErr: true},
		{name: "nan", ratio: math.NaN(), wantErr: true},
		{name: "positive infinity", ratio: math.Inf(1), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := edgeValidSnapshotPageResponseV1(EdgeSnapshotDatasetGroupsV1)
			response.Payload.Groups[0].UsingGroups[0].Ratio = test.ratio
			err := response.Validate()
			if test.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestEdgeRoutingPolicyV1Validate(t *testing.T) {
	valid := edgeValidRoutingPolicyV1()
	require.NoError(t, valid.Validate())

	for _, source := range []EdgeChannelAffinityKeySourceV1{
		{Type: EdgeChannelAffinityKeySourceContextIntV1, Key: "token_id"},
		{Type: EdgeChannelAffinityKeySourceContextStringV1, Key: "username"},
		{Type: EdgeChannelAffinityKeySourceRequestHeaderV1, Key: "X-Affinity-Key"},
		{Type: EdgeChannelAffinityKeySourceGJSONV1, Path: "metadata.user_id"},
	} {
		require.NoError(t, source.Validate())
	}

	sourceCases := []EdgeChannelAffinityKeySourceV1{
		{Type: "body", Key: "token_id"},
		{Type: EdgeChannelAffinityKeySourceContextIntV1},
		{Type: EdgeChannelAffinityKeySourceContextStringV1, Key: "bad key"},
		{Type: EdgeChannelAffinityKeySourceContextIntV1, Key: "token_id", Path: "token.id"},
		{Type: EdgeChannelAffinityKeySourceRequestHeaderV1, Key: "Bad Header"},
		{Type: EdgeChannelAffinityKeySourceRequestHeaderV1, Key: "X-Key", Path: "header"},
		{Type: EdgeChannelAffinityKeySourceGJSONV1, Key: "body", Path: "metadata.user_id"},
		{Type: EdgeChannelAffinityKeySourceGJSONV1},
	}
	for i, source := range sourceCases {
		assert.Error(t, source.Validate(), i)
	}

	ruleCases := []struct {
		name   string
		mutate func(*EdgeChannelAffinityRuleV1)
	}{
		{name: "empty model regex", mutate: func(value *EdgeChannelAffinityRuleV1) { value.ModelRegex = nil }},
		{name: "invalid model regex", mutate: func(value *EdgeChannelAffinityRuleV1) { value.ModelRegex = []string{"["} }},
		{name: "duplicate model regex", mutate: func(value *EdgeChannelAffinityRuleV1) { value.ModelRegex = []string{"^gpt", "^gpt"} }},
		{name: "invalid path regex", mutate: func(value *EdgeChannelAffinityRuleV1) { value.PathRegex = []string{"("} }},
		{name: "duplicate user agent include", mutate: func(value *EdgeChannelAffinityRuleV1) { value.UserAgentInclude = []string{"Codex", "codex"} }},
		{name: "empty key sources", mutate: func(value *EdgeChannelAffinityRuleV1) { value.KeySources = nil }},
		{name: "duplicate key source", mutate: func(value *EdgeChannelAffinityRuleV1) {
			value.KeySources = append(value.KeySources, value.KeySources[0])
		}},
		{name: "invalid value regex", mutate: func(value *EdgeChannelAffinityRuleV1) { value.ValueRegex = "[" }},
		{name: "negative ttl", mutate: func(value *EdgeChannelAffinityRuleV1) { value.TTLSeconds = -1 }},
		{name: "ttl over limit", mutate: func(value *EdgeChannelAffinityRuleV1) { value.TTLSeconds = EdgeControlMaxAffinityTTLSecondsV1 + 1 }},
		{name: "invalid pass header", mutate: func(value *EdgeChannelAffinityRuleV1) { value.PassHeaders = []string{"Bad Header"} }},
		{name: "duplicate pass header", mutate: func(value *EdgeChannelAffinityRuleV1) { value.PassHeaders = []string{"X-Test", "x-test"} }},
		{name: "keep origin without headers", mutate: func(value *EdgeChannelAffinityRuleV1) { value.PassHeaders = nil }},
	}
	for _, tc := range ruleCases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeValidRoutingPolicyV1().ChannelAffinity.Rules[0]
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}

	policy := edgeValidRoutingPolicyV1().ChannelAffinity
	policy.MaxEntries = 0
	assert.Error(t, policy.Validate())
	policy = edgeValidRoutingPolicyV1().ChannelAffinity
	policy.DefaultTTLSeconds = 0
	assert.Error(t, policy.Validate())
	policy = edgeValidRoutingPolicyV1().ChannelAffinity
	policy.Rules = append(policy.Rules, policy.Rules[0])
	assert.Error(t, policy.Validate())

	page := edgeValidSnapshotPageResponseV1(EdgeSnapshotDatasetRoutingV1)
	require.NoError(t, page.Validate())
	page.Payload.Routing = append(page.Payload.Routing, edgeValidRoutingPolicyV1())
	page.ItemCount = 2
	assert.Error(t, page.Validate())
}

func TestEdgeRoutingPolicyV1StableJSON(t *testing.T) {
	encoded, err := common.Marshal(edgeValidRoutingPolicyV1())
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"channel_affinity": {
			"enabled": true,
			"switch_on_success": true,
			"keep_on_channel_disabled": false,
			"max_entries": 100000,
			"default_ttl_seconds": 3600,
			"rules": [{
				"name": "codex cli trace",
				"model_regex": ["^gpt-.*$"],
				"path_regex": ["^/v1/responses$"],
				"key_sources": [
					{"type":"gjson","path":"prompt_cache_key"},
					{"type":"request_header","key":"X-Client-Request-Id"}
				],
				"value_regex": "^[[:graph:]]+$",
				"ttl_seconds": 0,
				"pass_headers": ["Originator","Session_id","User-Agent"],
				"keep_origin": true,
				"skip_retry": true,
				"include_using_group": true,
				"include_model_name": false,
				"include_rule_name": true
			}]
		}
	}`, string(encoded))
	assert.NotContains(t, string(encoded), "param_override")
	assert.NotContains(t, string(encoded), "operations")
}

func TestEdgePricingPolicyV1ValidateAndDetectDynamicDependencies(t *testing.T) {
	for _, mode := range []EdgeBillingModeV1{EdgeBillingModeRatioV1, EdgeBillingModeFixedPriceV1, EdgeBillingModeTieredExprV1} {
		t.Run(string(mode), func(t *testing.T) {
			require.NoError(t, edgeValidPricingPolicyV1(mode).Validate())
		})
	}
	zeroModelRatio := edgeValidPricingPolicyV1(EdgeBillingModeRatioV1)
	zeroModelRatio.ModelRatio = edgeFloat64PtrV1(0)
	require.NoError(t, zeroModelRatio.Validate())

	cases := []struct {
		name   string
		mutate func(*EdgePricingPolicyV1)
	}{
		{name: "unknown mode", mutate: func(value *EdgePricingPolicyV1) { value.BillingMode = "dynamic" }},
		{name: "negative model ratio", mutate: func(value *EdgePricingPolicyV1) { value.ModelRatio = edgeFloat64PtrV1(-1) }},
		{name: "nan ratio", mutate: func(value *EdgePricingPolicyV1) { value.ModelRatio = edgeFloat64PtrV1(math.NaN()) }},
		{name: "infinite model ratio", mutate: func(value *EdgePricingPolicyV1) { value.ModelRatio = edgeFloat64PtrV1(math.Inf(1)) }},
		{name: "infinite completion ratio", mutate: func(value *EdgePricingPolicyV1) { value.CompletionRatio = edgeFloat64PtrV1(math.Inf(1)) }},
		{name: "negative cache ratio", mutate: func(value *EdgePricingPolicyV1) { value.CacheReadRatio = edgeFloat64PtrV1(-0.1) }},
		{name: "zero quota per unit", mutate: func(value *EdgePricingPolicyV1) { value.QuotaPerUnit = 0 }},
		{name: "missing expression", mutate: func(value *EdgePricingPolicyV1) { value.BillingExpression = "" }},
		{name: "hash mismatch", mutate: func(value *EdgePricingPolicyV1) { value.BillingExpressionHash = edgeTestDigestV1("a") }},
		{name: "unknown expression version", mutate: func(value *EdgePricingPolicyV1) {
			value.BillingExpression = `v2:tier("base", p)`
			value.BillingExpressionHash = billingexpr.ExprHashString(value.BillingExpression)
		}},
		{name: "expression fields on ratio mode", mutate: func(value *EdgePricingPolicyV1) {
			value.BillingMode = EdgeBillingModeRatioV1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeValidPricingPolicyV1(EdgeBillingModeTieredExprV1)
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}

	for _, expression := range []string{
		`p * (param("service_tier") == "fast" ? 2 : 1)`,
		`p * (header("x-speed") == "fast" ? 2 : 1)`,
		`p * (hour("UTC") >= 0 ? 1 : 1)`,
		`p * (minute("UTC") >= 0 ? 1 : 1)`,
		`p * (weekday("UTC") >= 0 ? 1 : 1)`,
		`p * (month("UTC") >= 1 ? 1 : 1)`,
		`p * (day("UTC") >= 1 ? 1 : 1)`,
		`tier("base", p * 2)|||when(header("x-speed") == "fast") * 2`,
	} {
		assert.True(t, EdgeBillingExpressionHasRequestOrTimeDependenciesV1(expression), expression)
	}
	assert.False(t, EdgeBillingExpressionHasRequestOrTimeDependenciesV1(`tier("param header hour minute weekday month day", p * 2 + c)`))
	assert.False(t, EdgeBillingExpressionHasRequestOrTimeDependenciesV1(`tier("base", p * 2 + c)`))
	assert.False(t, EdgeBillingExpressionHasRequestOrTimeDependenciesV1(`tier("header( param( hour(", p)|||"weekday("`))
}

func TestEdgeUsageBillingAndEventV1Validate(t *testing.T) {
	event := edgeValidUsageEventV1()
	require.NoError(t, event.Validate())
	zeroGroupRatio := event.Billing
	zeroGroupRatio.GroupRatio = 0
	require.NoError(t, zeroGroupRatio.Validate())

	billingCases := []struct {
		name   string
		mutate func(*EdgeUsageBillingV1)
	}{
		{name: "negative group ratio", mutate: func(value *EdgeUsageBillingV1) { value.GroupRatio = -1 }},
		{name: "nan group ratio", mutate: func(value *EdgeUsageBillingV1) { value.GroupRatio = math.NaN() }},
		{name: "infinite group ratio", mutate: func(value *EdgeUsageBillingV1) { value.GroupRatio = math.Inf(1) }},
		{name: "nan applied ratio", mutate: func(value *EdgeUsageBillingV1) { value.AppliedRatios["speed"] = math.NaN() }},
		{name: "applied ratio product overflow", mutate: func(value *EdgeUsageBillingV1) {
			value.AppliedRatios = map[string]float64{"a": float64(common.MaxQuota), "b": 2}
		}},
		{name: "bad expression hash", mutate: func(value *EdgeUsageBillingV1) { value.BillingExpressionHash = "bad" }},
		{name: "quota over maximum", mutate: func(value *EdgeUsageBillingV1) { value.ReservedQuota = int64(common.MaxQuota) + 1 }},
		{name: "charge over maximum", mutate: func(value *EdgeUsageBillingV1) { value.ChargedQuota = int64(common.MaxQuota) + 1 }},
	}
	for _, tc := range billingCases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeValidUsageEventV1().Billing
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}

	eventCases := []struct {
		name   string
		mutate func(*EdgeUsageEventV1)
	}{
		{name: "zero sequence", mutate: func(value *EdgeUsageEventV1) { value.Sequence = 0 }},
		{name: "missing snapshot", mutate: func(value *EdgeUsageEventV1) { value.SnapshotID = "" }},
		{name: "invalid balance revision", mutate: func(value *EdgeUsageEventV1) { value.BalanceRevision = 0 }},
		{name: "invalid funding source", mutate: func(value *EdgeUsageEventV1) { value.FundingSource = "lease" }},
		{name: "wallet with subscription", mutate: func(value *EdgeUsageEventV1) { value.UserSubscriptionID = 9 }},
		{name: "invalid endpoint", mutate: func(value *EdgeUsageEventV1) { value.Endpoint = "images" }},
		{name: "finish before start", mutate: func(value *EdgeUsageEventV1) { value.FinishedAtUnixMilli = value.StartedAtUnixMilli - 1 }},
		{name: "invalid outcome", mutate: func(value *EdgeUsageEventV1) { value.Outcome = "timeout" }},
		{name: "invalid http status", mutate: func(value *EdgeUsageEventV1) { status := 99; value.HTTPStatus = &status }},
		{name: "non 2xx success", mutate: func(value *EdgeUsageEventV1) { status := 500; value.HTTPStatus = &status }},
		{name: "success error code", mutate: func(value *EdgeUsageEventV1) { value.ErrorCode = "upstream_error" }},
		{name: "negative usage count", mutate: func(value *EdgeUsageEventV1) { value.Usage.OpenAIUsage.PromptTokens = -1 }},
		{name: "oversized usage detail", mutate: func(value *EdgeUsageEventV1) {
			value.Usage.OpenAIUsage.PromptTokensDetails.CachedTokens = EdgeControlMaxBillingTokenCountV1 + 1
		}},
	}
	for _, tc := range eventCases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeValidUsageEventV1()
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}

	claudeUsage := &BillingUsage{
		Source:   BillingUsageSourceClaudeMessages,
		Semantic: BillingUsageSemanticAnthropic,
		ClaudeUsage: &ClaudeUsage{
			InputTokens:  10,
			OutputTokens: 5,
		},
	}
	require.NoError(t, validateEdgeBillingUsageV1(claudeUsage))
	claudeUsage.ClaudeUsage.OutputTokens = -1
	assert.Error(t, validateEdgeBillingUsageV1(claudeUsage))

	geminiUsage := &BillingUsage{
		Source:   BillingUsageSourceGeminiChat,
		Semantic: BillingUsageSemanticGemini,
		GeminiUsageMetadata: &GeminiUsageMetadata{
			PromptTokenCount: 10,
			PromptTokensDetails: []GeminiPromptTokensDetails{{
				Modality:   "text",
				TokenCount: 10,
			}},
		},
	}
	require.NoError(t, validateEdgeBillingUsageV1(geminiUsage))
	geminiUsage.GeminiUsageMetadata.PromptTokensDetails[0].TokenCount = -1
	assert.Error(t, validateEdgeBillingUsageV1(geminiUsage))

	mixed := edgeValidUsageEventV1().Usage
	mixed.ClaudeUsage = &ClaudeUsage{InputTokens: 1}
	assert.Error(t, validateEdgeBillingUsageV1(mixed))
}

func TestEdgeSettlementBlockAndAckV1Validate(t *testing.T) {
	block := edgeValidSettlementBlockV1()
	require.NoError(t, block.Validate())

	second := block.Events[0]
	second.EventID = "event-validation-10"
	second.Sequence = 10
	second.ReservationID = "reservation-validation-10"
	second.RequestID = "relay-request-validation-10"
	block.Events = append(block.Events, second)
	block.LastSequence = second.Sequence
	require.NoError(t, block.Validate())

	blockCases := []struct {
		name   string
		mutate func(*EdgeSettlementBlockRequestV1)
	}{
		{name: "previous id without digest", mutate: func(value *EdgeSettlementBlockRequestV1) { value.PreviousBlockDigest = "" }},
		{name: "same previous block", mutate: func(value *EdgeSettlementBlockRequestV1) { value.PreviousBlockID = value.BlockID }},
		{name: "invalid sequence range", mutate: func(value *EdgeSettlementBlockRequestV1) { value.LastSequence = value.FirstSequence - 1 }},
		{name: "event count mismatch", mutate: func(value *EdgeSettlementBlockRequestV1) { value.LastSequence++ }},
		{name: "event sequence gap", mutate: func(value *EdgeSettlementBlockRequestV1) { value.Events[0].Sequence++ }},
		{name: "duplicate event", mutate: func(value *EdgeSettlementBlockRequestV1) {
			duplicate := value.Events[0]
			duplicate.Sequence++
			duplicate.ReservationID = "reservation-validation-10"
			duplicate.RequestID = "relay-request-validation-10"
			value.Events = append(value.Events, duplicate)
			value.LastSequence++
		}},
		{name: "duplicate reservation", mutate: func(value *EdgeSettlementBlockRequestV1) {
			duplicate := value.Events[0]
			duplicate.EventID = "event-validation-10"
			duplicate.Sequence++
			duplicate.RequestID = "relay-request-validation-10"
			value.Events = append(value.Events, duplicate)
			value.LastSequence++
		}},
		{name: "created before event finished", mutate: func(value *EdgeSettlementBlockRequestV1) {
			value.CreatedAtUnixMilli = value.Events[0].FinishedAtUnixMilli - 1
		}},
	}
	for _, tc := range blockCases {
		t.Run(tc.name, func(t *testing.T) {
			value := edgeValidSettlementBlockV1()
			tc.mutate(&value)
			assert.Error(t, value.Validate())
		})
	}

	ack := edgeTestSettlementAckV1()
	require.NoError(t, ack.Validate())
	ack.NextExpectedSequence++
	assert.Error(t, ack.Validate())
	ack = edgeTestSettlementAckV1()
	ack.AcceptedEventCount = EdgeControlMaxSettlementEventsV1 + 1
	assert.Error(t, ack.Validate())
	ack = edgeTestSettlementAckV1()
	ack.Status = "partial"
	assert.Error(t, ack.Validate())

	response := EdgeSettlementBlockResponseV1{
		Meta: edgeTestResponseMetaV1("request-settlement-validation"),
		Ack:  edgeTestSettlementAckV1(),
	}
	require.NoError(t, response.Validate())
	response.Meta.ProtocolVersion = "edge-control.v3"
	assert.Error(t, response.Validate())
}
