// Package edgesettlement defines the shared, versioned digest for durable
// edge settlement blocks. Both the edge outbox builder and master verifier
// must use this package; a caller-provided digest is never authoritative.
package edgesettlement

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
)

const (
	VersionV1           = "v1"
	HashAlgorithm       = "sha256"
	BlockDigestDomainV1 = "NEWAPI-EDGE-SETTLEMENT-BLOCK-SHA256-V1"
)

var ErrInvalidInput = errors.New("edgesettlement: invalid input")

// canonicalBlockV1 deliberately excludes request_id, transport idempotency,
// nonce, timestamp, signature, block_digest and optional observability passenger
// fields. Those values may change on an HTTP retry or do not participate in
// accounting identity. Node identity and generation are included to prevent
// transplanting an otherwise valid chain to another edge or replacement
// generation.
type canonicalBlockV1 struct {
	ProtocolVersion     string                 `json:"protocol_version"`
	NodeID              string                 `json:"node_id"`
	NodeGeneration      int64                  `json:"node_generation"`
	BlockID             string                 `json:"block_id"`
	PreviousBlockID     string                 `json:"previous_block_id,omitempty"`
	PreviousBlockDigest string                 `json:"previous_block_digest,omitempty"`
	FirstSequence       int64                  `json:"first_sequence"`
	LastSequence        int64                  `json:"last_sequence"`
	CreatedAtUnixMilli  int64                  `json:"created_at_unix_milli"`
	Events              []dto.EdgeUsageEventV1 `json:"events"`
}

// CanonicalBlockV1 returns the exact domain-separated bytes hashed by
// DigestBlockV1. common.Marshal gives deterministic struct field order and
// deterministic string-key map order for AppliedRatios.
func CanonicalBlockV1(nodeID string, generation int64, request dto.EdgeSettlementBlockRequestV1) ([]byte, error) {
	if err := edgeauth.ValidateNodeID(strings.TrimSpace(nodeID)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if generation <= 0 {
		return nil, fmt.Errorf("%w: node generation must be greater than zero", ErrInvalidInput)
	}

	// Reuse the protocol's complete block/event validation without requiring a
	// caller to invent the digest before computing it.
	validated := request
	validated.BlockDigest = strings.Repeat("0", sha256.Size*2)
	if err := validated.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	events := append([]dto.EdgeUsageEventV1(nil), request.Events...)
	for i := range events {
		// First-response timing is an observability passenger field. It must not
		// change the accounting identity or exactly-once settlement digest.
		events[i].FirstResponseAtUnixMilli = nil
	}
	canonicalJSON, err := common.Marshal(canonicalBlockV1{
		ProtocolVersion:     request.Meta.ProtocolVersion,
		NodeID:              nodeID,
		NodeGeneration:      generation,
		BlockID:             request.BlockID,
		PreviousBlockID:     request.PreviousBlockID,
		PreviousBlockDigest: request.PreviousBlockDigest,
		FirstSequence:       request.FirstSequence,
		LastSequence:        request.LastSequence,
		CreatedAtUnixMilli:  request.CreatedAtUnixMilli,
		Events:              events,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: marshal canonical block: %v", ErrInvalidInput, err)
	}
	canonical := make([]byte, 0, len(BlockDigestDomainV1)+1+len(canonicalJSON))
	canonical = append(canonical, BlockDigestDomainV1...)
	canonical = append(canonical, '\n')
	canonical = append(canonical, canonicalJSON...)
	return canonical, nil
}

func DigestBlockV1(nodeID string, generation int64, request dto.EdgeSettlementBlockRequestV1) (string, error) {
	canonical, err := CanonicalBlockV1(nodeID, generation, request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

// SetBlockDigestV1 is the edge-side convenience used immediately before a
// durable block is inserted into the local outbox.
func SetBlockDigestV1(nodeID string, generation int64, request *dto.EdgeSettlementBlockRequestV1) error {
	if request == nil {
		return fmt.Errorf("%w: settlement block is nil", ErrInvalidInput)
	}
	digest, err := DigestBlockV1(nodeID, generation, *request)
	if err != nil {
		return err
	}
	request.BlockDigest = digest
	return nil
}
