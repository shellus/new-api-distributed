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

func TestMasterLeaseTableMigrationDropsOnlyDrainedLegacyState(t *testing.T) {
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

			err = migrateRemovedEdgeLeaseTables()
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
