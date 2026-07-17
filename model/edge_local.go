package model

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

const (
	edgeSQLiteBusyTimeoutMilliseconds = 30_000
	edgeLocalControlStateID           = 1
)

var (
	ErrEdgeLocalLeaseUnavailable     = errors.New("edge local lease is unavailable")
	ErrEdgeLocalLeaseExpired         = errors.New("edge local lease is expired")
	ErrEdgeLocalSnapshotMismatch     = errors.New("edge local lease snapshot does not match the applied snapshot")
	ErrEdgeLocalSnapshotStale        = errors.New("edge local snapshot revision is stale")
	ErrEdgeLocalSnapshotConflict     = errors.New("edge local snapshot revision conflicts with applied state")
	ErrEdgeLocalSnapshotExpired      = errors.New("edge local snapshot is expired")
	ErrEdgeLocalQuotaInsufficient    = errors.New("edge local lease quota is insufficient")
	ErrEdgeLocalReservationConflict  = errors.New("edge local reservation conflicts with existing state")
	ErrEdgeLocalReservationFinalized = errors.New("edge local reservation is already finalized")
	ErrEdgeLocalSettlementStaged     = errors.New("edge local reservation has a durable staged settlement")
	ErrEdgeLocalSettlementConflict   = errors.New("edge local settlement conflicts with durable state")
	ErrEdgeLocalNoPendingUsageEvents = errors.New("edge local outbox has no pending usage events")
	ErrEdgeLocalSettlementOutOfOrder = errors.New("edge local settlement sequence is not contiguous")
	ErrEdgeLocalAccountingCorruption = errors.New("edge local accounting invariant is violated")
)

var edgeLocalSnapshotDatasetOrder = [...]dto.EdgeSnapshotDatasetV1{
	dto.EdgeSnapshotDatasetAuthenticationV1,
	dto.EdgeSnapshotDatasetUsersV1,
	dto.EdgeSnapshotDatasetGroupsV1,
	dto.EdgeSnapshotDatasetModelsV1,
	dto.EdgeSnapshotDatasetChannelsV1,
	dto.EdgeSnapshotDatasetPricingV1,
	dto.EdgeSnapshotDatasetRoutingV1,
}

type EdgeLocalControlState struct {
	ID                         int    `gorm:"primaryKey;autoIncrement:false"`
	SnapshotID                 string `gorm:"type:varchar(64);not null"`
	SnapshotRevision           int64  `gorm:"not null"`
	SnapshotDigest             string `gorm:"type:char(64);not null"`
	SnapshotAppliedAtUnixMilli int64  `gorm:"not null"`
	SnapshotExpiresAtUnixMilli int64  `gorm:"not null"`
	TokenFingerprintAlgorithm  string `gorm:"type:varchar(32);not null"`
	TokenFingerprintKeyID      string `gorm:"type:varchar(64);not null"`
	TokenFingerprintVersion    int    `gorm:"not null"`
	NextEventSequence          int64  `gorm:"not null"`
	LastAckedSequence          int64  `gorm:"not null"`
	LastAckedBlockID           string `gorm:"type:varchar(64);not null"`
	LastAckedBlockDigest       string `gorm:"type:char(64);not null"`
	CreatedAtUnixMilli         int64  `gorm:"not null"`
	UpdatedAtUnixMilli         int64  `gorm:"not null"`
}

func (EdgeLocalControlState) TableName() string { return "edge_local_control_state" }

type EdgeLocalDatasetState struct {
	Dataset  dto.EdgeSnapshotDatasetV1 `gorm:"type:varchar(32);primaryKey;autoIncrement:false"`
	Revision int64                     `gorm:"not null"`
}

func (EdgeLocalDatasetState) TableName() string { return "edge_local_dataset_states" }

type EdgeLocalAuthProjection struct {
	TokenFingerprint   string `gorm:"type:varchar(128);primaryKey;autoIncrement:false"`
	TokenID            int64  `gorm:"not null;index"`
	UserID             int64  `gorm:"not null;index"`
	Enabled            bool   `gorm:"not null"`
	ExpiresAtUnixMilli *int64 `gorm:"index"`
	Payload            string `gorm:"type:text;not null"`
}

func (EdgeLocalAuthProjection) TableName() string { return "edge_local_auth_projections" }

type EdgeLocalUserProjection struct {
	UserID  int64  `gorm:"primaryKey;autoIncrement:false"`
	Enabled bool   `gorm:"not null"`
	Payload string `gorm:"type:text;not null"`
}

func (EdgeLocalUserProjection) TableName() string { return "edge_local_user_projections" }

type EdgeLocalGroupProjection struct {
	UserGroup string `gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	Payload   string `gorm:"type:text;not null"`
}

func (EdgeLocalGroupProjection) TableName() string { return "edge_local_group_projections" }

type EdgeLocalModelProjection struct {
	Model   string `gorm:"type:varchar(255);primaryKey;autoIncrement:false"`
	Enabled bool   `gorm:"not null;index"`
	Payload string `gorm:"type:text;not null"`
}

func (EdgeLocalModelProjection) TableName() string { return "edge_local_model_projections" }

type EdgeLocalChannelProjection struct {
	ChannelID    int64                  `gorm:"primaryKey;autoIncrement:false"`
	Enabled      bool                   `gorm:"not null;index"`
	LocalService dto.EdgeLocalServiceV1 `gorm:"type:varchar(64);not null;index"`
	Payload      string                 `gorm:"type:text;not null"`
}

func (EdgeLocalChannelProjection) TableName() string { return "edge_local_channel_projections" }

type EdgeLocalPricingProjection struct {
	PolicyID string `gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	Version  string `gorm:"type:varchar(64);not null"`
	Model    string `gorm:"type:varchar(255);not null;index:idx_edge_local_pricing_model_version,priority:1"`
	Payload  string `gorm:"type:text;not null"`
}

func (EdgeLocalPricingProjection) TableName() string { return "edge_local_pricing_projections" }

type EdgeLocalRoutingProjection struct {
	ID      int    `gorm:"primaryKey;autoIncrement:false"`
	Payload string `gorm:"type:text;not null"`
}

func (EdgeLocalRoutingProjection) TableName() string { return "edge_local_routing_projection" }

type EdgeLocalQuotaLease struct {
	LeaseID                  string                `gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	Version                  int64                 `gorm:"not null"`
	Status                   dto.EdgeLeaseStatusV1 `gorm:"type:varchar(32);not null;index"`
	NodeID                   string                `gorm:"type:varchar(64);not null;index:idx_edge_local_lease_node,priority:1"`
	NodeGeneration           int64                 `gorm:"not null;index:idx_edge_local_lease_node,priority:2"`
	UserID                   int64                 `gorm:"not null;index:idx_edge_local_lease_subject,priority:1"`
	TokenID                  int64                 `gorm:"not null;index:idx_edge_local_lease_subject,priority:2"`
	GrantedQuota             int64                 `gorm:"not null"`
	RemainingQuota           int64                 `gorm:"not null"`
	ReservedQuota            int64                 `gorm:"not null"`
	ConsumedQuota            int64                 `gorm:"not null"`
	RenewAfterRemainingQuota int64                 `gorm:"not null"`
	IssuedAtUnixMilli        int64                 `gorm:"not null"`
	ExpiresAtUnixMilli       int64                 `gorm:"not null;index"`
	SnapshotID               string                `gorm:"type:varchar(64);not null"`
	SnapshotRevision         int64                 `gorm:"not null"`
	PricingRevision          int64                 `gorm:"not null"`
	CreatedAtUnixMilli       int64                 `gorm:"not null"`
	UpdatedAtUnixMilli       int64                 `gorm:"not null"`
}

func (EdgeLocalQuotaLease) TableName() string { return "edge_local_quota_leases" }

// EdgeLocalLeaseAcquireIntent keeps the exact logical lease request durable
// until the returned lease is installed. The composite primary key permits at
// most one pending acquisition per user/token pair, so retries after a lost
// response reuse the original request and idempotency key.
type EdgeLocalLeaseAcquireIntent struct {
	UserID             int64  `gorm:"primaryKey;autoIncrement:false"`
	TokenID            int64  `gorm:"primaryKey;autoIncrement:false"`
	RequestID          string `gorm:"type:varchar(64);not null;uniqueIndex"`
	Payload            string `gorm:"type:text;not null"`
	CreatedAtUnixMilli int64  `gorm:"not null"`
	UpdatedAtUnixMilli int64  `gorm:"not null"`
}

func (EdgeLocalLeaseAcquireIntent) TableName() string {
	return "edge_local_lease_acquire_intents"
}

type EdgeLocalReservationStatus string

const (
	EdgeLocalReservationStatusActive   EdgeLocalReservationStatus = "active"
	EdgeLocalReservationStatusSettled  EdgeLocalReservationStatus = "settled"
	EdgeLocalReservationStatusRefunded EdgeLocalReservationStatus = "refunded"
)

type EdgeLocalQuotaReservation struct {
	ReservationID        string                     `gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	RequestID            string                     `gorm:"type:varchar(64);not null;uniqueIndex"`
	LeaseID              string                     `gorm:"type:varchar(64);not null;index"`
	UserID               int64                      `gorm:"not null;index"`
	TokenID              int64                      `gorm:"not null;index"`
	Status               EdgeLocalReservationStatus `gorm:"type:varchar(32);not null;index"`
	ReservedQuota        int64                      `gorm:"not null"`
	ChargedQuota         int64                      `gorm:"not null"`
	EventID              string                     `gorm:"type:varchar(64);not null;index"`
	EventSequence        int64                      `gorm:"not null;index"`
	StagedEventID        string                     `gorm:"type:varchar(64);not null;default:'';index"`
	StagedEventPayload   string                     `gorm:"type:text;not null;default:''"`
	StagedAtUnixMilli    int64                      `gorm:"not null;default:0;index"`
	CreatedAtUnixMilli   int64                      `gorm:"not null"`
	UpdatedAtUnixMilli   int64                      `gorm:"not null"`
	FinalizedAtUnixMilli int64                      `gorm:"not null"`
}

func (EdgeLocalQuotaReservation) TableName() string { return "edge_local_quota_reservations" }

type EdgeLocalUsageEvent struct {
	EventID                 string `gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	Sequence                int64  `gorm:"not null;uniqueIndex"`
	LeaseID                 string `gorm:"type:varchar(64);not null;index"`
	ReservationID           string `gorm:"type:varchar(64);not null;uniqueIndex"`
	RequestID               string `gorm:"type:varchar(64);not null;uniqueIndex"`
	Payload                 string `gorm:"type:text;not null"`
	BlockID                 string `gorm:"type:varchar(64);not null;index"`
	Acknowledged            bool   `gorm:"not null;index"`
	CreatedAtUnixMilli      int64  `gorm:"not null"`
	AcknowledgedAtUnixMilli int64  `gorm:"not null"`
}

func (EdgeLocalUsageEvent) TableName() string { return "edge_local_usage_events" }

type EdgeLocalOutboxStatus string

const (
	EdgeLocalOutboxStatusPending EdgeLocalOutboxStatus = "pending"
	EdgeLocalOutboxStatusInBlock EdgeLocalOutboxStatus = "in_block"
	EdgeLocalOutboxStatusAcked   EdgeLocalOutboxStatus = "acked"
)

type EdgeLocalOutbox struct {
	ID                      int64                 `gorm:"primaryKey"`
	EventID                 string                `gorm:"type:varchar(64);not null;uniqueIndex"`
	Sequence                int64                 `gorm:"not null;uniqueIndex"`
	BlockID                 string                `gorm:"type:varchar(64);not null;index"`
	Status                  EdgeLocalOutboxStatus `gorm:"type:varchar(32);not null;index"`
	Payload                 string                `gorm:"type:text;not null"`
	CreatedAtUnixMilli      int64                 `gorm:"not null"`
	UpdatedAtUnixMilli      int64                 `gorm:"not null"`
	AcknowledgedAtUnixMilli int64                 `gorm:"not null"`
}

func (EdgeLocalOutbox) TableName() string { return "edge_local_outbox" }

type EdgeLocalSettlementBlockStatus string

const (
	EdgeLocalSettlementBlockStatusPending EdgeLocalSettlementBlockStatus = "pending"
	EdgeLocalSettlementBlockStatusAcked   EdgeLocalSettlementBlockStatus = "acked"
)

type EdgeLocalSettlementBlock struct {
	BlockID                 string                         `gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	NodeID                  string                         `gorm:"type:varchar(64);not null"`
	NodeGeneration          int64                          `gorm:"not null"`
	PreviousBlockID         string                         `gorm:"type:varchar(64);not null"`
	PreviousBlockDigest     string                         `gorm:"type:char(64);not null"`
	FirstSequence           int64                          `gorm:"not null;uniqueIndex"`
	LastSequence            int64                          `gorm:"not null;uniqueIndex"`
	EventCount              int                            `gorm:"not null"`
	BlockDigest             string                         `gorm:"type:char(64);not null"`
	Status                  EdgeLocalSettlementBlockStatus `gorm:"type:varchar(32);not null;index"`
	Payload                 string                         `gorm:"type:text;not null"`
	AckPayload              string                         `gorm:"type:text;not null"`
	CreatedAtUnixMilli      int64                          `gorm:"not null"`
	AcknowledgedAtUnixMilli int64                          `gorm:"not null"`
}

func (EdgeLocalSettlementBlock) TableName() string { return "edge_local_settlement_blocks" }

type EdgeLocalSnapshotProjectionData struct {
	State              dto.EdgeSnapshotStateV1
	Digest             string
	ExpiresAtUnixMilli int64
	TokenFingerprint   dto.EdgeTokenFingerprintSchemeV1
	Authentication     []dto.EdgeTokenAuthRecordV1
	Users              []dto.EdgeUserPolicyV1
	Groups             []dto.EdgeGroupPolicyV1
	Models             []dto.EdgeModelPolicyV1
	Channels           []dto.EdgeChannelProjectionV1
	Pricing            []dto.EdgePricingPolicyV1
	Routing            []dto.EdgeRoutingPolicyV1
}

type EdgeLocalReservationRequest struct {
	ReservationID string
	RequestID     string
	LeaseID       string
	Quota         int64
	NowUnixMilli  int64
}

func InitEdgeDB() error {
	path := strings.TrimSpace(os.Getenv("EDGE_SQLITE_PATH"))
	if path == "" {
		return errors.New("EDGE_SQLITE_PATH is required for edge runtime")
	}
	db, err := OpenEdgeSQLite(path)
	if err != nil {
		return err
	}
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.MemoryCacheEnabled = false
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	initCol()
	DB = db
	LOG_DB = db
	return nil
}

// OpenEdgeSQLite opens and migrates an isolated edge-local SQLite database.
// It never reads SQL_DSN, LOG_SQL_DSN, common.SQLitePath, or master migration
// state. Tests and recovery tools can pass an explicit file DSN here.
func OpenEdgeSQLite(path string) (*gorm.DB, error) {
	dsn, err := edgeSQLiteDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		PrepareStmt:                              true,
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open edge SQLite: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(16)
	sqlDB.SetMaxIdleConns(4)

	var journalMode string
	if err := db.Raw("PRAGMA journal_mode").Scan(&journalMode).Error; err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("read edge SQLite journal mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("edge SQLite must use WAL mode, got %q", journalMode)
	}
	var busyTimeout int
	if err := db.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error; err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("read edge SQLite busy timeout: %w", err)
	}
	if busyTimeout < edgeSQLiteBusyTimeoutMilliseconds {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("edge SQLite busy timeout must be at least %dms", edgeSQLiteBusyTimeoutMilliseconds)
	}
	var foreignKeys int
	if err := db.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("read edge SQLite foreign key mode: %w", err)
	}
	if foreignKeys != 0 {
		_ = sqlDB.Close()
		return nil, errors.New("edge SQLite foreign keys must be disabled")
	}

	if err := migrateEdgeLocalDB(db); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func edgeSQLiteDSN(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("edge SQLite path is empty")
	}
	if strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("edge SQLite path contains control characters")
	}
	if path == ":memory:" || strings.Contains(strings.ToLower(path), "mode=memory") {
		return "", errors.New("edge SQLite requires a file-backed database for WAL durability")
	}
	separator := "?"
	if queryIndex := strings.IndexByte(path, '?'); queryIndex >= 0 {
		separator = "&"
		query, err := url.ParseQuery(path[queryIndex+1:])
		if err != nil {
			return "", fmt.Errorf("parse edge SQLite query: %w", err)
		}
		for _, forbidden := range []string{"_pragma", "_txlock", "_busy_timeout"} {
			if _, exists := query[forbidden]; exists {
				return "", fmt.Errorf("edge SQLite option %s is managed by the runtime", forbidden)
			}
		}
	}
	return path + separator +
		"_pragma=busy_timeout(30000)&_pragma=foreign_keys(0)&_pragma=journal_mode(WAL)&_txlock=immediate", nil
}

func migrateEdgeLocalDB(db *gorm.DB) error {
	if db == nil {
		return errors.New("edge SQLite database is nil")
	}
	if db.Dialector.Name() != "sqlite" {
		return errors.New("edge local tables are supported only on SQLite")
	}
	if err := db.AutoMigrate(
		&Channel{},
		&Ability{},
		&Log{},
		&EdgeLocalControlState{},
		&EdgeLocalDatasetState{},
		&EdgeLocalAuthProjection{},
		&EdgeLocalUserProjection{},
		&EdgeLocalGroupProjection{},
		&EdgeLocalModelProjection{},
		&EdgeLocalChannelProjection{},
		&EdgeLocalPricingProjection{},
		&EdgeLocalRoutingProjection{},
		&EdgeLocalQuotaLease{},
		&EdgeLocalLeaseAcquireIntent{},
		&EdgeLocalQuotaReservation{},
		&EdgeLocalUsageEvent{},
		&EdgeLocalOutbox{},
		&EdgeLocalSettlementBlock{},
	); err != nil {
		return fmt.Errorf("migrate edge SQLite: %w", err)
	}
	now := time.Now().UnixMilli()
	state := EdgeLocalControlState{
		ID:                 edgeLocalControlStateID,
		NextEventSequence:  1,
		CreatedAtUnixMilli: now,
		UpdatedAtUnixMilli: now,
	}
	if err := db.Where("id = ?", edgeLocalControlStateID).FirstOrCreate(&state).Error; err != nil {
		return fmt.Errorf("initialize edge local control state: %w", err)
	}
	return nil
}

func ApplyEdgeLocalSnapshot(db *gorm.DB, snapshot EdgeLocalSnapshotProjectionData) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge snapshot projection requires SQLite")
	}
	if err := validateEdgeLocalSnapshot(snapshot); err != nil {
		return err
	}

	authRows := make([]EdgeLocalAuthProjection, 0, len(snapshot.Authentication))
	for i := range snapshot.Authentication {
		payload, err := common.Marshal(snapshot.Authentication[i])
		if err != nil {
			return err
		}
		authRows = append(authRows, EdgeLocalAuthProjection{
			TokenFingerprint:   snapshot.Authentication[i].TokenFingerprint,
			TokenID:            snapshot.Authentication[i].TokenID,
			UserID:             snapshot.Authentication[i].UserID,
			Enabled:            snapshot.Authentication[i].Enabled,
			ExpiresAtUnixMilli: snapshot.Authentication[i].ExpiresAtUnixMilli,
			Payload:            string(payload),
		})
	}
	userRows := make([]EdgeLocalUserProjection, 0, len(snapshot.Users))
	for i := range snapshot.Users {
		payload, err := common.Marshal(snapshot.Users[i])
		if err != nil {
			return err
		}
		userRows = append(userRows, EdgeLocalUserProjection{UserID: snapshot.Users[i].UserID, Enabled: snapshot.Users[i].Enabled, Payload: string(payload)})
	}
	groupRows := make([]EdgeLocalGroupProjection, 0, len(snapshot.Groups))
	for i := range snapshot.Groups {
		payload, err := common.Marshal(snapshot.Groups[i])
		if err != nil {
			return err
		}
		groupRows = append(groupRows, EdgeLocalGroupProjection{UserGroup: snapshot.Groups[i].UserGroup, Payload: string(payload)})
	}
	modelRows := make([]EdgeLocalModelProjection, 0, len(snapshot.Models))
	for i := range snapshot.Models {
		payload, err := common.Marshal(snapshot.Models[i])
		if err != nil {
			return err
		}
		modelRows = append(modelRows, EdgeLocalModelProjection{Model: snapshot.Models[i].Model, Enabled: snapshot.Models[i].Enabled, Payload: string(payload)})
	}
	channelRows := make([]EdgeLocalChannelProjection, 0, len(snapshot.Channels))
	for i := range snapshot.Channels {
		projection := snapshot.Channels[i]
		payload, err := common.Marshal(projection)
		if err != nil {
			return err
		}
		channelRows = append(channelRows, EdgeLocalChannelProjection{
			ChannelID: projection.ChannelID, Enabled: projection.Enabled,
			LocalService: projection.LocalService, Payload: string(payload),
		})
	}
	legacyChannels, legacyAbilities, err := buildEdgeLocalLegacyRuntime(snapshot.Channels, snapshot.Models, snapshot.State.AppliedAtUnixMilli)
	if err != nil {
		return err
	}
	pricingRows := make([]EdgeLocalPricingProjection, 0, len(snapshot.Pricing))
	for i := range snapshot.Pricing {
		payload, err := common.Marshal(snapshot.Pricing[i])
		if err != nil {
			return err
		}
		pricingRows = append(pricingRows, EdgeLocalPricingProjection{
			PolicyID: snapshot.Pricing[i].PolicyID, Version: snapshot.Pricing[i].Version,
			Model: snapshot.Pricing[i].Model, Payload: string(payload),
		})
	}
	routingPayload, err := common.Marshal(snapshot.Routing[0])
	if err != nil {
		return err
	}
	datasetRows := make([]EdgeLocalDatasetState, 0, len(snapshot.State.Datasets))
	for i := range snapshot.State.Datasets {
		datasetRows = append(datasetRows, EdgeLocalDatasetState{
			Dataset: snapshot.State.Datasets[i].Dataset, Revision: snapshot.State.Datasets[i].Revision,
		})
	}

	return db.Transaction(func(tx *gorm.DB) error {
		var current EdgeLocalControlState
		if err := tx.First(&current, edgeLocalControlStateID).Error; err != nil {
			return err
		}
		if snapshot.State.Revision < current.SnapshotRevision {
			return ErrEdgeLocalSnapshotStale
		}
		if snapshot.State.Revision == current.SnapshotRevision && current.SnapshotRevision > 0 {
			if snapshot.State.SnapshotID != current.SnapshotID || snapshot.Digest != current.SnapshotDigest ||
				snapshot.ExpiresAtUnixMilli != current.SnapshotExpiresAtUnixMilli {
				return ErrEdgeLocalSnapshotConflict
			}
		}
		for _, table := range []any{
			&Ability{}, &Channel{},
			&EdgeLocalAuthProjection{}, &EdgeLocalUserProjection{}, &EdgeLocalGroupProjection{},
			&EdgeLocalModelProjection{}, &EdgeLocalChannelProjection{}, &EdgeLocalPricingProjection{},
			&EdgeLocalRoutingProjection{}, &EdgeLocalDatasetState{},
		} {
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(table).Error; err != nil {
				return err
			}
		}
		for _, batch := range []any{authRows, userRows, groupRows, modelRows, channelRows, pricingRows, legacyChannels, legacyAbilities, datasetRows} {
			if err := createEdgeLocalBatch(tx, batch); err != nil {
				return err
			}
		}
		if err := tx.Create(&EdgeLocalRoutingProjection{ID: edgeLocalControlStateID, Payload: string(routingPayload)}).Error; err != nil {
			return err
		}
		updates := map[string]any{
			"snapshot_id":                    snapshot.State.SnapshotID,
			"snapshot_revision":              snapshot.State.Revision,
			"snapshot_digest":                snapshot.Digest,
			"snapshot_applied_at_unix_milli": snapshot.State.AppliedAtUnixMilli,
			"snapshot_expires_at_unix_milli": snapshot.ExpiresAtUnixMilli,
			"token_fingerprint_algorithm":    snapshot.TokenFingerprint.Algorithm,
			"token_fingerprint_key_id":       snapshot.TokenFingerprint.KeyID,
			"token_fingerprint_version":      snapshot.TokenFingerprint.Version,
			"updated_at_unix_milli":          snapshot.State.AppliedAtUnixMilli,
		}
		result := tx.Model(&EdgeLocalControlState{}).Where("id = ?", edgeLocalControlStateID).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalAccountingCorruption
		}
		return nil
	})
}

// RefreshEdgeLocalChannelRuntime reapplies edge-owned CPA addresses and API
// keys to the durable Channel/Ability runtime projection without changing the
// signed business snapshot or any accounting state.
func RefreshEdgeLocalChannelRuntime(db *gorm.DB) error {
	return refreshEdgeLocalChannelRuntime(db)
}

func refreshEdgeLocalChannelRuntime(db *gorm.DB) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge local channel runtime refresh requires SQLite")
	}
	var control EdgeLocalControlState
	if err := db.First(&control, edgeLocalControlStateID).Error; err != nil {
		return err
	}
	var storedChannels []EdgeLocalChannelProjection
	if err := db.Order("channel_id asc").Find(&storedChannels).Error; err != nil {
		return err
	}
	var storedModels []EdgeLocalModelProjection
	if err := db.Order("model asc").Find(&storedModels).Error; err != nil {
		return err
	}
	channels := make([]dto.EdgeChannelProjectionV1, 0, len(storedChannels))
	for i := range storedChannels {
		var channel dto.EdgeChannelProjectionV1
		if err := common.Unmarshal([]byte(storedChannels[i].Payload), &channel); err != nil {
			return err
		}
		channels = append(channels, channel)
	}
	models := make([]dto.EdgeModelPolicyV1, 0, len(storedModels))
	for i := range storedModels {
		var modelPolicy dto.EdgeModelPolicyV1
		if err := common.Unmarshal([]byte(storedModels[i].Payload), &modelPolicy); err != nil {
			return err
		}
		models = append(models, modelPolicy)
	}
	legacyChannels, legacyAbilities, err := buildEdgeLocalLegacyRuntime(channels, models, control.SnapshotAppliedAtUnixMilli)
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		for _, table := range []any{&Ability{}, &Channel{}} {
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(table).Error; err != nil {
				return err
			}
		}
		if err := createEdgeLocalBatch(tx, legacyChannels); err != nil {
			return err
		}
		return createEdgeLocalBatch(tx, legacyAbilities)
	})
}

func buildEdgeLocalLegacyRuntime(channels []dto.EdgeChannelProjectionV1, models []dto.EdgeModelPolicyV1, appliedAtUnixMilli int64) ([]Channel, []Ability, error) {
	localConfigs, err := loadEdgeLocalChannelConfigs()
	if err != nil {
		return nil, nil, err
	}
	modelPolicies := make(map[string]dto.EdgeModelPolicyV1, len(models))
	for i := range models {
		modelPolicies[models[i].Model] = models[i]
	}
	legacyChannels := make([]Channel, 0, len(channels))
	legacyAbilities := make([]Ability, 0)
	for i := range channels {
		localConfig, configured := localConfigs[channels[i].Name]
		legacyChannel, abilities, err := edgeLocalLegacyChannel(channels[i], modelPolicies, appliedAtUnixMilli, localConfig, configured)
		if err != nil {
			return nil, nil, err
		}
		legacyChannels = append(legacyChannels, legacyChannel)
		legacyAbilities = append(legacyAbilities, abilities...)
	}
	return legacyChannels, legacyAbilities, nil
}

func createEdgeLocalBatch(tx *gorm.DB, batch any) error {
	value := reflect.ValueOf(batch)
	if value.Kind() != reflect.Slice {
		return errors.New("edge local batch must be a slice")
	}
	if value.Len() == 0 {
		return nil
	}
	return tx.CreateInBatches(batch, 200).Error
}

func validateEdgeLocalSnapshot(snapshot EdgeLocalSnapshotProjectionData) error {
	if err := snapshot.State.Validate(); err != nil {
		return err
	}
	if snapshot.State.SnapshotID == "" || snapshot.State.Revision <= 0 || snapshot.State.AppliedAtUnixMilli <= 0 {
		return errors.New("edge local snapshot state must be fully applied")
	}
	if snapshot.ExpiresAtUnixMilli <= snapshot.State.AppliedAtUnixMilli {
		return errors.New("edge local snapshot expiry must be after its applied time")
	}
	if err := validateEdgeLocalSHA256(snapshot.Digest); err != nil {
		return fmt.Errorf("edge local snapshot digest: %w", err)
	}
	if err := snapshot.TokenFingerprint.Validate(); err != nil {
		return err
	}
	if len(snapshot.State.Datasets) != len(edgeLocalSnapshotDatasetOrder) {
		return errors.New("edge local snapshot must contain all seven datasets")
	}
	for i, expected := range edgeLocalSnapshotDatasetOrder {
		if snapshot.State.Datasets[i].Dataset != expected || snapshot.State.Datasets[i].Revision <= 0 {
			return fmt.Errorf("edge local snapshot dataset %d must be %s with a positive revision", i, expected)
		}
	}
	if len(snapshot.Routing) != 1 {
		return errors.New("edge local snapshot must contain exactly one routing policy")
	}

	seenAuth := make(map[string]struct{}, len(snapshot.Authentication))
	for i := range snapshot.Authentication {
		payload := dto.EdgeSnapshotPagePayloadV1{Authentication: []dto.EdgeTokenAuthRecordV1{snapshot.Authentication[i]}}
		if err := payload.Validate(dto.EdgeSnapshotDatasetAuthenticationV1, 1); err != nil {
			return err
		}
		if _, exists := seenAuth[snapshot.Authentication[i].TokenFingerprint]; exists {
			return errors.New("edge local authentication projection contains duplicate fingerprints")
		}
		seenAuth[snapshot.Authentication[i].TokenFingerprint] = struct{}{}
	}
	seenUsers := make(map[int64]struct{}, len(snapshot.Users))
	for i := range snapshot.Users {
		payload := dto.EdgeSnapshotPagePayloadV1{Users: []dto.EdgeUserPolicyV1{snapshot.Users[i]}}
		if err := payload.Validate(dto.EdgeSnapshotDatasetUsersV1, 1); err != nil {
			return err
		}
		if _, exists := seenUsers[snapshot.Users[i].UserID]; exists {
			return errors.New("edge local user projection contains duplicate users")
		}
		seenUsers[snapshot.Users[i].UserID] = struct{}{}
	}
	seenGroups := make(map[string]struct{}, len(snapshot.Groups))
	for i := range snapshot.Groups {
		payload := dto.EdgeSnapshotPagePayloadV1{Groups: []dto.EdgeGroupPolicyV1{snapshot.Groups[i]}}
		if err := payload.Validate(dto.EdgeSnapshotDatasetGroupsV1, 1); err != nil {
			return err
		}
		if _, exists := seenGroups[snapshot.Groups[i].UserGroup]; exists {
			return errors.New("edge local group projection contains duplicate groups")
		}
		seenGroups[snapshot.Groups[i].UserGroup] = struct{}{}
	}
	seenModels := make(map[string]struct{}, len(snapshot.Models))
	for i := range snapshot.Models {
		if err := snapshot.Models[i].Validate(); err != nil {
			return err
		}
		if _, exists := seenModels[snapshot.Models[i].Model]; exists {
			return errors.New("edge local model projection contains duplicate models")
		}
		seenModels[snapshot.Models[i].Model] = struct{}{}
	}
	seenChannels := make(map[int64]struct{}, len(snapshot.Channels))
	for i := range snapshot.Channels {
		if err := snapshot.Channels[i].Validate(); err != nil {
			return err
		}
		if snapshot.Channels[i].ChannelID > int64(math.MaxInt) {
			return errors.New("edge local channel ID exceeds the local integer range")
		}
		if _, exists := seenChannels[snapshot.Channels[i].ChannelID]; exists {
			return errors.New("edge local channel projection contains duplicate channels")
		}
		seenChannels[snapshot.Channels[i].ChannelID] = struct{}{}
	}
	seenPricing := make(map[string]struct{}, len(snapshot.Pricing))
	for i := range snapshot.Pricing {
		if err := snapshot.Pricing[i].Validate(); err != nil {
			return err
		}
		if _, exists := seenPricing[snapshot.Pricing[i].PolicyID]; exists {
			return errors.New("edge local pricing projection contains duplicate policies")
		}
		seenPricing[snapshot.Pricing[i].PolicyID] = struct{}{}
	}
	return snapshot.Routing[0].Validate()
}

func edgeLocalLegacyChannel(
	projection dto.EdgeChannelProjectionV1,
	models map[string]dto.EdgeModelPolicyV1,
	appliedAtUnixMilli int64,
	localConfig edgeLocalChannelConfig,
	configured bool,
) (Channel, []Ability, error) {
	if configured && localConfig.Type != constant.ChannelTypeUnknown && localConfig.Type != projection.Type {
		return Channel{}, nil, fmt.Errorf("edge channel %q type does not match master projection", projection.Name)
	}
	baseURL := ""
	apiKey := ""
	channelModels := append([]string(nil), projection.Models...)
	channelGroups := append([]string(nil), projection.Groups...)
	if configured {
		baseURL = localConfig.BaseURL
		apiKey = localConfig.Auth
		channelModels = filterEdgeLocalChannelValues(channelModels, localConfig.Models)
		channelGroups = filterEdgeLocalChannelValues(channelGroups, localConfig.Groups)
	}
	channelEnabled := projection.Enabled && configured && localConfig.Enabled && apiKey != ""
	weight := uint(projection.Weight)
	priority := projection.Priority
	if configured && localConfig.Weight != nil {
		weight = *localConfig.Weight
	}
	if configured && localConfig.Priority != nil {
		priority = *localConfig.Priority
	}
	autoBan := 0
	modelMapping := make(map[string]string, len(projection.ModelMapping)+len(localConfig.ModelMapping))
	for source, target := range projection.ModelMapping {
		modelMapping[source] = target
	}
	if configured {
		for source, target := range localConfig.ModelMapping {
			modelMapping[source] = target
		}
	}
	allowedModels := stringSet(channelModels)
	for source := range modelMapping {
		if _, exists := allowedModels[source]; !exists {
			delete(modelMapping, source)
		}
	}
	modelMappingPayload, err := common.Marshal(modelMapping)
	if err != nil {
		return Channel{}, nil, err
	}
	statusMappingPayload, err := common.Marshal(projection.StatusCodeMapping)
	if err != nil {
		return Channel{}, nil, err
	}
	projectionPayload, err := common.Marshal(projection)
	if err != nil {
		return Channel{}, nil, err
	}
	masterChannelSetting := dto.ChannelSettings{
		ForceFormat: projection.TextPolicy.ForceFormat, ThinkingToContent: projection.TextPolicy.ThinkingToContent,
		PassThroughBodyEnabled: projection.TextPolicy.PassThroughBodyEnabled,
		SystemPrompt:           projection.TextPolicy.SystemPrompt, SystemPromptOverride: projection.TextPolicy.SystemPromptOverride,
	}
	channelSetting, err := edgeLocalChannelStructMap(masterChannelSetting)
	if err != nil {
		return Channel{}, nil, err
	}
	if configured {
		mergeEdgeLocalChannelMap(channelSetting, localConfig.ChannelSetting)
	}
	channelSettingPayload, err := common.Marshal(channelSetting)
	if err != nil {
		return Channel{}, nil, err
	}
	masterOtherSettings := dto.ChannelOtherSettings{
		AllowServiceTier: projection.TextPolicy.AllowServiceTier, AllowInferenceGeo: projection.TextPolicy.AllowInferenceGeo,
		AllowSpeed: projection.TextPolicy.AllowSpeed, DisableStore: projection.TextPolicy.DisableStore,
		AllowSafetyIdentifier:   projection.TextPolicy.AllowSafetyIdentifier,
		AllowIncludeObfuscation: projection.TextPolicy.AllowIncludeObfuscation,
	}
	channelOtherSettings, err := edgeLocalChannelStructMap(masterOtherSettings)
	if err != nil {
		return Channel{}, nil, err
	}
	if configured {
		mergeEdgeLocalChannelMap(channelOtherSettings, localConfig.Settings)
	}
	channelOtherSettingsPayload, err := common.Marshal(channelOtherSettings)
	if err != nil {
		return Channel{}, nil, err
	}
	modelMappingJSON := string(modelMappingPayload)
	statusMapping := string(statusMappingPayload)
	channelSettingJSON := string(channelSettingPayload)
	channel := Channel{
		Id: int(projection.ChannelID), Type: projection.Type, Key: apiKey,
		Status: common.ChannelStatusManuallyDisabled, Name: projection.Name, Weight: &weight,
		CreatedTime: appliedAtUnixMilli / 1000, BaseURL: &baseURL, Models: strings.Join(channelModels, ","),
		Group: strings.Join(channelGroups, ","), ModelMapping: &modelMappingJSON,
		StatusCodeMapping: &statusMapping, Priority: &priority, AutoBan: &autoBan,
		Setting: &channelSettingJSON, OtherInfo: string(projectionPayload), OtherSettings: string(channelOtherSettingsPayload),
	}
	if configured {
		channel.OpenAIOrganization = localConfig.OpenAIOrganization
		channel.Other = localConfig.Other
		channel.ParamOverride, err = marshalEdgeLocalChannelMapPointer(localConfig.ParamOverride)
		if err != nil {
			return Channel{}, nil, err
		}
		channel.HeaderOverride, err = marshalEdgeLocalChannelMapPointer(localConfig.HeaderOverride)
		if err != nil {
			return Channel{}, nil, err
		}
		channel.ChannelInfo.MultiKeyMode = localConfig.MultiKeyMode
		keys := channel.GetKeys()
		channel.ChannelInfo.IsMultiKey = len(keys) > 1 || localConfig.MultiKeyMode != ""
		if channel.ChannelInfo.IsMultiKey {
			channel.ChannelInfo.MultiKeySize = len(keys)
		}
	}
	if channelEnabled {
		channel.Status = common.ChannelStatusEnabled
	}
	abilities := make([]Ability, 0, len(channelGroups)*len(channelModels))
	for _, group := range channelGroups {
		for _, modelName := range channelModels {
			policy, exists := models[modelName]
			enabled := channelEnabled && exists && policy.Enabled && edgeLocalModelAllowsChannel(policy, projection.ChannelID)
			abilities = append(abilities, Ability{
				Group: group, Model: modelName, ChannelId: int(projection.ChannelID), Enabled: enabled,
				Priority: &priority, Weight: weight,
			})
		}
	}
	return channel, abilities, nil
}

func filterEdgeLocalChannelValues(values []string, allowed map[string]struct{}) []string {
	if allowed == nil {
		return values
	}
	filtered := values[:0]
	for _, value := range values {
		if _, exists := allowed[value]; exists {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func edgeLocalChannelStructMap(value any) (map[string]any, error) {
	payload, err := common.Marshal(value)
	if err != nil {
		return nil, err
	}
	result := make(map[string]any)
	if err := common.Unmarshal(payload, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func mergeEdgeLocalChannelMap(target map[string]any, override map[string]any) {
	for key, value := range override {
		target[key] = value
	}
}

func marshalEdgeLocalChannelMapPointer(value map[string]any) (*string, error) {
	if len(value) == 0 {
		return nil, nil
	}
	payload, err := common.Marshal(value)
	if err != nil {
		return nil, err
	}
	encoded := string(payload)
	return &encoded, nil
}

func edgeLocalModelAllowsChannel(policy dto.EdgeModelPolicyV1, channelID int64) bool {
	for _, allowed := range policy.ChannelIDs {
		if allowed == channelID {
			return true
		}
	}
	return false
}

func GetEdgeLocalSnapshotState(db *gorm.DB) (*dto.EdgeSnapshotStateV1, error) {
	var control EdgeLocalControlState
	if err := db.First(&control, edgeLocalControlStateID).Error; err != nil {
		return nil, err
	}
	var stored []EdgeLocalDatasetState
	if err := db.Find(&stored).Error; err != nil {
		return nil, err
	}
	byDataset := make(map[dto.EdgeSnapshotDatasetV1]int64, len(stored))
	for i := range stored {
		byDataset[stored[i].Dataset] = stored[i].Revision
	}
	state := &dto.EdgeSnapshotStateV1{
		SnapshotID: control.SnapshotID, Revision: control.SnapshotRevision,
		AppliedAtUnixMilli: control.SnapshotAppliedAtUnixMilli,
		Datasets:           make([]dto.EdgeSnapshotDatasetStateV1, 0, len(edgeLocalSnapshotDatasetOrder)),
	}
	for _, dataset := range edgeLocalSnapshotDatasetOrder {
		if revision, exists := byDataset[dataset]; exists {
			state.Datasets = append(state.Datasets, dto.EdgeSnapshotDatasetStateV1{Dataset: dataset, Revision: revision})
		}
	}
	return state, nil
}

func GetEdgeLocalSnapshotExpiry(db *gorm.DB) (int64, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return 0, errors.New("edge local snapshot expiry query requires SQLite")
	}
	var expiresAt int64
	if err := db.Model(&EdgeLocalControlState{}).Where("id = ?", edgeLocalControlStateID).
		Select("snapshot_expires_at_unix_milli").Scan(&expiresAt).Error; err != nil {
		return 0, err
	}
	return expiresAt, nil
}

func GetEdgeLocalTokenAuth(db *gorm.DB, fingerprint string) (*dto.EdgeTokenAuthRecordV1, error) {
	var stored EdgeLocalAuthProjection
	if err := db.Where("token_fingerprint = ?", fingerprint).First(&stored).Error; err != nil {
		return nil, err
	}
	var result dto.EdgeTokenAuthRecordV1
	if err := common.Unmarshal([]byte(stored.Payload), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func GetEdgeLocalUser(db *gorm.DB, userID int64) (*dto.EdgeUserPolicyV1, error) {
	var stored EdgeLocalUserProjection
	if err := db.First(&stored, userID).Error; err != nil {
		return nil, err
	}
	var result dto.EdgeUserPolicyV1
	if err := common.Unmarshal([]byte(stored.Payload), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func GetEdgeLocalGroup(db *gorm.DB, userGroup string) (*dto.EdgeGroupPolicyV1, error) {
	var stored EdgeLocalGroupProjection
	if err := db.Where("user_group = ?", userGroup).First(&stored).Error; err != nil {
		return nil, err
	}
	var result dto.EdgeGroupPolicyV1
	if err := common.Unmarshal([]byte(stored.Payload), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func GetEdgeLocalModel(db *gorm.DB, modelName string) (*dto.EdgeModelPolicyV1, error) {
	var stored EdgeLocalModelProjection
	if err := db.Where("model = ?", modelName).First(&stored).Error; err != nil {
		return nil, err
	}
	var result dto.EdgeModelPolicyV1
	if err := common.Unmarshal([]byte(stored.Payload), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func GetEdgeLocalChannelProjection(db *gorm.DB, channelID int64) (*dto.EdgeChannelProjectionV1, error) {
	var stored EdgeLocalChannelProjection
	if err := db.First(&stored, channelID).Error; err != nil {
		return nil, err
	}
	var result dto.EdgeChannelProjectionV1
	if err := common.Unmarshal([]byte(stored.Payload), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func GetEdgeLocalPricing(db *gorm.DB, modelName string) ([]dto.EdgePricingPolicyV1, error) {
	var stored []EdgeLocalPricingProjection
	if err := db.Where("model = ?", modelName).Order("policy_id asc").Find(&stored).Error; err != nil {
		return nil, err
	}
	result := make([]dto.EdgePricingPolicyV1, 0, len(stored))
	for i := range stored {
		var policy dto.EdgePricingPolicyV1
		if err := common.Unmarshal([]byte(stored[i].Payload), &policy); err != nil {
			return nil, err
		}
		result = append(result, policy)
	}
	return result, nil
}

func GetEdgeLocalRouting(db *gorm.DB) (*dto.EdgeRoutingPolicyV1, error) {
	var stored EdgeLocalRoutingProjection
	if err := db.First(&stored, edgeLocalControlStateID).Error; err != nil {
		return nil, err
	}
	var result dto.EdgeRoutingPolicyV1
	if err := common.Unmarshal([]byte(stored.Payload), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func InstallEdgeLocalLease(db *gorm.DB, lease dto.EdgeQuotaLeaseV1) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge lease installation requires SQLite")
	}
	if err := lease.Validate(); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	return db.Transaction(func(tx *gorm.DB) error {
		return installEdgeLocalLeaseTx(tx, lease, now)
	})
}

func installEdgeLocalLeaseTx(tx *gorm.DB, lease dto.EdgeQuotaLeaseV1, nowUnixMilli int64) error {
	if tx == nil {
		return errors.New("edge lease installation transaction is nil")
	}
	if nowUnixMilli <= 0 {
		return errors.New("edge lease installation time must be positive")
	}
	var existing EdgeLocalQuotaLease
	query := tx.Where("lease_id = ?", lease.LeaseID).Limit(1).Find(&existing)
	if query.Error != nil {
		return query.Error
	}
	if query.RowsAffected == 0 {
		return tx.Create(&EdgeLocalQuotaLease{
			LeaseID: lease.LeaseID, Version: lease.Version, Status: lease.Status,
			NodeID: lease.NodeID, NodeGeneration: lease.NodeGeneration,
			UserID: lease.Subject.UserID, TokenID: lease.Subject.TokenID,
			GrantedQuota: lease.GrantedQuota, RemainingQuota: lease.GrantedQuota,
			RenewAfterRemainingQuota: lease.RenewAfterRemainingQuota,
			IssuedAtUnixMilli:        lease.IssuedAtUnixMilli, ExpiresAtUnixMilli: lease.ExpiresAtUnixMilli,
			SnapshotID: lease.SnapshotID, SnapshotRevision: lease.SnapshotRevision,
			PricingRevision:    lease.PricingRevision,
			CreatedAtUnixMilli: nowUnixMilli, UpdatedAtUnixMilli: nowUnixMilli,
		}).Error
	}
	if existing.NodeID != lease.NodeID || existing.NodeGeneration != lease.NodeGeneration ||
		existing.UserID != lease.Subject.UserID || existing.TokenID != lease.Subject.TokenID ||
		existing.SnapshotID != lease.SnapshotID || existing.SnapshotRevision != lease.SnapshotRevision ||
		existing.PricingRevision != lease.PricingRevision {
		return ErrEdgeLocalSettlementConflict
	}
	if lease.Version < existing.Version {
		return ErrEdgeLocalSettlementConflict
	}
	if lease.Version == existing.Version {
		if edgeLocalLeaseMatches(existing, lease) && validateEdgeLocalLeaseAccounting(existing) == nil {
			return nil
		}
		return ErrEdgeLocalSettlementConflict
	}
	accounted := existing.ReservedQuota + existing.ConsumedQuota
	if accounted < 0 || accounted > lease.GrantedQuota {
		return ErrEdgeLocalQuotaInsufficient
	}
	if existing.Status != dto.EdgeLeaseStatusActiveV1 && lease.Status == dto.EdgeLeaseStatusActiveV1 {
		return ErrEdgeLocalSettlementConflict
	}
	updates := map[string]any{
		"version": lease.Version, "status": lease.Status,
		"granted_quota": lease.GrantedQuota, "remaining_quota": lease.GrantedQuota - accounted,
		"renew_after_remaining_quota": lease.RenewAfterRemainingQuota,
		"issued_at_unix_milli":        lease.IssuedAtUnixMilli, "expires_at_unix_milli": lease.ExpiresAtUnixMilli,
		"updated_at_unix_milli": nowUnixMilli,
	}
	return tx.Model(&EdgeLocalQuotaLease{}).Where("lease_id = ?", lease.LeaseID).Updates(updates).Error
}

// GetOrCreateEdgeLocalLeaseAcquireIntent persists the exact acquire request on
// first use. If the same subject already has a pending intent, the durable
// request is returned unchanged so a retry cannot accidentally reserve twice
// under a new request ID.
func GetOrCreateEdgeLocalLeaseAcquireIntent(
	db *gorm.DB,
	request dto.EdgeLeaseAcquireRequestV1,
	nowUnixMilli int64,
) (*dto.EdgeLeaseAcquireRequestV1, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge lease acquire intent requires SQLite")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if err := edgeauth.ValidateIdempotencyKey(request.Meta.RequestID); err != nil {
		return nil, err
	}
	if nowUnixMilli <= 0 {
		return nil, errors.New("edge lease acquire intent time must be positive")
	}

	var durable *dto.EdgeLeaseAcquireRequestV1
	err := db.Transaction(func(tx *gorm.DB) error {
		var existing EdgeLocalLeaseAcquireIntent
		query := tx.Where("user_id = ? AND token_id = ?", request.Subject.UserID, request.Subject.TokenID).
			Limit(1).Find(&existing)
		if query.Error != nil {
			return query.Error
		}
		if query.RowsAffected == 1 {
			stored, err := decodeEdgeLocalLeaseAcquireIntent(existing)
			if err != nil {
				return err
			}
			durable = stored
			return nil
		}

		payload, err := common.Marshal(request)
		if err != nil {
			return err
		}
		intent := EdgeLocalLeaseAcquireIntent{
			UserID: request.Subject.UserID, TokenID: request.Subject.TokenID,
			RequestID: request.Meta.RequestID, Payload: string(payload),
			CreatedAtUnixMilli: nowUnixMilli, UpdatedAtUnixMilli: nowUnixMilli,
		}
		if err := tx.Create(&intent).Error; err != nil {
			return err
		}
		copy := request
		durable = &copy
		return nil
	})
	if err != nil {
		return nil, err
	}
	return durable, nil
}

// GetEdgeLocalLeaseAcquireIntent restores one pending request after restart.
func GetEdgeLocalLeaseAcquireIntent(db *gorm.DB, userID, tokenID int64) (*dto.EdgeLeaseAcquireRequestV1, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge lease acquire intent query requires SQLite")
	}
	if userID <= 0 || tokenID <= 0 {
		return nil, errors.New("edge lease acquire intent subject must be positive")
	}
	var stored EdgeLocalLeaseAcquireIntent
	if err := db.Where("user_id = ? AND token_id = ?", userID, tokenID).First(&stored).Error; err != nil {
		return nil, err
	}
	return decodeEdgeLocalLeaseAcquireIntent(stored)
}

func decodeEdgeLocalLeaseAcquireIntent(stored EdgeLocalLeaseAcquireIntent) (*dto.EdgeLeaseAcquireRequestV1, error) {
	var request dto.EdgeLeaseAcquireRequestV1
	if err := common.Unmarshal([]byte(stored.Payload), &request); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, ErrEdgeLocalAccountingCorruption
	}
	if err := edgeauth.ValidateIdempotencyKey(request.Meta.RequestID); err != nil {
		return nil, ErrEdgeLocalAccountingCorruption
	}
	if request.Subject.UserID != stored.UserID || request.Subject.TokenID != stored.TokenID ||
		request.Meta.RequestID != stored.RequestID {
		return nil, ErrEdgeLocalAccountingCorruption
	}
	return &request, nil
}

// InstallEdgeLocalLeaseFromAcquireIntent atomically installs the successful
// response and clears only its matching durable intent.
func InstallEdgeLocalLeaseFromAcquireIntent(db *gorm.DB, requestID string, lease dto.EdgeQuotaLeaseV1) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge lease acquire completion requires SQLite")
	}
	if err := edgeauth.ValidateIdempotencyKey(requestID); err != nil {
		return fmt.Errorf("lease acquire request ID: %w", err)
	}
	if err := lease.Validate(); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	return db.Transaction(func(tx *gorm.DB) error {
		var intent EdgeLocalLeaseAcquireIntent
		if err := tx.Where("request_id = ?", requestID).First(&intent).Error; err != nil {
			return err
		}
		request, err := decodeEdgeLocalLeaseAcquireIntent(intent)
		if err != nil {
			return err
		}
		if request.Subject != lease.Subject || request.SnapshotID != lease.SnapshotID ||
			request.SnapshotRevision != lease.SnapshotRevision {
			return ErrEdgeLocalSettlementConflict
		}
		if lease.Status != dto.EdgeLeaseStatusActiveV1 || lease.GrantedQuota < request.MinimumAcceptableQuota ||
			lease.GrantedQuota > request.RequestedQuota {
			return ErrEdgeLocalSettlementConflict
		}
		if err := installEdgeLocalLeaseTx(tx, lease, now); err != nil {
			return err
		}
		result := tx.Where("user_id = ? AND token_id = ? AND request_id = ?",
			intent.UserID, intent.TokenID, intent.RequestID).Delete(&EdgeLocalLeaseAcquireIntent{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalSettlementConflict
		}
		return nil
	})
}

// DiscardEdgeLocalLeaseAcquireIntent clears an exact request only after the
// caller has received an authoritative rejection. Ambiguous transport errors
// must retain the intent and retry it instead.
func DiscardEdgeLocalLeaseAcquireIntent(db *gorm.DB, requestID string) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge lease acquire intent discard requires SQLite")
	}
	if err := edgeauth.ValidateIdempotencyKey(requestID); err != nil {
		return err
	}
	return db.Where("request_id = ?", requestID).Delete(&EdgeLocalLeaseAcquireIntent{}).Error
}

func edgeLocalLeaseMatches(stored EdgeLocalQuotaLease, lease dto.EdgeQuotaLeaseV1) bool {
	return stored.Version == lease.Version && stored.Status == lease.Status &&
		stored.NodeID == lease.NodeID && stored.NodeGeneration == lease.NodeGeneration &&
		stored.UserID == lease.Subject.UserID && stored.TokenID == lease.Subject.TokenID &&
		stored.GrantedQuota == lease.GrantedQuota && stored.RenewAfterRemainingQuota == lease.RenewAfterRemainingQuota &&
		stored.IssuedAtUnixMilli == lease.IssuedAtUnixMilli && stored.ExpiresAtUnixMilli == lease.ExpiresAtUnixMilli &&
		stored.SnapshotID == lease.SnapshotID && stored.SnapshotRevision == lease.SnapshotRevision &&
		stored.PricingRevision == lease.PricingRevision
}

func GetEdgeLocalLease(db *gorm.DB, leaseID string) (*EdgeLocalQuotaLease, error) {
	var lease EdgeLocalQuotaLease
	if err := db.Where("lease_id = ?", leaseID).First(&lease).Error; err != nil {
		return nil, err
	}
	if err := validateEdgeLocalLeaseAccounting(lease); err != nil {
		return nil, err
	}
	return &lease, nil
}

func GetEdgeLocalReservation(db *gorm.DB, reservationID string) (*EdgeLocalQuotaReservation, error) {
	if err := validateEdgeLocalIdentifier(reservationID); err != nil {
		return nil, err
	}
	var reservation EdgeLocalQuotaReservation
	if err := db.Where("reservation_id = ?", reservationID).First(&reservation).Error; err != nil {
		return nil, err
	}
	return &reservation, nil
}

func validateEdgeLocalLeaseAccounting(lease EdgeLocalQuotaLease) error {
	for _, quota := range []int64{lease.GrantedQuota, lease.RemainingQuota, lease.ReservedQuota, lease.ConsumedQuota} {
		if quota < 0 || quota > int64(common.MaxQuota) {
			return ErrEdgeLocalAccountingCorruption
		}
	}
	if lease.RemainingQuota+lease.ReservedQuota+lease.ConsumedQuota != lease.GrantedQuota {
		return ErrEdgeLocalAccountingCorruption
	}
	return nil
}

func FindEdgeLocalActiveLease(db *gorm.DB, userID, tokenID, nowUnixMilli int64) (*EdgeLocalQuotaLease, error) {
	var lease EdgeLocalQuotaLease
	err := db.Where("user_id = ? AND token_id = ? AND status = ? AND expires_at_unix_milli > ?",
		userID, tokenID, dto.EdgeLeaseStatusActiveV1, nowUnixMilli).
		Order("issued_at_unix_milli desc, lease_id asc").First(&lease).Error
	if err != nil {
		return nil, err
	}
	return &lease, nil
}

func ReserveEdgeLocalQuota(db *gorm.DB, request EdgeLocalReservationRequest) (*EdgeLocalQuotaReservation, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local reservation requires SQLite")
	}
	if err := validateEdgeLocalIdentifier(request.ReservationID); err != nil {
		return nil, fmt.Errorf("reservation ID: %w", err)
	}
	if err := validateEdgeLocalIdentifier(request.RequestID); err != nil {
		return nil, fmt.Errorf("request ID: %w", err)
	}
	if err := validateEdgeLocalIdentifier(request.LeaseID); err != nil {
		return nil, fmt.Errorf("lease ID: %w", err)
	}
	if err := validateEdgeLocalQuota(request.Quota, true); err != nil {
		return nil, err
	}
	if request.NowUnixMilli <= 0 {
		return nil, errors.New("reservation time must be positive")
	}

	var reservation *EdgeLocalQuotaReservation
	err := db.Transaction(func(tx *gorm.DB) error {
		var existing EdgeLocalQuotaReservation
		query := tx.Where("reservation_id = ? OR request_id = ?", request.ReservationID, request.RequestID).Limit(1).Find(&existing)
		if query.Error != nil {
			return query.Error
		}
		if query.RowsAffected == 1 {
			if existing.ReservationID == request.ReservationID && existing.RequestID == request.RequestID &&
				existing.LeaseID == request.LeaseID && existing.ReservedQuota == request.Quota {
				if existing.Status != EdgeLocalReservationStatusActive {
					return ErrEdgeLocalReservationFinalized
				}
				reservation = &existing
				return nil
			}
			return ErrEdgeLocalReservationConflict
		}
		var control EdgeLocalControlState
		if err := tx.First(&control, edgeLocalControlStateID).Error; err != nil {
			return err
		}
		if control.SnapshotExpiresAtUnixMilli <= request.NowUnixMilli {
			return ErrEdgeLocalSnapshotExpired
		}
		var pricingDataset EdgeLocalDatasetState
		if err := tx.Where("dataset = ?", dto.EdgeSnapshotDatasetPricingV1).First(&pricingDataset).Error; err != nil {
			return ErrEdgeLocalSnapshotMismatch
		}
		result := tx.Model(&EdgeLocalQuotaLease{}).
			Where("lease_id = ? AND status = ? AND expires_at_unix_milli > ? AND snapshot_id = ? AND snapshot_revision = ? AND pricing_revision = ? AND remaining_quota >= ?",
				request.LeaseID, dto.EdgeLeaseStatusActiveV1, request.NowUnixMilli,
				control.SnapshotID, control.SnapshotRevision, pricingDataset.Revision, request.Quota).
			Updates(map[string]any{
				"remaining_quota":       gorm.Expr("remaining_quota - ?", request.Quota),
				"reserved_quota":        gorm.Expr("reserved_quota + ?", request.Quota),
				"updated_at_unix_milli": request.NowUnixMilli,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return diagnoseEdgeLocalLeaseReservation(tx, request.LeaseID, request.Quota, request.NowUnixMilli, control, pricingDataset.Revision)
		}
		var lease EdgeLocalQuotaLease
		if err := tx.Where("lease_id = ?", request.LeaseID).First(&lease).Error; err != nil {
			return err
		}
		created := &EdgeLocalQuotaReservation{
			ReservationID: request.ReservationID, RequestID: request.RequestID, LeaseID: request.LeaseID,
			UserID: lease.UserID, TokenID: lease.TokenID, Status: EdgeLocalReservationStatusActive,
			ReservedQuota: request.Quota, CreatedAtUnixMilli: request.NowUnixMilli,
			UpdatedAtUnixMilli: request.NowUnixMilli,
		}
		if err := tx.Create(created).Error; err != nil {
			return err
		}
		reservation = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

func diagnoseEdgeLocalLeaseReservation(tx *gorm.DB, leaseID string, quota, nowUnixMilli int64, control EdgeLocalControlState, pricingRevision int64) error {
	var lease EdgeLocalQuotaLease
	if err := tx.Where("lease_id = ?", leaseID).First(&lease).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrEdgeLocalLeaseUnavailable
		}
		return err
	}
	if lease.Status != dto.EdgeLeaseStatusActiveV1 {
		return ErrEdgeLocalLeaseUnavailable
	}
	if lease.ExpiresAtUnixMilli <= nowUnixMilli {
		return ErrEdgeLocalLeaseExpired
	}
	if lease.SnapshotID != control.SnapshotID || lease.SnapshotRevision != control.SnapshotRevision || lease.PricingRevision != pricingRevision {
		return ErrEdgeLocalSnapshotMismatch
	}
	if lease.RemainingQuota < quota {
		return ErrEdgeLocalQuotaInsufficient
	}
	return ErrEdgeLocalLeaseUnavailable
}

func AdjustEdgeLocalReservation(db *gorm.DB, reservationID string, targetQuota, nowUnixMilli int64) (*EdgeLocalQuotaReservation, error) {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return nil, errors.New("edge local reservation adjustment requires SQLite")
	}
	if err := validateEdgeLocalIdentifier(reservationID); err != nil {
		return nil, err
	}
	if err := validateEdgeLocalQuota(targetQuota, true); err != nil {
		return nil, err
	}
	if nowUnixMilli <= 0 {
		return nil, errors.New("reservation adjustment time must be positive")
	}
	var adjusted *EdgeLocalQuotaReservation
	err := db.Transaction(func(tx *gorm.DB) error {
		var reservation EdgeLocalQuotaReservation
		if err := tx.Where("reservation_id = ?", reservationID).First(&reservation).Error; err != nil {
			return err
		}
		if reservation.Status != EdgeLocalReservationStatusActive {
			return ErrEdgeLocalReservationFinalized
		}
		delta := targetQuota - reservation.ReservedQuota
		if delta == 0 {
			adjusted = &reservation
			return nil
		}
		staged, err := edgeLocalReservationSettlementStaged(reservation)
		if err != nil {
			return err
		}
		if staged {
			return ErrEdgeLocalSettlementStaged
		}
		if delta > 0 {
			var control EdgeLocalControlState
			if err := tx.First(&control, edgeLocalControlStateID).Error; err != nil {
				return err
			}
			if control.SnapshotExpiresAtUnixMilli <= nowUnixMilli {
				return ErrEdgeLocalSnapshotExpired
			}
			var pricingDataset EdgeLocalDatasetState
			if err := tx.Where("dataset = ?", dto.EdgeSnapshotDatasetPricingV1).First(&pricingDataset).Error; err != nil {
				return ErrEdgeLocalSnapshotMismatch
			}
			result := tx.Model(&EdgeLocalQuotaLease{}).
				Where("lease_id = ? AND status = ? AND expires_at_unix_milli > ? AND snapshot_id = ? AND snapshot_revision = ? AND pricing_revision = ? AND remaining_quota >= ?",
					reservation.LeaseID, dto.EdgeLeaseStatusActiveV1, nowUnixMilli,
					control.SnapshotID, control.SnapshotRevision, pricingDataset.Revision, delta).
				Updates(map[string]any{
					"remaining_quota":       gorm.Expr("remaining_quota - ?", delta),
					"reserved_quota":        gorm.Expr("reserved_quota + ?", delta),
					"updated_at_unix_milli": nowUnixMilli,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return diagnoseEdgeLocalLeaseReservation(tx, reservation.LeaseID, delta, nowUnixMilli, control, pricingDataset.Revision)
			}
		} else {
			release := -delta
			result := tx.Model(&EdgeLocalQuotaLease{}).
				Where("lease_id = ? AND reserved_quota >= ?", reservation.LeaseID, release).
				Updates(map[string]any{
					"remaining_quota":       gorm.Expr("remaining_quota + ?", release),
					"reserved_quota":        gorm.Expr("reserved_quota - ?", release),
					"updated_at_unix_milli": nowUnixMilli,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrEdgeLocalAccountingCorruption
			}
		}
		reservation.ReservedQuota = targetQuota
		reservation.UpdatedAtUnixMilli = nowUnixMilli
		result := tx.Model(&EdgeLocalQuotaReservation{}).Where("reservation_id = ? AND status = ?", reservationID, EdgeLocalReservationStatusActive).
			Updates(map[string]any{"reserved_quota": targetQuota, "updated_at_unix_milli": nowUnixMilli})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalReservationConflict
		}
		adjusted = &reservation
		return nil
	})
	if err != nil {
		return nil, err
	}
	return adjusted, nil
}

func RefundEdgeLocalReservation(db *gorm.DB, reservationID string, nowUnixMilli int64) error {
	if db == nil || db.Dialector.Name() != "sqlite" {
		return errors.New("edge local reservation refund requires SQLite")
	}
	if err := validateEdgeLocalIdentifier(reservationID); err != nil {
		return err
	}
	if nowUnixMilli <= 0 {
		return errors.New("reservation refund time must be positive")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var reservation EdgeLocalQuotaReservation
		if err := tx.Where("reservation_id = ?", reservationID).First(&reservation).Error; err != nil {
			return err
		}
		switch reservation.Status {
		case EdgeLocalReservationStatusRefunded:
			return nil
		case EdgeLocalReservationStatusSettled:
			return ErrEdgeLocalReservationFinalized
		case EdgeLocalReservationStatusActive:
			staged, err := edgeLocalReservationSettlementStaged(reservation)
			if err != nil {
				return err
			}
			if staged {
				return ErrEdgeLocalSettlementStaged
			}
		default:
			return ErrEdgeLocalAccountingCorruption
		}
		result := tx.Model(&EdgeLocalQuotaLease{}).
			Where("lease_id = ? AND reserved_quota >= ?", reservation.LeaseID, reservation.ReservedQuota).
			Updates(map[string]any{
				"remaining_quota":       gorm.Expr("remaining_quota + ?", reservation.ReservedQuota),
				"reserved_quota":        gorm.Expr("reserved_quota - ?", reservation.ReservedQuota),
				"updated_at_unix_milli": nowUnixMilli,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalAccountingCorruption
		}
		result = tx.Model(&EdgeLocalQuotaReservation{}).
			Where("reservation_id = ? AND status = ?", reservationID, EdgeLocalReservationStatusActive).
			Updates(map[string]any{
				"status": EdgeLocalReservationStatusRefunded, "updated_at_unix_milli": nowUnixMilli,
				"finalized_at_unix_milli": nowUnixMilli,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEdgeLocalReservationConflict
		}
		return nil
	})
}

func validateEdgeLocalIdentifier(value string) error {
	if value == "" || len(value) > 64 {
		return errors.New("identifier must contain between 1 and 64 bytes")
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		switch character {
		case '-', '_', '.', ':':
			continue
		default:
			return errors.New("identifier must use lowercase ASCII letters, digits, '-', '_', '.' or ':'")
		}
	}
	return nil
}

func validateEdgeLocalQuota(quota int64, allowZero bool) error {
	if quota < 0 || quota > int64(common.MaxQuota) || (!allowZero && quota == 0) {
		return fmt.Errorf("quota must be between %d and %d", map[bool]int64{true: 0, false: 1}[allowZero], int64(common.MaxQuota))
	}
	return nil
}

func validateEdgeLocalSHA256(value string) error {
	if len(value) != 64 || strings.ToLower(value) != value {
		return errors.New("SHA-256 digest must be 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return errors.New("SHA-256 digest must be 64 lowercase hexadecimal characters")
	}
	return nil
}
