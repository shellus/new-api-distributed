package model

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	clickhouseclient "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clickhousedriver "gorm.io/driver/clickhouse"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestEdgeConsumeLogSQLOutboxDialectIntegration(t *testing.T) {
	cases := []struct {
		name      string
		envName   string
		dbType    common.DatabaseType
		dialector func(string) gorm.Dialector
	}{
		{name: "mysql", envName: "OUTBOX_TEST_MYSQL_DSN", dbType: common.DatabaseTypeMySQL, dialector: func(dsn string) gorm.Dialector { return mysql.Open(dsn) }},
		{name: "postgres", envName: "OUTBOX_TEST_POSTGRES_DSN", dbType: common.DatabaseTypePostgreSQL, dialector: func(dsn string) gorm.Dialector { return postgres.Open(dsn) }},
	}
	configured := false
	for _, testCase := range cases {
		dsn := os.Getenv(testCase.envName)
		if dsn == "" {
			continue
		}
		configured = true
		t.Run(testCase.name, func(t *testing.T) {
			db, err := gorm.Open(testCase.dialector(dsn), &gorm.Config{PrepareStmt: true})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(32)
			t.Cleanup(func() { _ = sqlDB.Close() })

			require.NoError(t, db.Migrator().DropTable(&EdgeConsumeLogOutbox{}, &Log{}))
			t.Cleanup(func() { _ = db.Migrator().DropTable(&EdgeConsumeLogOutbox{}, &Log{}) })
			require.NoError(t, db.AutoMigrate(&Log{}, &EdgeConsumeLogOutbox{}))
			previousDB := DB
			previousLogDB := LOG_DB
			previousMainType := common.MainDatabaseType()
			previousLogType := common.LogDatabaseType()
			DB = db
			LOG_DB = db
			common.SetDatabaseTypes(testCase.dbType, testCase.dbType)
			t.Cleanup(func() {
				DB = previousDB
				LOG_DB = previousLogDB
				common.SetDatabaseTypes(previousMainType, previousLogType)
			})
			emptyClaim, err := ClaimEdgeConsumeLogOutbox(context.Background(), time.Now(), time.Minute)
			require.NoError(t, err)
			assert.Nil(t, emptyClaim)

			// All supported SQL databases must permit multiple ordinary logs with
			// a NULL billing key while enforcing uniqueness for edge logs.
			require.NoError(t, db.Create(&Log{UserId: 1, CreatedAt: 1, Type: LogTypeSystem, RequestId: "ordinary-1"}).Error)
			require.NoError(t, db.Create(&Log{UserId: 1, CreatedAt: 1, Type: LogTypeSystem, RequestId: "ordinary-2"}).Error)

			key, err := EdgeConsumeLogBillingEventKey("edge.sql", 1, "event-sql")
			require.NoError(t, err)
			newLog := func() *Log {
				return &Log{
					UserId: 1, CreatedAt: 1, Type: LogTypeConsume, PromptTokens: 2, CompletionTokens: 1,
					ModelName: "gpt-test", Quota: 3, ChannelId: 2, TokenId: 3, UseTime: 1,
					Group: "default", RequestId: "request-sql",
				}
			}
			const publishers = 12
			start := make(chan struct{})
			var inserted atomic.Int32
			errs := make(chan error, publishers)
			var wait sync.WaitGroup
			for i := 0; i < publishers; i++ {
				wait.Add(1)
				go func() {
					defer wait.Done()
					<-start
					created, createErr := CreateEdgeConsumeLogOnce(context.Background(), newLog(), key)
					if createErr != nil {
						errs <- createErr
						return
					}
					if created {
						inserted.Add(1)
					}
				}()
			}
			close(start)
			wait.Wait()
			close(errs)
			for createErr := range errs {
				require.NoError(t, createErr)
			}
			assert.Equal(t, int32(1), inserted.Load())
			var logCount int64
			require.NoError(t, db.Model(&Log{}).Where("billing_event_key = ?", key).Count(&logCount).Error)
			assert.Equal(t, int64(1), logCount)

			now := time.Now().Truncate(time.Second)
			require.NoError(t, db.Create(&EdgeConsumeLogOutbox{
				EventID: 1, EventUID: key, Payload: "{}", AvailableAt: now.Unix(),
			}).Error)
			claims := make(chan *EdgeConsumeLogOutbox, publishers)
			claimErrors := make(chan error, publishers)
			start = make(chan struct{})
			wait = sync.WaitGroup{}
			for i := 0; i < publishers; i++ {
				wait.Add(1)
				go func() {
					defer wait.Done()
					<-start
					claim, claimErr := ClaimEdgeConsumeLogOutbox(context.Background(), now, time.Minute)
					if claimErr != nil {
						claimErrors <- claimErr
						return
					}
					if claim != nil {
						claims <- claim
					}
				}()
			}
			close(start)
			wait.Wait()
			close(claims)
			close(claimErrors)
			claimCount := 0
			for range claims {
				claimCount++
			}
			assert.Equal(t, 1, claimCount)
			for claimErr := range claimErrors {
				require.NoError(t, claimErr)
			}
		})
	}
	if !configured {
		t.Skip("set OUTBOX_TEST_MYSQL_DSN and/or OUTBOX_TEST_POSTGRES_DSN to run SQL dialect integration")
	}
}

func TestEdgeConsumeLogClickHouseIntegration(t *testing.T) {
	dsn := os.Getenv("OUTBOX_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("set OUTBOX_TEST_CLICKHOUSE_DSN to run ClickHouse integration")
	}
	db, err := gorm.Open(clickhousedriver.Open(dsn), &gorm.Config{PrepareStmt: false})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	previousLogDB := LOG_DB
	previousLogType := common.LogDatabaseType()
	LOG_DB = db
	common.SetLogDatabaseType(common.DatabaseTypeClickHouse)
	t.Cleanup(func() {
		LOG_DB = previousLogDB
		common.SetLogDatabaseType(previousLogType)
	})
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS logs").Error)
	t.Cleanup(func() { _ = db.Exec("DROP TABLE IF EXISTS logs").Error })
	require.NoError(t, migrateClickHouseLogDB())

	key, err := EdgeConsumeLogBillingEventKey("edge.clickhouse", 1, "event-clickhouse")
	require.NoError(t, err)
	newLog := func() *Log {
		keyCopy := key
		return &Log{
			UserId: 1, CreatedAt: 1, Type: LogTypeConsume, PromptTokens: 2, CompletionTokens: 1,
			ModelName: "gpt-test", Quota: 3, ChannelId: 2, TokenId: 3, UseTime: 1,
			Group: "default", RequestId: "request-clickhouse", BillingEventKey: &keyCopy,
		}
	}
	insertContext := clickhouseclient.Context(context.Background(), clickhouseclient.WithSettings(clickhouseclient.Settings{
		"insert_deduplication_token": key,
	}))
	require.NoError(t, db.WithContext(insertContext).Create(newLog()).Error)
	require.NoError(t, db.WithContext(insertContext).Create(newLog()).Error)

	var count int64
	require.NoError(t, db.Model(&Log{}).Where("billing_event_key = ?", key).Count(&count).Error)
	assert.Equal(t, int64(1), count)
	inserted, err := CreateEdgeConsumeLogOnce(context.Background(), newLog(), key)
	require.NoError(t, err)
	assert.False(t, inserted)
	var createSQL string
	require.NoError(t, db.Raw("SHOW CREATE TABLE logs").Scan(&createSQL).Error)
	assert.Contains(t, createSQL, "non_replicated_deduplication_window = 100000")
}
