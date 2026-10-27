// Package httpapi 是 HTTP 接口层（开发文档 §6、§8），基于 gin：登录、看板接口、CSV 导出和 /healthz。
package httpapi

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/FacePhys/TokenUseSys/internal/application/dashboard"
)

// trustedProxies 是采信 X-Forwarded-For / X-Real-IP 的对端地址：本机回环和内网地址（§8，同 rag）。
// 服务只绑本机端口，经同一台机器上的 Nginx 访问；跑在 compose 里时对端是 compose 网络的网关，也是内网地址。
// 公网地址发来的这两个请求头不采信，按对端地址算，登录失败锁定才不会被伪造的 IP 绕过。
var trustedProxies = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

// Options 是路由的依赖。
type Options struct {
	Log       *slog.Logger
	Dashboard *dashboard.Service
	Auth      AuthOptions
	// Web 是页面和静态资源（web.FS），为空时只提供接口。
	Web fs.FS
	// Now 替换时钟，测试用。
	Now func() time.Time
}

type handler struct {
	log      *slog.Logger
	dash     *dashboard.Service
	auth     AuthOptions
	sessions *sessions
	limiter  *loginLimiter
	now      func() time.Time
}

// NewRouter 创建路由。/healthz 免登录（Nginx 不对外暴露）；/api/v1 下除登录、退出外都要登录。
func NewRouter(opts Options) (*gin.Engine, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := opts.Auth.validate(); err != nil {
		return nil, err
	}
	r := gin.New()
	// 不做自动跳转（补末尾斜杠、修正大小写）：接口路径固定，写错时直接返回 404。
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	if err := r.SetTrustedProxies(trustedProxies); err != nil {
		return nil, fmt.Errorf("配置可信代理: %w", err)
	}
	r.Use(recovery(opts.Log), requestLog(opts.Log))
	h := &handler{log: opts.Log, dash: opts.Dashboard, auth: opts.Auth, sessions: newSessions(opts.Auth),
		limiter: newLoginLimiter(), now: opts.Now}

	r.GET("/healthz", h.healthz)
	api := r.Group("/api/v1", noStore)
	api.POST("/login", h.login)
	api.POST("/logout", h.logout)
	authed := api.Group("", h.requireSession)
	authed.GET("/summary", h.summary)
	authed.GET("/tokens", h.tokens)
	authed.GET("/tokens/:id", h.token)
	authed.GET("/tokens/:id/logs", h.logs)
	authed.GET("/usage/series", h.series)
	authed.GET("/usage/models", h.models)
	authed.GET("/export.csv", h.export)
	if opts.Web != nil {
		files, err := loadStatic(opts.Web)
		if err != nil {
			return nil, err
		}
		registerStatic(r, files)
	}
	r.NoRoute(notFound)
	return r, nil
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func abort(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, apiError{Code: code, Message: message})
}

// fail 把查询层的错误翻译成状态码：参数错误 400、不存在 404，其他记日志后返回 500，不向外暴露内部错误。
func (h *handler) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, dashboard.ErrInvalidArgument):
		abort(c, http.StatusBadRequest, "invalid_argument", err.Error())
	case errors.Is(err, dashboard.ErrNotFound):
		abort(c, http.StatusNotFound, "not_found", err.Error())
	default:
		h.log.ErrorContext(c.Request.Context(), "处理请求失败", "route", c.FullPath(), "error", err)
		abort(c, http.StatusInternalServerError, "internal", "服务内部错误")
	}
}

// noStore 让浏览器和代理都不缓存接口响应：里面是账号用量数据。
func noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Next()
}

// recovery 把处理请求时的 panic 记进日志并返回 500。gin 自带的 Recovery 会把请求头（含会话 Cookie）
// 写到标准错误，这里不写。
func recovery(log *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, err any) {
		log.ErrorContext(c.Request.Context(), "处理请求时发生 panic",
			"route", c.FullPath(), "panic", fmt.Sprint(err), "stack", string(debug.Stack()))
		c.AbortWithStatus(http.StatusInternalServerError)
	})
}

// requestLog 只记录方法、路由、状态码和耗时，不记录查询参数、请求头和请求体。
func requestLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		level := slog.LevelInfo
		switch route {
		case "/healthz":
			level = slog.LevelDebug
		case "":
			route = "unmatched"
		}
		log.Log(c.Request.Context(), level, "http request",
			"method", c.Request.Method,
			"route", route,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	}
}
