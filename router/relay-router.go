package router

import (
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

func SetRelayRouter(router *gin.Engine) {
	router.Use(middleware.CORS())
	router.Use(middleware.DecompressRequestMiddleware())
	router.Use(middleware.BodyStorageCleanup()) // 清理请求体存储
	router.Use(middleware.StatsMiddleware())
	// https://platform.openai.com/docs/api-reference/introduction
	modelsRouter := router.Group("/v1/models")
	modelsRouter.Use(middleware.RouteTag("relay"))
	modelsRouter.Use(middleware.TokenAuth())
	registerOpenAIModelRoutes(modelsRouter)

	geminiRouter := router.Group("/v1beta/models")
	geminiRouter.Use(middleware.RouteTag("relay"))
	geminiRouter.Use(middleware.TokenAuth())
	registerGeminiModelListRoute(geminiRouter, constant.ChannelTypeGemini)

	geminiCompatibleRouter := router.Group("/v1beta/openai/models")
	geminiCompatibleRouter.Use(middleware.RouteTag("relay"))
	geminiCompatibleRouter.Use(middleware.TokenAuth())
	registerGeminiModelListRoute(geminiCompatibleRouter, constant.ChannelTypeOpenAI)

	playgroundRouter := router.Group("/pg")
	playgroundRouter.Use(middleware.RouteTag("relay"))
	playgroundRouter.Use(middleware.SystemPerformanceCheck())
	playgroundRouter.Use(middleware.UserAuth(), middleware.Distribute())
	{
		playgroundRouter.POST("/chat/completions", controller.Playground)
	}
	relayV1Router := router.Group("/v1")
	relayV1Router.Use(middleware.RouteTag("relay"))
	relayV1Router.Use(middleware.SystemPerformanceCheck())
	relayV1Router.Use(middleware.TokenAuth())
	relayV1Router.Use(middleware.ModelRequestRateLimit())
	registerRelayV1DataPlane(relayV1Router)

	relayMjRouter := router.Group("/mj")
	relayMjRouter.Use(middleware.RouteTag("relay"))
	relayMjRouter.Use(middleware.SystemPerformanceCheck())
	registerMjRouterGroup(relayMjRouter, middleware.TokenAuth())

	relayMjModeRouter := router.Group("/:mode/mj")
	relayMjModeRouter.Use(middleware.RouteTag("relay"))
	relayMjModeRouter.Use(middleware.SystemPerformanceCheck())
	registerMjRouterGroup(relayMjModeRouter, middleware.TokenAuth())
	//relayMjRouter.Use()

	relaySunoRouter := router.Group("/suno")
	relaySunoRouter.Use(middleware.RouteTag("relay"))
	relaySunoRouter.Use(middleware.SystemPerformanceCheck())
	relaySunoRouter.Use(middleware.TokenAuth(), middleware.Distribute())
	registerSunoRouterGroup(relaySunoRouter)

	relayGeminiRouter := router.Group("/v1beta")
	relayGeminiRouter.Use(middleware.RouteTag("relay"))
	relayGeminiRouter.Use(middleware.SystemPerformanceCheck())
	relayGeminiRouter.Use(middleware.TokenAuth())
	relayGeminiRouter.Use(middleware.ModelRequestRateLimit())
	relayGeminiRouter.Use(middleware.Distribute())
	registerGeminiRelayRouterGroup(relayGeminiRouter)
}

func registerOpenAIModelRoutes(modelsRouter *gin.RouterGroup) {
	modelsRouter.GET("", func(c *gin.Context) {
		switch {
		case c.GetHeader("x-api-key") != "" && c.GetHeader("anthropic-version") != "":
			controller.ListModels(c, constant.ChannelTypeAnthropic)
		case c.GetHeader("x-goog-api-key") != "" || c.Query("key") != "":
			controller.RetrieveModel(c, constant.ChannelTypeGemini)
		default:
			controller.ListModels(c, constant.ChannelTypeOpenAI)
		}
	})
	modelsRouter.GET("/:model", func(c *gin.Context) {
		switch {
		case c.GetHeader("x-api-key") != "" && c.GetHeader("anthropic-version") != "":
			controller.RetrieveModel(c, constant.ChannelTypeAnthropic)
		default:
			controller.RetrieveModel(c, constant.ChannelTypeOpenAI)
		}
	})
}

func registerGeminiModelListRoute(modelsRouter *gin.RouterGroup, channelType int) {
	modelsRouter.GET("", func(c *gin.Context) {
		controller.ListModels(c, channelType)
	})
}

func registerRelayV1DataPlane(relayV1Router *gin.RouterGroup) {
	relayRouter := relayV1Router.Group("")
	relayRouter.Use(middleware.Distribute())
	relayRouter.GET("/realtime", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIRealtime)
	})
	relayRouter.POST("/messages", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatClaude)
	})
	relayRouter.POST("/completions", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAI)
	})
	relayRouter.POST("/chat/completions", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAI)
	})
	relayRouter.POST("/responses", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIResponses)
	})
	relayRouter.POST("/responses/compact", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIResponsesCompaction)
	})
	relayRouter.POST("/alpha/search", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIAlphaSearch)
	})
	relayRouter.POST("/edits", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIImage)
	})
	relayRouter.POST("/images/generations", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIImage)
	})
	relayRouter.POST("/images/edits", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIImage)
	})
	relayRouter.POST("/embeddings", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatEmbedding)
	})
	relayRouter.POST("/audio/transcriptions", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIAudio)
	})
	relayRouter.POST("/audio/translations", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIAudio)
	})
	relayRouter.POST("/audio/speech", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAIAudio)
	})
	relayRouter.POST("/rerank", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatRerank)
	})
	relayRouter.POST("/engines/:model/embeddings", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatGemini)
	})
	relayRouter.POST("/models/*path", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatGemini)
	})
	relayRouter.POST("/moderations", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatOpenAI)
	})
	relayRouter.POST("/images/variations", controller.RelayNotImplemented)
	relayRouter.GET("/files", controller.RelayNotImplemented)
	relayRouter.POST("/files", controller.RelayNotImplemented)
	relayRouter.DELETE("/files/:id", controller.RelayNotImplemented)
	relayRouter.GET("/files/:id", controller.RelayNotImplemented)
	relayRouter.GET("/files/:id/content", controller.RelayNotImplemented)
	relayRouter.POST("/fine-tunes", controller.RelayNotImplemented)
	relayRouter.GET("/fine-tunes", controller.RelayNotImplemented)
	relayRouter.GET("/fine-tunes/:id", controller.RelayNotImplemented)
	relayRouter.POST("/fine-tunes/:id/cancel", controller.RelayNotImplemented)
	relayRouter.GET("/fine-tunes/:id/events", controller.RelayNotImplemented)
	relayRouter.DELETE("/models/:model", controller.RelayNotImplemented)
}

func registerSunoRouterGroup(relaySunoRouter *gin.RouterGroup) {
	relaySunoRouter.POST("/submit/:action", controller.RelayTask)
	relaySunoRouter.POST("/fetch", controller.RelayTaskFetch)
	relaySunoRouter.GET("/fetch/:id", controller.RelayTaskFetch)
}

func registerGeminiRelayRouterGroup(relayGeminiRouter *gin.RouterGroup) {
	relayGeminiRouter.POST("/models/*path", func(c *gin.Context) {
		controller.Relay(c, types.RelayFormatGemini)
	})
}

func registerMjRouterGroup(relayMjRouter *gin.RouterGroup, tokenAuth gin.HandlerFunc) {
	relayMjRouter.GET("/image/:id", relay.RelayMidjourneyImage)
	relayMjRouter.Use(tokenAuth, middleware.Distribute())
	{
		relayMjRouter.POST("/submit/action", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/shorten", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/modal", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/imagine", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/change", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/simple-change", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/describe", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/blend", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/edits", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/video", controller.RelayMidjourney)
		//relayMjRouter.POST("/notify", controller.RelayMidjourney)
		relayMjRouter.GET("/task/:id/fetch", controller.RelayMidjourney)
		relayMjRouter.GET("/task/:id/image-seed", controller.RelayMidjourney)
		relayMjRouter.POST("/task/list-by-condition", controller.RelayMidjourney)
		relayMjRouter.POST("/insight-face/swap", controller.RelayMidjourney)
		relayMjRouter.POST("/submit/upload-discord-images", controller.RelayMidjourney)
	}
}
