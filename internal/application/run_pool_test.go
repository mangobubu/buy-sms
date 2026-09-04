package application

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type runPoolRepository struct {
	store.Repository

	mu        sync.Mutex
	providers map[string]domain.Provider
	due       []domain.Order
	claimed   bool
	updates   map[string]string
	updated   chan string
}

func (r *runPoolRepository) WithOrderLock(ctx context.Context, _ string, callback func(context.Context) error) error {
	return callback(ctx)
}

func (r *runPoolRepository) GetOrder(_ context.Context, id, _ string) (domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, order := range r.due {
		if order.ID == id {
			return order, nil
		}
	}
	return domain.Order{}, store.ErrNotFound
}
func (r *runPoolRepository) ClaimDueRenewals(context.Context, int, time.Time, time.Duration) ([]domain.Order, error) {
	return []domain.Order{}, nil
}
func (r *runPoolRepository) ClaimDueOrders(context.Context, int, time.Time, time.Duration) ([]domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimed {
		return []domain.Order{}, nil
	}
	r.claimed = true
	return append([]domain.Order(nil), r.due...), nil
}

func (r *runPoolRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	provider, ok := r.providers[id]
	if !ok {
		return domain.Provider{}, store.ErrNotFound
	}
	return provider, nil
}

func (r *runPoolRepository) UpdatePoll(_ context.Context, id, state string, _ time.Time, _ int) error {
	r.mu.Lock()
	r.updates[id] = state
	r.mu.Unlock()
	select {
	case r.updated <- id:
	default:
	}
	return nil
}

func (r *runPoolRepository) SetOrderStatus(_ context.Context, id, status, _ string) error {
	r.mu.Lock()
	r.updates[id] = status
	r.mu.Unlock()
	select {
	case r.updated <- id:
	default:
	}
	return nil
}

func (r *runPoolRepository) Maintenance(context.Context, time.Time) error { return nil }

func TestRunWorkerPoolLetsFastProviderFinishAndStopsAfterCancellation(t *testing.T) {
	slowEntered := make(chan struct{})
	slowCanceled := make(chan struct{})
	fastServed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/slow/api/v1/activations/slow-upstream/otp":
			select {
			case <-slowEntered:
			default:
				close(slowEntered)
			}
			<-request.Context().Done()
			close(slowCanceled)
		case "/fast/api/v1/activations/fast-upstream/otp":
			close(fastServed)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"status":"waiting","otpList":[]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	vault, err := secure.NewVault([]byte("bounded-worker-pool-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, _ := vault.Encrypt("provider-secret")
	settings, _ := json.Marshal(map[string]any{"pollingIntervalSeconds": 30})
	repo := &runPoolRepository{
		providers: map[string]domain.Provider{
			"slow-provider": {ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/slow/api/v1", APIKeyCipher: apiKey, Config: settings},
			"fast-provider": {ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/fast/api/v1", APIKeyCipher: apiKey, Config: settings},
		},
		due: []domain.Order{
			{ID: "slow-order", ProviderID: "slow-provider", UpstreamID: "slow-upstream", Status: domain.OrderActive},
			{ID: "fast-order", ProviderID: "fast-provider", UpstreamID: "fast-upstream", Status: domain.OrderActive},
		},
		updates: make(map[string]string), updated: make(chan string, 8),
	}
	// providerClient 根据订单 ProviderID 查询配置后还会用 Provider.ID 创建客户端，
	// 因此测试仓储按测试别名查找，返回的真实供应商 ID 保持 HeroSMS。
	service := New(repo, nil, vault, config.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Run(ctx)
	}()

	select {
	case <-slowEntered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("慢供应商轮询未开始")
	}
	select {
	case <-fastServed:
	case <-time.After(500 * time.Millisecond):
		cancel()
		t.Fatal("慢供应商阻塞了快供应商任务")
	}
	select {
	case id := <-repo.updated:
		if id != "fast-order" {
			t.Fatalf("首个完成的轮询任务=%q，期望快订单", id)
		}
	case <-time.After(500 * time.Millisecond):
		cancel()
		t.Fatal("快供应商结果未及时保存")
	}

	cancel()
	select {
	case <-slowCanceled:
	case <-time.After(time.Second):
		t.Fatal("取消后慢供应商请求未退出")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("取消后有界任务池未退出")
	}

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if _, ok := repo.updates["fast-order"]; !ok {
		t.Fatalf("快订单没有轮询结果: %v", repo.updates)
	}
	if len(repo.updates) > 2 {
		t.Fatalf("任务池产生异常更新: %s", fmt.Sprint(repo.updates))
	}
}
