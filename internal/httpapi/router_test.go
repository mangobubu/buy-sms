package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buysms/internal/application"
	"buysms/internal/auth"
	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/httpapi"
	"buysms/internal/secure"
	"github.com/gin-gonic/gin"
)

const testAdminPath = "/ops-7f0a9c51d8424e8bb75d"

func TestProtectedAPIRoutesRejectMissingAuthentication(t *testing.T) {
	router, _, _ := newTestRouter(t, newMemoryRepository())
	for _, path := range []string{
		"/api/auth/me",
		"/api/dashboard",
		"/api/providers",
		"/api/orders",
		"/api/users",
	} {
		t.Run(path, func(t *testing.T) {
			response := performRequest(router, http.MethodGet, path, "", nil)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("未鉴权请求状态码=%d，响应=%s", response.Code, response.Body.String())
			}
			assertJSONMessage(t, response, auth.ErrUnauthorized.Error())
		})
	}
}

func TestRandomAdminEntryOnlyServesKnownRoutes(t *testing.T) {
	router, _, _ := newTestRouter(t, newMemoryRepository())
	for _, path := range []string{testAdminPath, testAdminPath + "/login", testAdminPath + "/dashboard"} {
		response := performRequest(router, http.MethodGet, path, "", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("已知后台路由 %s 状态码=%d，响应=%s", path, response.Code, response.Body.String())
		}
		if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
			t.Fatalf("已知后台路由 Content-Type=%q", contentType)
		}
	}

	for _, path := range []string{testAdminPath + "/not-a-page", "/admin", "/not-the-entry"} {
		response := performRequest(router, http.MethodGet, path, "", nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("未知页面 %s 状态码=%d，响应=%s", path, response.Code, response.Body.String())
		}
		if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
			t.Fatalf("未知页面 Content-Type=%q", contentType)
		}
	}
}

func TestRandomAdminEntryRejectsUnsupportedMethod(t *testing.T) {
	router, _, _ := newTestRouter(t, newMemoryRepository())
	for _, path := range []string{testAdminPath, testAdminPath + "/login", testAdminPath + "/dashboard"} {
		t.Run(path, func(t *testing.T) {
			response := performRequest(router, http.MethodPost, path, "", nil)
			if response.Code != http.StatusNotFound {
				t.Fatalf("POST 后台页面 %s 状态码=%d，响应=%s", path, response.Code, response.Body.String())
			}
			if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("POST 后台页面 Content-Type=%q", contentType)
			}
			assertJSONMessage(t, response, "路由不存在")
		})
	}
}

func TestUnknownAPIRouteReturnsJSON404(t *testing.T) {
	router, _, _ := newTestRouter(t, newMemoryRepository())
	response := performRequest(router, http.MethodGet, "/api/route-that-does-not-exist", "", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("未知 API 状态码=%d，响应=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("未知 API Content-Type=%q", contentType)
	}
	assertJSONMessage(t, response, "路由不存在")
}

func TestCaptchaRequiresMatchingAdminEntryHeader(t *testing.T) {
	repo := newMemoryRepository()
	router, _, _ := newTestRouter(t, repo)

	for name, header := range map[string]string{"缺少入口头": "", "入口头错误": "/wrong-entry"} {
		t.Run(name, func(t *testing.T) {
			response := performRequest(router, http.MethodGet, "/api/public/captcha", header, nil)
			if response.Code != http.StatusNotFound {
				t.Fatalf("状态码=%d，响应=%s", response.Code, response.Body.String())
			}
		})
	}
	if count := repo.captchaCount(); count != 0 {
		t.Fatalf("入口校验失败时不应生成验证码，实际=%d", count)
	}

	response := performRequest(router, http.MethodGet, "/api/public/captcha", testAdminPath, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("正确入口头状态码=%d，响应=%s", response.Code, response.Body.String())
	}
	if cacheControl := response.Header().Get("Cache-Control"); !strings.Contains(cacheControl, "no-store") {
		t.Fatalf("验证码 Cache-Control=%q", cacheControl)
	}
	if vary := response.Header().Get("Vary"); !strings.Contains(vary, "X-Admin-Path") {
		t.Fatalf("验证码 Vary=%q", vary)
	}
	var captcha auth.Captcha
	if err := json.Unmarshal(response.Body.Bytes(), &captcha); err != nil {
		t.Fatalf("解析验证码响应失败: %v", err)
	}
	if captcha.ID == "" || !strings.HasPrefix(captcha.Image, "data:image/png;base64,") {
		t.Fatalf("验证码响应不完整: %+v", captcha)
	}
	if count := repo.captchaCount(); count != 1 {
		t.Fatalf("验证码应只保存于仓储，实际记录=%d", count)
	}
}

func TestProvidersHideConfigurationFromOperator(t *testing.T) {
	repo := newMemoryRepository()
	router, _, vault := newTestRouter(t, repo)
	const webhookToken = "provider-webhook-token-0123456789"
	tokenCipher, err := vault.Encrypt(webhookToken)
	if err != nil {
		t.Fatal(err)
	}
	repo.putProvider(domain.Provider{
		ID: domain.ProviderHeroSMS, Name: "HeroSMS", BaseURL: "https://provider.example/api/v1",
		Enabled: true, APIKeyConfigured: true, WebhookConfigured: true,
		WebhookTokenCipher: tokenCipher,
		Config:             json.RawMessage(`{"pollingIntervalSeconds":45,"webhookEnabled":true}`),
	})
	operator := domain.User{ID: "operator-1", Username: "operator", Role: "operator", Active: true}
	admin := domain.User{ID: "admin-1", Username: "admin", Role: "admin", Active: true}
	repo.putSession([]byte("router-test-session-pepper"), "operator-token", operator)
	repo.putSession([]byte("router-test-session-pepper"), "admin-token", admin)

	operatorResponse := performAuthenticatedRequest(router, http.MethodGet, "/api/providers", "operator-token")
	if operatorResponse.Code != http.StatusOK {
		t.Fatalf("operator 查询供应商状态码=%d，响应=%s", operatorResponse.Code, operatorResponse.Body.String())
	}
	var operatorProviders []map[string]any
	if err = json.Unmarshal(operatorResponse.Body.Bytes(), &operatorProviders); err != nil {
		t.Fatal(err)
	}
	if len(operatorProviders) != 1 {
		t.Fatalf("operator 供应商数量=%d", len(operatorProviders))
	}
	operatorView := operatorProviders[0]
	if _, exposed := operatorView["webhookUrl"]; exposed {
		t.Fatalf("operator 响应暴露 webhookUrl: %v", operatorView)
	}
	if operatorView["apiBaseUrl"] != "" || operatorView["hasApiKey"] != false || operatorView["hasWebhookToken"] != false {
		t.Fatalf("operator 响应暴露供应商配置细节: %v", operatorView)
	}

	adminResponse := performAuthenticatedRequest(router, http.MethodGet, "/api/providers", "admin-token")
	if adminResponse.Code != http.StatusOK {
		t.Fatalf("admin 查询供应商状态码=%d，响应=%s", adminResponse.Code, adminResponse.Body.String())
	}
	var adminProviders []map[string]any
	if err = json.Unmarshal(adminResponse.Body.Bytes(), &adminProviders); err != nil {
		t.Fatal(err)
	}
	adminView := adminProviders[0]
	expectedWebhookURL := "/api/webhooks/herosms/" + webhookToken
	actualWebhookURL, _ := adminView["webhookUrl"].(string)
	if !strings.HasSuffix(actualWebhookURL, expectedWebhookURL) {
		t.Fatalf("admin webhookUrl=%q，期望后缀=%q", actualWebhookURL, expectedWebhookURL)
	}
	if adminView["apiBaseUrl"] != "https://provider.example/api/v1" || adminView["hasApiKey"] != true || adminView["hasWebhookToken"] != true {
		t.Fatalf("admin 应看到供应商配置状态: %v", adminView)
	}
}

func TestOperatorDashboardOnlyContainsOwnOrdersAndMessages(t *testing.T) {
	repo := newMemoryRepository()
	router, _, _ := newTestRouter(t, repo)
	operator := domain.User{ID: "operator-1", Username: "operator", Role: "operator", Active: true}
	repo.putSession([]byte("router-test-session-pepper"), "operator-token", operator)
	repo.putProvider(domain.Provider{ID: domain.ProviderHeroSMS, Name: "HeroSMS", Enabled: true})
	now := time.Now().UTC()
	repo.putOrder(domain.Order{
		ID: "own-order", UserID: operator.ID, ProviderID: domain.ProviderHeroSMS,
		PhoneNumber: "+15550001111", Status: domain.OrderActive, Cost: 1.25,
		Currency: "USD", CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	})
	repo.putOrder(domain.Order{
		ID: "other-order", UserID: "operator-2", ProviderID: domain.ProviderHeroSMS,
		PhoneNumber: "+15559998888", Status: domain.OrderActive, Cost: 8.75,
		Currency: "USD", CreatedAt: now, UpdatedAt: now,
	})
	repo.messages = append(repo.messages,
		domain.SMSMessage{ID: "own-message", OrderID: "own-order", Code: "1234", Text: "own 1234", ReceivedAt: now},
		domain.SMSMessage{ID: "other-message", OrderID: "other-order", Code: "9999", Text: "other 9999", ReceivedAt: now},
	)

	response := performAuthenticatedRequest(router, http.MethodGet, "/api/dashboard", "operator-token")
	if response.Code != http.StatusOK {
		t.Fatalf("operator 仪表盘状态码=%d，响应=%s", response.Code, response.Body.String())
	}
	var dashboard struct {
		ActiveOrders  int                    `json:"activeOrders"`
		TodayOrders   int                    `json:"todayOrders"`
		TodayMessages int                    `json:"todayMessages"`
		TodaySpend    string                 `json:"todaySpend"`
		RecentOrders  []application.OrderDTO `json:"recentOrders"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.ActiveOrders != 1 || dashboard.TodayOrders != 1 || dashboard.TodayMessages != 1 || dashboard.TodaySpend != "1.25" {
		t.Fatalf("operator 统计未按用户隔离: %+v", dashboard)
	}
	if len(dashboard.RecentOrders) != 1 || dashboard.RecentOrders[0].ID != "own-order" || dashboard.RecentOrders[0].PhoneNumber != "+15550001111" {
		t.Fatalf("operator 最近订单未按用户隔离: %+v", dashboard.RecentOrders)
	}
	encoded := response.Body.String()
	for _, forbidden := range []string{"other-order", "+15559998888", "9999", "other 9999"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("operator 仪表盘泄露其他用户数据 %q: %s", forbidden, encoded)
		}
	}
}

func newTestRouter(t *testing.T, repo *memoryRepository) (*gin.Engine, *application.Service, *secure.Vault) {
	t.Helper()
	vault, err := secure.NewVault([]byte("router-test-encryption-key-32byte"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Environment:   "test",
		AdminPath:     testAdminPath,
		SessionPepper: []byte("router-test-session-pepper"),
		CaptchaTTL:    5 * time.Minute,
		SessionTTL:    time.Hour,
	}
	authentication := auth.New(repo, cfg.SessionPepper, cfg.AdminPath, cfg.CaptchaTTL, cfg.SessionTTL)
	app := application.New(repo, authentication, vault, cfg)
	return httpapi.New(app, authentication, cfg), app, vault
}

func performRequest(router http.Handler, method, target, adminPath string, body *strings.Reader) *httptest.ResponseRecorder {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, body)
		request.Header.Set("Content-Type", "application/json")
	}
	if adminPath != "" {
		request.Header.Set("X-Admin-Path", adminPath)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func performAuthenticatedRequest(router http.Handler, method, target, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func assertJSONMessage(t *testing.T, response *httptest.ResponseRecorder, expected string) {
	t.Helper()
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v；正文=%s", err, response.Body.String())
	}
	if body.Message != expected {
		t.Fatalf("响应消息=%q，期望=%q", body.Message, expected)
	}
}
