package model

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgesnapshot"

	"gorm.io/gorm"
)

const (
	edgeCompiledSnapshotMinUnixSeconds = int64(946684800)    // 2000-01-01T00:00:00Z
	edgeCompiledSnapshotMaxUnixSeconds = int64(253402300799) // 9999-12-31T23:59:59Z
)

type EdgeCompiledSnapshotStatus string

const (
	EdgeCompiledSnapshotStatusDraft     EdgeCompiledSnapshotStatus = "draft"
	EdgeCompiledSnapshotStatusPublished EdgeCompiledSnapshotStatus = "published"
	EdgeCompiledSnapshotStatusRetired   EdgeCompiledSnapshotStatus = "retired"
)

var (
	ErrInvalidEdgeCompiledSnapshotStatus  = errors.New("invalid edge compiled snapshot status")
	ErrEdgeCompiledSnapshotImmutable      = errors.New("published edge compiled snapshot content is immutable")
	ErrEdgeCompiledSnapshotIncomplete     = errors.New("edge compiled snapshot is incomplete")
	ErrEdgeCompiledSnapshotExpired        = errors.New("edge compiled snapshot is expired")
	ErrEdgeCompiledSnapshotDigestMismatch = errors.New("edge compiled snapshot digest does not match persisted content")
)

var edgeCompiledSnapshotDatasetOrder = [...]dto.EdgeSnapshotDatasetV1{
	dto.EdgeSnapshotDatasetAuthenticationV1,
	dto.EdgeSnapshotDatasetUsersV1,
	dto.EdgeSnapshotDatasetGroupsV1,
	dto.EdgeSnapshotDatasetModelsV1,
	dto.EdgeSnapshotDatasetChannelsV1,
	dto.EdgeSnapshotDatasetPricingV1,
	dto.EdgeSnapshotDatasetRoutingV1,
}

// EdgeCompiledSnapshot is the master-side immutable manifest compiled for an
// edge. SigningPublicKey contains public verification material only; private
// snapshot-signing keys must never be persisted here.
type EdgeCompiledSnapshot struct {
	ID                        int64                      `json:"id" gorm:"primaryKey"`
	SnapshotUID               string                     `json:"snapshot_uid" gorm:"type:varchar(64);not null;uniqueIndex"`
	Revision                  int64                      `json:"revision" gorm:"type:bigint;not null;uniqueIndex;index:idx_edge_compiled_snapshots_status_revision,priority:2"`
	ProtocolVersion           string                     `json:"protocol_version" gorm:"type:varchar(32);not null"`
	Status                    EdgeCompiledSnapshotStatus `json:"status" gorm:"type:varchar(32);not null;index:idx_edge_compiled_snapshots_status_revision,priority:1"`
	HashAlgorithm             string                     `json:"hash_algorithm" gorm:"type:varchar(16);not null"`
	Digest                    string                     `json:"digest" gorm:"type:char(64);not null"`
	TokenFingerprintAlgorithm string                     `json:"token_fingerprint_algorithm" gorm:"type:varchar(32);not null"`
	TokenFingerprintKeyID     string                     `json:"token_fingerprint_key_id" gorm:"type:varchar(64);not null"`
	TokenFingerprintVersion   int                        `json:"token_fingerprint_version" gorm:"not null"`
	SigningAlgorithm          string                     `json:"signing_algorithm" gorm:"type:varchar(32);not null"`
	SigningKeyID              string                     `json:"signing_key_id" gorm:"type:varchar(64);not null"`
	SigningPublicKey          string                     `json:"signing_public_key" gorm:"type:varchar(128);not null"`
	SigningKeyNotBefore       int64                      `json:"signing_key_not_before" gorm:"type:bigint;not null"`
	SigningKeyExpiresAt       int64                      `json:"signing_key_expires_at" gorm:"type:bigint;not null"`
	CreatedAt                 int64                      `json:"created_at" gorm:"type:bigint;not null;index"`
	ExpiresAt                 int64                      `json:"expires_at" gorm:"type:bigint;not null;index"`
	PublishedAt               int64                      `json:"published_at" gorm:"type:bigint;not null"`
	RetiredAt                 int64                      `json:"retired_at" gorm:"type:bigint;not null"`
	UpdatedAt                 int64                      `json:"updated_at" gorm:"type:bigint;not null;index"`
}

type EdgeCompiledSnapshotDataset struct {
	ID           int64                     `json:"id" gorm:"primaryKey"`
	SnapshotID   int64                     `json:"snapshot_id" gorm:"not null;uniqueIndex:idx_edge_compiled_snapshot_dataset,priority:1"`
	Dataset      dto.EdgeSnapshotDatasetV1 `json:"dataset" gorm:"type:varchar(32);not null;uniqueIndex:idx_edge_compiled_snapshot_dataset,priority:2"`
	Revision     int64                     `json:"revision" gorm:"type:bigint;not null"`
	ItemCount    int64                     `json:"item_count" gorm:"type:bigint;not null"`
	PageCount    int                       `json:"page_count" gorm:"not null"`
	Digest       string                    `json:"digest" gorm:"type:char(64);not null"`
	Signature    string                    `json:"signature" gorm:"type:text;not null"`
	SigningKeyID string                    `json:"signing_key_id" gorm:"type:varchar(64);not null"`
}

type EdgeCompiledSnapshotPage struct {
	ID        int64  `json:"id" gorm:"primaryKey"`
	DatasetID int64  `json:"dataset_id" gorm:"not null;uniqueIndex:idx_edge_compiled_snapshot_page,priority:1"`
	Ordinal   int    `json:"ordinal" gorm:"not null;uniqueIndex:idx_edge_compiled_snapshot_page,priority:2"`
	ItemCount int64  `json:"item_count" gorm:"type:bigint;not null"`
	Digest    string `json:"digest" gorm:"type:char(64);not null"`
	Payload   string `json:"payload" gorm:"type:text;not null"`
}

// EdgeCompiledSnapshotManifest bundles the immutable protocol manifest with
// the public verification key that remains valid for the snapshot lifetime.
type EdgeCompiledSnapshotManifest struct {
	Manifest        dto.EdgeSnapshotManifestV1
	VerificationKey dto.EdgeSnapshotVerificationKeyV1
}

func (s EdgeCompiledSnapshotStatus) Valid() bool {
	switch s {
	case EdgeCompiledSnapshotStatusDraft, EdgeCompiledSnapshotStatusPublished, EdgeCompiledSnapshotStatusRetired:
		return true
	default:
		return false
	}
}

func (s *EdgeCompiledSnapshot) BeforeCreate(_ *gorm.DB) error {
	if s == nil {
		return errors.New("edge compiled snapshot is nil")
	}
	if s.Status == "" {
		s.Status = EdgeCompiledSnapshotStatusDraft
	}
	if s.Status != EdgeCompiledSnapshotStatusDraft {
		return errors.New("new edge compiled snapshot must be draft")
	}
	if s.CreatedAt == 0 {
		s.CreatedAt = common.GetTimestamp()
	}
	if s.UpdatedAt == 0 {
		s.UpdatedAt = s.CreatedAt
	}
	s.PublishedAt = 0
	s.RetiredAt = 0
	return validateEdgeCompiledSnapshot(s)
}

func (s *EdgeCompiledSnapshot) BeforeUpdate(tx *gorm.DB) error {
	if s == nil || s.ID <= 0 {
		return ErrEdgeCompiledSnapshotImmutable
	}
	if err := validateEdgeCompiledSnapshot(s); err != nil {
		return err
	}
	var existing EdgeCompiledSnapshot
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).First(&existing, s.ID).Error; err != nil {
		return err
	}
	if existing.Status == EdgeCompiledSnapshotStatusDraft && s.Status == EdgeCompiledSnapshotStatusDraft {
		return nil
	}
	if existing.Status == EdgeCompiledSnapshotStatusDraft && s.Status == EdgeCompiledSnapshotStatusPublished &&
		edgeCompiledSnapshotContentEqual(&existing, s) && s.PublishedAt > 0 && s.RetiredAt == 0 {
		return nil
	}
	if existing.Status == EdgeCompiledSnapshotStatusPublished && s.Status == EdgeCompiledSnapshotStatusRetired &&
		edgeCompiledSnapshotContentEqual(&existing, s) && existing.PublishedAt == s.PublishedAt &&
		s.RetiredAt >= s.PublishedAt {
		return nil
	}
	return ErrEdgeCompiledSnapshotImmutable
}

func (s *EdgeCompiledSnapshot) BeforeDelete(tx *gorm.DB) error {
	if s == nil || s.ID <= 0 {
		return ErrEdgeCompiledSnapshotImmutable
	}
	var existing EdgeCompiledSnapshot
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).First(&existing, s.ID).Error; err != nil {
		return err
	}
	if existing.Status != EdgeCompiledSnapshotStatusDraft {
		return ErrEdgeCompiledSnapshotImmutable
	}
	return nil
}

func (d *EdgeCompiledSnapshotDataset) BeforeCreate(tx *gorm.DB) error {
	return validateEdgeCompiledSnapshotDatasetTx(tx, d, true)
}

func (d *EdgeCompiledSnapshotDataset) BeforeUpdate(tx *gorm.DB) error {
	if d == nil || d.ID <= 0 {
		return ErrEdgeCompiledSnapshotImmutable
	}
	var existing EdgeCompiledSnapshotDataset
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).First(&existing, d.ID).Error; err != nil {
		return err
	}
	var parent EdgeCompiledSnapshot
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).First(&parent, existing.SnapshotID).Error; err != nil {
		return err
	}
	if parent.Status != EdgeCompiledSnapshotStatusDraft {
		return ErrEdgeCompiledSnapshotImmutable
	}
	return validateEdgeCompiledSnapshotDatasetTx(tx, d, true)
}

func (d *EdgeCompiledSnapshotDataset) BeforeDelete(tx *gorm.DB) error {
	if d == nil || d.ID <= 0 {
		return ErrEdgeCompiledSnapshotImmutable
	}
	var parent EdgeCompiledSnapshot
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
		Table("edge_compiled_snapshots").
		Select("edge_compiled_snapshots.*").
		Joins("JOIN edge_compiled_snapshot_datasets ON edge_compiled_snapshot_datasets.snapshot_id = edge_compiled_snapshots.id").
		Where("edge_compiled_snapshot_datasets.id = ?", d.ID).
		First(&parent).Error; err != nil {
		return err
	}
	if parent.Status != EdgeCompiledSnapshotStatusDraft {
		return ErrEdgeCompiledSnapshotImmutable
	}
	return nil
}

func (p *EdgeCompiledSnapshotPage) BeforeCreate(tx *gorm.DB) error {
	return validateEdgeCompiledSnapshotPageTx(tx, p, true)
}

func (p *EdgeCompiledSnapshotPage) BeforeUpdate(tx *gorm.DB) error {
	if p == nil || p.ID <= 0 {
		return ErrEdgeCompiledSnapshotImmutable
	}
	var parentStatus EdgeCompiledSnapshotStatus
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
		Table("edge_compiled_snapshots").
		Select("edge_compiled_snapshots.status").
		Joins("JOIN edge_compiled_snapshot_datasets ON edge_compiled_snapshot_datasets.snapshot_id = edge_compiled_snapshots.id").
		Joins("JOIN edge_compiled_snapshot_pages ON edge_compiled_snapshot_pages.dataset_id = edge_compiled_snapshot_datasets.id").
		Where("edge_compiled_snapshot_pages.id = ?", p.ID).
		Scan(&parentStatus).Error; err != nil {
		return err
	}
	if parentStatus != EdgeCompiledSnapshotStatusDraft {
		return ErrEdgeCompiledSnapshotImmutable
	}
	return validateEdgeCompiledSnapshotPageTx(tx, p, true)
}

func (p *EdgeCompiledSnapshotPage) BeforeDelete(tx *gorm.DB) error {
	if p == nil || p.ID <= 0 {
		return ErrEdgeCompiledSnapshotImmutable
	}
	var parentStatus EdgeCompiledSnapshotStatus
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
		Table("edge_compiled_snapshots").
		Select("edge_compiled_snapshots.status").
		Joins("JOIN edge_compiled_snapshot_datasets ON edge_compiled_snapshot_datasets.snapshot_id = edge_compiled_snapshots.id").
		Joins("JOIN edge_compiled_snapshot_pages ON edge_compiled_snapshot_pages.dataset_id = edge_compiled_snapshot_datasets.id").
		Where("edge_compiled_snapshot_pages.id = ?", p.ID).
		Scan(&parentStatus).Error; err != nil {
		return err
	}
	if parentStatus != EdgeCompiledSnapshotStatusDraft {
		return ErrEdgeCompiledSnapshotImmutable
	}
	return nil
}

// PublishEdgeCompiledSnapshot atomically publishes one complete draft and
// retires every previously published snapshot in the same database transaction.
func PublishEdgeCompiledSnapshot(snapshotID int64, publishedAt int64) (bool, error) {
	var published bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		published, err = PublishEdgeCompiledSnapshotTx(tx, snapshotID, publishedAt)
		return err
	})
	return published, err
}

func PublishEdgeCompiledSnapshotTx(tx *gorm.DB, snapshotID int64, publishedAt int64) (bool, error) {
	if tx == nil {
		return false, errors.New("database is nil")
	}
	if snapshotID <= 0 {
		return false, errors.New("invalid edge compiled snapshot ID")
	}
	if publishedAt == 0 {
		publishedAt = common.GetTimestamp()
	}
	if err := validateEdgeCompiledSnapshotUnixSeconds("published_at", publishedAt); err != nil {
		return false, err
	}

	// Lock the complete publication set in a stable order. This serializes two
	// concurrent publishers even when no row is published at transaction start.
	var snapshots []EdgeCompiledSnapshot
	if err := lockForUpdate(tx).Order("id ASC").Find(&snapshots).Error; err != nil {
		return false, err
	}
	var target *EdgeCompiledSnapshot
	for i := range snapshots {
		if snapshots[i].ID == snapshotID {
			target = &snapshots[i]
			break
		}
	}
	if target == nil {
		return false, gorm.ErrRecordNotFound
	}
	if target.Status == EdgeCompiledSnapshotStatusRetired {
		return false, ErrEdgeCompiledSnapshotImmutable
	}
	if publishedAt < target.CreatedAt || publishedAt >= target.ExpiresAt {
		return false, ErrEdgeCompiledSnapshotExpired
	}
	if err := validateEdgeCompiledSnapshotGraphTx(tx, target); err != nil {
		return false, err
	}
	if target.Status == EdgeCompiledSnapshotStatusPublished {
		return false, nil
	}
	if target.Status != EdgeCompiledSnapshotStatusDraft {
		return false, ErrEdgeCompiledSnapshotImmutable
	}

	for i := range snapshots {
		if snapshots[i].ID == target.ID || snapshots[i].Status != EdgeCompiledSnapshotStatusPublished {
			continue
		}
		snapshots[i].Status = EdgeCompiledSnapshotStatusRetired
		snapshots[i].RetiredAt = publishedAt
		snapshots[i].UpdatedAt = publishedAt
		if err := tx.Save(&snapshots[i]).Error; err != nil {
			return false, err
		}
	}
	target.Status = EdgeCompiledSnapshotStatusPublished
	target.PublishedAt = publishedAt
	target.UpdatedAt = publishedAt
	if err := tx.Save(target).Error; err != nil {
		return false, err
	}
	return true, nil
}

func RetireEdgeCompiledSnapshot(snapshotID int64, retiredAt int64) (bool, error) {
	var retired bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		retired, err = RetireEdgeCompiledSnapshotTx(tx, snapshotID, retiredAt)
		return err
	})
	return retired, err
}

func RetireEdgeCompiledSnapshotTx(tx *gorm.DB, snapshotID int64, retiredAt int64) (bool, error) {
	if tx == nil {
		return false, errors.New("database is nil")
	}
	if snapshotID <= 0 {
		return false, errors.New("invalid edge compiled snapshot ID")
	}
	if retiredAt == 0 {
		retiredAt = common.GetTimestamp()
	}
	if err := validateEdgeCompiledSnapshotUnixSeconds("retired_at", retiredAt); err != nil {
		return false, err
	}
	var snapshot EdgeCompiledSnapshot
	if err := lockForUpdate(tx).First(&snapshot, snapshotID).Error; err != nil {
		return false, err
	}
	if snapshot.Status == EdgeCompiledSnapshotStatusRetired {
		return false, nil
	}
	if snapshot.Status != EdgeCompiledSnapshotStatusPublished || retiredAt < snapshot.PublishedAt {
		return false, ErrEdgeCompiledSnapshotImmutable
	}
	snapshot.Status = EdgeCompiledSnapshotStatusRetired
	snapshot.RetiredAt = retiredAt
	snapshot.UpdatedAt = retiredAt
	if err := tx.Save(&snapshot).Error; err != nil {
		return false, err
	}
	return true, nil
}

func GetLatestPublishedEdgeCompiledSnapshotManifest(now int64) (*EdgeCompiledSnapshotManifest, error) {
	return GetLatestPublishedEdgeCompiledSnapshotManifestTx(DB, now)
}

func GetLatestPublishedEdgeCompiledSnapshotManifestTx(tx *gorm.DB, now int64) (*EdgeCompiledSnapshotManifest, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if now == 0 {
		now = common.GetTimestamp()
	}
	if err := validateEdgeCompiledSnapshotUnixSeconds("now", now); err != nil {
		return nil, err
	}
	var snapshot EdgeCompiledSnapshot
	if err := tx.Where(
		"status = ? AND published_at > 0 AND published_at <= ? AND created_at <= ? AND expires_at > ?",
		EdgeCompiledSnapshotStatusPublished, now, now, now,
	).Order("revision DESC").First(&snapshot).Error; err != nil {
		return nil, err
	}
	return buildEdgeCompiledSnapshotManifestTx(tx, &snapshot)
}

// GetEdgeCompiledSnapshotManifest resolves a stable snapshot identity for as
// long as its signed lifetime remains valid. Retired snapshots stay readable
// so an edge can finish or retry a paged download across a new publication.
func GetEdgeCompiledSnapshotManifest(snapshotUID string, now int64) (*EdgeCompiledSnapshotManifest, error) {
	return GetEdgeCompiledSnapshotManifestTx(DB, snapshotUID, now)
}

func GetEdgeCompiledSnapshotManifestTx(tx *gorm.DB, snapshotUID string, now int64) (*EdgeCompiledSnapshotManifest, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if err := edgeauth.ValidateNodeID(snapshotUID); err != nil {
		return nil, err
	}
	if now == 0 {
		now = common.GetTimestamp()
	}
	if err := validateEdgeCompiledSnapshotUnixSeconds("now", now); err != nil {
		return nil, err
	}
	var snapshot EdgeCompiledSnapshot
	if err := tx.Where("snapshot_uid = ?", snapshotUID).
		Where("status IN ?", []EdgeCompiledSnapshotStatus{
			EdgeCompiledSnapshotStatusPublished,
			EdgeCompiledSnapshotStatusRetired,
		}).
		Where("published_at > 0 AND published_at <= ? AND created_at <= ? AND expires_at > ?", now, now, now).
		First(&snapshot).Error; err != nil {
		return nil, err
	}
	return buildEdgeCompiledSnapshotManifestTx(tx, &snapshot)
}

func buildEdgeCompiledSnapshotManifestTx(tx *gorm.DB, snapshot *EdgeCompiledSnapshot) (*EdgeCompiledSnapshotManifest, error) {
	if err := validateEdgeCompiledSnapshot(snapshot); err != nil {
		return nil, err
	}
	datasets, err := loadOrderedEdgeCompiledSnapshotDatasetsTx(tx, snapshot)
	if err != nil {
		return nil, err
	}
	publicKey, err := edgeauth.ParsePublicKey(snapshot.SigningPublicKey)
	if err != nil {
		return nil, err
	}

	manifest := dto.EdgeSnapshotManifestV1{
		SnapshotID:         snapshot.SnapshotUID,
		Revision:           snapshot.Revision,
		CreatedAtUnixMilli: snapshot.CreatedAt * 1000,
		ExpiresAtUnixMilli: snapshot.ExpiresAt * 1000,
		HashAlgorithm:      snapshot.HashAlgorithm,
		Digest:             snapshot.Digest,
		TokenFingerprint: dto.EdgeTokenFingerprintSchemeV1{
			Algorithm: snapshot.TokenFingerprintAlgorithm,
			KeyID:     snapshot.TokenFingerprintKeyID,
			Version:   snapshot.TokenFingerprintVersion,
		},
		Datasets: make([]dto.EdgeSnapshotDatasetManifestV1, 0, len(datasets)),
	}
	for i := range datasets {
		if err := validateEdgeCompiledSnapshotDataset(snapshot, &datasets[i], publicKey); err != nil {
			return nil, err
		}
		manifest.Datasets = append(manifest.Datasets, dto.EdgeSnapshotDatasetManifestV1{
			Dataset:   datasets[i].Dataset,
			Revision:  datasets[i].Revision,
			ItemCount: datasets[i].ItemCount,
			PageCount: datasets[i].PageCount,
			Digest:    datasets[i].Digest,
			DetachedSignature: dto.EdgeDetachedContentSignatureV1{
				Algorithm:     snapshot.SigningAlgorithm,
				KeyID:         datasets[i].SigningKeyID,
				PayloadDigest: datasets[i].Digest,
				Value:         datasets[i].Signature,
			},
		})
	}
	verificationKey := dto.EdgeSnapshotVerificationKeyV1{
		KeyID:              snapshot.SigningKeyID,
		Algorithm:          snapshot.SigningAlgorithm,
		PublicKey:          snapshot.SigningPublicKey,
		NotBeforeUnixMilli: snapshot.SigningKeyNotBefore * 1000,
		ExpiresAtUnixMilli: snapshot.SigningKeyExpiresAt * 1000,
	}
	if err := manifest.TokenFingerprint.Validate(); err != nil {
		return nil, err
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if err := verificationKey.Validate(); err != nil {
		return nil, err
	}
	return &EdgeCompiledSnapshotManifest{Manifest: manifest, VerificationKey: verificationKey}, nil
}

// GetPublishedEdgeCompiledSnapshotPage resolves a page by its stable protocol
// identity and serves published or retired snapshots until signed expiry.
func GetPublishedEdgeCompiledSnapshotPage(snapshotUID string, dataset dto.EdgeSnapshotDatasetV1, ordinal int, now int64) (*EdgeCompiledSnapshotPage, error) {
	return GetPublishedEdgeCompiledSnapshotPageTx(DB, snapshotUID, dataset, ordinal, now)
}

func GetPublishedEdgeCompiledSnapshotPageTx(tx *gorm.DB, snapshotUID string, dataset dto.EdgeSnapshotDatasetV1, ordinal int, now int64) (*EdgeCompiledSnapshotPage, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if err := edgeauth.ValidateNodeID(snapshotUID); err != nil {
		return nil, err
	}
	if !dataset.Valid() {
		return nil, errors.New("invalid edge snapshot dataset")
	}
	if ordinal < 0 {
		return nil, errors.New("edge snapshot page ordinal cannot be negative")
	}
	if now == 0 {
		now = common.GetTimestamp()
	}
	if err := validateEdgeCompiledSnapshotUnixSeconds("now", now); err != nil {
		return nil, err
	}
	var page EdgeCompiledSnapshotPage
	if err := tx.Table("edge_compiled_snapshot_pages").
		Select("edge_compiled_snapshot_pages.*").
		Joins("JOIN edge_compiled_snapshot_datasets ON edge_compiled_snapshot_datasets.id = edge_compiled_snapshot_pages.dataset_id").
		Joins("JOIN edge_compiled_snapshots ON edge_compiled_snapshots.id = edge_compiled_snapshot_datasets.snapshot_id").
		Where("edge_compiled_snapshots.snapshot_uid = ?", snapshotUID).
		Where("edge_compiled_snapshots.status IN ?", []EdgeCompiledSnapshotStatus{
			EdgeCompiledSnapshotStatusPublished,
			EdgeCompiledSnapshotStatusRetired,
		}).
		Where("edge_compiled_snapshots.published_at > 0 AND edge_compiled_snapshots.published_at <= ?", now).
		Where("edge_compiled_snapshots.created_at <= ? AND edge_compiled_snapshots.expires_at > ?", now, now).
		Where("edge_compiled_snapshot_datasets.dataset = ?", dataset).
		Where("edge_compiled_snapshot_pages.ordinal = ?", ordinal).
		First(&page).Error; err != nil {
		return nil, err
	}
	if err := validateEdgeCompiledSnapshotPagePayload(&page, dataset); err != nil {
		return nil, err
	}
	return &page, nil
}

func validateEdgeCompiledSnapshot(snapshot *EdgeCompiledSnapshot) error {
	if snapshot == nil {
		return errors.New("edge compiled snapshot is nil")
	}
	if err := edgeauth.ValidateNodeID(snapshot.SnapshotUID); err != nil {
		return fmt.Errorf("snapshot_uid: %w", err)
	}
	if snapshot.Revision <= 0 {
		return errors.New("edge compiled snapshot revision must be greater than zero")
	}
	if snapshot.ProtocolVersion != dto.EdgeControlProtocolVersionV1 {
		return fmt.Errorf("edge compiled snapshot protocol_version must be %q", dto.EdgeControlProtocolVersionV1)
	}
	if !snapshot.Status.Valid() {
		return ErrInvalidEdgeCompiledSnapshotStatus
	}
	if snapshot.HashAlgorithm != edgesnapshot.HashAlgorithm {
		return fmt.Errorf("edge compiled snapshot hash_algorithm must be %q", edgesnapshot.HashAlgorithm)
	}
	if err := validateEdgeCompiledSnapshotDigest(snapshot.Digest); err != nil {
		return fmt.Errorf("edge compiled snapshot digest: %w", err)
	}
	fingerprint := dto.EdgeTokenFingerprintSchemeV1{
		Algorithm: snapshot.TokenFingerprintAlgorithm,
		KeyID:     snapshot.TokenFingerprintKeyID,
		Version:   snapshot.TokenFingerprintVersion,
	}
	if err := fingerprint.Validate(); err != nil {
		return err
	}
	if snapshot.SigningAlgorithm != edgesnapshot.SignatureAlgorithm {
		return fmt.Errorf("edge compiled snapshot signing_algorithm must be %q", edgesnapshot.SignatureAlgorithm)
	}
	if err := edgeauth.ValidateKeyID(snapshot.SigningKeyID); err != nil {
		return err
	}
	publicKey, err := edgeauth.ParsePublicKey(snapshot.SigningPublicKey)
	if err != nil {
		return err
	}
	canonicalPublicKey, err := edgeauth.EncodePublicKey(publicKey)
	if err != nil {
		return err
	}
	if canonicalPublicKey != snapshot.SigningPublicKey {
		return errors.New("edge compiled snapshot signing public key is not canonical")
	}
	for _, timestamp := range []struct {
		field string
		value int64
	}{
		{field: "signing_key_not_before", value: snapshot.SigningKeyNotBefore},
		{field: "signing_key_expires_at", value: snapshot.SigningKeyExpiresAt},
		{field: "created_at", value: snapshot.CreatedAt},
		{field: "expires_at", value: snapshot.ExpiresAt},
		{field: "updated_at", value: snapshot.UpdatedAt},
	} {
		if err := validateEdgeCompiledSnapshotUnixSeconds(timestamp.field, timestamp.value); err != nil {
			return err
		}
	}
	if snapshot.ExpiresAt <= snapshot.CreatedAt {
		return errors.New("edge compiled snapshot expires_at must be after created_at")
	}
	if snapshot.SigningKeyExpiresAt <= snapshot.SigningKeyNotBefore {
		return errors.New("edge compiled snapshot signing key expiry must be after not-before")
	}
	if snapshot.SigningKeyNotBefore > snapshot.CreatedAt || snapshot.SigningKeyExpiresAt < snapshot.ExpiresAt {
		return errors.New("edge compiled snapshot lifetime must be covered by the signing key")
	}
	verificationKey := dto.EdgeSnapshotVerificationKeyV1{
		KeyID:              snapshot.SigningKeyID,
		Algorithm:          snapshot.SigningAlgorithm,
		PublicKey:          snapshot.SigningPublicKey,
		NotBeforeUnixMilli: snapshot.SigningKeyNotBefore * 1000,
		ExpiresAtUnixMilli: snapshot.SigningKeyExpiresAt * 1000,
	}
	if err := verificationKey.Validate(); err != nil {
		return err
	}
	switch snapshot.Status {
	case EdgeCompiledSnapshotStatusDraft:
		if snapshot.PublishedAt != 0 || snapshot.RetiredAt != 0 {
			return errors.New("draft edge compiled snapshot cannot have publication timestamps")
		}
	case EdgeCompiledSnapshotStatusPublished:
		if err := validateEdgeCompiledSnapshotUnixSeconds("published_at", snapshot.PublishedAt); err != nil {
			return err
		}
		if snapshot.PublishedAt < snapshot.CreatedAt || snapshot.PublishedAt >= snapshot.ExpiresAt || snapshot.RetiredAt != 0 {
			return errors.New("invalid edge compiled snapshot publication timestamps")
		}
	case EdgeCompiledSnapshotStatusRetired:
		if err := validateEdgeCompiledSnapshotUnixSeconds("published_at", snapshot.PublishedAt); err != nil {
			return err
		}
		if err := validateEdgeCompiledSnapshotUnixSeconds("retired_at", snapshot.RetiredAt); err != nil {
			return err
		}
		if snapshot.PublishedAt < snapshot.CreatedAt || snapshot.PublishedAt >= snapshot.ExpiresAt || snapshot.RetiredAt < snapshot.PublishedAt {
			return errors.New("invalid edge compiled snapshot retirement timestamps")
		}
	}
	return nil
}

func validateEdgeCompiledSnapshotDatasetTx(tx *gorm.DB, dataset *EdgeCompiledSnapshotDataset, requireDraft bool) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if dataset == nil || dataset.SnapshotID <= 0 {
		return errors.New("invalid edge compiled snapshot dataset identity")
	}
	var snapshot EdgeCompiledSnapshot
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).First(&snapshot, dataset.SnapshotID).Error; err != nil {
		return err
	}
	if requireDraft && snapshot.Status != EdgeCompiledSnapshotStatusDraft {
		return ErrEdgeCompiledSnapshotImmutable
	}
	publicKey, err := edgeauth.ParsePublicKey(snapshot.SigningPublicKey)
	if err != nil {
		return err
	}
	return validateEdgeCompiledSnapshotDataset(&snapshot, dataset, publicKey)
}

func validateEdgeCompiledSnapshotDataset(snapshot *EdgeCompiledSnapshot, dataset *EdgeCompiledSnapshotDataset, publicKey ed25519.PublicKey) error {
	if snapshot == nil || dataset == nil {
		return errors.New("edge compiled snapshot dataset is nil")
	}
	if !dataset.Dataset.Valid() {
		return errors.New("invalid edge snapshot dataset")
	}
	if dataset.Revision <= 0 || dataset.ItemCount < 0 || dataset.PageCount < 0 {
		return errors.New("invalid edge compiled snapshot dataset counts")
	}
	if dataset.SigningKeyID != snapshot.SigningKeyID {
		return errors.New("edge compiled snapshot dataset signing key does not match snapshot")
	}
	if err := edgeauth.ValidateKeyID(dataset.SigningKeyID); err != nil {
		return err
	}
	manifest := edgesnapshot.DatasetManifest{
		SnapshotID:    snapshot.SnapshotUID,
		Dataset:       string(dataset.Dataset),
		Revision:      dataset.Revision,
		ItemCount:     dataset.ItemCount,
		PageCount:     dataset.PageCount,
		PayloadDigest: dataset.Digest,
	}
	protocolManifest := dto.EdgeSnapshotDatasetManifestV1{
		Dataset:   dataset.Dataset,
		Revision:  dataset.Revision,
		ItemCount: dataset.ItemCount,
		PageCount: dataset.PageCount,
		Digest:    dataset.Digest,
		DetachedSignature: dto.EdgeDetachedContentSignatureV1{
			Algorithm:     snapshot.SigningAlgorithm,
			KeyID:         dataset.SigningKeyID,
			PayloadDigest: dataset.Digest,
			Value:         dataset.Signature,
		},
	}
	if err := protocolManifest.Validate(); err != nil {
		return err
	}
	if dataset.Revision > snapshot.Revision {
		return errors.New("edge compiled snapshot dataset revision cannot exceed snapshot revision")
	}
	return edgesnapshot.VerifyDatasetManifest(publicKey, manifest, dataset.Signature)
}

func validateEdgeCompiledSnapshotPageTx(tx *gorm.DB, page *EdgeCompiledSnapshotPage, requireDraft bool) error {
	if tx == nil {
		return errors.New("database is nil")
	}
	if page == nil || page.DatasetID <= 0 {
		return errors.New("invalid edge compiled snapshot page identity")
	}
	var dataset EdgeCompiledSnapshotDataset
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).First(&dataset, page.DatasetID).Error; err != nil {
		return err
	}
	var parentStatus EdgeCompiledSnapshotStatus
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
		Table("edge_compiled_snapshots").
		Select("edge_compiled_snapshots.status").
		Where("edge_compiled_snapshots.id = ?", dataset.SnapshotID).
		Scan(&parentStatus).Error; err != nil {
		return err
	}
	if requireDraft && parentStatus != EdgeCompiledSnapshotStatusDraft {
		return ErrEdgeCompiledSnapshotImmutable
	}
	return validateEdgeCompiledSnapshotPagePayload(page, dataset.Dataset)
}

func validateEdgeCompiledSnapshotPage(page *EdgeCompiledSnapshotPage) error {
	if page == nil || page.DatasetID <= 0 {
		return errors.New("invalid edge compiled snapshot page identity")
	}
	if page.Ordinal < 0 || page.Ordinal >= dto.EdgeControlMaxSnapshotPagesV1 || page.ItemCount < 0 {
		return errors.New("invalid edge compiled snapshot page counts")
	}
	if err := validateEdgeCompiledSnapshotDigest(page.Digest); err != nil {
		return fmt.Errorf("edge compiled snapshot page digest: %w", err)
	}
	if page.Payload == "" {
		return errors.New("edge compiled snapshot page payload is empty")
	}
	var raw json.RawMessage
	if err := common.UnmarshalJsonStr(page.Payload, &raw); err != nil {
		return fmt.Errorf("edge compiled snapshot page payload is invalid JSON: %w", err)
	}
	canonical, digest, err := edgesnapshot.MarshalPagePayload(raw)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, []byte(page.Payload)) || digest != page.Digest {
		return ErrEdgeCompiledSnapshotDigestMismatch
	}
	return nil
}

func validateEdgeCompiledSnapshotPagePayload(page *EdgeCompiledSnapshotPage, dataset dto.EdgeSnapshotDatasetV1) error {
	if err := validateEdgeCompiledSnapshotPage(page); err != nil {
		return err
	}
	if page.ItemCount > int64(dto.EdgeControlMaxSnapshotPageLimitV1) {
		return fmt.Errorf("edge compiled snapshot page item_count exceeds %d", dto.EdgeControlMaxSnapshotPageLimitV1)
	}
	var payload dto.EdgeSnapshotPagePayloadV1
	if err := common.UnmarshalJsonStr(page.Payload, &payload); err != nil {
		return fmt.Errorf("edge compiled snapshot page payload is invalid: %w", err)
	}
	if err := payload.Validate(dataset, int(page.ItemCount)); err != nil {
		return err
	}
	canonical, digest, err := edgesnapshot.MarshalPagePayload(payload)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, []byte(page.Payload)) || digest != page.Digest {
		return ErrEdgeCompiledSnapshotDigestMismatch
	}
	return nil
}

func validateEdgeCompiledSnapshotGraphTx(tx *gorm.DB, snapshot *EdgeCompiledSnapshot) error {
	if err := validateEdgeCompiledSnapshot(snapshot); err != nil {
		return err
	}
	datasets, err := loadOrderedEdgeCompiledSnapshotDatasetsTx(tx, snapshot)
	if err != nil {
		return err
	}
	publicKey, err := edgeauth.ParsePublicKey(snapshot.SigningPublicKey)
	if err != nil {
		return err
	}
	datasetManifests := make([]edgesnapshot.DatasetManifest, 0, len(datasets))
	for i := range datasets {
		if err := validateEdgeCompiledSnapshotDataset(snapshot, &datasets[i], publicKey); err != nil {
			return err
		}
		var pages []EdgeCompiledSnapshotPage
		if err := tx.Where("dataset_id = ?", datasets[i].ID).Order("ordinal ASC").Find(&pages).Error; err != nil {
			return err
		}
		if len(pages) != datasets[i].PageCount {
			return fmt.Errorf("%w: dataset %s page count", ErrEdgeCompiledSnapshotIncomplete, datasets[i].Dataset)
		}
		pageDigests := make([]string, 0, len(pages))
		var itemCount int64
		for ordinal := range pages {
			if pages[ordinal].Ordinal != ordinal {
				return fmt.Errorf("%w: dataset %s page ordinals", ErrEdgeCompiledSnapshotIncomplete, datasets[i].Dataset)
			}
			if err := validateEdgeCompiledSnapshotPagePayload(&pages[ordinal], datasets[i].Dataset); err != nil {
				return err
			}
			if pages[ordinal].ItemCount > math.MaxInt64-itemCount {
				return errors.New("edge compiled snapshot item count overflow")
			}
			itemCount += pages[ordinal].ItemCount
			pageDigests = append(pageDigests, pages[ordinal].Digest)
		}
		if itemCount != datasets[i].ItemCount {
			return fmt.Errorf("%w: dataset %s item count", ErrEdgeCompiledSnapshotIncomplete, datasets[i].Dataset)
		}
		digest, err := edgesnapshot.AggregatePageDigests(pageDigests)
		if err != nil {
			return err
		}
		if digest != datasets[i].Digest {
			return fmt.Errorf("%w: dataset %s aggregate", ErrEdgeCompiledSnapshotDigestMismatch, datasets[i].Dataset)
		}
		datasetManifests = append(datasetManifests, edgesnapshot.DatasetManifest{
			SnapshotID:    snapshot.SnapshotUID,
			Dataset:       string(datasets[i].Dataset),
			Revision:      datasets[i].Revision,
			ItemCount:     datasets[i].ItemCount,
			PageCount:     datasets[i].PageCount,
			PayloadDigest: datasets[i].Digest,
		})
	}
	snapshotDigest, err := edgesnapshot.AggregateDatasetManifests(snapshot.SnapshotUID, snapshot.Revision, datasetManifests)
	if err != nil {
		return err
	}
	if snapshotDigest != snapshot.Digest {
		return fmt.Errorf("%w: top-level dataset aggregate", ErrEdgeCompiledSnapshotDigestMismatch)
	}
	return nil
}

func loadOrderedEdgeCompiledSnapshotDatasetsTx(tx *gorm.DB, snapshot *EdgeCompiledSnapshot) ([]EdgeCompiledSnapshotDataset, error) {
	var stored []EdgeCompiledSnapshotDataset
	if err := tx.Where("snapshot_id = ?", snapshot.ID).Find(&stored).Error; err != nil {
		return nil, err
	}
	if len(stored) != len(edgeCompiledSnapshotDatasetOrder) {
		return nil, ErrEdgeCompiledSnapshotIncomplete
	}
	byDataset := make(map[dto.EdgeSnapshotDatasetV1]EdgeCompiledSnapshotDataset, len(stored))
	for i := range stored {
		if !stored[i].Dataset.Valid() {
			return nil, errors.New("invalid edge snapshot dataset")
		}
		if _, exists := byDataset[stored[i].Dataset]; exists {
			return nil, ErrEdgeCompiledSnapshotIncomplete
		}
		byDataset[stored[i].Dataset] = stored[i]
	}
	ordered := make([]EdgeCompiledSnapshotDataset, 0, len(edgeCompiledSnapshotDatasetOrder))
	for _, dataset := range edgeCompiledSnapshotDatasetOrder {
		storedDataset, exists := byDataset[dataset]
		if !exists {
			return nil, ErrEdgeCompiledSnapshotIncomplete
		}
		ordered = append(ordered, storedDataset)
	}
	return ordered, nil
}

func edgeCompiledSnapshotContentEqual(left *EdgeCompiledSnapshot, right *EdgeCompiledSnapshot) bool {
	return left.SnapshotUID == right.SnapshotUID &&
		left.Revision == right.Revision &&
		left.ProtocolVersion == right.ProtocolVersion &&
		left.HashAlgorithm == right.HashAlgorithm &&
		left.Digest == right.Digest &&
		left.TokenFingerprintAlgorithm == right.TokenFingerprintAlgorithm &&
		left.TokenFingerprintKeyID == right.TokenFingerprintKeyID &&
		left.TokenFingerprintVersion == right.TokenFingerprintVersion &&
		left.SigningAlgorithm == right.SigningAlgorithm &&
		left.SigningKeyID == right.SigningKeyID &&
		left.SigningPublicKey == right.SigningPublicKey &&
		left.SigningKeyNotBefore == right.SigningKeyNotBefore &&
		left.SigningKeyExpiresAt == right.SigningKeyExpiresAt &&
		left.CreatedAt == right.CreatedAt &&
		left.ExpiresAt == right.ExpiresAt
}

func validateEdgeCompiledSnapshotDigest(value string) error {
	_, err := edgesnapshot.AggregatePageDigests([]string{value})
	return err
}

func validateEdgeCompiledSnapshotUnixSeconds(field string, value int64) error {
	if value < edgeCompiledSnapshotMinUnixSeconds || value > edgeCompiledSnapshotMaxUnixSeconds {
		return fmt.Errorf("%s must be a Unix timestamp between years 2000 and 9999", field)
	}
	return nil
}
