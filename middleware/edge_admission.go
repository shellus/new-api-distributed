package middleware

import (
	"net/http"

	edgeservice "github.com/QuantumNous/new-api/service/edge"

	"github.com/gin-gonic/gin"
)

// EdgeRequestAdmission rejects before authentication, channel selection,
// lease reservation or CPA access unless the verified snapshot is currently
// valid and the application is accepting new data-plane work.
func EdgeRequestAdmission() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !edgeservice.BeginEdgeRequest(c) {
			abortWithOpenAiMessage(c, http.StatusServiceUnavailable, "edge is not ready")
			return
		}
		defer edgeservice.EndEdgeRequest(c)
		c.Next()
	}
}
