package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

// SetEdgeControlRouter must run before SetRelayRouter, whose engine-level
// middleware is intentionally limited to public data-plane routes.
func SetEdgeControlRouter(router *gin.Engine) {
	control := router.Group("/control/v1")
	control.Use(middleware.DisableCache(), middleware.EdgeControlAuth())
	{
		control.POST("/bootstrap", controller.EdgeControlBootstrap)
		control.POST("/heartbeat", controller.EdgeControlHeartbeat)
		control.POST("/snapshot/manifest", controller.EdgeControlSnapshotManifest)
		control.POST("/snapshot/page", controller.EdgeControlSnapshotPage)
		control.POST("/lease/acquire", controller.EdgeControlLeaseAcquire)
		control.POST("/lease/close", controller.EdgeControlLeaseClose)
		control.POST("/settlement/block", controller.EdgeControlSettlementBlock)
	}
}
