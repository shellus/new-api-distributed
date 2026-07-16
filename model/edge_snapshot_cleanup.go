package model

import (
	"errors"

	"gorm.io/gorm"
)

// CleanupObsoleteEdgeCompiledSnapshots removes only expired retired snapshots
// and drafts whose update time is older than staleDraftBefore. Published
// snapshots and unexpired retired snapshots are deliberately never selected,
// because an edge may continue retrying an old snapshot by ID until expiry.
func CleanupObsoleteEdgeCompiledSnapshots(now int64, staleDraftBefore int64) (int64, error) {
	var deleted int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		deleted, err = CleanupObsoleteEdgeCompiledSnapshotsTx(tx, now, staleDraftBefore)
		return err
	})
	return deleted, err
}

func CleanupObsoleteEdgeCompiledSnapshotsTx(tx *gorm.DB, now int64, staleDraftBefore int64) (int64, error) {
	if tx == nil {
		return 0, errors.New("database is nil")
	}
	if err := validateEdgeCompiledSnapshotUnixSeconds("now", now); err != nil {
		return 0, err
	}
	if staleDraftBefore <= 0 || staleDraftBefore >= now {
		return 0, errors.New("stale draft cutoff must be positive and before now")
	}

	var candidates []EdgeCompiledSnapshot
	if err := lockForUpdate(tx).
		Where("(status = ? AND expires_at <= ?) OR (status = ? AND updated_at < ?)",
			EdgeCompiledSnapshotStatusRetired, now,
			EdgeCompiledSnapshotStatusDraft, staleDraftBefore,
		).
		Order("id ASC").
		Find(&candidates).Error; err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	withoutHooks := tx.Session(&gorm.Session{SkipHooks: true})
	snapshotIDs := make([]int64, 0, len(candidates))
	for i := range candidates {
		result := withoutHooks.Where("id = ? AND ((status = ? AND expires_at <= ?) OR (status = ? AND updated_at < ?))",
			candidates[i].ID,
			EdgeCompiledSnapshotStatusRetired, now,
			EdgeCompiledSnapshotStatusDraft, staleDraftBefore,
		).Delete(&EdgeCompiledSnapshot{})
		if result.Error != nil {
			return 0, result.Error
		}
		if result.RowsAffected == 1 {
			snapshotIDs = append(snapshotIDs, candidates[i].ID)
		}
	}
	if len(snapshotIDs) == 0 {
		return 0, nil
	}
	var datasetIDs []int64
	if err := tx.Model(&EdgeCompiledSnapshotDataset{}).
		Where("snapshot_id IN ?", snapshotIDs).
		Pluck("id", &datasetIDs).Error; err != nil {
		return 0, err
	}
	if len(datasetIDs) > 0 {
		if err := withoutHooks.Where("dataset_id IN ?", datasetIDs).Delete(&EdgeCompiledSnapshotPage{}).Error; err != nil {
			return 0, err
		}
	}
	if err := withoutHooks.Where("snapshot_id IN ?", snapshotIDs).Delete(&EdgeCompiledSnapshotDataset{}).Error; err != nil {
		return 0, err
	}
	return int64(len(snapshotIDs)), nil
}
