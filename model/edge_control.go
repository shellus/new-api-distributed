package model

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type EdgeNodeStatus string

const (
	EdgeNodeStatusActive   EdgeNodeStatus = "active"
	EdgeNodeStatusDisabled EdgeNodeStatus = "disabled"
	EdgeNodeStatusRevoked  EdgeNodeStatus = "revoked"
)

type EdgeNodeCredentialStatus string

const (
	EdgeNodeCredentialStatusActive  EdgeNodeCredentialStatus = "active"
	EdgeNodeCredentialStatusRetired EdgeNodeCredentialStatus = "retired"
	EdgeNodeCredentialStatusRevoked EdgeNodeCredentialStatus = "revoked"
)

type EdgeRequestReceiptStatus string

const (
	EdgeRequestReceiptStatusProcessing EdgeRequestReceiptStatus = "processing"
	EdgeRequestReceiptStatusCommitted  EdgeRequestReceiptStatus = "committed"
	EdgeRequestReceiptStatusRejected   EdgeRequestReceiptStatus = "rejected"
)

type EdgePolicySnapshotStatus string

const (
	EdgePolicySnapshotStatusDraft     EdgePolicySnapshotStatus = "draft"
	EdgePolicySnapshotStatusPublished EdgePolicySnapshotStatus = "published"
	EdgePolicySnapshotStatusRetired   EdgePolicySnapshotStatus = "retired"
)

var (
	ErrInvalidEdgeNodeStatus                 = errors.New("invalid edge node status")
	ErrInvalidEdgeNodeCredentialStatus       = errors.New("invalid edge node credential status")
	ErrEdgeNodeCredentialInactive            = errors.New("edge node credential is inactive")
	ErrEdgeNodeCredentialNotYetValid         = errors.New("edge node credential is not yet valid")
	ErrEdgeNodeCredentialExpired             = errors.New("edge node credential is expired")
	ErrEdgeNodeCredentialRevoked             = errors.New("edge node credential is revoked")
	ErrEdgeRequestReceiptNonceConflict       = errors.New("edge request nonce was reused for a different request")
	ErrEdgeRequestReceiptIdempotencyConflict = errors.New("edge request idempotency key was reused for a different request")
	ErrEdgeRequestReceiptHashConflict        = ErrEdgeRequestReceiptNonceConflict
	ErrEdgeRequestReceiptAlreadyFinalized    = errors.New("edge request receipt is already finalized")
	ErrEdgePolicySnapshotImmutable           = errors.New("published edge policy snapshot is immutable")
	ErrEdgePolicySnapshotUpdateRequiresModel = errors.New("edge policy snapshot update requires a loaded model")
	ErrInvalidEdgePolicySnapshotStatus       = errors.New("invalid edge policy snapshot status")
	ErrInvalidEdgeControlHash                = errors.New("edge control hash must be a lowercase SHA-256 hex digest")
)

// EdgeNode is the master-side identity and control cursor for one trusted edge.
// Generation changes only when the edge's durable local accounting state is
// explicitly replaced; ordinary process restarts keep the same generation.
type EdgeNode struct {
	ID                  int64          `json:"id" gorm:"primaryKey"`
	NodeUID             string         `json:"node_uid" gorm:"type:varchar(64);not null;uniqueIndex"`
	Name                string         `json:"name" gorm:"type:varchar(128);not null"`
	Region              string         `json:"region" gorm:"type:varchar(64);not null"`
	Status              EdgeNodeStatus `json:"status" gorm:"type:varchar(32);not null;index:idx_edge_nodes_status_seen,priority:1"`
	Generation          int64          `json:"generation" gorm:"type:bigint;not null;index:idx_edge_nodes_generation_block,priority:1"`
	ProtocolVersion     string         `json:"protocol_version" gorm:"type:varchar(32);not null"`
	DeclaredPublicURL   string         `json:"declared_public_url" gorm:"type:text"`
	SoftwareVersion     string         `json:"software_version" gorm:"type:varchar(64);not null"`
	StartedAt           int64          `json:"started_at" gorm:"type:bigint;not null"`
	Capabilities        string         `json:"capabilities" gorm:"type:text;not null"`
	LastPolicyVersion   int64          `json:"last_policy_version" gorm:"type:bigint;not null"`
	LastBlockSeq        int64          `json:"last_block_seq" gorm:"type:bigint;not null;index:idx_edge_nodes_generation_block,priority:2"`
	LastEventSeq        int64          `json:"last_event_seq" gorm:"type:bigint;not null"`
	MaxOutstandingQuota int64          `json:"max_outstanding_quota" gorm:"type:bigint;not null"`
	LastSeenAt          int64          `json:"last_seen_at" gorm:"type:bigint;not null;index:idx_edge_nodes_status_seen,priority:2"`
	CreatedAt           int64          `json:"created_at" gorm:"type:bigint;not null;index"`
	UpdatedAt           int64          `json:"updated_at" gorm:"type:bigint;not null;index"`
}

func (n *EdgeNode) BeforeCreate(_ *gorm.DB) error {
	if n == nil {
		return errors.New("edge node is nil")
	}
	n.NodeUID = strings.TrimSpace(n.NodeUID)
	n.Name = strings.TrimSpace(n.Name)
	n.Region = strings.TrimSpace(n.Region)
	n.DeclaredPublicURL = strings.TrimSpace(n.DeclaredPublicURL)
	n.SoftwareVersion = strings.TrimSpace(n.SoftwareVersion)
	if err := edgeauth.ValidateNodeID(n.NodeUID); err != nil {
		return err
	}
	if n.Name == "" {
		return errors.New("edge node name is empty")
	}
	if len(n.Name) > 128 || len(n.Region) > 64 || len(n.SoftwareVersion) > 64 {
		return errors.New("edge node declaration field exceeds storage limit")
	}
	if n.Generation <= 0 {
		return errors.New("edge node generation must be greater than zero")
	}
	if n.ProtocolVersion == "" {
		n.ProtocolVersion = dto.EdgeControlProtocolVersionV1
	}
	if n.ProtocolVersion != dto.EdgeControlProtocolVersionV1 {
		return fmt.Errorf("unsupported edge node protocol version: %s", n.ProtocolVersion)
	}
	if n.LastPolicyVersion < 0 || n.LastBlockSeq < 0 || n.LastEventSeq < 0 || n.MaxOutstandingQuota < 0 {
		return errors.New("edge node cursors and limits cannot be negative")
	}
	if n.Status == "" {
		n.Status = EdgeNodeStatusActive
	}
	if !n.Status.Valid() {
		return ErrInvalidEdgeNodeStatus
	}
	if strings.TrimSpace(n.Capabilities) == "" {
		data, err := common.Marshal([]EdgeNodeCapability{})
		if err != nil {
			return err
		}
		n.Capabilities = string(data)
	} else {
		var capabilities []EdgeNodeCapability
		if err := common.UnmarshalJsonStr(n.Capabilities, &capabilities); err != nil {
			return err
		}
		if err := validateEdgeNodeCapabilities(capabilities); err != nil {
			return err
		}
		data, err := common.Marshal(capabilities)
		if err != nil {
			return err
		}
		n.Capabilities = string(data)
	}
	now := common.GetTimestamp()
	if n.CreatedAt == 0 {
		n.CreatedAt = now
	}
	if n.UpdatedAt == 0 {
		n.UpdatedAt = now
	}
	return nil
}

func (s EdgeNodeStatus) Valid() bool {
	switch s {
	case EdgeNodeStatusActive, EdgeNodeStatusDisabled, EdgeNodeStatusRevoked:
		return true
	default:
		return false
	}
}

func (n *EdgeNode) CanIssueLease() bool {
	return n != nil && n.Status == EdgeNodeStatusActive
}

func (n *EdgeNode) CanAcceptSettlement() bool {
	return n != nil && (n.Status == EdgeNodeStatusActive || n.Status == EdgeNodeStatusDisabled)
}

func LockEdgeNodeByIDTx(tx *gorm.DB, nodeID int64) (*EdgeNode, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if nodeID <= 0 {
		return nil, errors.New("invalid edge node ID")
	}
	var node EdgeNode
	if err := lockForUpdate(tx).First(&node, nodeID).Error; err != nil {
		return nil, err
	}
	return &node, nil
}

type EdgeNodeCapability = dto.EdgeEndpointCapabilityV1

type EdgeNodeDeclarationUpdate struct {
	Name              string
	Region            string
	PublicURL         string
	SoftwareVersion   string
	StartedAt         int64
	Capabilities      []dto.EdgeEndpointCapabilityV1
	LastPolicyVersion int64
	LastSeenAt        int64
}

func (n *EdgeNode) DecodeCapabilities() ([]dto.EdgeEndpointCapabilityV1, error) {
	if n == nil || strings.TrimSpace(n.Capabilities) == "" {
		return []dto.EdgeEndpointCapabilityV1{}, nil
	}
	var capabilities []dto.EdgeEndpointCapabilityV1
	if err := common.UnmarshalJsonStr(n.Capabilities, &capabilities); err != nil {
		return nil, err
	}
	return capabilities, nil
}

func validateEdgeNodeCapabilities(capabilities []dto.EdgeEndpointCapabilityV1) error {
	if len(capabilities) > 32 {
		return errors.New("edge node capability count exceeds limit")
	}
	seen := make(map[string]struct{}, len(capabilities))
	for i := range capabilities {
		endpoint := dto.EdgeEndpointV1(strings.TrimSpace(string(capabilities[i].Endpoint)))
		switch endpoint {
		case dto.EdgeEndpointOpenAIChatCompletionsV1, dto.EdgeEndpointOpenAIResponsesV1:
		default:
			return errors.New("invalid edge node capability endpoint")
		}
		if _, exists := seen[string(endpoint)]; exists {
			return fmt.Errorf("duplicate edge node capability endpoint: %s", endpoint)
		}
		seen[string(endpoint)] = struct{}{}
		capabilities[i].Endpoint = endpoint
	}
	return nil
}

// UpdateEdgeNodeDeclaration accepts edge-owned runtime declarations only for
// the authenticated generation and for statuses that may still communicate
// with the control plane. A disabled node may settle and report health, while
// a revoked node may not update its declaration.
func UpdateEdgeNodeDeclaration(nodeUID string, generation int64, declaration EdgeNodeDeclarationUpdate) (bool, error) {
	return UpdateEdgeNodeDeclarationTx(DB, nodeUID, generation, declaration)
}

func UpdateEdgeNodeDeclarationTx(tx *gorm.DB, nodeUID string, generation int64, declaration EdgeNodeDeclarationUpdate) (bool, error) {
	if tx == nil {
		return false, errors.New("database is nil")
	}
	nodeUID = strings.TrimSpace(nodeUID)
	if err := edgeauth.ValidateNodeID(nodeUID); err != nil {
		return false, err
	}
	if generation <= 0 {
		return false, errors.New("edge node declaration generation must be greater than zero")
	}
	declaration.Name = strings.TrimSpace(declaration.Name)
	declaration.Region = strings.TrimSpace(declaration.Region)
	declaration.PublicURL = strings.TrimSpace(declaration.PublicURL)
	declaration.SoftwareVersion = strings.TrimSpace(declaration.SoftwareVersion)
	if declaration.Name == "" || declaration.PublicURL == "" || declaration.SoftwareVersion == "" || declaration.StartedAt <= 0 {
		return false, errors.New("incomplete edge node declaration")
	}
	if len(declaration.Name) > 128 || len(declaration.Region) > 64 || len(declaration.SoftwareVersion) > 64 {
		return false, errors.New("edge node declaration field exceeds storage limit")
	}
	if declaration.LastPolicyVersion < 0 {
		return false, errors.New("last policy version cannot be negative")
	}
	if err := validateEdgeNodeCapabilities(declaration.Capabilities); err != nil {
		return false, err
	}
	capabilities, err := common.Marshal(declaration.Capabilities)
	if err != nil {
		return false, err
	}
	if declaration.LastSeenAt <= 0 {
		declaration.LastSeenAt = common.GetTimestamp()
	}
	result := tx.Model(&EdgeNode{}).
		Where("node_uid = ? AND generation = ? AND status IN ?", nodeUID, generation, []EdgeNodeStatus{
			EdgeNodeStatusActive,
			EdgeNodeStatusDisabled,
		}).
		Updates(map[string]any{
			"name":                declaration.Name,
			"region":              declaration.Region,
			"declared_public_url": declaration.PublicURL,
			"software_version":    declaration.SoftwareVersion,
			"started_at":          declaration.StartedAt,
			"capabilities":        string(capabilities),
			"last_policy_version": declaration.LastPolicyVersion,
			"last_seen_at":        declaration.LastSeenAt,
			"updated_at":          common.GetTimestamp(),
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected > 0 {
		return true, nil
	}
	var count int64
	err = tx.Model(&EdgeNode{}).
		Where("node_uid = ? AND generation = ? AND status IN ?", nodeUID, generation, []EdgeNodeStatus{
			EdgeNodeStatusActive,
			EdgeNodeStatusDisabled,
		}).
		Count(&count).Error
	return count == 1, err
}

// EdgeNodeCredential stores only Ed25519 public verification material. The
// corresponding private key remains on the edge and must never be persisted by
// the master.
type EdgeNodeCredential struct {
	ID             int64                    `json:"id" gorm:"primaryKey"`
	CredentialUID  string                   `json:"credential_uid" gorm:"type:varchar(64);not null;uniqueIndex"`
	NodeID         int64                    `json:"node_id" gorm:"not null;index:idx_edge_credentials_node_generation_status,priority:1"`
	Generation     int64                    `json:"generation" gorm:"type:bigint;not null;index:idx_edge_credentials_node_generation_status,priority:2"`
	Algorithm      string                   `json:"algorithm" gorm:"type:varchar(32);not null"`
	VerifyMaterial string                   `json:"verify_material" gorm:"type:text;not null"`
	Fingerprint    string                   `json:"fingerprint" gorm:"type:char(64);not null;uniqueIndex"`
	Status         EdgeNodeCredentialStatus `json:"status" gorm:"type:varchar(32);not null;index:idx_edge_credentials_node_generation_status,priority:3;index:idx_edge_credentials_status_expiry,priority:1"`
	NotBefore      int64                    `json:"not_before" gorm:"type:bigint;not null"`
	ExpiresAt      int64                    `json:"expires_at" gorm:"type:bigint;not null;index:idx_edge_credentials_status_expiry,priority:2"`
	RevokedAt      int64                    `json:"revoked_at" gorm:"type:bigint;not null"`
	LastUsedAt     int64                    `json:"last_used_at" gorm:"type:bigint;not null"`
	CreatedAt      int64                    `json:"created_at" gorm:"type:bigint;not null;index"`
	UpdatedAt      int64                    `json:"updated_at" gorm:"type:bigint;not null;index"`
}

type EdgeControlIdentity struct {
	Node       *EdgeNode
	Credential *EdgeNodeCredential
}

func GetEdgeControlIdentity(nodeUID string, generation int64, credentialUID string) (*EdgeControlIdentity, error) {
	return getEdgeControlIdentityTx(DB, nodeUID, generation, credentialUID, false)
}

// LockEdgeControlIdentityTx reloads the authenticated identity inside the
// caller's mutation transaction. The shared row lock closes the revocation
// race between HTTP signature verification and receipt/domain mutation.
func LockEdgeControlIdentityTx(tx *gorm.DB, nodeUID string, generation int64, credentialUID string) (*EdgeControlIdentity, error) {
	return getEdgeControlIdentityTx(tx, nodeUID, generation, credentialUID, true)
}

func getEdgeControlIdentityTx(tx *gorm.DB, nodeUID string, generation int64, credentialUID string, lock bool) (*EdgeControlIdentity, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	nodeUID = strings.TrimSpace(nodeUID)
	credentialUID = strings.TrimSpace(credentialUID)
	if err := edgeauth.ValidateNodeID(nodeUID); err != nil {
		return nil, err
	}
	if generation <= 0 {
		return nil, errors.New("edge node generation must be greater than zero")
	}
	if err := edgeauth.ValidateKeyID(credentialUID); err != nil {
		return nil, err
	}

	nodeQuery := tx
	credentialQuery := tx
	if lock {
		nodeQuery = lockForUpdate(tx)
		credentialQuery = lockForUpdate(tx)
	}
	var node EdgeNode
	if err := nodeQuery.Where("node_uid = ? AND generation = ?", nodeUID, generation).First(&node).Error; err != nil {
		return nil, err
	}
	var credential EdgeNodeCredential
	if err := credentialQuery.
		Where("credential_uid = ? AND node_id = ? AND generation = ?", credentialUID, node.ID, generation).
		First(&credential).Error; err != nil {
		return nil, err
	}
	return &EdgeControlIdentity{Node: &node, Credential: &credential}, nil
}

func (c *EdgeNodeCredential) BeforeCreate(_ *gorm.DB) error {
	if c == nil {
		return errors.New("edge node credential is nil")
	}
	c.CredentialUID = strings.TrimSpace(c.CredentialUID)
	c.VerifyMaterial = strings.TrimSpace(c.VerifyMaterial)
	if err := edgeauth.ValidateKeyID(c.CredentialUID); err != nil {
		return err
	}
	if c.NodeID <= 0 || c.Generation <= 0 {
		return errors.New("invalid edge node credential identity")
	}
	if c.Algorithm == "" {
		c.Algorithm = edgeauth.Algorithm
	}
	if c.Algorithm != edgeauth.Algorithm {
		return fmt.Errorf("unsupported edge node credential algorithm: %s", c.Algorithm)
	}
	if c.Status == "" {
		c.Status = EdgeNodeCredentialStatusActive
	}
	if !c.Status.Valid() {
		return ErrInvalidEdgeNodeCredentialStatus
	}
	publicKey, err := edgeauth.ParsePublicKey(c.VerifyMaterial)
	if err != nil {
		return err
	}
	c.Fingerprint = edgePublicKeyFingerprint(publicKey)
	if c.ExpiresAt > 0 && c.NotBefore > 0 && c.ExpiresAt <= c.NotBefore {
		return errors.New("edge node credential expiry must be after not-before")
	}
	if c.NotBefore < 0 || c.ExpiresAt < 0 || c.RevokedAt < 0 || c.LastUsedAt < 0 {
		return errors.New("edge node credential timestamps cannot be negative")
	}
	now := common.GetTimestamp()
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	if c.UpdatedAt == 0 {
		c.UpdatedAt = now
	}
	return nil
}

func (s EdgeNodeCredentialStatus) Valid() bool {
	switch s {
	case EdgeNodeCredentialStatusActive, EdgeNodeCredentialStatusRetired, EdgeNodeCredentialStatusRevoked:
		return true
	default:
		return false
	}
}

func (c *EdgeNodeCredential) Ed25519PublicKey() (ed25519.PublicKey, error) {
	if c == nil || c.Algorithm != edgeauth.Algorithm {
		return nil, edgeauth.ErrInvalidPublicKey
	}
	return edgeauth.ParsePublicKey(c.VerifyMaterial)
}

func (c *EdgeNodeCredential) ValidateAt(now int64) error {
	if c == nil {
		return ErrEdgeNodeCredentialInactive
	}
	switch c.Status {
	case EdgeNodeCredentialStatusRevoked:
		return ErrEdgeNodeCredentialRevoked
	case EdgeNodeCredentialStatusActive:
	default:
		return ErrEdgeNodeCredentialInactive
	}
	if c.RevokedAt > 0 && c.RevokedAt <= now {
		return ErrEdgeNodeCredentialRevoked
	}
	if c.NotBefore > 0 && now < c.NotBefore {
		return ErrEdgeNodeCredentialNotYetValid
	}
	if c.ExpiresAt > 0 && now >= c.ExpiresAt {
		return ErrEdgeNodeCredentialExpired
	}
	_, err := c.Ed25519PublicKey()
	return err
}

func edgePublicKeyFingerprint(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return hex.EncodeToString(digest[:])
}

type EdgeRequestReceipt struct {
	ID           int64  `json:"id" gorm:"primaryKey"`
	NodeID       int64  `json:"node_id" gorm:"not null;uniqueIndex:ux_edge_receipts_idempotency,priority:1"`
	CredentialID int64  `json:"credential_id" gorm:"not null;uniqueIndex:ux_edge_receipts_credential_nonce,priority:1"`
	Generation   int64  `json:"generation" gorm:"type:bigint;not null;uniqueIndex:ux_edge_receipts_idempotency,priority:2"`
	NonceHash    string `json:"nonce_hash" gorm:"type:char(64);not null;uniqueIndex:ux_edge_receipts_credential_nonce,priority:2"`
	RequestKind  string `json:"request_kind" gorm:"type:varchar(32);not null;uniqueIndex:ux_edge_receipts_idempotency,priority:3"`
	// RequestHash is the stable logical request digest over method, escaped
	// path, raw query, and body digest. It excludes timestamp, nonce, and
	// signature so retries and credential rotation resolve to one receipt.
	RequestHash     string                   `json:"request_hash" gorm:"type:char(64);not null"`
	IdempotencyKey  string                   `json:"idempotency_key" gorm:"type:varchar(64);not null;uniqueIndex:ux_edge_receipts_idempotency,priority:4"`
	Status          EdgeRequestReceiptStatus `json:"status" gorm:"type:varchar(32);not null;index"`
	ResultRef       string                   `json:"result_ref" gorm:"type:varchar(64);not null"`
	ResponseStatus  int                      `json:"response_status" gorm:"not null"`
	ResponsePayload string                   `json:"response_payload" gorm:"type:text;not null"`
	ExpiresAt       int64                    `json:"expires_at" gorm:"type:bigint;not null;index"`
	CreatedAt       int64                    `json:"created_at" gorm:"type:bigint;not null;index"`
	UpdatedAt       int64                    `json:"updated_at" gorm:"type:bigint;not null;index"`
}

// EdgeRequestNonceClaim records every nonce accepted for a logical request.
// A retry may use a fresh nonce while resolving to the same receipt, but that
// nonce must not later authorize a different request.
type EdgeRequestNonceClaim struct {
	ID           int64  `json:"id" gorm:"primaryKey"`
	ReceiptID    int64  `json:"receipt_id" gorm:"not null;index"`
	CredentialID int64  `json:"credential_id" gorm:"not null;uniqueIndex:ux_edge_request_nonce_claim,priority:1"`
	NonceHash    string `json:"nonce_hash" gorm:"type:char(64);not null;uniqueIndex:ux_edge_request_nonce_claim,priority:2"`
	CreatedAt    int64  `json:"created_at" gorm:"type:bigint;not null;index"`
}

func (c *EdgeRequestNonceClaim) BeforeCreate(_ *gorm.DB) error {
	if c == nil || c.ReceiptID <= 0 || c.CredentialID <= 0 {
		return errors.New("invalid edge request nonce claim identity")
	}
	var err error
	c.NonceHash, err = normalizeEdgeControlHash(c.NonceHash)
	if err != nil {
		return fmt.Errorf("invalid nonce hash: %w", err)
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = common.GetTimestamp()
	}
	return nil
}

type EdgeRequestReceiptClaimResult struct {
	Receipt *EdgeRequestReceipt
	Claimed bool
}

func (r *EdgeRequestReceipt) BeforeCreate(_ *gorm.DB) error {
	if r == nil {
		return errors.New("edge request receipt is nil")
	}
	if r.NodeID <= 0 || r.CredentialID <= 0 || r.Generation <= 0 {
		return errors.New("invalid edge request receipt identity")
	}
	r.RequestKind = strings.TrimSpace(r.RequestKind)
	r.IdempotencyKey = strings.TrimSpace(r.IdempotencyKey)
	if r.RequestKind == "" || len(r.RequestKind) > 32 {
		return errors.New("edge request receipt kind is empty")
	}
	if err := edgeauth.ValidateIdempotencyKey(r.IdempotencyKey); err != nil {
		return err
	}
	var err error
	r.NonceHash, err = normalizeEdgeControlHash(r.NonceHash)
	if err != nil {
		return fmt.Errorf("invalid nonce hash: %w", err)
	}
	r.RequestHash, err = normalizeEdgeControlHash(r.RequestHash)
	if err != nil {
		return fmt.Errorf("invalid request hash: %w", err)
	}
	if r.Status == "" {
		r.Status = EdgeRequestReceiptStatusProcessing
	}
	if r.Status != EdgeRequestReceiptStatusProcessing {
		return errors.New("new edge request receipt must be processing")
	}
	now := common.GetTimestamp()
	if r.ExpiresAt <= now {
		return errors.New("edge request receipt expiry must be in the future")
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.UpdatedAt == 0 {
		r.UpdatedAt = now
	}
	return nil
}

// ClaimEdgeRequestReceipt atomically owns both a logical idempotency key and
// every credential nonce accepted for it. The two unique indexes are the
// concurrency primitives across SQLite, MySQL and PostgreSQL.
func ClaimEdgeRequestReceipt(candidate *EdgeRequestReceipt) (*EdgeRequestReceiptClaimResult, error) {
	var claim *EdgeRequestReceiptClaimResult
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		claim, err = ClaimEdgeRequestReceiptTx(tx, candidate)
		return err
	})
	return claim, err
}

func ClaimEdgeRequestReceiptTx(tx *gorm.DB, candidate *EdgeRequestReceipt) (*EdgeRequestReceiptClaimResult, error) {
	if tx == nil {
		return nil, errors.New("database is nil")
	}
	if candidate == nil {
		return nil, errors.New("edge request receipt is nil")
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(candidate)
	if result.Error != nil {
		return nil, result.Error
	}

	claimed := result.RowsAffected == 1
	receipt := candidate
	if !claimed {
		var existing EdgeRequestReceipt
		err := lockForUpdate(tx).
			Where("node_id = ? AND generation = ? AND request_kind = ? AND idempotency_key = ?",
				candidate.NodeID,
				candidate.Generation,
				candidate.RequestKind,
				candidate.IdempotencyKey,
			).
			First(&existing).Error
		if err == nil {
			if existing.RequestHash != candidate.RequestHash {
				return nil, ErrEdgeRequestReceiptIdempotencyConflict
			}
			receipt = &existing
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		} else {
			var nonceOwner EdgeRequestReceipt
			err = lockForUpdate(tx).
				Where("credential_id = ? AND nonce_hash = ?", candidate.CredentialID, candidate.NonceHash).
				First(&nonceOwner).Error
			if err == nil {
				return nil, ErrEdgeRequestReceiptNonceConflict
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, err
			}
			return nil, errors.New("edge request receipt conflicted without a matching idempotency key or nonce")
		}
	}

	nonceClaim := &EdgeRequestNonceClaim{
		ReceiptID:    receipt.ID,
		CredentialID: candidate.CredentialID,
		NonceHash:    candidate.NonceHash,
	}
	nonceResult := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(nonceClaim)
	if nonceResult.Error != nil {
		return nil, nonceResult.Error
	}
	if nonceResult.RowsAffected == 0 {
		var existingNonce EdgeRequestNonceClaim
		if err := lockForUpdate(tx).
			Where("credential_id = ? AND nonce_hash = ?", candidate.CredentialID, candidate.NonceHash).
			First(&existingNonce).Error; err != nil {
			return nil, err
		}
		if existingNonce.ReceiptID != receipt.ID {
			if claimed {
				if err := tx.Delete(&EdgeRequestReceipt{}, receipt.ID).Error; err != nil {
					return nil, err
				}
				candidate.ID = 0
			}
			return nil, ErrEdgeRequestReceiptNonceConflict
		}
	}

	return &EdgeRequestReceiptClaimResult{Receipt: receipt, Claimed: claimed}, nil
}

func CommitEdgeRequestReceipt(receiptID int64, resultRef string, responseStatus int, response any) (bool, error) {
	return CommitEdgeRequestReceiptTx(DB, receiptID, resultRef, responseStatus, response)
}

func CommitEdgeRequestReceiptTx(tx *gorm.DB, receiptID int64, resultRef string, responseStatus int, response any) (bool, error) {
	if responseStatus < 200 || responseStatus > 299 {
		return false, errors.New("committed edge request receipt requires a 2xx response status")
	}
	return finalizeEdgeRequestReceiptTx(tx, receiptID, EdgeRequestReceiptStatusCommitted, resultRef, responseStatus, response)
}

func RejectEdgeRequestReceipt(receiptID int64, responseStatus int, response any) (bool, error) {
	return RejectEdgeRequestReceiptTx(DB, receiptID, responseStatus, response)
}

func RejectEdgeRequestReceiptTx(tx *gorm.DB, receiptID int64, responseStatus int, response any) (bool, error) {
	if responseStatus < 400 || responseStatus > 599 {
		return false, errors.New("rejected edge request receipt requires a 4xx or 5xx response status")
	}
	return finalizeEdgeRequestReceiptTx(tx, receiptID, EdgeRequestReceiptStatusRejected, "", responseStatus, response)
}

func finalizeEdgeRequestReceiptTx(tx *gorm.DB, receiptID int64, status EdgeRequestReceiptStatus, resultRef string, responseStatus int, response any) (bool, error) {
	if tx == nil {
		return false, errors.New("database is nil")
	}
	if receiptID <= 0 {
		return false, errors.New("invalid edge request receipt ID")
	}
	responsePayload := ""
	if response != nil {
		data, err := common.Marshal(response)
		if err != nil {
			return false, err
		}
		responsePayload = string(data)
	}
	resultRef = strings.TrimSpace(resultRef)
	now := common.GetTimestamp()
	result := tx.Model(&EdgeRequestReceipt{}).
		Where("id = ? AND status = ?", receiptID, EdgeRequestReceiptStatusProcessing).
		Updates(map[string]any{
			"status":           status,
			"result_ref":       resultRef,
			"response_status":  responseStatus,
			"response_payload": responsePayload,
			"updated_at":       now,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 1 {
		return true, nil
	}
	var existing EdgeRequestReceipt
	if err := tx.First(&existing, receiptID).Error; err != nil {
		return false, err
	}
	if existing.Status == status &&
		existing.ResultRef == resultRef &&
		existing.ResponseStatus == responseStatus &&
		existing.ResponsePayload == responsePayload {
		return false, nil
	}
	return false, ErrEdgeRequestReceiptAlreadyFinalized
}

func (r *EdgeRequestReceipt) DecodeResponse(out any) error {
	if r == nil || strings.TrimSpace(r.ResponsePayload) == "" {
		return nil
	}
	return common.UnmarshalJsonStr(r.ResponsePayload, out)
}

type EdgePolicySnapshot struct {
	ID            int64                    `json:"version" gorm:"primaryKey;index:idx_edge_policy_kind_status_version,priority:3"`
	Kind          string                   `json:"kind" gorm:"type:varchar(32);not null;index:idx_edge_policy_kind_status_version,priority:1"`
	Format        string                   `json:"format" gorm:"type:varchar(16);not null"`
	BaseVersion   int64                    `json:"base_version" gorm:"type:bigint;not null"`
	SchemaVersion int                      `json:"schema_version" gorm:"not null"`
	Payload       string                   `json:"payload" gorm:"type:text;not null"`
	PayloadHash   string                   `json:"payload_hash" gorm:"type:char(64);not null;index"`
	Signature     string                   `json:"signature" gorm:"type:text;not null"`
	SigningKeyID  string                   `json:"signing_key_id" gorm:"type:varchar(64);not null"`
	Status        EdgePolicySnapshotStatus `json:"status" gorm:"type:varchar(32);not null;index:idx_edge_policy_kind_status_version,priority:2"`
	CreatedAt     int64                    `json:"created_at" gorm:"type:bigint;not null;index"`
	PublishedAt   int64                    `json:"published_at" gorm:"type:bigint;not null"`
	RetiredAt     int64                    `json:"retired_at" gorm:"type:bigint;not null"`
	UpdatedAt     int64                    `json:"updated_at" gorm:"type:bigint;not null"`
}

func NewEdgePolicySnapshot(kind string, format string, schemaVersion int, baseVersion int64, payload any) (*EdgePolicySnapshot, error) {
	kind = strings.TrimSpace(kind)
	format = strings.TrimSpace(format)
	if kind == "" || format == "" || schemaVersion <= 0 || baseVersion < 0 {
		return nil, errors.New("invalid edge policy snapshot metadata")
	}
	data, err := common.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &EdgePolicySnapshot{
		Kind:          kind,
		Format:        format,
		BaseVersion:   baseVersion,
		SchemaVersion: schemaVersion,
		Payload:       string(data),
		PayloadHash:   edgePayloadHash(data),
		Status:        EdgePolicySnapshotStatusDraft,
	}, nil
}

func (s *EdgePolicySnapshot) BeforeCreate(_ *gorm.DB) error {
	if s == nil {
		return errors.New("edge policy snapshot is nil")
	}
	s.Kind = strings.TrimSpace(s.Kind)
	s.Format = strings.TrimSpace(s.Format)
	if s.Kind == "" || s.Format == "" || s.SchemaVersion <= 0 || s.BaseVersion < 0 {
		return errors.New("invalid edge policy snapshot metadata")
	}
	if s.Status == "" {
		s.Status = EdgePolicySnapshotStatusDraft
	}
	if s.Status != EdgePolicySnapshotStatusDraft {
		return errors.New("new edge policy snapshot must be draft")
	}
	if !s.Status.Valid() {
		return ErrInvalidEdgePolicySnapshotStatus
	}
	s.PayloadHash = edgePayloadHash([]byte(s.Payload))
	s.Signature = ""
	s.SigningKeyID = ""
	s.PublishedAt = 0
	s.RetiredAt = 0
	now := common.GetTimestamp()
	if s.CreatedAt == 0 {
		s.CreatedAt = now
	}
	if s.UpdatedAt == 0 {
		s.UpdatedAt = now
	}
	return nil
}

// BeforeUpdate prevents callers from mutating immutable snapshot content after
// publication, including through Save. Updates must target a loaded model so
// the hook can compare the persisted immutable fields.
func (s *EdgePolicySnapshot) BeforeUpdate(tx *gorm.DB) error {
	if s == nil || s.ID <= 0 {
		return ErrEdgePolicySnapshotUpdateRequiresModel
	}
	var existing EdgePolicySnapshot
	if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).First(&existing, s.ID).Error; err != nil {
		return err
	}
	if existing.Status == EdgePolicySnapshotStatusDraft {
		if s.Status == EdgePolicySnapshotStatusDraft {
			s.PayloadHash = edgePayloadHash([]byte(s.Payload))
			s.Signature = ""
			s.SigningKeyID = ""
			s.PublishedAt = 0
			s.RetiredAt = 0
			return nil
		}
		if s.Status != EdgePolicySnapshotStatusPublished ||
			strings.TrimSpace(s.Signature) == "" ||
			strings.TrimSpace(s.SigningKeyID) == "" ||
			s.PublishedAt <= 0 ||
			existing.Kind != s.Kind ||
			existing.Format != s.Format ||
			existing.BaseVersion != s.BaseVersion ||
			existing.SchemaVersion != s.SchemaVersion ||
			existing.Payload != s.Payload ||
			existing.PayloadHash != s.PayloadHash ||
			existing.CreatedAt != s.CreatedAt {
			return ErrEdgePolicySnapshotImmutable
		}
		return nil
	}
	if existing.Kind != s.Kind ||
		existing.Format != s.Format ||
		existing.BaseVersion != s.BaseVersion ||
		existing.SchemaVersion != s.SchemaVersion ||
		existing.Payload != s.Payload ||
		existing.PayloadHash != s.PayloadHash ||
		existing.Signature != s.Signature ||
		existing.SigningKeyID != s.SigningKeyID ||
		existing.PublishedAt != s.PublishedAt ||
		existing.CreatedAt != s.CreatedAt {
		return ErrEdgePolicySnapshotImmutable
	}
	if existing.Status == EdgePolicySnapshotStatusPublished && s.Status == EdgePolicySnapshotStatusRetired && s.RetiredAt > 0 {
		return nil
	}
	if existing.Status == s.Status && existing.RetiredAt == s.RetiredAt {
		return nil
	}
	return ErrEdgePolicySnapshotImmutable
}

func (s EdgePolicySnapshotStatus) Valid() bool {
	switch s {
	case EdgePolicySnapshotStatusDraft, EdgePolicySnapshotStatusPublished, EdgePolicySnapshotStatusRetired:
		return true
	default:
		return false
	}
}

func UpdateDraftEdgePolicySnapshot(snapshotID int64, payload any) (bool, error) {
	if snapshotID <= 0 {
		return false, errors.New("invalid edge policy snapshot ID")
	}
	data, err := common.Marshal(payload)
	if err != nil {
		return false, err
	}
	var snapshot EdgePolicySnapshot
	if err := DB.First(&snapshot, snapshotID).Error; err != nil {
		return false, err
	}
	if snapshot.Status != EdgePolicySnapshotStatusDraft {
		return false, ErrEdgePolicySnapshotImmutable
	}
	snapshot.Payload = string(data)
	snapshot.PayloadHash = edgePayloadHash(data)
	snapshot.UpdatedAt = common.GetTimestamp()
	if err := DB.Save(&snapshot).Error; err != nil {
		return false, err
	}
	return true, nil
}

func PublishEdgePolicySnapshot(snapshotID int64, signature string, signingKeyID string, publishedAt int64) (bool, error) {
	signature = strings.TrimSpace(signature)
	signingKeyID = strings.TrimSpace(signingKeyID)
	if snapshotID <= 0 || signature == "" || signingKeyID == "" {
		return false, errors.New("invalid edge policy publication")
	}
	if err := edgeauth.ValidateKeyID(signingKeyID); err != nil {
		return false, err
	}
	if publishedAt <= 0 {
		publishedAt = common.GetTimestamp()
	}
	var published bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		var snapshot EdgePolicySnapshot
		if err := lockForUpdate(tx).First(&snapshot, snapshotID).Error; err != nil {
			return err
		}
		if snapshot.Status == EdgePolicySnapshotStatusPublished {
			if snapshot.Signature == signature && snapshot.SigningKeyID == signingKeyID {
				return nil
			}
			return ErrEdgePolicySnapshotImmutable
		}
		if snapshot.Status != EdgePolicySnapshotStatusDraft {
			return ErrEdgePolicySnapshotImmutable
		}
		snapshot.Signature = signature
		snapshot.SigningKeyID = signingKeyID
		snapshot.Status = EdgePolicySnapshotStatusPublished
		snapshot.PublishedAt = publishedAt
		snapshot.UpdatedAt = publishedAt
		if err := tx.Save(&snapshot).Error; err != nil {
			return err
		}
		published = true
		return nil
	})
	return published, err
}

func RetireEdgePolicySnapshot(snapshotID int64, retiredAt int64) (bool, error) {
	if snapshotID <= 0 {
		return false, errors.New("invalid edge policy snapshot ID")
	}
	if retiredAt <= 0 {
		retiredAt = common.GetTimestamp()
	}
	var snapshot EdgePolicySnapshot
	if err := DB.First(&snapshot, snapshotID).Error; err != nil {
		return false, err
	}
	if snapshot.Status == EdgePolicySnapshotStatusRetired {
		return false, nil
	}
	if snapshot.Status != EdgePolicySnapshotStatusPublished {
		return false, ErrEdgePolicySnapshotImmutable
	}
	snapshot.Status = EdgePolicySnapshotStatusRetired
	snapshot.RetiredAt = retiredAt
	snapshot.UpdatedAt = retiredAt
	if err := DB.Save(&snapshot).Error; err != nil {
		return false, err
	}
	return true, nil
}

func (s *EdgePolicySnapshot) DecodePayload(out any) error {
	if s == nil {
		return errors.New("edge policy snapshot is nil")
	}
	return common.UnmarshalJsonStr(s.Payload, out)
}

func normalizeEdgeControlHash(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || len(value) != sha256.Size*2 {
		return "", ErrInvalidEdgeControlHash
	}
	return value, nil
}

func edgePayloadHash(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
