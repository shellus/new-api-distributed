package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeRouterExposesSharedDataPlaneRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetEdgeRouter(engine)

	routes := engine.Routes()
	registered := make(map[string]bool, len(routes))
	for _, route := range routes {
		registered[route.Method+" "+route.Path] = true
	}
	assert.True(t, registered[http.MethodGet+" /healthz"])
	assert.True(t, registered[http.MethodGet+" /readyz"])
	assert.True(t, registered[http.MethodPost+" /v1/chat/completions"])
	assert.True(t, registered[http.MethodPost+" /v1/responses"])
	assert.True(t, registered[http.MethodPost+" /v1/responses/compact"])
	assert.True(t, registered[http.MethodPost+" /v1/images/generations"])
	assert.True(t, registered[http.MethodPost+" /v1/audio/transcriptions"])
	assert.True(t, registered[http.MethodPost+" /v1/embeddings"])
	assert.True(t, registered[http.MethodPost+" /v1/messages"])
	assert.True(t, registered[http.MethodPost+" /v1beta/models/*path"])
	assert.True(t, registered[http.MethodGet+" /v1/realtime"])

	healthRecorder := httptest.NewRecorder()
	healthRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	engine.ServeHTTP(healthRecorder, healthRequest)
	require.Equal(t, http.StatusOK, healthRecorder.Code)

	var healthResponse struct {
		Success bool `json:"success"`
		Data    struct {
			Mode common.RuntimeMode `json:"mode"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(healthRecorder.Body.Bytes(), &healthResponse))
	assert.True(t, healthResponse.Success)
	assert.Equal(t, common.RuntimeModeEdge, healthResponse.Data.Mode)

	readyRecorder := httptest.NewRecorder()
	readyRequest := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	engine.ServeHTTP(readyRecorder, readyRequest)
	assert.Equal(t, http.StatusServiceUnavailable, readyRecorder.Code)

	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/responses/compact", "/v1/images/generations"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, nil)
		engine.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	}

	for _, requestSpec := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/"},
		{method: http.MethodGet, path: "/api/status"},
		{method: http.MethodPost, path: "/api/user/login"},
	} {
		t.Run(requestSpec.method+" "+requestSpec.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(requestSpec.method, requestSpec.path, nil)
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, http.StatusNotFound, recorder.Code)
		})
	}
}

func TestEdgeRouterMatchesMasterUserDataPlaneRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	master := gin.New()
	SetRelayRouter(master)
	SetVideoRouter(master)
	edge := gin.New()
	SetEdgeRouter(edge)

	dataPlaneRoutes := func(engine *gin.Engine) map[string]struct{} {
		result := make(map[string]struct{})
		for _, route := range engine.Routes() {
			path := route.Path
			if path == "/healthz" || path == "/readyz" || len(path) >= 3 && path[:3] == "/pg" {
				continue
			}
			result[route.Method+" "+path] = struct{}{}
		}
		return result
	}

	assert.Equal(t, dataPlaneRoutes(master), dataPlaneRoutes(edge))
}
