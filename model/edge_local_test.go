package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgetoken"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const edgeLocalTestNow = int64(1_784_160_000_000)

func TestOpenEdgeSQLiteUsesDedicatedDurableSchema(t *testing.T) {
	db := openEdgeLocalTestDB(t, "schema.db")

	var journalMode string
	require.NoError(t, db.Raw("PRAGMA journal_mode").Scan(&journalMode).Error)
	assert.Equal(t, "wal", strings.ToLower(journalMode))
	var busyTimeout int
	require.NoError(t, db.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error)
	assert.GreaterOrEqual(t, busyTimeout, edgeSQLiteBusyTimeoutMilliseconds)
	var foreignKeys int
	require.NoError(t, db.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error)
	assert.Zero(t, foreignKeys)

	var tableNames []string
	require.NoError(t, db.Raw("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name").Scan(&tableNames).Error)
	assert.Contains(t, tableNames, "channels")
	assert.Contains(t, tableNames, "abilities")
	assert.Contains(t, tableNames, "logs")
	assert.Contains(t, tableNames, "edge_local_balance_accounts")
	assert.NotContains(t, tableNames, "edge_local_quota_leases")
	assert.NotContains(t, tableNames, "edge_local_lease_acquire_intents")
	assert.NotContains(t, tableNames, "users")
	assert.NotContains(t, tableNames, "tokens")
	assert.NotContains(t, tableNames, "options")

	rows, err := db.Raw("PRAGMA foreign_key_list(edge_local_quota_reservations)").Rows()
	require.NoError(t, err)
	defer rows.Close()
	assert.False(t, rows.Next())
}

func TestGetEdgeLocalPendingSettlementBlockReturnsNilForEmptyQueue(t *testing.T) {
	db := openEdgeLocalTestDB(t, "empty-settlement-queue.db")

	block, err := GetEdgeLocalPendingSettlementBlock(db)
	require.NoError(t, err)
	assert.Nil(t, block)
}

func TestInitEdgeDBIgnoresMasterDSNAndDisablesSharedCaches(t *testing.T) {
	previousDB := DB
	previousLogDB := LOG_DB
	previousRedis := common.RedisEnabled
	previousBatch := common.BatchUpdateEnabled
	previousMemoryCache := common.MemoryCacheEnabled
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	t.Cleanup(func() {
		if DB != nil && DB != previousDB {
			if sqlDB, err := DB.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
		DB = previousDB
		LOG_DB = previousLogDB
		common.RedisEnabled = previousRedis
		common.BatchUpdateEnabled = previousBatch
		common.MemoryCacheEnabled = previousMemoryCache
		common.SetDatabaseTypes(previousMainType, previousLogType)
		initCol()
	})

	t.Setenv("SQL_DSN", "postgres://must-not-be-used.invalid/master")
	t.Setenv("LOG_SQL_DSN", "clickhouse://must-not-be-used.invalid/log")
	t.Setenv("EDGE_SQLITE_PATH", filepath.Join(t.TempDir(), "edge.db"))
	common.RedisEnabled = true
	common.BatchUpdateEnabled = true
	common.MemoryCacheEnabled = true

	require.NoError(t, InitEdgeDB())
	require.NotNil(t, DB)
	assert.Same(t, DB, LOG_DB)
	assert.Equal(t, "sqlite", DB.Dialector.Name())
	assert.False(t, common.RedisEnabled)
	assert.False(t, common.BatchUpdateEnabled)
	assert.False(t, common.MemoryCacheEnabled)
	assert.False(t, DB.Migrator().HasTable(&Token{}))
}

func TestApplyEdgeLocalSnapshotMergesLocalChannelYAMLAndTextPolicy(t *testing.T) {
	db := openEdgeLocalTestDB(t, "cpa-config.db")
	writeEdgeLocalTestChannelConfig(t, "edge-channel", `name: edge-channel
type: openai
base_url: http://test-cpa-x4.internal:9317
auth: test-key-x4
channel_setting:
  proxy: http://edge-proxy.internal:8080
settings:
  allow_speed: false
`)
	writeEdgeLocalTestChannelConfig(t, "edge-channel-x5", `name: edge-channel-x5
type: openai
base_url: https://test-cpa-x5.invalid
auth: test-key-x5
`)
	writeEdgeLocalTestChannelConfig(t, "edge-channel-x6", `name: edge-channel-x6
type: openai
base_url: http://test-cpa-x6.internal:8317/
auth: test-key-x6
`)
	writeEdgeLocalTestChannelConfig(t, "edge-channel-vip", `name: edge-channel-vip
type: openai
base_url: http://test-cpa-vip.internal:8317
auth: test-key-vip
`)
	snapshot := edgeLocalTestSnapshot(7)
	snapshot.Models[0].ChannelIDs = []int64{31, 32, 33, 34}
	snapshot.Channels[0].TextPolicy = dto.EdgeTextRequestPolicyV1{
		ForceFormat: true, ThinkingToContent: true, PassThroughBodyEnabled: true,
		SystemPrompt: "edge test prompt", SystemPromptOverride: true,
		AllowServiceTier: true, AllowInferenceGeo: true, AllowSpeed: true,
		DisableStore: true, AllowSafetyIdentifier: true, AllowIncludeObfuscation: true,
	}
	snapshot.Channels = append(snapshot.Channels,
		dto.EdgeChannelProjectionV1{
			ChannelID: 32, Type: 1, Name: "edge-channel-x5", Enabled: true,
			Groups: []string{"default"}, Models: []string{"gpt-4o-mini"},
			Priority: 9, Weight: 90, LocalService: dto.EdgeLocalServiceCPAPro20x5V1,
		},
		dto.EdgeChannelProjectionV1{
			ChannelID: 33, Type: 1, Name: "edge-channel-x6", Enabled: true,
			Groups: []string{"default"}, Models: []string{"gpt-4o-mini"},
			Priority: 8, Weight: 80, LocalService: dto.EdgeLocalServiceCPAPro20x6V1,
		},
		dto.EdgeChannelProjectionV1{
			ChannelID: 34, Type: 1, Name: "edge-channel-vip", Enabled: true,
			Groups: []string{"default"}, Models: []string{"gpt-4o-mini"},
			Priority: 7, Weight: 70, LocalService: dto.EdgeLocalServiceCPAVIPV1,
		},
	)
	require.NoError(t, ApplyEdgeLocalSnapshot(db, snapshot))

	for _, expected := range []struct {
		channelID int
		baseURL   string
		key       string
	}{
		{channelID: 31, baseURL: "http://test-cpa-x4.internal:9317", key: "test-key-x4"},
		{channelID: 32, baseURL: "https://test-cpa-x5.invalid", key: "test-key-x5"},
		{channelID: 33, baseURL: "http://test-cpa-x6.internal:8317", key: "test-key-x6"},
		{channelID: 34, baseURL: "http://test-cpa-vip.internal:8317", key: "test-key-vip"},
	} {
		var channel Channel
		require.NoError(t, db.First(&channel, expected.channelID).Error)
		assert.Equal(t, common.ChannelStatusEnabled, channel.Status)
		assert.Equal(t, expected.key, channel.Key)
		require.NotNil(t, channel.BaseURL)
		assert.Equal(t, expected.baseURL, *channel.BaseURL)
		var ability Ability
		require.NoError(t, db.Where("channel_id = ?", expected.channelID).First(&ability).Error)
		assert.True(t, ability.Enabled)
	}
	var textChannel Channel
	require.NoError(t, db.First(&textChannel, 31).Error)
	setting := textChannel.GetSetting()
	assert.True(t, setting.ForceFormat)
	assert.True(t, setting.ThinkingToContent)
	assert.True(t, setting.PassThroughBodyEnabled)
	assert.Equal(t, "edge test prompt", setting.SystemPrompt)
	assert.True(t, setting.SystemPromptOverride)
	assert.Equal(t, "http://edge-proxy.internal:8080", setting.Proxy)
	other := textChannel.GetOtherSettings()
	assert.True(t, other.AllowServiceTier)
	assert.True(t, other.AllowInferenceGeo)
	assert.False(t, other.AllowSpeed)
	assert.True(t, other.DisableStore)
	assert.True(t, other.AllowSafetyIdentifier)
	assert.True(t, other.AllowIncludeObfuscation)

	writeEdgeLocalTestChannelConfig(t, "edge-channel", `name: edge-channel
type: openai
base_url: http://rotated-cpa-x4.internal:8317
auth: rotated-test-key-x4
`)
	require.NoError(t, RefreshEdgeLocalChannelRuntime(db))
	require.NoError(t, db.First(&textChannel, 31).Error)
	require.NotNil(t, textChannel.BaseURL)
	assert.Equal(t, "http://rotated-cpa-x4.internal:8317", *textChannel.BaseURL)
	assert.Equal(t, "rotated-test-key-x4", textChannel.Key)

	writeEdgeLocalTestChannelConfig(t, "edge-channel", `name: edge-channel
type: openai
base_url: http://rotated-cpa-x4.internal:8317
enabled: false
auth: rotated-test-key-x4
`)
	require.NoError(t, RefreshEdgeLocalChannelRuntime(db))
	require.NoError(t, db.First(&textChannel, 31).Error)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, textChannel.Status)
	var disabledAbility Ability
	require.NoError(t, db.Where("channel_id = ?", 31).First(&disabledAbility).Error)
	assert.False(t, disabledAbility.Enabled)
}

func TestApplyEdgeLocalSnapshotDisablesChannelWithoutLocalConfig(t *testing.T) {
	db := openEdgeLocalTestDB(t, "cpa-disabled.db")
	t.Setenv(edgeChannelConfigDirEnv, t.TempDir())
	snapshot := edgeLocalTestSnapshot(7)
	require.NoError(t, ApplyEdgeLocalSnapshot(db, snapshot))

	var channel Channel
	require.NoError(t, db.First(&channel, 31).Error)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, channel.Status)
	assert.Empty(t, channel.Key)
	require.NotNil(t, channel.BaseURL)
	assert.Empty(t, *channel.BaseURL)
	var ability Ability
	require.NoError(t, db.Where("channel_id = ?", 31).First(&ability).Error)
	assert.False(t, ability.Enabled)
}

func openEdgeLocalTestDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	channelConfigDir := t.TempDir()
	t.Setenv(edgeChannelConfigDirEnv, channelConfigDir)
	require.NoError(t, os.WriteFile(filepath.Join(channelConfigDir, "edge-channel.yaml"), []byte(`name: edge-channel
type: openai
base_url: http://edge-channel:8317
auth: edge-test-key
`), 0o600))
	db, err := OpenEdgeSQLite(filepath.Join(t.TempDir(), name))
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func writeEdgeLocalTestChannelConfig(t *testing.T, name string, content string) {
	t.Helper()
	directory := os.Getenv(edgeChannelConfigDirEnv)
	require.NotEmpty(t, directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, name+".yaml"), []byte(content), 0o600))
}

func edgeLocalTestSnapshot(revision int64) EdgeLocalSnapshotProjectionData {
	modelPrice := 0.001
	datasets := make([]dto.EdgeSnapshotDatasetStateV1, 0, len(edgeLocalSnapshotDatasetOrder))
	for _, dataset := range edgeLocalSnapshotDatasetOrder {
		datasets = append(datasets, dto.EdgeSnapshotDatasetStateV1{Dataset: dataset, Revision: revision})
	}
	return EdgeLocalSnapshotProjectionData{
		State: dto.EdgeSnapshotStateV1{
			SnapshotID: fmt.Sprintf("snapshot-%d", revision), Revision: revision,
			AppliedAtUnixMilli: edgeLocalTestNow + revision, Datasets: datasets,
		},
		Digest: strings.Repeat("a", 64), ExpiresAtUnixMilli: edgeLocalTestNow + 120_000,
		TokenFingerprint: dto.EdgeTokenFingerprintSchemeV1{
			Algorithm: edgetoken.FingerprintAlgorithm, Version: edgetoken.FingerprintVersion,
		},
		Authentication: []dto.EdgeTokenAuthRecordV1{{
			TokenFingerprint: strings.Repeat("b", 64), TokenID: 11, UserID: 7,
			Enabled: true, Group: "default",
		}},
		Users: []dto.EdgeUserPolicyV1{{
			UserID: 7, Enabled: true, Username: "edge-user-7", DefaultGroup: "default",
			Setting: dto.EdgeUserSettingV1{Language: "zh", BillingPreference: "subscription_first"},
		}},
		Groups: []dto.EdgeGroupPolicyV1{{
			UserGroup:   "default",
			UsingGroups: []dto.EdgeUsingGroupPolicyV1{{Group: "default", Enabled: true, Ratio: 1}},
		}},
		Models: []dto.EdgeModelPolicyV1{{
			Model: "gpt-4o-mini", Enabled: true,
			Endpoints: []dto.EdgeEndpointV1{dto.EdgeEndpointOpenAIChatCompletionsV1, dto.EdgeEndpointOpenAIResponsesV1},
			Streaming: true, ChannelIDs: []int64{31},
		}},
		Channels: []dto.EdgeChannelProjectionV1{{
			ChannelID: 31, Type: 1, Name: "edge-channel", Enabled: true,
			Groups: []string{"default"}, Models: []string{"gpt-4o-mini"},
			Priority: 10, Weight: 100, LocalService: dto.EdgeLocalServiceCPAPro20x4V1,
		}},
		Pricing: []dto.EdgePricingPolicyV1{{
			PolicyID: fmt.Sprintf("pricing-%d", revision), Version: fmt.Sprintf("v%d", revision),
			Model: "gpt-4o-mini", BillingMode: dto.EdgeBillingModeFixedPriceV1,
			ModelPrice: &modelPrice, QuotaPerUnit: 500_000,
		}},
		Routing: []dto.EdgeRoutingPolicyV1{{
			ChannelAffinity: dto.EdgeChannelAffinityPolicyV1{
				Enabled: false, MaxEntries: 1_000, DefaultTTLSeconds: 60,
			},
		}},
	}
}
