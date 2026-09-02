package application

import (
	"context"
	"encoding/json"
	"errors"
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

type purchaseRepository struct {
	store.Repository

	mu        sync.Mutex
	provider  domain.Provider
	records   map[string]store.PurchaseRecord
	failCodes map[string]string
	orders    map[string]domain.Order
	reserves  int
	completes int
}

func newPurchaseRepository(provider domain.Provider) *purchaseRepository {
	return &purchaseRepository{
		provider: provider,
		records:  make(map[string]store.PurchaseRecord), failCodes: make(map[string]string),
		orders: make(map[string]domain.Order),
	}
}

func (r *purchaseRepository) ReservePurchase(_ context.Context, record store.PurchaseRecord) (store.PurchaseRecord, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reserves++
	key := record.UserID + "\x00" + record.IdempotencyKey
	if existing, ok := r.records[key]; ok {
		return existing, false, nil
	}
	record.Status = "provisioning"
	r.records[key] = record
	return record, true, nil
}

func (r *purchaseRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.provider.ID != id {
		return domain.Provider{}, store.ErrNotFound
	}
	return r.provider, nil
}

func (r *purchaseRepository) CompletePurchase(_ context.Context, recordID string, order domain.Order) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, record := range r.records {
		if record.ID != recordID || record.Status != "provisioning" {
			continue
		}
		record.Status = "succeeded"
		record.OrderID = order.ID
		r.records[key] = record
		r.orders[order.ID] = order
		r.completes++
		return nil
	}
	return store.ErrConflict
}

func (r *purchaseRepository) FailPurchase(ctx context.Context, recordID, status, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, record := range r.records {
		if record.ID != recordID || record.Status != "provisioning" {
			continue
		}
		record.Status = status
		r.records[key] = record
		r.failCodes[key] = code
		return nil
	}
	return nil
}

func (r *purchaseRepository) GetOrder(_ context.Context, id, userID string) (domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	order, ok := r.orders[id]
	if !ok || (userID != "" && order.UserID != userID) {
		return domain.Order{}, store.ErrNotFound
	}
	return order, nil
}

func (r *purchaseRepository) Audit(context.Context, *string, string, string, string, string, json.RawMessage) error {
	return nil
}

func (r *purchaseRepository) snapshot(userID, key string) (store.PurchaseRecord, string, int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lookup := userID + "\x00" + key
	return r.records[lookup], r.failCodes[lookup], len(r.orders), r.reserves, r.completes
}

func purchaseInput(maxPrice, idempotencyKey string) PurchaseInput {
	return PurchaseInput{
		Provider: domain.ProviderHeroSMS, CountryCode: "2", ServiceCode: "tg",
		MaxPrice: maxPrice, IdempotencyKey: idempotencyKey,
	}
}

func purchaseService(t *testing.T, repo *purchaseRepository) *Service {
	t.Helper()
	vault, err := secure.NewVault([]byte("purchase-test-encryption-key"))
	if err != nil {
		t.Fatal(err)
	}
	return New(repo, nil, vault, config.Config{})
}

func TestPurchaseRejectsNonFiniteAndExcessiveMaximumBeforeProviderCall(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls.Add(1)
	}))
	t.Cleanup(server.Close)
	vault, err := secure.NewVault([]byte("purchase-validation-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey, Enabled: true,
	})
	service := New(repo, nil, vault, config.Config{})
	user := domain.User{ID: "operator-purchase", Role: "operator"}

	for _, maxPrice := range []string{"NaN", "Inf", "+Inf", "-Inf", "1000000.000001", "1e309"} {
		t.Run(maxPrice, func(t *testing.T) {
			_, callErr := service.Purchase(context.Background(), purchaseInput(maxPrice, "idem-validation-1234567890"), user, "127.0.0.1")
			if !errors.Is(callErr, ErrBadRequest) {
				t.Fatalf("maxPrice=%q 错误=%v，期望参数错误", maxPrice, callErr)
			}
		})
	}
	if calls := providerCalls.Load(); calls != 0 {
		t.Fatalf("非法价格不应调用供应商，实际=%d", calls)
	}
	_, _, orders, reserves, completes := repo.snapshot(user.ID, "idem-validation-1234567890")
	if reserves != 0 || completes != 0 || orders != 0 {
		t.Fatalf("非法价格不应创建购买意图或订单: reserves=%d completes=%d orders=%d", reserves, completes, orders)
	}
}

func TestPurchaseCancelsUpstreamWhenActualCostExceedsMaximum(t *testing.T) {
	var purchaseCalls, cancelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/activations":
			purchaseCalls.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":[{"id":"expensive-upstream","phone":"77001112233","price":10.01}]}`))
		case request.Method == http.MethodDelete && request.URL.Path == "/api/v1/activations/expensive-upstream":
			cancelCalls.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	vault, _ := secure.NewVault([]byte("purchase-price-cap-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey, Enabled: true,
	})
	service := New(repo, nil, vault, config.Config{})
	user := domain.User{ID: "operator-price-cap", Role: "operator"}
	const key = "idem-price-cap-1234567890"

	_, callErr := service.Purchase(context.Background(), purchaseInput("10", key), user, "127.0.0.1")
	if !errors.Is(callErr, ErrConflict) {
		t.Fatalf("超价购买错误=%v，期望冲突", callErr)
	}
	record, code, orders, _, completes := repo.snapshot(user.ID, key)
	if purchaseCalls.Load() != 1 || cancelCalls.Load() != 1 {
		t.Fatalf("上游购买/补偿取消次数异常: purchase=%d cancel=%d", purchaseCalls.Load(), cancelCalls.Load())
	}
	if record.Status != "failed" || code != "price_exceeded" || orders != 0 || completes != 0 {
		t.Fatalf("超价意图状态异常: record=%+v code=%q orders=%d completes=%d", record, code, orders, completes)
	}
}

func TestConcurrentPurchaseWithSameIdempotencyKeyCallsUpstreamOnce(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstreamEntered := make(chan struct{})
	releaseUpstream := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/activations" {
			http.NotFound(writer, request)
			return
		}
		if upstreamCalls.Add(1) == 1 {
			close(upstreamEntered)
		}
		<-releaseUpstream
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"id":"single-upstream","phone":"77002223344","price":0.2}]}`))
	}))
	t.Cleanup(server.Close)
	vault, _ := secure.NewVault([]byte("purchase-concurrent-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey, Enabled: true,
	})
	service := New(repo, nil, vault, config.Config{})
	user := domain.User{ID: "operator-idempotent", Role: "operator"}
	const key = "idem-concurrent-1234567890"
	firstResult := make(chan error, 1)
	go func() {
		_, callErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
		firstResult <- callErr
	}()
	select {
	case <-upstreamEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("首个购买请求未进入供应商")
	}
	_, secondErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
	if !errors.Is(secondErr, ErrConflict) {
		close(releaseUpstream)
		t.Fatalf("并发同幂等键第二请求错误=%v，期望冲突", secondErr)
	}
	close(releaseUpstream)
	if firstErr := <-firstResult; firstErr != nil {
		t.Fatalf("首个购买请求失败: %v", firstErr)
	}
	record, _, orders, reserves, completes := repo.snapshot(user.ID, key)
	if upstreamCalls.Load() != 1 || reserves != 2 || completes != 1 || orders != 1 || record.Status != "succeeded" {
		t.Fatalf("幂等购买结果异常: upstream=%d reserves=%d completes=%d orders=%d record=%+v", upstreamCalls.Load(), reserves, completes, orders, record)
	}
}

func TestPurchaseTimeoutMarksUnknownAndRetryDoesNotCallUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		select {
		case <-request.Context().Done():
		case <-releaseHandler:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseHandler) })
	vault, _ := secure.NewVault([]byte("purchase-timeout-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey, Enabled: true,
	})
	service := New(repo, nil, vault, config.Config{})
	user := domain.User{ID: "operator-timeout", Role: "operator"}
	const key = "idem-timeout-1234567890"
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	_, firstErr := service.Purchase(ctx, purchaseInput("1", key), user, "127.0.0.1")
	if !errors.Is(firstErr, ErrProvider) {
		t.Fatalf("超时购买错误=%v，期望供应商错误", firstErr)
	}
	record, code, orders, _, completes := repo.snapshot(user.ID, key)
	if record.Status != "unknown" || code != "provider_error" || orders != 0 || completes != 0 {
		t.Fatalf("超时意图状态异常: record=%+v code=%q orders=%d completes=%d", record, code, orders, completes)
	}
	_, retryErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
	if !errors.Is(retryErr, ErrConflict) {
		t.Fatalf("unknown 意图同键重试错误=%v，期望冲突", retryErr)
	}
	if calls := upstreamCalls.Load(); calls != 1 {
		t.Fatalf("unknown 意图重试不应再次调用上游，实际=%d", calls)
	}
}
