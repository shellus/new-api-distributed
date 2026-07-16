package edge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

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
			fixture := newMasterLeaseTestFixture(t, "consume-log-"+name, "wallet_only", 5_000, 5_000, 10_000)
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

func TestPublishMasterConsumeLogOutboxCrashRecoveryDoesNotDuplicateLog(t *testing.T) {
	for _, separateLogDB := range []bool{false, true} {
		name := "shared"
		if separateLogDB {
			name = "separate"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newMasterLeaseTestFixture(t, "consume-log-crash-"+name, "wallet_only", 5_000, 5_000, 10_000)
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
			fixture := newMasterLeaseTestFixture(t, "consume-log-after-log-"+name, "wallet_only", 5_000, 5_000, 10_000)
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
	fixture := newMasterLeaseTestFixture(t, "consume-log-legacy", "wallet_only", 5_000, 5_000, 10_000)
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
	fixture := newMasterLeaseTestFixture(t, "consume-log-tampered", "wallet_only", 5_000, 5_000, 10_000)
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

func configureConsumeLogFixture(t *testing.T, fixture *masterLeaseTestFixture, separateLogDB bool) *gorm.DB {
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

func settleConsumeLogEventForTest(t *testing.T, fixture *masterLeaseTestFixture) model.EdgeConsumeLogOutbox {
	t.Helper()
	lease := acquireMasterLeaseForTest(t, fixture, "consume-log-lease", 200, 200)
	request := masterSettlementBlockForTest(t, fixture, lease, 1, 120)
	require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
		_, err := SettleMasterUsageBlockTx(tx, fixture.identity, MasterSettlementCommand{
			Request: request, IdempotencyKey: "consume-log-settlement", RequestHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Now: fixture.now.Add(time.Minute),
		})
		return err
	}))
	var outbox model.EdgeConsumeLogOutbox
	require.NoError(t, fixture.db.First(&outbox).Error)
	return outbox
}
