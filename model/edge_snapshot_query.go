package model

import (
	"errors"

	"github.com/QuantumNous/new-api/dto"

	"gorm.io/gorm"
)

// GetPublishedEdgeCompiledSnapshotForLeaseTx resolves the exact snapshot an
// edge used for authorization. New leases require the currently published,
// unexpired snapshot; settlements keep referring to the immutable row even
// after a newer publication retires it.
func GetPublishedEdgeCompiledSnapshotForLeaseTx(tx *gorm.DB, snapshotUID string, revision int64, nowUnixSeconds int64) (*EdgeCompiledSnapshot, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if err := validateEdgeStoredIdentifier("snapshot UID", snapshotUID); err != nil {
		return nil, err
	}
	if revision <= 0 || nowUnixSeconds <= 0 {
		return nil, errors.New("invalid edge snapshot lease query")
	}
	var snapshot EdgeCompiledSnapshot
	if err := tx.Where("snapshot_uid = ? AND revision = ? AND status = ? AND created_at <= ? AND expires_at > ?",
		snapshotUID, revision, EdgeCompiledSnapshotStatusPublished, nowUnixSeconds, nowUnixSeconds).
		First(&snapshot).Error; err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func GetEdgeCompiledSnapshotForSettlementTx(tx *gorm.DB, snapshotID int64, snapshotUID string, revision int64) (*EdgeCompiledSnapshot, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if snapshotID <= 0 || revision <= 0 {
		return nil, errors.New("invalid edge snapshot settlement query")
	}
	if err := validateEdgeStoredIdentifier("snapshot UID", snapshotUID); err != nil {
		return nil, err
	}
	var snapshot EdgeCompiledSnapshot
	if err := tx.Where("id = ? AND snapshot_uid = ? AND revision = ? AND status IN ?",
		snapshotID, snapshotUID, revision,
		[]EdgeCompiledSnapshotStatus{EdgeCompiledSnapshotStatusPublished, EdgeCompiledSnapshotStatusRetired}).
		First(&snapshot).Error; err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func GetEdgeCompiledSnapshotDatasetPagesTx(tx *gorm.DB, snapshotID int64, dataset dto.EdgeSnapshotDatasetV1) ([]EdgeCompiledSnapshotPage, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if snapshotID <= 0 {
		return nil, errors.New("invalid edge snapshot ID")
	}
	var storedDataset EdgeCompiledSnapshotDataset
	if err := tx.Where("snapshot_id = ? AND dataset = ?", snapshotID, dataset).First(&storedDataset).Error; err != nil {
		return nil, err
	}
	var pages []EdgeCompiledSnapshotPage
	if err := tx.Where("dataset_id = ?", storedDataset.ID).Order("ordinal asc").Find(&pages).Error; err != nil {
		return nil, err
	}
	if len(pages) != storedDataset.PageCount {
		return nil, ErrEdgeCompiledSnapshotIncomplete
	}
	return pages, nil
}
