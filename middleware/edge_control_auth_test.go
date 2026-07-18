package middleware

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/edgeauth"
	edgeservice "github.com/QuantumNous/new-api/service/edge"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEdgeControlAuthStoresVerifiedPrincipal(t *testing.T) {
	privateKey, node, credential := newEdgeControlMiddlewareFixture(t)
	body := []byte(`{"meta":{"protocol_version":"edge-control.v1","request_id":"request-1"}}`)
	request := newSignedEdgeControlMiddlewareRequest(t, body, privateKey, node, credential)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RequestId())
	engine.POST("/control/v1/bootstrap", EdgeControlAuth(), func(c *gin.Context) {
		principal, ok := common.GetContextKeyType[*edgeservice.ControlPrincipal](c, constant.ContextKeyEdgeControlPrincipal)
		require.True(t, ok)
		assert.Equal(t, node.NodeUID, principal.NodeUID)
		assert.Equal(t, body, principal.RawBody)
		c.Status(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusNoContent, recorder.Code)
}

func TestEdgeControlAuthRejectsInvalidMediaEncodingSignatureAndSize(t *testing.T) {
	privateKey, node, credential := newEdgeControlMiddlewareFixture(t)
	validBody := []byte(`{"meta":{"protocol_version":"edge-control.v1","request_id":"request-2"}}`)

	tests := []struct {
		name       string
		mutate     func(*http.Request)
		body       []byte
		statusCode int
		code       dto.EdgeControlErrorCodeV1
	}{
		{
			name: "content type",
			mutate: func(request *http.Request) {
				request.Header.Set("Content-Type", "text/plain")
			},
			body:       validBody,
			statusCode: http.StatusUnsupportedMediaType,
			code:       dto.EdgeControlErrorCodeInvalidRequestV1,
		},
		{
			name: "content encoding",
			mutate: func(request *http.Request) {
				request.Header.Set("Content-Encoding", "gzip")
			},
			body:       validBody,
			statusCode: http.StatusUnsupportedMediaType,
			code:       dto.EdgeControlErrorCodeInvalidRequestV1,
		},
		{
			name: "signature",
			mutate: func(request *http.Request) {
				request.Header.Set(edgeauth.HeaderSignature, strings.Repeat("A", 88))
			},
			body:       validBody,
			statusCode: http.StatusUnauthorized,
			code:       dto.EdgeControlErrorCodeInvalidSignatureV1,
		},
		{
			name:       "body limit",
			body:       bytes.Repeat([]byte{'x'}, int(edgeControlMaxRequestBodyBytes)+1),
			statusCode: http.StatusRequestEntityTooLarge,
			code:       dto.EdgeControlErrorCodeInvalidRequestV1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newSignedEdgeControlMiddlewareRequest(t, validBody, privateKey, node, credential)
			request.Body = io.NopCloser(bytes.NewReader(test.body))
			request.ContentLength = int64(len(test.body))
			if test.mutate != nil {
				test.mutate(request)
			}

			gin.SetMode(gin.TestMode)
			engine := gin.New()
			engine.Use(RequestId())
			engine.POST("/control/v1/bootstrap", EdgeControlAuth(), func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, test.statusCode, recorder.Code)
			assert.Contains(t, recorder.Body.String(), `"code":"`+string(test.code)+`"`)
			assert.Contains(t, recorder.Body.String(), `"server_request_id"`)
			assert.NotContains(t, recorder.Body.String(), `"request_id"`)
		})
	}
}

func newEdgeControlMiddlewareFixture(t *testing.T) (ed25519.PrivateKey, *model.EdgeNode, *model.EdgeNodeCredential) {
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

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	verifyMaterial, err := edgeauth.EncodePublicKey(publicKey)
	require.NoError(t, err)
	now := time.Now().Unix()
	node := &model.EdgeNode{
		NodeUID:    "edge.middleware",
		Name:       "Middleware",
		Status:     model.EdgeNodeStatusActive,
		Generation: 1,
	}
	require.NoError(t, db.Create(node).Error)
	credential := &model.EdgeNodeCredential{
		CredentialUID:  "edge-key-middleware",
		NodeID:         node.ID,
		Generation:     node.Generation,
		VerifyMaterial: verifyMaterial,
		Status:         model.EdgeNodeCredentialStatusActive,
		NotBefore:      now - 60,
		ExpiresAt:      now + 3600,
	}
	require.NoError(t, db.Create(credential).Error)
	return privateKey, node, credential
}

func newSignedEdgeControlMiddlewareRequest(t *testing.T, body []byte, privateKey ed25519.PrivateKey, node *model.EdgeNode, credential *model.EdgeNodeCredential) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/control/v1/bootstrap", bytes.NewReader(body))
	request.Header.Set("Content-Type", gin.MIMEJSON)
	require.NoError(t, edgeauth.SignHTTPRequest(request, body, privateKey, edgeauth.Metadata{
		Version:              edgeauth.VersionV1,
		NodeID:               node.NodeUID,
		Generation:           node.Generation,
		KeyID:                credential.CredentialUID,
		TimestampUnixSeconds: time.Now().Unix(),
		Nonce:                "MDEyMzQ1Njc4OWFiY2RlZg",
		IdempotencyKey:       "middleware-request-1",
	}))
	return request
}
