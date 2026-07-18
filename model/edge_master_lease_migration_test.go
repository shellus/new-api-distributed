package model

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestMasterLeaseSchemaMigrationDropsOnlyDrainedLegacyState(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    string
		wantError bool
	}{
		{name: "closed", status: "closed"},
		{name: "active", status: "active", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			previousDB := DB
			previousMainType := common.MainDatabaseType()
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "master.db")), &gorm.Config{})
			require.NoError(t, err)
			DB = db
			common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.LogDatabaseType())
			t.Cleanup(func() {
				DB = previousDB
				common.SetDatabaseTypes(previousMainType, common.LogDatabaseType())
				if sqlDB, sqlErr := db.DB(); sqlErr == nil {
					_ = sqlDB.Close()
				}
			})
			require.NoError(t, db.Exec("CREATE TABLE edge_quota_leases (id INTEGER PRIMARY KEY, status TEXT NOT NULL)").Error)
			require.NoError(t, db.Exec("CREATE TABLE edge_lease_fundings (id INTEGER PRIMARY KEY)").Error)
			require.NoError(t, db.Exec("INSERT INTO edge_quota_leases (status) VALUES (?)", test.status).Error)

			err = migrateRemovedEdgeLeaseSchema()
			if test.wantError {
				require.Error(t, err)
				assert.True(t, db.Migrator().HasTable("edge_quota_leases"))
				return
			}
			require.NoError(t, err)
			assert.False(t, db.Migrator().HasTable("edge_quota_leases"))
			assert.False(t, db.Migrator().HasTable("edge_lease_fundings"))
		})
	}
}

func TestMasterLeaseSchemaMigrationRemovesLegacyNodeQuotaLimit(t *testing.T) {
	previousDB := DB
	previousMainType := common.MainDatabaseType()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "master.db")), &gorm.Config{})
	require.NoError(t, err)
	DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.LogDatabaseType())
	t.Cleanup(func() {
		DB = previousDB
		common.SetDatabaseTypes(previousMainType, common.LogDatabaseType())
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&legacyEdgeNodeQuotaLimitMigration{}))
	require.NoError(t, db.Exec("INSERT INTO edge_nodes (node_uid, max_outstanding_quota, updated_at) VALUES (?, ?, ?)", "edge.legacy", 500000, 1).Error)
	assert.True(t, db.Migrator().HasTable(&legacyEdgeNodeQuotaLimitMigration{}))
	assert.True(t, db.Migrator().HasColumn(&legacyEdgeNodeQuotaLimitMigration{}, "MaxOutstandingQuota"))

	require.NoError(t, migrateRemovedEdgeLeaseSchema())
	columns, err := db.Migrator().ColumnTypes("edge_nodes")
	require.NoError(t, err)
	columnNames := make([]string, 0, len(columns))
	for _, column := range columns {
		columnNames = append(columnNames, column.Name())
	}
	assert.NotContains(t, columnNames, "max_outstanding_quota")
	var nodeUID string
	require.NoError(t, db.Table("edge_nodes").Select("node_uid").Scan(&nodeUID).Error)
	assert.Equal(t, "edge.legacy", nodeUID)
}

type edgeNodeBalanceNotNullMigrationTest struct {
	ID                        int64  `gorm:"primaryKey"`
	SettlementCircuitOpen     bool   `gorm:"not null"`
	SettlementCircuitOpenedAt int64  `gorm:"type:bigint;not null"`
	SettlementCircuitReason   string `gorm:"type:text;not null"`
	SettlementCircuitEpoch    int64  `gorm:"type:bigint;not null"`
}

func (edgeNodeBalanceNotNullMigrationTest) TableName() string { return "edge_nodes" }

type edgeHeartbeatBalanceNotNullMigrationTest struct {
	ID              int64 `gorm:"primaryKey"`
	BalanceRevision int64 `gorm:"type:bigint;not null"`
}

func (edgeHeartbeatBalanceNotNullMigrationTest) TableName() string {
	return "edge_node_heartbeats"
}

type edgeUsageBalanceNotNullMigrationTest struct {
	ID                  int64  `gorm:"primaryKey"`
	SnapshotID          int64  `gorm:"type:bigint;not null"`
	SnapshotRevision    int64  `gorm:"type:bigint;not null"`
	PricingRevision     int64  `gorm:"type:bigint;not null"`
	BalanceRevision     int64  `gorm:"type:bigint;not null"`
	FundingSource       string `gorm:"type:varchar(32);not null"`
	UserSubscriptionID  int64  `gorm:"type:bigint;not null"`
	TokenUnlimitedQuota bool   `gorm:"not null"`
}

func (edgeUsageBalanceNotNullMigrationTest) TableName() string { return "edge_usage_events" }

func TestMasterBalanceSchemaMigrationBackfillsLegacyRowsBeforeNotNullMigration(t *testing.T) {
	previousDB := DB
	previousMainType := common.MainDatabaseType()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "master.db")), &gorm.Config{})
	require.NoError(t, err)
	DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.LogDatabaseType())
	t.Cleanup(func() {
		DB = previousDB
		common.SetDatabaseTypes(previousMainType, common.LogDatabaseType())
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	exerciseMasterBalanceSchemaMigration(t, db)
}

func TestMasterBalanceSchemaMigrationDialects(t *testing.T) {
	tests := []struct {
		name    string
		envName string
		dbType  common.DatabaseType
		open    func(string) gorm.Dialector
	}{
		{name: "mysql", envName: "EDGE_BALANCE_MIGRATION_TEST_MYSQL_DSN", dbType: common.DatabaseTypeMySQL, open: func(dsn string) gorm.Dialector { return mysql.Open(dsn) }},
		{name: "postgres", envName: "EDGE_BALANCE_MIGRATION_TEST_POSTGRES_DSN", dbType: common.DatabaseTypePostgreSQL, open: func(dsn string) gorm.Dialector { return postgres.Open(dsn) }},
	}
	run := false
	for _, test := range tests {
		dsn := os.Getenv(test.envName)
		if dsn == "" {
			continue
		}
		run = true
		t.Run(test.name, func(t *testing.T) {
			previousDB := DB
			previousMainType := common.MainDatabaseType()
			db, err := gorm.Open(test.open(dsn), &gorm.Config{})
			require.NoError(t, err)
			DB = db
			common.SetDatabaseTypes(test.dbType, common.LogDatabaseType())
			t.Cleanup(func() {
				_ = db.Migrator().DropTable("edge_consume_log_outboxes", "edge_usage_events", "edge_node_heartbeats", "edge_nodes")
				DB = previousDB
				common.SetDatabaseTypes(previousMainType, common.LogDatabaseType())
				if sqlDB, sqlErr := db.DB(); sqlErr == nil {
					_ = sqlDB.Close()
				}
			})
			require.NoError(t, db.Migrator().DropTable("edge_consume_log_outboxes", "edge_usage_events", "edge_node_heartbeats", "edge_nodes"))
			exerciseMasterBalanceSchemaMigration(t, db)
		})
	}
	if !run {
		t.Skip("set EDGE_BALANCE_MIGRATION_TEST_MYSQL_DSN and/or EDGE_BALANCE_MIGRATION_TEST_POSTGRES_DSN to run SQL dialect integration")
	}
}

func exerciseMasterBalanceSchemaMigration(t *testing.T, db *gorm.DB) {
	t.Helper()

	require.NoError(t, db.Exec("CREATE TABLE edge_nodes (id INTEGER PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("CREATE TABLE edge_node_heartbeats (id INTEGER PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("CREATE TABLE edge_usage_events (id INTEGER PRIMARY KEY, lease_id INTEGER NOT NULL)").Error)
	require.NoError(t, db.Exec("CREATE TABLE edge_consume_log_outboxes (id INTEGER PRIMARY KEY, status TEXT NOT NULL)").Error)
	require.NoError(t, db.Exec("INSERT INTO edge_nodes (id) VALUES (1)").Error)
	require.NoError(t, db.Exec("INSERT INTO edge_node_heartbeats (id) VALUES (1)").Error)
	require.NoError(t, db.Exec("INSERT INTO edge_usage_events (id, lease_id) VALUES (1, 9)").Error)
	require.NoError(t, db.Exec("INSERT INTO edge_consume_log_outboxes (id, status) VALUES (1, ?)", EdgeConsumeLogOutboxStatusPublished).Error)

	require.NoError(t, migrateLegacyEdgeBalanceSchema())
	require.NoError(t, migrateLegacyEdgeBalanceSchema())
	require.NoError(t, db.AutoMigrate(
		&edgeNodeBalanceNotNullMigrationTest{},
		&edgeHeartbeatBalanceNotNullMigrationTest{},
		&edgeUsageBalanceNotNullMigrationTest{},
	))

	var node edgeNodeBalanceNotNullMigrationTest
	require.NoError(t, db.First(&node, 1).Error)
	assert.False(t, node.SettlementCircuitOpen)
	assert.Zero(t, node.SettlementCircuitOpenedAt)
	assert.Empty(t, node.SettlementCircuitReason)
	assert.Zero(t, node.SettlementCircuitEpoch)

	var heartbeat edgeHeartbeatBalanceNotNullMigrationTest
	require.NoError(t, db.First(&heartbeat, 1).Error)
	assert.Zero(t, heartbeat.BalanceRevision)

	var usage edgeUsageBalanceNotNullMigrationTest
	require.NoError(t, db.First(&usage, 1).Error)
	assert.Zero(t, usage.SnapshotID)
	assert.Zero(t, usage.SnapshotRevision)
	assert.Zero(t, usage.PricingRevision)
	assert.Zero(t, usage.BalanceRevision)
	assert.Equal(t, legacyEdgeUsageFundingSource, usage.FundingSource)
	assert.Zero(t, usage.UserSubscriptionID)
	assert.False(t, usage.TokenUnlimitedQuota)
	assert.True(t, db.Migrator().HasColumn(&legacyEdgeUsageBalanceMigration{}, "lease_id"))
}

func TestMasterBalanceSchemaMigrationRejectsUnpublishedLegacyOutbox(t *testing.T) {
	previousDB := DB
	previousMainType := common.MainDatabaseType()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "master.db")), &gorm.Config{})
	require.NoError(t, err)
	DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.LogDatabaseType())
	t.Cleanup(func() {
		DB = previousDB
		common.SetDatabaseTypes(previousMainType, common.LogDatabaseType())
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	require.NoError(t, db.Exec("CREATE TABLE edge_usage_events (id INTEGER PRIMARY KEY, lease_id INTEGER NOT NULL)").Error)
	require.NoError(t, db.Exec("CREATE TABLE edge_consume_log_outboxes (id INTEGER PRIMARY KEY, status TEXT NOT NULL)").Error)
	require.NoError(t, db.Exec("INSERT INTO edge_usage_events (id, lease_id) VALUES (1, 9)").Error)
	require.NoError(t, db.Exec("INSERT INTO edge_consume_log_outboxes (id, status) VALUES (1, ?)", EdgeConsumeLogOutboxStatusPending).Error)

	err = migrateLegacyEdgeBalanceSchema()
	require.ErrorContains(t, err, "requires all legacy edge consume log outboxes to be published")
	assert.False(t, db.Migrator().HasColumn(&legacyEdgeUsageBalanceMigration{}, "snapshot_id"))
}
