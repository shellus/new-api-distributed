package model

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeLocalLeaseSchemaMigrationDropsCleanLegacyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge.db")
	db, err := OpenEdgeSQLite(path)
	require.NoError(t, err)
	require.NoError(t, db.Exec("ALTER TABLE edge_local_quota_reservations ADD COLUMN lease_id TEXT NOT NULL DEFAULT ''").Error)
	require.NoError(t, db.Exec("ALTER TABLE edge_local_usage_events ADD COLUMN lease_id TEXT NOT NULL DEFAULT ''").Error)
	require.NoError(t, db.Exec("CREATE TABLE edge_local_quota_leases (lease_id TEXT PRIMARY KEY, status TEXT NOT NULL)").Error)
	require.NoError(t, db.Exec("CREATE TABLE edge_local_lease_acquire_intents (request_id TEXT PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("INSERT INTO edge_local_quota_leases (lease_id, status) VALUES (?, ?)", "lease-closed", "closed").Error)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	db, err = OpenEdgeSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	assert.False(t, db.Migrator().HasTable("edge_local_quota_leases"))
	assert.False(t, db.Migrator().HasTable("edge_local_lease_acquire_intents"))
	assert.False(t, db.Migrator().HasColumn("edge_local_quota_reservations", "lease_id"))
	assert.False(t, db.Migrator().HasColumn("edge_local_usage_events", "lease_id"))
}

func TestEdgeLocalLeaseSchemaMigrationRejectsDirtyAccounting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge.db")
	db, err := OpenEdgeSQLite(path)
	require.NoError(t, err)
	now := time.Now().UnixMilli()
	require.NoError(t, db.Create(&EdgeLocalQuotaReservation{
		ReservationID: "reservation-dirty", RequestID: "request-dirty", UserID: 1, TokenID: 2,
		FundingAccountType: EdgeBalanceAccountTypeWallet, FundingAccountID: 1, TokenAccountID: 2,
		Status: EdgeLocalReservationStatusActive, CreatedAtUnixMilli: now, UpdatedAtUnixMilli: now,
	}).Error)
	require.NoError(t, db.Exec("CREATE TABLE edge_local_quota_leases (lease_id TEXT PRIMARY KEY, status TEXT NOT NULL)").Error)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	_, err = OpenEdgeSQLite(path)
	require.Error(t, err)
	assert.ErrorContains(t, err, "active reservation")
}
