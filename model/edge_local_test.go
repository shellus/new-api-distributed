package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgesettlement"
	"github.com/QuantumNous/new-api/pkg/edgetoken"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const edgeLocalTestNow = int64(1_784_160_000_000)

type edgeLocalQuotaReservationBeforeStaging struct {
	ReservationID        string                     `gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	RequestID            string                     `gorm:"type:varchar(64);not null;uniqueIndex"`
	LeaseID              string                     `gorm:"type:varchar(64);not null;index"`
	UserID               int64                      `gorm:"not null;index"`
	TokenID              int64                      `gorm:"not null;index"`
	Status               EdgeLocalReservationStatus `gorm:"type:varchar(32);not null;index"`
	ReservedQuota        int64                      `gorm:"not null"`
	ChargedQuota         int64                      `gorm:"not null"`
	EventID              string                     `gorm:"type:varchar(64);not null;index"`
	EventSequence        int64                      `gorm:"not null;index"`
	CreatedAtUnixMilli   int64                      `gorm:"not null"`
	UpdatedAtUnixMilli   int64                      `gorm:"not null"`
	FinalizedAtUnixMilli int64                      `gorm:"not null"`
}

func (edgeLocalQuotaReservationBeforeStaging) TableName() string {
	return "edge_local_quota_reservations"
}

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
	assert.Contains(t, tableNames, "edge_local_quota_leases")
	assert.Contains(t, tableNames, "edge_local_lease_acquire_intents")
	assert.NotContains(t, tableNames, "users")
	assert.NotContains(t, tableNames, "tokens")
	assert.NotContains(t, tableNames, "options")

	rows, err := db.Raw("PRAGMA foreign_key_list(edge_local_quota_reservations)").Rows()
	require.NoError(t, err)
	defer rows.Close()
	assert.False(t, rows.Next())
}

func TestOpenEdgeSQLiteMigratesExistingReservationsForSettlementStaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staging-migration.db")
	dsn, err := edgeSQLiteDSN(path)
	require.NoError(t, err)
	legacyDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, legacyDB.AutoMigrate(&edgeLocalQuotaReservationBeforeStaging{}))
	require.NoError(t, legacyDB.Create(&edgeLocalQuotaReservationBeforeStaging{
		ReservationID: "reservation-before-staging", RequestID: "request-before-staging",
		LeaseID: "lease-before-staging", UserID: 7, TokenID: 11, Status: EdgeLocalReservationStatusActive,
		ReservedQuota: 10, CreatedAtUnixMilli: edgeLocalTestNow, UpdatedAtUnixMilli: edgeLocalTestNow,
	}).Error)
	legacySQLDB, err := legacyDB.DB()
	require.NoError(t, err)
	require.NoError(t, legacySQLDB.Close())

	db, err := OpenEdgeSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	reservation, err := GetEdgeLocalReservation(db, "reservation-before-staging")
	require.NoError(t, err)
	assert.Empty(t, reservation.StagedEventID)
	assert.Empty(t, reservation.StagedEventPayload)
	assert.Zero(t, reservation.StagedAtUnixMilli)
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

func TestApplyEdgeLocalSnapshotIsAtomicAndPreservesAccounting(t *testing.T) {
	db := openEdgeLocalTestDB(t, "snapshot.db")
	snapshot := edgeLocalTestSnapshot(7)
	require.NoError(t, ApplyEdgeLocalSnapshot(db, snapshot))

	auth, err := GetEdgeLocalTokenAuth(db, snapshot.Authentication[0].TokenFingerprint)
	require.NoError(t, err)
	assert.Equal(t, int64(11), auth.TokenID)
	user, err := GetEdgeLocalUser(db, 7)
	require.NoError(t, err)
	assert.Equal(t, "edge-user-7", user.Username)
	pricing, err := GetEdgeLocalPricing(db, "gpt-4o-mini")
	require.NoError(t, err)
	require.Len(t, pricing, 1)
	assert.Equal(t, "pricing-7", pricing[0].PolicyID)

	lease := edgeLocalTestLease("lease-snapshot", 1_000, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-snapshot", RequestID: "request-snapshot", LeaseID: lease.LeaseID,
		Quota: 100, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)
	_, err = SettleEdgeLocalReservation(db, "reservation-snapshot", edgeLocalTestUsageEvent("event-snapshot", 80))
	require.NoError(t, err)
	_, err = BuildEdgeLocalSettlementBlock(
		db,
		dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV1, RequestID: "snapshot-preserve-block"},
		"block-snapshot", 100, edgeLocalTestNow+100,
	)
	require.NoError(t, err)
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-inflight-settle", RequestID: "request-inflight-settle", LeaseID: lease.LeaseID,
		Quota: 50, NowUnixMilli: edgeLocalTestNow + 110,
	})
	require.NoError(t, err)
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-inflight-refund", RequestID: "request-inflight-refund", LeaseID: lease.LeaseID,
		Quota: 40, NowUnixMilli: edgeLocalTestNow + 120,
	})
	require.NoError(t, err)

	replacement := edgeLocalTestSnapshot(8)
	replacement.Users[0].Username = "edge-user-replaced"
	replacement.Authentication[0].TokenFingerprint = strings.Repeat("c", 64)
	replacement.Channels[0].Name = "replacement-channel"
	require.NoError(t, ApplyEdgeLocalSnapshot(db, replacement))
	_, err = AdjustEdgeLocalReservation(db, "reservation-inflight-settle", 60, edgeLocalTestNow+130)
	assert.ErrorIs(t, err, ErrEdgeLocalSnapshotMismatch)
	inflightEvent := edgeLocalTestUsageEvent("event-inflight", 45)
	inflightEvent.StartedAtUnixMilli = edgeLocalTestNow + 110
	inflightEvent.FinishedAtUnixMilli = edgeLocalTestNow + 140
	_, err = SettleEdgeLocalReservation(db, "reservation-inflight-settle", inflightEvent)
	require.NoError(t, err)
	require.NoError(t, RefundEdgeLocalReservation(db, "reservation-inflight-refund", edgeLocalTestNow+150))

	_, err = GetEdgeLocalTokenAuth(db, snapshot.Authentication[0].TokenFingerprint)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	auth, err = GetEdgeLocalTokenAuth(db, replacement.Authentication[0].TokenFingerprint)
	require.NoError(t, err)
	assert.Equal(t, int64(11), auth.TokenID)
	user, err = GetEdgeLocalUser(db, 7)
	require.NoError(t, err)
	assert.Equal(t, "edge-user-replaced", user.Username)
	state, err := GetEdgeLocalSnapshotState(db)
	require.NoError(t, err)
	assert.Equal(t, int64(8), state.Revision)
	require.Len(t, state.Datasets, 7)
	expiresAt, err := GetEdgeLocalSnapshotExpiry(db)
	require.NoError(t, err)
	assert.Equal(t, replacement.ExpiresAtUnixMilli, expiresAt)

	for table, expected := range map[any]int64{
		&EdgeLocalQuotaLease{}: 1, &EdgeLocalQuotaReservation{}: 3,
		&EdgeLocalUsageEvent{}: 2, &EdgeLocalOutbox{}: 2, &EdgeLocalSettlementBlock{}: 1,
	} {
		var count int64
		require.NoError(t, db.Model(table).Count(&count).Error)
		assert.Equal(t, expected, count)
	}
	var channel Channel
	require.NoError(t, db.First(&channel, 31).Error)
	assert.Equal(t, "replacement-channel", channel.Name)
	assert.ErrorIs(t, ApplyEdgeLocalSnapshot(db, snapshot), ErrEdgeLocalSnapshotStale)
	conflictingRevision := replacement
	conflictingRevision.Digest = strings.Repeat("d", 64)
	assert.ErrorIs(t, ApplyEdgeLocalSnapshot(db, conflictingRevision), ErrEdgeLocalSnapshotConflict)
	require.NoError(t, ApplyEdgeLocalSnapshot(db, replacement))
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

func TestEdgeLocalConcurrentReservationsNeverOversellLease(t *testing.T) {
	db := openEdgeLocalTestDB(t, "concurrent.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(7)))
	lease := edgeLocalTestLease("lease-concurrent", 100, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))

	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	errorsSeen := make([]error, 0, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
				ReservationID: fmt.Sprintf("reservation-%02d", index),
				RequestID:     fmt.Sprintf("request-%02d", index),
				LeaseID:       lease.LeaseID, Quota: 10, NowUnixMilli: edgeLocalTestNow,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
				return
			}
			errorsSeen = append(errorsSeen, err)
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 10, successes)
	require.Len(t, errorsSeen, 10)
	for _, err := range errorsSeen {
		assert.ErrorIs(t, err, ErrEdgeLocalQuotaInsufficient)
	}
	storedLease, err := GetEdgeLocalLease(db, lease.LeaseID)
	require.NoError(t, err)
	assert.Zero(t, storedLease.RemainingQuota)
	assert.Equal(t, int64(100), storedLease.ReservedQuota)
	assert.Zero(t, storedLease.ConsumedQuota)
	var reservationCount int64
	require.NoError(t, db.Model(&EdgeLocalQuotaReservation{}).Count(&reservationCount).Error)
	assert.Equal(t, int64(10), reservationCount)
}

func TestEdgeLocalAccountingSurvivesRestartAndKeepsContiguousBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := OpenEdgeSQLite(path)
	require.NoError(t, err)
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(7)))
	lease := edgeLocalTestLease("lease-restart", 1_000, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))

	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-one", RequestID: "request-one", LeaseID: lease.LeaseID,
		Quota: 200, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)
	_, err = AdjustEdgeLocalReservation(db, "reservation-one", 300, edgeLocalTestNow+1)
	require.NoError(t, err)
	event := edgeLocalTestUsageEvent("event-one", 250)
	settled, err := SettleEdgeLocalReservation(db, "reservation-one", event)
	require.NoError(t, err)
	assert.Equal(t, int64(1), settled.Sequence)
	assert.Equal(t, int64(300), settled.Billing.ReservedQuota)

	replayed, err := SettleEdgeLocalReservation(db, "reservation-one", *settled)
	require.NoError(t, err)
	assert.Equal(t, settled, replayed)
	conflict := *settled
	conflict.Billing.ChargedQuota = 249
	_, err = SettleEdgeLocalReservation(db, "reservation-one", conflict)
	assert.ErrorIs(t, err, ErrEdgeLocalSettlementConflict)

	storedLease, err := GetEdgeLocalLease(db, lease.LeaseID)
	require.NoError(t, err)
	assert.Equal(t, int64(750), storedLease.RemainingQuota)
	assert.Zero(t, storedLease.ReservedQuota)
	assert.Equal(t, int64(250), storedLease.ConsumedQuota)

	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-refund", RequestID: "request-refund", LeaseID: lease.LeaseID,
		Quota: 100, NowUnixMilli: edgeLocalTestNow + 10,
	})
	require.NoError(t, err)
	require.NoError(t, RefundEdgeLocalReservation(db, "reservation-refund", edgeLocalTestNow+11))
	require.NoError(t, RefundEdgeLocalReservation(db, "reservation-refund", edgeLocalTestNow+12))
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-refund", RequestID: "request-refund", LeaseID: lease.LeaseID,
		Quota: 100, NowUnixMilli: edgeLocalTestNow + 13,
	})
	assert.ErrorIs(t, err, ErrEdgeLocalReservationFinalized)

	meta := dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV1, RequestID: "settlement-request-one"}
	block, err := BuildEdgeLocalSettlementBlock(db, meta, "block-one", 100, edgeLocalTestNow+100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), block.FirstSequence)
	assert.Equal(t, int64(1), block.LastSequence)
	require.Len(t, block.Events, 1)
	expectedDigest, err := edgesettlement.DigestBlockV1(lease.NodeID, lease.NodeGeneration, *block)
	require.NoError(t, err)
	assert.Equal(t, expectedDigest, block.BlockDigest)

	var storedEvent EdgeLocalUsageEvent
	var outbox EdgeLocalOutbox
	require.NoError(t, db.Where("event_id = ?", "event-one").First(&storedEvent).Error)
	require.NoError(t, db.Where("event_id = ?", "event-one").First(&outbox).Error)
	assert.Equal(t, storedEvent.Payload, outbox.Payload)
	canonicalEvent, err := common.Marshal(*settled)
	require.NoError(t, err)
	assert.Equal(t, string(canonicalEvent), storedEvent.Payload)

	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-crash", RequestID: "request-crash", LeaseID: lease.LeaseID,
		Quota: 25, NowUnixMilli: edgeLocalTestNow + 150,
	})
	require.NoError(t, err)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	db, err = OpenEdgeSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		reopened, sqlErr := db.DB()
		if sqlErr == nil {
			_ = reopened.Close()
		}
	})

	recovered, err := BuildEdgeLocalSettlementBlock(
		db,
		dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV1, RequestID: "ignored-retry-meta"},
		"ignored-retry-block", 1, edgeLocalTestNow+200,
	)
	require.NoError(t, err)
	assert.Equal(t, block, recovered)
	var crashReservation EdgeLocalQuotaReservation
	require.NoError(t, db.Where("reservation_id = ?", "reservation-crash").First(&crashReservation).Error)
	assert.Equal(t, EdgeLocalReservationStatusActive, crashReservation.Status)
	require.NoError(t, RefundEdgeLocalReservation(db, "reservation-crash", edgeLocalTestNow+250))

	ack := dto.EdgeSettlementAckV1{
		Status: dto.EdgeSettlementAckAcceptedV1, NodeID: lease.NodeID, NodeGeneration: lease.NodeGeneration,
		BlockID: block.BlockID, AckedThroughSequence: 1, NextExpectedSequence: 2,
		AcceptedEventCount: 1, AcknowledgedAtUnixMilli: edgeLocalTestNow + 300,
	}
	require.NoError(t, AcknowledgeEdgeLocalSettlementBlock(db, ack))
	ack.Status = dto.EdgeSettlementAckDuplicateV1
	ack.AcknowledgedAtUnixMilli++
	require.NoError(t, AcknowledgeEdgeLocalSettlementBlock(db, ack))
	state, err := GetEdgeLocalSettlementState(db)
	require.NoError(t, err)
	assert.Equal(t, int64(1), state.LastAckedSequence)
	assert.Equal(t, int64(2), state.NextEventSequence)
	assert.Zero(t, state.PendingEventCount)
	assert.Zero(t, state.PendingBlockCount)

	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-two", RequestID: "request-two", LeaseID: lease.LeaseID,
		Quota: 50, NowUnixMilli: edgeLocalTestNow + 400,
	})
	require.NoError(t, err)
	secondEvent := edgeLocalTestUsageEvent("event-two", 50)
	secondEvent.StartedAtUnixMilli = edgeLocalTestNow + 400
	secondEvent.FinishedAtUnixMilli = edgeLocalTestNow + 410
	second, err := SettleEdgeLocalReservation(db, "reservation-two", secondEvent)
	require.NoError(t, err)
	assert.Equal(t, int64(2), second.Sequence)
	secondBlock, err := BuildEdgeLocalSettlementBlock(
		db,
		dto.EdgeControlRequestMetaV1{ProtocolVersion: dto.EdgeControlProtocolVersionV1, RequestID: "settlement-request-two"},
		"block-two", 100, edgeLocalTestNow+500,
	)
	require.NoError(t, err)
	assert.Equal(t, block.BlockID, secondBlock.PreviousBlockID)
	assert.Equal(t, block.BlockDigest, secondBlock.PreviousBlockDigest)
	assert.Equal(t, int64(2), secondBlock.FirstSequence)
}

func TestEdgeLocalReservationFailsClosedAndChargedQuotaCannotExceedReserve(t *testing.T) {
	db := openEdgeLocalTestDB(t, "fail-closed.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(7)))

	_, err := ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-missing", RequestID: "request-missing", LeaseID: "lease-missing",
		Quota: 1, NowUnixMilli: edgeLocalTestNow,
	})
	assert.ErrorIs(t, err, ErrEdgeLocalLeaseUnavailable)

	expired := edgeLocalTestLease("lease-expired", 100, 7)
	expired.IssuedAtUnixMilli = edgeLocalTestNow - 2_000
	expired.ExpiresAtUnixMilli = edgeLocalTestNow - 1_000
	require.NoError(t, InstallEdgeLocalLease(db, expired))
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-expired", RequestID: "request-expired", LeaseID: expired.LeaseID,
		Quota: 1, NowUnixMilli: edgeLocalTestNow,
	})
	assert.ErrorIs(t, err, ErrEdgeLocalLeaseExpired)

	lease := edgeLocalTestLease("lease-small", 10, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-too-large", RequestID: "request-too-large", LeaseID: lease.LeaseID,
		Quota: 11, NowUnixMilli: edgeLocalTestNow,
	})
	assert.ErrorIs(t, err, ErrEdgeLocalQuotaInsufficient)
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-charge", RequestID: "request-charge", LeaseID: lease.LeaseID,
		Quota: 10, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)
	_, err = SettleEdgeLocalReservation(db, "reservation-charge", edgeLocalTestUsageEvent("event-overcharge", 11))
	assert.ErrorIs(t, err, ErrEdgeLocalQuotaInsufficient)
	storedLease, err := GetEdgeLocalLease(db, lease.LeaseID)
	require.NoError(t, err)
	assert.Zero(t, storedLease.RemainingQuota)
	assert.Equal(t, int64(10), storedLease.ReservedQuota)
	assert.Zero(t, storedLease.ConsumedQuota)
	err = RefundEdgeLocalReservation(db, "reservation-charge", edgeLocalTestNow+1)
	assert.ErrorIs(t, err, ErrEdgeLocalSettlementStaged, "completed usage must not be refunded after settlement was staged")
	staged, err := ListStagedEdgeLocalReservationIDs(db, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"reservation-charge"}, staged)

	shortLease := edgeLocalTestLease("lease-short", 20, 7)
	shortLease.ExpiresAtUnixMilli = edgeLocalTestNow + 5
	require.NoError(t, InstallEdgeLocalLease(db, shortLease))
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-short", RequestID: "request-short", LeaseID: shortLease.LeaseID,
		Quota: 10, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)
	_, err = AdjustEdgeLocalReservation(db, "reservation-short", 15, edgeLocalTestNow+6)
	assert.ErrorIs(t, err, ErrEdgeLocalLeaseExpired)
	shortEvent := edgeLocalTestUsageEvent("event-short", 10)
	shortEvent.FinishedAtUnixMilli = edgeLocalTestNow + 10
	_, err = SettleEdgeLocalReservation(db, "reservation-short", shortEvent)
	require.NoError(t, err, "a request reserved before expiry must still settle durably after expiry")
}

func TestEdgeLocalSettlementAtomicallyTopsUpActualCharge(t *testing.T) {
	db := openEdgeLocalTestDB(t, "settlement-top-up.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(7)))
	lease := edgeLocalTestLease("lease-top-up", 100, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))
	_, err := ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-top-up", RequestID: "request-top-up", LeaseID: lease.LeaseID,
		Quota: 40, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)

	settled, err := SettleEdgeLocalReservation(db, "reservation-top-up", edgeLocalTestUsageEvent("event-top-up", 60))
	require.NoError(t, err)
	assert.Equal(t, int64(60), settled.Billing.ReservedQuota)
	assert.Equal(t, int64(60), settled.Billing.ChargedQuota)

	storedLease, err := GetEdgeLocalLease(db, lease.LeaseID)
	require.NoError(t, err)
	assert.Equal(t, int64(40), storedLease.RemainingQuota)
	assert.Zero(t, storedLease.ReservedQuota)
	assert.Equal(t, int64(60), storedLease.ConsumedQuota)
	reservation, err := GetEdgeLocalReservation(db, "reservation-top-up")
	require.NoError(t, err)
	assert.Equal(t, int64(60), reservation.ReservedQuota)
	assert.Equal(t, int64(60), reservation.ChargedQuota)
}

func TestEdgeLocalStagedSettlementSurvivesRestartAndCannotBeRefunded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staged-restart.db")
	db, err := OpenEdgeSQLite(path)
	require.NoError(t, err)
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(7)))
	lease := edgeLocalTestLease("lease-staged-restart", 100, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-staged-restart", RequestID: "request-staged-restart", LeaseID: lease.LeaseID,
		Quota: 40, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)
	event := edgeLocalTestUsageEvent("event-staged-restart", 40)
	require.NoError(t, StageEdgeLocalReservationSettlement(db, "reservation-staged-restart", event))
	require.NoError(t, StageEdgeLocalReservationSettlement(db, "reservation-staged-restart", event), "staging must be idempotent")
	_, err = AdjustEdgeLocalReservation(db, "reservation-staged-restart", 30, edgeLocalTestNow+1)
	assert.ErrorIs(t, err, ErrEdgeLocalSettlementStaged, "a staged usage payload must freeze its original reservation")

	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	db, err = OpenEdgeSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		reopened, sqlErr := db.DB()
		if sqlErr == nil {
			_ = reopened.Close()
		}
	})

	err = RefundEdgeLocalReservation(db, "reservation-staged-restart", edgeLocalTestNow+1)
	assert.ErrorIs(t, err, ErrEdgeLocalSettlementStaged)
	staged, err := ListStagedEdgeLocalReservationIDs(db, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"reservation-staged-restart"}, staged)

	settled, err := SettleStagedEdgeLocalReservation(db, "reservation-staged-restart")
	require.NoError(t, err)
	assert.Equal(t, int64(1), settled.Sequence)
	assert.Equal(t, int64(40), settled.Billing.ChargedQuota)
	staged, err = ListStagedEdgeLocalReservationIDs(db, 10)
	require.NoError(t, err)
	assert.Empty(t, staged)

	var eventCount int64
	var outboxCount int64
	require.NoError(t, db.Model(&EdgeLocalUsageEvent{}).Where("event_id = ?", event.EventID).Count(&eventCount).Error)
	require.NoError(t, db.Model(&EdgeLocalOutbox{}).Where("event_id = ?", event.EventID).Count(&outboxCount).Error)
	assert.Equal(t, int64(1), eventCount)
	assert.Equal(t, int64(1), outboxCount)
}

func TestEdgeLocalStagedSettlementRetriesAfterReservedQuotaReturns(t *testing.T) {
	db := openEdgeLocalTestDB(t, "staged-retry.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(7)))
	lease := edgeLocalTestLease("lease-staged-retry", 100, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))
	_, err := ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-staged-retry", RequestID: "request-staged-retry", LeaseID: lease.LeaseID,
		Quota: 40, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-staged-blocker", RequestID: "request-staged-blocker", LeaseID: lease.LeaseID,
		Quota: 60, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)

	event := edgeLocalTestUsageEvent("event-staged-retry", 60)
	_, err = SettleEdgeLocalReservation(db, "reservation-staged-retry", event)
	assert.ErrorIs(t, err, ErrEdgeLocalQuotaInsufficient)
	err = RefundEdgeLocalReservation(db, "reservation-staged-retry", edgeLocalTestNow+1)
	assert.ErrorIs(t, err, ErrEdgeLocalSettlementStaged)
	require.NoError(t, RefundEdgeLocalReservation(db, "reservation-staged-blocker", edgeLocalTestNow+2))

	settled, err := SettleStagedEdgeLocalReservation(db, "reservation-staged-retry")
	require.NoError(t, err)
	assert.Equal(t, int64(60), settled.Billing.ReservedQuota)
	assert.Equal(t, int64(60), settled.Billing.ChargedQuota)
	storedLease, err := GetEdgeLocalLease(db, lease.LeaseID)
	require.NoError(t, err)
	assert.Equal(t, int64(40), storedLease.RemainingQuota)
	assert.Zero(t, storedLease.ReservedQuota)
	assert.Equal(t, int64(60), storedLease.ConsumedQuota)
}

func TestEdgeLocalPartialSettlementMarkerFailsClosed(t *testing.T) {
	db := openEdgeLocalTestDB(t, "partial-staging-marker.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(7)))
	lease := edgeLocalTestLease("lease-partial-staging", 100, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))
	_, err := ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-partial-staging", RequestID: "request-partial-staging", LeaseID: lease.LeaseID,
		Quota: 40, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)
	require.NoError(t, db.Model(&EdgeLocalQuotaReservation{}).
		Where("reservation_id = ?", "reservation-partial-staging").
		Update("staged_event_id", "event-partial-staging").Error)

	_, err = ListStagedEdgeLocalReservationIDs(db, 10)
	assert.ErrorIs(t, err, ErrEdgeLocalAccountingCorruption)
	err = RefundEdgeLocalReservation(db, "reservation-partial-staging", edgeLocalTestNow+1)
	assert.ErrorIs(t, err, ErrEdgeLocalAccountingCorruption)
	storedLease, err := GetEdgeLocalLease(db, lease.LeaseID)
	require.NoError(t, err)
	assert.Equal(t, int64(60), storedLease.RemainingQuota)
	assert.Equal(t, int64(40), storedLease.ReservedQuota)
}

func TestEdgeLocalZeroQuotaReservationStillRequiresLeaseAndSettlesOutbox(t *testing.T) {
	db := openEdgeLocalTestDB(t, "zero-quota.db")
	require.NoError(t, ApplyEdgeLocalSnapshot(db, edgeLocalTestSnapshot(7)))
	_, err := ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-zero-missing", RequestID: "request-zero-missing", LeaseID: "lease-zero-missing",
		Quota: 0, NowUnixMilli: edgeLocalTestNow,
	})
	assert.ErrorIs(t, err, ErrEdgeLocalLeaseUnavailable)

	lease := edgeLocalTestLease("lease-zero", 1, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))
	reservation, err := ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-zero", RequestID: "request-zero", LeaseID: lease.LeaseID,
		Quota: 0, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)
	assert.Zero(t, reservation.ReservedQuota)
	storedLease, err := GetEdgeLocalLease(db, lease.LeaseID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), storedLease.RemainingQuota)
	assert.Zero(t, storedLease.ReservedQuota)

	settled, err := SettleEdgeLocalReservation(db, reservation.ReservationID, edgeLocalTestUsageEvent("event-zero", 0))
	require.NoError(t, err)
	assert.Zero(t, settled.Billing.ReservedQuota)
	assert.Zero(t, settled.Billing.ChargedQuota)
	var outbox EdgeLocalOutbox
	require.NoError(t, db.Where("event_id = ?", settled.EventID).First(&outbox).Error)
	assert.Equal(t, EdgeLocalOutboxStatusPending, outbox.Status)
}

func TestEdgeLocalSnapshotExpiryFailsClosedForNewFunding(t *testing.T) {
	db := openEdgeLocalTestDB(t, "snapshot-expiry.db")
	snapshot := edgeLocalTestSnapshot(7)
	snapshot.State.AppliedAtUnixMilli = edgeLocalTestNow - 1_000
	snapshot.ExpiresAtUnixMilli = edgeLocalTestNow + 5
	require.NoError(t, ApplyEdgeLocalSnapshot(db, snapshot))
	expiresAt, err := GetEdgeLocalSnapshotExpiry(db)
	require.NoError(t, err)
	assert.Equal(t, edgeLocalTestNow+5, expiresAt)
	lease := edgeLocalTestLease("lease-snapshot-expiry", 100, 7)
	require.NoError(t, InstallEdgeLocalLease(db, lease))
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-before-snapshot-expiry", RequestID: "request-before-snapshot-expiry", LeaseID: lease.LeaseID,
		Quota: 10, NowUnixMilli: edgeLocalTestNow,
	})
	require.NoError(t, err)
	_, err = AdjustEdgeLocalReservation(db, "reservation-before-snapshot-expiry", 11, edgeLocalTestNow+6)
	assert.ErrorIs(t, err, ErrEdgeLocalSnapshotExpired)
	_, err = ReserveEdgeLocalQuota(db, EdgeLocalReservationRequest{
		ReservationID: "reservation-after-snapshot-expiry", RequestID: "request-after-snapshot-expiry", LeaseID: lease.LeaseID,
		Quota: 1, NowUnixMilli: edgeLocalTestNow + 6,
	})
	assert.ErrorIs(t, err, ErrEdgeLocalSnapshotExpired)
	event := edgeLocalTestUsageEvent("event-after-snapshot-expiry", 10)
	event.FinishedAtUnixMilli = edgeLocalTestNow + 7
	_, err = SettleEdgeLocalReservation(db, "reservation-before-snapshot-expiry", event)
	require.NoError(t, err)
}

func TestEdgeLocalLeaseAcquireIntentSurvivesRestartAndReusesExactRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease-intent.db")
	db, err := OpenEdgeSQLite(path)
	require.NoError(t, err)
	request := edgeLocalTestLeaseAcquireRequest("lease-acquire-original")
	durable, err := GetOrCreateEdgeLocalLeaseAcquireIntent(db, request, edgeLocalTestNow)
	require.NoError(t, err)
	assert.Equal(t, request, *durable)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	db, err = OpenEdgeSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		reopened, sqlErr := db.DB()
		if sqlErr == nil {
			_ = reopened.Close()
		}
	})

	retry := request
	retry.Meta.RequestID = "lease-acquire-new-key"
	retry.RequestedQuota = 200
	reused, err := GetOrCreateEdgeLocalLeaseAcquireIntent(db, retry, edgeLocalTestNow+1)
	require.NoError(t, err)
	assert.Equal(t, request, *reused)
	restored, err := GetEdgeLocalLeaseAcquireIntent(db, request.Subject.UserID, request.Subject.TokenID)
	require.NoError(t, err)
	assert.Equal(t, request, *restored)

	conflictingLease := edgeLocalTestLease("lease-intent-conflict", 100, 6)
	err = InstallEdgeLocalLeaseFromAcquireIntent(db, request.Meta.RequestID, conflictingLease)
	assert.ErrorIs(t, err, ErrEdgeLocalSettlementConflict)
	_, err = GetEdgeLocalLeaseAcquireIntent(db, request.Subject.UserID, request.Subject.TokenID)
	require.NoError(t, err, "failed completion must preserve the intent")
	_, err = GetEdgeLocalLease(db, conflictingLease.LeaseID)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)

	lease := edgeLocalTestLease("lease-from-intent", 100, 7)
	require.NoError(t, InstallEdgeLocalLeaseFromAcquireIntent(db, request.Meta.RequestID, lease))
	_, err = GetEdgeLocalLeaseAcquireIntent(db, request.Subject.UserID, request.Subject.TokenID)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	installed, err := GetEdgeLocalLease(db, lease.LeaseID)
	require.NoError(t, err)
	assert.Equal(t, lease.Subject.UserID, installed.UserID)

	rejected := edgeLocalTestLeaseAcquireRequest("lease-acquire-rejected")
	_, err = GetOrCreateEdgeLocalLeaseAcquireIntent(db, rejected, edgeLocalTestNow+2)
	require.NoError(t, err)
	require.NoError(t, DiscardEdgeLocalLeaseAcquireIntent(db, rejected.Meta.RequestID))
	_, err = GetEdgeLocalLeaseAcquireIntent(db, rejected.Subject.UserID, rejected.Subject.TokenID)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
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

func edgeLocalTestLeaseAcquireRequest(requestID string) dto.EdgeLeaseAcquireRequestV1 {
	return dto.EdgeLeaseAcquireRequestV1{
		Meta: dto.EdgeControlRequestMetaV1{
			ProtocolVersion: dto.EdgeControlProtocolVersionV1,
			RequestID:       requestID,
		},
		Subject:                dto.EdgeLeaseSubjectV1{UserID: 7, TokenID: 11},
		RequestedQuota:         100,
		MinimumAcceptableQuota: 50,
		SnapshotID:             "snapshot-7",
		SnapshotRevision:       7,
	}
}

func edgeLocalTestLease(leaseID string, quota, snapshotRevision int64) dto.EdgeQuotaLeaseV1 {
	return dto.EdgeQuotaLeaseV1{
		LeaseID: leaseID, Version: 1, Status: dto.EdgeLeaseStatusActiveV1,
		NodeID: "edge.test", NodeGeneration: 1,
		Subject:      dto.EdgeLeaseSubjectV1{UserID: 7, TokenID: 11},
		GrantedQuota: quota, RenewAfterRemainingQuota: quota / 10,
		IssuedAtUnixMilli: edgeLocalTestNow - 1_000, ExpiresAtUnixMilli: edgeLocalTestNow + 60_000,
		SnapshotID: fmt.Sprintf("snapshot-%d", snapshotRevision), SnapshotRevision: snapshotRevision,
		PricingRevision: snapshotRevision,
	}
}

func edgeLocalTestUsageEvent(eventID string, chargedQuota int64) dto.EdgeUsageEventV1 {
	httpStatus := 200
	return dto.EdgeUsageEventV1{
		EventID: eventID, ChannelID: 31,
		Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1, Streaming: true,
		Model: "gpt-4o-mini", Group: "default",
		StartedAtUnixMilli: edgeLocalTestNow, FinishedAtUnixMilli: edgeLocalTestNow + 10,
		Outcome: dto.EdgeUsageOutcomeSuccessV1, HTTPStatus: &httpStatus,
		Billing: dto.EdgeUsageBillingV1{
			PricingPolicyID: "pricing-7", PricingPolicyVersion: "v7",
			BillingMode: dto.EdgeBillingModeFixedPriceV1, GroupRatio: 1,
			ChargedQuota: chargedQuota,
		},
	}
}
