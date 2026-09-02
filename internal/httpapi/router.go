package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"buysms/internal/application"
	"buysms/internal/auth"
	"buysms/internal/config"
	"buysms/internal/domain"
	appweb "buysms/web"
	"github.com/gin-gonic/gin"
)

const userContextKey = "authenticated_user"

type Handler struct {
	app  *application.Service
	auth *auth.Service
	cfg  config.Config
	dist fs.FS
}

func New(app *application.Service, authentication *auth.Service, cfg config.Config) *gin.Engine {
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.Use(gin.Recovery(), securityHeaders(), requestTimeout(35*time.Second))
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		panic(err)
	}
	dist, _ := fs.Sub(appweb.Dist, "dist")
	h := &Handler{app: app, auth: authentication, cfg: cfg, dist: dist}
	r.GET("/healthz", h.health)
	api := r.Group("/api")
	api.GET("/public/captcha", h.captcha)
	api.POST("/public/login", h.login)
	api.POST("/webhooks/:provider/:token", h.webhook)
	authed := api.Group("")
	authed.Use(noStore(), h.authenticate())
	authed.GET("/auth/me", h.me)
	authed.POST("/auth/logout", h.logout)
	authed.POST("/auth/change-password", h.changePassword)
	authed.GET("/dashboard", h.dashboard)
	authed.GET("/providers", h.providers)
	authed.PUT("/providers/:id", h.requireAdmin(), h.updateProvider)
	authed.GET("/catalog/countries", h.countries)
	authed.GET("/catalog/services", h.services)
	authed.GET("/catalog/quote", h.quote)
	authed.POST("/orders", h.createOrder)
	authed.GET("/orders", h.orders)
	authed.GET("/orders/:id", h.order)
	authed.POST("/orders/:id/complete", h.completeOrder)
	authed.POST("/orders/:id/cancel", h.cancelOrder)
	authed.GET("/users", h.requireAdmin(), h.users)
	authed.POST("/users", h.requireAdmin(), h.createUser)
	authed.PUT("/users/:id", h.requireAdmin(), h.updateUser)
	r.NoMethod(func(c *gin.Context) { notFound(c) })
	r.NoRoute(h.frontend)
	return r
}

func (h *Handler) health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if err := h.app.Health(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
func (h *Handler) captcha(c *gin.Context) {
	setNoStore(c)
	c.Header("Vary", "X-Admin-Path")
	entry := strings.TrimRight(c.GetHeader("X-Admin-Path"), "/")
	if len(entry) != len(h.cfg.AdminPath) || subtle.ConstantTimeCompare([]byte(entry), []byte(h.cfg.AdminPath)) != 1 {
		notFound(c)
		return
	}
	allowed, err := h.app.CaptchaAllowed(c.Request.Context(), c.ClientIP())
	if err != nil {
		serverError(c, err)
		return
	}
	if !allowed {
		fail(c, http.StatusTooManyRequests, "验证码请求过于频繁")
		return
	}
	value, err := h.auth.Captcha(c.Request.Context())
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) login(c *gin.Context) {
	setNoStore(c)
	var in auth.LoginInput
	if !bind(c, &in) {
		return
	}
	if header := c.GetHeader("X-Admin-Path"); header != "" {
		if in.AdminPath != "" && in.AdminPath != header {
			bad(c, "后台入口校验失败")
			return
		}
		in.AdminPath = header
	}
	in.IP = c.ClientIP()
	in.UserAgent = c.Request.UserAgent()
	result, err := h.auth.Login(c.Request.Context(), in)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			fail(c, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, auth.ErrCredentials):
			fail(c, http.StatusUnauthorized, err.Error())
		default:
			serverError(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *Handler) webhook(c *gin.Context) {
	setNoStore(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	body, err := io.ReadAll(c.Request.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(c, http.StatusRequestEntityTooLarge, "回调数据超过大小限制")
		return
	}
	if err != nil || len(body) == 0 {
		bad(c, "回调数据无效")
		return
	}
	headers, _ := json.Marshal(selectedHeaders(c.Request.Header))
	err = h.app.Webhook(c.Request.Context(), c.Param("provider"), c.Param("token"), body, headers)
	if errors.Is(err, application.ErrNotFound) {
		notFound(c)
		return
	}
	if errors.Is(err, application.ErrBadRequest) {
		bad(c, "回调数据无效")
		return
	}
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("Authorization")
		scheme, token, ok := strings.Cut(raw, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			fail(c, http.StatusUnauthorized, auth.ErrUnauthorized.Error())
			c.Abort()
			return
		}
		u, err := h.auth.Authenticate(c.Request.Context(), strings.TrimSpace(token))
		if err != nil {
			fail(c, http.StatusUnauthorized, auth.ErrUnauthorized.Error())
			c.Abort()
			return
		}
		c.Set(userContextKey, u)
		c.Set("bearer_token", strings.TrimSpace(token))
		c.Next()
	}
}
func (h *Handler) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if currentUser(c).Role != "admin" {
			fail(c, http.StatusForbidden, application.ErrForbidden.Error())
			c.Abort()
			return
		}
		c.Next()
	}
}
func (h *Handler) me(c *gin.Context) { c.JSON(http.StatusOK, application.UserView(currentUser(c))) }
func (h *Handler) logout(c *gin.Context) {
	if err := h.auth.Logout(c.Request.Context(), c.GetString("bearer_token")); err != nil {
		serverError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
func (h *Handler) changePassword(c *gin.Context) {
	var in struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !bind(c, &in) {
		return
	}
	if err := h.app.ChangePassword(c.Request.Context(), currentUser(c), in.CurrentPassword, in.NewPassword, c.ClientIP()); err != nil {
		respondError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
func (h *Handler) dashboard(c *gin.Context) {
	value, err := h.app.Dashboard(c.Request.Context(), currentUser(c))
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) providers(c *gin.Context) {
	value, err := h.app.Providers(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	if currentUser(c).Role != "admin" {
		for i := range value {
			value[i].APIBaseURL = ""
			value[i].PollingIntervalSeconds = 0
			value[i].WebhookSupported = false
			value[i].WebhookEnabled = false
			value[i].HasAPIKey = false
			value[i].HasWebhookToken = false
			value[i].WebhookURL = ""
		}
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) updateProvider(c *gin.Context) {
	var in application.UpdateProviderInput
	if !bind(c, &in) {
		return
	}
	value, err := h.app.UpdateProvider(c.Request.Context(), c.Param("id"), in, currentUser(c), c.ClientIP())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) countries(c *gin.Context) {
	value, err := h.app.Countries(c.Request.Context(), c.Query("provider"))
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) services(c *gin.Context) {
	value, err := h.app.Services(c.Request.Context(), c.Query("provider"), c.Query("country"))
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) quote(c *gin.Context) {
	value, err := h.app.Quote(c.Request.Context(), c.Query("provider"), c.Query("country"), c.Query("service"))
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) createOrder(c *gin.Context) {
	var in application.PurchaseInput
	if !bind(c, &in) {
		return
	}
	in.IdempotencyKey = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	value, err := h.app.Purchase(c.Request.Context(), in, currentUser(c), c.ClientIP())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, value)
}
func (h *Handler) orders(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	value, err := h.app.Orders(c.Request.Context(), application.OrderQuery{Page: page, PageSize: size, Status: c.Query("status"), Provider: c.Query("provider"), Keyword: c.Query("keyword")}, currentUser(c))
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) order(c *gin.Context) {
	value, err := h.app.Order(c.Request.Context(), c.Param("id"), currentUser(c))
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) completeOrder(c *gin.Context) { h.finish(c, "complete") }
func (h *Handler) cancelOrder(c *gin.Context)   { h.finish(c, "cancel") }
func (h *Handler) finish(c *gin.Context, action string) {
	value, err := h.app.FinishOrder(c.Request.Context(), c.Param("id"), action, currentUser(c), c.ClientIP())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) users(c *gin.Context) {
	value, err := h.app.Users(c.Request.Context())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *Handler) createUser(c *gin.Context) {
	var in application.SaveUserInput
	if !bind(c, &in) {
		return
	}
	value, err := h.app.CreateUser(c.Request.Context(), in, currentUser(c), c.ClientIP())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, value)
}
func (h *Handler) updateUser(c *gin.Context) {
	var in application.SaveUserInput
	if !bind(c, &in) {
		return
	}
	value, err := h.app.UpdateUser(c.Request.Context(), c.Param("id"), in, currentUser(c), c.ClientIP())
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *Handler) frontend(c *gin.Context) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		notFound(c)
		return
	}
	if c.Request.URL.Path == "/api" || strings.HasPrefix(c.Request.URL.Path, "/api/") {
		notFound(c)
		return
	}
	requested := path.Clean(strings.TrimPrefix(c.Request.URL.Path, "/"))
	if requested != "." && strings.HasPrefix(requested, "assets/") {
		if _, err := fs.Stat(h.dist, requested); err == nil {
			http.FileServer(http.FS(h.dist)).ServeHTTP(c.Writer, c.Request)
			return
		}
	}
	admin := strings.TrimPrefix(h.cfg.AdminPath, "/")
	relative := ""
	if requested == admin {
		relative = ""
	} else if strings.HasPrefix(requested, admin+"/") {
		relative = strings.TrimPrefix(requested, admin+"/")
	} else {
		htmlNotFound(c)
		return
	}
	allowed := map[string]bool{"": true, "login": true, "dashboard": true, "buy": true, "orders": true, "providers": true, "users": true, "security": true}
	if !allowed[relative] {
		htmlNotFound(c)
		return
	}
	index, err := fs.ReadFile(h.dist, "index.html")
	if err != nil {
		index = []byte(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="robots" content="noindex"><title>短信聚合管理台</title><div id="app">前端资源尚未构建</div></html>`)
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", index)
}

func bind(c *gin.Context, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		bad(c, "请求数据格式不正确")
		return false
	}
	return true
}
func currentUser(c *gin.Context) domain.User {
	v, _ := c.Get(userContextKey)
	u, _ := v.(domain.User)
	return u
}
func respondError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, application.ErrBadRequest):
		bad(c, err.Error())
	case errors.Is(err, application.ErrNotFound):
		fail(c, http.StatusNotFound, err.Error())
	case errors.Is(err, application.ErrConflict):
		fail(c, http.StatusConflict, err.Error())
	case errors.Is(err, application.ErrForbidden):
		fail(c, http.StatusForbidden, err.Error())
	case errors.Is(err, application.ErrProvider):
		fail(c, http.StatusBadGateway, err.Error())
	default:
		serverError(c, err)
	}
}
func bad(c *gin.Context, message string)              { fail(c, http.StatusBadRequest, message) }
func fail(c *gin.Context, status int, message string) { c.JSON(status, gin.H{"message": message}) }
func serverError(c *gin.Context, err error) {
	requestPath := c.Request.URL.Path
	if strings.HasPrefix(requestPath, "/api/webhooks/") {
		requestPath = "/api/webhooks/:provider/:token"
	}
	slog.Error("请求处理失败", "error", err, "path", requestPath)
	fail(c, http.StatusInternalServerError, "服务器处理请求失败")
}
func notFound(c *gin.Context) { fail(c, http.StatusNotFound, "路由不存在") }
func htmlNotFound(c *gin.Context) {
	c.Data(http.StatusNotFound, "text/html; charset=utf-8", []byte("<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\"><title>404</title><h1>404</h1><p>页面不存在</p></html>"))
}
func selectedHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	for _, key := range []string{"Content-Type", "User-Agent", "X-Forwarded-For", "X-Request-Id"} {
		if values := h.Values(key); len(values) > 0 {
			out[key] = values
		}
	}
	return out
}
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'")
		c.Next()
	}
}
func noStore() gin.HandlerFunc {
	return func(c *gin.Context) {
		setNoStore(c)
		c.Next()
	}
}
func setNoStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store, max-age=0")
	c.Header("Pragma", "no-cache")
}
func requestTimeout(timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
