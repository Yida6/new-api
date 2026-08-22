package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

func SetVideoRouter(router *gin.Engine) {
	// Video proxy: accepts either session auth (dashboard) or token auth (API clients)
	videoProxyRouter := router.Group("/v1")
	videoProxyRouter.Use(middleware.RouteTag("relay"))
	videoProxyRouter.Use(middleware.TokenOrUserAuth())
	{
		videoProxyRouter.GET("/videos/:task_id/content", controller.VideoProxy)
		videoProxyRouter.GET("/videos/:task_id/playback-url", controller.IssueVideoPlaybackURL)
	}

	// Native video elements cannot attach Authorization headers. Playback uses
	// a short-lived, user/task-scoped signed URL issued by the authenticated
	// route above; the handler still performs the normal ownership check.
	videoPlaybackRouter := router.Group("/v1")
	videoPlaybackRouter.Use(middleware.RouteTag("relay"))
	videoPlaybackRouter.GET("/videos/:task_id/playback", controller.VideoPlaybackProxy)

	// Seedance 创建接口（POST /v1/video/generations）单独挂载模型请求限流：
	// TokenAuth() → ModelRequestRateLimit() → Distribute() → RelayTask。
	// 限流读取认证后的用户 ID（c.GetInt("id")）与 Token 分组（ContextKeyTokenGroup，
	// 空值时回退 ContextKeyUserGroup），并在渠道分配与上游请求之前完成。
	// 仅此创建接口计入"按用户计数、按分组覆盖"的生成请求额度；GET 轮询、
	// 视频内容代理、播放地址与播放接口均不计数（保持原有路由结构不变）。
	seedanceCreateRouter := router.Group("/v1/video/generations")
	seedanceCreateRouter.Use(middleware.RouteTag("relay"))
	seedanceCreateRouter.Use(middleware.TokenAuth(), middleware.ModelRequestRateLimit(), middleware.Distribute())
	{
		seedanceCreateRouter.POST("", controller.RelayTask)
	}

	videoV1Router := router.Group("/v1")
	videoV1Router.Use(middleware.RouteTag("relay"))
	videoV1Router.Use(middleware.TokenAuth(), middleware.Distribute())
	{
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
	klingV1Router.Use(middleware.RouteTag("relay"))
	klingV1Router.Use(middleware.KlingRequestConvert(), middleware.TokenAuth(), middleware.Distribute())
	{
		klingV1Router.POST("/videos/text2video", controller.RelayTask)
		klingV1Router.POST("/videos/image2video", controller.RelayTask)
		klingV1Router.GET("/videos/text2video/:task_id", controller.RelayTaskFetch)
		klingV1Router.GET("/videos/image2video/:task_id", controller.RelayTaskFetch)
	}

	// Jimeng official API routes - direct mapping to official API format
	jimengOfficialGroup := router.Group("jimeng")
	jimengOfficialGroup.Use(middleware.RouteTag("relay"))
	jimengOfficialGroup.Use(middleware.JimengRequestConvert(), middleware.TokenAuth(), middleware.Distribute())
	{
		// Maps to: /?Action=CVSync2AsyncSubmitTask&Version=2022-08-31 and /?Action=CVSync2AsyncGetResult&Version=2022-08-31
		jimengOfficialGroup.POST("/", controller.RelayTask)
	}
}
