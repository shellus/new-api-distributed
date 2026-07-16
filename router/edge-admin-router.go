package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

func registerEdgeAdminRoutes(apiRouter *gin.RouterGroup) {
	edgeRoute := apiRouter.Group("/edge")
	edgeRoute.Use(middleware.RootAuth())
	edgeRoute.Use(middleware.DisableCache())
	{
		edgeRoute.GET("/nodes", controller.ListEdgeNodes)
		edgeRoute.GET("/snapshots/latest", controller.GetLatestEdgeSnapshot)
		edgeRoute.POST("/nodes", middleware.CriticalRateLimit(), controller.CreateEdgeNode)
		edgeRoute.POST("/snapshots/publish", middleware.CriticalRateLimit(), controller.PublishEdgeSnapshot)
		edgeRoute.POST("/nodes/:id/status", middleware.CriticalRateLimit(), controller.UpdateEdgeNodeStatus)
		edgeRoute.POST("/nodes/:id/credentials/rotate", middleware.CriticalRateLimit(), controller.RotateEdgeNodeCredential)
	}
}
