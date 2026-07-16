package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"gorm.io/gorm"
)

type EdgeQuotaLeaseStatus string

const (
	EdgeQuotaLeaseStatusActive      EdgeQuotaLeaseStatus = "active"
	EdgeQuotaLeaseStatusClosing     EdgeQuotaLeaseStatus = "closing"
	EdgeQuotaLeaseStatusClosed      EdgeQuotaLeaseStatus = "closed"
	EdgeQuotaLeaseStatusRevoked     EdgeQuotaLeaseStatus = "revoked"
	EdgeQuotaLeaseStatusForceClosed EdgeQuotaLeaseStatus = "force_closed"
)

type EdgeLeaseFundingSource string

const (
	EdgeLeaseFundingSourceWallet       EdgeLeaseFundingSource = "wallet"
	EdgeLeaseFundingSourceSubscription EdgeLeaseFundingSource = "subscription"
)

type EdgeLeaseFundingStatus string

const (
	EdgeLeaseFundingStatusReserved  EdgeLeaseFundingStatus = "reserved"
	EdgeLeaseFundingStatusReleased  EdgeLeaseFundingStatus = "released"
	EdgeLeaseFundingStatusForfeited EdgeLeaseFundingStatus = "forfeited"
)

type EdgeSettlementBlockStatus string

const (
	EdgeSettlementBlockStatusAccepted EdgeSettlementBlockStatus = "accepted"
)

type EdgeConsumeLogOutboxStatus string

const (
	EdgeConsumeLogOutboxStatusPending   EdgeConsumeLogOutboxStatus = "pending"
	EdgeConsumeLogOutboxStatusPublished EdgeConsumeLogOutboxStatus = "published"
	EdgeConsumeLogOutboxStatusFailed    EdgeConsumeLogOutboxStatus = "failed"
)

var (
	ErrInvalidEdgeQuotaLeaseStatus       = errors.New("invalid edge quota lease status")
	ErrInvalidEdgeQuotaLeaseTransition   = errors.New("invalid edge quota lease status transition")
	ErrInvalidEdgeLeaseFundingSource     = errors.New("invalid edge lease funding source")
	ErrInvalidEdgeLeaseFundingStatus     = errors.New("invalid edge lease funding status")
	ErrInvalidEdgeSettlementBlockStatus  = errors.New("invalid edge settlement block status")
	ErrInvalidEdgeConsumeLogOutboxStatus = errors.New("invalid edge consume log outbox status")
)

// EdgeQuotaLease is the master-owned reservation granted to one edge, user
// and token. Positive GrantedQuota is deducted from the authoritative funding
// source and (for finite tokens) token remain_quota before the row is created;
// a zero grant only carries auditable usage for a signed free pricing policy.
type EdgeQuotaLease struct {
	ID                       int64                  `json:"id" gorm:"primaryKey"`
	LeaseUID                 string                 `json:"lease_uid" gorm:"type:varchar(64);not null;uniqueIndex"`
	NodeID                   int64                  `json:"node_id" gorm:"not null;index:idx_edge_leases_node_status,priority:1;uniqueIndex:ux_edge_lease_request,priority:1"`
	NodeGeneration           int64                  `json:"node_generation" gorm:"type:bigint;not null;index:idx_edge_leases_node_status,priority:2;uniqueIndex:ux_edge_lease_request,priority:2"`
	UserID                   int                    `json:"user_id" gorm:"not null;index:idx_edge_leases_subject,priority:1"`
	TokenID                  int                    `json:"token_id" gorm:"not null;index:idx_edge_leases_subject,priority:2"`
	SnapshotID               int64                  `json:"snapshot_id" gorm:"not null;index"`
	SnapshotUID              string                 `json:"snapshot_uid" gorm:"type:varchar(64);not null"`
	SnapshotRevision         int64                  `json:"snapshot_revision" gorm:"type:bigint;not null"`
	PricingRevision          int64                  `json:"pricing_revision" gorm:"type:bigint;not null"`
	RequestIdempotencyKey    string                 `json:"request_idempotency_key" gorm:"type:varchar(64);not null;uniqueIndex:ux_edge_lease_request,priority:3"`
	RequestHash              string                 `json:"request_hash" gorm:"type:char(64);not null"`
	Version                  int64                  `json:"version" gorm:"type:bigint;not null"`
	Status                   EdgeQuotaLeaseStatus   `json:"status" gorm:"type:varchar(32);not null;index:idx_edge_leases_node_status,priority:3"`
	FundingSource            EdgeLeaseFundingSource `json:"funding_source" gorm:"type:varchar(32);not null"`
	TokenUnlimited           bool                   `json:"token_unlimited" gorm:"not null"`
	GrantedQuota             int64                  `json:"granted_quota" gorm:"type:bigint;not null"`
	ConsumedQuota            int64                  `json:"consumed_quota" gorm:"type:bigint;not null"`
	ReturnedQuota            int64                  `json:"returned_quota" gorm:"type:bigint;not null"`
	ForfeitedQuota           int64                  `json:"forfeited_quota" gorm:"type:bigint;not null"`
	RenewAfterRemainingQuota int64                  `json:"renew_after_remaining_quota" gorm:"type:bigint;not null"`
	IssuedAtUnixMilli        int64                  `json:"issued_at_unix_milli" gorm:"type:bigint;not null;index"`
	ExpiresAtUnixMilli       int64                  `json:"expires_at_unix_milli" gorm:"type:bigint;not null;index"`
	CloseAfterEventSequence  int64                  `json:"close_after_event_sequence" gorm:"type:bigint;not null"`
	ClosedAtUnixMilli        int64                  `json:"closed_at_unix_milli" gorm:"type:bigint;not null"`
	CreatedAt                int64                  `json:"created_at" gorm:"type:bigint;not null;index"`
	UpdatedAt                int64                  `json:"updated_at" gorm:"type:bigint;not null;index"`
}

// EdgeLeaseFunding records how the authoritative reservation was funded. V1
// uses exactly one funding row per lease; the separate table keeps the lease
// state machine independent from wallet and subscription persistence details.
type EdgeLeaseFunding struct {
	ID                 int64                  `json:"id" gorm:"primaryKey"`
	LeaseID            int64                  `json:"lease_id" gorm:"not null;uniqueIndex"`
	Source             EdgeLeaseFundingSource `json:"source" gorm:"type:varchar(32);not null;index"`
	UserID             int                    `json:"user_id" gorm:"not null;index"`
	UserSubscriptionID int                    `json:"user_subscription_id" gorm:"not null;index"`
	ReservedQuota      int64                  `json:"reserved_quota" gorm:"type:bigint;not null"`
	ConsumedQuota      int64                  `json:"consumed_quota" gorm:"type:bigint;not null"`
	ReturnedQuota      int64                  `json:"returned_quota" gorm:"type:bigint;not null"`
	ForfeitedQuota     int64                  `json:"forfeited_quota" gorm:"type:bigint;not null"`
	Status             EdgeLeaseFundingStatus `json:"status" gorm:"type:varchar(32);not null;index"`
	CreatedAt          int64                  `json:"created_at" gorm:"type:bigint;not null;index"`
	UpdatedAt          int64                  `json:"updated_at" gorm:"type:bigint;not null;index"`
}

// EdgeSettlementBlock is an accepted, contiguous block in a node generation.
// Rejected attempts are persisted by EdgeRequestReceipt so a retry receives
// the exact response without creating a partial accounting block.
type EdgeSettlementBlock struct {
	ID                      int64                     `json:"id" gorm:"primaryKey"`
	NodeID                  int64                     `json:"node_id" gorm:"not null;uniqueIndex:ux_edge_block_uid,priority:1;uniqueIndex:ux_edge_block_idem,priority:1;uniqueIndex:ux_edge_block_ordinal,priority:1"`
	NodeGeneration          int64                     `json:"node_generation" gorm:"type:bigint;not null;uniqueIndex:ux_edge_block_uid,priority:2;uniqueIndex:ux_edge_block_idem,priority:2;uniqueIndex:ux_edge_block_ordinal,priority:2"`
	BlockUID                string                    `json:"block_uid" gorm:"type:varchar(64);not null;uniqueIndex:ux_edge_block_uid,priority:3"`
	IdempotencyKey          string                    `json:"idempotency_key" gorm:"type:varchar(64);not null;uniqueIndex:ux_edge_block_idem,priority:3"`
	RequestHash             string                    `json:"request_hash" gorm:"type:char(64);not null"`
	BlockOrdinal            int64                     `json:"block_ordinal" gorm:"type:bigint;not null;uniqueIndex:ux_edge_block_ordinal,priority:3"`
	PreviousBlockUID        string                    `json:"previous_block_uid" gorm:"type:varchar(64);not null"`
	PreviousBlockDigest     string                    `json:"previous_block_digest" gorm:"type:char(64);not null"`
	FirstSequence           int64                     `json:"first_sequence" gorm:"type:bigint;not null"`
	LastSequence            int64                     `json:"last_sequence" gorm:"type:bigint;not null;index"`
	EventCount              int                       `json:"event_count" gorm:"not null"`
	BlockDigest             string                    `json:"block_digest" gorm:"type:char(64);not null"`
	Status                  EdgeSettlementBlockStatus `json:"status" gorm:"type:varchar(32);not null;index"`
	EdgeCreatedAtUnixMilli  int64                     `json:"edge_created_at_unix_milli" gorm:"type:bigint;not null"`
	AcknowledgedAtUnixMilli int64                     `json:"acknowledged_at_unix_milli" gorm:"type:bigint;not null;index"`
	CreatedAt               int64                     `json:"created_at" gorm:"type:bigint;not null;index"`
}

// EdgeUsageEvent is the immutable accounting fact accepted from an edge.
// UsagePayload and BillingPayload contain only the already allowlisted DTO
// payloads; request bodies, plaintext tokens and upstream credentials never
// enter this table.
type EdgeUsageEvent struct {
	ID                   int64  `json:"id" gorm:"primaryKey"`
	NodeID               int64  `json:"node_id" gorm:"not null;uniqueIndex:ux_edge_usage_seq,priority:1;uniqueIndex:ux_edge_usage_event,priority:1;uniqueIndex:ux_edge_usage_res,priority:1"`
	NodeGeneration       int64  `json:"node_generation" gorm:"type:bigint;not null;uniqueIndex:ux_edge_usage_seq,priority:2;uniqueIndex:ux_edge_usage_event,priority:2;uniqueIndex:ux_edge_usage_res,priority:2"`
	BlockID              int64  `json:"block_id" gorm:"not null;index"`
	LeaseID              int64  `json:"lease_id" gorm:"not null;index"`
	EventUID             string `json:"event_uid" gorm:"type:varchar(64);not null;uniqueIndex:ux_edge_usage_event,priority:3"`
	ReservationUID       string `json:"reservation_uid" gorm:"type:varchar(64);not null;uniqueIndex:ux_edge_usage_res,priority:3"`
	RequestUID           string `json:"request_uid" gorm:"type:varchar(64);not null;index"`
	Sequence             int64  `json:"sequence" gorm:"type:bigint;not null;uniqueIndex:ux_edge_usage_seq,priority:3"`
	UserID               int    `json:"user_id" gorm:"not null;index"`
	TokenID              int    `json:"token_id" gorm:"not null;index"`
	ChannelID            int    `json:"channel_id" gorm:"not null;index"`
	Endpoint             string `json:"endpoint" gorm:"type:varchar(64);not null"`
	Streaming            bool   `json:"streaming" gorm:"not null"`
	Model                string `json:"model" gorm:"type:varchar(256);not null;index"`
	Group                string `json:"group" gorm:"type:varchar(64);not null;index"`
	Outcome              string `json:"outcome" gorm:"type:varchar(32);not null;index"`
	HTTPStatus           int    `json:"http_status" gorm:"not null"`
	ErrorCode            string `json:"error_code" gorm:"type:varchar(128);not null"`
	StartedAtUnixMilli   int64  `json:"started_at_unix_milli" gorm:"type:bigint;not null"`
	FinishedAtUnixMilli  int64  `json:"finished_at_unix_milli" gorm:"type:bigint;not null;index"`
	PromptTokens         int    `json:"prompt_tokens" gorm:"not null"`
	CompletionTokens     int    `json:"completion_tokens" gorm:"not null"`
	ReservedQuota        int64  `json:"reserved_quota" gorm:"type:bigint;not null"`
	ChargedQuota         int64  `json:"charged_quota" gorm:"type:bigint;not null"`
	PricingPolicyID      string `json:"pricing_policy_id" gorm:"type:varchar(64);not null"`
	PricingPolicyVersion string `json:"pricing_policy_version" gorm:"type:varchar(64);not null"`
	UsagePayload         string `json:"usage_payload" gorm:"type:text;not null"`
	BillingPayload       string `json:"billing_payload" gorm:"type:text;not null"`
	CreatedAt            int64  `json:"created_at" gorm:"type:bigint;not null;index"`
}

// EdgeConsumeLogOutbox bridges the main accounting database transaction to
// LOG_DB, which may be a different database. A separate worker can publish
// each payload idempotently by EventID without weakening settlement atomicity.
type EdgeConsumeLogOutbox struct {
	ID      int64 `json:"id" gorm:"primaryKey"`
	EventID int64 `json:"event_id" gorm:"not null;uniqueIndex"`
	// EventUID is the master-global billing event key derived from node UID,
	// node generation and the edge-scoped event ID kept inside Payload.
	EventUID    string                     `json:"event_uid" gorm:"type:varchar(64);not null;uniqueIndex"`
	Payload     string                     `json:"payload" gorm:"type:text;not null"`
	Status      EdgeConsumeLogOutboxStatus `json:"status" gorm:"type:varchar(32);not null;index:idx_edge_log_outbox_ready,priority:1"`
	Attempts    int                        `json:"attempts" gorm:"not null"`
	AvailableAt int64                      `json:"available_at" gorm:"type:bigint;not null;index:idx_edge_log_outbox_ready,priority:2"`
	LastError   string                     `json:"last_error" gorm:"type:text;not null"`
	PublishedAt int64                      `json:"published_at" gorm:"type:bigint;not null"`
	CreatedAt   int64                      `json:"created_at" gorm:"type:bigint;not null;index"`
	UpdatedAt   int64                      `json:"updated_at" gorm:"type:bigint;not null;index"`
}

func (s EdgeQuotaLeaseStatus) Valid() bool {
	switch s {
	case EdgeQuotaLeaseStatusActive, EdgeQuotaLeaseStatusClosing, EdgeQuotaLeaseStatusClosed,
		EdgeQuotaLeaseStatusRevoked, EdgeQuotaLeaseStatusForceClosed:
		return true
	default:
		return false
	}
}

func (s EdgeQuotaLeaseStatus) Terminal() bool {
	return s == EdgeQuotaLeaseStatusClosed || s == EdgeQuotaLeaseStatusForceClosed
}

func (s EdgeLeaseFundingSource) Valid() bool {
	return s == EdgeLeaseFundingSourceWallet || s == EdgeLeaseFundingSourceSubscription
}

func (s EdgeLeaseFundingStatus) Valid() bool {
	return s == EdgeLeaseFundingStatusReserved || s == EdgeLeaseFundingStatusReleased || s == EdgeLeaseFundingStatusForfeited
}

func (s EdgeSettlementBlockStatus) Valid() bool {
	return s == EdgeSettlementBlockStatusAccepted
}

func (s EdgeConsumeLogOutboxStatus) Valid() bool {
	switch s {
	case EdgeConsumeLogOutboxStatusPending, EdgeConsumeLogOutboxStatusPublished, EdgeConsumeLogOutboxStatusFailed:
		return true
	default:
		return false
	}
}

func (l *EdgeQuotaLease) RemainingQuota() int64 {
	if l == nil {
		return 0
	}
	remaining := l.GrantedQuota - l.ConsumedQuota - l.ReturnedQuota - l.ForfeitedQuota
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (l *EdgeQuotaLease) ToDTO(nodeUID string) dto.EdgeQuotaLeaseV1 {
	if l == nil {
		return dto.EdgeQuotaLeaseV1{}
	}
	return dto.EdgeQuotaLeaseV1{
		LeaseID:                  l.LeaseUID,
		Version:                  l.Version,
		Status:                   dto.EdgeLeaseStatusV1(l.Status),
		NodeID:                   nodeUID,
		NodeGeneration:           l.NodeGeneration,
		Subject:                  dto.EdgeLeaseSubjectV1{UserID: int64(l.UserID), TokenID: int64(l.TokenID)},
		GrantedQuota:             l.GrantedQuota,
		RenewAfterRemainingQuota: l.RenewAfterRemainingQuota,
		IssuedAtUnixMilli:        l.IssuedAtUnixMilli,
		ExpiresAtUnixMilli:       l.ExpiresAtUnixMilli,
		SnapshotID:               l.SnapshotUID,
		SnapshotRevision:         l.SnapshotRevision,
		PricingRevision:          l.PricingRevision,
	}
}

func (l *EdgeQuotaLease) BeforeCreate(_ *gorm.DB) error {
	if l == nil {
		return errors.New("edge quota lease is nil")
	}
	if l.Version == 0 {
		l.Version = 1
	}
	if l.Status == "" {
		l.Status = EdgeQuotaLeaseStatusActive
	}
	if l.CreatedAt == 0 {
		l.CreatedAt = common.GetTimestamp()
	}
	if l.UpdatedAt == 0 {
		l.UpdatedAt = l.CreatedAt
	}
	return validateEdgeQuotaLease(l, true)
}

func (l *EdgeQuotaLease) BeforeUpdate(tx *gorm.DB) error {
	if l == nil || l.ID <= 0 {
		return errors.New("edge quota lease update requires a loaded model")
	}
	if err := validateEdgeQuotaLease(l, false); err != nil {
		return err
	}
	var existing EdgeQuotaLease
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).First(&existing, l.ID).Error; err != nil {
		return err
	}
	if !edgeQuotaLeaseTransitionAllowed(existing.Status, l.Status) {
		return ErrInvalidEdgeQuotaLeaseTransition
	}
	if existing.Status == l.Status && l.Version != existing.Version {
		return errors.New("edge quota lease version cannot change without a status transition")
	}
	if existing.Status != l.Status && l.Version != existing.Version+1 {
		return errors.New("edge quota lease status transition must increment version exactly once")
	}
	if existing.LeaseUID != l.LeaseUID || existing.NodeID != l.NodeID || existing.NodeGeneration != l.NodeGeneration ||
		existing.UserID != l.UserID || existing.TokenID != l.TokenID || existing.SnapshotID != l.SnapshotID ||
		existing.SnapshotUID != l.SnapshotUID || existing.SnapshotRevision != l.SnapshotRevision ||
		existing.PricingRevision != l.PricingRevision || existing.RequestIdempotencyKey != l.RequestIdempotencyKey ||
		existing.RequestHash != l.RequestHash || existing.FundingSource != l.FundingSource ||
		existing.TokenUnlimited != l.TokenUnlimited || existing.GrantedQuota != l.GrantedQuota ||
		existing.RenewAfterRemainingQuota != l.RenewAfterRemainingQuota || existing.IssuedAtUnixMilli != l.IssuedAtUnixMilli ||
		existing.ExpiresAtUnixMilli != l.ExpiresAtUnixMilli || existing.CreatedAt != l.CreatedAt {
		return errors.New("edge quota lease immutable identity changed")
	}
	if l.Version < existing.Version || l.ConsumedQuota < existing.ConsumedQuota || l.ReturnedQuota < existing.ReturnedQuota ||
		l.ForfeitedQuota < existing.ForfeitedQuota || l.CloseAfterEventSequence < existing.CloseAfterEventSequence {
		return errors.New("edge quota lease counters cannot move backwards")
	}
	return nil
}

func validateEdgeQuotaLease(l *EdgeQuotaLease, creating bool) error {
	if l.NodeID <= 0 || l.NodeGeneration <= 0 || l.UserID <= 0 || l.TokenID <= 0 || l.SnapshotID <= 0 {
		return errors.New("invalid edge quota lease identity")
	}
	if err := validateEdgeStoredIdentifier("lease UID", l.LeaseUID); err != nil {
		return err
	}
	if err := validateEdgeStoredIdentifier("snapshot UID", l.SnapshotUID); err != nil {
		return err
	}
	if err := edgeauth.ValidateIdempotencyKey(strings.TrimSpace(l.RequestIdempotencyKey)); err != nil {
		return err
	}
	if err := validateEdgeStoredHash(l.RequestHash); err != nil {
		return fmt.Errorf("invalid lease request hash: %w", err)
	}
	if l.SnapshotRevision <= 0 || l.PricingRevision <= 0 || l.PricingRevision > l.SnapshotRevision {
		return errors.New("invalid edge quota lease snapshot revisions")
	}
	if !l.Status.Valid() {
		return ErrInvalidEdgeQuotaLeaseStatus
	}
	if !l.FundingSource.Valid() {
		return ErrInvalidEdgeLeaseFundingSource
	}
	if l.Version <= 0 || l.GrantedQuota < 0 || l.GrantedQuota > int64(common.MaxQuota) {
		return errors.New("invalid edge quota lease version or grant")
	}
	for _, value := range []int64{l.ConsumedQuota, l.ReturnedQuota, l.ForfeitedQuota, l.RenewAfterRemainingQuota} {
		if value < 0 || value > int64(common.MaxQuota) {
			return errors.New("edge quota lease quota counter is out of range")
		}
	}
	if l.ConsumedQuota+l.ReturnedQuota+l.ForfeitedQuota > l.GrantedQuota {
		return errors.New("edge quota lease accounting exceeds its grant")
	}
	if l.GrantedQuota == 0 && l.RenewAfterRemainingQuota != 0 {
		return errors.New("zero edge quota lease must have a zero renewal threshold")
	}
	if l.GrantedQuota > 0 && l.RenewAfterRemainingQuota >= l.GrantedQuota {
		return errors.New("edge quota lease renew threshold must be below its grant")
	}
	if l.IssuedAtUnixMilli <= 0 || l.ExpiresAtUnixMilli <= l.IssuedAtUnixMilli {
		return errors.New("invalid edge quota lease lifetime")
	}
	if l.CloseAfterEventSequence < 0 || l.ClosedAtUnixMilli < 0 {
		return errors.New("edge quota lease close fields cannot be negative")
	}
	if creating && (l.ConsumedQuota != 0 || l.ReturnedQuota != 0 || l.ForfeitedQuota != 0 ||
		l.CloseAfterEventSequence != 0 || l.ClosedAtUnixMilli != 0 || l.Status != EdgeQuotaLeaseStatusActive) {
		return errors.New("new edge quota lease must be an unused active grant")
	}
	if l.Status.Terminal() {
		if l.ClosedAtUnixMilli <= 0 || l.ConsumedQuota+l.ReturnedQuota+l.ForfeitedQuota != l.GrantedQuota {
			return errors.New("terminal edge quota lease must fully account for its grant")
		}
	} else if l.ClosedAtUnixMilli != 0 {
		return errors.New("non-terminal edge quota lease cannot have a closed timestamp")
	}
	if l.Status == EdgeQuotaLeaseStatusForceClosed && l.ReturnedQuota != 0 {
		return errors.New("force-closed edge quota lease cannot return quota")
	}
	return nil
}

func edgeQuotaLeaseTransitionAllowed(from EdgeQuotaLeaseStatus, to EdgeQuotaLeaseStatus) bool {
	if from == to {
		return true
	}
	switch from {
	case EdgeQuotaLeaseStatusActive:
		return to == EdgeQuotaLeaseStatusClosing || to == EdgeQuotaLeaseStatusRevoked || to == EdgeQuotaLeaseStatusForceClosed
	case EdgeQuotaLeaseStatusClosing:
		return to == EdgeQuotaLeaseStatusClosed || to == EdgeQuotaLeaseStatusForceClosed
	case EdgeQuotaLeaseStatusRevoked:
		return to == EdgeQuotaLeaseStatusClosed || to == EdgeQuotaLeaseStatusForceClosed
	default:
		return false
	}
}

func (f *EdgeLeaseFunding) BeforeCreate(_ *gorm.DB) error {
	if f == nil {
		return errors.New("edge lease funding is nil")
	}
	if f.Status == "" {
		f.Status = EdgeLeaseFundingStatusReserved
	}
	if f.CreatedAt == 0 {
		f.CreatedAt = common.GetTimestamp()
	}
	if f.UpdatedAt == 0 {
		f.UpdatedAt = f.CreatedAt
	}
	return validateEdgeLeaseFunding(f, true)
}

func (f *EdgeLeaseFunding) BeforeUpdate(tx *gorm.DB) error {
	if f == nil || f.ID <= 0 {
		return errors.New("edge lease funding update requires a loaded model")
	}
	if err := validateEdgeLeaseFunding(f, false); err != nil {
		return err
	}
	var existing EdgeLeaseFunding
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).First(&existing, f.ID).Error; err != nil {
		return err
	}
	if existing.LeaseID != f.LeaseID || existing.Source != f.Source || existing.UserID != f.UserID ||
		existing.UserSubscriptionID != f.UserSubscriptionID || existing.ReservedQuota != f.ReservedQuota ||
		existing.CreatedAt != f.CreatedAt {
		return errors.New("edge lease funding immutable identity changed")
	}
	if f.ConsumedQuota < existing.ConsumedQuota || f.ReturnedQuota < existing.ReturnedQuota || f.ForfeitedQuota < existing.ForfeitedQuota {
		return errors.New("edge lease funding counters cannot move backwards")
	}
	if existing.Status != f.Status {
		if existing.Status != EdgeLeaseFundingStatusReserved ||
			(f.Status != EdgeLeaseFundingStatusReleased && f.Status != EdgeLeaseFundingStatusForfeited) {
			return ErrInvalidEdgeLeaseFundingStatus
		}
	} else if existing.Status != EdgeLeaseFundingStatusReserved &&
		(existing.ConsumedQuota != f.ConsumedQuota || existing.ReturnedQuota != f.ReturnedQuota || existing.ForfeitedQuota != f.ForfeitedQuota) {
		return errors.New("terminal edge lease funding is immutable")
	}
	return nil
}

func validateEdgeLeaseFunding(f *EdgeLeaseFunding, creating bool) error {
	if f == nil || f.LeaseID <= 0 || f.UserID <= 0 || f.ReservedQuota < 0 || f.ReservedQuota > int64(common.MaxQuota) {
		return errors.New("invalid edge lease funding identity or reservation")
	}
	if !f.Source.Valid() {
		return ErrInvalidEdgeLeaseFundingSource
	}
	if !f.Status.Valid() {
		return ErrInvalidEdgeLeaseFundingStatus
	}
	if f.Source == EdgeLeaseFundingSourceWallet && f.UserSubscriptionID != 0 {
		return errors.New("wallet edge lease funding cannot reference a subscription")
	}
	if f.Source == EdgeLeaseFundingSourceSubscription && f.UserSubscriptionID <= 0 {
		return errors.New("subscription edge lease funding requires a subscription")
	}
	if f.ConsumedQuota < 0 || f.ReturnedQuota < 0 || f.ForfeitedQuota < 0 ||
		f.ConsumedQuota+f.ReturnedQuota+f.ForfeitedQuota > f.ReservedQuota {
		return errors.New("edge lease funding accounting exceeds its reservation")
	}
	if creating && (f.Status != EdgeLeaseFundingStatusReserved || f.ConsumedQuota != 0 || f.ReturnedQuota != 0 || f.ForfeitedQuota != 0) {
		return errors.New("new edge lease funding must be reserved")
	}
	if f.Status == EdgeLeaseFundingStatusReleased && f.ConsumedQuota+f.ReturnedQuota+f.ForfeitedQuota != f.ReservedQuota {
		return errors.New("released edge lease funding must be fully accounted")
	}
	if f.Status == EdgeLeaseFundingStatusForfeited && (f.ReturnedQuota != 0 || f.ConsumedQuota+f.ForfeitedQuota != f.ReservedQuota) {
		return errors.New("forfeited edge lease funding cannot return quota")
	}
	return nil
}

func (b *EdgeSettlementBlock) BeforeCreate(_ *gorm.DB) error {
	if b == nil {
		return errors.New("edge settlement block is nil")
	}
	if b.Status == "" {
		b.Status = EdgeSettlementBlockStatusAccepted
	}
	if b.CreatedAt == 0 {
		b.CreatedAt = common.GetTimestamp()
	}
	return validateEdgeSettlementBlock(b)
}

func validateEdgeSettlementBlock(b *EdgeSettlementBlock) error {
	if b.NodeID <= 0 || b.NodeGeneration <= 0 || b.BlockOrdinal <= 0 {
		return errors.New("invalid edge settlement block identity")
	}
	if err := validateEdgeStoredIdentifier("block UID", b.BlockUID); err != nil {
		return err
	}
	if err := edgeauth.ValidateIdempotencyKey(strings.TrimSpace(b.IdempotencyKey)); err != nil {
		return err
	}
	if err := validateEdgeStoredHash(b.RequestHash); err != nil {
		return fmt.Errorf("invalid settlement request hash: %w", err)
	}
	if b.PreviousBlockUID == "" {
		if b.PreviousBlockDigest != "" {
			return errors.New("previous settlement block digest requires an ID")
		}
	} else {
		if err := validateEdgeStoredIdentifier("previous block UID", b.PreviousBlockUID); err != nil {
			return err
		}
		if err := validateEdgeStoredHash(b.PreviousBlockDigest); err != nil {
			return err
		}
	}
	if err := validateEdgeStoredHash(b.BlockDigest); err != nil {
		return err
	}
	if b.FirstSequence <= 0 || b.LastSequence < b.FirstSequence ||
		int64(b.EventCount) != b.LastSequence-b.FirstSequence+1 || b.EventCount <= 0 ||
		b.EventCount > dto.EdgeControlMaxSettlementEventsV1 {
		return errors.New("invalid edge settlement block sequence range")
	}
	if !b.Status.Valid() {
		return ErrInvalidEdgeSettlementBlockStatus
	}
	if b.EdgeCreatedAtUnixMilli <= 0 || b.AcknowledgedAtUnixMilli <= 0 {
		return errors.New("invalid edge settlement block timestamps")
	}
	return nil
}

func (e *EdgeUsageEvent) BeforeCreate(_ *gorm.DB) error {
	if e == nil {
		return errors.New("edge usage event is nil")
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = common.GetTimestamp()
	}
	return validateEdgeUsageEvent(e)
}

func validateEdgeUsageEvent(e *EdgeUsageEvent) error {
	if e.NodeID <= 0 || e.NodeGeneration <= 0 || e.BlockID <= 0 || e.LeaseID <= 0 ||
		e.Sequence <= 0 || e.UserID <= 0 || e.TokenID <= 0 || e.ChannelID <= 0 {
		return errors.New("invalid edge usage event identity")
	}
	for name, value := range map[string]string{
		"event UID": e.EventUID, "reservation UID": e.ReservationUID, "request UID": e.RequestUID,
		"pricing policy ID": e.PricingPolicyID, "pricing policy version": e.PricingPolicyVersion,
	} {
		if err := validateEdgeStoredIdentifier(name, value); err != nil {
			return err
		}
	}
	if strings.TrimSpace(e.Endpoint) == "" || strings.TrimSpace(e.Model) == "" || strings.TrimSpace(e.Group) == "" || strings.TrimSpace(e.Outcome) == "" {
		return errors.New("edge usage event routing fields are empty")
	}
	if e.StartedAtUnixMilli <= 0 || e.FinishedAtUnixMilli < e.StartedAtUnixMilli {
		return errors.New("invalid edge usage event timestamps")
	}
	if e.PromptTokens < 0 || e.CompletionTokens < 0 || e.ReservedQuota < 0 || e.ChargedQuota < 0 ||
		e.ReservedQuota > int64(common.MaxQuota) || e.ChargedQuota > e.ReservedQuota {
		return errors.New("invalid edge usage event accounting values")
	}
	if strings.TrimSpace(e.UsagePayload) == "" || strings.TrimSpace(e.BillingPayload) == "" {
		return errors.New("edge usage event payloads are empty")
	}
	return nil
}

func (o *EdgeConsumeLogOutbox) BeforeCreate(_ *gorm.DB) error {
	if o == nil {
		return errors.New("edge consume-log outbox is nil")
	}
	if o.Status == "" {
		o.Status = EdgeConsumeLogOutboxStatusPending
	}
	if o.CreatedAt == 0 {
		o.CreatedAt = common.GetTimestamp()
	}
	if o.UpdatedAt == 0 {
		o.UpdatedAt = o.CreatedAt
	}
	if o.AvailableAt == 0 {
		o.AvailableAt = o.CreatedAt
	}
	return validateEdgeConsumeLogOutbox(o)
}

func (o *EdgeConsumeLogOutbox) BeforeUpdate(_ *gorm.DB) error {
	return validateEdgeConsumeLogOutbox(o)
}

func validateEdgeConsumeLogOutbox(o *EdgeConsumeLogOutbox) error {
	if o == nil || o.EventID <= 0 || strings.TrimSpace(o.Payload) == "" || o.Attempts < 0 || o.AvailableAt <= 0 {
		return errors.New("invalid edge consume-log outbox")
	}
	if err := validateEdgeStoredIdentifier("event UID", o.EventUID); err != nil {
		return err
	}
	if !o.Status.Valid() {
		return ErrInvalidEdgeConsumeLogOutboxStatus
	}
	if o.Status == EdgeConsumeLogOutboxStatusPublished && o.PublishedAt <= 0 {
		return errors.New("published edge consume-log outbox requires a timestamp")
	}
	return nil
}

func LockEdgeQuotaLeaseByUIDTx(tx *gorm.DB, nodeID int64, generation int64, leaseUID string) (*EdgeQuotaLease, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if nodeID <= 0 || generation <= 0 {
		return nil, errors.New("invalid edge lease node identity")
	}
	if err := validateEdgeStoredIdentifier("lease UID", leaseUID); err != nil {
		return nil, err
	}
	var lease EdgeQuotaLease
	if err := lockForUpdate(tx).
		Where("node_id = ? AND node_generation = ? AND lease_uid = ?", nodeID, generation, leaseUID).
		First(&lease).Error; err != nil {
		return nil, err
	}
	return &lease, nil
}

func LockEdgeLeaseFundingTx(tx *gorm.DB, leaseID int64) (*EdgeLeaseFunding, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if leaseID <= 0 {
		return nil, errors.New("invalid edge lease ID")
	}
	var funding EdgeLeaseFunding
	if err := lockForUpdate(tx).Where("lease_id = ?", leaseID).First(&funding).Error; err != nil {
		return nil, err
	}
	return &funding, nil
}

func validateEdgeStoredIdentifier(name string, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > dto.EdgeControlMaxIdentifierLengthV1 {
		return fmt.Errorf("%s is empty or too long", name)
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' || ch == ':' {
			continue
		}
		return fmt.Errorf("%s is not canonical", name)
	}
	return nil
}

func validateEdgeStoredHash(value string) error {
	_, err := normalizeEdgeControlHash(value)
	return err
}
