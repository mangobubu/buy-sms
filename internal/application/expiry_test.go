package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

func TestOrderViewExposesProviderExpiry(t *testing.T) {
	expiresAt := time.Date(2026, time.September, 4, 12, 34, 56, 0, time.UTC)
	view := OrderView(domain.Order{
		ID: "order-with-expiry", ProviderID: domain.ProviderHeroSMS,
		Status: domain.OrderActive, ExpiresAt: &expiresAt,
	}, false, expiresAt.Add(-time.Minute))
	if view.ExpiresAt == nil || !view.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("订单视图过期时间=%v，期望=%s", view.ExpiresAt, expiresAt)
	}
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"expiresAt":"2026-09-04T12:34:56Z"`) {
		t.Fatalf("订单 JSON 未暴露供应商过期时间: %s", payload)
	}

	withoutExpiry := OrderView(domain.Order{ID: "order-without-expiry"}, false, expiresAt)
	payload, err = json.Marshal(withoutExpiry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "expiresAt") {
		t.Fatalf("供应商未返回期限时不应构造过期时间: %s", payload)
	}
}

func TestPurchaseUsesOnlyProviderExpiry(t *testing.T) {
	providerExpiry := time.Date(2026, time.September, 4, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		response   string
		wantExpiry *time.Time
	}{
		{
			name:     "供应商未返回期限时保持未知",
			response: `{"data":[{"id":"upstream-no-expiry","phone":"77001112222","price":0.2}]}`,
		},
		{
			name:       "透传供应商绝对期限",
			response:   `{"data":[{"id":"upstream-with-expiry","phone":"77003334444","price":0.2,"expiredAt":"2026-09-04T13:00:00Z"}]}`,
			wantExpiry: &providerExpiry,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/api/v1/activations" {
					http.NotFound(writer, request)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(tt.response))
			}))
			t.Cleanup(server.Close)

			vault, err := secure.NewVault([]byte("provider-expiry-purchase-test-key"))
			if err != nil {
				t.Fatal(err)
			}
			apiKey, err := vault.Encrypt("provider-secret")
			if err != nil {
				t.Fatal(err)
			}
			repo := newPurchaseRepository(domain.Provider{
				ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1",
				APIKeyCipher: apiKey, Enabled: true,
				Config: json.RawMessage(`{}`),
			})
			service := New(repo, nil, vault, config.Config{})
			service.now = func() time.Time {
				return time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
			}
			input := purchaseInput("1", "expiry-purchase-idempotency-12345")
			view, err := service.Purchase(
				context.Background(), input,
				domain.User{ID: "expiry-test-user", Role: "operator"}, "127.0.0.1",
			)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := repo.GetOrder(context.Background(), view.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantExpiry == nil {
				if view.ExpiresAt != nil || stored.ExpiresAt != nil {
					t.Fatalf("供应商未返回期限却生成了兜底值: view=%v stored=%v", view.ExpiresAt, stored.ExpiresAt)
				}
				return
			}
			if view.ExpiresAt == nil || stored.ExpiresAt == nil ||
				!view.ExpiresAt.Equal(*tt.wantExpiry) || !stored.ExpiresAt.Equal(*tt.wantExpiry) {
				t.Fatalf("供应商期限未完整透传: view=%v stored=%v want=%s", view.ExpiresAt, stored.ExpiresAt, tt.wantExpiry)
			}
		})
	}
}

type expiryPollingRepository struct {
	store.Repository
	provider        domain.Provider
	order           domain.Order
	messages        []domain.SMSMessage
	nextPoll        time.Time
	expiryUpdateErr error
}

func (r *expiryPollingRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	if id != r.provider.ID {
		return domain.Provider{}, store.ErrNotFound
	}
	return r.provider, nil
}

func (r *expiryPollingRepository) UpdateOrderExpiresAt(_ context.Context, id string, expiresAt time.Time) error {
	if r.expiryUpdateErr != nil {
		return r.expiryUpdateErr
	}
	if id == r.order.ID {
		r.order.ExpiresAt = &expiresAt
	}
	return nil
}

func (r *expiryPollingRepository) SaveMessage(_ context.Context, message domain.SMSMessage, _ bool) (bool, error) {
	r.messages = append(r.messages, message)
	return true, nil
}

func (r *expiryPollingRepository) UpdatePoll(_ context.Context, id, state string, next time.Time, failures int) error {
	if id == r.order.ID {
		r.order.LastProviderState = state
		r.order.PollFailures = failures
		r.nextPoll = next
	}
	return nil
}

func (r *expiryPollingRepository) WithOrderLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

func (r *expiryPollingRepository) GetOrder(_ context.Context, id, _ string) (domain.Order, error) {
	if id != r.order.ID {
		return domain.Order{}, store.ErrNotFound
	}
	return r.order, nil
}

func (r *expiryPollingRepository) SetOrderStatus(_ context.Context, id, status, providerState string) error {
	if id == r.order.ID {
		r.order.Status = status
		r.order.LastProviderState = providerState
	}
	return nil
}

func TestPollPersistsExpiryWithoutDroppingMessagesOrProviderTerminalState(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		response       string
		wantStatus     string
		wantState      string
		wantMessages   int
		wantNextPollAt *time.Time
		initialExpiry  *time.Time
	}{
		{
			name:       "已过期响应仍先保存短信",
			response:   `{"status":"active","expiresAt":"2026-09-04T11:59:59Z","otpList":[{"id":"message-1","smsCode":"24680","smsText":"code 24680"}]}`,
			wantStatus: domain.OrderExpired, wantState: "local_expired", wantMessages: 1,
		},
		{
			name:       "供应商终态优先于本地期限",
			response:   `{"status":"completed","expiresAt":"2026-09-04T11:59:59Z","otpList":[{"id":"terminal-message","smsCode":"86420","smsText":"code 86420"}]}`,
			wantStatus: domain.OrderCompleted, wantState: "completed", wantMessages: 1,
			initialExpiry: func() *time.Time {
				value := now.Add(-time.Second)
				return &value
			}(),
		},
		{
			name:       "下一次轮询不晚于供应商期限",
			response:   `{"status":"waiting","expiresAt":"2026-09-04T12:00:10Z","otpList":[]}`,
			wantStatus: domain.OrderActive, wantState: "waiting",
			wantNextPollAt: func() *time.Time {
				value := now.Add(10 * time.Second)
				return &value
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/api/v1/activations/upstream-expiry/otp" {
					http.NotFound(writer, request)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(tt.response))
			}))
			t.Cleanup(server.Close)

			vault, err := secure.NewVault([]byte("provider-expiry-poll-test-key"))
			if err != nil {
				t.Fatal(err)
			}
			apiKey, err := vault.Encrypt("provider-secret")
			if err != nil {
				t.Fatal(err)
			}
			repo := &expiryPollingRepository{
				provider: domain.Provider{
					ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1",
					APIKeyCipher: apiKey, Enabled: true,
					Config: json.RawMessage(`{"pollingIntervalSeconds":30}`),
				},
				order: domain.Order{
					ID: "order-expiry", ProviderID: domain.ProviderHeroSMS,
					UpstreamID: "upstream-expiry", Status: domain.OrderActive,
					ExpiresAt: tt.initialExpiry,
				},
			}
			service := New(repo, nil, vault, config.Config{})
			service.now = func() time.Time { return now }
			service.pollOne(context.Background(), repo.order)

			if repo.order.Status != tt.wantStatus || repo.order.LastProviderState != tt.wantState {
				t.Fatalf("轮询终态=(%q,%q)，期望=(%q,%q)", repo.order.Status, repo.order.LastProviderState, tt.wantStatus, tt.wantState)
			}
			if len(repo.messages) != tt.wantMessages {
				t.Fatalf("保存短信数=%d，期望=%d", len(repo.messages), tt.wantMessages)
			}
			if repo.order.ExpiresAt == nil {
				t.Fatal("轮询返回的供应商期限未写回订单")
			}
			if tt.wantNextPollAt != nil && !repo.nextPoll.Equal(*tt.wantNextPollAt) {
				t.Fatalf("下次轮询=%s，期望按供应商期限截断为 %s", repo.nextPoll, tt.wantNextPollAt)
			}
		})
	}
}

func TestPollExpiryWriteFailureDoesNotDropCurrentMessages(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"status":"active",
			"expiresAt":"2026-09-04T12:05:00Z",
			"otpList":[{"id":"message-after-expiry-write-error","smsCode":"13579","smsText":"code 13579"}]
		}`))
	}))
	t.Cleanup(server.Close)

	vault, err := secure.NewVault([]byte("provider-expiry-write-failure-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, err := vault.Encrypt("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	repo := &expiryPollingRepository{
		provider: domain.Provider{
			ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1",
			APIKeyCipher: apiKey, Enabled: true,
		},
		order: domain.Order{
			ID: "order-expiry-write-error", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "upstream-expiry-write-error", Status: domain.OrderActive,
		},
		expiryUpdateErr: store.ErrConflict,
	}
	service := New(repo, nil, vault, config.Config{})
	service.now = func() time.Time { return now }
	service.pollOne(context.Background(), repo.order)

	if len(repo.messages) != 1 || repo.messages[0].Code != "13579" {
		t.Fatalf("期限写入失败后本次短信未保存: %+v", repo.messages)
	}
	if repo.order.Status != domain.OrderActive || repo.order.LastProviderState != "database_error" {
		t.Fatalf("期限写入失败后的订单状态异常: %+v", repo.order)
	}
}

func TestNextPollAtUsesKnownProviderExpiry(t *testing.T) {
	candidate := time.Date(2026, time.September, 4, 12, 1, 0, 0, time.UTC)
	earlier := candidate.Add(-20 * time.Second)
	if got := nextPollAt(candidate, &earlier); !got.Equal(earlier) {
		t.Fatalf("下次轮询=%s，期望=%s", got, earlier)
	}
	later := candidate.Add(time.Second)
	if got := nextPollAt(candidate, &later); !got.Equal(candidate) {
		t.Fatalf("未到期限时下次轮询=%s，期望=%s", got, candidate)
	}
}
