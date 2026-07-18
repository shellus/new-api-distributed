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
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

const (
	edgeSQLiteBusyTimeoutMilliseconds = 30_000
	edgeLocalControlStateID           = 1
)

var (
	ErrEdgeLocalSnapshotMismatch     = errors.New("edge local balance state does not match the applied snapshot")
	ErrEdgeLocalSnapshotStale        = errors.New("edge local snapshot revision is stale")
	ErrEdgeLocalSnapshotConflict     = errors.New("edge local snapshot revision conflicts with applied state")
	ErrEdgeLocalQuotaInsufficient    = errors.New("edge local balance is insufficient")
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
	NodeID                     string `gorm:"type:varchar(64);not null;default:''"`
	NodeGeneration             int64  `gorm:"not null;default:0"`
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
	BalanceRevision            int64  `gorm:"not null;default:0"`
	BalanceInitialized         bool   `gorm:"not null;default:false"`
	BalanceSettlementSequence  int64  `gorm:"not null;default:0"`
	SettlementCircuitOpen      bool   `gorm:"not null;default:false"`
	SettlementCircuitEpoch     int64  `gorm:"not null;default:0"`
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

type EdgeBalanceAccountType string

const (
	EdgeBalanceAccountTypeWallet       EdgeBalanceAccountType = "wallet"
	EdgeBalanceAccountTypeToken        EdgeBalanceAccountType = "token"
	EdgeBalanceAccountTypeSubscription EdgeBalanceAccountType = "subscription"
)

type EdgeLocalBalanceAccount struct {
	AccountType          EdgeBalanceAccountType `gorm:"type:varchar(32);primaryKey;autoIncrement:false"`
	AccountID            int64                  `gorm:"primaryKey;autoIncrement:false"`
	UserID               int64                  `gorm:"not null;index"`
	ReplicatedQuota      int64                  `gorm:"not null"`
	UnlimitedQuota       bool                   `gorm:"not null"`
	ReservedQuota        int64                  `gorm:"not null"`
	UnsettledQuota       int64                  `gorm:"not null"`
	TotalQuota           int64                  `gorm:"not null"`
	NextResetAtUnixMilli int64                  `gorm:"not null"`
	ExpiresAtUnixMilli   int64                  `gorm:"not null"`
	AllowWalletOverflow  bool                   `gorm:"not null"`
	Deleted              bool                   `gorm:"not null;index"`
	BalanceRevision      int64                  `gorm:"not null;index"`
	UpdatedAtUnixMilli   int64                  `gorm:"not null"`
}

func (EdgeLocalBalanceAccount) TableName() string { return "edge_local_balance_accounts" }

type EdgeLocalReservationStatus string

const (
	EdgeLocalReservationStatusActive   EdgeLocalReservationStatus = "active"
	EdgeLocalReservationStatusSettled  EdgeLocalReservationStatus = "settled"
	EdgeLocalReservationStatusRefunded EdgeLocalReservationStatus = "refunded"
)

type EdgeLocalQuotaReservation struct {
	ReservationID        string                     `gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	RequestID            string                     `gorm:"type:varchar(64);not null;uniqueIndex"`
	UserID               int64                      `gorm:"not null;index"`
	TokenID              int64                      `gorm:"not null;index"`
	FundingAccountType   EdgeBalanceAccountType     `gorm:"type:varchar(32);not null;default:'';index"`
	FundingAccountID     int64                      `gorm:"not null;default:0;index"`
	TokenAccountID       int64                      `gorm:"not null;default:0"`
	TokenUnlimitedQuota  bool                       `gorm:"not null;default:false"`
	SnapshotID           string                     `gorm:"type:varchar(64);not null;default:''"`
	SnapshotRevision     int64                      `gorm:"not null;default:0"`
	PricingRevision      int64                      `gorm:"not null;default:0"`
	BalanceRevision      int64                      `gorm:"not null;default:0"`
	NegativeFloorQuota   int64                      `gorm:"not null;default:0"`
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
	RequestCircuitEpoch     int64                          `gorm:"not null;default:0"`
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
		&EdgeLocalBalanceAccount{},
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
	if err := migrateEdgeLocalLeaseSchema(db); err != nil {
		return err
	}
	return nil
}

func migrateEdgeLocalLeaseSchema(db *gorm.DB) error {
	hasLeaseTable := db.Migrator().HasTable("edge_local_quota_leases")
	hasIntentTable := db.Migrator().HasTable("edge_local_lease_acquire_intents")
	hasReservationLeaseID := db.Migrator().HasColumn("edge_local_quota_reservations", "lease_id")
	hasUsageLeaseID := db.Migrator().HasColumn("edge_local_usage_events", "lease_id")
	if !hasLeaseTable && !hasIntentTable && !hasReservationLeaseID && !hasUsageLeaseID {
		return nil
	}

	dirtyChecks := []struct {
		table string
		where string
		args  []any
		label string
	}{
		{table: "edge_local_quota_reservations", where: "status = ?", args: []any{EdgeLocalReservationStatusActive}, label: "active reservation"},
		{
			table: "edge_local_quota_reservations",
			where: "status <> ? AND (staged_event_id <> '' OR staged_event_payload <> '' OR staged_at_unix_milli <> 0)",
			args:  []any{EdgeLocalReservationStatusSettled},
			label: "non-settled staged usage",
		},
		{table: "edge_local_outbox", where: "status IN ?", args: []any{[]EdgeLocalOutboxStatus{EdgeLocalOutboxStatusPending, EdgeLocalOutboxStatusInBlock}}, label: "pending outbox"},
		{table: "edge_local_settlement_blocks", where: "status = ?", args: []any{EdgeLocalSettlementBlockStatusPending}, label: "pending settlement block"},
	}
	for _, check := range dirtyChecks {
		if !db.Migrator().HasTable(check.table) {
			continue
		}
		var count int64
		if err := db.Table(check.table).Where(check.where, check.args...).Count(&count).Error; err != nil {
			return fmt.Errorf("inspect edge SQLite %s before balance migration: %w", check.label, err)
		}
		if count != 0 {
			return fmt.Errorf("edge SQLite balance migration requires drained accounting: found %s", check.label)
		}
	}
	if hasLeaseTable && db.Migrator().HasColumn("edge_local_quota_leases", "status") {
		var count int64
		if err := db.Table("edge_local_quota_leases").Where("status IN ?", []string{"active", "closing", "revoked"}).Count(&count).Error; err != nil {
			return fmt.Errorf("inspect edge SQLite live leases before balance migration: %w", err)
		}
		if count != 0 {
			return errors.New("edge SQLite balance migration requires all quota leases to be closed")
		}
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if hasReservationLeaseID {
			if err := rebuildEdgeLocalTableTx(tx, &EdgeLocalQuotaReservation{}, "edge_local_quota_reservations"); err != nil {
				return fmt.Errorf("remove edge reservation lease_id: %w", err)
			}
		}
		if hasUsageLeaseID {
			if err := rebuildEdgeLocalTableTx(tx, &EdgeLocalUsageEvent{}, "edge_local_usage_events"); err != nil {
				return fmt.Errorf("remove edge usage lease_id: %w", err)
			}
		}
		if hasIntentTable {
			if err := tx.Migrator().DropTable("edge_local_lease_acquire_intents"); err != nil {
				return fmt.Errorf("drop edge lease acquire intents: %w", err)
			}
		}
		if hasLeaseTable {
			if err := tx.Migrator().DropTable("edge_local_quota_leases"); err != nil {
				return fmt.Errorf("drop edge local quota leases: %w", err)
			}
		}
		return tx.Model(&EdgeLocalControlState{}).Where("id = ?", edgeLocalControlStateID).
			Update("balance_settlement_sequence", gorm.Expr("last_acked_sequence")).Error
	})
}

func rebuildEdgeLocalTableTx(tx *gorm.DB, target any, tableName string) error {
	temporaryName := tableName + "_balance_migration"
	if tx.Migrator().HasTable(temporaryName) {
		return fmt.Errorf("temporary migration table %s already exists", temporaryName)
	}
	type sqliteColumn struct {
		Name string `gorm:"column:name"`
	}
	var legacyColumns []sqliteColumn
	if err := tx.Raw("PRAGMA table_info(\"" + tableName + "\")").Scan(&legacyColumns).Error; err != nil {
		return err
	}
	type sqliteIndex struct {
		Name string `gorm:"column:name"`
	}
	var indexes []sqliteIndex
	if err := tx.Raw("SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND name NOT LIKE 'sqlite_autoindex%'", tableName).
		Scan(&indexes).Error; err != nil {
		return err
	}
	for _, index := range indexes {
		quotedIndex := "\"" + strings.ReplaceAll(index.Name, "\"", "\"\"") + "\""
		if err := tx.Exec("DROP INDEX " + quotedIndex).Error; err != nil {
			return err
		}
	}
	if err := tx.Migrator().RenameTable(tableName, temporaryName); err != nil {
		return err
	}
	if err := tx.AutoMigrate(target); err != nil {
		return err
	}
	var targetColumns []sqliteColumn
	if err := tx.Raw("PRAGMA table_info(\"" + tableName + "\")").Scan(&targetColumns).Error; err != nil {
		return err
	}
	legacySet := make(map[string]struct{}, len(legacyColumns))
	for _, column := range legacyColumns {
		legacySet[column.Name] = struct{}{}
	}
	columns := make([]string, 0, len(targetColumns))
	for _, column := range targetColumns {
		if _, exists := legacySet[column.Name]; exists {
			columns = append(columns, "\""+strings.ReplaceAll(column.Name, "\"", "\"\"")+"\"")
		}
	}
	if len(columns) == 0 {
		return fmt.Errorf("no shared columns found while rebuilding %s", tableName)
	}
	quotedTable := "\"" + strings.ReplaceAll(tableName, "\"", "\"\"") + "\""
	quotedTemporary := "\"" + strings.ReplaceAll(temporaryName, "\"", "\"\"") + "\""
	columnList := strings.Join(columns, ", ")
	if err := tx.Exec("INSERT INTO " + quotedTable + " (" + columnList + ") SELECT " + columnList + " FROM " + quotedTemporary).Error; err != nil {
		return err
	}
	return tx.Migrator().DropTable(temporaryName)
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
		if err := refundEdgeLocalBalanceReservationTx(tx, reservation, nowUnixMilli); err != nil {
			return err
		}
		result := tx.Model(&EdgeLocalQuotaReservation{}).
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
