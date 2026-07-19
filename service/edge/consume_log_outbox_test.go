package edge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgesettlement"
	coreservice "github.com/QuantumNous/new-api/service"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestPublishMasterConsumeLogOutboxSupportsSharedAndSeparateLogDB(t *testing.T) {
	for _, separateLogDB := range []bool{false, true} {
		name := "shared"
		if separateLogDB {
			name = "separate"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newMasterSettlementTestFixture(t, "consume-log-"+name, 5_000, 5_000)
			logDB := configureConsumeLogFixture(t, fixture, separateLogDB)

			outbox := settleConsumeLogEventForTest(t, fixture)
			expectedKey, err := model.EdgeConsumeLogBillingEventKey(fixture.node.NodeUID, fixture.node.Generation, "event-1")
			require.NoError(t, err)
			assert.Equal(t, expectedKey, outbox.EventUID)

			processed, err := PublishMasterConsumeLogOutboxBatch(context.Background(), fixture.now.Add(2*time.Minute), 10)
			require.NoError(t, err)
			assert.Equal(t, 1, processed)

			var storedLog model.Log
			require.NoError(t, logDB.Where("billing_event_key = ?", expectedKey).First(&storedLog).Error)
			require.NotNil(t, storedLog.BillingEventKey)
			assert.Equal(t, expectedKey, *storedLog.BillingEventKey)
			assert.Equal(t, fixture.user.Id, storedLog.UserId)
			assert.Equal(t, fixture.token.Id, storedLog.TokenId)
			assert.Equal(t, fixture.channel.Id, storedLog.ChannelId)
			assert.Equal(t, 120, storedLog.Quota)
			assert.Equal(t, "request-1", storedLog.RequestId)
			var other map[string]interface{}
			require.NoError(t, common.UnmarshalJsonStr(storedLog.Other, &other))
			assert.NotContains(t, other, "frt")

			require.NoError(t, fixture.db.First(&outbox, outbox.ID).Error)
			assert.Equal(t, model.EdgeConsumeLogOutboxStatusPublished, outbox.Status)
			assert.NotZero(t, outbox.PublishedAt)
			var quotaData model.QuotaData
			require.NoError(t, fixture.db.First(&quotaData).Error)
			assert.Equal(t, 1, quotaData.Count)
			assert.Equal(t, 120, quotaData.Quota)
			assert.Equal(t, 110, quotaData.TokenUsed)
			assert.Equal(t, fixture.node.NodeUID, quotaData.NodeName)

			processed, err = PublishMasterConsumeLogOutboxBatch(context.Background(), fixture.now.Add(3*time.Minute), 10)
			require.NoError(t, err)
			assert.Zero(t, processed)
		})
	}
}

func TestPublishMasterConsumeLogOutboxProjectsFirstResponseTime(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "consume-log-frt", 5_000, 5_000)
	logDB := configureConsumeLogFixture(t, fixture, false)
	request := masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false)
	firstResponseAt := request.Events[0].StartedAtUnixMilli + 750
	request.Events[0].FirstResponseAtUnixMilli = &firstResponseAt
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &request))
	settleMasterBlockForTest(t, fixture, request, "consume-log-frt-settlement")

	processed, err := PublishMasterConsumeLogOutboxBatch(context.Background(), fixture.now.Add(2*time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	var storedLog model.Log
	require.NoError(t, logDB.Where("request_id = ?", "request-1").First(&storedLog).Error)
	var other map[string]interface{}
	require.NoError(t, common.UnmarshalJsonStr(storedLog.Other, &other))
	assert.Equal(t, float64(750), other["frt"])
}

func TestPublishMasterConsumeLogOutboxUsesRequestSnapshotAndAuthoritativeSubscriptionFinalization(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "consume-log-rich-snapshot", 5_000, 5_000)
	logDB := configureConsumeLogFixture(t, fixture, false)
	require.NoError(t, fixture.db.AutoMigrate(&model.SubscriptionPlan{}))
	plan := &model.SubscriptionPlan{Title: "Pro", TotalAmount: 1_000, Enabled: true}
	require.NoError(t, fixture.db.Create(plan).Error)
	subscription := &model.UserSubscription{
		UserId: fixture.user.Id, PlanId: plan.Id, AmountTotal: 1_000, AmountUsed: 100,
		StartTime: fixture.now.Add(-time.Hour).Unix(), EndTime: fixture.now.Add(time.Hour).Unix(), Status: "active",
	}
	require.NoError(t, fixture.db.Create(subscription).Error)

	request := masterSettlementBlockForTest(t, fixture, 1, "subscription", int64(subscription.Id), false)
	request.Events[0].Billing.ReservedQuota = 50
	useTime := int64(7)
	request.Events[0].ConsumeLogSnapshot = &dto.EdgeConsumeLogSnapshotV1{
		Username: "request-user", TokenName: "request-token", ModelName: "gpt-4o-gizmo-*",
		Content: "request content", UseTimeSeconds: &useTime, IP: "203.0.113.10",
		RequestID: "request-visible-1", UpstreamRequestID: "upstream-1",
		Other: map[string]interface{}{
			"request_path":      "/v1/chat/completions",
			"model_ratio":       float64(1),
			"stream_status":     map[string]interface{}{"status": "error", "end_reason": "client_gone"},
			"admin_info":        map[string]interface{}{"usage_billing_path": "billing-usage-openai"},
			"future_auto_carry": map[string]interface{}{"nested": "preserved"},
		},
	}
	firstResponseAt := request.Events[0].StartedAtUnixMilli + 750
	request.Events[0].FirstResponseAtUnixMilli = &firstResponseAt
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &request))
	settleMasterBlockForTest(t, fixture, request, "consume-log-rich-snapshot")

	require.NoError(t, fixture.db.Model(&model.User{}).Where("id = ?", fixture.user.Id).Update("username", "renamed-user").Error)
	require.NoError(t, fixture.db.Model(&model.Token{}).Where("id = ?", fixture.token.Id).Update("name", "renamed-token").Error)
	processed, err := PublishMasterConsumeLogOutboxBatch(context.Background(), fixture.now.Add(2*time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	var storedLog model.Log
	require.NoError(t, logDB.Where("request_id = ?", "request-visible-1").First(&storedLog).Error)
	assert.Equal(t, "request-user", storedLog.Username)
	assert.Equal(t, "request-token", storedLog.TokenName)
	assert.Equal(t, "gpt-4o-gizmo-*", storedLog.ModelName)
	assert.Equal(t, "request content", storedLog.Content)
	assert.Equal(t, 7, storedLog.UseTime)
	assert.Equal(t, "203.0.113.10", storedLog.Ip)
	assert.Equal(t, "upstream-1", storedLog.UpstreamRequestId)

	var other map[string]interface{}
	require.NoError(t, common.UnmarshalJsonStr(storedLog.Other, &other))
	assert.Equal(t, float64(1), other["model_ratio"])
	assert.Equal(t, float64(750), other["frt"])
	assert.Equal(t, "subscription", other["billing_source"])
	assert.Equal(t, "subscription_first", other["billing_preference"])
	assert.Equal(t, float64(subscription.Id), other["subscription_id"])
	assert.Equal(t, float64(50), other["subscription_pre_consumed"])
	assert.Equal(t, float64(70), other["subscription_post_delta"])
	assert.Equal(t, float64(plan.Id), other["subscription_plan_id"])
	assert.Equal(t, "Pro", other["subscription_plan_title"])
	assert.Equal(t, float64(1_000), other["subscription_total"])
	assert.Equal(t, float64(220), other["subscription_used"])
	assert.Equal(t, float64(780), other["subscription_remain"])
	assert.Equal(t, float64(120), other["subscription_consumed"])
	assert.Equal(t, float64(0), other["wallet_quota_deducted"])
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "billing-usage-openai", adminInfo["usage_billing_path"])
	edgeSettlement, ok := adminInfo["edge_settlement"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, edgeSettlement["edge_settlement"])
	delete(adminInfo, "edge_settlement")
	expectedSnapshot, err := coreservice.FinalizeTextConsumeLogSnapshot(request.Events[0].ConsumeLogSnapshot, coreservice.TextConsumeLogSettlementFacts{
		BillingSource: "subscription", BillingPreference: "subscription_first",
		SubscriptionID: subscription.Id, SubscriptionPreConsumed: 50, SubscriptionPostDelta: 70,
		SubscriptionPlanID: plan.Id, SubscriptionPlanTitle: "Pro", SubscriptionTotal: 1_000, SubscriptionUsed: 220,
	})
	require.NoError(t, err)
	expectedSnapshot.Other["frt"] = float64(750)
	expectedJSON, err := common.Marshal(expectedSnapshot.Other)
	require.NoError(t, err)
	var normalizedExpected map[string]interface{}
	require.NoError(t, common.Unmarshal(expectedJSON, &normalizedExpected))
	assert.Equal(t, normalizedExpected, other, "all shared other fields must carry automatically; only edge admin metadata is allowlisted")
}

func TestPublishMasterConsumeLogOutboxProjectsZeroUsageRecord(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "consume-log-zero-usage", 5_000, 5_000)
	logDB := configureConsumeLogFixture(t, fixture, false)
	request := masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false)
	request.Events[0].Usage = &dto.BillingUsage{
		Source: dto.BillingUsageSourceOAIChat, Semantic: dto.BillingUsageSemanticOpenAI, OpenAIUsage: &dto.Usage{},
	}
	request.Events[0].Billing.ReservedQuota = 0
	request.Events[0].Billing.ChargedQuota = 0
	useTime := int64(1)
	request.Events[0].ConsumeLogSnapshot = &dto.EdgeConsumeLogSnapshotV1{
		Username: fixture.user.Username, TokenName: fixture.token.Name, ModelName: "gpt-test",
		Content: "上游没有返回计费信息，无法扣费（可能是上游超时）", UseTimeSeconds: &useTime,
		RequestID: "zero-visible", Other: map[string]interface{}{"request_path": "/v1/chat/completions"},
	}
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &request))
	settleMasterBlockForTest(t, fixture, request, "consume-log-zero-usage")

	processed, err := PublishMasterConsumeLogOutboxBatch(context.Background(), fixture.now.Add(2*time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	var storedLog model.Log
	require.NoError(t, logDB.Where("request_id = ?", "zero-visible").First(&storedLog).Error)
	assert.Zero(t, storedLog.Quota)
	assert.Zero(t, storedLog.PromptTokens)
	assert.Zero(t, storedLog.CompletionTokens)
	assert.Equal(t, request.Events[0].ConsumeLogSnapshot.Content, storedLog.Content)
	var quotaData model.QuotaData
	require.NoError(t, fixture.db.First(&quotaData).Error)
	assert.Equal(t, 1, quotaData.Count)
	assert.Zero(t, quotaData.Quota)
	assert.Zero(t, quotaData.TokenUsed)
}

func TestPublishMasterConsumeLogOutboxProjectsFixedPriceWalletLog(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "consume-log-fixed-wallet", 20_000, 20_000)
	logDB := configureConsumeLogFixture(t, fixture, false)
	var pricingDataset model.EdgeCompiledSnapshotDataset
	require.NoError(t, fixture.db.Where("snapshot_id = ? AND dataset = ?", fixture.snapshot.ID, dto.EdgeSnapshotDatasetPricingV1).First(&pricingDataset).Error)
	modelPrice := 0.02
	pricingPayload := dto.EdgeSnapshotPagePayloadV1{Pricing: []dto.EdgePricingPolicyV1{{
		PolicyID: "price-gpt-test", Version: "v1", Model: "gpt-test", BillingMode: dto.EdgeBillingModeFixedPriceV1,
		ModelPrice: &modelPrice, QuotaPerUnit: 500_000,
	}}}
	require.NoError(t, pricingPayload.Validate(dto.EdgeSnapshotDatasetPricingV1, 1))
	payloadJSON, err := common.Marshal(pricingPayload)
	require.NoError(t, err)
	require.NoError(t, fixture.db.Session(&gorm.Session{SkipHooks: true}).Model(&model.EdgeCompiledSnapshotPage{}).
		Where("dataset_id = ?", pricingDataset.ID).Update("payload", string(payloadJSON)).Error)

	request := masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false)
	request.Events[0].Usage = dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})
	request.Events[0].Billing.BillingMode = dto.EdgeBillingModeFixedPriceV1
	request.Events[0].Billing.ReservedQuota = 10_000
	request.Events[0].Billing.ChargedQuota = 10_000
	useTime := int64(2)
	request.Events[0].ConsumeLogSnapshot = &dto.EdgeConsumeLogSnapshotV1{
		Username: fixture.user.Username, TokenName: "fixed-token", ModelName: "gpt-test", UseTimeSeconds: &useTime,
		RequestID: "fixed-visible", Other: map[string]interface{}{"model_price": modelPrice, "request_path": "/v1/chat/completions"},
	}
	require.NoError(t, edgesettlement.SetBlockDigestV1(fixture.node.NodeUID, fixture.node.Generation, &request))
	settleMasterBlockForTest(t, fixture, request, "consume-log-fixed-wallet")

	processed, err := PublishMasterConsumeLogOutboxBatch(context.Background(), fixture.now.Add(2*time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	var storedLog model.Log
	require.NoError(t, logDB.Where("request_id = ?", "fixed-visible").First(&storedLog).Error)
	assert.Equal(t, 10_000, storedLog.Quota)
	assert.Equal(t, "fixed-token", storedLog.TokenName)
	var other map[string]interface{}
	require.NoError(t, common.UnmarshalJsonStr(storedLog.Other, &other))
	assert.Equal(t, modelPrice, other["model_price"])
	assert.Equal(t, "wallet", other["billing_source"])
	assert.Equal(t, "subscription_first", other["billing_preference"])
	assert.NotContains(t, other, "wallet_quota_deducted")
}

func TestPublishMasterConsumeLogOutboxCrashRecoveryDoesNotDuplicateLog(t *testing.T) {
	for _, separateLogDB := range []bool{false, true} {
		name := "shared"
		if separateLogDB {
			name = "separate"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newMasterSettlementTestFixture(t, "consume-log-crash-"+name, 5_000, 5_000)
			logDB := configureConsumeLogFixture(t, fixture, separateLogDB)
			settleConsumeLogEventForTest(t, fixture)

			ctx := context.Background()
			claimTime := fixture.now.Add(2 * time.Minute)
			firstClaim, err := model.ClaimEdgeConsumeLogOutbox(ctx, claimTime, time.Second)
			require.NoError(t, err)
			require.NoError(t, publishMasterConsumeLogClaim(ctx, firstClaim))

			// Simulate a crash after both durable projections committed but before
			// the main-database outbox claim was acknowledged. A mutable username
			// change during recovery must not turn the replay into a conflict.
			renamedUsername := "renamed-" + name
			require.NoError(t, fixture.db.Model(&model.User{}).Where("id = ?", fixture.user.Id).
				Update("username", renamedUsername).Error)
			recoveredClaim, err := model.ClaimEdgeConsumeLogOutbox(ctx, claimTime.Add(2*time.Second), time.Second)
			require.NoError(t, err)
			assert.ErrorIs(t,
				model.MarkEdgeConsumeLogOutboxPublished(ctx, firstClaim, claimTime.Add(2*time.Second)),
				model.ErrEdgeConsumeLogOutboxClaimLost,
			)
			require.NoError(t, publishMasterConsumeLogClaim(ctx, recoveredClaim))
			require.NoError(t, model.MarkEdgeConsumeLogOutboxPublished(ctx, recoveredClaim, claimTime.Add(2*time.Second)))

			var count int64
			require.NoError(t, logDB.Model(&model.Log{}).Where("request_id = ?", "request-1").Count(&count).Error)
			assert.Equal(t, int64(1), count)
			require.NoError(t, fixture.db.Model(&model.EdgeQuotaDataEvent{}).Count(&count).Error)
			assert.Equal(t, int64(1), count)
			var quotaData model.QuotaData
			require.NoError(t, fixture.db.First(&quotaData).Error)
			assert.Equal(t, 1, quotaData.Count)
			assert.Equal(t, 120, quotaData.Quota)
			assert.Equal(t, 110, quotaData.TokenUsed)
			assert.Equal(t, fixture.user.Username, quotaData.Username)
		})
	}
}

func TestPublishMasterConsumeLogOutboxCrashAfterLogWriteProjectsQuotaOnce(t *testing.T) {
	for _, separateLogDB := range []bool{false, true} {
		name := "shared"
		if separateLogDB {
			name = "separate"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newMasterSettlementTestFixture(t, "consume-log-after-log-"+name, 5_000, 5_000)
			logDB := configureConsumeLogFixture(t, fixture, separateLogDB)
			outbox := settleConsumeLogEventForTest(t, fixture)

			ctx := context.Background()
			claimTime := fixture.now.Add(2 * time.Minute)
			firstClaim, err := model.ClaimEdgeConsumeLogOutbox(ctx, claimTime, time.Second)
			require.NoError(t, err)
			var stored model.EdgeUsageEvent
			require.NoError(t, fixture.db.First(&stored, outbox.EventID).Error)
			inserted, err := model.CreateEdgeConsumeLogOnce(ctx, &model.Log{
				UserId: stored.UserID, Username: fixture.user.Username, CreatedAt: stored.FinishedAtUnixMilli / 1000,
				Type: model.LogTypeConsume, PromptTokens: stored.PromptTokens, CompletionTokens: stored.CompletionTokens,
				ModelName: stored.Model, Quota: int(stored.ChargedQuota), ChannelId: stored.ChannelID,
				TokenId: stored.TokenID, UseTime: int((stored.FinishedAtUnixMilli - stored.StartedAtUnixMilli) / 1000),
				IsStream: stored.Streaming, Group: stored.Group, RequestId: stored.RequestUID,
			}, outbox.EventUID)
			require.NoError(t, err)
			assert.True(t, inserted)
			var count int64
			require.NoError(t, fixture.db.Model(&model.EdgeQuotaDataEvent{}).Count(&count).Error)
			assert.Zero(t, count)

			// LOG_DB committed, but the worker crashed before quota_data and outbox
			// acknowledgement. Recovery must deduplicate the log and project once.
			recoveredClaim, err := model.ClaimEdgeConsumeLogOutbox(ctx, claimTime.Add(2*time.Second), time.Second)
			require.NoError(t, err)
			assert.ErrorIs(t,
				model.MarkEdgeConsumeLogOutboxPublished(ctx, firstClaim, claimTime.Add(2*time.Second)),
				model.ErrEdgeConsumeLogOutboxClaimLost,
			)
			require.NoError(t, publishMasterConsumeLogClaim(ctx, recoveredClaim))
			require.NoError(t, model.MarkEdgeConsumeLogOutboxPublished(ctx, recoveredClaim, claimTime.Add(2*time.Second)))

			require.NoError(t, logDB.Model(&model.Log{}).Where("billing_event_key = ?", outbox.EventUID).Count(&count).Error)
			assert.Equal(t, int64(1), count)
			require.NoError(t, fixture.db.Model(&model.EdgeQuotaDataEvent{}).Count(&count).Error)
			assert.Equal(t, int64(1), count)
			var quotaData model.QuotaData
			require.NoError(t, fixture.db.First(&quotaData).Error)
			assert.Equal(t, 1, quotaData.Count)
			assert.Equal(t, 120, quotaData.Quota)
			assert.Equal(t, 110, quotaData.TokenUsed)
		})
	}
}

func TestPublishMasterConsumeLogOutboxUpgradesLegacyRawEventIdentity(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "consume-log-legacy", 5_000, 5_000)
	require.NoError(t, fixture.db.AutoMigrate(&model.Log{}))
	configureConsumeLogTestDB(t, fixture.db)
	outbox := settleConsumeLogEventForTest(t, fixture)
	require.NoError(t, fixture.db.Model(&model.EdgeConsumeLogOutbox{}).
		Where("id = ?", outbox.ID).
		UpdateColumn("event_uid", "event-1").Error)

	processed, err := PublishMasterConsumeLogOutboxBatch(context.Background(), fixture.now.Add(2*time.Minute), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	expectedKey, err := model.EdgeConsumeLogBillingEventKey(fixture.node.NodeUID, fixture.node.Generation, "event-1")
	require.NoError(t, err)
	var count int64
	require.NoError(t, fixture.db.Model(&model.Log{}).Where("billing_event_key = ?", expectedKey).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestPublishMasterConsumeLogOutboxRejectsTamperedProjection(t *testing.T) {
	fixture := newMasterSettlementTestFixture(t, "consume-log-tampered", 5_000, 5_000)
	require.NoError(t, fixture.db.AutoMigrate(&model.Log{}))
	configureConsumeLogTestDB(t, fixture.db)
	outbox := settleConsumeLogEventForTest(t, fixture)

	var payload edgeConsumeLogOutboxPayload
	require.NoError(t, common.UnmarshalJsonStr(outbox.Payload, &payload))
	payload.Quota++
	tampered, err := common.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, fixture.db.Model(&model.EdgeConsumeLogOutbox{}).
		Where("id = ?", outbox.ID).
		UpdateColumn("payload", string(tampered)).Error)

	processed, err := PublishMasterConsumeLogOutboxBatch(context.Background(), fixture.now.Add(2*time.Minute), 1)
	require.Error(t, err)
	assert.Equal(t, 1, processed)

	require.NoError(t, fixture.db.First(&outbox, outbox.ID).Error)
	assert.Equal(t, model.EdgeConsumeLogOutboxStatusFailed, outbox.Status)
	assert.Contains(t, outbox.LastError, "does not match the authoritative usage event")
	var count int64
	require.NoError(t, fixture.db.Model(&model.Log{}).Count(&count).Error)
	assert.Zero(t, count)
}

func configureConsumeLogTestDB(t *testing.T, logDB *gorm.DB) {
	t.Helper()
	previousLogDB := model.LOG_DB
	previousLogType := common.LogDatabaseType()
	previousLogConsumeEnabled := common.LogConsumeEnabled
	previousDataExportEnabled := common.DataExportEnabled
	model.LOG_DB = logDB
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)
	common.LogConsumeEnabled = true
	common.DataExportEnabled = true
	t.Cleanup(func() {
		model.LOG_DB = previousLogDB
		common.SetLogDatabaseType(previousLogType)
		common.LogConsumeEnabled = previousLogConsumeEnabled
		common.DataExportEnabled = previousDataExportEnabled
		if logDB != model.DB {
			if sqlDB, err := logDB.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
	})
}

func configureConsumeLogFixture(t *testing.T, fixture *masterSettlementTestFixture, separateLogDB bool) *gorm.DB {
	t.Helper()
	logDB := fixture.db
	if separateLogDB {
		var err error
		logDB, err = gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "logs.db")+"?_busy_timeout=30000"), &gorm.Config{})
		require.NoError(t, err)
	}
	require.NoError(t, logDB.AutoMigrate(&model.Log{}))
	configureConsumeLogTestDB(t, logDB)
	return logDB
}

func settleConsumeLogEventForTest(t *testing.T, fixture *masterSettlementTestFixture) model.EdgeConsumeLogOutbox {
	t.Helper()
	request := masterSettlementBlockForTest(t, fixture, 1, "wallet", 0, false)
	settleMasterBlockForTest(t, fixture, request, "consume-log-settlement")
	var outbox model.EdgeConsumeLogOutbox
	require.NoError(t, fixture.db.First(&outbox).Error)
	return outbox
}
