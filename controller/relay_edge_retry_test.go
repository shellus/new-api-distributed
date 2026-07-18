package controller

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateEdgeRetrySnapshotPinsBalancePolicyAcrossExpiry(t *testing.T) {
	db, err := model.OpenEdgeSQLite(filepath.Join(t.TempDir(), "edge-retry.db"))
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB })

	now := time.Now().UnixMilli()
	require.NoError(t, db.Model(&model.EdgeLocalControlState{}).Where("id = ?", 1).Updates(map[string]any{
		"snapshot_id":                    "snapshot-retry",
		"snapshot_revision":              int64(7),
		"snapshot_applied_at_unix_milli": now,
		"snapshot_expires_at_unix_milli": now + int64(time.Minute/time.Millisecond),
		"updated_at_unix_milli":          now,
	}).Error)
	require.NoError(t, db.Create(&model.EdgeLocalDatasetState{
		Dataset: dto.EdgeSnapshotDatasetPricingV1, Revision: 11,
	}).Error)
	info := &relaycommon.RelayInfo{
		EdgeSnapshotID:       "snapshot-retry",
		EdgeSnapshotRevision: 7,
		EdgePricingRevision:  11,
	}

	require.NoError(t, validateEdgeRetrySnapshot(info))

	info.EdgeSnapshotRevision = 6
	assert.ErrorContains(t, validateEdgeRetrySnapshot(info), "cross-snapshot retry is denied")
	info.EdgeSnapshotRevision = 7

	info.EdgePricingRevision = 10
	assert.ErrorContains(t, validateEdgeRetrySnapshot(info), "cross-snapshot retry is denied")
	info.EdgePricingRevision = 11

	require.NoError(t, db.Model(&model.EdgeLocalControlState{}).Where("id = ?", 1).
		Update("snapshot_expires_at_unix_milli", now-1).Error)
	require.NoError(t, validateEdgeRetrySnapshot(info))
}

func TestValidateEdgeRetrySnapshotRejectsMissingBalancePin(t *testing.T) {
	assert.ErrorContains(t, validateEdgeRetrySnapshot(&relaycommon.RelayInfo{}), "no pinned balance snapshot")
}
