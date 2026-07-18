package edge

import (
	"crypto/ed25519"
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

func TestCreateNodeStoresOnlyPublicCredentialMaterial(t *testing.T) {
	db := newNodeAdminTestDB(t)
	expiresAt := time.Now().Add(time.Hour).Truncate(time.Second)

	response, err := CreateNode(dto.EdgeNodeCreateRequest{
		NodeID:                       "edge.pro20x4.sg",
		Name:                         "Singapore Pro20x4",
		Region:                       "ap-southeast-1",
		CredentialExpiresAtUnixMilli: expiresAt.UnixMilli(),
	})
	require.NoError(t, err)
	assert.Equal(t, "edge.pro20x4.sg", response.Node.NodeID)
	assert.Equal(t, int64(1), response.Node.Generation)
	assert.Equal(t, dto.EdgeControlProtocolVersionV2, response.Node.ProtocolVersion)
	assert.Equal(t, string(model.EdgeNodeStatusActive), response.Node.Status)
	assert.Equal(t, edgeauth.Algorithm, response.Credential.Algorithm)
	assert.True(t, response.Credential.ReturnedOnce)
	assert.Equal(t, expiresAt.UnixMilli(), response.Credential.ExpiresAtUnixMilli)

	privateKey, err := edgeauth.ParsePrivateKey(response.Credential.PrivateKey)
	require.NoError(t, err)
	derivedPublicKey := privateKey.Public().(ed25519.PublicKey)

	var storedCredential model.EdgeNodeCredential
	require.NoError(t, db.Where("credential_uid = ?", response.Credential.CredentialID).First(&storedCredential).Error)
	storedPublicKey, err := storedCredential.Ed25519PublicKey()
	require.NoError(t, err)
	assert.Equal(t, derivedPublicKey, storedPublicKey)
	assert.Equal(t, response.Credential.Fingerprint, storedCredential.Fingerprint)
	assert.NotEqual(t, response.Credential.PrivateKey, storedCredential.VerifyMaterial)

	var nodeCount int64
	var credentialCount int64
	require.NoError(t, db.Model(&model.EdgeNode{}).Count(&nodeCount).Error)
	require.NoError(t, db.Model(&model.EdgeNodeCredential{}).Count(&credentialCount).Error)
	assert.Equal(t, int64(1), nodeCount)
	assert.Equal(t, int64(1), credentialCount)
}

func TestCreateNodeIsTransactionalOnDuplicateNodeID(t *testing.T) {
	db := newNodeAdminTestDB(t)
	request := dto.EdgeNodeCreateRequest{
		NodeID: "edge.duplicate",
		Name:   "Duplicate",
	}

	_, err := CreateNode(request)
	require.NoError(t, err)
	_, err = CreateNode(request)
	require.Error(t, err)

	var nodeCount int64
	var credentialCount int64
	require.NoError(t, db.Model(&model.EdgeNode{}).Count(&nodeCount).Error)
	require.NoError(t, db.Model(&model.EdgeNodeCredential{}).Count(&credentialCount).Error)
	assert.Equal(t, int64(1), nodeCount)
	assert.Equal(t, int64(1), credentialCount)
}

func TestCreateNodeRejectsExpiredCredential(t *testing.T) {
	newNodeAdminTestDB(t)
	_, err := CreateNode(dto.EdgeNodeCreateRequest{
		NodeID:                       "edge.expired",
		Name:                         "Expired",
		CredentialExpiresAtUnixMilli: time.Now().Add(-time.Minute).UnixMilli(),
	})
	require.Error(t, err)
}

func TestNodeStatusAndCredentialRotationLifecycle(t *testing.T) {
	db := newNodeAdminTestDB(t)
	created, err := CreateNode(dto.EdgeNodeCreateRequest{
		NodeID: "edge.lifecycle",
		Name:   "Lifecycle",
	})
	require.NoError(t, err)

	disabled, err := UpdateNodeStatus(created.Node.ID, model.EdgeNodeStatusDisabled)
	require.NoError(t, err)
	assert.Equal(t, string(model.EdgeNodeStatusDisabled), disabled.Status)

	rotated, err := RotateNodeCredential(created.Node.ID, dto.EdgeNodeCredentialRotateRequest{})
	require.NoError(t, err)
	assert.NotEqual(t, created.Credential.CredentialID, rotated.CredentialID)
	_, err = edgeauth.ParsePrivateKey(rotated.PrivateKey)
	require.NoError(t, err)

	var credentials []model.EdgeNodeCredential
	require.NoError(t, db.Where("node_id = ?", created.Node.ID).Order("id asc").Find(&credentials).Error)
	require.Len(t, credentials, 2)
	assert.Equal(t, model.EdgeNodeCredentialStatusRetired, credentials[0].Status)
	assert.Equal(t, model.EdgeNodeCredentialStatusActive, credentials[1].Status)

	revoked, err := UpdateNodeStatus(created.Node.ID, model.EdgeNodeStatusRevoked)
	require.NoError(t, err)
	assert.Equal(t, string(model.EdgeNodeStatusRevoked), revoked.Status)
	require.NoError(t, db.Where("node_id = ?", created.Node.ID).Order("id asc").Find(&credentials).Error)
	for _, credential := range credentials {
		assert.Equal(t, model.EdgeNodeCredentialStatusRevoked, credential.Status)
		assert.Positive(t, credential.RevokedAt)
	}

	_, err = RotateNodeCredential(created.Node.ID, dto.EdgeNodeCredentialRotateRequest{})
	require.ErrorIs(t, err, ErrControlNodeRevoked)
	_, err = UpdateNodeStatus(created.Node.ID, model.EdgeNodeStatusActive)
	require.Error(t, err)

	nodes, err := ListNodes()
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Equal(t, string(model.EdgeNodeStatusRevoked), nodes[0].Status)
}

func newNodeAdminTestDB(t *testing.T) *gorm.DB {
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
