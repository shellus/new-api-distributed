package edge

import (
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type controlMutationTestResponse struct {
	RequestID string `json:"request_id"`
	Value     int64  `json:"value"`
}

func TestExecuteControlMutationPersistsAndReplaysExactResponse(t *testing.T) {
	db, principal := newControlMutationFixture(t)
	callbackCount := 0
	mutate := func(tx *gorm.DB, identity *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		callbackCount++
		updatedAt := int64(1_784_160_123)
		if err := tx.Model(&model.EdgeNode{}).Where("id = ?", identity.Node.ID).Update("last_seen_at", updatedAt).Error; err != nil {
			return nil, err
		}
		return &ControlMutationResult{
			StatusCode: 200,
			ResultRef:  "bootstrap-accepted",
			Response: controlMutationTestResponse{
				RequestID: "request-1",
				Value:     updatedAt,
			},
		}, nil
	}

	first, err := ExecuteControlMutation(principal, "bootstrap", time.Hour, mutate)
	require.NoError(t, err)
	assert.False(t, first.Replayed)
	assert.Equal(t, `{"request_id":"request-1","value":1784160123}`, string(first.Body))
	assert.Equal(t, 1, callbackCount)

	retry := *principal
	retry.SignedRequest = &edgeauth.SignedHTTPRequest{
		Metadata: principal.SignedRequest.Metadata,
		Request:  principal.SignedRequest.Request,
	}
	retry.SignedRequest.Metadata.Nonce = "ZmVkY2JhOTg3NjU0MzIxMA"
	retry.NonceHash = edgeauth.BodySHA256([]byte(retry.SignedRequest.Metadata.Nonce))
	replayed, err := ExecuteControlMutation(&retry, "bootstrap", time.Hour, mutate)
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)
	assert.Equal(t, first.StatusCode, replayed.StatusCode)
	assert.Equal(t, first.Body, replayed.Body)
	assert.Equal(t, 1, callbackCount)

	var receiptCount int64
	var nonceCount int64
	require.NoError(t, db.Model(&model.EdgeRequestReceipt{}).Count(&receiptCount).Error)
	require.NoError(t, db.Model(&model.EdgeRequestNonceClaim{}).Count(&nonceCount).Error)
	assert.Equal(t, int64(1), receiptCount)
	assert.Equal(t, int64(2), nonceCount)
}

func TestExecuteControlMutationPersistsTrustedRejection(t *testing.T) {
	db, principal := newControlMutationFixture(t)
	mutate := func(tx *gorm.DB, identity *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		require.NoError(t, tx.Model(&model.EdgeNode{}).Where("id = ?", identity.Node.ID).
			Update("last_seen_at", int64(99)).Error)
		return &ControlMutationResult{
			StatusCode: 400,
			Response: map[string]any{
				"error": "invalid declaration",
			},
		}, nil
	}

	first, err := ExecuteControlMutation(principal, "heartbeat", time.Hour, mutate)
	require.NoError(t, err)
	assert.Equal(t, 400, first.StatusCode)
	var node model.EdgeNode
	require.NoError(t, db.First(&node, principal.NodeID).Error)
	assert.Zero(t, node.LastSeenAt)

	replay, err := ExecuteControlMutation(principal, "heartbeat", time.Hour, mutate)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, first.Body, replay.Body)

	var receipt model.EdgeRequestReceipt
	require.NoError(t, db.First(&receipt).Error)
	assert.Equal(t, model.EdgeRequestReceiptStatusRejected, receipt.Status)
	assert.Equal(t, 400, receipt.ResponseStatus)
}

func TestExecuteControlMutationRollsBackReceiptAndDomainMutationOnError(t *testing.T) {
	db, principal := newControlMutationFixture(t)
	rollbackErr := errors.New("rollback mutation")
	_, err := ExecuteControlMutation(principal, "bootstrap", time.Hour, func(tx *gorm.DB, identity *model.EdgeControlIdentity) (*ControlMutationResult, error) {
		require.NoError(t, tx.Model(&model.EdgeNode{}).Where("id = ?", identity.Node.ID).Update("last_seen_at", int64(99)).Error)
		return nil, rollbackErr
	})
	require.ErrorIs(t, err, rollbackErr)

	var node model.EdgeNode
	require.NoError(t, db.First(&node, principal.NodeID).Error)
	assert.Zero(t, node.LastSeenAt)
	var receiptCount int64
	var nonceCount int64
	require.NoError(t, db.Model(&model.EdgeRequestReceipt{}).Count(&receiptCount).Error)
	require.NoError(t, db.Model(&model.EdgeRequestNonceClaim{}).Count(&nonceCount).Error)
	assert.Zero(t, receiptCount)
	assert.Zero(t, nonceCount)
}

func newControlMutationFixture(t *testing.T) (*gorm.DB, *ControlPrincipal) {
	t.Helper()
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.EdgeNode{},
		&model.EdgeNodeCredential{},
		&model.EdgeRequestReceipt{},
		&model.EdgeRequestNonceClaim{},
		&model.EdgeNodeHeartbeat{},
		&model.EdgeCompiledSnapshot{},
		&model.EdgeCompiledSnapshotDataset{},
		&model.EdgeCompiledSnapshotPage{},
	))
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	now := common.GetTimestamp()
	node := &model.EdgeNode{
		NodeUID:             "edge.control-mutation",
		Name:                "Control Mutation",
		Status:              model.EdgeNodeStatusActive,
		Generation:          1,
		MaxOutstandingQuota: 1,
	}
	require.NoError(t, db.Create(node).Error)
	verifyMaterial, err := edgeauth.EncodePublicKey(make([]byte, 32))
	require.NoError(t, err)
	credential := &model.EdgeNodeCredential{
		CredentialUID:  "edge-key-control-mutation",
		NodeID:         node.ID,
		Generation:     node.Generation,
		VerifyMaterial: verifyMaterial,
		Status:         model.EdgeNodeCredentialStatusActive,
		NotBefore:      now - 60,
		ExpiresAt:      now + 3600,
	}
	require.NoError(t, db.Create(credential).Error)
	request := edgeauth.Request{
		Method:      "POST",
		EscapedPath: "/control/v1/bootstrap",
		Body:        []byte(`{"meta":{"request_id":"request-1"}}`),
	}
	requestHash, err := edgeauth.IdempotencySHA256(request)
	require.NoError(t, err)
	metadata := edgeauth.Metadata{
		Version:              edgeauth.VersionV1,
		NodeID:               node.NodeUID,
		Generation:           node.Generation,
		KeyID:                credential.CredentialUID,
		TimestampUnixSeconds: now,
		Nonce:                "MDEyMzQ1Njc4OWFiY2RlZg",
		IdempotencyKey:       "request-1",
	}
	return db, &ControlPrincipal{
		NodeID:                node.ID,
		NodeUID:               node.NodeUID,
		NodeStatus:            node.Status,
		Generation:            node.Generation,
		CredentialID:          credential.ID,
		CredentialUID:         credential.CredentialUID,
		CredentialFingerprint: credential.Fingerprint,
		SignedRequest: &edgeauth.SignedHTTPRequest{
			Metadata: metadata,
			Request:  request,
		},
		RawBody:     request.Body,
		RequestHash: requestHash,
		NonceHash:   edgeauth.BodySHA256([]byte(metadata.Nonce)),
	}
}
