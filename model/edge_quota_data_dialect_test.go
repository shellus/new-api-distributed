package model

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestEdgeQuotaDataDialectIntegration(t *testing.T) {
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

			require.NoError(t, db.Migrator().DropTable(&EdgeQuotaDataEvent{}, &EdgeQuotaDataBucket{}, &QuotaData{}))
			t.Cleanup(func() {
				_ = db.Migrator().DropTable(&EdgeQuotaDataEvent{}, &EdgeQuotaDataBucket{}, &QuotaData{})
			})
			previousDB := DB
			previousMainType := common.MainDatabaseType()
			previousLogType := common.LogDatabaseType()
			DB = db
			common.SetDatabaseTypes(testCase.dbType, testCase.dbType)
			t.Cleanup(func() {
				DB = previousDB
				common.SetDatabaseTypes(previousMainType, previousLogType)
			})
			verifyQuotaDataLongModelMigration(t, db)
			require.NoError(t, db.AutoMigrate(&EdgeQuotaDataEvent{}, &EdgeQuotaDataBucket{}))
			if testCase.dbType == common.DatabaseTypeMySQL {
				var prefixLength int64
				require.NoError(t, db.Raw(`SELECT COALESCE(SUB_PART, 0)
					FROM information_schema.statistics
					WHERE table_schema = DATABASE() AND table_name = 'quota_data'
					AND index_name = 'idx_qdt_model_user_name' AND column_name = 'model_name'`).
					Scan(&prefixLength).Error)
				assert.Equal(t, int64(quotaDataModelNameIndexPrefixLength), prefixLength)
				var maxKeyBytes int64
				require.NoError(t, db.Raw(`SELECT SUM(COALESCE(s.SUB_PART, c.CHARACTER_MAXIMUM_LENGTH) * cs.MAXLEN)
					FROM information_schema.statistics s
					JOIN information_schema.columns c
					  ON c.table_schema = s.table_schema AND c.table_name = s.table_name AND c.column_name = s.column_name
					JOIN information_schema.character_sets cs ON cs.CHARACTER_SET_NAME = c.CHARACTER_SET_NAME
					WHERE s.table_schema = DATABASE() AND s.table_name = 'quota_data'
					  AND s.index_name = 'idx_qdt_model_user_name'`).Scan(&maxKeyBytes).Error)
				assert.LessOrEqual(t, maxKeyBytes, int64(767))
			}

			params := QuotaDataLogParams{
				UserID: 1, Username: "edge-user", ModelName: strings.Repeat("m", quotaDataModelNameMaxLength), Quota: 7,
				CreatedAt: 7_201, TokenUsed: 5, UseGroup: "default", TokenID: 2,
				ChannelID: 3, NodeName: "edge.sql",
			}

			const replayPublishers = 12
			replayKey := fmt.Sprintf("%064x", 1)
			start := make(chan struct{})
			errs := make(chan error, replayPublishers)
			var inserted atomic.Int32
			var wait sync.WaitGroup
			for i := 0; i < replayPublishers; i++ {
				wait.Add(1)
				go func() {
					defer wait.Done()
					<-start
					created, createErr := RecordEdgeQuotaDataOnce(context.Background(), replayKey, params)
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

			const distinctPublishers = 15
			start = make(chan struct{})
			errs = make(chan error, distinctPublishers)
			inserted.Store(0)
			wait = sync.WaitGroup{}
			for i := 0; i < distinctPublishers; i++ {
				key := fmt.Sprintf("%064x", i+2)
				wait.Add(1)
				go func() {
					defer wait.Done()
					<-start
					created, createErr := RecordEdgeQuotaDataOnce(context.Background(), key, params)
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
			assert.Equal(t, int32(distinctPublishers), inserted.Load())

			var count int64
			require.NoError(t, db.Model(&EdgeQuotaDataEvent{}).Count(&count).Error)
			assert.Equal(t, int64(distinctPublishers+1), count)
			require.NoError(t, db.Model(&EdgeQuotaDataBucket{}).Count(&count).Error)
			assert.Equal(t, int64(1), count)
			require.NoError(t, db.Model(&QuotaData{}).Count(&count).Error)
			assert.Equal(t, int64(1), count)
			var bucket QuotaData
			require.NoError(t, db.First(&bucket).Error)
			assert.Equal(t, distinctPublishers+1, bucket.Count)
			assert.Equal(t, (distinctPublishers+1)*params.Quota, bucket.Quota)
			assert.Equal(t, (distinctPublishers+1)*params.TokenUsed, bucket.TokenUsed)
			assert.Equal(t, params.ModelName, bucket.ModelName)
		})
	}
	if !configured {
		t.Skip("set OUTBOX_TEST_MYSQL_DSN and/or OUTBOX_TEST_POSTGRES_DSN to run quota-data dialect integration")
	}
}
