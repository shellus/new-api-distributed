package model

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type testReceiptResponse struct {
	LeaseID string `json:"lease_id"`
}

type testReceiptErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func createTestEdgeNode(t *testing.T, nodeUID string, generation int64) *EdgeNode {
	t.Helper()
	node := &EdgeNode{
		NodeUID:    nodeUID,
		Name:       "Test Edge",
		Status:     EdgeNodeStatusActive,
		Generation: generation,
	}
	require.NoError(t, DB.Create(node).Error)
	return node
}

func testEdgePublicKeyMaterial(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	publicKey := ed25519.PublicKey(bytes.Repeat([]byte{0x42}, ed25519.PublicKeySize))
	encoded, err := edgeauth.EncodePublicKey(publicKey)
	require.NoError(t, err)
	return encoded, publicKey
}

func createTestEdgeCredential(t *testing.T, node *EdgeNode, credentialUID string, now int64) *EdgeNodeCredential {
	t.Helper()
	verifyMaterial, _ := testEdgePublicKeyMaterial(t)
	credential := &EdgeNodeCredential{
		CredentialUID:  credentialUID,
		NodeID:         node.ID,
		Generation:     node.Generation,
		Algorithm:      edgeauth.Algorithm,
		VerifyMaterial: verifyMaterial,
		Status:         EdgeNodeCredentialStatusActive,
		NotBefore:      now - 60,
		ExpiresAt:      now + 3600,
	}
	require.NoError(t, DB.Create(credential).Error)
	return credential
}

func TestEdgeNodePermissionsAndConditionalDeclaration(t *testing.T) {
	truncateTables(t)
	now := common.GetTimestamp()
	node := createTestEdgeNode(t, "edge-node-a", 7)

	assert.True(t, node.CanIssueLease())
	assert.True(t, node.CanAcceptSettlement())

	declaration := EdgeNodeDeclarationUpdate{
		Name:              "Edge A",
		Region:            "ap-southeast-1",
		PublicURL:         "https://edge-a.example",
		SoftwareVersion:   "v1.2.3",
		StartedAt:         now - 60,
		Capabilities:      []dto.EdgeEndpointCapabilityV1{{Endpoint: dto.EdgeEndpointOpenAIChatCompletionsV1, Streaming: true}},
		LastPolicyVersion: 3,
		LastSeenAt:        now + 1,
	}
	updated, err := UpdateEdgeNodeDeclaration(node.NodeUID, 6, declaration)
	require.NoError(t, err)
	assert.False(t, updated)

	updated, err = UpdateEdgeNodeDeclaration(node.NodeUID, node.Generation, declaration)
	require.NoError(t, err)
	assert.True(t, updated)
	require.NoError(t, DB.First(node, node.ID).Error)
	assert.Equal(t, "https://edge-a.example", node.DeclaredPublicURL)
	assert.Equal(t, int64(3), node.LastPolicyVersion)
	assert.Equal(t, "ap-southeast-1", node.Region)
	assert.Equal(t, "v1.2.3", node.SoftwareVersion)
	assert.Equal(t, now-60, node.StartedAt)
	capabilities, err := node.DecodeCapabilities()
	require.NoError(t, err)
	assert.Equal(t, declaration.Capabilities, capabilities)

	require.NoError(t, DB.Model(node).Update("status", EdgeNodeStatusDisabled).Error)
	require.NoError(t, DB.First(node, node.ID).Error)
	assert.False(t, node.CanIssueLease())
	assert.True(t, node.CanAcceptSettlement())
	declaration.PublicURL = "https://edge-a-disabled.example"
	declaration.LastPolicyVersion = 4
	declaration.LastSeenAt = now + 2
	updated, err = UpdateEdgeNodeDeclaration(node.NodeUID, node.Generation, declaration)
	require.NoError(t, err)
	assert.True(t, updated)

	require.NoError(t, DB.Model(node).Update("status", EdgeNodeStatusRevoked).Error)
	require.NoError(t, DB.First(node, node.ID).Error)
	assert.False(t, node.CanIssueLease())
	assert.False(t, node.CanAcceptSettlement())
	declaration.PublicURL = "https://revoked.example"
	declaration.LastPolicyVersion = 5
	declaration.LastSeenAt = now + 3
	updated, err = UpdateEdgeNodeDeclaration(node.NodeUID, node.Generation, declaration)
	require.NoError(t, err)
	assert.False(t, updated)
}

func TestEdgeControlModelsRejectInvalidIdentityAndCounters(t *testing.T) {
	truncateTables(t)

	tests := []struct {
		name string
		node EdgeNode
	}{
		{
			name: "uppercase node UID",
			node: EdgeNode{NodeUID: "Edge-Upper", Name: "Edge", Generation: 1},
		},
		{
			name: "unsupported protocol version",
			node: EdgeNode{NodeUID: "edge-unsupported-protocol", Name: "Edge", Generation: 1, ProtocolVersion: "edge-control.v3"},
		},
		{
			name: "negative accounting cursor",
			node: EdgeNode{NodeUID: "edge-negative-cursor", Name: "Edge", Generation: 1, LastEventSeq: -1},
		},
		{
			name: "negative risk limit",
			node: EdgeNode{NodeUID: "edge-negative-limit", Name: "Edge", Generation: 1, MaxOutstandingQuota: -1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := DB.Create(&test.node).Error
			require.Error(t, err)
		})
	}

	node := createTestEdgeNode(t, "edge-valid-identifiers", 1)
	verifyMaterial, _ := testEdgePublicKeyMaterial(t)
	err := DB.Create(&EdgeNodeCredential{
		CredentialUID:  "Key-Upper",
		NodeID:         node.ID,
		Generation:     node.Generation,
		Algorithm:      edgeauth.Algorithm,
		VerifyMaterial: verifyMaterial,
		Status:         EdgeNodeCredentialStatusActive,
	}).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, edgeauth.ErrInvalidInput)
}

func TestEdgeNodeCredentialValidityAndPublicKeyMaterial(t *testing.T) {
	truncateTables(t)
	now := common.GetTimestamp()
	node := createTestEdgeNode(t, "edge-node-credential", 2)
	verifyMaterial, publicKey := testEdgePublicKeyMaterial(t)
	credential := &EdgeNodeCredential{
		CredentialUID:  "edge-key-1",
		NodeID:         node.ID,
		Generation:     node.Generation,
		Algorithm:      edgeauth.Algorithm,
		VerifyMaterial: verifyMaterial,
		Status:         EdgeNodeCredentialStatusActive,
		NotBefore:      now - 10,
		ExpiresAt:      now + 10,
	}
	require.NoError(t, DB.Create(credential).Error)

	digest := sha256.Sum256(publicKey)
	assert.Equal(t, hex.EncodeToString(digest[:]), credential.Fingerprint)
	assert.NoError(t, credential.ValidateAt(now))
	parsed, err := credential.Ed25519PublicKey()
	require.NoError(t, err)
	assert.Equal(t, publicKey, parsed)

	notYetValid := *credential
	notYetValid.NotBefore = now + 1
	assert.ErrorIs(t, notYetValid.ValidateAt(now), ErrEdgeNodeCredentialNotYetValid)

	expired := *credential
	expired.ExpiresAt = now
	assert.ErrorIs(t, expired.ValidateAt(now), ErrEdgeNodeCredentialExpired)

	retired := *credential
	retired.Status = EdgeNodeCredentialStatusRetired
	assert.ErrorIs(t, retired.ValidateAt(now), ErrEdgeNodeCredentialInactive)

	revoked := *credential
	revoked.Status = EdgeNodeCredentialStatusRevoked
	assert.ErrorIs(t, revoked.ValidateAt(now), ErrEdgeNodeCredentialRevoked)

	invalidMaterial := *credential
	invalidMaterial.VerifyMaterial = "not-an-ed25519-public-key"
	err = invalidMaterial.ValidateAt(now)
	require.Error(t, err)
	assert.ErrorIs(t, err, edgeauth.ErrInvalidPublicKey)
}

func TestGetAndLockEdgeControlIdentityRequireMatchingGenerationAndNode(t *testing.T) {
	truncateTables(t)
	now := common.GetTimestamp()
	node := createTestEdgeNode(t, "edge-node-identity", 12)
	credential := createTestEdgeCredential(t, node, "edge-key-identity", now)

	identity, err := GetEdgeControlIdentity(node.NodeUID, node.Generation, credential.CredentialUID)
	require.NoError(t, err)
	assert.Equal(t, node.ID, identity.Node.ID)
	assert.Equal(t, credential.ID, identity.Credential.ID)

	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		locked, err := LockEdgeControlIdentityTx(tx, node.NodeUID, node.Generation, credential.CredentialUID)
		if err != nil {
			return err
		}
		assert.Equal(t, node.ID, locked.Node.ID)
		assert.Equal(t, credential.ID, locked.Credential.ID)
		return nil
	}))

	_, err = GetEdgeControlIdentity(node.NodeUID, node.Generation+1, credential.CredentialUID)
	require.Error(t, err)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)

	otherNode := createTestEdgeNode(t, "edge-node-identity-other", 12)
	otherPublicKey := ed25519.PublicKey(bytes.Repeat([]byte{0x44}, ed25519.PublicKeySize))
	otherVerifyMaterial, err := edgeauth.EncodePublicKey(otherPublicKey)
	require.NoError(t, err)
	otherCredential := &EdgeNodeCredential{
		CredentialUID:  "edge-key-identity-other",
		NodeID:         otherNode.ID,
		Generation:     otherNode.Generation,
		VerifyMaterial: otherVerifyMaterial,
		Status:         EdgeNodeCredentialStatusActive,
		NotBefore:      now - 60,
		ExpiresAt:      now + 3600,
	}
	require.NoError(t, DB.Create(otherCredential).Error)
	_, err = GetEdgeControlIdentity(node.NodeUID, node.Generation, otherCredential.CredentialUID)
	require.Error(t, err)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestClaimEdgeRequestReceiptRecognizesReplayAndConflict(t *testing.T) {
	truncateTables(t)
	now := common.GetTimestamp()
	node := createTestEdgeNode(t, "edge-node-receipt", 3)
	credential := createTestEdgeCredential(t, node, "edge-key-receipt", now)
	stableRequestHash, err := edgeauth.IdempotencySHA256(edgeauth.Request{
		Method:      "POST",
		EscapedPath: "/api/edge/control/v1/leases",
		RawQuery:    "region=ap-southeast-1",
		Body:        []byte(`{"requested_quota":1000}`),
	})
	require.NoError(t, err)

	candidate := &EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("a", 64),
		RequestKind:    "lease",
		RequestHash:    stableRequestHash,
		IdempotencyKey: "lease-request-1",
		ExpiresAt:      now + 600,
	}
	claim, err := ClaimEdgeRequestReceipt(candidate)
	require.NoError(t, err)
	require.True(t, claim.Claimed)
	require.NotZero(t, claim.Receipt.ID)

	replay := &EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("a", 64),
		RequestKind:    "lease",
		RequestHash:    stableRequestHash,
		IdempotencyKey: "lease-request-1",
		ExpiresAt:      now + 600,
	}
	claim, err = ClaimEdgeRequestReceipt(replay)
	require.NoError(t, err)
	assert.False(t, claim.Claimed)
	assert.Equal(t, candidate.ID, claim.Receipt.ID)

	freshNonceReplay := &EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("d", 64),
		RequestKind:    "lease",
		RequestHash:    stableRequestHash,
		IdempotencyKey: "lease-request-1",
		ExpiresAt:      now + 600,
	}
	claim, err = ClaimEdgeRequestReceipt(freshNonceReplay)
	require.NoError(t, err)
	assert.False(t, claim.Claimed)
	assert.Equal(t, candidate.ID, claim.Receipt.ID)

	rotatedPublicKey := ed25519.PublicKey(bytes.Repeat([]byte{0x43}, ed25519.PublicKeySize))
	rotatedVerifyMaterial, err := edgeauth.EncodePublicKey(rotatedPublicKey)
	require.NoError(t, err)
	rotatedCredential := &EdgeNodeCredential{
		CredentialUID:  "edge-key-receipt-rotated",
		NodeID:         node.ID,
		Generation:     node.Generation,
		Algorithm:      edgeauth.Algorithm,
		VerifyMaterial: rotatedVerifyMaterial,
		Status:         EdgeNodeCredentialStatusActive,
		NotBefore:      now - 60,
		ExpiresAt:      now + 3600,
	}
	require.NoError(t, DB.Create(rotatedCredential).Error)
	credentialRotationReplay := &EdgeRequestReceipt{
		NodeID:       node.ID,
		CredentialID: rotatedCredential.ID,
		Generation:   node.Generation,
		// The stable logical request hash excludes nonce, timestamp, and
		// signature, so it remains unchanged after credential rotation.
		NonceHash:      strings.Repeat("a", 64),
		RequestKind:    "lease",
		RequestHash:    stableRequestHash,
		IdempotencyKey: "lease-request-1",
		ExpiresAt:      now + 600,
	}
	claim, err = ClaimEdgeRequestReceipt(credentialRotationReplay)
	require.NoError(t, err)
	assert.False(t, claim.Claimed)
	assert.Equal(t, candidate.ID, claim.Receipt.ID)
	assert.Equal(t, credential.ID, claim.Receipt.CredentialID)

	idempotencyConflict := &EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("e", 64),
		RequestKind:    "lease",
		RequestHash:    strings.Repeat("c", 64),
		IdempotencyKey: "lease-request-1",
		ExpiresAt:      now + 600,
	}
	_, err = ClaimEdgeRequestReceipt(idempotencyConflict)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeRequestReceiptIdempotencyConflict)

	nonceConflict := &EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("a", 64),
		RequestKind:    "lease",
		RequestHash:    strings.Repeat("f", 64),
		IdempotencyKey: "lease-request-2",
		ExpiresAt:      now + 600,
	}
	_, err = ClaimEdgeRequestReceipt(nonceConflict)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeRequestReceiptNonceConflict)

	aliasNonceConflict := &EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("d", 64),
		RequestKind:    "lease",
		RequestHash:    strings.Repeat("1", 64),
		IdempotencyKey: "lease-request-3",
		ExpiresAt:      now + 600,
	}
	_, err = ClaimEdgeRequestReceipt(aliasNonceConflict)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeRequestReceiptNonceConflict)

	var receiptCount int64
	require.NoError(t, DB.Model(&EdgeRequestReceipt{}).Count(&receiptCount).Error)
	assert.Equal(t, int64(1), receiptCount)
	var nonceCount int64
	require.NoError(t, DB.Model(&EdgeRequestNonceClaim{}).Count(&nonceCount).Error)
	assert.Equal(t, int64(3), nonceCount)
}

func TestClaimEdgeRequestReceiptConcurrentDuplicate(t *testing.T) {
	truncateTables(t)
	now := common.GetTimestamp()
	node := createTestEdgeNode(t, "edge-node-concurrent-receipt", 4)
	credential := createTestEdgeCredential(t, node, "edge-key-concurrent-receipt", now)

	start := make(chan struct{})
	results := make(chan *EdgeRequestReceiptClaimResult, 2)
	errorsCh := make(chan error, 2)
	for i := range 2 {
		go func(nonceByte byte) {
			<-start
			result, err := ClaimEdgeRequestReceipt(&EdgeRequestReceipt{
				NodeID:         node.ID,
				CredentialID:   credential.ID,
				Generation:     node.Generation,
				NonceHash:      strings.Repeat(string(nonceByte), 64),
				RequestKind:    "settlement",
				RequestHash:    strings.Repeat("e", 64),
				IdempotencyKey: "block-1",
				ExpiresAt:      now + 600,
			})
			errorsCh <- err
			results <- result
		}(byte('d' + i))
	}
	close(start)

	claimedCount := 0
	var receiptID int64
	for range 2 {
		require.NoError(t, <-errorsCh)
		result := <-results
		require.NotNil(t, result)
		if result.Claimed {
			claimedCount++
		}
		if receiptID == 0 {
			receiptID = result.Receipt.ID
		} else {
			assert.Equal(t, receiptID, result.Receipt.ID)
		}
	}
	assert.Equal(t, 1, claimedCount)

	var receiptCount int64
	require.NoError(t, DB.Model(&EdgeRequestReceipt{}).Count(&receiptCount).Error)
	assert.Equal(t, int64(1), receiptCount)
	var nonceCount int64
	require.NoError(t, DB.Model(&EdgeRequestNonceClaim{}).Count(&nonceCount).Error)
	assert.Equal(t, int64(2), nonceCount)
}

func TestCommitEdgeRequestReceiptIsIdempotent(t *testing.T) {
	truncateTables(t)
	now := common.GetTimestamp()
	node := createTestEdgeNode(t, "edge-node-receipt-commit", 5)
	credential := createTestEdgeCredential(t, node, "edge-key-receipt-commit", now)
	claim, err := ClaimEdgeRequestReceipt(&EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("f", 64),
		RequestKind:    "lease",
		RequestHash:    strings.Repeat("1", 64),
		IdempotencyKey: "lease-request-commit",
		ExpiresAt:      now + 600,
	})
	require.NoError(t, err)

	response := testReceiptResponse{LeaseID: "lease-1"}
	committed, err := CommitEdgeRequestReceipt(claim.Receipt.ID, response.LeaseID, 200, response)
	require.NoError(t, err)
	assert.True(t, committed)
	committed, err = CommitEdgeRequestReceipt(claim.Receipt.ID, response.LeaseID, 200, response)
	require.NoError(t, err)
	assert.False(t, committed)

	var receipt EdgeRequestReceipt
	require.NoError(t, DB.First(&receipt, claim.Receipt.ID).Error)
	assert.Equal(t, EdgeRequestReceiptStatusCommitted, receipt.Status)
	assert.Equal(t, response.LeaseID, receipt.ResultRef)
	assert.Equal(t, 200, receipt.ResponseStatus)
	var decoded testReceiptResponse
	require.NoError(t, receipt.DecodeResponse(&decoded))
	assert.Equal(t, response, decoded)

	replay, err := ClaimEdgeRequestReceipt(&EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("2", 64),
		RequestKind:    "lease",
		RequestHash:    strings.Repeat("1", 64),
		IdempotencyKey: "lease-request-commit",
		ExpiresAt:      now + 600,
	})
	require.NoError(t, err)
	assert.False(t, replay.Claimed)
	assert.Equal(t, EdgeRequestReceiptStatusCommitted, replay.Receipt.Status)
	assert.Equal(t, 200, replay.Receipt.ResponseStatus)
	decoded = testReceiptResponse{}
	require.NoError(t, replay.Receipt.DecodeResponse(&decoded))
	assert.Equal(t, response, decoded)

	_, err = CommitEdgeRequestReceipt(claim.Receipt.ID, "lease-2", 200, testReceiptResponse{LeaseID: "lease-2"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeRequestReceiptAlreadyFinalized)
	_, err = CommitEdgeRequestReceipt(claim.Receipt.ID, response.LeaseID, 409, response)
	require.Error(t, err)
}

func TestRejectEdgeRequestReceiptIsIdempotentAndReplayable(t *testing.T) {
	truncateTables(t)
	now := common.GetTimestamp()
	node := createTestEdgeNode(t, "edge-node-receipt-reject", 8)
	credential := createTestEdgeCredential(t, node, "edge-key-receipt-reject", now)
	claim, err := ClaimEdgeRequestReceipt(&EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("4", 64),
		RequestKind:    "settlement",
		RequestHash:    strings.Repeat("5", 64),
		IdempotencyKey: "settlement-reject",
		ExpiresAt:      now + 600,
	})
	require.NoError(t, err)

	response := testReceiptErrorResponse{Code: "lease_revoked", Message: "lease is no longer active"}
	rejected, err := RejectEdgeRequestReceipt(claim.Receipt.ID, 409, response)
	require.NoError(t, err)
	assert.True(t, rejected)
	rejected, err = RejectEdgeRequestReceipt(claim.Receipt.ID, 409, response)
	require.NoError(t, err)
	assert.False(t, rejected)

	replay, err := ClaimEdgeRequestReceipt(&EdgeRequestReceipt{
		NodeID:         node.ID,
		CredentialID:   credential.ID,
		Generation:     node.Generation,
		NonceHash:      strings.Repeat("6", 64),
		RequestKind:    "settlement",
		RequestHash:    strings.Repeat("5", 64),
		IdempotencyKey: "settlement-reject",
		ExpiresAt:      now + 600,
	})
	require.NoError(t, err)
	assert.False(t, replay.Claimed)
	assert.Equal(t, EdgeRequestReceiptStatusRejected, replay.Receipt.Status)
	assert.Empty(t, replay.Receipt.ResultRef)
	assert.Equal(t, 409, replay.Receipt.ResponseStatus)
	var decoded testReceiptErrorResponse
	require.NoError(t, replay.Receipt.DecodeResponse(&decoded))
	assert.Equal(t, response, decoded)

	_, err = RejectEdgeRequestReceipt(claim.Receipt.ID, 410, response)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeRequestReceiptAlreadyFinalized)
	_, err = CommitEdgeRequestReceipt(claim.Receipt.ID, "forbidden", 200, testReceiptResponse{LeaseID: "forbidden"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgeRequestReceiptAlreadyFinalized)
	_, err = RejectEdgeRequestReceipt(claim.Receipt.ID, 200, response)
	require.Error(t, err)
}

func TestEdgeRequestReceiptTxRollsBackClaimAndCommitTogether(t *testing.T) {
	truncateTables(t)
	now := common.GetTimestamp()
	node := createTestEdgeNode(t, "edge-node-receipt-tx", 6)
	credential := createTestEdgeCredential(t, node, "edge-key-receipt-tx", now)
	rollbackErr := errors.New("rollback receipt transaction")

	err := DB.Transaction(func(tx *gorm.DB) error {
		claim, err := ClaimEdgeRequestReceiptTx(tx, &EdgeRequestReceipt{
			NodeID:         node.ID,
			CredentialID:   credential.ID,
			Generation:     node.Generation,
			NonceHash:      strings.Repeat("2", 64),
			RequestKind:    "lease",
			RequestHash:    strings.Repeat("3", 64),
			IdempotencyKey: "lease-request-tx",
			ExpiresAt:      now + 600,
		})
		if err != nil {
			return err
		}
		if _, err := CommitEdgeRequestReceiptTx(tx, claim.Receipt.ID, "lease-tx", 200, testReceiptResponse{LeaseID: "lease-tx"}); err != nil {
			return err
		}
		return rollbackErr
	})
	require.ErrorIs(t, err, rollbackErr)

	var count int64
	require.NoError(t, DB.Model(&EdgeRequestReceipt{}).
		Where("credential_id = ? AND nonce_hash = ?", credential.ID, strings.Repeat("2", 64)).
		Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, DB.Model(&EdgeRequestNonceClaim{}).
		Where("credential_id = ? AND nonce_hash = ?", credential.ID, strings.Repeat("2", 64)).
		Count(&count).Error)
	assert.Zero(t, count)
}

func TestPublishedEdgePolicySnapshotIsImmutable(t *testing.T) {
	truncateTables(t)
	now := common.GetTimestamp()
	snapshot, err := NewEdgePolicySnapshot("business", "full", 1, 0, map[string]any{"model": "gpt-test"})
	require.NoError(t, err)
	require.NoError(t, DB.Create(snapshot).Error)

	updated, err := UpdateDraftEdgePolicySnapshot(snapshot.ID, map[string]any{"model": "gpt-updated"})
	require.NoError(t, err)
	assert.True(t, updated)
	require.NoError(t, DB.First(snapshot, snapshot.ID).Error)
	var payload map[string]any
	require.NoError(t, snapshot.DecodePayload(&payload))
	assert.Equal(t, "gpt-updated", payload["model"])

	published, err := PublishEdgePolicySnapshot(snapshot.ID, " signature-1 ", " master-key-1 ", now)
	require.NoError(t, err)
	assert.True(t, published)
	published, err = PublishEdgePolicySnapshot(snapshot.ID, "signature-1", "master-key-1", now)
	require.NoError(t, err)
	assert.False(t, published)

	updated, err = UpdateDraftEdgePolicySnapshot(snapshot.ID, map[string]any{"model": "forbidden"})
	require.Error(t, err)
	assert.False(t, updated)
	assert.ErrorIs(t, err, ErrEdgePolicySnapshotImmutable)

	require.NoError(t, DB.First(snapshot, snapshot.ID).Error)
	originalPayload := snapshot.Payload
	snapshot.Payload = `{"model":"forbidden-save"}`
	err = DB.Save(snapshot).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgePolicySnapshotImmutable)
	require.NoError(t, DB.First(snapshot, snapshot.ID).Error)
	assert.Equal(t, originalPayload, snapshot.Payload)

	retired, err := RetireEdgePolicySnapshot(snapshot.ID, now+1)
	require.NoError(t, err)
	assert.True(t, retired)
	require.NoError(t, DB.First(snapshot, snapshot.ID).Error)
	assert.Equal(t, EdgePolicySnapshotStatusRetired, snapshot.Status)

	snapshot.RetiredAt = now + 2
	err = DB.Save(snapshot).Error
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEdgePolicySnapshotImmutable)
	require.NoError(t, DB.First(snapshot, snapshot.ID).Error)
	assert.Equal(t, now+1, snapshot.RetiredAt)

	snapshot.Kind = "auth"
	err = DB.Save(snapshot).Error
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEdgePolicySnapshotImmutable))
}
