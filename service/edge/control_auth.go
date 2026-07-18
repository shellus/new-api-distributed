package edge

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"gorm.io/gorm"
)

var (
	ErrControlAuthentication = errors.New("edge control authentication failed")
	ErrControlNodeRevoked    = errors.New("edge control node is revoked")
	ErrControlProtocol       = errors.New("edge control node protocol mismatch")
)

type ControlPrincipal struct {
	NodeID                int64
	NodeUID               string
	NodeStatus            model.EdgeNodeStatus
	Generation            int64
	CredentialID          int64
	CredentialUID         string
	CredentialFingerprint string
	SignedRequest         *edgeauth.SignedHTTPRequest
	RawBody               []byte
	RequestHash           string
	NonceHash             string
}

// AuthenticateControlRequest verifies the exact request bytes before any DTO
// decoding. Persistent nonce/idempotency ownership is intentionally deferred
// to the endpoint's mutation transaction.
func AuthenticateControlRequest(request *http.Request, rawBody []byte, now time.Time, maxClockSkew time.Duration) (*ControlPrincipal, error) {
	body := append([]byte(nil), rawBody...)
	signedRequest, err := edgeauth.ParseHTTPRequest(request, body)
	if err != nil {
		return nil, err
	}

	identity, err := model.GetEdgeControlIdentity(
		signedRequest.Metadata.NodeID,
		signedRequest.Metadata.Generation,
		signedRequest.Metadata.KeyID,
	)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrControlAuthentication
		}
		return nil, fmt.Errorf("load edge control identity: %w", err)
	}
	if identity.Node.Status == model.EdgeNodeStatusRevoked {
		return nil, ErrControlNodeRevoked
	}
	if !model.SupportedEdgeControlProtocolVersion(identity.Node.ProtocolVersion) {
		return nil, ErrControlProtocol
	}
	if err := identity.Credential.ValidateAt(now.Unix()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrControlAuthentication, err)
	}
	publicKey, err := identity.Credential.Ed25519PublicKey()
	if err != nil {
		return nil, fmt.Errorf("%w: invalid verification material", ErrControlAuthentication)
	}
	if err := signedRequest.Verify(publicKey, edgeauth.VerifyOptions{
		Now:          now,
		MaxClockSkew: maxClockSkew,
	}); err != nil {
		return nil, err
	}

	requestHash, err := edgeauth.IdempotencySHA256(signedRequest.Request)
	if err != nil {
		return nil, err
	}
	return &ControlPrincipal{
		NodeID:                identity.Node.ID,
		NodeUID:               identity.Node.NodeUID,
		NodeStatus:            identity.Node.Status,
		Generation:            identity.Node.Generation,
		CredentialID:          identity.Credential.ID,
		CredentialUID:         identity.Credential.CredentialUID,
		CredentialFingerprint: identity.Credential.Fingerprint,
		SignedRequest:         signedRequest,
		RawBody:               body,
		RequestHash:           requestHash,
		NonceHash:             edgeauth.BodySHA256([]byte(signedRequest.Metadata.Nonce)),
	}, nil
}
