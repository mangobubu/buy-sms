package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/application"
	"buysms/internal/auth"
	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
)

func TestWebhookTokenIdempotencyAndMultipleMessages(t *testing.T) {
	repo := newMemoryRepository()
	router, _, vault := newTestRouter(t, repo)
	const (
		webhookToken = "webhook-token-0123456789abcdef"
		upstreamID   = "activation-42"
		orderID      = "order-42"
	)
	tokenCipher, err := vault.Encrypt(webhookToken)
	if err != nil {
		t.Fatal(err)
	}
	repo.putProvider(domain.Provider{
		ID: domain.ProviderHeroSMS, Name: "HeroSMS", Enabled: true,
		WebhookTokenCipher: tokenCipher,
	})
	repo.putOrder(domain.Order{
		ID: orderID, UserID: "user-1", ProviderID: domain.ProviderHeroSMS,
		UpstreamID: upstreamID, CountryCode: "2", ServiceCode: "tg",
		Status: domain.OrderActive, UpdatedAt: time.Now().UTC().Add(-time.Hour),
	})

	firstAt := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Millisecond)
	secondAt := firstAt.Add(time.Minute)
	thirdAt := secondAt.Add(time.Minute)
	first := webhookBody(upstreamID, "24680", "验证码 24680", firstAt)
	firstReordered := fmt.Sprintf(
		`{ "receivedAt": %q, "country": 2, "text": "验证码 24680", "service": "tg", "code": "24680", "activationId": %q }`,
		firstAt.Format(time.RFC3339Nano), upstreamID,
	)
	secondSameCode := webhookBody(upstreamID, "24680", "验证码 24680", secondAt)
	thirdDifferentCode := webhookBody(upstreamID, "97531", "验证码 97531", thirdAt)

	wrong := performRequest(router, http.MethodPost, "/api/webhooks/herosms/wrong-token", "", strings.NewReader(first))
	if wrong.Code != http.StatusNotFound {
		t.Fatalf("错误 webhook token 状态码=%d，响应=%s", wrong.Code, wrong.Body.String())
	}

	for index, body := range []string{first, firstReordered, secondSameCode, thirdDifferentCode} {
		response := performRequest(router, http.MethodPost, "/api/webhooks/herosms/"+webhookToken, "", strings.NewReader(body))
		if response.Code != http.StatusOK {
			t.Fatalf("第 %d 次 webhook 状态码=%d，响应=%s", index+1, response.Code, response.Body.String())
		}
	}

	orders, messages, events, transitions := repo.snapshot()
	if len(events) != 3 {
		t.Fatalf("相同 JSON 的重排回调应幂等，唯一事件=%d，期望=3", len(events))
	}
	if len(messages) != 3 {
		t.Fatalf("同一订单应保存全部唯一短信，实际=%d，期望=3", len(messages))
	}
	if len(transitions) != 0 || orders[orderID].Status != domain.OrderActive {
		t.Fatalf("收到验证码后不应自动结算，状态=%q，迁移=%v", orders[orderID].Status, transitions)
	}

	var sameCodeTimes []time.Time
	for _, message := range messages {
		if message.OrderID != orderID || message.Source != "webhook" {
			t.Fatalf("消息归属或来源错误: %+v", message)
		}
		if message.Code == "24680" {
			sameCodeTimes = append(sameCodeTimes, message.ReceivedAt)
		}
	}
	if len(sameCodeTimes) != 2 || sameCodeTimes[0].Equal(sameCodeTimes[1]) {
		t.Fatalf("相同验证码在不同 receivedAt 应保存为两条，实际=%v", sameCodeTimes)
	}
}

func TestWebhookRejectsPayloadLargerThanTwoMiBBeforeApplication(t *testing.T) {
	repo := newMemoryRepository()
	router, _, vault := newTestRouter(t, repo)
	const token = "oversized-webhook-token-123456789"
	tokenCipher, err := vault.Encrypt(token)
	if err != nil {
		t.Fatal(err)
	}
	repo.putProvider(domain.Provider{ID: domain.ProviderHeroSMS, WebhookTokenCipher: tokenCipher})
	payload := bytes.Repeat([]byte("x"), (2<<20)+1)
	request := httptest.NewRequest(http.MethodPost, "/api/webhooks/herosms/"+token, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 webhook 状态码=%d，响应=%s", response.Code, response.Body.String())
	}
	assertJSONMessage(t, response, "回调数据超过大小限制")
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store, max-age=0" {
		t.Fatalf("超大 webhook Cache-Control=%q", cacheControl)
	}
	if reads := repo.providerReadCount(); reads != 0 {
		t.Fatalf("超大 webhook 不应进入应用层查询供应商，实际=%d", reads)
	}
	orders, messages, events, transitions := repo.snapshot()
	if len(orders) != 0 || len(messages) != 0 || len(events) != 0 || len(transitions) != 0 {
		t.Fatalf("超大 webhook 不应进入应用或仓储: orders=%d messages=%d events=%d transitions=%d", len(orders), len(messages), len(events), len(transitions))
	}
}

func TestPollingStoresAllOTPAndKeepsOrderActive(t *testing.T) {
	var requests atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/activations/upstream-7/otp" {
			t.Errorf("轮询请求不正确: %s %s", request.Method, request.URL.Path)
			http.NotFound(w, request)
			return
		}
		if authorization := request.Header.Get("Authorization"); authorization != "ApiKey provider-secret" {
			t.Errorf("轮询鉴权头=%q", authorization)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"active",
			"otpList":[
				{"id":"otp-1","smsCode":"11223","smsText":"first 11223","receivedAt":"2026-09-01T01:02:03Z"},
				{"id":"otp-2","smsCode":"44556","smsText":"second 44556","receivedAt":"2026-09-01T01:03:04Z"}
			]
		}`))
	}))
	t.Cleanup(providerServer.Close)

	repo := newMemoryRepository()
	vault, err := secure.NewVault([]byte("polling-test-encryption-key-32byt"))
	if err != nil {
		t.Fatal(err)
	}
	apiKeyCipher, err := vault.Encrypt("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]any{"pollingIntervalSeconds": 5, "webhookEnabled": true})
	repo.putProvider(domain.Provider{
		ID: domain.ProviderHeroSMS, Name: "HeroSMS", Enabled: true,
		BaseURL: providerServer.URL + "/api/v1", APIKeyCipher: apiKeyCipher, Config: settings,
	})
	const orderID = "poll-order-7"
	repo.putOrder(domain.Order{
		ID: orderID, UserID: "user-1", ProviderID: domain.ProviderHeroSMS,
		UpstreamID: "upstream-7", ServiceCode: "tg", CountryCode: "2",
		Status: domain.OrderActive, NextPollAt: time.Now().Add(-time.Minute),
	})
	cfg := config.Config{
		AdminPath: testAdminPath, SessionPepper: []byte("poll-session-pepper"),
		CaptchaTTL: time.Minute, SessionTTL: time.Hour,
	}
	authentication := auth.New(repo, cfg.SessionPepper, cfg.AdminPath, cfg.CaptchaTTL, cfg.SessionTTL)
	app := application.New(repo, authentication, vault, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.Run(ctx)
	}()

	select {
	case <-repo.pollUpdated:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("等待轮询完成超时")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("轮询服务停止超时")
	}

	orders, messages, _, transitions := repo.snapshot()
	order := orders[orderID]
	if len(messages) != 2 {
		t.Fatalf("一次轮询应保存完整 OTP 列表，实际=%d，消息=%+v", len(messages), messages)
	}
	if messages[0].Code != "11223" || messages[1].Code != "44556" {
		t.Fatalf("验证码顺序或内容错误: %+v", messages)
	}
	if order.Status != domain.OrderActive || len(transitions) != 0 {
		t.Fatalf("轮询收到验证码后不应自动完成，状态=%q，迁移=%v", order.Status, transitions)
	}
	if !order.NextPollAt.After(time.Now()) {
		t.Fatalf("活动订单应安排后续轮询，nextPollAt=%s", order.NextPollAt)
	}
	if count := requests.Load(); count != 1 {
		t.Fatalf("供应商轮询次数=%d，期望=1", count)
	}
}

func webhookBody(upstreamID, code, text string, receivedAt time.Time) string {
	payload, _ := json.Marshal(map[string]any{
		"activationId": upstreamID,
		"service":      "tg",
		"country":      2,
		"code":         code,
		"text":         text,
		"receivedAt":   receivedAt.Format(time.RFC3339Nano),
	})
	return string(payload)
}
