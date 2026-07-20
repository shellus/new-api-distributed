package router

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	edgeservice "github.com/QuantumNous/new-api/service/edge"

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

	modelsRouter := router.Group("/v1/models")
	modelsRouter.Use(middleware.RouteTag("edge_relay"))
	modelsRouter.Use(middleware.EdgeRequestAdmission())
	modelsRouter.Use(middleware.EdgeTokenAuth())
	registerOpenAIModelRoutes(modelsRouter)

	geminiModelsRouter := router.Group("/v1beta/models")
	geminiModelsRouter.Use(middleware.RouteTag("edge_relay"))
	geminiModelsRouter.Use(middleware.EdgeRequestAdmission())
	geminiModelsRouter.Use(middleware.EdgeTokenAuth())
	registerGeminiModelListRoute(geminiModelsRouter, constant.ChannelTypeGemini)

	geminiCompatibleModelsRouter := router.Group("/v1beta/openai/models")
	geminiCompatibleModelsRouter.Use(middleware.RouteTag("edge_relay"))
	geminiCompatibleModelsRouter.Use(middleware.EdgeRequestAdmission())
	geminiCompatibleModelsRouter.Use(middleware.EdgeTokenAuth())
	registerGeminiModelListRoute(geminiCompatibleModelsRouter, constant.ChannelTypeOpenAI)

	relayV1 := router.Group("/v1")
	relayV1.Use(middleware.RouteTag("edge_relay"))
	relayV1.Use(middleware.EdgeRequestAdmission())
	relayV1.Use(middleware.EdgeTokenAuth())
	registerRelayV1DataPlane(relayV1)

	relayMjRouter := router.Group("/mj")
	relayMjRouter.Use(middleware.RouteTag("edge_relay"))
	relayMjRouter.Use(middleware.EdgeRequestAdmission())
	registerMjRouterGroup(relayMjRouter, middleware.EdgeTokenAuth())

	relayMjModeRouter := router.Group("/:mode/mj")
	relayMjModeRouter.Use(middleware.RouteTag("edge_relay"))
	relayMjModeRouter.Use(middleware.EdgeRequestAdmission())
	registerMjRouterGroup(relayMjModeRouter, middleware.EdgeTokenAuth())

	relaySunoRouter := router.Group("/suno")
	relaySunoRouter.Use(middleware.RouteTag("edge_relay"))
	relaySunoRouter.Use(middleware.EdgeRequestAdmission())
	relaySunoRouter.Use(middleware.EdgeTokenAuth(), middleware.Distribute())
	registerSunoRouterGroup(relaySunoRouter)

	relayGeminiRouter := router.Group("/v1beta")
	relayGeminiRouter.Use(middleware.RouteTag("edge_relay"))
	relayGeminiRouter.Use(middleware.EdgeRequestAdmission())
	relayGeminiRouter.Use(middleware.EdgeTokenAuth())
	relayGeminiRouter.Use(middleware.Distribute())
	registerGeminiRelayRouterGroup(relayGeminiRouter)

	registerVideoDataPlaneRoutes(
		router, "edge_relay", middleware.EdgeRequestAdmission(), middleware.EdgeTokenAuth(), middleware.EdgeTokenAuth(),
	)

	router.NoRoute(func(c *gin.Context) {
		c.Set(middleware.RouteTagKey, "edge_not_found")
		controller.RelayNotFound(c)
	})
}
