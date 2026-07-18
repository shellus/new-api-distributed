package model

import (
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
