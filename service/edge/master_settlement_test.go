package edge

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgesettlement"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type masterSettlementTestFixture struct {
	db         *gorm.DB
	now        time.Time
	node       *model.EdgeNode
	credential *model.EdgeNodeCredential
	identity   *model.EdgeControlIdentity
	user       *model.User
	token      *model.Token
	channel    *model.Channel
	snapshot   *model.EdgeCompiledSnapshot
}

func TestMasterSettlementChargesWalletAndTokenExactlyOnce(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "wallet", 5_000, 5_000)
	request := masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false)

	ack := settleMasterBlockForTest(t, fixture, request, "settlement-wallet")
	assert.Equal(t, dto.EdgeSettlementAckAcceptedV1, ack.Status)
	assertMasterSettlementBalances(t, fixture, 4_880, 120, 4_880, 120, 120)

	duplicate := settleMasterBlockForTest(t, fixture, request, "settlement-wallet")
	assert.Equal(t, dto.EdgeSettlementAckDuplicateV1, duplicate.Status)
	assertMasterSettlementBalances(t, fixture, 4_880, 120, 4_880, 120, 120)
	for _, target := range []any{&model.EdgeSettlementBlock{}, &model.EdgeUsageEvent{}, &model.EdgeConsumeLogOutbox{}} {
		var count int64
		require.NoError(t, fixture.db.Model(target).Count(&count).Error)
		assert.Equal(t, int64(1), count)
	}
}

func TestMasterSettlementChargesSubscriptionAndAllowsActualAboveReserve(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "subscription", 5_000, 5_000)
	subscription := &model.UserSubscription{
		UserId: fixture.user.Id, AmountTotal: 1_000, AmountUsed: 100,
		StartTime: fixture.now.Add(-time.Hour).Unix(), EndTime: fixture.now.Add(time.Hour).Unix(), Status: "active",
	}
	require.NoError(t, fixture.db.Create(subscription).Error)
	request := masterSettlementBlockForTest(t, fixture, 1, "subscription", int64(subscription.Id), false)
	request.Events[0].Billing.ReservedQuota = 50
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &request))

	settleMasterBlockForTest(t, fixture, request, "settlement-subscription")
	assertMasterSettlementBalances(t, fixture, 5_000, 120, 4_880, 120, 120)
	var stored model.UserSubscription
	require.NoError(t, fixture.db.First(&stored, subscription.Id).Error)
	assert.Equal(t, int64(220), stored.AmountUsed)
}

func TestMasterSettlementZeroUsageCreatesEventWithoutConsumptionStatistics(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "zero-usage-log", 5_000, 5_000)
	request := masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false)
	request.Events[0].Usage = &dto.BillingUsage{
		Source: dto.BillingUsageSourceOAIChat, Semantic: dto.BillingUsageSemanticOpenAI,
		OpenAIUsage: &dto.Usage{},
	}
	request.Events[0].Billing.ReservedQuota = 0
	request.Events[0].Billing.ChargedQuota = 0
	useTime := int64(1)
	request.Events[0].ConsumeLogSnapshot = &dto.EdgeConsumeLogSnapshotV1{
		Username: fixture.user.Username, TokenName: fixture.token.Name, ModelName: "gpt-test",
		UseTimeSeconds: &useTime, RequestID: "zero-visible", Other: map[string]interface{}{"request_path": "/v1/chat/completions"},
	}
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &request))

	settleMasterBlockForTest(t, fixture, request, "settlement-zero-usage-log")
	assertMasterSettlementBalances(t, fixture, 5_000, 0, 5_000, 0, 0)
	var user model.User
	require.NoError(t, fixture.db.First(&user, fixture.user.Id).Error)
	assert.Zero(t, user.RequestCount)
	for _, target := range []any{&model.EdgeUsageEvent{}, &model.EdgeConsumeLogOutbox{}} {
		var count int64
		require.NoError(t, fixture.db.Model(target).Count(&count).Error)
		assert.Equal(t, int64(1), count)
	}
}

func TestMasterSettlementUsesSharedGeminiCompletionFallback(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "gemini-normalization", 5_000, 5_000)
	request := masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false)
	request.Events[0].Usage = dto.NewGeminiChatBillingUsage(&dto.GeminiUsageMetadata{
		PromptTokenCount: 100, TotalTokenCount: 110,
	})
	request.Events[0].Billing.ReservedQuota = 120
	request.Events[0].Billing.ChargedQuota = 120
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &request))

	settleMasterBlockForTest(t, fixture, request, "settlement-gemini-normalization")
	var stored model.EdgeUsageEvent
	require.NoError(t, fixture.db.First(&stored).Error)
	assert.Equal(t, 100, stored.PromptTokens)
	assert.Equal(t, 10, stored.CompletionTokens)
}

func TestRecomputeMasterUsageQuotaAcceptsNormalizedOpenRouterClaudeUsage(t *testing.T) {
	modelRatio := 1.0
	completionRatio := 1.0
	cacheRatio := 0.1
	policies := &masterSnapshotPolicies{
		users: map[int64]dto.EdgeUserPolicyV1{7: {UserID: 7, Enabled: true, DefaultGroup: "default"}},
		groups: map[string]dto.EdgeGroupPolicyV1{"default": {
			UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}},
		}},
		models: map[string]dto.EdgeModelPolicyV1{"anthropic/claude-test": {
			Model: "anthropic/claude-test", Enabled: true, Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointOpenAIChatCompletionsV1}, ChannelIDs: []int64{31},
		}},
		channels: map[int64]dto.EdgeChannelProjectionV1{31: {
			ChannelID: 31, Type: constant.ChannelTypeOpenRouter, Enabled: true, Groups: []string{"default"}, Models: []string{"anthropic/claude-test"},
		}},
		pricing: map[string]dto.EdgePricingPolicyV1{
			masterPricingKey("claude-policy", "v1", "anthropic/claude-test"): {
				PolicyID: "claude-policy", Version: "v1", Model: "anthropic/claude-test", BillingMode: dto.EdgeBillingModeRatioV1,
				ModelRatio: &modelRatio, CompletionRatio: &completionRatio, CacheReadRatio: &cacheRatio,
				CacheCreationRatio: &modelRatio, CacheCreation1hRatio: &modelRatio, QuotaPerUnit: 1,
			},
		},
	}
	event := &dto.EdgeUsageEventV1{
		UserID: 7, ChannelID: 31, Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1,
		Model: "anthropic/claude-test", Group: "default", Outcome: dto.EdgeUsageOutcomeSuccessV1,
		Usage: &dto.BillingUsage{
			Source: dto.BillingUsageSourceClaudeMessages, Semantic: dto.BillingUsageSemanticAnthropic,
			ClaudeUsage: &dto.ClaudeUsage{InputTokens: 172, OutputTokens: 383, CacheReadInputTokens: 2_432},
		},
		Billing: dto.EdgeUsageBillingV1{
			PricingPolicyID: "claude-policy", PricingPolicyVersion: "v1", BillingMode: dto.EdgeBillingModeRatioV1, GroupRatio: 1,
		},
	}

	quota, usage, err := recomputeMasterUsageQuota(policies, 7, event)
	require.NoError(t, err)
	assert.Equal(t, int64(798), quota)
	assert.Equal(t, 172, usage.PromptTokens)
	assert.Equal(t, 2_432, usage.PromptTokensDetails.CachedTokens)
}

func TestRecomputeMasterUsageQuotaUsesMasterCacheCreationDefaults(t *testing.T) {
	modelRatio := 1.0
	for _, tc := range []struct {
		name                  string
		cacheCreation1hTokens int
		wantQuota             int64
	}{
		{name: "default cache creation", wantQuota: 10},
		{name: "one hour cache creation", cacheCreation1hTokens: 8, wantQuota: 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policies := &masterSnapshotPolicies{
				users: map[int64]dto.EdgeUserPolicyV1{7: {UserID: 7, Enabled: true, DefaultGroup: "default"}},
				groups: map[string]dto.EdgeGroupPolicyV1{"default": {
					UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}},
				}},
				models: map[string]dto.EdgeModelPolicyV1{"gpt-cache-defaults": {
					Model: "gpt-cache-defaults", Enabled: true, Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointOpenAIChatCompletionsV1}, ChannelIDs: []int64{31},
				}},
				channels: map[int64]dto.EdgeChannelProjectionV1{31: {
					ChannelID: 31, Enabled: true, Groups: []string{"default"}, Models: []string{"gpt-cache-defaults"},
				}},
				pricing: map[string]dto.EdgePricingPolicyV1{
					masterPricingKey("cache-defaults", "v1", "gpt-cache-defaults"): {
						PolicyID: "cache-defaults", Version: "v1", Model: "gpt-cache-defaults",
						BillingMode: dto.EdgeBillingModeRatioV1, ModelRatio: &modelRatio, QuotaPerUnit: 1,
					},
				},
			}
			event := &dto.EdgeUsageEventV1{
				UserID: 7, ChannelID: 31, Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1,
				Model: "gpt-cache-defaults", Group: "default", Outcome: dto.EdgeUsageOutcomeSuccessV1,
				Usage: dto.NewOpenAIChatBillingUsage(&dto.Usage{
					PromptTokens: 8, TotalTokens: 8,
					PromptTokensDetails:         dto.InputTokenDetails{CachedCreationTokens: 8},
					ClaudeCacheCreation1hTokens: tc.cacheCreation1hTokens,
				}),
				Billing: dto.EdgeUsageBillingV1{
					PricingPolicyID: "cache-defaults", PricingPolicyVersion: "v1",
					BillingMode: dto.EdgeBillingModeRatioV1, GroupRatio: 1,
				},
			}

			quota, _, err := recomputeMasterUsageQuota(policies, 7, event)
			require.NoError(t, err)
			assert.Equal(t, tc.wantQuota, quota)
		})
	}
}

func TestRecomputeMasterUsageQuotaAcceptsTaskReceiptWithoutTokenUsage(t *testing.T) {
	modelRatio := 2.0
	groupRatio := 1.25
	quotaBeforeRatios := 40.0
	policies := &masterSnapshotPolicies{
		users: map[int64]dto.EdgeUserPolicyV1{7: {UserID: 7, Enabled: true, DefaultGroup: "default"}},
		groups: map[string]dto.EdgeGroupPolicyV1{"default": {
			UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: groupRatio}},
		}},
		models: map[string]dto.EdgeModelPolicyV1{"video-task": {
			Model: "video-task", Enabled: true, Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointDataPlaneV1}, ChannelIDs: []int64{31},
		}},
		channels: map[int64]dto.EdgeChannelProjectionV1{31: {
			ChannelID: 31, Enabled: true, Groups: []string{"default"}, Models: []string{"video-task"},
		}},
		pricing: map[string]dto.EdgePricingPolicyV1{
			masterPricingKey("task-policy", "v1", "video-task"): {
				PolicyID: "task-policy", Version: "v1", Model: "video-task", BillingMode: dto.EdgeBillingModeRatioV1,
				ModelRatio: &modelRatio, QuotaPerUnit: 500_000,
			},
		},
	}
	event := &dto.EdgeUsageEventV1{
		UserID: 7, ChannelID: 31, Endpoint: dto.EdgeEndpointTaskV1,
		Model: "video-task", Group: "default", Outcome: dto.EdgeUsageOutcomeSuccessV1,
		Billing: dto.EdgeUsageBillingV1{
			PricingPolicyID: "task-policy", PricingPolicyVersion: "v1", BillingMode: dto.EdgeBillingModeRatioV1,
			GroupRatio: groupRatio, AppliedRatios: map[string]float64{"duration": 2},
			Facts: dto.EdgeBillingFactsV1{TaskQuotaBeforeRatios: &quotaBeforeRatios},
		},
	}

	quota, usage, err := recomputeMasterUsageQuota(policies, 7, event)
	require.NoError(t, err)
	assert.Equal(t, int64(80), quota)
	require.NotNil(t, usage)
	assert.Zero(t, usage.TotalTokens)
}

func TestProcessSettlementCircuitRejectionCommitsOnlyCircuitAndReceipt(t *testing.T) {
	t.Setenv("EDGE_NODE_SETTLEMENT_WINDOW_SECONDS", "60")
	t.Setenv("EDGE_NODE_SETTLEMENT_WINDOW_QUOTA", "200")
	fixture := newMasterSettlementTestFixture(t, "circuit", 5_000, 5_000)
	first := masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false)
	settleMasterBlockForTest(t, fixture, first, "settlement-circuit-first")

	second := masterSettlementBlockForTest(t, fixture, 2, "wallet", 0, false)
	second.Meta.RequestID = "settlement-circuit-rejected"
	second.PreviousBlockID = first.BlockID
	second.PreviousBlockDigest = first.BlockDigest
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &second))
	principal := masterSettlementPrincipalForTest(t, fixture, second, "MDEyMzQ1Njc4OWFiY2RlZg")

	response, err := ProcessSettlementBlock(principal, second, "server-circuit-rejected", fixture.now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, response.StatusCode)
	assert.False(t, response.Replayed)

	var node model.EdgeNode
	require.NoError(t, fixture.db.First(&node, fixture.node.ID).Error)
	assert.True(t, node.SettlementCircuitOpen)
	assert.NotZero(t, node.SettlementCircuitOpenedAt)
	assert.NotEmpty(t, node.SettlementCircuitReason)
	assert.Equal(t, int64(1), node.SettlementCircuitEpoch)
	assert.Equal(t, int64(1), node.LastEventSeq)
	assertMasterSettlementBalances(t, fixture, 4_880, 120, 4_880, 120, 120)

	var blockCount, eventCount, outboxCount int64
	require.NoError(t, fixture.db.Model(&model.EdgeSettlementBlock{}).Count(&blockCount).Error)
	require.NoError(t, fixture.db.Model(&model.EdgeUsageEvent{}).Count(&eventCount).Error)
	require.NoError(t, fixture.db.Model(&model.EdgeConsumeLogOutbox{}).Count(&outboxCount).Error)
	assert.Equal(t, int64(1), blockCount)
	assert.Equal(t, int64(1), eventCount)
	assert.Equal(t, int64(1), outboxCount)
	var receipt model.EdgeRequestReceipt
	require.NoError(t, fixture.db.Where("idempotency_key = ?", second.Meta.RequestID).First(&receipt).Error)
	assert.Equal(t, model.EdgeRequestReceiptStatusRejected, receipt.Status)

	replayPrincipal := masterSettlementPrincipalForTest(t, fixture, second, "ZmVkY2JhOTg3NjU0MzIxMA")
	replayed, err := ProcessSettlementBlock(replayPrincipal, second, "server-circuit-replay", fixture.now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)
	assert.Equal(t, response.StatusCode, replayed.StatusCode)
	assert.Equal(t, response.Body, replayed.Body)

	cleared, err := ClearNodeSettlementCircuit(fixture.node.ID)
	require.NoError(t, err)
	assert.False(t, cleared.SettlementCircuitOpen)
	assert.Equal(t, int64(2), cleared.SettlementCircuitEpoch)
	t.Setenv("EDGE_NODE_SETTLEMENT_WINDOW_QUOTA", "500")
	second.Meta.RequestID = "settlement-circuit-retry"
	retryPrincipal := masterSettlementPrincipalForTest(t, fixture, second, "YWJjZGVmMDEyMzQ1Njc4OQ")
	accepted, err := ProcessSettlementBlock(retryPrincipal, second, "server-circuit-retry", fixture.now.Add(3*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, accepted.StatusCode)
	assertMasterSettlementBalances(t, fixture, 4_760, 240, 4_760, 240, 240)
}

func TestMasterSettlementWindowUsesEventTimeAndOpenLowerBoundary(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "window-boundary", 5_000, 5_000)
	first := masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false)
	settleMasterBlockForTest(t, fixture, first, "settlement-window-first")
	baseFinished := first.Events[0].FinishedAtUnixMilli

	for _, test := range []struct {
		name       string
		finishedAt int64
		wantOpen   bool
	}{
		{name: "inside window", finishedAt: baseFinished + 59_000, wantOpen: true},
		{name: "exact lower boundary excluded", finishedAt: baseFinished + 60_000, wantOpen: false},
		{name: "replay upload burst outside event window", finishedAt: baseFinished + 120_000, wantOpen: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := masterSettlementBlockForTest(t, fixture, 2, "wallet", 0, false).Events[0]
			event.FinishedAtUnixMilli = test.finishedAt
			charges := []masterSettlementCharge{{event: &event, chargedQuota: 120}}
			exceeded, _, err := masterSettlementWindowExceededTx(fixture.db, fixture.node, charges, 60, 200)
			require.NoError(t, err)
			assert.Equal(t, test.wantOpen, exceeded)
		})
	}
}

func TestMasterSettlementConcurrentBlocksChargeOnlyOne(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "concurrent-blocks", 5_000, 5_000)
	requests := []dto.EdgeSettlementBlockRequestV1{
		masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false),
		masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false),
	}
	requests[1].Meta.RequestID = "settlement-concurrent-2"
	requests[1].BlockID = "block-concurrent-2"
	requests[1].Events[0].EventID = "event-concurrent-2"
	requests[1].Events[0].ReservationID = "reservation-concurrent-2"
	requests[1].Events[0].RequestID = "request-concurrent-2"
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &requests[1]))

	start := make(chan struct{})
	results := make(chan error, len(requests))
	var ready sync.WaitGroup
	ready.Add(len(requests))
	for i := range requests {
		request := requests[i]
		go func(index int) {
			ready.Done()
			<-start
			results <- fixture.db.Transaction(func(tx *gorm.DB) error {
				_, err := SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
					Request: request, IdempotencyKey: fmt.Sprintf("settlement-concurrent-%d", index+1),
					RequestHash: strings.Repeat(string(rune('a'+index)), 64), Now: fixture.now.Add(time.Minute),
				})
				return err
			})
		}(i)
	}
	ready.Wait()
	close(start)

	successes := 0
	for range requests {
		if err := <-results; err == nil {
			successes++
		}
	}
	assert.Equal(t, 1, successes)
	assertMasterSettlementBalances(t, fixture, 4_880, 120, 4_880, 120, 120)
	for _, target := range []any{&model.EdgeSettlementBlock{}, &model.EdgeUsageEvent{}, &model.EdgeConsumeLogOutbox{}} {
		var count int64
		require.NoError(t, fixture.db.Model(target).Count(&count).Error)
		assert.Equal(t, int64(1), count)
	}
}

func newMasterSettlementTestFixture(t *testing.T, name string, userQuota int, tokenQuota int) *masterSettlementTestFixture {
	t.Helper()
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:master-settlement-%s?mode=memory&cache=shared&_busy_timeout=30000", name)), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.EdgeNode{}, &model.EdgeNodeCredential{}, &model.User{}, &model.Token{}, &model.Channel{},
		&model.EdgeRequestReceipt{}, &model.EdgeRequestNonceClaim{},
		&model.EdgeCompiledSnapshot{}, &model.EdgeCompiledSnapshotDataset{}, &model.EdgeCompiledSnapshotPage{},
		&model.UserSubscription{}, &model.EdgeSettlementBlock{}, &model.EdgeUsageEvent{}, &model.EdgeConsumeLogOutbox{},
		&model.QuotaData{}, &model.EdgeQuotaDataEvent{}, &model.EdgeQuotaDataBucket{},
	))
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB = previousDB
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	now := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	material, err := edgeauth.EncodePublicKey(publicKey)
	require.NoError(t, err)
	node := &model.EdgeNode{
		NodeUID: "edge." + name, Name: "edge-" + name, Status: model.EdgeNodeStatusActive,
		Generation: 1, ProtocolVersion: dto.EdgeControlProtocolVersionV2,
	}
	require.NoError(t, db.Create(node).Error)
	credential := &model.EdgeNodeCredential{
		CredentialUID: "key-" + name, NodeID: node.ID, Generation: 1, VerifyMaterial: material,
		Status:    model.EdgeNodeCredentialStatusActive,
		NotBefore: time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC).Unix(),
		ExpiresAt: time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC).Unix(),
	}
	require.NoError(t, db.Create(credential).Error)
	user := &model.User{Username: "user-" + name, Password: "password", Status: common.UserStatusEnabled, Quota: userQuota, Group: "default"}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{
		UserId: user.Id, Key: "token-" + name, Status: common.TokenStatusEnabled,
		CreatedTime: now.Unix(), ExpiredTime: -1, RemainQuota: tokenQuota, Group: "default",
	}
	require.NoError(t, db.Create(token).Error)
	channel := &model.Channel{Type: 1, Key: "upstream", Status: common.ChannelStatusEnabled, Name: "channel-" + name, Models: "gpt-test", Group: "default"}
	require.NoError(t, db.Create(channel).Error)
	snapshot := createMasterSettlementSnapshotForTest(t, db, now, user, token, channel)
	return &masterSettlementTestFixture{
		db: db, now: now, node: node, credential: credential,
		identity: &model.EdgeControlIdentity{Node: node, Credential: credential},
		user:     user, token: token, channel: channel, snapshot: snapshot,
	}
}

func masterSettlementPrincipalForTest(t *testing.T, fixture *masterSettlementTestFixture, request dto.EdgeSettlementBlockRequestV1, nonce string) *ControlPrincipal {
	t.Helper()
	body, err := common.Marshal(request)
	require.NoError(t, err)
	signedRequest := edgeauth.Request{Method: http.MethodPost, EscapedPath: "/control/v1/settlement/block", Body: body}
	requestHash, err := edgeauth.IdempotencySHA256(signedRequest)
	require.NoError(t, err)
	metadata := edgeauth.Metadata{
		Version: edgeauth.VersionV1, NodeID: fixture.node.NodeUID, Generation: fixture.node.Generation,
		KeyID: fixture.credential.CredentialUID, TimestampUnixSeconds: time.Now().Unix(),
		Nonce: nonce, IdempotencyKey: request.Meta.RequestID,
	}
	return &ControlPrincipal{
		NodeID: fixture.node.ID, NodeUID: fixture.node.NodeUID, NodeStatus: fixture.node.Status,
		Generation: fixture.node.Generation, CredentialID: fixture.credential.ID,
		CredentialUID: fixture.credential.CredentialUID, CredentialFingerprint: fixture.credential.Fingerprint,
		SignedRequest: &edgeauth.SignedHTTPRequest{Metadata: metadata, Request: signedRequest}, RawBody: body,
		RequestHash: requestHash, NonceHash: edgeauth.BodySHA256([]byte(nonce)),
	}
}

func createMasterSettlementSnapshotForTest(t *testing.T, db *gorm.DB, now time.Time, user *model.User, token *model.Token, channel *model.Channel) *model.EdgeCompiledSnapshot {
	t.Helper()
	snapshot := &model.EdgeCompiledSnapshot{
		SnapshotUID: "snapshot-1", Revision: 1, ProtocolVersion: dto.EdgeControlProtocolVersionV2,
		Status: model.EdgeCompiledSnapshotStatusPublished, CreatedAt: now.Add(-time.Minute).Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(), PublishedAt: now.Add(-time.Minute).Unix(),
	}
	require.NoError(t, db.Session(&gorm.Session{SkipHooks: true}).Create(snapshot).Error)
	modelRatio := 1.0
	completionRatio := 2.0
	cacheRatio := 1.0
	payloads := map[dto.EdgeSnapshotDatasetV1]dto.EdgeSnapshotPagePayloadV1{
		dto.EdgeSnapshotDatasetAuthenticationV1: {Authentication: []dto.EdgeTokenAuthRecordV1{{
			TokenFingerprint: strings.Repeat("a", 64), TokenID: int64(token.Id), UserID: int64(user.Id), Enabled: true, Group: "default",
		}}},
		dto.EdgeSnapshotDatasetUsersV1: {Users: []dto.EdgeUserPolicyV1{{
			UserID: int64(user.Id), Enabled: true, Username: user.Username, DefaultGroup: "default",
			Setting: dto.EdgeUserSettingV1{BillingPreference: "subscription_first"},
		}}},
		dto.EdgeSnapshotDatasetGroupsV1: {Groups: []dto.EdgeGroupPolicyV1{{
			UserGroup: "default", UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}},
		}}},
		dto.EdgeSnapshotDatasetModelsV1: {Models: []dto.EdgeModelPolicyV1{{
			Model: "gpt-test", Enabled: true, Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointOpenAIChatCompletionsV1},
			Streaming: true, ChannelIDs: []int64{int64(channel.Id)},
		}}},
		dto.EdgeSnapshotDatasetChannelsV1: {Channels: []dto.EdgeChannelProjectionV1{{
			ChannelID: int64(channel.Id), Type: 1, Name: "test", Enabled: true,
			Groups: []string{"default"}, Models: []string{"gpt-test"}, Weight: 1,
			LocalService: dto.EdgeLocalServiceCPAPro20x4V1,
		}}},
		dto.EdgeSnapshotDatasetPricingV1: {Pricing: []dto.EdgePricingPolicyV1{{
			PolicyID: "price-gpt-test", Version: "v1", Model: "gpt-test", BillingMode: dto.EdgeBillingModeRatioV1,
			ModelRatio: &modelRatio, CompletionRatio: &completionRatio, CacheReadRatio: &cacheRatio,
			CacheCreationRatio: &cacheRatio, CacheCreation1hRatio: &cacheRatio, QuotaPerUnit: 1,
		}}},
	}
	for _, datasetName := range []dto.EdgeSnapshotDatasetV1{
		dto.EdgeSnapshotDatasetAuthenticationV1, dto.EdgeSnapshotDatasetUsersV1, dto.EdgeSnapshotDatasetGroupsV1,
		dto.EdgeSnapshotDatasetModelsV1, dto.EdgeSnapshotDatasetChannelsV1, dto.EdgeSnapshotDatasetPricingV1,
	} {
		payload := payloads[datasetName]
		require.NoError(t, payload.Validate(datasetName, 1))
		payloadBytes, err := common.Marshal(payload)
		require.NoError(t, err)
		dataset := &model.EdgeCompiledSnapshotDataset{SnapshotID: snapshot.ID, Dataset: datasetName, Revision: 1, ItemCount: 1, PageCount: 1}
		require.NoError(t, db.Session(&gorm.Session{SkipHooks: true}).Create(dataset).Error)
		require.NoError(t, db.Session(&gorm.Session{SkipHooks: true}).Create(&model.EdgeCompiledSnapshotPage{
			DatasetID: dataset.ID, Ordinal: 0, ItemCount: 1, Payload: string(payloadBytes),
		}).Error)
	}
	return snapshot
}

func masterSettlementBlockForTest(
	t *testing.T,
	fixture *masterSettlementTestFixture,
	sequence int64,
	fundingSource string,
	userSubscriptionID int64,
	tokenUnlimited bool,
) dto.EdgeSettlementBlockRequestV1 {
	t.Helper()
	status := 200
	request := dto.EdgeSettlementBlockRequestV1{
		Meta:    dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV2, RequestID: fmt.Sprintf("settlement-%d", sequence)},
		BlockID: fmt.Sprintf("block-%d", sequence), FirstSequence: sequence, LastSequence: sequence,
		CreatedAtUnixMilli: fixture.now.Add(30 * time.Second).UnixMilli(), BlockDigest: strings.Repeat("9", 64),
		Events: []dto.EdgeUsageEventV1{{
			EventID: fmt.Sprintf("event-%d", sequence), Sequence: sequence,
			ReservationID: fmt.Sprintf("reservation-%d", sequence), RequestID: fmt.Sprintf("request-%d", sequence),
			UserID: int64(fixture.user.Id), TokenID: int64(fixture.token.Id),
			SnapshotID: fixture.snapshot.SnapshotUID, SnapshotRevision: fixture.snapshot.Revision,
			PricingRevision: 1, BalanceRevision: 1, FundingSource: fundingSource,
			UserSubscriptionID: userSubscriptionID, TokenUnlimitedQuota: tokenUnlimited,
			ChannelID: int64(fixture.channel.Id), Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1,
			Model: "gpt-test", Group: "default", StartedAtUnixMilli: fixture.now.Add(10 * time.Second).UnixMilli(),
			FinishedAtUnixMilli: fixture.now.Add(20 * time.Second).UnixMilli(), Outcome: dto.EdgeUsageOutcomeSuccessV1,
			HTTPStatus: &status, Usage: dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}),
			Billing: dto.EdgeUsageBillingV1{
				PricingPolicyID: "price-gpt-test", PricingPolicyVersion: "v1", BillingMode: dto.EdgeBillingModeRatioV1,
				GroupRatio: 1, ReservedQuota: 100, ChargedQuota: 120,
			},
		}},
	}
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &request))
	return request
}

func settleMasterBlockForTest(t *testing.T, fixture *masterSettlementTestFixture, request dto.EdgeSettlementBlockRequestV1, idempotencyKey string) *dto.EdgeSettlementAckV1 {
	t.Helper()
	var ack *dto.EdgeSettlementAckV1
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		var err error
		ack, err = SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: request, IdempotencyKey: idempotencyKey,
			RequestHash: strings.Repeat("a", 64), Now: fixture.now.Add(time.Minute),
		})
		return err
	}))
	require.NotNil(t, ack)
	return ack
}

func assertMasterSettlementBalances(t *testing.T, fixture *masterSettlementTestFixture, userQuota int, userUsed int, tokenRemain int, tokenUsed int, channelUsed int64) {
	t.Helper()
	var user model.User
	var token model.Token
	var channel model.Channel
	require.NoError(t, fixture.db.First(&user, fixture.user.Id).Error)
	require.NoError(t, fixture.db.First(&token, fixture.token.Id).Error)
	require.NoError(t, fixture.db.First(&channel, fixture.channel.Id).Error)
	assert.Equal(t, userQuota, user.Quota)
	assert.Equal(t, userUsed, user.UsedQuota)
	assert.Equal(t, tokenRemain, token.RemainQuota)
	assert.Equal(t, tokenUsed, token.UsedQuota)
	assert.Equal(t, channelUsed, channel.UsedQuota)
}
