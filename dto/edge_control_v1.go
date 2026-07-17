package dto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	"github.com/QuantumNous/new-api/pkg/edgetoken"
)

const (
	EdgeControlProtocolVersionV1                = "edge-control.v1"
	EdgeControlMaxIdentifierLengthV1            = 64
	EdgeControlMaxPublicURLLengthV1             = 2048
	EdgeControlMaxNodeNameLengthV1              = 128
	EdgeControlMaxRegionLengthV1                = 64
	EdgeControlMaxSoftwareVersionLengthV1       = 64
	EdgeControlMaxModelLengthV1                 = 256
	EdgeControlMaxSupportedProtocolVersionsV1   = 8
	EdgeControlMaxCapabilitiesV1                = 32
	EdgeControlMaxSnapshotDatasetsV1            = 7
	EdgeControlMaxHeartbeatLeasesV1             = 4096
	EdgeControlMaxHeartbeatCPAStatusesV1        = 4096
	EdgeControlMaxAvailableModelsV1             = 4096
	EdgeControlMaxChannelGroupsV1               = 256
	EdgeControlMaxChannelModelsV1               = 4096
	EdgeControlMaxHeartbeatIntervalSecondsV1    = int64(3600)
	EdgeControlMaxSnapshotPollIntervalSecondsV1 = int64(86400)
	EdgeControlMaxSnapshotPageLimitV1           = 10000
	EdgeControlMaxSettlementEventsV1            = 10000
	EdgeControlMaxSettlementDelaySecondsV1      = int64(3600)
	EdgeControlMaxClockSkewToleranceSecondsV1   = int64(900)
	EdgeControlMaxSnapshotVerificationKeysV1    = 16
	EdgeControlMaxSnapshotItemsV1               = int64(10_000_000)
	EdgeControlMaxSnapshotPagesV1               = 1_000_000
	EdgeControlMaxBillingExpressionLengthV1     = 65_536
	EdgeControlMaxAppliedRatiosV1               = 64
	EdgeControlMaxBillingTokenCountV1           = common.MaxQuota / 2
	EdgeControlMaxBillingUsageDetailsV1         = 64
	EdgeControlMaxRoutingRulesV1                = 256
	EdgeControlMaxAffinityKeySourcesV1          = 16
	EdgeControlMaxAffinityRegexesV1             = 64
	EdgeControlMaxAffinityRegexLengthV1         = 2048
	EdgeControlMaxAffinityIncludesV1            = 64
	EdgeControlMaxAffinityIncludeLengthV1       = 256
	EdgeControlMaxAffinityPassHeadersV1         = 128
	EdgeControlMaxAffinitySourcePathLengthV1    = 1024
	EdgeControlMaxAffinityEntriesV1             = 10_000_000
	EdgeControlMaxAffinityTTLSecondsV1          = int64(31_536_000)

	edgeControlMinUnixMilliV1 = int64(946684800000)    // 2000-01-01T00:00:00Z
	edgeControlMaxUnixMilliV1 = int64(253402300799999) // 9999-12-31T23:59:59.999Z
)

type EdgeEndpointV1 string

const (
	EdgeEndpointOpenAIChatCompletionsV1 EdgeEndpointV1 = "openai_chat_completions"
	EdgeEndpointOpenAIResponsesV1       EdgeEndpointV1 = "openai_responses"
)

type EdgeLocalServiceV1 string

// These names remain as source-compatible conveniences for existing callers;
// Valid accepts any canonical service identifier and does not use this list as
// an allowlist.
const (
	EdgeLocalServiceCPAVIPV1     EdgeLocalServiceV1 = "cpa-vip"
	EdgeLocalServiceCPAPro20x4V1 EdgeLocalServiceV1 = "cpa-pro20x4"
	EdgeLocalServiceCPAPro20x5V1 EdgeLocalServiceV1 = "cpa-pro20x5"
	EdgeLocalServiceCPAPro20x6V1 EdgeLocalServiceV1 = "cpa-pro20x6"
)

type EdgeSnapshotDatasetV1 string

const (
	EdgeSnapshotDatasetAuthenticationV1 EdgeSnapshotDatasetV1 = "authentication"
	EdgeSnapshotDatasetUsersV1          EdgeSnapshotDatasetV1 = "users"
	EdgeSnapshotDatasetGroupsV1         EdgeSnapshotDatasetV1 = "groups"
	EdgeSnapshotDatasetModelsV1         EdgeSnapshotDatasetV1 = "models"
	EdgeSnapshotDatasetChannelsV1       EdgeSnapshotDatasetV1 = "channels"
	EdgeSnapshotDatasetPricingV1        EdgeSnapshotDatasetV1 = "pricing"
	EdgeSnapshotDatasetRoutingV1        EdgeSnapshotDatasetV1 = "routing"
)

type EdgeChannelAffinityKeySourceTypeV1 string

const (
	EdgeChannelAffinityKeySourceContextIntV1    EdgeChannelAffinityKeySourceTypeV1 = "context_int"
	EdgeChannelAffinityKeySourceContextStringV1 EdgeChannelAffinityKeySourceTypeV1 = "context_string"
	EdgeChannelAffinityKeySourceRequestHeaderV1 EdgeChannelAffinityKeySourceTypeV1 = "request_header"
	EdgeChannelAffinityKeySourceGJSONV1         EdgeChannelAffinityKeySourceTypeV1 = "gjson"
)

type EdgeBillingModeV1 string

const (
	EdgeBillingModeRatioV1      EdgeBillingModeV1 = "ratio"
	EdgeBillingModeFixedPriceV1 EdgeBillingModeV1 = "fixed_price"
	EdgeBillingModeTieredExprV1 EdgeBillingModeV1 = "tiered_expr"
)

type EdgeUsageOutcomeV1 string

const (
	EdgeUsageOutcomeSuccessV1       EdgeUsageOutcomeV1 = "success"
	EdgeUsageOutcomeUpstreamErrorV1 EdgeUsageOutcomeV1 = "upstream_error"
	EdgeUsageOutcomeClientCancelV1  EdgeUsageOutcomeV1 = "client_cancel"
	EdgeUsageOutcomeRejectedV1      EdgeUsageOutcomeV1 = "rejected"
)

type EdgeSettlementAckStatusV1 string

const (
	EdgeSettlementAckAcceptedV1  EdgeSettlementAckStatusV1 = "accepted"
	EdgeSettlementAckDuplicateV1 EdgeSettlementAckStatusV1 = "duplicate"
)

type EdgeLeaseStatusV1 string

const (
	EdgeLeaseStatusActiveV1      EdgeLeaseStatusV1 = "active"
	EdgeLeaseStatusClosingV1     EdgeLeaseStatusV1 = "closing"
	EdgeLeaseStatusClosedV1      EdgeLeaseStatusV1 = "closed"
	EdgeLeaseStatusRevokedV1     EdgeLeaseStatusV1 = "revoked"
	EdgeLeaseStatusForceClosedV1 EdgeLeaseStatusV1 = "force_closed"
)

type EdgeControlErrorCodeV1 string

const (
	EdgeControlErrorCodeInvalidRequestV1         EdgeControlErrorCodeV1 = "invalid_request"
	EdgeControlErrorCodeUnsupportedProtocolV1    EdgeControlErrorCodeV1 = "unsupported_protocol"
	EdgeControlErrorCodeAuthenticationFailedV1   EdgeControlErrorCodeV1 = "authentication_failed"
	EdgeControlErrorCodeInvalidSignatureV1       EdgeControlErrorCodeV1 = "invalid_signature"
	EdgeControlErrorCodeReplayDetectedV1         EdgeControlErrorCodeV1 = "replay_detected"
	EdgeControlErrorCodeNodeDisabledV1           EdgeControlErrorCodeV1 = "node_disabled"
	EdgeControlErrorCodeIdempotencyConflictV1    EdgeControlErrorCodeV1 = "idempotency_conflict"
	EdgeControlErrorCodeSnapshotNotFoundV1       EdgeControlErrorCodeV1 = "snapshot_not_found"
	EdgeControlErrorCodeSnapshotCursorStaleV1    EdgeControlErrorCodeV1 = "snapshot_cursor_stale"
	EdgeControlErrorCodeLeaseUnavailableV1       EdgeControlErrorCodeV1 = "lease_unavailable"
	EdgeControlErrorCodeLeaseConflictV1          EdgeControlErrorCodeV1 = "lease_conflict"
	EdgeControlErrorCodeSettlementOutOfOrderV1   EdgeControlErrorCodeV1 = "settlement_out_of_order"
	EdgeControlErrorCodeSettlementConflictV1     EdgeControlErrorCodeV1 = "settlement_conflict"
	EdgeControlErrorCodeRateLimitedV1            EdgeControlErrorCodeV1 = "rate_limited"
	EdgeControlErrorCodeTemporarilyUnavailableV1 EdgeControlErrorCodeV1 = "temporarily_unavailable"
	EdgeControlErrorCodeInternalV1               EdgeControlErrorCodeV1 = "internal_error"
)

// EdgeControlRequestMetaV1 contains protocol correlation only. Node identity,
// generation, timestamp, nonce, idempotency and the Ed25519 signature belong
// to the canonical HTTP transport metadata defined by pkg/edgeauth.
type EdgeControlRequestMetaV1 struct {
	ProtocolVersion string `json:"protocol_version"`
	RequestID       string `json:"request_id"`
}

type EdgeControlResponseMetaV1 struct {
	ProtocolVersion     string `json:"protocol_version"`
	RequestID           string `json:"request_id,omitempty"`
	ServerRequestID     string `json:"server_request_id"`
	ServerTimeUnixMilli int64  `json:"server_time_unix_milli"`
}

type EdgeEndpointCapabilityV1 struct {
	Endpoint  EdgeEndpointV1 `json:"endpoint"`
	Streaming bool           `json:"streaming"`
}

// EdgeNodeDeclarationV1 is edge-owned runtime state. PublicURL is declared by
// the authenticated edge; CPA addresses and credentials are intentionally not
// part of the contract.
type EdgeNodeDeclarationV1 struct {
	Name               string                     `json:"name"`
	Region             string                     `json:"region,omitempty"`
	PublicURL          string                     `json:"public_url"`
	SoftwareVersion    string                     `json:"software_version"`
	StartedAtUnixMilli int64                      `json:"started_at_unix_milli"`
	Capabilities       []EdgeEndpointCapabilityV1 `json:"capabilities"`
}

type EdgeSnapshotDatasetStateV1 struct {
	Dataset  EdgeSnapshotDatasetV1 `json:"dataset"`
	Revision int64                 `json:"revision"`
}

type EdgeSnapshotStateV1 struct {
	SnapshotID         string                       `json:"snapshot_id,omitempty"`
	Revision           int64                        `json:"revision,omitempty"`
	AppliedAtUnixMilli int64                        `json:"applied_at_unix_milli,omitempty"`
	Datasets           []EdgeSnapshotDatasetStateV1 `json:"datasets,omitempty"`
}

type EdgeSettlementStateV1 struct {
	LastAckedSequence      int64  `json:"last_acked_sequence"`
	LastAckedBlockID       string `json:"last_acked_block_id,omitempty"`
	NextEventSequence      int64  `json:"next_event_sequence"`
	PendingEventCount      int64  `json:"pending_event_count"`
	PendingBlockCount      int64  `json:"pending_block_count"`
	OldestPendingUnixMilli int64  `json:"oldest_pending_unix_milli,omitempty"`
}

type EdgeBootstrapRequestV1 struct {
	Meta                      EdgeControlRequestMetaV1 `json:"meta"`
	SupportedProtocolVersions []string                 `json:"supported_protocol_versions"`
	Declaration               EdgeNodeDeclarationV1    `json:"declaration"`
	Snapshot                  EdgeSnapshotStateV1      `json:"snapshot"`
	Settlement                EdgeSettlementStateV1    `json:"settlement"`
}

type EdgeNodeControlConfigV1 struct {
	NodeID                      string                          `json:"node_id"`
	NodeGeneration              int64                           `json:"node_generation"`
	Enabled                     bool                            `json:"enabled"`
	HeartbeatIntervalSeconds    int64                           `json:"heartbeat_interval_seconds"`
	SnapshotPollIntervalSeconds int64                           `json:"snapshot_poll_interval_seconds"`
	SnapshotPageLimit           int                             `json:"snapshot_page_limit"`
	SettlementMaxEvents         int                             `json:"settlement_max_events"`
	SettlementMaxDelaySeconds   int64                           `json:"settlement_max_delay_seconds"`
	ClockSkewToleranceSeconds   int64                           `json:"clock_skew_tolerance_seconds"`
	SnapshotVerificationKeys    []EdgeSnapshotVerificationKeyV1 `json:"snapshot_verification_keys"`
}

type EdgeBootstrapResponseV1 struct {
	Meta          EdgeControlResponseMetaV1 `json:"meta"`
	Control       EdgeNodeControlConfigV1   `json:"control"`
	Snapshot      EdgeSnapshotManifestV1    `json:"snapshot"`
	SettlementAck *EdgeSettlementAckV1      `json:"settlement_ack,omitempty"`
}

type EdgeTokenFingerprintSchemeV1 struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id,omitempty"`
	Version   int    `json:"version"`
}

// EdgeSnapshotVerificationKeyV1 is a master snapshot-signing public key made
// available to an edge. Future keys may be delivered before NotBefore so key
// rotation does not require a synchronous master call while applying a page.
type EdgeSnapshotVerificationKeyV1 struct {
	KeyID              string `json:"key_id"`
	Algorithm          string `json:"algorithm"`
	PublicKey          string `json:"public_key"`
	NotBeforeUnixMilli int64  `json:"not_before_unix_milli"`
	ExpiresAtUnixMilli int64  `json:"expires_at_unix_milli"`
}

// EdgeDetachedContentSignatureV1 authenticates immutable snapshot payload
// bytes identified by PayloadDigest. DetachedSignature lives outside those
// bytes, so the signature never recursively covers its own value.
type EdgeDetachedContentSignatureV1 struct {
	Algorithm     string `json:"algorithm"`
	KeyID         string `json:"key_id"`
	PayloadDigest string `json:"payload_digest"`
	Value         string `json:"value"`
}

type EdgeSnapshotDatasetManifestV1 struct {
	Dataset           EdgeSnapshotDatasetV1          `json:"dataset"`
	Revision          int64                          `json:"revision"`
	ItemCount         int64                          `json:"item_count"`
	PageCount         int                            `json:"page_count"`
	Digest            string                         `json:"digest"`
	DetachedSignature EdgeDetachedContentSignatureV1 `json:"detached_signature"`
}

// EdgeSnapshotManifestV1 describes an immutable snapshot. Dataset pages can
// be retried safely by snapshot ID and cursor until ExpiresAtUnixMilli.
type EdgeSnapshotManifestV1 struct {
	SnapshotID         string                          `json:"snapshot_id"`
	Revision           int64                           `json:"revision"`
	CreatedAtUnixMilli int64                           `json:"created_at_unix_milli"`
	ExpiresAtUnixMilli int64                           `json:"expires_at_unix_milli"`
	HashAlgorithm      string                          `json:"hash_algorithm"`
	Digest             string                          `json:"digest"`
	TokenFingerprint   EdgeTokenFingerprintSchemeV1    `json:"token_fingerprint"`
	Datasets           []EdgeSnapshotDatasetManifestV1 `json:"datasets"`
}

type EdgeSnapshotManifestRequestV1 struct {
	Meta    EdgeControlRequestMetaV1 `json:"meta"`
	Current EdgeSnapshotStateV1      `json:"current"`
}

type EdgeSnapshotManifestResponseV1 struct {
	Meta     EdgeControlResponseMetaV1 `json:"meta"`
	Changed  bool                      `json:"changed"`
	Snapshot *EdgeSnapshotManifestV1   `json:"snapshot,omitempty"`
}

type EdgeSnapshotPageRequestV1 struct {
	Meta       EdgeControlRequestMetaV1 `json:"meta"`
	SnapshotID string                   `json:"snapshot_id"`
	Dataset    EdgeSnapshotDatasetV1    `json:"dataset"`
	Cursor     string                   `json:"cursor,omitempty"`
	Limit      int                      `json:"limit"`
}

type EdgeTokenAuthRecordV1 struct {
	TokenFingerprint   string   `json:"token_fingerprint"`
	TokenID            int64    `json:"token_id"`
	UserID             int64    `json:"user_id"`
	Enabled            bool     `json:"enabled"`
	ExpiresAtUnixMilli *int64   `json:"expires_at_unix_milli,omitempty"`
	Group              string   `json:"group,omitempty"`
	ModelLimitEnabled  bool     `json:"model_limit_enabled"`
	AllowedModels      []string `json:"allowed_models,omitempty"`
	AllowedCIDRs       []string `json:"allowed_cidrs,omitempty"`
	CrossGroupRetry    bool     `json:"cross_group_retry"`
}

// EdgeUserSettingV1 is the allowlisted subset of user settings consumed by the
// text relay. Billing preference, notification endpoints and their secrets are
// deliberately excluded because lease funding is authoritative on edge.
type EdgeUserSettingV1 struct {
	AcceptUnsetRatioModel bool   `json:"accept_unset_ratio_model"`
	Language              string `json:"language,omitempty"`
}

// EdgeUserPolicyV1 is the non-secret user context required by local token
// authorization, relay logging and text billing. Role is deliberately absent:
// edge does not honor the admin-only token suffix that selects a channel.
type EdgeUserPolicyV1 struct {
	UserID       int64             `json:"user_id"`
	Enabled      bool              `json:"enabled"`
	Username     string            `json:"username"`
	Email        string            `json:"email,omitempty"`
	DefaultGroup string            `json:"default_group"`
	Setting      EdgeUserSettingV1 `json:"setting"`
}

type EdgeUsingGroupPolicyV1 struct {
	Group   string  `json:"group"`
	Enabled bool    `json:"enabled"`
	Ratio   float64 `json:"ratio"`
}

type EdgeGroupPolicyV1 struct {
	UserGroup   string                   `json:"user_group"`
	UsingGroups []EdgeUsingGroupPolicyV1 `json:"using_groups"`
}

type EdgeModelPolicyV1 struct {
	Model      string           `json:"model"`
	Enabled    bool             `json:"enabled"`
	Endpoints  []EdgeEndpointV1 `json:"endpoints"`
	Streaming  bool             `json:"streaming"`
	ChannelIDs []int64          `json:"channel_ids"`
}

// EdgeTextRequestPolicyV1 contains only request behavior needed by the first
// Chat/Responses text boundary. Secret-bearing header overrides and proxies
// are intentionally absent.
type EdgeTextRequestPolicyV1 struct {
	ForceFormat             bool   `json:"force_format"`
	ThinkingToContent       bool   `json:"thinking_to_content"`
	PassThroughBodyEnabled  bool   `json:"pass_through_body_enabled"`
	SystemPrompt            string `json:"system_prompt,omitempty"`
	SystemPromptOverride    bool   `json:"system_prompt_override"`
	AllowServiceTier        bool   `json:"allow_service_tier"`
	AllowInferenceGeo       bool   `json:"allow_inference_geo"`
	AllowSpeed              bool   `json:"allow_speed"`
	DisableStore            bool   `json:"disable_store"`
	AllowSafetyIdentifier   bool   `json:"allow_safety_identifier"`
	AllowIncludeObfuscation bool   `json:"allow_include_obfuscation"`
}

// EdgeChannelProjectionV1 names an edge-local upstream service instead of
// carrying the master channel key or base URL.
type EdgeChannelProjectionV1 struct {
	ChannelID         int64                   `json:"channel_id"`
	Type              int                     `json:"type"`
	Name              string                  `json:"name"`
	Enabled           bool                    `json:"enabled"`
	Groups            []string                `json:"groups"`
	Models            []string                `json:"models"`
	ModelMapping      map[string]string       `json:"model_mapping,omitempty"`
	Priority          int64                   `json:"priority"`
	Weight            int                     `json:"weight"`
	LocalService      EdgeLocalServiceV1      `json:"local_service"`
	StatusCodeMapping map[string]int          `json:"status_code_mapping,omitempty"`
	TextPolicy        EdgeTextRequestPolicyV1 `json:"text_policy"`
}

type EdgePricingPolicyV1 struct {
	PolicyID                 string            `json:"policy_id"`
	Version                  string            `json:"version"`
	Model                    string            `json:"model"`
	BillingMode              EdgeBillingModeV1 `json:"billing_mode"`
	ModelPrice               *float64          `json:"model_price,omitempty"`
	ModelRatio               *float64          `json:"model_ratio,omitempty"`
	CompletionRatio          *float64          `json:"completion_ratio,omitempty"`
	CacheReadRatio           *float64          `json:"cache_read_ratio,omitempty"`
	CacheCreationRatio       *float64          `json:"cache_creation_ratio,omitempty"`
	CacheCreation1hRatio     *float64          `json:"cache_creation_1h_ratio,omitempty"`
	BillingExpression        string            `json:"billing_expression,omitempty"`
	BillingExpressionHash    string            `json:"billing_expression_hash,omitempty"`
	BillingExpressionVersion int               `json:"billing_expression_version,omitempty"`
	QuotaPerUnit             float64           `json:"quota_per_unit"`
}

// EdgeChannelAffinityKeySourceV1 describes one ordered affinity-key probe.
// It names a safe, typed read source only; it cannot modify request fields.
type EdgeChannelAffinityKeySourceV1 struct {
	Type EdgeChannelAffinityKeySourceTypeV1 `json:"type"`
	Key  string                             `json:"key,omitempty"`
	Path string                             `json:"path,omitempty"`
}

// EdgeChannelAffinityRuleV1 projects the existing master affinity matcher.
// PassHeaders is the only permitted channel-override operation on edge; the
// arbitrary master ParamOverrideTemplate map is deliberately excluded.
type EdgeChannelAffinityRuleV1 struct {
	Name              string                           `json:"name"`
	ModelRegex        []string                         `json:"model_regex"`
	PathRegex         []string                         `json:"path_regex,omitempty"`
	UserAgentInclude  []string                         `json:"user_agent_include,omitempty"`
	KeySources        []EdgeChannelAffinityKeySourceV1 `json:"key_sources"`
	ValueRegex        string                           `json:"value_regex,omitempty"`
	TTLSeconds        int64                            `json:"ttl_seconds"`
	PassHeaders       []string                         `json:"pass_headers,omitempty"`
	KeepOrigin        bool                             `json:"keep_origin"`
	SkipRetry         bool                             `json:"skip_retry"`
	IncludeUsingGroup bool                             `json:"include_using_group"`
	IncludeModelName  bool                             `json:"include_model_name"`
	IncludeRuleName   bool                             `json:"include_rule_name"`
}

type EdgeChannelAffinityPolicyV1 struct {
	Enabled               bool                        `json:"enabled"`
	SwitchOnSuccess       bool                        `json:"switch_on_success"`
	KeepOnChannelDisabled bool                        `json:"keep_on_channel_disabled"`
	MaxEntries            int                         `json:"max_entries"`
	DefaultTTLSeconds     int64                       `json:"default_ttl_seconds"`
	Rules                 []EdgeChannelAffinityRuleV1 `json:"rules"`
}

// EdgeRoutingPolicyV1 is the single aggregate record in a routing snapshot
// page. Keeping it typed prevents general option maps or channel overrides
// from crossing the control plane.
type EdgeRoutingPolicyV1 struct {
	ChannelAffinity EdgeChannelAffinityPolicyV1 `json:"channel_affinity"`
}

// EdgeSnapshotPagePayloadV1 is a typed union. The Dataset field on the page
// selects the single populated slice and prevents arbitrary configuration JSON
// from crossing the control plane.
type EdgeSnapshotPagePayloadV1 struct {
	Authentication []EdgeTokenAuthRecordV1   `json:"authentication,omitempty"`
	Users          []EdgeUserPolicyV1        `json:"users,omitempty"`
	Groups         []EdgeGroupPolicyV1       `json:"groups,omitempty"`
	Models         []EdgeModelPolicyV1       `json:"models,omitempty"`
	Channels       []EdgeChannelProjectionV1 `json:"channels,omitempty"`
	Pricing        []EdgePricingPolicyV1     `json:"pricing,omitempty"`
	Routing        []EdgeRoutingPolicyV1     `json:"routing,omitempty"`
}

type EdgeSnapshotPageResponseV1 struct {
	Meta       EdgeControlResponseMetaV1 `json:"meta"`
	SnapshotID string                    `json:"snapshot_id"`
	Dataset    EdgeSnapshotDatasetV1     `json:"dataset"`
	Revision   int64                     `json:"revision"`
	Cursor     string                    `json:"cursor,omitempty"`
	NextCursor string                    `json:"next_cursor,omitempty"`
	ItemCount  int                       `json:"item_count"`
	Digest     string                    `json:"digest"`
	Payload    EdgeSnapshotPagePayloadV1 `json:"payload"`
}

type EdgeLeaseSubjectV1 struct {
	UserID  int64 `json:"user_id"`
	TokenID int64 `json:"token_id"`
}

type EdgeLeaseAcquireRequestV1 struct {
	Meta                   EdgeControlRequestMetaV1 `json:"meta"`
	Subject                EdgeLeaseSubjectV1       `json:"subject"`
	RequestedQuota         int64                    `json:"requested_quota"`
	MinimumAcceptableQuota int64                    `json:"minimum_acceptable_quota"`
	ExistingLeaseID        string                   `json:"existing_lease_id,omitempty"`
	SnapshotID             string                   `json:"snapshot_id"`
	SnapshotRevision       int64                    `json:"snapshot_revision"`
}

type EdgeQuotaLeaseV1 struct {
	LeaseID                  string             `json:"lease_id"`
	Version                  int64              `json:"version"`
	Status                   EdgeLeaseStatusV1  `json:"status"`
	NodeID                   string             `json:"node_id"`
	NodeGeneration           int64              `json:"node_generation"`
	Subject                  EdgeLeaseSubjectV1 `json:"subject"`
	GrantedQuota             int64              `json:"granted_quota"`
	RenewAfterRemainingQuota int64              `json:"renew_after_remaining_quota"`
	IssuedAtUnixMilli        int64              `json:"issued_at_unix_milli"`
	ExpiresAtUnixMilli       int64              `json:"expires_at_unix_milli"`
	SnapshotID               string             `json:"snapshot_id"`
	SnapshotRevision         int64              `json:"snapshot_revision"`
	PricingRevision          int64              `json:"pricing_revision"`
}

type EdgeLeaseAcquireResponseV1 struct {
	Meta  EdgeControlResponseMetaV1 `json:"meta"`
	Lease EdgeQuotaLeaseV1          `json:"lease"`
}

// EdgeLeaseCloseRequestV1 declares only the edge's final durable event
// watermark. Unused quota is intentionally absent and is computed by master
// after all events through FinalEventSequence have been accepted.
type EdgeLeaseCloseRequestV1 struct {
	Meta               EdgeControlRequestMetaV1 `json:"meta"`
	LeaseID            string                   `json:"lease_id"`
	LeaseVersion       int64                    `json:"lease_version"`
	FinalEventSequence int64                    `json:"final_event_sequence"`
}

type EdgeLeaseCloseResponseV1 struct {
	Meta                    EdgeControlResponseMetaV1 `json:"meta"`
	LeaseID                 string                    `json:"lease_id"`
	LeaseVersion            int64                     `json:"lease_version"`
	Status                  EdgeLeaseStatusV1         `json:"status"`
	GrantedQuota            int64                     `json:"granted_quota"`
	AcceptedQuota           int64                     `json:"accepted_quota"`
	ReturnedQuota           int64                     `json:"returned_quota"`
	CloseAfterEventSequence int64                     `json:"close_after_event_sequence"`
}

type EdgeUsageBillingV1 struct {
	PricingPolicyID       string             `json:"pricing_policy_id"`
	PricingPolicyVersion  string             `json:"pricing_policy_version"`
	BillingMode           EdgeBillingModeV1  `json:"billing_mode"`
	GroupRatio            float64            `json:"group_ratio"`
	AppliedRatios         map[string]float64 `json:"applied_ratios,omitempty"`
	BillingExpressionHash string             `json:"billing_expression_hash,omitempty"`
	MatchedTier           string             `json:"matched_tier,omitempty"`
	ReservedQuota         int64              `json:"reserved_quota"`
	ChargedQuota          int64              `json:"charged_quota"`
}

// EdgeUsageEventV1 contains accounting facts only. It carries no request body,
// plaintext user token, upstream credential or response content.
type EdgeUsageEventV1 struct {
	EventID             string             `json:"event_id"`
	Sequence            int64              `json:"sequence"`
	LeaseID             string             `json:"lease_id"`
	ReservationID       string             `json:"reservation_id"`
	RequestID           string             `json:"request_id"`
	UserID              int64              `json:"user_id"`
	TokenID             int64              `json:"token_id"`
	ChannelID           int64              `json:"channel_id"`
	Endpoint            EdgeEndpointV1     `json:"endpoint"`
	Streaming           bool               `json:"streaming"`
	Model               string             `json:"model"`
	Group               string             `json:"group"`
	StartedAtUnixMilli  int64              `json:"started_at_unix_milli"`
	FinishedAtUnixMilli int64              `json:"finished_at_unix_milli"`
	Outcome             EdgeUsageOutcomeV1 `json:"outcome"`
	HTTPStatus          *int               `json:"http_status,omitempty"`
	ErrorCode           string             `json:"error_code,omitempty"`
	Usage               *BillingUsage      `json:"usage,omitempty"`
	Billing             EdgeUsageBillingV1 `json:"billing"`
}

type EdgeSettlementBlockRequestV1 struct {
	Meta                EdgeControlRequestMetaV1 `json:"meta"`
	BlockID             string                   `json:"block_id"`
	PreviousBlockID     string                   `json:"previous_block_id,omitempty"`
	PreviousBlockDigest string                   `json:"previous_block_digest,omitempty"`
	FirstSequence       int64                    `json:"first_sequence"`
	LastSequence        int64                    `json:"last_sequence"`
	CreatedAtUnixMilli  int64                    `json:"created_at_unix_milli"`
	BlockDigest         string                   `json:"block_digest"`
	Events              []EdgeUsageEventV1       `json:"events"`
}

type EdgeSettlementAckV1 struct {
	Status                  EdgeSettlementAckStatusV1 `json:"status"`
	NodeID                  string                    `json:"node_id"`
	NodeGeneration          int64                     `json:"node_generation"`
	BlockID                 string                    `json:"block_id"`
	AckedThroughSequence    int64                     `json:"acked_through_sequence"`
	NextExpectedSequence    int64                     `json:"next_expected_sequence"`
	AcceptedEventCount      int                       `json:"accepted_event_count"`
	AcknowledgedAtUnixMilli int64                     `json:"acknowledged_at_unix_milli"`
}

type EdgeSettlementBlockResponseV1 struct {
	Meta EdgeControlResponseMetaV1 `json:"meta"`
	Ack  EdgeSettlementAckV1       `json:"ack"`
}

type EdgeLeaseRuntimeStateV1 struct {
	LeaseID            string            `json:"lease_id"`
	Version            int64             `json:"version"`
	Status             EdgeLeaseStatusV1 `json:"status"`
	RemainingQuota     int64             `json:"remaining_quota"`
	ReservedQuota      int64             `json:"reserved_quota"`
	ConsumedQuota      int64             `json:"consumed_quota"`
	ExpiresAtUnixMilli int64             `json:"expires_at_unix_milli"`
}

type EdgeRuntimeStatusV1 struct {
	UptimeSeconds      int64 `json:"uptime_seconds"`
	InFlightRequests   int64 `json:"in_flight_requests"`
	RecentRequestCount int64 `json:"recent_request_count"`
	RecentErrorCount   int64 `json:"recent_error_count"`
	Draining           bool  `json:"draining"`
}

// EdgeCPAStatusV1 reports a logical local service and observations only. It
// does not disclose the service address or any OAuth credential.
// Deprecated: the edge no longer probes local services; heartbeats send an
// empty list. The type and the heartbeat field remain for wire and database
// compatibility only.
type EdgeCPAStatusV1 struct {
	LocalService        EdgeLocalServiceV1 `json:"local_service"`
	Healthy             bool               `json:"healthy"`
	LatencyMilliseconds int64              `json:"latency_milliseconds"`
	AvailableModels     []string           `json:"available_models,omitempty"`
	CheckedAtUnixMilli  int64              `json:"checked_at_unix_milli"`
}

type EdgeHeartbeatRequestV1 struct {
	Meta        EdgeControlRequestMetaV1  `json:"meta"`
	Declaration EdgeNodeDeclarationV1     `json:"declaration"`
	Snapshot    EdgeSnapshotStateV1       `json:"snapshot"`
	Settlement  EdgeSettlementStateV1     `json:"settlement"`
	Leases      []EdgeLeaseRuntimeStateV1 `json:"leases"`
	Runtime     EdgeRuntimeStatusV1       `json:"runtime"`
	CPA         []EdgeCPAStatusV1         `json:"cpa"`
}

type EdgeHeartbeatResponseV1 struct {
	Meta          EdgeControlResponseMetaV1 `json:"meta"`
	Control       EdgeNodeControlConfigV1   `json:"control"`
	Snapshot      *EdgeSnapshotManifestV1   `json:"snapshot,omitempty"`
	SettlementAck *EdgeSettlementAckV1      `json:"settlement_ack,omitempty"`
}

type EdgeControlExpectedStateV1 struct {
	ProtocolVersions       []string `json:"protocol_versions,omitempty"`
	NodeGeneration         *int64   `json:"node_generation,omitempty"`
	SnapshotID             string   `json:"snapshot_id,omitempty"`
	SnapshotRevision       *int64   `json:"snapshot_revision,omitempty"`
	NextSettlementSequence *int64   `json:"next_settlement_sequence,omitempty"`
}

type EdgeControlErrorV1 struct {
	Code              EdgeControlErrorCodeV1      `json:"code"`
	Message           string                      `json:"message"`
	Retryable         bool                        `json:"retryable"`
	RetryAfterSeconds *int64                      `json:"retry_after_seconds,omitempty"`
	Expected          *EdgeControlExpectedStateV1 `json:"expected,omitempty"`
}

type EdgeControlErrorResponseV1 struct {
	Meta  EdgeControlResponseMetaV1 `json:"meta"`
	Error EdgeControlErrorV1        `json:"error"`
}

func (e EdgeEndpointV1) Valid() bool {
	switch e {
	case EdgeEndpointOpenAIChatCompletionsV1, EdgeEndpointOpenAIResponsesV1:
		return true
	default:
		return false
	}
}

func (d EdgeSnapshotDatasetV1) Valid() bool {
	switch d {
	case EdgeSnapshotDatasetAuthenticationV1,
		EdgeSnapshotDatasetUsersV1,
		EdgeSnapshotDatasetGroupsV1,
		EdgeSnapshotDatasetModelsV1,
		EdgeSnapshotDatasetChannelsV1,
		EdgeSnapshotDatasetPricingV1,
		EdgeSnapshotDatasetRoutingV1:
		return true
	default:
		return false
	}
}

func (t EdgeChannelAffinityKeySourceTypeV1) Valid() bool {
	switch t {
	case EdgeChannelAffinityKeySourceContextIntV1,
		EdgeChannelAffinityKeySourceContextStringV1,
		EdgeChannelAffinityKeySourceRequestHeaderV1,
		EdgeChannelAffinityKeySourceGJSONV1:
		return true
	default:
		return false
	}
}

func (s EdgeLeaseStatusV1) Valid() bool {
	switch s {
	case EdgeLeaseStatusActiveV1,
		EdgeLeaseStatusClosingV1,
		EdgeLeaseStatusClosedV1,
		EdgeLeaseStatusRevokedV1,
		EdgeLeaseStatusForceClosedV1:
		return true
	default:
		return false
	}
}

func (m EdgeBillingModeV1) Valid() bool {
	switch m {
	case EdgeBillingModeRatioV1, EdgeBillingModeFixedPriceV1, EdgeBillingModeTieredExprV1:
		return true
	default:
		return false
	}
}

func (o EdgeUsageOutcomeV1) Valid() bool {
	switch o {
	case EdgeUsageOutcomeSuccessV1,
		EdgeUsageOutcomeUpstreamErrorV1,
		EdgeUsageOutcomeClientCancelV1,
		EdgeUsageOutcomeRejectedV1:
		return true
	default:
		return false
	}
}

func (s EdgeSettlementAckStatusV1) Valid() bool {
	switch s {
	case EdgeSettlementAckAcceptedV1, EdgeSettlementAckDuplicateV1:
		return true
	default:
		return false
	}
}

func (s EdgeLocalServiceV1) Valid() bool {
	value := string(s)
	if len(value) == 0 || len(value) > EdgeControlMaxIdentifierLengthV1 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if index > 0 && index < len(value)-1 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return false
	}
	return true
}

func (m EdgeControlRequestMetaV1) Validate() error {
	if m.ProtocolVersion != EdgeControlProtocolVersionV1 {
		return fmt.Errorf("protocol_version must be %q", EdgeControlProtocolVersionV1)
	}
	return validateEdgeControlIdentifierV1("request_id", m.RequestID)
}

func (m EdgeControlResponseMetaV1) Validate() error {
	if m.ProtocolVersion != EdgeControlProtocolVersionV1 {
		return fmt.Errorf("protocol_version must be %q", EdgeControlProtocolVersionV1)
	}
	if m.RequestID != "" {
		if err := validateEdgeControlIdentifierV1("request_id", m.RequestID); err != nil {
			return err
		}
	}
	if err := validateEdgeControlIdentifierV1("server_request_id", m.ServerRequestID); err != nil {
		return err
	}
	return validateEdgeControlUnixMilliV1("server_time_unix_milli", m.ServerTimeUnixMilli, false)
}

func (s EdgeTokenFingerprintSchemeV1) Validate() error {
	if s.Algorithm != edgetoken.FingerprintAlgorithm {
		return fmt.Errorf("token_fingerprint.algorithm must be %q", edgetoken.FingerprintAlgorithm)
	}
	if s.Version != edgetoken.FingerprintVersion {
		return fmt.Errorf("token_fingerprint.version must be %d", edgetoken.FingerprintVersion)
	}
	if s.KeyID != "" {
		return fmt.Errorf("token_fingerprint.key_id must be empty for unkeyed SHA-256")
	}
	return nil
}

func (k EdgeSnapshotVerificationKeyV1) Validate() error {
	if err := validateEdgeControlIdentifierV1("snapshot_verification_key.key_id", k.KeyID); err != nil {
		return err
	}
	if k.Algorithm != edgeauth.Algorithm {
		return fmt.Errorf("snapshot_verification_key.algorithm must be %q", edgeauth.Algorithm)
	}
	if _, err := edgeauth.ParsePublicKey(k.PublicKey); err != nil {
		return fmt.Errorf("snapshot_verification_key.public_key is invalid: %w", err)
	}
	if err := validateEdgeControlUnixMilliV1("snapshot_verification_key.not_before_unix_milli", k.NotBeforeUnixMilli, false); err != nil {
		return err
	}
	if err := validateEdgeControlUnixMilliV1("snapshot_verification_key.expires_at_unix_milli", k.ExpiresAtUnixMilli, false); err != nil {
		return err
	}
	if k.ExpiresAtUnixMilli <= k.NotBeforeUnixMilli {
		return fmt.Errorf("snapshot_verification_key.expires_at_unix_milli must be after not_before_unix_milli")
	}
	return nil
}

func (c EdgeNodeControlConfigV1) Validate() error {
	if err := validateEdgeControlIdentifierV1("control.node_id", c.NodeID); err != nil {
		return err
	}
	if c.NodeGeneration <= 0 {
		return fmt.Errorf("control.node_generation must be greater than zero")
	}
	if err := validateEdgeControlPositiveLimitV1("control.heartbeat_interval_seconds", c.HeartbeatIntervalSeconds, EdgeControlMaxHeartbeatIntervalSecondsV1); err != nil {
		return err
	}
	if err := validateEdgeControlPositiveLimitV1("control.snapshot_poll_interval_seconds", c.SnapshotPollIntervalSeconds, EdgeControlMaxSnapshotPollIntervalSecondsV1); err != nil {
		return err
	}
	if err := validateEdgeControlPositiveLimitV1("control.snapshot_page_limit", int64(c.SnapshotPageLimit), int64(EdgeControlMaxSnapshotPageLimitV1)); err != nil {
		return err
	}
	if err := validateEdgeControlPositiveLimitV1("control.settlement_max_events", int64(c.SettlementMaxEvents), int64(EdgeControlMaxSettlementEventsV1)); err != nil {
		return err
	}
	if err := validateEdgeControlPositiveLimitV1("control.settlement_max_delay_seconds", c.SettlementMaxDelaySeconds, EdgeControlMaxSettlementDelaySecondsV1); err != nil {
		return err
	}
	if err := validateEdgeControlPositiveLimitV1("control.clock_skew_tolerance_seconds", c.ClockSkewToleranceSeconds, EdgeControlMaxClockSkewToleranceSecondsV1); err != nil {
		return err
	}
	if len(c.SnapshotVerificationKeys) == 0 {
		return fmt.Errorf("control.snapshot_verification_keys must not be empty")
	}
	if len(c.SnapshotVerificationKeys) > EdgeControlMaxSnapshotVerificationKeysV1 {
		return fmt.Errorf("control.snapshot_verification_keys exceeds %d items", EdgeControlMaxSnapshotVerificationKeysV1)
	}
	seenKeyIDs := make(map[string]struct{}, len(c.SnapshotVerificationKeys))
	seenPublicKeys := make(map[string]struct{}, len(c.SnapshotVerificationKeys))
	for i, key := range c.SnapshotVerificationKeys {
		if err := key.Validate(); err != nil {
			return fmt.Errorf("control.snapshot_verification_keys[%d]: %w", i, err)
		}
		if _, exists := seenKeyIDs[key.KeyID]; exists {
			return fmt.Errorf("control.snapshot_verification_keys contains duplicate key_id %q", key.KeyID)
		}
		seenKeyIDs[key.KeyID] = struct{}{}
		if _, exists := seenPublicKeys[key.PublicKey]; exists {
			return fmt.Errorf("control.snapshot_verification_keys contains duplicate public_key")
		}
		seenPublicKeys[key.PublicKey] = struct{}{}
	}
	return nil
}

func (d EdgeNodeDeclarationV1) Validate() error {
	if err := validateEdgeControlTextV1("declaration.name", d.Name, EdgeControlMaxNodeNameLengthV1, false); err != nil {
		return err
	}
	if d.Region != "" {
		if len(d.Region) > EdgeControlMaxRegionLengthV1 {
			return fmt.Errorf("declaration.region exceeds %d bytes", EdgeControlMaxRegionLengthV1)
		}
		if err := validateEdgeControlIdentifierV1("declaration.region", d.Region); err != nil {
			return err
		}
	}
	if err := validateEdgeControlPublicURLV1(d.PublicURL); err != nil {
		return err
	}
	if err := validateEdgeControlTextV1("declaration.software_version", d.SoftwareVersion, EdgeControlMaxSoftwareVersionLengthV1, false); err != nil {
		return err
	}
	if err := validateEdgeControlUnixMilliV1("declaration.started_at_unix_milli", d.StartedAtUnixMilli, false); err != nil {
		return err
	}
	if len(d.Capabilities) == 0 {
		return fmt.Errorf("declaration.capabilities must not be empty")
	}
	if len(d.Capabilities) > EdgeControlMaxCapabilitiesV1 {
		return fmt.Errorf("declaration.capabilities exceeds %d items", EdgeControlMaxCapabilitiesV1)
	}
	seen := make(map[EdgeEndpointV1]struct{}, len(d.Capabilities))
	for i, capability := range d.Capabilities {
		if !capability.Endpoint.Valid() {
			return fmt.Errorf("declaration.capabilities[%d].endpoint is invalid", i)
		}
		if _, exists := seen[capability.Endpoint]; exists {
			return fmt.Errorf("declaration.capabilities contains duplicate endpoint %q", capability.Endpoint)
		}
		seen[capability.Endpoint] = struct{}{}
	}
	return nil
}

func (s EdgeSnapshotStateV1) Validate() error {
	if s.SnapshotID != "" {
		if err := validateEdgeControlIdentifierV1("snapshot.snapshot_id", s.SnapshotID); err != nil {
			return err
		}
	}
	if s.Revision < 0 {
		return fmt.Errorf("snapshot.revision must not be negative")
	}
	if err := validateEdgeControlUnixMilliV1("snapshot.applied_at_unix_milli", s.AppliedAtUnixMilli, true); err != nil {
		return err
	}
	if len(s.Datasets) > EdgeControlMaxSnapshotDatasetsV1 {
		return fmt.Errorf("snapshot.datasets exceeds %d items", EdgeControlMaxSnapshotDatasetsV1)
	}
	seen := make(map[EdgeSnapshotDatasetV1]struct{}, len(s.Datasets))
	for i, dataset := range s.Datasets {
		if !dataset.Dataset.Valid() {
			return fmt.Errorf("snapshot.datasets[%d].dataset is invalid", i)
		}
		if dataset.Revision < 0 {
			return fmt.Errorf("snapshot.datasets[%d].revision must not be negative", i)
		}
		if _, exists := seen[dataset.Dataset]; exists {
			return fmt.Errorf("snapshot.datasets contains duplicate dataset %q", dataset.Dataset)
		}
		seen[dataset.Dataset] = struct{}{}
	}
	return nil
}

func (s EdgeSettlementStateV1) Validate() error {
	if s.LastAckedSequence < 0 {
		return fmt.Errorf("settlement.last_acked_sequence must not be negative")
	}
	if s.LastAckedBlockID != "" {
		if err := validateEdgeControlIdentifierV1("settlement.last_acked_block_id", s.LastAckedBlockID); err != nil {
			return err
		}
	}
	if s.NextEventSequence < 0 {
		return fmt.Errorf("settlement.next_event_sequence must not be negative")
	}
	if s.PendingEventCount < 0 {
		return fmt.Errorf("settlement.pending_event_count must not be negative")
	}
	if s.PendingBlockCount < 0 {
		return fmt.Errorf("settlement.pending_block_count must not be negative")
	}
	return validateEdgeControlUnixMilliV1("settlement.oldest_pending_unix_milli", s.OldestPendingUnixMilli, true)
}

func (r EdgeBootstrapRequestV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if len(r.SupportedProtocolVersions) == 0 {
		return fmt.Errorf("supported_protocol_versions must not be empty")
	}
	if len(r.SupportedProtocolVersions) > EdgeControlMaxSupportedProtocolVersionsV1 {
		return fmt.Errorf("supported_protocol_versions exceeds %d items", EdgeControlMaxSupportedProtocolVersionsV1)
	}
	containsV1 := false
	seen := make(map[string]struct{}, len(r.SupportedProtocolVersions))
	for i, version := range r.SupportedProtocolVersions {
		if err := validateEdgeControlIdentifierV1(fmt.Sprintf("supported_protocol_versions[%d]", i), version); err != nil {
			return err
		}
		if _, exists := seen[version]; exists {
			return fmt.Errorf("supported_protocol_versions contains duplicate version %q", version)
		}
		seen[version] = struct{}{}
		containsV1 = containsV1 || version == EdgeControlProtocolVersionV1
	}
	if !containsV1 {
		return fmt.Errorf("supported_protocol_versions must contain %q", EdgeControlProtocolVersionV1)
	}
	if err := r.Declaration.Validate(); err != nil {
		return err
	}
	if err := r.Snapshot.Validate(); err != nil {
		return err
	}
	return r.Settlement.Validate()
}

func (l EdgeLeaseRuntimeStateV1) Validate() error {
	if err := validateEdgeControlIdentifierV1("lease.lease_id", l.LeaseID); err != nil {
		return err
	}
	if l.Version <= 0 {
		return fmt.Errorf("lease.version must be greater than zero")
	}
	if !l.Status.Valid() {
		return fmt.Errorf("lease.status is invalid")
	}
	if err := validateEdgeControlQuotaV1("lease.remaining_quota", l.RemainingQuota); err != nil {
		return err
	}
	if err := validateEdgeControlQuotaV1("lease.reserved_quota", l.ReservedQuota); err != nil {
		return err
	}
	if err := validateEdgeControlQuotaV1("lease.consumed_quota", l.ConsumedQuota); err != nil {
		return err
	}
	return validateEdgeControlUnixMilliV1("lease.expires_at_unix_milli", l.ExpiresAtUnixMilli, false)
}

func (r EdgeRuntimeStatusV1) Validate() error {
	if r.UptimeSeconds < 0 {
		return fmt.Errorf("runtime.uptime_seconds must not be negative")
	}
	if r.InFlightRequests < 0 {
		return fmt.Errorf("runtime.in_flight_requests must not be negative")
	}
	if r.RecentRequestCount < 0 {
		return fmt.Errorf("runtime.recent_request_count must not be negative")
	}
	if r.RecentErrorCount < 0 {
		return fmt.Errorf("runtime.recent_error_count must not be negative")
	}
	return nil
}

func (c EdgeCPAStatusV1) Validate() error {
	if !c.LocalService.Valid() {
		return fmt.Errorf("cpa.local_service is invalid")
	}
	if c.LatencyMilliseconds < 0 {
		return fmt.Errorf("cpa.latency_milliseconds must not be negative")
	}
	if err := validateEdgeControlModelListV1("cpa.available_models", c.AvailableModels, EdgeControlMaxAvailableModelsV1); err != nil {
		return err
	}
	return validateEdgeControlUnixMilliV1("cpa.checked_at_unix_milli", c.CheckedAtUnixMilli, false)
}

func (r EdgeHeartbeatRequestV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if err := r.Declaration.Validate(); err != nil {
		return err
	}
	if err := r.Snapshot.Validate(); err != nil {
		return err
	}
	if err := r.Settlement.Validate(); err != nil {
		return err
	}
	if len(r.Leases) > EdgeControlMaxHeartbeatLeasesV1 {
		return fmt.Errorf("leases exceeds %d items", EdgeControlMaxHeartbeatLeasesV1)
	}
	seenLeases := make(map[string]struct{}, len(r.Leases))
	for i, lease := range r.Leases {
		if err := lease.Validate(); err != nil {
			return fmt.Errorf("leases[%d]: %w", i, err)
		}
		if _, exists := seenLeases[lease.LeaseID]; exists {
			return fmt.Errorf("leases contains duplicate lease_id %q", lease.LeaseID)
		}
		seenLeases[lease.LeaseID] = struct{}{}
	}
	if err := r.Runtime.Validate(); err != nil {
		return err
	}
	if len(r.CPA) > EdgeControlMaxHeartbeatCPAStatusesV1 {
		return fmt.Errorf("cpa exceeds %d items", EdgeControlMaxHeartbeatCPAStatusesV1)
	}
	seenCPA := make(map[EdgeLocalServiceV1]struct{}, len(r.CPA))
	for i, status := range r.CPA {
		if err := status.Validate(); err != nil {
			return fmt.Errorf("cpa[%d]: %w", i, err)
		}
		if _, exists := seenCPA[status.LocalService]; exists {
			return fmt.Errorf("cpa contains duplicate local_service %q", status.LocalService)
		}
		seenCPA[status.LocalService] = struct{}{}
	}
	return nil
}

func (c EdgeChannelProjectionV1) Validate() error {
	if c.ChannelID <= 0 {
		return fmt.Errorf("channel.channel_id must be greater than zero")
	}
	if err := validateEdgeControlTextV1("channel.name", c.Name, EdgeControlMaxNodeNameLengthV1, false); err != nil {
		return err
	}
	if !c.LocalService.Valid() {
		return fmt.Errorf("channel.local_service is invalid")
	}
	if c.Weight < 0 {
		return fmt.Errorf("channel.weight must not be negative")
	}
	if len(c.Groups) > EdgeControlMaxChannelGroupsV1 {
		return fmt.Errorf("channel.groups exceeds %d items", EdgeControlMaxChannelGroupsV1)
	}
	seenGroups := make(map[string]struct{}, len(c.Groups))
	for i, group := range c.Groups {
		if err := validateEdgeControlTextV1(fmt.Sprintf("channel.groups[%d]", i), group, EdgeControlMaxIdentifierLengthV1, false); err != nil {
			return err
		}
		if _, exists := seenGroups[group]; exists {
			return fmt.Errorf("channel.groups contains duplicate value %q", group)
		}
		seenGroups[group] = struct{}{}
	}
	if err := validateEdgeControlModelListV1("channel.models", c.Models, EdgeControlMaxChannelModelsV1); err != nil {
		return err
	}
	if len(c.ModelMapping) > EdgeControlMaxChannelModelsV1 {
		return fmt.Errorf("channel.model_mapping exceeds %d items", EdgeControlMaxChannelModelsV1)
	}
	for source, target := range c.ModelMapping {
		if err := validateEdgeControlModelV1("channel.model_mapping source", source); err != nil {
			return err
		}
		if err := validateEdgeControlModelV1("channel.model_mapping target", target); err != nil {
			return err
		}
	}
	return nil
}

func (m EdgeModelPolicyV1) Validate() error {
	if err := validateEdgeControlModelV1("model.model", m.Model); err != nil {
		return err
	}
	if len(m.Endpoints) == 0 {
		return fmt.Errorf("model.endpoints must not be empty")
	}
	if len(m.Endpoints) > 2 {
		return fmt.Errorf("model.endpoints exceeds 2 items")
	}
	seenEndpoints := make(map[EdgeEndpointV1]struct{}, len(m.Endpoints))
	for i, endpoint := range m.Endpoints {
		if !endpoint.Valid() {
			return fmt.Errorf("model.endpoints[%d] is invalid", i)
		}
		if _, exists := seenEndpoints[endpoint]; exists {
			return fmt.Errorf("model.endpoints contains duplicate endpoint %q", endpoint)
		}
		seenEndpoints[endpoint] = struct{}{}
	}
	if len(m.ChannelIDs) > EdgeControlMaxChannelModelsV1 {
		return fmt.Errorf("model.channel_ids exceeds %d items", EdgeControlMaxChannelModelsV1)
	}
	seenChannels := make(map[int64]struct{}, len(m.ChannelIDs))
	for i, channelID := range m.ChannelIDs {
		if channelID <= 0 {
			return fmt.Errorf("model.channel_ids[%d] must be greater than zero", i)
		}
		if _, exists := seenChannels[channelID]; exists {
			return fmt.Errorf("model.channel_ids contains duplicate channel_id %d", channelID)
		}
		seenChannels[channelID] = struct{}{}
	}
	return nil
}

func (s EdgeDetachedContentSignatureV1) Validate() error {
	if s.Algorithm != edgeauth.Algorithm {
		return fmt.Errorf("detached_signature.algorithm must be %q", edgeauth.Algorithm)
	}
	if err := validateEdgeControlIdentifierV1("detached_signature.key_id", s.KeyID); err != nil {
		return err
	}
	if err := validateEdgeControlSHA256V1("detached_signature.payload_digest", s.PayloadDigest); err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(s.Value)
	if err != nil || len(decoded) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(decoded) != s.Value {
		return fmt.Errorf("detached_signature.value must be a canonical standard-base64 Ed25519 signature")
	}
	return nil
}

func (m EdgeSnapshotDatasetManifestV1) Validate() error {
	if !m.Dataset.Valid() {
		return fmt.Errorf("dataset_manifest.dataset is invalid")
	}
	if m.Revision <= 0 {
		return fmt.Errorf("dataset_manifest.revision must be greater than zero")
	}
	if m.ItemCount < 0 || m.ItemCount > EdgeControlMaxSnapshotItemsV1 {
		return fmt.Errorf("dataset_manifest.item_count must be between 0 and %d", EdgeControlMaxSnapshotItemsV1)
	}
	if m.PageCount < 0 || m.PageCount > EdgeControlMaxSnapshotPagesV1 {
		return fmt.Errorf("dataset_manifest.page_count must be between 0 and %d", EdgeControlMaxSnapshotPagesV1)
	}
	if m.ItemCount == 0 && m.PageCount != 0 {
		return fmt.Errorf("dataset_manifest.page_count must be zero when item_count is zero")
	}
	if m.ItemCount > 0 && (m.PageCount == 0 || int64(m.PageCount) > m.ItemCount) {
		return fmt.Errorf("dataset_manifest.page_count must be between 1 and item_count")
	}
	if err := validateEdgeControlSHA256V1("dataset_manifest.digest", m.Digest); err != nil {
		return err
	}
	if err := m.DetachedSignature.Validate(); err != nil {
		return err
	}
	if m.DetachedSignature.PayloadDigest != m.Digest {
		return fmt.Errorf("detached_signature.payload_digest must equal dataset_manifest.digest")
	}
	return nil
}

func (m EdgeSnapshotManifestV1) Validate() error {
	if err := validateEdgeControlIdentifierV1("snapshot_manifest.snapshot_id", m.SnapshotID); err != nil {
		return err
	}
	if m.Revision <= 0 {
		return fmt.Errorf("snapshot_manifest.revision must be greater than zero")
	}
	if err := validateEdgeControlUnixMilliV1("snapshot_manifest.created_at_unix_milli", m.CreatedAtUnixMilli, false); err != nil {
		return err
	}
	if err := validateEdgeControlUnixMilliV1("snapshot_manifest.expires_at_unix_milli", m.ExpiresAtUnixMilli, false); err != nil {
		return err
	}
	if m.ExpiresAtUnixMilli <= m.CreatedAtUnixMilli {
		return fmt.Errorf("snapshot_manifest.expires_at_unix_milli must be after created_at_unix_milli")
	}
	if m.HashAlgorithm != edgetoken.FingerprintAlgorithm {
		return fmt.Errorf("snapshot_manifest.hash_algorithm must be %q", edgetoken.FingerprintAlgorithm)
	}
	if err := validateEdgeControlSHA256V1("snapshot_manifest.digest", m.Digest); err != nil {
		return err
	}
	if err := m.TokenFingerprint.Validate(); err != nil {
		return err
	}
	if len(m.Datasets) == 0 || len(m.Datasets) > EdgeControlMaxSnapshotDatasetsV1 {
		return fmt.Errorf("snapshot_manifest.datasets length must be between 1 and %d", EdgeControlMaxSnapshotDatasetsV1)
	}
	seen := make(map[EdgeSnapshotDatasetV1]struct{}, len(m.Datasets))
	previousRank := -1
	for i, dataset := range m.Datasets {
		if err := dataset.Validate(); err != nil {
			return fmt.Errorf("snapshot_manifest.datasets[%d]: %w", i, err)
		}
		if dataset.Revision > m.Revision {
			return fmt.Errorf("snapshot_manifest.datasets[%d].revision must not exceed snapshot revision", i)
		}
		if _, exists := seen[dataset.Dataset]; exists {
			return fmt.Errorf("snapshot_manifest.datasets contains duplicate dataset %q", dataset.Dataset)
		}
		seen[dataset.Dataset] = struct{}{}
		rank := edgeSnapshotDatasetRankV1(dataset.Dataset)
		if rank <= previousRank {
			return fmt.Errorf("snapshot_manifest.datasets must use canonical dataset order")
		}
		previousRank = rank
	}
	return nil
}

func (r EdgeSnapshotManifestRequestV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	return r.Current.Validate()
}

func (r EdgeSnapshotManifestResponseV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if r.Changed != (r.Snapshot != nil) {
		return fmt.Errorf("snapshot must be present exactly when changed is true")
	}
	if r.Snapshot == nil {
		return nil
	}
	return r.Snapshot.Validate()
}

func (r EdgeSnapshotPageRequestV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if err := validateEdgeControlIdentifierV1("snapshot_page.snapshot_id", r.SnapshotID); err != nil {
		return err
	}
	if !r.Dataset.Valid() {
		return fmt.Errorf("snapshot_page.dataset is invalid")
	}
	if r.Cursor != "" {
		if err := validateEdgeControlIdentifierV1("snapshot_page.cursor", r.Cursor); err != nil {
			return err
		}
	}
	return validateEdgeControlPositiveLimitV1("snapshot_page.limit", int64(r.Limit), int64(EdgeControlMaxSnapshotPageLimitV1))
}

func (p EdgeSnapshotPagePayloadV1) Validate(dataset EdgeSnapshotDatasetV1, itemCount int) error {
	if !dataset.Valid() {
		return fmt.Errorf("snapshot_page.dataset is invalid")
	}
	if itemCount <= 0 || itemCount > EdgeControlMaxSnapshotPageLimitV1 {
		return fmt.Errorf("snapshot_page.item_count must be between 1 and %d", EdgeControlMaxSnapshotPageLimitV1)
	}
	populated := 0
	for _, length := range []int{len(p.Authentication), len(p.Users), len(p.Groups), len(p.Models), len(p.Channels), len(p.Pricing), len(p.Routing)} {
		if length > 0 {
			populated++
		}
	}
	if populated != 1 {
		return fmt.Errorf("snapshot_page.payload must populate exactly one dataset field")
	}

	var actual int
	switch dataset {
	case EdgeSnapshotDatasetAuthenticationV1:
		actual = len(p.Authentication)
		previous := ""
		for i, record := range p.Authentication {
			if err := validateEdgeTokenAuthRecordV1(record); err != nil {
				return fmt.Errorf("snapshot_page.payload.authentication[%d]: %w", i, err)
			}
			if i > 0 && record.TokenFingerprint <= previous {
				return fmt.Errorf("snapshot_page.payload.authentication must be unique and ordered by token_fingerprint")
			}
			previous = record.TokenFingerprint
		}
	case EdgeSnapshotDatasetUsersV1:
		actual = len(p.Users)
		previous := int64(0)
		for i, user := range p.Users {
			if err := validateEdgeUserPolicyV1(user); err != nil {
				return fmt.Errorf("snapshot_page.payload.users[%d]: %w", i, err)
			}
			if i > 0 && user.UserID <= previous {
				return fmt.Errorf("snapshot_page.payload.users must be unique and ordered by user_id")
			}
			previous = user.UserID
		}
	case EdgeSnapshotDatasetGroupsV1:
		actual = len(p.Groups)
		previous := ""
		for i, group := range p.Groups {
			if err := validateEdgeGroupPolicyV1(group); err != nil {
				return fmt.Errorf("snapshot_page.payload.groups[%d]: %w", i, err)
			}
			if i > 0 && group.UserGroup <= previous {
				return fmt.Errorf("snapshot_page.payload.groups must be unique and ordered by user_group")
			}
			previous = group.UserGroup
		}
	case EdgeSnapshotDatasetModelsV1:
		actual = len(p.Models)
		previous := ""
		for i, model := range p.Models {
			if err := model.Validate(); err != nil {
				return fmt.Errorf("snapshot_page.payload.models[%d]: %w", i, err)
			}
			if i > 0 && model.Model <= previous {
				return fmt.Errorf("snapshot_page.payload.models must be unique and ordered by model")
			}
			previous = model.Model
		}
	case EdgeSnapshotDatasetChannelsV1:
		actual = len(p.Channels)
		previous := int64(0)
		for i, channel := range p.Channels {
			if err := channel.Validate(); err != nil {
				return fmt.Errorf("snapshot_page.payload.channels[%d]: %w", i, err)
			}
			if i > 0 && channel.ChannelID <= previous {
				return fmt.Errorf("snapshot_page.payload.channels must be unique and ordered by channel_id")
			}
			previous = channel.ChannelID
		}
	case EdgeSnapshotDatasetPricingV1:
		actual = len(p.Pricing)
		previous := ""
		for i, pricing := range p.Pricing {
			if err := pricing.Validate(); err != nil {
				return fmt.Errorf("snapshot_page.payload.pricing[%d]: %w", i, err)
			}
			if i > 0 && pricing.PolicyID <= previous {
				return fmt.Errorf("snapshot_page.payload.pricing must be unique and ordered by policy_id")
			}
			previous = pricing.PolicyID
		}
	case EdgeSnapshotDatasetRoutingV1:
		actual = len(p.Routing)
		if actual != 1 {
			return fmt.Errorf("snapshot_page.payload.routing must contain exactly one policy")
		}
		if err := p.Routing[0].Validate(); err != nil {
			return fmt.Errorf("snapshot_page.payload.routing[0]: %w", err)
		}
	}
	if actual != itemCount {
		return fmt.Errorf("snapshot_page.item_count must equal the selected payload length")
	}
	return nil
}

func (r EdgeSnapshotPageResponseV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if err := validateEdgeControlIdentifierV1("snapshot_page.snapshot_id", r.SnapshotID); err != nil {
		return err
	}
	if !r.Dataset.Valid() {
		return fmt.Errorf("snapshot_page.dataset is invalid")
	}
	if r.Revision <= 0 {
		return fmt.Errorf("snapshot_page.revision must be greater than zero")
	}
	if r.Cursor != "" {
		if err := validateEdgeControlIdentifierV1("snapshot_page.cursor", r.Cursor); err != nil {
			return err
		}
	}
	if r.NextCursor != "" {
		if err := validateEdgeControlIdentifierV1("snapshot_page.next_cursor", r.NextCursor); err != nil {
			return err
		}
		if r.NextCursor == r.Cursor {
			return fmt.Errorf("snapshot_page.next_cursor must differ from cursor")
		}
	}
	if err := validateEdgeControlSHA256V1("snapshot_page.digest", r.Digest); err != nil {
		return err
	}
	return r.Payload.Validate(r.Dataset, r.ItemCount)
}

func (p EdgePricingPolicyV1) Validate() error {
	if err := validateEdgeControlIdentifierV1("pricing.policy_id", p.PolicyID); err != nil {
		return err
	}
	if err := validateEdgeControlIdentifierV1("pricing.version", p.Version); err != nil {
		return err
	}
	if err := validateEdgeControlModelV1("pricing.model", p.Model); err != nil {
		return err
	}
	if !p.BillingMode.Valid() {
		return fmt.Errorf("pricing.billing_mode is invalid")
	}
	for _, value := range []struct {
		name     string
		value    *float64
		positive bool
	}{
		{name: "model_price", value: p.ModelPrice},
		{name: "model_ratio", value: p.ModelRatio},
		{name: "completion_ratio", value: p.CompletionRatio},
		{name: "cache_read_ratio", value: p.CacheReadRatio},
		{name: "cache_creation_ratio", value: p.CacheCreationRatio},
		{name: "cache_creation_1h_ratio", value: p.CacheCreation1hRatio},
	} {
		if value.value == nil {
			continue
		}
		if err := validateEdgeControlFiniteFloatV1("pricing."+value.name, *value.value, value.positive); err != nil {
			return err
		}
	}
	if err := validateEdgeControlFiniteFloatV1("pricing.quota_per_unit", p.QuotaPerUnit, true); err != nil {
		return err
	}

	switch p.BillingMode {
	case EdgeBillingModeRatioV1:
		if p.ModelRatio == nil {
			return fmt.Errorf("pricing.model_ratio is required for ratio billing")
		}
	case EdgeBillingModeFixedPriceV1:
		if p.ModelPrice == nil {
			return fmt.Errorf("pricing.model_price is required for fixed_price billing")
		}
	case EdgeBillingModeTieredExprV1:
		if len(p.BillingExpression) == 0 || len(p.BillingExpression) > EdgeControlMaxBillingExpressionLengthV1 {
			return fmt.Errorf("pricing.billing_expression length must be between 1 and %d bytes", EdgeControlMaxBillingExpressionLengthV1)
		}
		if strings.TrimSpace(p.BillingExpression) == "" {
			return fmt.Errorf("pricing.billing_expression must not be blank")
		}
		if p.BillingExpressionVersion != billingexpr.DefaultExprVersion {
			return fmt.Errorf("pricing.billing_expression_version must be %d", billingexpr.DefaultExprVersion)
		}
		if hasUnsupportedEdgeBillingExpressionVersionV1(p.BillingExpression) {
			return fmt.Errorf("pricing.billing_expression uses an unsupported version prefix")
		}
		if err := validateEdgeControlSHA256V1("pricing.billing_expression_hash", p.BillingExpressionHash); err != nil {
			return err
		}
		if billingexpr.ExprHashString(p.BillingExpression) != p.BillingExpressionHash {
			return fmt.Errorf("pricing.billing_expression_hash must equal the SHA-256 digest of billing_expression")
		}
	}
	if p.BillingMode != EdgeBillingModeTieredExprV1 && (p.BillingExpression != "" || p.BillingExpressionHash != "" || p.BillingExpressionVersion != 0) {
		return fmt.Errorf("pricing billing expression fields are only valid for tiered_expr billing")
	}
	return nil
}

func (s EdgeChannelAffinityKeySourceV1) Validate() error {
	if !s.Type.Valid() {
		return fmt.Errorf("channel_affinity key source type is invalid")
	}
	switch s.Type {
	case EdgeChannelAffinityKeySourceContextIntV1, EdgeChannelAffinityKeySourceContextStringV1:
		if err := validateEdgeControlIdentifierV1("channel_affinity key source key", s.Key); err != nil {
			return err
		}
		if s.Path != "" {
			return fmt.Errorf("channel_affinity context key source path must be empty")
		}
	case EdgeChannelAffinityKeySourceRequestHeaderV1:
		if !validHTTPHeaderTokenV1(s.Key) {
			return fmt.Errorf("channel_affinity request_header key must be an HTTP header token")
		}
		if s.Path != "" {
			return fmt.Errorf("channel_affinity request_header path must be empty")
		}
	case EdgeChannelAffinityKeySourceGJSONV1:
		if s.Key != "" {
			return fmt.Errorf("channel_affinity gjson key must be empty")
		}
		if err := validateEdgeControlTextV1("channel_affinity gjson path", s.Path, EdgeControlMaxAffinitySourcePathLengthV1, false); err != nil {
			return err
		}
	}
	return nil
}

func (r EdgeChannelAffinityRuleV1) Validate() error {
	if err := validateEdgeControlTextV1("channel_affinity rule name", r.Name, EdgeControlMaxNodeNameLengthV1, false); err != nil {
		return err
	}
	if err := validateEdgeAffinityRegexListV1("channel_affinity model_regex", r.ModelRegex, false); err != nil {
		return err
	}
	if err := validateEdgeAffinityRegexListV1("channel_affinity path_regex", r.PathRegex, true); err != nil {
		return err
	}
	if err := validateEdgeAffinityStringListV1("channel_affinity user_agent_include", r.UserAgentInclude, EdgeControlMaxAffinityIncludesV1, EdgeControlMaxAffinityIncludeLengthV1, true); err != nil {
		return err
	}
	if len(r.KeySources) == 0 || len(r.KeySources) > EdgeControlMaxAffinityKeySourcesV1 {
		return fmt.Errorf("channel_affinity key_sources length must be between 1 and %d", EdgeControlMaxAffinityKeySourcesV1)
	}
	seenSources := make(map[string]struct{}, len(r.KeySources))
	for i, source := range r.KeySources {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("channel_affinity key_sources[%d]: %w", i, err)
		}
		identity := string(source.Type) + "\x00" + strings.ToLower(source.Key) + "\x00" + source.Path
		if _, exists := seenSources[identity]; exists {
			return fmt.Errorf("channel_affinity key_sources contains a duplicate source")
		}
		seenSources[identity] = struct{}{}
	}
	if r.ValueRegex != "" {
		if len(r.ValueRegex) > EdgeControlMaxAffinityRegexLengthV1 {
			return fmt.Errorf("channel_affinity value_regex exceeds %d bytes", EdgeControlMaxAffinityRegexLengthV1)
		}
		if _, err := regexp.Compile(r.ValueRegex); err != nil {
			return fmt.Errorf("channel_affinity value_regex is invalid: %w", err)
		}
	}
	if r.TTLSeconds < 0 || r.TTLSeconds > EdgeControlMaxAffinityTTLSecondsV1 {
		return fmt.Errorf("channel_affinity ttl_seconds must be between 0 and %d", EdgeControlMaxAffinityTTLSecondsV1)
	}
	if len(r.PassHeaders) > EdgeControlMaxAffinityPassHeadersV1 {
		return fmt.Errorf("channel_affinity pass_headers exceeds %d items", EdgeControlMaxAffinityPassHeadersV1)
	}
	seenHeaders := make(map[string]struct{}, len(r.PassHeaders))
	for i, header := range r.PassHeaders {
		if !validHTTPHeaderTokenV1(header) {
			return fmt.Errorf("channel_affinity pass_headers[%d] must be an HTTP header token", i)
		}
		canonical := strings.ToLower(header)
		if _, exists := seenHeaders[canonical]; exists {
			return fmt.Errorf("channel_affinity pass_headers contains duplicate header %q", header)
		}
		seenHeaders[canonical] = struct{}{}
	}
	if r.KeepOrigin && len(r.PassHeaders) == 0 {
		return fmt.Errorf("channel_affinity keep_origin requires pass_headers")
	}
	return nil
}

func (p EdgeChannelAffinityPolicyV1) Validate() error {
	if p.MaxEntries <= 0 || p.MaxEntries > EdgeControlMaxAffinityEntriesV1 {
		return fmt.Errorf("channel_affinity max_entries must be between 1 and %d", EdgeControlMaxAffinityEntriesV1)
	}
	if p.DefaultTTLSeconds <= 0 || p.DefaultTTLSeconds > EdgeControlMaxAffinityTTLSecondsV1 {
		return fmt.Errorf("channel_affinity default_ttl_seconds must be between 1 and %d", EdgeControlMaxAffinityTTLSecondsV1)
	}
	if len(p.Rules) > EdgeControlMaxRoutingRulesV1 {
		return fmt.Errorf("channel_affinity rules exceeds %d items", EdgeControlMaxRoutingRulesV1)
	}
	seenRules := make(map[string]struct{}, len(p.Rules))
	for i, rule := range p.Rules {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("channel_affinity rules[%d]: %w", i, err)
		}
		if _, exists := seenRules[rule.Name]; exists {
			return fmt.Errorf("channel_affinity rules contains duplicate name %q", rule.Name)
		}
		seenRules[rule.Name] = struct{}{}
	}
	return nil
}

func (p EdgeRoutingPolicyV1) Validate() error {
	return p.ChannelAffinity.Validate()
}

// EdgeBillingExpressionHasRequestOrTimeDependenciesV1 reports whether a
// syntactically valid billing expression reads request data or wall-clock
// facts. The edge snapshot compiler uses this after normal expression
// compilation and rejects true for the first protocol version, whose replayed
// settlements must not depend on request bodies, headers, or current time.
func EdgeBillingExpressionHasRequestOrTimeDependenciesV1(expression string) bool {
	used := billingexpr.UsedVars(expression)
	for _, dependency := range []string{"param", "header", "hour", "minute", "weekday", "month", "day"} {
		if used[dependency] {
			return true
		}
	}
	if used != nil {
		return false
	}

	// Combined request-rule strings are not necessarily accepted by the core
	// expression compiler as one program. Fall back to a small string-aware
	// lexical scan so an uncompiled rule cannot hide a request/time dependency.
	dependencies := map[string]struct{}{
		"param": {}, "header": {}, "hour": {}, "minute": {},
		"weekday": {}, "month": {}, "day": {},
	}
	for i := 0; i < len(expression); {
		if expression[i] == '"' || expression[i] == '\'' || expression[i] == '`' {
			quote := expression[i]
			i++
			for i < len(expression) {
				if expression[i] == '\\' && quote != '`' {
					i += 2
					continue
				}
				if i < len(expression) && expression[i] == quote {
					i++
					break
				}
				i++
			}
			continue
		}
		if !edgeBillingExpressionIdentifierStartV1(expression[i]) {
			i++
			continue
		}
		start := i
		i++
		for i < len(expression) && edgeBillingExpressionIdentifierPartV1(expression[i]) {
			i++
		}
		name := expression[start:i]
		if _, tracked := dependencies[name]; !tracked {
			continue
		}
		for i < len(expression) && (expression[i] == ' ' || expression[i] == '\t' || expression[i] == '\r' || expression[i] == '\n') {
			i++
		}
		if i < len(expression) && expression[i] == '(' {
			return true
		}
	}
	return false
}

func (s EdgeLeaseSubjectV1) Validate() error {
	if s.UserID <= 0 {
		return fmt.Errorf("lease.subject.user_id must be greater than zero")
	}
	if s.TokenID <= 0 {
		return fmt.Errorf("lease.subject.token_id must be greater than zero")
	}
	return nil
}

func (r EdgeLeaseAcquireRequestV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if err := r.Subject.Validate(); err != nil {
		return err
	}
	if err := validateEdgeControlQuotaV1("lease.requested_quota", r.RequestedQuota); err != nil {
		return err
	}
	if err := validateEdgeControlQuotaV1("lease.minimum_acceptable_quota", r.MinimumAcceptableQuota); err != nil {
		return err
	}
	if r.MinimumAcceptableQuota > r.RequestedQuota {
		return fmt.Errorf("lease.minimum_acceptable_quota must not exceed requested_quota")
	}
	if r.ExistingLeaseID != "" {
		if err := validateEdgeControlIdentifierV1("lease.existing_lease_id", r.ExistingLeaseID); err != nil {
			return err
		}
	}
	if err := validateEdgeControlIdentifierV1("lease.snapshot_id", r.SnapshotID); err != nil {
		return err
	}
	if r.SnapshotRevision <= 0 {
		return fmt.Errorf("lease.snapshot_revision must be greater than zero")
	}
	return nil
}

func (l EdgeQuotaLeaseV1) Validate() error {
	if err := validateEdgeControlIdentifierV1("lease.lease_id", l.LeaseID); err != nil {
		return err
	}
	if l.Version <= 0 {
		return fmt.Errorf("lease.version must be greater than zero")
	}
	if !l.Status.Valid() {
		return fmt.Errorf("lease.status is invalid")
	}
	if err := validateEdgeControlIdentifierV1("lease.node_id", l.NodeID); err != nil {
		return err
	}
	if l.NodeGeneration <= 0 {
		return fmt.Errorf("lease.node_generation must be greater than zero")
	}
	if err := l.Subject.Validate(); err != nil {
		return err
	}
	if err := validateEdgeControlQuotaV1("lease.granted_quota", l.GrantedQuota); err != nil {
		return err
	}
	if err := validateEdgeControlQuotaV1("lease.renew_after_remaining_quota", l.RenewAfterRemainingQuota); err != nil {
		return err
	}
	if l.GrantedQuota == 0 && l.RenewAfterRemainingQuota != 0 {
		return fmt.Errorf("zero-quota lease must have a zero renewal threshold")
	}
	if l.GrantedQuota > 0 && l.RenewAfterRemainingQuota >= l.GrantedQuota {
		return fmt.Errorf("lease.renew_after_remaining_quota must be less than granted_quota")
	}
	if err := validateEdgeControlUnixMilliV1("lease.issued_at_unix_milli", l.IssuedAtUnixMilli, false); err != nil {
		return err
	}
	if err := validateEdgeControlUnixMilliV1("lease.expires_at_unix_milli", l.ExpiresAtUnixMilli, false); err != nil {
		return err
	}
	if l.ExpiresAtUnixMilli <= l.IssuedAtUnixMilli {
		return fmt.Errorf("lease.expires_at_unix_milli must be after issued_at_unix_milli")
	}
	if err := validateEdgeControlIdentifierV1("lease.snapshot_id", l.SnapshotID); err != nil {
		return err
	}
	if l.SnapshotRevision <= 0 {
		return fmt.Errorf("lease.snapshot_revision must be greater than zero")
	}
	if l.PricingRevision <= 0 || l.PricingRevision > l.SnapshotRevision {
		return fmt.Errorf("lease.pricing_revision must be between 1 and snapshot_revision")
	}
	return nil
}

func (r EdgeLeaseAcquireResponseV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if err := r.Lease.Validate(); err != nil {
		return err
	}
	if r.Lease.Status != EdgeLeaseStatusActiveV1 {
		return fmt.Errorf("lease acquisition must return an active lease")
	}
	return nil
}

func (r EdgeLeaseCloseRequestV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if err := validateEdgeControlIdentifierV1("lease_close.lease_id", r.LeaseID); err != nil {
		return err
	}
	if r.LeaseVersion <= 0 {
		return fmt.Errorf("lease_close.lease_version must be greater than zero")
	}
	if r.FinalEventSequence < 0 {
		return fmt.Errorf("lease_close.final_event_sequence must not be negative")
	}
	return nil
}

func (r EdgeLeaseCloseResponseV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if err := validateEdgeControlIdentifierV1("lease_close.lease_id", r.LeaseID); err != nil {
		return err
	}
	if r.LeaseVersion <= 0 {
		return fmt.Errorf("lease_close.lease_version must be greater than zero")
	}
	switch r.Status {
	case EdgeLeaseStatusClosingV1, EdgeLeaseStatusClosedV1, EdgeLeaseStatusRevokedV1, EdgeLeaseStatusForceClosedV1:
	default:
		return fmt.Errorf("lease_close.status is invalid")
	}
	if err := validateEdgeControlQuotaV1("lease_close.granted_quota", r.GrantedQuota); err != nil {
		return err
	}
	if err := validateEdgeControlQuotaV1("lease_close.accepted_quota", r.AcceptedQuota); err != nil {
		return err
	}
	if err := validateEdgeControlQuotaV1("lease_close.returned_quota", r.ReturnedQuota); err != nil {
		return err
	}
	accounted := r.AcceptedQuota + r.ReturnedQuota
	if accounted > r.GrantedQuota {
		return fmt.Errorf("lease_close accepted_quota plus returned_quota must not exceed granted_quota")
	}
	if r.Status != EdgeLeaseStatusClosingV1 && accounted != r.GrantedQuota {
		return fmt.Errorf("terminal lease_close response must account for all granted_quota")
	}
	if r.CloseAfterEventSequence < 0 {
		return fmt.Errorf("lease_close.close_after_event_sequence must not be negative")
	}
	return nil
}

func (b EdgeUsageBillingV1) Validate() error {
	if err := validateEdgeControlIdentifierV1("usage.billing.pricing_policy_id", b.PricingPolicyID); err != nil {
		return err
	}
	if err := validateEdgeControlIdentifierV1("usage.billing.pricing_policy_version", b.PricingPolicyVersion); err != nil {
		return err
	}
	if !b.BillingMode.Valid() {
		return fmt.Errorf("usage.billing.billing_mode is invalid")
	}
	if err := validateEdgeControlFiniteFloatV1("usage.billing.group_ratio", b.GroupRatio, false); err != nil {
		return err
	}
	if len(b.AppliedRatios) > EdgeControlMaxAppliedRatiosV1 {
		return fmt.Errorf("usage.billing.applied_ratios exceeds %d items", EdgeControlMaxAppliedRatiosV1)
	}
	product := 1.0
	for name, ratio := range b.AppliedRatios {
		if err := validateEdgeControlIdentifierV1("usage.billing.applied_ratios key", name); err != nil {
			return err
		}
		if err := validateEdgeControlFiniteFloatV1("usage.billing.applied_ratios["+name+"]", ratio, true); err != nil {
			return err
		}
		product *= ratio
		if math.IsInf(product, 0) || math.IsNaN(product) || product > float64(common.MaxQuota) {
			return fmt.Errorf("usage.billing.applied_ratios product exceeds the supported range")
		}
	}
	if b.BillingMode == EdgeBillingModeTieredExprV1 {
		if err := validateEdgeControlSHA256V1("usage.billing.billing_expression_hash", b.BillingExpressionHash); err != nil {
			return err
		}
		if b.MatchedTier != "" {
			if err := validateEdgeControlTextV1("usage.billing.matched_tier", b.MatchedTier, EdgeControlMaxNodeNameLengthV1, false); err != nil {
				return err
			}
		}
	} else if b.BillingExpressionHash != "" || b.MatchedTier != "" {
		return fmt.Errorf("usage.billing tiered fields are only valid for tiered_expr billing")
	}
	if err := validateEdgeControlQuotaV1("usage.billing.reserved_quota", b.ReservedQuota); err != nil {
		return err
	}
	if err := validateEdgeControlQuotaV1("usage.billing.charged_quota", b.ChargedQuota); err != nil {
		return err
	}
	if b.ChargedQuota > b.ReservedQuota {
		return fmt.Errorf("usage.billing.charged_quota must not exceed reserved_quota")
	}
	return nil
}

func (e EdgeUsageEventV1) Validate() error {
	for field, value := range map[string]string{
		"usage.event_id":       e.EventID,
		"usage.lease_id":       e.LeaseID,
		"usage.reservation_id": e.ReservationID,
		"usage.request_id":     e.RequestID,
	} {
		if err := validateEdgeControlIdentifierV1(field, value); err != nil {
			return err
		}
	}
	if e.Sequence <= 0 {
		return fmt.Errorf("usage.sequence must be greater than zero")
	}
	if e.UserID <= 0 || e.TokenID <= 0 || e.ChannelID <= 0 {
		return fmt.Errorf("usage user_id, token_id and channel_id must be greater than zero")
	}
	if !e.Endpoint.Valid() {
		return fmt.Errorf("usage.endpoint is invalid")
	}
	if err := validateEdgeControlModelV1("usage.model", e.Model); err != nil {
		return err
	}
	if err := validateEdgeControlTextV1("usage.group", e.Group, EdgeControlMaxIdentifierLengthV1, false); err != nil {
		return err
	}
	if err := validateEdgeControlUnixMilliV1("usage.started_at_unix_milli", e.StartedAtUnixMilli, false); err != nil {
		return err
	}
	if err := validateEdgeControlUnixMilliV1("usage.finished_at_unix_milli", e.FinishedAtUnixMilli, false); err != nil {
		return err
	}
	if e.FinishedAtUnixMilli < e.StartedAtUnixMilli {
		return fmt.Errorf("usage.finished_at_unix_milli must not be before started_at_unix_milli")
	}
	if !e.Outcome.Valid() {
		return fmt.Errorf("usage.outcome is invalid")
	}
	if e.HTTPStatus != nil {
		if *e.HTTPStatus < 100 || *e.HTTPStatus > 599 {
			return fmt.Errorf("usage.http_status must be between 100 and 599")
		}
		if e.Outcome == EdgeUsageOutcomeSuccessV1 && (*e.HTTPStatus < 200 || *e.HTTPStatus > 299) {
			return fmt.Errorf("successful usage event must have a 2xx http_status")
		}
	}
	if e.ErrorCode != "" {
		if err := validateEdgeControlTextV1("usage.error_code", e.ErrorCode, EdgeControlMaxNodeNameLengthV1, false); err != nil {
			return err
		}
	}
	if e.Outcome == EdgeUsageOutcomeSuccessV1 && e.ErrorCode != "" {
		return fmt.Errorf("successful usage event must not have error_code")
	}
	if e.Usage != nil {
		if err := validateEdgeBillingUsageV1(e.Usage); err != nil {
			return err
		}
	}
	return e.Billing.Validate()
}

func (r EdgeSettlementBlockRequestV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	if err := validateEdgeControlIdentifierV1("settlement.block_id", r.BlockID); err != nil {
		return err
	}
	if (r.PreviousBlockID == "") != (r.PreviousBlockDigest == "") {
		return fmt.Errorf("settlement.previous_block_id and previous_block_digest must be present together")
	}
	if r.PreviousBlockID != "" {
		if err := validateEdgeControlIdentifierV1("settlement.previous_block_id", r.PreviousBlockID); err != nil {
			return err
		}
		if r.PreviousBlockID == r.BlockID {
			return fmt.Errorf("settlement.previous_block_id must differ from block_id")
		}
		if err := validateEdgeControlSHA256V1("settlement.previous_block_digest", r.PreviousBlockDigest); err != nil {
			return err
		}
	}
	if r.FirstSequence <= 0 || r.LastSequence < r.FirstSequence {
		return fmt.Errorf("settlement sequence range is invalid")
	}
	eventCount := r.LastSequence - r.FirstSequence + 1
	if eventCount > int64(EdgeControlMaxSettlementEventsV1) {
		return fmt.Errorf("settlement sequence range exceeds %d events", EdgeControlMaxSettlementEventsV1)
	}
	if len(r.Events) == 0 || len(r.Events) > EdgeControlMaxSettlementEventsV1 || int64(len(r.Events)) != eventCount {
		return fmt.Errorf("settlement.events must exactly cover the declared sequence range")
	}
	if err := validateEdgeControlUnixMilliV1("settlement.created_at_unix_milli", r.CreatedAtUnixMilli, false); err != nil {
		return err
	}
	if err := validateEdgeControlSHA256V1("settlement.block_digest", r.BlockDigest); err != nil {
		return err
	}
	seenEvents := make(map[string]struct{}, len(r.Events))
	seenReservations := make(map[string]struct{}, len(r.Events))
	for i, event := range r.Events {
		if err := event.Validate(); err != nil {
			return fmt.Errorf("settlement.events[%d]: %w", i, err)
		}
		expectedSequence := r.FirstSequence + int64(i)
		if event.Sequence != expectedSequence {
			return fmt.Errorf("settlement.events[%d].sequence must be %d", i, expectedSequence)
		}
		if event.FinishedAtUnixMilli > r.CreatedAtUnixMilli {
			return fmt.Errorf("settlement.events[%d] must finish no later than block creation", i)
		}
		if _, exists := seenEvents[event.EventID]; exists {
			return fmt.Errorf("settlement.events contains duplicate event_id %q", event.EventID)
		}
		seenEvents[event.EventID] = struct{}{}
		if _, exists := seenReservations[event.ReservationID]; exists {
			return fmt.Errorf("settlement.events contains duplicate reservation_id %q", event.ReservationID)
		}
		seenReservations[event.ReservationID] = struct{}{}
	}
	return nil
}

func (a EdgeSettlementAckV1) Validate() error {
	if !a.Status.Valid() {
		return fmt.Errorf("settlement_ack.status is invalid")
	}
	if err := validateEdgeControlIdentifierV1("settlement_ack.node_id", a.NodeID); err != nil {
		return err
	}
	if a.NodeGeneration <= 0 {
		return fmt.Errorf("settlement_ack.node_generation must be greater than zero")
	}
	if err := validateEdgeControlIdentifierV1("settlement_ack.block_id", a.BlockID); err != nil {
		return err
	}
	if a.AckedThroughSequence < 0 || a.AckedThroughSequence == math.MaxInt64 {
		return fmt.Errorf("settlement_ack.acked_through_sequence is invalid")
	}
	if a.NextExpectedSequence != a.AckedThroughSequence+1 {
		return fmt.Errorf("settlement_ack.next_expected_sequence must follow acked_through_sequence")
	}
	if a.AcceptedEventCount < 0 || a.AcceptedEventCount > EdgeControlMaxSettlementEventsV1 {
		return fmt.Errorf("settlement_ack.accepted_event_count must be between 0 and %d", EdgeControlMaxSettlementEventsV1)
	}
	return validateEdgeControlUnixMilliV1("settlement_ack.acknowledged_at_unix_milli", a.AcknowledgedAtUnixMilli, false)
}

func (r EdgeSettlementBlockResponseV1) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	return r.Ack.Validate()
}

func edgeSnapshotDatasetRankV1(dataset EdgeSnapshotDatasetV1) int {
	switch dataset {
	case EdgeSnapshotDatasetAuthenticationV1:
		return 0
	case EdgeSnapshotDatasetUsersV1:
		return 1
	case EdgeSnapshotDatasetGroupsV1:
		return 2
	case EdgeSnapshotDatasetModelsV1:
		return 3
	case EdgeSnapshotDatasetChannelsV1:
		return 4
	case EdgeSnapshotDatasetPricingV1:
		return 5
	case EdgeSnapshotDatasetRoutingV1:
		return 6
	default:
		return -1
	}
}

func validateEdgeTokenAuthRecordV1(record EdgeTokenAuthRecordV1) error {
	if err := edgetoken.ValidateFingerprint(record.TokenFingerprint); err != nil {
		return fmt.Errorf("token_fingerprint is invalid: %w", err)
	}
	if record.TokenID <= 0 || record.UserID <= 0 {
		return fmt.Errorf("token_id and user_id must be greater than zero")
	}
	if record.ExpiresAtUnixMilli != nil {
		if err := validateEdgeControlUnixMilliV1("expires_at_unix_milli", *record.ExpiresAtUnixMilli, false); err != nil {
			return err
		}
	}
	if record.Group != "" {
		if err := validateEdgeControlTextV1("group", record.Group, EdgeControlMaxIdentifierLengthV1, false); err != nil {
			return err
		}
	}
	if err := validateEdgeControlModelListV1("allowed_models", record.AllowedModels, EdgeControlMaxChannelModelsV1); err != nil {
		return err
	}
	for i := 1; i < len(record.AllowedModels); i++ {
		if record.AllowedModels[i] <= record.AllowedModels[i-1] {
			return fmt.Errorf("allowed_models must use canonical order")
		}
	}
	if !record.ModelLimitEnabled && len(record.AllowedModels) != 0 {
		return fmt.Errorf("allowed_models must be empty when model_limit_enabled is false")
	}
	if len(record.AllowedCIDRs) > EdgeControlMaxChannelGroupsV1 {
		return fmt.Errorf("allowed_cidrs exceeds %d items", EdgeControlMaxChannelGroupsV1)
	}
	for i, cidr := range record.AllowedCIDRs {
		if err := validateEdgeControlTextV1(fmt.Sprintf("allowed_cidrs[%d]", i), cidr, EdgeControlMaxIdentifierLengthV1, false); err != nil {
			return err
		}
		if i > 0 && cidr <= record.AllowedCIDRs[i-1] {
			return fmt.Errorf("allowed_cidrs must be unique and use canonical order")
		}
	}
	return nil
}

func validateEdgeUserPolicyV1(user EdgeUserPolicyV1) error {
	if user.UserID <= 0 {
		return fmt.Errorf("user_id must be greater than zero")
	}
	if err := validateEdgeControlTextV1("username", user.Username, EdgeControlMaxNodeNameLengthV1, false); err != nil {
		return err
	}
	if user.Email != "" {
		if err := validateEdgeControlTextV1("email", user.Email, 320, false); err != nil {
			return err
		}
	}
	if err := validateEdgeControlTextV1("default_group", user.DefaultGroup, EdgeControlMaxIdentifierLengthV1, false); err != nil {
		return err
	}
	if user.Setting.Language != "" {
		if err := validateEdgeControlIdentifierV1("setting.language", user.Setting.Language); err != nil {
			return err
		}
	}
	return nil
}

func validateEdgeGroupPolicyV1(group EdgeGroupPolicyV1) error {
	if err := validateEdgeControlTextV1("user_group", group.UserGroup, EdgeControlMaxIdentifierLengthV1, false); err != nil {
		return err
	}
	if len(group.UsingGroups) > EdgeControlMaxChannelGroupsV1 {
		return fmt.Errorf("using_groups exceeds %d items", EdgeControlMaxChannelGroupsV1)
	}
	previous := ""
	for i, usingGroup := range group.UsingGroups {
		if err := validateEdgeControlTextV1(fmt.Sprintf("using_groups[%d].group", i), usingGroup.Group, EdgeControlMaxIdentifierLengthV1, false); err != nil {
			return err
		}
		if i > 0 && usingGroup.Group <= previous {
			return fmt.Errorf("using_groups must be unique and ordered by group")
		}
		previous = usingGroup.Group
		if err := validateEdgeControlFiniteFloatV1(fmt.Sprintf("using_groups[%d].ratio", i), usingGroup.Ratio, false); err != nil {
			return err
		}
	}
	return nil
}

func validateEdgeBillingUsageV1(usage *BillingUsage) error {
	if usage == nil {
		return nil
	}
	variants := 0
	if usage.OpenAIUsage != nil {
		variants++
	}
	if usage.ClaudeUsage != nil {
		variants++
	}
	if usage.GeminiUsageMetadata != nil {
		variants++
	}
	if variants != 1 {
		return fmt.Errorf("usage.billing_usage must contain exactly one usage variant")
	}

	switch usage.Source {
	case BillingUsageSourceOAIChat, BillingUsageSourceOAIResponses:
		if usage.Semantic != BillingUsageSemanticOpenAI || usage.OpenAIUsage == nil {
			return fmt.Errorf("usage.billing_usage source, semantic and OpenAI payload do not match")
		}
	case BillingUsageSourceClaudeMessages:
		if usage.Semantic != BillingUsageSemanticAnthropic || usage.ClaudeUsage == nil {
			return fmt.Errorf("usage.billing_usage source, semantic and Claude payload do not match")
		}
	case BillingUsageSourceGeminiChat:
		if usage.Semantic != BillingUsageSemanticGemini || usage.GeminiUsageMetadata == nil {
			return fmt.Errorf("usage.billing_usage source, semantic and Gemini payload do not match")
		}
	default:
		return fmt.Errorf("usage.billing_usage.source is invalid")
	}

	if openAI := usage.OpenAIUsage; openAI != nil {
		if openAI.BillingUsage != nil || openAI.Cost != nil {
			return fmt.Errorf("usage.billing_usage.openai_usage must not contain nested billing usage or dynamic cost")
		}
		for _, count := range []struct {
			name  string
			value int
		}{
			{name: "prompt_tokens", value: openAI.PromptTokens},
			{name: "completion_tokens", value: openAI.CompletionTokens},
			{name: "total_tokens", value: openAI.TotalTokens},
			{name: "prompt_cache_hit_tokens", value: openAI.PromptCacheHitTokens},
			{name: "input_tokens", value: openAI.InputTokens},
			{name: "output_tokens", value: openAI.OutputTokens},
			{name: "claude_cache_creation_5_m_tokens", value: openAI.ClaudeCacheCreation5mTokens},
			{name: "claude_cache_creation_1_h_tokens", value: openAI.ClaudeCacheCreation1hTokens},
			{name: "prompt_tokens_details.cached_tokens", value: openAI.PromptTokensDetails.CachedTokens},
			{name: "prompt_tokens_details.cached_creation_tokens", value: openAI.PromptTokensDetails.CachedCreationTokens},
			{name: "prompt_tokens_details.cache_write_tokens", value: openAI.PromptTokensDetails.CacheWriteTokens},
			{name: "prompt_tokens_details.text_tokens", value: openAI.PromptTokensDetails.TextTokens},
			{name: "prompt_tokens_details.audio_tokens", value: openAI.PromptTokensDetails.AudioTokens},
			{name: "prompt_tokens_details.image_tokens", value: openAI.PromptTokensDetails.ImageTokens},
			{name: "completion_tokens_details.text_tokens", value: openAI.CompletionTokenDetails.TextTokens},
			{name: "completion_tokens_details.audio_tokens", value: openAI.CompletionTokenDetails.AudioTokens},
			{name: "completion_tokens_details.image_tokens", value: openAI.CompletionTokenDetails.ImageTokens},
			{name: "completion_tokens_details.reasoning_tokens", value: openAI.CompletionTokenDetails.ReasoningTokens},
		} {
			if err := validateEdgeBillingCountV1("usage.billing_usage.openai_usage."+count.name, count.value); err != nil {
				return err
			}
		}
		if openAI.InputTokensDetails != nil {
			for _, count := range []struct {
				name  string
				value int
			}{
				{name: "cached_tokens", value: openAI.InputTokensDetails.CachedTokens},
				{name: "cached_creation_tokens", value: openAI.InputTokensDetails.CachedCreationTokens},
				{name: "cache_write_tokens", value: openAI.InputTokensDetails.CacheWriteTokens},
				{name: "text_tokens", value: openAI.InputTokensDetails.TextTokens},
				{name: "audio_tokens", value: openAI.InputTokensDetails.AudioTokens},
				{name: "image_tokens", value: openAI.InputTokensDetails.ImageTokens},
			} {
				if err := validateEdgeBillingCountV1("usage.billing_usage.openai_usage.input_tokens_details."+count.name, count.value); err != nil {
					return err
				}
			}
		}
	}

	if claude := usage.ClaudeUsage; claude != nil {
		if claude.BillingUsage != nil {
			return fmt.Errorf("usage.billing_usage.claude_usage must not contain nested billing usage")
		}
		for _, count := range []struct {
			name  string
			value int
		}{
			{name: "input_tokens", value: claude.InputTokens},
			{name: "cache_creation_input_tokens", value: claude.CacheCreationInputTokens},
			{name: "cache_read_input_tokens", value: claude.CacheReadInputTokens},
			{name: "output_tokens", value: claude.OutputTokens},
			{name: "claude_cache_creation_5_m_tokens", value: claude.ClaudeCacheCreation5mTokens},
			{name: "claude_cache_creation_1_h_tokens", value: claude.ClaudeCacheCreation1hTokens},
		} {
			if err := validateEdgeBillingCountV1("usage.billing_usage.claude_usage."+count.name, count.value); err != nil {
				return err
			}
		}
		if claude.CacheCreation != nil {
			if err := validateEdgeBillingCountV1("usage.billing_usage.claude_usage.cache_creation.ephemeral_5m_input_tokens", claude.CacheCreation.Ephemeral5mInputTokens); err != nil {
				return err
			}
			if err := validateEdgeBillingCountV1("usage.billing_usage.claude_usage.cache_creation.ephemeral_1h_input_tokens", claude.CacheCreation.Ephemeral1hInputTokens); err != nil {
				return err
			}
		}
		if claude.ServerToolUse != nil {
			if err := validateEdgeBillingCountV1("usage.billing_usage.claude_usage.server_tool_use.web_search_requests", claude.ServerToolUse.WebSearchRequests); err != nil {
				return err
			}
		}
	}

	if gemini := usage.GeminiUsageMetadata; gemini != nil {
		if gemini.BillingUsage != nil {
			return fmt.Errorf("usage.billing_usage.gemini_usage_metadata must not contain nested billing usage")
		}
		for _, count := range []struct {
			name  string
			value int
		}{
			{name: "prompt_token_count", value: gemini.PromptTokenCount},
			{name: "tool_use_prompt_token_count", value: gemini.ToolUsePromptTokenCount},
			{name: "candidates_token_count", value: gemini.CandidatesTokenCount},
			{name: "total_token_count", value: gemini.TotalTokenCount},
			{name: "thoughts_token_count", value: gemini.ThoughtsTokenCount},
			{name: "cached_content_token_count", value: gemini.CachedContentTokenCount},
		} {
			if err := validateEdgeBillingCountV1("usage.billing_usage.gemini_usage_metadata."+count.name, count.value); err != nil {
				return err
			}
		}
		for name, details := range map[string][]GeminiPromptTokensDetails{
			"prompt_tokens_details":          gemini.PromptTokensDetails,
			"tool_use_prompt_tokens_details": gemini.ToolUsePromptTokensDetails,
			"candidates_tokens_details":      gemini.CandidatesTokensDetails,
		} {
			if len(details) > EdgeControlMaxBillingUsageDetailsV1 {
				return fmt.Errorf("usage.billing_usage.gemini_usage_metadata.%s exceeds %d items", name, EdgeControlMaxBillingUsageDetailsV1)
			}
			for i, detail := range details {
				if err := validateEdgeControlTextV1(fmt.Sprintf("usage.billing_usage.gemini_usage_metadata.%s[%d].modality", name, i), detail.Modality, EdgeControlMaxIdentifierLengthV1, false); err != nil {
					return err
				}
				if err := validateEdgeBillingCountV1(fmt.Sprintf("usage.billing_usage.gemini_usage_metadata.%s[%d].token_count", name, i), detail.TokenCount); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateEdgeBillingCountV1(field string, value int) error {
	if value < 0 || int64(value) > int64(EdgeControlMaxBillingTokenCountV1) {
		return fmt.Errorf("%s must be between 0 and %d", field, EdgeControlMaxBillingTokenCountV1)
	}
	return nil
}

func validateEdgeControlSHA256V1(field string, value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("%s must be a lowercase SHA-256 hexadecimal digest", field)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%s must be a lowercase SHA-256 hexadecimal digest", field)
	}
	return nil
}

func validateEdgeControlFiniteFloatV1(field string, value float64, positive bool) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value > float64(common.MaxQuota) {
		return fmt.Errorf("%s must be finite and not exceed %d", field, common.MaxQuota)
	}
	if positive && value <= 0 {
		return fmt.Errorf("%s must be greater than zero", field)
	}
	if !positive && value < 0 {
		return fmt.Errorf("%s must not be negative", field)
	}
	return nil
}

func validateEdgeControlQuotaV1(field string, value int64) error {
	if value < 0 || value > int64(common.MaxQuota) {
		return fmt.Errorf("%s must be between 0 and %d", field, common.MaxQuota)
	}
	return nil
}

func hasUnsupportedEdgeBillingExpressionVersionV1(expression string) bool {
	colon := strings.IndexByte(expression, ':')
	if colon < 2 || colon > 10 || expression[0] != 'v' {
		return false
	}
	for i := 1; i < colon; i++ {
		if expression[i] < '0' || expression[i] > '9' {
			return false
		}
	}
	return expression[:colon+1] != "v1:"
}

func edgeBillingExpressionIdentifierStartV1(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character == '_'
}

func edgeBillingExpressionIdentifierPartV1(character byte) bool {
	return edgeBillingExpressionIdentifierStartV1(character) || character >= '0' && character <= '9'
}

func validateEdgeAffinityRegexListV1(field string, values []string, optional bool) error {
	if !optional && len(values) == 0 {
		return fmt.Errorf("%s must not be empty", field)
	}
	if len(values) > EdgeControlMaxAffinityRegexesV1 {
		return fmt.Errorf("%s exceeds %d items", field, EdgeControlMaxAffinityRegexesV1)
	}
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if err := validateEdgeControlTextV1(fmt.Sprintf("%s[%d]", field, i), value, EdgeControlMaxAffinityRegexLengthV1, false); err != nil {
			return err
		}
		if _, err := regexp.Compile(value); err != nil {
			return fmt.Errorf("%s[%d] is invalid: %w", field, i, err)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s contains duplicate regular expression", field)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateEdgeAffinityStringListV1(field string, values []string, maxItems int, maxLength int, caseInsensitive bool) error {
	if len(values) > maxItems {
		return fmt.Errorf("%s exceeds %d items", field, maxItems)
	}
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if err := validateEdgeControlTextV1(fmt.Sprintf("%s[%d]", field, i), value, maxLength, false); err != nil {
			return err
		}
		identity := value
		if caseInsensitive {
			identity = strings.ToLower(value)
		}
		if _, exists := seen[identity]; exists {
			return fmt.Errorf("%s contains duplicate value %q", field, value)
		}
		seen[identity] = struct{}{}
	}
	return nil
}

func validHTTPHeaderTokenV1(value string) bool {
	if len(value) == 0 || len(value) > EdgeControlMaxAffinityIncludeLengthV1 {
		return false
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			continue
		}
		switch character {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validateEdgeControlIdentifierV1(field string, value string) error {
	if len(value) == 0 || len(value) > EdgeControlMaxIdentifierLengthV1 {
		return fmt.Errorf("%s length must be between 1 and %d bytes", field, EdgeControlMaxIdentifierLengthV1)
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
			return fmt.Errorf("%s must use lowercase ASCII letters, digits, '-', '_', '.' or ':'", field)
		}
	}
	return nil
}

func validateEdgeControlPublicURLV1(value string) error {
	if len(value) == 0 || len(value) > EdgeControlMaxPublicURLLengthV1 {
		return fmt.Errorf("declaration.public_url length must be between 1 and %d bytes", EdgeControlMaxPublicURLLengthV1)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("declaration.public_url must not contain surrounding whitespace")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("declaration.public_url is invalid: %w", err)
	}
	if !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return fmt.Errorf("declaration.public_url must be an absolute HTTP URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("declaration.public_url scheme must be http or https")
	}
	if parsed.User != nil {
		return fmt.Errorf("declaration.public_url must not contain userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("declaration.public_url must not contain a query")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("declaration.public_url must not contain a fragment")
	}
	return nil
}

func validateEdgeControlUnixMilliV1(field string, value int64, optional bool) error {
	if optional && value == 0 {
		return nil
	}
	if value < edgeControlMinUnixMilliV1 || value > edgeControlMaxUnixMilliV1 {
		return fmt.Errorf("%s must be a Unix millisecond timestamp between years 2000 and 9999", field)
	}
	return nil
}

func validateEdgeControlPositiveLimitV1(field string, value int64, maximum int64) error {
	if value <= 0 || value > maximum {
		return fmt.Errorf("%s must be between 1 and %d", field, maximum)
	}
	return nil
}

func validateEdgeControlTextV1(field string, value string, maxLength int, optional bool) error {
	if optional && value == "" {
		return nil
	}
	if len(value) == 0 || len(value) > maxLength {
		return fmt.Errorf("%s length must be between 1 and %d bytes", field, maxLength)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must not contain surrounding whitespace", field)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return fmt.Errorf("%s must not contain control characters", field)
		}
	}
	return nil
}

func validateEdgeControlModelV1(field string, value string) error {
	return validateEdgeControlTextV1(field, value, EdgeControlMaxModelLengthV1, false)
}

func validateEdgeControlModelListV1(field string, values []string, maxItems int) error {
	if len(values) > maxItems {
		return fmt.Errorf("%s exceeds %d items", field, maxItems)
	}
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if err := validateEdgeControlModelV1(fmt.Sprintf("%s[%d]", field, i), value); err != nil {
			return err
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s contains duplicate model %q", field, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}
