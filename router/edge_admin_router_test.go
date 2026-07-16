package router

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeAdminRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api")

	require.NotPanics(t, func() {
		registerEdgeAdminRoutes(api)
	})

	wanted := map[string]bool{
		http.MethodGet + " /api/edge/nodes":                         false,
		http.MethodGet + " /api/edge/snapshots/latest":              false,
		http.MethodPost + " /api/edge/nodes":                        false,
		http.MethodPost + " /api/edge/snapshots/publish":            false,
		http.MethodPost + " /api/edge/nodes/:id/status":             false,
		http.MethodPost + " /api/edge/nodes/:id/credentials/rotate": false,
	}
	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, exists := wanted[key]; exists {
			wanted[key] = true
		}
	}
	for route, found := range wanted {
		assert.True(t, found, route)
	}
}
