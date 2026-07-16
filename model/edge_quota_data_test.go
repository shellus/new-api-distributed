package model

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRecordEdgeQuotaDataOnceIsIdempotentAndRejectsConflicts(t *testing.T) {
	db := newEdgeQuotaDataTestDB(t, "idempotency")

	key := strings.Repeat("a", 64)
	params := QuotaDataLogParams{
		UserID: 1, Username: "edge-user", ModelName: strings.Repeat("m", quotaDataModelNameMaxLength), Quota: 120,
		CreatedAt: 7_201, TokenUsed: 110, UseGroup: "default", TokenID: 2,
		ChannelID: 3, NodeName: "edge.test",
	}
	created, err := RecordEdgeQuotaDataOnce(context.Background(), key, params)
	require.NoError(t, err)
	assert.True(t, created)
	created, err = RecordEdgeQuotaDataOnce(context.Background(), key, params)
	require.NoError(t, err)
	assert.False(t, created)
	renamed := params
	renamed.Username = "renamed-edge-user"
	created, err = RecordEdgeQuotaDataOnce(context.Background(), key, renamed)
	require.NoError(t, err)
	assert.False(t, created)

	var bucket QuotaData
	require.NoError(t, db.First(&bucket).Error)
	assert.Equal(t, int64(7_200), bucket.CreatedAt)
	assert.Equal(t, 1, bucket.Count)
	assert.Equal(t, 120, bucket.Quota)
	assert.Equal(t, 110, bucket.TokenUsed)
	assert.Equal(t, params.ModelName, bucket.ModelName)
	var markers int64
	require.NoError(t, db.Model(&EdgeQuotaDataEvent{}).Count(&markers).Error)
	assert.Equal(t, int64(1), markers)
	var marker EdgeQuotaDataEvent
	require.NoError(t, db.First(&marker).Error)
	assert.Contains(t, marker.Payload, `"username":"edge-user"`)
	assert.Equal(t, params.CreatedAt, marker.ProjectedAt)
	var buckets int64
	require.NoError(t, db.Model(&EdgeQuotaDataBucket{}).Count(&buckets).Error)
	assert.Equal(t, int64(1), buckets)

	conflict := params
	conflict.Quota++
	_, err = RecordEdgeQuotaDataOnce(context.Background(), key, conflict)
	assert.ErrorContains(t, err, "different projection")
	require.NoError(t, db.First(&bucket).Error)
	assert.Equal(t, 1, bucket.Count)
	assert.Equal(t, 120, bucket.Quota)

	_, err = RecordEdgeQuotaDataOnce(context.Background(), strings.Repeat("A", 64), params)
	assert.ErrorContains(t, err, "lowercase SHA-256")
	oversized := params
	oversized.Quota = common.MaxQuota + 1
	_, err = RecordEdgeQuotaDataOnce(context.Background(), strings.Repeat("b", 64), oversized)
	assert.ErrorContains(t, err, "invalid edge quota-data projection")
	oversized = params
	oversized.TokenUsed = common.MaxQuota + 1
	_, err = RecordEdgeQuotaDataOnce(context.Background(), strings.Repeat("c", 64), oversized)
	assert.ErrorContains(t, err, "invalid edge quota-data projection")
	oversized = params
	oversized.ModelName = strings.Repeat("m", quotaDataModelNameMaxLength+1)
	_, err = RecordEdgeQuotaDataOnce(context.Background(), strings.Repeat("d", 64), oversized)
	assert.ErrorContains(t, err, "invalid edge quota-data projection")
}

func TestQuotaDataLongModelSQLiteMigration(t *testing.T) {
	db, err := OpenEdgeSQLite(filepath.Join(t.TempDir(), "long-model-migration.db"))
	require.NoError(t, err)
	previousDB := DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		DB = previousDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	verifyQuotaDataLongModelMigration(t, db)
}

func TestRecordEdgeQuotaDataOnceSerializesConcurrentBucketProjection(t *testing.T) {
	db := newEdgeQuotaDataTestDB(t, "concurrent")
	params := QuotaDataLogParams{
		UserID: 1, Username: "edge-user", ModelName: "gpt-test", Quota: 7,
		CreatedAt: 7_201, TokenUsed: 5, UseGroup: "default", TokenID: 2,
		ChannelID: 3, NodeName: "edge.test",
	}

	const publishers = 16
	start := make(chan struct{})
	errs := make(chan error, publishers)
	var inserted atomic.Int32
	var wait sync.WaitGroup
	for i := 0; i < publishers; i++ {
		key := fmt.Sprintf("%064x", i+1)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			created, err := RecordEdgeQuotaDataOnce(context.Background(), key, params)
			if err != nil {
				errs <- err
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
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(publishers), inserted.Load())

	var rowCount int64
	require.NoError(t, db.Model(&QuotaData{}).Count(&rowCount).Error)
	assert.Equal(t, int64(1), rowCount)
	var bucket QuotaData
	require.NoError(t, db.First(&bucket).Error)
	assert.Equal(t, publishers, bucket.Count)
	assert.Equal(t, publishers*params.Quota, bucket.Quota)
	assert.Equal(t, publishers*params.TokenUsed, bucket.TokenUsed)
	require.NoError(t, db.Model(&EdgeQuotaDataEvent{}).Count(&rowCount).Error)
	assert.Equal(t, int64(publishers), rowCount)
	require.NoError(t, db.Model(&EdgeQuotaDataBucket{}).Count(&rowCount).Error)
	assert.Equal(t, int64(1), rowCount)
}

func newEdgeQuotaDataTestDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	previousDB := DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	db, err := OpenEdgeSQLite(filepath.Join(t.TempDir(), name+".db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&QuotaData{}, &EdgeQuotaDataEvent{}, &EdgeQuotaDataBucket{}))
	DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		DB = previousDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

type legacyQuotaData struct {
	Id        int
	UserID    int    `gorm:"index"`
	Username  string `gorm:"index:idx_qdt_model_user_name,priority:2;size:64;default:''"`
	ModelName string `gorm:"index:idx_qdt_model_user_name,priority:1;size:64;default:''"`
	CreatedAt int64  `gorm:"bigint;index:idx_qdt_created_at,priority:2"`
	UseGroup  string `gorm:"index;size:64;default:''"`
	TokenID   int    `gorm:"index;default:0"`
	ChannelID int    `gorm:"index;default:0"`
	NodeName  string `gorm:"index;size:64;default:''"`
	TokenUsed int    `gorm:"default:0"`
	Count     int    `gorm:"default:0"`
	Quota     int    `gorm:"default:0"`
}

func (legacyQuotaData) TableName() string { return "quota_data" }

func verifyQuotaDataLongModelMigration(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&legacyQuotaData{}))
	legacy := legacyQuotaData{
		UserID: 9, Username: "legacy-user", ModelName: "legacy-model", CreatedAt: 3_600,
		UseGroup: "default", TokenID: 8, ChannelID: 7, NodeName: "legacy.node",
		TokenUsed: 5, Count: 1, Quota: 6,
	}
	require.NoError(t, db.Create(&legacy).Error)
	require.NoError(t, prepareQuotaDataModelNameMigration())
	require.NoError(t, db.AutoMigrate(&QuotaData{}))
	require.NoError(t, prepareQuotaDataModelNameMigration())

	var preserved QuotaData
	require.NoError(t, db.First(&preserved, legacy.Id).Error)
	assert.Equal(t, legacy.ModelName, preserved.ModelName)
	require.NoError(t, db.Delete(&preserved).Error)
}
