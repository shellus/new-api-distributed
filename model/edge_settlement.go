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
	ErrInvalidEdgeSettlementBlockStatus  = errors.New("invalid edge settlement block status")
	ErrInvalidEdgeConsumeLogOutboxStatus = errors.New("invalid edge consume log outbox status")
)

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
	ID                          int64   `json:"id" gorm:"primaryKey"`
	NodeID                      int64   `json:"node_id" gorm:"not null;uniqueIndex:ux_edge_usage_seq,priority:1;uniqueIndex:ux_edge_usage_event,priority:1;uniqueIndex:ux_edge_usage_res,priority:1"`
	NodeGeneration              int64   `json:"node_generation" gorm:"type:bigint;not null;uniqueIndex:ux_edge_usage_seq,priority:2;uniqueIndex:ux_edge_usage_event,priority:2;uniqueIndex:ux_edge_usage_res,priority:2"`
	BlockID                     int64   `json:"block_id" gorm:"not null;index"`
	EventUID                    string  `json:"event_uid" gorm:"type:varchar(64);not null;uniqueIndex:ux_edge_usage_event,priority:3"`
	ReservationUID              string  `json:"reservation_uid" gorm:"type:varchar(64);not null;uniqueIndex:ux_edge_usage_res,priority:3"`
	RequestUID                  string  `json:"request_uid" gorm:"type:varchar(64);not null;index"`
	Sequence                    int64   `json:"sequence" gorm:"type:bigint;not null;uniqueIndex:ux_edge_usage_seq,priority:3"`
	UserID                      int     `json:"user_id" gorm:"not null;index"`
	TokenID                     int     `json:"token_id" gorm:"not null;index"`
	SnapshotID                  int64   `json:"snapshot_id" gorm:"not null;index"`
	SnapshotRevision            int64   `json:"snapshot_revision" gorm:"type:bigint;not null"`
	PricingRevision             int64   `json:"pricing_revision" gorm:"type:bigint;not null"`
	BalanceRevision             int64   `json:"balance_revision" gorm:"type:bigint;not null"`
	FundingSource               string  `json:"funding_source" gorm:"type:varchar(32);not null;index"`
	UserSubscriptionID          int     `json:"user_subscription_id" gorm:"not null;index"`
	TokenUnlimitedQuota         bool    `json:"token_unlimited_quota" gorm:"not null"`
	ChannelID                   int     `json:"channel_id" gorm:"not null;index"`
	Endpoint                    string  `json:"endpoint" gorm:"type:varchar(64);not null"`
	Streaming                   bool    `json:"streaming" gorm:"not null"`
	Model                       string  `json:"model" gorm:"type:varchar(256);not null;index"`
	Group                       string  `json:"group" gorm:"type:varchar(64);not null;index"`
	Outcome                     string  `json:"outcome" gorm:"type:varchar(32);not null;index"`
	HTTPStatus                  int     `json:"http_status" gorm:"not null"`
	ErrorCode                   string  `json:"error_code" gorm:"type:varchar(128);not null"`
	StartedAtUnixMilli          int64   `json:"started_at_unix_milli" gorm:"type:bigint;not null"`
	FirstResponseAtUnixMilli    *int64  `json:"first_response_at_unix_milli,omitempty" gorm:"type:bigint"`
	FinishedAtUnixMilli         int64   `json:"finished_at_unix_milli" gorm:"type:bigint;not null;index"`
	PromptTokens                int     `json:"prompt_tokens" gorm:"not null"`
	CompletionTokens            int     `json:"completion_tokens" gorm:"not null"`
	ReservedQuota               int64   `json:"reserved_quota" gorm:"type:bigint;not null"`
	ChargedQuota                int64   `json:"charged_quota" gorm:"type:bigint;not null"`
	PricingPolicyID             string  `json:"pricing_policy_id" gorm:"type:varchar(64);not null"`
	PricingPolicyVersion        string  `json:"pricing_policy_version" gorm:"type:varchar(64);not null"`
	UsagePayload                string  `json:"usage_payload" gorm:"type:text;not null"`
	BillingPayload              string  `json:"billing_payload" gorm:"type:text;not null"`
	ConsumeLogSnapshotPayload   *string `json:"consume_log_snapshot_payload,omitempty" gorm:"type:text"`
	ConsumeLogSettlementPayload *string `json:"consume_log_settlement_payload,omitempty" gorm:"type:text"`
	CreatedAt                   int64   `json:"created_at" gorm:"type:bigint;not null;index"`
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
	if e.NodeID <= 0 || e.NodeGeneration <= 0 || e.BlockID <= 0 || e.Sequence <= 0 ||
		e.UserID <= 0 || e.TokenID <= 0 || e.SnapshotID <= 0 || e.SnapshotRevision <= 0 ||
		e.PricingRevision <= 0 || e.PricingRevision > e.SnapshotRevision || e.BalanceRevision <= 0 || e.ChannelID <= 0 {
		return errors.New("invalid edge usage event identity")
	}
	switch e.FundingSource {
	case "wallet":
		if e.UserSubscriptionID != 0 {
			return errors.New("wallet edge usage has a subscription ID")
		}
	case "subscription":
		if e.UserSubscriptionID <= 0 {
			return errors.New("subscription edge usage is missing its subscription ID")
		}
	default:
		return errors.New("invalid edge usage funding source")
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
	if e.FirstResponseAtUnixMilli != nil &&
		(*e.FirstResponseAtUnixMilli <= e.StartedAtUnixMilli || *e.FirstResponseAtUnixMilli > e.FinishedAtUnixMilli) {
		return errors.New("invalid edge usage event first response timestamp")
	}
	if e.PromptTokens < 0 || e.CompletionTokens < 0 || e.ReservedQuota < 0 || e.ChargedQuota < 0 ||
		e.ReservedQuota > int64(common.MaxQuota) || e.ChargedQuota > int64(common.MaxQuota) {
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
