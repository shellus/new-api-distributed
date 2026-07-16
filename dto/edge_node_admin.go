package dto

type EdgeNodeCreateRequest struct {
	NodeID                       string `json:"node_id"`
	Name                         string `json:"name"`
	Region                       string `json:"region,omitempty"`
	Generation                   int64  `json:"generation,omitempty"`
	MaxOutstandingQuota          int64  `json:"max_outstanding_quota"`
	CredentialExpiresAtUnixMilli int64  `json:"credential_expires_at_unix_milli,omitempty"`
}

type EdgeNodeAdminView struct {
	ID                  int64  `json:"id"`
	NodeID              string `json:"node_id"`
	Name                string `json:"name"`
	Region              string `json:"region,omitempty"`
	Status              string `json:"status"`
	Generation          int64  `json:"generation"`
	ProtocolVersion     string `json:"protocol_version"`
	DeclaredPublicURL   string `json:"declared_public_url,omitempty"`
	SoftwareVersion     string `json:"software_version,omitempty"`
	MaxOutstandingQuota int64  `json:"max_outstanding_quota"`
	LastSeenAtUnixMilli int64  `json:"last_seen_at_unix_milli,omitempty"`
	CreatedAtUnixMilli  int64  `json:"created_at_unix_milli"`
	UpdatedAtUnixMilli  int64  `json:"updated_at_unix_milli"`
}

// EdgeNodeProvisionedCredential contains the edge-side private key exactly
// once. The master persists only its public verification material.
type EdgeNodeProvisionedCredential struct {
	CredentialID       string `json:"credential_id"`
	Algorithm          string `json:"algorithm"`
	PrivateKey         string `json:"private_key"`
	Fingerprint        string `json:"fingerprint"`
	NotBeforeUnixMilli int64  `json:"not_before_unix_milli"`
	ExpiresAtUnixMilli int64  `json:"expires_at_unix_milli,omitempty"`
	ReturnedOnce       bool   `json:"returned_once"`
}

type EdgeNodeCreateResponse struct {
	Node       EdgeNodeAdminView             `json:"node"`
	Credential EdgeNodeProvisionedCredential `json:"credential"`
}

type EdgeNodeStatusUpdateRequest struct {
	Status string `json:"status"`
}

type EdgeNodeCredentialRotateRequest struct {
	ExpiresAtUnixMilli int64 `json:"expires_at_unix_milli,omitempty"`
}
