package router

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	edgeservice "github.com/QuantumNous/new-api/service/edge"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func SetEdgeRouter(router *gin.Engine) {
	router.Use(middleware.CORS())
	router.Use(middleware.DecompressRequestMiddleware())
	router.Use(middleware.BodyStorageCleanup())
	router.Use(middleware.StatsMiddleware())

	router.GET("/healthz", middleware.RouteTag("edge_health"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data": gin.H{
				"mode":       common.RuntimeModeEdge,
				"version":    common.Version,
				"node_name":  common.NodeName,
				"start_time": common.StartTime,
			},
		})
	})
	router.GET("/readyz", middleware.RouteTag("edge_readiness"), func(c *gin.Context) {
		status := http.StatusServiceUnavailable
		if edgeservice.EdgeServingReady() {
			status = http.StatusOK
		}
		c.JSON(status, gin.H{"success": status == http.StatusOK})
	})

	relayV1 := router.Group("/v1")
	relayV1.Use(middleware.RouteTag("edge_relay"))
	relayV1.Use(middleware.EdgeRequestAdmission())
	relayV1.Use(middleware.EdgeTokenAuth())
	relayV1.Use(middleware.EdgeTextBoundary())
	relayV1.Use(middleware.EdgeModelPolicy())
	relayV1.Use(middleware.Distribute())
	{
		relayV1.POST("/chat/completions", func(c *gin.Context) {
			controller.Relay(c, types.RelayFormatOpenAI)
		})
		relayV1.POST("/responses", func(c *gin.Context) {
			controller.Relay(c, types.RelayFormatOpenAIResponses)
		})
	}

	router.NoRoute(func(c *gin.Context) {
		c.Set(middleware.RouteTagKey, "edge_not_found")
		controller.RelayNotFound(c)
	})
}
