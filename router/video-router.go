package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

func SetVideoRouter(router *gin.Engine) {
	registerVideoDataPlaneRoutes(router, "relay", nil, middleware.TokenAuth(), middleware.TokenOrUserAuth())
}

func registerVideoDataPlaneRoutes(
	router *gin.Engine,
	routeTag string,
	admission gin.HandlerFunc,
	relayAuth gin.HandlerFunc,
	contentAuth gin.HandlerFunc,
) {
	videoProxyRouter := router.Group("/v1")
	videoProxyRouter.Use(middleware.RouteTag(routeTag))
	if admission != nil {
		videoProxyRouter.Use(admission)
	}
	videoProxyRouter.Use(contentAuth)
	{
		videoProxyRouter.GET("/videos/:task_id/content", controller.VideoProxy)
	}

	videoV1Router := router.Group("/v1")
	videoV1Router.Use(middleware.RouteTag(routeTag))
	if admission != nil {
		videoV1Router.Use(admission)
	}
	videoV1Router.Use(relayAuth, middleware.Distribute())
	{
		videoV1Router.POST("/video/generations", controller.RelayTask)
		videoV1Router.GET("/video/generations/:task_id", controller.RelayTaskFetch)
		videoV1Router.POST("/videos/:video_id/remix", controller.RelayTask)
	}
	// openai compatible API video routes
	// docs: https://platform.openai.com/docs/api-reference/videos/create
	{
		videoV1Router.POST("/videos", controller.RelayTask)
		videoV1Router.GET("/videos/:task_id", controller.RelayTaskFetch)
	}

	klingV1Router := router.Group("/kling/v1")
	klingV1Router.Use(middleware.RouteTag(routeTag))
	if admission != nil {
		klingV1Router.Use(admission)
	}
	klingV1Router.Use(middleware.KlingRequestConvert(), relayAuth, middleware.Distribute())
	{
		klingV1Router.POST("/videos/text2video", controller.RelayTask)
		klingV1Router.POST("/videos/image2video", controller.RelayTask)
		klingV1Router.GET("/videos/text2video/:task_id", controller.RelayTaskFetch)
		klingV1Router.GET("/videos/image2video/:task_id", controller.RelayTaskFetch)
	}

	// Jimeng official API routes - direct mapping to official API format
	jimengOfficialGroup := router.Group("jimeng")
	jimengOfficialGroup.Use(middleware.RouteTag(routeTag))
	if admission != nil {
		jimengOfficialGroup.Use(admission)
	}
	jimengOfficialGroup.Use(middleware.JimengRequestConvert(), relayAuth, middleware.Distribute())
	{
		// Maps to: /?Action=CVSync2AsyncSubmitTask&Version=2022-08-31 and /?Action=CVSync2AsyncGetResult&Version=2022-08-31
		jimengOfficialGroup.POST("/", controller.RelayTask)
	}
}
