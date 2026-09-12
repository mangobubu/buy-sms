package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/domain"
	"buysms/internal/store"
)

func (r *memoryRepository) CompleteOrderLocally(_ context.Context, id, _, _ string, _ json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	order, ok := r.orders[id]
	if !ok || order.ProviderID != domain.ProviderSMSBower || order.Status != domain.OrderActive || order.RenewalInflight || order.RequestNextInflight {
		return store.ErrConflict
	}
	order.Status = domain.OrderCompleted
	order.LastProviderState = "user_local_complete"
	order.RequestNextPending = false
	r.orders[id] = order
	r.statusTransitions = append(r.statusTransitions, domain.OrderCompleted)
	return nil
}

func TestSMSBowerLocalCompletionEndpointRequiresAuthenticatedExplicitConfirmation(t *testing.T) {
	repo := newMemoryRepository()
	router, _, vault := newTestRouter(t, repo)
	const (
		orderID = "local-http-order"
		userID  = "local-http-user"
	)
	key, err := vault.Encrypt("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("ineligible request unexpectedly reached provider: %s", r.URL.RawQuery)
		http.Error(w, "UNEXPECTED", http.StatusInternalServerError)
	}))
	t.Cleanup(providerServer.Close)
	repo.putProvider(domain.Provider{ID: domain.ProviderSMSBower, Name: "SMSBower", Enabled: true, BaseURL: providerServer.URL + "/handler_api.php", APIKeyCipher: key})
	now := time.Now().UTC()
	repo.putOrder(domain.Order{ID: orderID, UserID: userID, ProviderID: domain.ProviderSMSBower, UpstreamID: "local-http-upstream", Status: domain.OrderActive, CreatedAt: now.Add(-time.Hour), ActivationStartedAt: now.Add(-time.Hour), Cost: .091, Currency: "USD"})
	if _, err := repo.SaveMessage(context.Background(), domain.SMSMessage{ID: "local-http-msg", OrderID: orderID, ProviderID: domain.ProviderSMSBower, Code: "654321", Text: "Your code is 654321", ReceivedAt: now.Add(-30 * time.Minute), UpstreamFingerprint: "http-fingerprint"}, false); err != nil {
		t.Fatal(err)
	}
	operator := domain.User{ID: userID, Username: "operator", Role: "operator", Active: true}
	other := domain.User{ID: "other-http-user", Username: "other", Role: "operator", Active: true}
	repo.putSession([]byte("router-test-session-pepper"), "local-http-token", operator)
	repo.putSession([]byte("router-test-session-pepper"), "other-http-token", other)

	for _, tt := range []struct {
		name, token, body string
		status            int
		code              string
	}{
		{name: "missing authentication", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "wrong owner", token: "other-http-token", body: `{"upstreamMissingConfirmed":true}`, status: http.StatusNotFound, code: "not_found"},
		{name: "explicit confirmation omitted", token: "local-http-token", body: `{}`, status: http.StatusConflict, code: "local_complete_not_allowed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/orders/"+orderID+"/close-local", strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			if tt.token != "" {
				request.Header.Set("Authorization", "Bearer "+tt.token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tt.status {
				t.Fatalf("状态码=%d，期望=%d，响应=%s", response.Code, tt.status, response.Body.String())
			}
			var result struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Code != tt.code {
				t.Fatalf("错误码=%q，期望=%q，响应=%s", result.Code, tt.code, response.Body.String())
			}
		})
	}
	orders, _, _, transitions := repo.snapshot()
	if orders[orderID].Status != domain.OrderActive || len(transitions) != 0 {
		t.Fatal("rejected local completion changed local state")
	}
}

func TestSMSBowerLocalCompletionEndpointReturnsCompletedOrderAndNoRefund(t *testing.T) {
	var setStatus, getStatus atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "setStatus":
			setStatus.Add(1)
			if r.URL.Query().Get("status") != "6" {
				t.Errorf("本地完成只能确认完成，实际 status=%q", r.URL.Query().Get("status"))
			}
			_, _ = w.Write([]byte("BAD_STATUS"))
		case "getStatus":
			getStatus.Add(1)
			_, _ = w.Write([]byte("STATUS_OK:987654"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(providerServer.Close)
	repo := newMemoryRepository()
	router, _, vault := newTestRouter(t, repo)
	key, err := vault.Encrypt("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	repo.putProvider(domain.Provider{ID: domain.ProviderSMSBower, Name: "SMSBower", Enabled: true, BaseURL: providerServer.URL + "/handler_api.php", APIKeyCipher: key})
	now := time.Now().UTC()
	const orderID = "local-http-success"
	repo.putOrder(domain.Order{ID: orderID, UserID: "local-http-user", ProviderID: domain.ProviderSMSBower, UpstreamID: "local-http-upstream", Status: domain.OrderActive, CreatedAt: now.Add(-time.Hour), ActivationStartedAt: now.Add(-time.Hour), Cost: .091, Currency: "USD"})
	_, err = repo.SaveMessage(context.Background(), domain.SMSMessage{ID: "local-http-msg", OrderID: orderID, ProviderID: domain.ProviderSMSBower, Code: "654321", Text: "Your code is 654321", ReceivedAt: now.Add(-30 * time.Minute), UpstreamFingerprint: "http-fingerprint"}, false)
	if err != nil {
		t.Fatal(err)
	}
	repo.putSession([]byte("router-test-session-pepper"), "local-http-token", domain.User{ID: "local-http-user", Role: "operator", Active: true})
	body := `{"upstreamMissingConfirmed":true,"reason":"已在供应商页面确认号码不存在"}`
	request := httptest.NewRequest(http.MethodPost, "/api/orders/"+orderID+"/close-local", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer local-http-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("本地完成状态码=%d，响应=%s", response.Code, response.Body.String())
	}
	var view struct {
		Status   string `json:"status"`
		Price    string `json:"price"`
		Currency string `json:"currency"`
		Messages []struct {
			Code string `json:"code"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Status != "completed" || view.Price != "0.091" || view.Currency != "USD" {
		t.Fatalf("响应未保留完成订单金额/状态: %+v", view)
	}
	orders, messages, _, transitions := repo.snapshot()
	if orders[orderID].Status != domain.OrderCompleted || orders[orderID].LastProviderState != "user_local_complete" || orders[orderID].Cost != .091 || len(transitions) != 1 {
		t.Fatalf("本地完成状态或金额错误: %+v transitions=%v", orders[orderID], transitions)
	}
	if setStatus.Load() != 1 || getStatus.Load() != 1 {
		t.Fatalf("应各调用一次完成与状态确认: setStatus=%d getStatus=%d", setStatus.Load(), getStatus.Load())
	}
	found := false
	for _, message := range messages {
		if message.OrderID == orderID && message.Code == "987654" {
			found = true
		}
	}
	if !found {
		t.Fatal("状态确认收到的新验证码未保存")
	}
}
