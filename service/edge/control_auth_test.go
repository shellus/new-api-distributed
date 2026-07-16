package edge

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAuthenticateControlRequestVerifiesIdentityAndExactBytes(t *testing.T) {
	db := newControlAuthTestDB(t)
	now := time.Now().Truncate(time.Second)
	node, credential, privateKey := createControlAuthIdentity(t, db, now)
	body := []byte(`{"meta":{"protocol_version":"edge-control.v1","request_id":"request-1"}}`)
	request := signedControlHTTPRequest(t, body, privateKey, edgeauth.Metadata{
		Version:              edgeauth.VersionV1,
		NodeID:               node.NodeUID,
		Generation:           node.Generation,
		KeyID:                credential.CredentialUID,
		TimestampUnixSeconds: now.Unix(),
		Nonce:                "MDEyMzQ1Njc4OWFiY2RlZg",
		IdempotencyKey:       "bootstrap-1",
	})

	principal, err := AuthenticateControlRequest(request, body, now, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, node.ID, principal.NodeID)
	assert.Equal(t, credential.ID, principal.CredentialID)
	assert.Equal(t, body, principal.RawBody)
	assert.NotSame(t, &body[0], &principal.RawBody[0])
	assert.Len(t, principal.RequestHash, 64)
	assert.Len(t, principal.NonceHash, 64)

	body[0] = 'X'
	assert.Equal(t, byte('{'), principal.RawBody[0])
}

func TestAuthenticateControlRequestRejectsTamperUnknownAndRevokedIdentity(t *testing.T) {
	db := newControlAuthTestDB(t)
	now := time.Now().Truncate(time.Second)
	node, credential, privateKey := createControlAuthIdentity(t, db, now)
	body := []byte(`{"meta":{"protocol_version":"edge-control.v1","request_id":"request-2"}}`)
	metadata := edgeauth.Metadata{
		Version:              edgeauth.VersionV1,
		NodeID:               node.NodeUID,
		Generation:           node.Generation,
		KeyID:                credential.CredentialUID,
		TimestampUnixSeconds: now.Unix(),
		Nonce:                "MDEyMzQ1Njc4OWFiY2RlZg",
		IdempotencyKey:       "heartbeat-1",
	}

	t.Run("body tamper", func(t *testing.T) {
		request := signedControlHTTPRequest(t, body, privateKey, metadata)
		_, err := AuthenticateControlRequest(request, []byte(`{"tampered":true}`), now, time.Minute)
		require.Error(t, err)
		assert.ErrorIs(t, err, edgeauth.ErrInvalidSignature)
	})

	t.Run("unknown node", func(t *testing.T) {
		unknown := metadata
		unknown.NodeID = "edge.unknown"
		request := signedControlHTTPRequest(t, body, privateKey, unknown)
		_, err := AuthenticateControlRequest(request, body, now, time.Minute)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrControlAuthentication)
	})

	t.Run("revoked node", func(t *testing.T) {
		require.NoError(t, db.Model(&model.EdgeNode{}).Where("id = ?", node.ID).Update("status", model.EdgeNodeStatusRevoked).Error)
		request := signedControlHTTPRequest(t, body, privateKey, metadata)
		_, err := AuthenticateControlRequest(request, body, now, time.Minute)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrControlNodeRevoked)
	})
}

func TestAuthenticateControlRequestAllowsDisabledNodeButRejectsExpiredCredential(t *testing.T) {
	db := newControlAuthTestDB(t)
	now := time.Now().Truncate(time.Second)
	node, credential, privateKey := createControlAuthIdentity(t, db, now)
	body := []byte(`{"meta":{"protocol_version":"edge-control.v1","request_id":"request-3"}}`)
	metadata := edgeauth.Metadata{
		Version:              edgeauth.VersionV1,
		NodeID:               node.NodeUID,
		Generation:           node.Generation,
		KeyID:                credential.CredentialUID,
		TimestampUnixSeconds: now.Unix(),
		Nonce:                "MDEyMzQ1Njc4OWFiY2RlZg",
		IdempotencyKey:       "bootstrap-3",
	}

	require.NoError(t, db.Model(&model.EdgeNode{}).Where("id = ?", node.ID).Update("status", model.EdgeNodeStatusDisabled).Error)
	request := signedControlHTTPRequest(t, body, privateKey, metadata)
	principal, err := AuthenticateControlRequest(request, body, now, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, model.EdgeNodeStatusDisabled, principal.NodeStatus)

	require.NoError(t, db.Model(&model.EdgeNodeCredential{}).Where("id = ?", credential.ID).Update("expires_at", now.Unix()).Error)
	_, err = AuthenticateControlRequest(request, body, now, time.Minute)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrControlAuthentication)
}

func newControlAuthTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.EdgeNode{}, &model.EdgeNodeCredential{}))
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func createControlAuthIdentity(t *testing.T, db *gorm.DB, now time.Time) (*model.EdgeNode, *model.EdgeNodeCredential, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	verifyMaterial, err := edgeauth.EncodePublicKey(publicKey)
	require.NoError(t, err)
	node := &model.EdgeNode{
		NodeUID:             "edge.control-auth",
		Name:                "Control Auth",
		Status:              model.EdgeNodeStatusActive,
		Generation:          1,
		ProtocolVersion:     dto.EdgeControlProtocolVersionV1,
		MaxOutstandingQuota: 1,
	}
	require.NoError(t, db.Create(node).Error)
	credential := &model.EdgeNodeCredential{
		CredentialUID:  "edge-key-control-auth",
		NodeID:         node.ID,
		Generation:     node.Generation,
		VerifyMaterial: verifyMaterial,
		Status:         model.EdgeNodeCredentialStatusActive,
		NotBefore:      now.Add(-time.Minute).Unix(),
		ExpiresAt:      now.Add(time.Hour).Unix(),
	}
	require.NoError(t, db.Create(credential).Error)
	return node, credential, privateKey
}

func signedControlHTTPRequest(t *testing.T, body []byte, privateKey ed25519.PrivateKey, metadata edgeauth.Metadata) *http.Request {
	t.Helper()
	parsedURL, err := url.Parse("https://master.example/control/v1/bootstrap")
	require.NoError(t, err)
	request := &http.Request{
		Method: http.MethodPost,
		URL:    parsedURL,
		Header: make(http.Header),
	}
	require.NoError(t, edgeauth.SignHTTPRequest(request, body, privateKey, metadata))
	return request
}
