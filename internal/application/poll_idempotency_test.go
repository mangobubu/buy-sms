package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type pollingRepository struct {
	store.Repository

	mu              sync.Mutex
	provider        domain.Provider
	order           domain.Order
	messages        []domain.SMSMessage
	statusChanges   []string
	pollUpdateCount int
}

type statusGenerationRepository struct {
	*lifecycleRepository
}

func (r *statusGenerationRepository) orderSnapshot() domain.Order {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order
}

func (r *statusGenerationRepository) SaveMessage(_ context.Context, message domain.SMSMessage, advance bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.order.Messages {
		if existing.UpstreamFingerprint == message.UpstreamFingerprint {
			return false, nil
		}
	}
	r.order.Messages = append(r.order.Messages, message)
	if advance {
		r.order.PollSequence++
		if r.order.CanGetAnotherSMS {
			r.order.RequestNextGeneration++
			if !r.order.RequestNextInflight {
				r.order.RequestNextPending = true
			}
		}
	}
	return true, nil
}

func (r *statusGenerationRepository) UpdatePoll(_ context.Context, id, state string, next time.Time, failures int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID == id {
		r.order.LastProviderState = state
		r.order.NextPollAt = next
		r.order.PollFailures = failures
	}
	return nil
}

func (r *statusGenerationRepository) UpdateRequestNext(_ context.Context, id string, pending bool, failures int, next time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID == id {
		r.order.RequestNextPending = pending
		r.order.RequestNextFailures = failures
		if pending && next.Before(r.order.NextPollAt) {
			r.order.NextPollAt = next
		}
	}
	return nil
}

func (r *pollingRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.provider.ID {
		return domain.Provider{}, store.ErrNotFound
	}
	return r.provider, nil
}

func (r *pollingRepository) SaveMessage(_ context.Context, message domain.SMSMessage, advance bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.messages {
		if existing.OrderID == message.OrderID && existing.UpstreamFingerprint == message.UpstreamFingerprint {
			return false, nil
		}
	}
	r.messages = append(r.messages, message)
	if advance {
		r.order.PollSequence++
	}
	return true, nil
}

func (r *pollingRepository) UpdatePoll(_ context.Context, id, state string, next time.Time, failures int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == r.order.ID {
		r.order.LastProviderState = state
		r.order.NextPollAt = next
		r.order.PollFailures = failures
	}
	r.pollUpdateCount++
	return nil
}

func (r *pollingRepository) SetOrderStatus(_ context.Context, id, status, providerState string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == r.order.ID {
		r.order.Status = status
		r.order.LastProviderState = providerState
	}
	r.statusChanges = append(r.statusChanges, status)
	return nil
}

func (r *pollingRepository) orderSnapshot() domain.Order {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order
}

func (r *pollingRepository) resultSnapshot() ([]domain.SMSMessage, []string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.SMSMessage(nil), r.messages...), append([]string(nil), r.statusChanges...), r.pollUpdateCount
}

func TestPollHistoryUsesStableProviderFingerprint(t *testing.T) {
	var requestCount atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := requestCount.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/activations/upstream-history/otp" {
			t.Errorf("轮询请求不正确: %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		if authorization := request.Header.Get("Authorization"); authorization != "ApiKey provider-secret" {
			t.Errorf("轮询鉴权头=%q", authorization)
		}
		writer.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = writer.Write([]byte(`{
				"status":"active",
				"otpList":[
					{"smsCode":"13579","smsText":"code 13579","generation":1}
				]
			}`))
			return
		}
		_, _ = writer.Write([]byte(`{
			"status":"active",
			"otpList":[
				{"smsCode":"13579","smsText":"code 13579","generation":1},
				{"smsCode":"13579","smsText":"code 13579","generation":2}
			]
		}`))
	}))
	t.Cleanup(providerServer.Close)

	vault, err := secure.NewVault([]byte("poll-idempotency-encryption-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKeyCipher, err := vault.Encrypt("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]any{"pollingIntervalSeconds": 30, "webhookEnabled": true})
	repo := &pollingRepository{
		provider: domain.Provider{
			ID: domain.ProviderHeroSMS, Name: "HeroSMS", Enabled: true,
			BaseURL: providerServer.URL + "/api/v1", APIKeyCipher: apiKeyCipher, Config: settings,
		},
		order: domain.Order{
			ID: "order-history", UserID: "user-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "upstream-history", Status: domain.OrderActive,
		},
	}
	service := New(repo, nil, vault, config.Config{})

	service.pollOne(context.Background(), repo.orderSnapshot())
	service.pollOne(context.Background(), repo.orderSnapshot())

	messages, statusChanges, pollUpdates := repo.resultSnapshot()
	if count := requestCount.Load(); count != 2 {
		t.Fatalf("供应商轮询次数=%d，期望=2", count)
	}
	if pollUpdates != 2 {
		t.Fatalf("轮询状态更新次数=%d，期望=2", pollUpdates)
	}
	if len(messages) != 2 {
		t.Fatalf("完整历史连续轮询后应保留两个唯一代次，实际=%d，消息=%+v", len(messages), messages)
	}
	if messages[0].Code != "13579" || messages[1].Code != "13579" {
		t.Fatalf("同码多代次内容错误: %+v", messages)
	}
	if messages[0].UpstreamFingerprint == messages[1].UpstreamFingerprint {
		t.Fatalf("不同 Generation/Fingerprint 必须形成不同幂等键: %+v", messages)
	}
	if len(statusChanges) != 0 || repo.orderSnapshot().Status != domain.OrderActive {
		t.Fatalf("多次收到验证码后订单仍应保持 active，状态=%q，迁移=%v", repo.orderSnapshot().Status, statusChanges)
	}
}

func TestSMSBowerGetStatusSameCodeAfterRetryCreatesNextGeneration(t *testing.T) {
	var statusCalls atomic.Int32
	var requestAnotherCalls atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Query().Get("action") {
		case "getAllSms":
			_, _ = writer.Write([]byte("BAD_ACTION"))
		case "getStatus":
			switch statusCalls.Add(1) {
			case 1:
				_, _ = writer.Write([]byte("STATUS_OK:2468"))
			case 2:
				_, _ = writer.Write([]byte("STATUS_OK:2468"))
			case 3:
				_, _ = writer.Write([]byte("STATUS_WAIT_RETRY:2468"))
			default:
				_, _ = writer.Write([]byte("STATUS_OK:2468"))
			}
		case "setStatus":
			requestAnotherCalls.Add(1)
			_, _ = writer.Write([]byte("ACCESS_RETRY_GET"))
		default:
			http.Error(writer, "BAD_ACTION", http.StatusBadRequest)
		}
	}))
	t.Cleanup(providerServer.Close)

	vault, err := secure.NewVault([]byte("smsbower-generation-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, _ := vault.Encrypt("provider-secret")
	baseRepo := &lifecycleRepository{
		provider: domain.Provider{
			ID: domain.ProviderSMSBower, BaseURL: providerServer.URL + "/stubs/handler_api.php",
			APIKeyCipher: apiKey, Config: json.RawMessage(`{"pollingIntervalSeconds":30}`),
		},
		order: domain.Order{
			ID: "smsbower-generation", ProviderID: domain.ProviderSMSBower, UpstreamID: "upstream-generation",
			Status: domain.OrderActive, CanGetAnotherSMS: true,
		},
	}
	repo := &statusGenerationRepository{lifecycleRepository: baseRepo}
	service := New(repo, nil, vault, config.Config{})

	service.pollOne(context.Background(), repo.orderSnapshot())
	service.pollOne(context.Background(), repo.orderSnapshot())
	service.pollOne(context.Background(), repo.orderSnapshot())
	service.pollOne(context.Background(), repo.orderSnapshot())

	order := repo.orderSnapshot()
	if len(order.Messages) != 2 {
		t.Fatalf("相同验证码进入下一 generation 后应保存两条，实际=%d，消息=%+v", len(order.Messages), order.Messages)
	}
	if order.Messages[0].Code != "2468" || order.Messages[1].Code != "2468" {
		t.Fatalf("下一 generation 验证码内容异常: %+v", order.Messages)
	}
	if order.Messages[0].UpstreamFingerprint == order.Messages[1].UpstreamFingerprint {
		t.Fatalf("下一 generation 应生成不同幂等键: %+v", order.Messages)
	}
	if order.Status != domain.OrderActive {
		t.Fatalf("多验证码期间订单应保持 active，实际=%s", order.Status)
	}
	if statusCalls.Load() != 4 || requestAnotherCalls.Load() != 2 {
		t.Fatalf("SMSBower 状态/下一代请求次数异常: status=%d requestAnother=%d", statusCalls.Load(), requestAnotherCalls.Load())
	}
}
