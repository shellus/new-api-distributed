package router

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestEdgeControlRoutesUseDedicatedNamespace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetEdgeControlRouter(engine)

	wanted := map[string]bool{
		http.MethodPost + " /control/v1/bootstrap":         false,
		http.MethodPost + " /control/v1/heartbeat":         false,
		http.MethodPost + " /control/v1/snapshot/manifest": false,
		http.MethodPost + " /control/v1/snapshot/page":     false,
		http.MethodPost + " /control/v1/settlement/block":  false,
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
