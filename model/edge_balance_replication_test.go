package model

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/glebarez/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestPrepareEdgeBalanceDeltaTxDiffRevisionAndSettlementWatermark(t *testing.T) {
	forEachEdgeBalanceTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		now := time.Unix(1_800_000_000, 0)
		require.NoError(t, db.Create(&User{Id: 1, Username: "balance-user-1", AffCode: "balance-aff-1", Quota: 100}).Error)
		require.NoError(t, db.Create(&Token{Id: 10, UserId: 1, Key: "balance-token-1", RemainQuota: 50}).Error)
		require.NoError(t, db.Create(&UserSubscription{
			Id: 20, UserId: 1, PlanId: 1, AmountTotal: 1000, AmountUsed: 250,
			Status: "active", StartTime: now.Add(-time.Hour).Unix(), EndTime: now.Add(time.Hour).Unix(),
			NextResetTime: now.Add(30 * time.Minute).Unix(), AllowWalletOverflow: true,
		}).Error)

		var initial *dto.EdgeBalanceDeltaV2
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			initial, err = PrepareEdgeBalanceDeltaTx(tx, 1, 1, 0, 0, now)
			return err
		}))
		require.NotNil(t, initial)
		assert.True(t, initial.Full)
		assert.Equal(t, int64(1), initial.Revision)
		assert.Equal(t, []dto.EdgeWalletBalanceV2{{UserID: 1, RemainQuota: 100}}, initial.Wallets)
		assert.Equal(t, int64(750), initial.Subscriptions[0].RemainQuota)

		var repeated *dto.EdgeBalanceDeltaV2
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			repeated, err = PrepareEdgeBalanceDeltaTx(tx, 1, 1, 0, 0, now)
			return err
		}))
		assert.Equal(t, initial, repeated)

		require.NoError(t, db.Model(&User{}).Where("id = ?", 1).Update("quota", 90).Error)
		require.NoError(t, db.Delete(&Token{}, 10).Error)
		require.NoError(t, db.Create(&User{Id: 2, Username: "balance-user-2", AffCode: "balance-aff-2", Quota: 200}).Error)
		require.NoError(t, db.Model(&UserSubscription{}).Where("id = ?", 20).Updates(map[string]interface{}{
			"amount_used": 300,
			"end_time":    now.Add(14 * 24 * time.Hour).Unix(),
		}).Error)

		var changed *dto.EdgeBalanceDeltaV2
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			changed, err = PrepareEdgeBalanceDeltaTx(tx, 1, 1, 1, 1, now)
			return err
		}))
		require.NotNil(t, changed)
		assert.False(t, changed.Full)
		assert.Equal(t, int64(1), changed.BaseRevision)
		assert.Equal(t, int64(2), changed.Revision)
		assert.Equal(t, []dto.EdgeWalletBalanceV2{{UserID: 1, RemainQuota: 90}, {UserID: 2, RemainQuota: 200}}, changed.Wallets)
		assert.Equal(t, []dto.EdgeTokenBalanceV2{{TokenID: 10, UserID: 1, Deleted: true}}, changed.Tokens)
		assert.Equal(t, int64(700), changed.Subscriptions[0].RemainQuota)
		assert.Equal(t, now.Add(14*24*time.Hour).UnixMilli(), changed.Subscriptions[0].ExpiresAtUnixMilli)

		var stable *dto.EdgeBalanceDeltaV2
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			stable, err = PrepareEdgeBalanceDeltaTx(tx, 1, 1, 2, 1, now)
			return err
		}))
		assert.Nil(t, stable)

		var watermark *dto.EdgeBalanceDeltaV2
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			watermark, err = PrepareEdgeBalanceDeltaTx(tx, 1, 1, 2, 5, now)
			return err
		}))
		require.NotNil(t, watermark)
		assert.Empty(t, watermark.Wallets)
		assert.Equal(t, int64(5), watermark.SettlementAppliedThroughSequence)

		var generationFull *dto.EdgeBalanceDeltaV2
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			generationFull, err = PrepareEdgeBalanceDeltaTx(tx, 1, 2, 0, 5, now)
			return err
		}))
		require.NotNil(t, generationFull)
		assert.True(t, generationFull.Full)

		var generationRepeated *dto.EdgeBalanceDeltaV2
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			generationRepeated, err = PrepareEdgeBalanceDeltaTx(tx, 1, 2, 0, 5, now)
			return err
		}))
		assert.Equal(t, generationFull, generationRepeated)

		var full *dto.EdgeBalanceDeltaV2
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			full, err = PrepareEdgeBalanceDeltaTx(tx, 1, 1, 99, 5, now)
			return err
		}))
		require.NotNil(t, full)
		assert.True(t, full.Full)
		assert.Equal(t, int64(99), full.BaseRevision)
		assert.Equal(t, int64(100), full.Revision)
	})
}

func TestPrepareEdgeBalanceDeltaTxConcurrentHeartbeatIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:edge-balance-concurrent?mode=memory&cache=shared&_busy_timeout=30000"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &UserSubscription{}, &EdgeBalanceReplicationState{}))
	require.NoError(t, db.Create(&User{Id: 1, Username: "concurrent-user", AffCode: "concurrent-aff", Quota: 100}).Error)

	start := make(chan struct{})
	results := make(chan *dto.EdgeBalanceDeltaV2, 2)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			var delta *dto.EdgeBalanceDeltaV2
			err := db.Transaction(func(tx *gorm.DB) error {
				var prepareErr error
				delta, prepareErr = PrepareEdgeBalanceDeltaTx(tx, 1, 1, 0, 0, time.Unix(1_800_000_000, 0))
				return prepareErr
			})
			if err != nil {
				errs <- err
				return
			}
			results <- delta
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var received []*dto.EdgeBalanceDeltaV2
	for delta := range results {
		received = append(received, delta)
	}
	require.Len(t, received, 2)
	assert.Equal(t, received[0], received[1])
}

func forEachEdgeBalanceTestDatabase(t *testing.T, test func(*testing.T, *gorm.DB)) {
	t.Helper()
	cases := []struct {
		name      string
		dbType    common.DatabaseType
		dsn       string
		dialector gorm.Dialector
	}{
		{name: "sqlite", dbType: common.DatabaseTypeSQLite, dialector: sqlite.Open("file:edge-balance-test?mode=memory&cache=shared")},
		{name: "mysql", dbType: common.DatabaseTypeMySQL, dsn: os.Getenv("EDGE_BALANCE_TEST_MYSQL_DSN")},
		{name: "postgres", dbType: common.DatabaseTypePostgreSQL, dsn: os.Getenv("EDGE_BALANCE_TEST_POSTGRES_DSN")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dialector := tc.dialector
			if tc.name != "sqlite" {
				if tc.dsn == "" {
					t.Skip("database DSN is not configured")
				}
				if tc.name == "mysql" {
					dialector = mysql.Open(tc.dsn)
				} else {
					dialector = postgres.Open(tc.dsn)
				}
			}
			db, err := gorm.Open(dialector, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			require.NoError(t, db.Migrator().DropTable(&EdgeBalanceReplicationState{}, &UserSubscription{}, &Token{}, &User{}))
			t.Cleanup(func() {
				_ = db.Migrator().DropTable(&EdgeBalanceReplicationState{}, &UserSubscription{}, &Token{}, &User{})
			})
			require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &UserSubscription{}, &EdgeBalanceReplicationState{}))
			previousMainType := common.MainDatabaseType()
			common.SetMainDatabaseType(tc.dbType)
			t.Cleanup(func() { common.SetMainDatabaseType(previousMainType) })
			test(t, db)
		})
	}
}
