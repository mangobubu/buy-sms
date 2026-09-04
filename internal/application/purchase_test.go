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
	"buysms/internal/provider"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type purchaseRepository struct {
	store.Repository

	mu          sync.Mutex
	provider    domain.Provider
	records     map[string]store.PurchaseRecord
	failCodes   map[string]string
	orders      map[string]domain.Order
	reserves    int
	completes   int
	completeErr error
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
	if r.completeErr != nil {
		return r.completeErr
	}
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
		record.ErrorCode = code
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

func requirePurchaseError(t *testing.T, err error, code, message string) *PurchaseError {
	t.Helper()
	var purchaseErr *PurchaseError
	if !errors.As(err, &purchaseErr) {
		t.Fatalf("错误类型=%T，期望 *PurchaseError；错误=%v", err, err)
	}
	if purchaseErr.Code != code || purchaseErr.Message != message {
		t.Fatalf("购买错误=(%q, %q)，期望=(%q, %q)", purchaseErr.Code, purchaseErr.Message, code, message)
	}
	return purchaseErr
}

func TestPurchaseErrorKeepsUncertainFailuresDistinct(t *testing.T) {
	definiteFailureCodes := map[string]bool{
		"configuration":            true,
		"duration_unavailable":     true,
		"insufficient_balance":     true,
		"invalid_selection":        true,
		"no_numbers":               true,
		"provider_disabled":        true,
		"provider_rate_limited":    true,
		"provider_preflight_error": true,
		"price_exceeded":           true,
		"purchase_setup_failed":    true,
	}
	tests := []struct {
		name string
		code string
		kind error
	}{
		{name: "供应商调用失败", code: "provider_error", kind: ErrProvider},
		{name: "购买结果未知", code: "purchase_result_unknown", kind: ErrConflict},
		{name: "订单落库失败", code: "database_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cause := errors.New("内部原因")
			purchaseErr := purchaseError(tt.code, cause)
			if purchaseErr.Code != tt.code || definiteFailureCodes[purchaseErr.Code] {
				t.Fatalf("不确定购买结果错误码=%q，不应标为明确失败", purchaseErr.Code)
			}
			if tt.kind != nil && !errors.Is(purchaseErr, tt.kind) {
				t.Fatalf("错误 %q 未保留 errors.Is(%v) 分类", tt.code, tt.kind)
			}
			if !errors.Is(purchaseErr, cause) {
				t.Fatalf("错误 %q 未保留原始原因", tt.code)
			}
		})
	}
}

func TestProviderPurchaseUnknownReason(t *testing.T) {
	tests := []struct {
		name, want string
		err        error
	}{
		{"请求超时", purchaseUnknownProviderTimeout, &provider.ProviderError{Code: "TIMEOUT"}},
		{"连接中断", purchaseUnknownProviderConnection, &provider.ProviderError{Code: "TRANSPORT_ERROR"}},
		{"请求取消", purchaseUnknownProviderConnection, &provider.ProviderError{Code: "CANCELED"}},
		{"HTTP 5xx", purchaseUnknownProviderHTTP, &provider.ProviderError{Code: "NO_BALANCE", HTTPStatus: http.StatusServiceUnavailable}},
		{"HTTP 429", purchaseUnknownProviderHTTP, &provider.ProviderError{Code: "RATE_LIMIT", HTTPStatus: http.StatusTooManyRequests}},
		{"响应读取失败", purchaseUnknownProviderRead, &provider.ProviderError{Code: "READ_ERROR"}},
		{"响应格式异常", purchaseUnknownProviderResponse, &provider.ProviderError{Code: "INVALID_RESPONSE"}},
		{"响应内容过大", purchaseUnknownProviderResponse, &provider.ProviderError{Code: "RESPONSE_TOO_LARGE"}},
		{"未知供应商错误", "provider_error", &provider.ProviderError{Code: "UNKNOWN"}},
		{"裸超时错误", purchaseUnknownProviderTimeout, context.DeadlineExceeded},
		{"裸取消错误", purchaseUnknownProviderConnection, context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerPurchaseUnknownReason(tt.err); got != tt.want {
				t.Fatalf("未知原因=%q，期望=%q", got, tt.want)
			}
			initial := providerPurchaseError(tt.want, tt.err)
			if initial.Code != "provider_error" || !errors.Is(initial, ErrProvider) || !errors.Is(initial, tt.err) {
				t.Fatalf("首次供应商错误契约异常: %+v", initial)
			}
			replay := purchaseResultUnknownError(tt.want, nil)
			if replay.Code != "purchase_result_unknown" || !errors.Is(replay, ErrConflict) {
				t.Fatalf("重放错误契约异常: %+v", replay)
			}
		})
	}
}

func TestClassifyProviderPurchasePreflightErrorsAsFailed(t *testing.T) {
	tests := []struct {
		name, operation, providerCode, wantCode string
		httpStatus                              int
	}{
		{"目录请求超时", "catalog.services", "TIMEOUT", "provider_preflight_error", 0},
		{"等级响应异常", "catalog.tier", "INVALID_RESPONSE", "provider_preflight_error", 0},
		{"目录 HTTP 5xx", "catalog.services", "UPSTREAM_ERROR", "provider_preflight_error", http.StatusServiceUnavailable},
		{"等级暂无号码", "purchase.tier", "NO_NUMBERS", "no_numbers", 0},
		{"目录服务无效", "catalog.services", "BAD_SERVICE", "invalid_selection", 0},
		{"长租目录时长无效", "catalog.duration.rent", "BAD_DURATION", "duration_unavailable", 0},
		{"长租购买时长失效", "purchase", "BAD_DURATION", "duration_unavailable", http.StatusUnprocessableEntity},
		{"长租国家无效", "catalog.duration.rent", "WRONG_COUNTRY", "invalid_selection", 0},
		{"长租服务无效", "catalog.duration.rent", "WRONG_SERVICE", "invalid_selection", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &provider.ProviderError{Operation: tt.operation, Code: tt.providerCode, HTTPStatus: tt.httpStatus}
			status, code := classifyProviderPurchaseError(err)
			if status != "failed" || code != tt.wantCode {
				t.Fatalf("前置错误分类=(%q,%q)，期望=(failed,%q)", status, code, tt.wantCode)
			}
		})
	}
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
	requirePurchaseError(t, callErr, "price_exceeded", "供应商实际价格超过所选价格，购买已取消")
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
	requirePurchaseError(t, secondErr, "purchase_in_progress", "购买请求仍在处理中，系统尚未收到最终结果；请等待最多2分钟，再使用当前请求重试确认；为避免重复扣费，仅暂停该请求的重复提交")
	close(releaseUpstream)
	if firstErr := <-firstResult; firstErr != nil {
		t.Fatalf("首个购买请求失败: %v", firstErr)
	}
	record, _, orders, reserves, completes := repo.snapshot(user.ID, key)
	if upstreamCalls.Load() != 1 || reserves != 2 || completes != 1 || orders != 1 || record.Status != "succeeded" {
		t.Fatalf("幂等购买结果异常: upstream=%d reserves=%d completes=%d orders=%d record=%+v", upstreamCalls.Load(), reserves, completes, orders, record)
	}
}

func TestPurchaseIdempotencyIncludesQualityTier(t *testing.T) {
	const (
		userID = "operator-tier-idempotency"
		key    = "idem-tier-1234567890"
	)
	repo := newPurchaseRepository(domain.Provider{ID: domain.ProviderSMSBower, Enabled: true})
	repo.records[userID+string(rune(0))+key] = store.PurchaseRecord{
		ID: "purchase-tier", UserID: userID, IdempotencyKey: key,
		ProviderID: domain.ProviderSMSBower, CountryCode: "2", ServiceCode: "tg",
		QualityTier: "gold", MaxPrice: 1, Status: "succeeded", OrderID: "order-tier",
	}
	repo.orders["order-tier"] = domain.Order{
		ID: "order-tier", UserID: userID, ProviderID: domain.ProviderSMSBower,
		CountryCode: "2", ServiceCode: "tg", QualityTier: "gold",
	}
	service := purchaseService(t, repo)
	user := domain.User{ID: userID, Role: "operator"}

	in := purchaseInput("1", key)
	in.Provider = domain.ProviderSMSBower
	in.QualityTier = " Gold "
	order, err := service.Purchase(context.Background(), in, user, "127.0.0.1")
	if err != nil || order.QualityTier != "gold" {
		t.Fatalf("同等级幂等重放失败: order=%+v err=%v", order, err)
	}

	in.QualityTier = "silver"
	if _, err = service.Purchase(context.Background(), in, user, "127.0.0.1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("同一幂等键更换等级错误=%v，期望冲突", err)
	}
	requirePurchaseError(t, err, "idempotency_mismatch", "该购买编号已用于其他条件，页面将生成新的购买请求")
}

func TestPurchaseProvisioningBecomesUnknownAfterTwoMinutes(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		updatedAt   time.Time
		wantCode    string
		wantStatus  string
		wantStored  string
		wantMessage string
	}{
		{
			name: "未满两分钟仍在处理中", updatedAt: now.Add(-2*time.Minute + time.Nanosecond),
			wantCode: "purchase_in_progress", wantStatus: "provisioning",
			wantMessage: "购买请求仍在处理中，系统尚未收到最终结果；请等待最多2分钟，再使用当前请求重试确认；为避免重复扣费，仅暂停该请求的重复提交",
		},
		{
			name: "恰好两分钟转为结果未知", updatedAt: now.Add(-2 * time.Minute),
			wantCode: "purchase_result_unknown", wantStatus: "unknown", wantStored: purchaseUnknownStaleProvisioning,
			wantMessage: "购买结果尚未确认：上一次请求处理已中断或状态更新失败，系统无法确认供应商是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const (
				userID = "operator-provisioning-boundary"
				key    = "idem-provisioning-boundary-12345"
			)
			repo := newPurchaseRepository(domain.Provider{ID: domain.ProviderHeroSMS, Enabled: true})
			repo.records[userID+"\x00"+key] = store.PurchaseRecord{
				ID: "stored-provisioning", UserID: userID, IdempotencyKey: key,
				ProviderID: domain.ProviderHeroSMS, CountryCode: "2", ServiceCode: "tg",
				MaxPrice: 1, Status: "provisioning", CreatedAt: now.Add(-time.Hour), UpdatedAt: tt.updatedAt,
			}
			service := purchaseService(t, repo)
			service.now = func() time.Time { return now }
			_, callErr := service.Purchase(
				context.Background(), purchaseInput("1", key),
				domain.User{ID: userID, Role: "operator"}, "127.0.0.1",
			)
			requirePurchaseError(t, callErr, tt.wantCode, tt.wantMessage)
			record, code, _, _, _ := repo.snapshot(userID, key)
			if record.Status != tt.wantStatus || code != tt.wantStored {
				t.Fatalf("处理中边界状态异常: record=%+v code=%q", record, code)
			}
		})
	}
}

func TestPurchaseUnknownIntentDoesNotBlockAnotherProvider(t *testing.T) {
	var providerBCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/activations" {
			http.NotFound(writer, request)
			return
		}
		providerBCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"id":"provider-b-upstream","phone":"77009998877","price":0.2}]}`))
	}))
	t.Cleanup(server.Close)

	vault, _ := secure.NewVault([]byte("purchase-provider-isolation-key"))
	apiKey, _ := vault.Encrypt("provider-b-secret")
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey, Enabled: true,
	})
	user := domain.User{ID: "operator-provider-isolation", Role: "operator"}
	const (
		providerAKey = "idem-provider-a-unknown-12345"
		providerBKey = "idem-provider-b-new-12345678"
	)
	repo.records[user.ID+"\x00"+providerAKey] = store.PurchaseRecord{
		ID: "provider-a-unknown", UserID: user.ID, IdempotencyKey: providerAKey,
		ProviderID: domain.ProviderSMSBower, CountryCode: "2", ServiceCode: "tg",
		MaxPrice: 1, Status: "unknown", ErrorCode: "provider_error",
	}
	service := New(repo, nil, vault, config.Config{})

	providerBInput := purchaseInput("1", providerBKey)
	order, err := service.Purchase(context.Background(), providerBInput, user, "127.0.0.1")
	if err != nil {
		t.Fatalf("平台 A 的未知购买不应阻止平台 B 使用新编号购买: %v", err)
	}
	if order.Provider != domain.ProviderHeroSMS || order.PhoneNumber != "77009998877" || providerBCalls.Load() != 1 {
		t.Fatalf("平台 B 购买结果异常: order=%+v calls=%d", order, providerBCalls.Load())
	}

	providerBInput.IdempotencyKey = providerAKey
	_, err = service.Purchase(context.Background(), providerBInput, user, "127.0.0.1")
	requirePurchaseError(t, err, "idempotency_mismatch", "该购买编号已用于其他条件，页面将生成新的购买请求")
	if providerBCalls.Load() != 1 {
		t.Fatalf("平台 B 误用平台 A 编号时不应再次调用上游，实际=%d", providerBCalls.Load())
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
	requirePurchaseError(t, firstErr, "provider_error", "购买结果尚未确认：供应商请求超时，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交")
	record, code, orders, _, completes := repo.snapshot(user.ID, key)
	if record.Status != "unknown" || code != purchaseUnknownProviderTimeout || orders != 0 || completes != 0 {
		t.Fatalf("超时意图状态异常: record=%+v code=%q orders=%d completes=%d", record, code, orders, completes)
	}
	_, retryErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
	if !errors.Is(retryErr, ErrConflict) {
		t.Fatalf("unknown 意图同键重试错误=%v，期望冲突", retryErr)
	}
	requirePurchaseError(t, retryErr, "purchase_result_unknown", "购买结果尚未确认：供应商请求超时，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交")
	if calls := upstreamCalls.Load(); calls != 1 {
		t.Fatalf("unknown 意图重试不应再次调用上游，实际=%d", calls)
	}
}

func TestPurchaseClassifiesDeterministicProviderErrors(t *testing.T) {
	tests := []struct {
		name, upstreamCode, wantCode, wantMessage string
		wantKind                                  error
	}{
		{"供应商价格超过所选价格", "MAX_PRICE_EXCEEDED", "price_exceeded", "供应商实际价格超过所选价格，购买已取消", ErrConflict},
		{"供应商暂无号码", "NO_NUMBERS", "no_numbers", "所选条件当前暂无可用号码，请稍后重试或调整条件", ErrConflict},
		{"供应商余额不足", "NO_BALANCE", "insufficient_balance", "供应商账户余额不足，请联系管理员充值", ErrProvider},
		{"供应商国家无效", "BAD_COUNTRY", "invalid_selection", "供应商不支持所选国家或服务，请重新选择", ErrBadRequest},
		{"供应商密钥无效", "BAD_KEY", "configuration", "供应商配置不完整，请联系管理员", ErrProvider},
		{"供应商限流", "RATE_LIMIT", "provider_rate_limited", "供应商请求过于频繁，请稍后重试", ErrProvider},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				_, _ = writer.Write([]byte(tt.upstreamCode))
			}))
			t.Cleanup(server.Close)
			vault, _ := secure.NewVault([]byte("purchase-provider-classification-key"))
			apiKey, _ := vault.Encrypt("provider-secret")
			repo := newPurchaseRepository(domain.Provider{
				ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/handler_api.php", APIKeyCipher: apiKey, Enabled: true,
			})
			service := New(repo, nil, vault, config.Config{})
			user := domain.User{ID: "operator-provider-classification", Role: "operator"}
			key := "idem-provider-" + tt.wantCode + "-123456"

			_, callErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
			if !errors.Is(callErr, tt.wantKind) {
				t.Fatalf("错误=%v，期望 errors.Is(%v)", callErr, tt.wantKind)
			}
			requirePurchaseError(t, callErr, tt.wantCode, tt.wantMessage)
			record, code, orders, _, completes := repo.snapshot(user.ID, key)
			if record.Status != "failed" || record.ErrorCode != tt.wantCode || code != tt.wantCode || orders != 0 || completes != 0 {
				t.Fatalf("确定性错误状态异常: record=%+v code=%q orders=%d completes=%d", record, code, orders, completes)
			}

			_, retryErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
			requirePurchaseError(t, retryErr, tt.wantCode, tt.wantMessage)
			if upstreamCalls.Load() != 1 {
				t.Fatalf("同键重放不应重复调用供应商，实际=%d", upstreamCalls.Load())
			}
		})
	}
}

func TestPurchaseClassifiesHeroSMSProblemTitleAsDeterministicFailure(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("HeroSMS 购买请求不是有效 JSON: %v", err)
		}
		if body["maxPrice"] != 0.09 || body["fixedPrice"] != true {
			t.Errorf("HeroSMS 未按用户选择的档位精确购买: %#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = writer.Write([]byte(`{"title":"WRONG_MAX_PRICE","details":"The maximum price is less than the permitted price","info":{"min":0.108}}`))
	}))
	t.Cleanup(server.Close)

	vault, _ := secure.NewVault([]byte("purchase-herosms-problem-title-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey, Enabled: true,
	})
	service := New(repo, nil, vault, config.Config{})
	user := domain.User{ID: "operator-herosms-problem-title", Role: "operator"}
	const key = "idem-herosms-title-123456"

	_, callErr := service.Purchase(context.Background(), purchaseInput("0.09", key), user, "127.0.0.1")
	requirePurchaseError(t, callErr, "price_exceeded", "供应商实际价格超过所选价格，购买已取消")
	record, code, orders, _, completes := repo.snapshot(user.ID, key)
	if record.Status != "failed" || record.ErrorCode != "price_exceeded" || code != "price_exceeded" || orders != 0 || completes != 0 {
		t.Fatalf("HeroSMS 明确拒单被误判: record=%+v code=%q orders=%d completes=%d", record, code, orders, completes)
	}

	_, retryErr := service.Purchase(context.Background(), purchaseInput("0.09", key), user, "127.0.0.1")
	requirePurchaseError(t, retryErr, "price_exceeded", "供应商实际价格超过所选价格，购买已取消")
	if upstreamCalls.Load() != 1 {
		t.Fatalf("HeroSMS 明确拒单重放不应再次调用供应商，实际=%d", upstreamCalls.Load())
	}
}

func TestPurchaseKeepsHTTPServerErrorsUnknownEvenWithBusinessCode(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("NO_BALANCE"))
	}))
	t.Cleanup(server.Close)
	vault, _ := secure.NewVault([]byte("purchase-http-uncertain-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/handler_api.php", APIKeyCipher: apiKey, Enabled: true,
	})
	service := New(repo, nil, vault, config.Config{})
	user := domain.User{ID: "operator-http-uncertain", Role: "operator"}
	const key = "idem-http-uncertain-123456"

	_, callErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
	requirePurchaseError(t, callErr, "provider_error", "购买结果尚未确认：供应商返回服务异常或限流响应，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交")
	record, code, orders, _, completes := repo.snapshot(user.ID, key)
	if record.Status != "unknown" || record.ErrorCode != purchaseUnknownProviderHTTP || code != purchaseUnknownProviderHTTP || orders != 0 || completes != 0 {
		t.Fatalf("HTTP 5xx 必须保持结果待确认: record=%+v code=%q orders=%d completes=%d", record, code, orders, completes)
	}
	_, retryErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
	requirePurchaseError(t, retryErr, "purchase_result_unknown", "购买结果尚未确认：供应商返回服务异常或限流响应，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交")
	if upstreamCalls.Load() != 1 {
		t.Fatalf("HTTP 5xx 后同键重放不应再次调用供应商，实际=%d", upstreamCalls.Load())
	}
}

func TestPurchaseReplaysStoredFailureReason(t *testing.T) {
	tests := []struct {
		name, storedCode, wantCode, wantMessage string
		wantKind                                error
	}{
		{"超价", "price_exceeded", "price_exceeded", "供应商实际价格超过所选价格，购买已取消", ErrConflict},
		{"供应商停用", "provider_disabled", "provider_disabled", "所选供应商已停用，请选择其他供应商", ErrConflict},
		{"供应商前置查询错误", "provider_preflight_error", "provider_preflight_error", "购买前获取供应商资源失败，号码购买尚未提交；可以重新提交当前平台与购买条件", ErrProvider},
		{"配置错误", "configuration", "configuration", "供应商配置不完整，请联系管理员", ErrProvider},
		{"未知失败码", "legacy_failure", "purchase_failed", "购买请求已失败，请刷新页面后重试", ErrConflict},
		{"空失败码", "", "purchase_failed", "购买请求已失败，请刷新页面后重试", ErrConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const (
				userID = "operator-stored-failure"
				key    = "idem-stored-failure-12345"
			)
			repo := newPurchaseRepository(domain.Provider{ID: domain.ProviderHeroSMS, Enabled: true})
			repo.records[userID+"\x00"+key] = store.PurchaseRecord{
				ID: "stored-failure", UserID: userID, IdempotencyKey: key,
				ProviderID: domain.ProviderHeroSMS, CountryCode: "2", ServiceCode: "tg",
				MaxPrice: 1, Status: "failed", ErrorCode: tt.storedCode,
			}
			_, callErr := purchaseService(t, repo).Purchase(
				context.Background(), purchaseInput("1", key),
				domain.User{ID: userID, Role: "operator"}, "127.0.0.1",
			)
			if !errors.Is(callErr, tt.wantKind) {
				t.Fatalf("错误=%v，期望 errors.Is(%v)", callErr, tt.wantKind)
			}
			requirePurchaseError(t, callErr, tt.wantCode, tt.wantMessage)
		})
	}
}

func TestPurchaseReplaysStoredUnknownReason(t *testing.T) {
	tests := []struct {
		name, storedCode, wantMessage string
	}{
		{"历史供应商响应异常", "provider_error", "购买结果尚未确认：供应商请求超时、连接中断或响应异常，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
		{"供应商请求超时", purchaseUnknownProviderTimeout, "购买结果尚未确认：供应商请求超时，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
		{"供应商连接中断", purchaseUnknownProviderConnection, "购买结果尚未确认：与供应商的连接中断或请求被取消，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
		{"供应商 HTTP 异常", purchaseUnknownProviderHTTP, "购买结果尚未确认：供应商返回服务异常或限流响应，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
		{"供应商响应读取失败", purchaseUnknownProviderRead, "购买结果尚未确认：读取供应商响应时中断，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
		{"供应商响应异常", purchaseUnknownProviderResponse, "购买结果尚未确认：供应商响应格式异常或内容过大，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
		{"处理中记录已陈旧", purchaseUnknownStaleProvisioning, "购买结果尚未确认：上一次请求处理已中断或状态更新失败，系统无法确认供应商是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
		{"超价取消未确认", "price_cancel_unknown", "购买结果尚未确认：供应商返回的价格超过所选价格，但取消结果未确认；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
		{"订单保存结果未确认", "database_error", "购买结果尚未确认：供应商已返回号码，但本地订单保存结果未确认，系统无法判断订单是否已记录或号码是否已取消；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
		{"旧未知原因", "legacy_unknown", "购买结果尚未确认：系统未能确定供应商是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const (
				userID = "operator-stored-unknown"
				key    = "idem-stored-unknown-12345"
			)
			repo := newPurchaseRepository(domain.Provider{ID: domain.ProviderHeroSMS, Enabled: true})
			repo.records[userID+"\x00"+key] = store.PurchaseRecord{
				ID: "stored-unknown", UserID: userID, IdempotencyKey: key,
				ProviderID: domain.ProviderHeroSMS, CountryCode: "2", ServiceCode: "tg",
				MaxPrice: 1, Status: "unknown", ErrorCode: tt.storedCode,
			}
			_, callErr := purchaseService(t, repo).Purchase(
				context.Background(), purchaseInput("1", key),
				domain.User{ID: userID, Role: "operator"}, "127.0.0.1",
			)
			if !errors.Is(callErr, ErrConflict) {
				t.Fatalf("错误=%v，期望结果待确认冲突", callErr)
			}
			requirePurchaseError(t, callErr, "purchase_result_unknown", tt.wantMessage)
		})
	}
}

func TestPurchasePriceCancelFailureRemainsUnknown(t *testing.T) {
	var purchaseCalls, cancelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/activations":
			purchaseCalls.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":[{"id":"uncancelled-upstream","phone":"77001112233","price":10.01}]}`))
		case request.Method == http.MethodDelete && request.URL.Path == "/api/v1/activations/uncancelled-upstream":
			cancelCalls.Add(1)
			http.Error(writer, "upstream unavailable", http.StatusBadGateway)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	vault, _ := secure.NewVault([]byte("purchase-cancel-failure-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey, Enabled: true,
	})
	service := New(repo, nil, vault, config.Config{})
	user := domain.User{ID: "operator-cancel-failure", Role: "operator"}
	const key = "idem-cancel-failure-12345"

	_, callErr := service.Purchase(context.Background(), purchaseInput("10", key), user, "127.0.0.1")
	if !errors.Is(callErr, ErrConflict) {
		t.Fatalf("取消失败错误=%v，期望结果待确认冲突", callErr)
	}
	requirePurchaseError(t, callErr, "purchase_result_unknown", "购买结果尚未确认：供应商返回的价格超过所选价格，但取消结果未确认；为避免重复扣费，仅暂停当前平台与购买条件的重复提交")
	record, code, orders, _, completes := repo.snapshot(user.ID, key)
	if record.Status != "unknown" || record.ErrorCode != "price_cancel_unknown" || code != "price_cancel_unknown" || orders != 0 || completes != 0 {
		t.Fatalf("取消失败意图状态异常: record=%+v code=%q orders=%d completes=%d", record, code, orders, completes)
	}

	_, retryErr := service.Purchase(context.Background(), purchaseInput("10", key), user, "127.0.0.1")
	requirePurchaseError(t, retryErr, "purchase_result_unknown", "购买结果尚未确认：供应商返回的价格超过所选价格，但取消结果未确认；为避免重复扣费，仅暂停当前平台与购买条件的重复提交")
	if purchaseCalls.Load() != 1 || cancelCalls.Load() != 1 {
		t.Fatalf("结果未知时不应重购: purchase=%d cancel=%d", purchaseCalls.Load(), cancelCalls.Load())
	}
}

func TestPurchaseDatabaseFailureRemainsUnknownRegardlessOfCancellationOutcome(t *testing.T) {
	tests := []struct {
		name         string
		cancelStatus int
	}{
		{name: "取消响应成功", cancelStatus: http.StatusNoContent},
		{name: "取消响应失败", cancelStatus: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var purchaseCalls, cancelCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodPost && request.URL.Path == "/api/v1/activations":
					purchaseCalls.Add(1)
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"data":[{"id":"database-failure-upstream","phone":"77001110000","price":0.2}]}`))
				case request.Method == http.MethodDelete && request.URL.Path == "/api/v1/activations/database-failure-upstream":
					cancelCalls.Add(1)
					if tt.cancelStatus == http.StatusNoContent {
						writer.WriteHeader(tt.cancelStatus)
						return
					}
					http.Error(writer, "upstream unavailable", tt.cancelStatus)
				default:
					http.NotFound(writer, request)
				}
			}))
			t.Cleanup(server.Close)

			vault, _ := secure.NewVault([]byte("purchase-database-failure-key"))
			apiKey, _ := vault.Encrypt("provider-secret")
			repo := newPurchaseRepository(domain.Provider{
				ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey, Enabled: true,
			})
			repo.completeErr = errors.New("database unavailable")
			service := New(repo, nil, vault, config.Config{})
			user := domain.User{ID: "operator-database-failure", Role: "operator"}
			const key = "idem-database-failure-12345"
			const message = "购买结果尚未确认：供应商已返回号码，但本地订单保存结果未确认，系统无法判断订单是否已记录或号码是否已取消；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"

			_, callErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
			requirePurchaseError(t, callErr, "database_error", message)
			record, code, orders, _, completes := repo.snapshot(user.ID, key)
			if record.Status != "unknown" || record.ErrorCode != "database_error" || code != "database_error" || orders != 0 || completes != 0 {
				t.Fatalf("保存失败意图状态异常: record=%+v code=%q orders=%d completes=%d", record, code, orders, completes)
			}

			_, retryErr := service.Purchase(context.Background(), purchaseInput("1", key), user, "127.0.0.1")
			requirePurchaseError(t, retryErr, "purchase_result_unknown", message)
			if purchaseCalls.Load() != 1 || cancelCalls.Load() != 1 {
				t.Fatalf("同编号重放不应重复调用供应商: purchase=%d cancel=%d", purchaseCalls.Load(), cancelCalls.Load())
			}
		})
	}
}

func TestPurchaseQualityTierValidationAndIdempotency(t *testing.T) {
	vault, err := secure.NewVault([]byte("purchase-tier-validation-key"))
	if err != nil {
		t.Fatal(err)
	}
	user := domain.User{ID: "operator-tier", Role: "operator"}

	t.Run("其他供应商拒绝等级", func(t *testing.T) {
		repo := newPurchaseRepository(domain.Provider{ID: domain.ProviderHeroSMS, Enabled: true})
		service := New(repo, nil, vault, config.Config{})
		input := purchaseInput("1", "idem-tier-provider-123456")
		input.QualityTier = "gold"
		_, callErr := service.Purchase(context.Background(), input, user, "127.0.0.1")
		if !errors.Is(callErr, ErrBadRequest) {
			t.Fatalf("HeroSMS 等级错误=%v，期望 ErrBadRequest", callErr)
		}
		if _, _, _, reserves, _ := repo.snapshot(user.ID, input.IdempotencyKey); reserves != 0 {
			t.Fatalf("非法供应商等级不应创建购买意图，实际=%d", reserves)
		}
	})

	t.Run("等级属于幂等请求身份", func(t *testing.T) {
		const key = "idem-tier-conflict-123456"
		repo := newPurchaseRepository(domain.Provider{ID: domain.ProviderSMSBower, Enabled: true})
		repo.records[user.ID+"\x00"+key] = store.PurchaseRecord{
			ID: "existing-tier-request", UserID: user.ID, IdempotencyKey: key,
			ProviderID: domain.ProviderSMSBower, CountryCode: "10", ServiceCode: "kt",
			QualityTier: "gold", MaxPrice: 1, Status: "provisioning",
		}
		service := New(repo, nil, vault, config.Config{})
		input := PurchaseInput{
			Provider: domain.ProviderSMSBower, CountryCode: "10", ServiceCode: "kt",
			QualityTier: "silver", MaxPrice: "1", IdempotencyKey: key,
		}
		_, callErr := service.Purchase(context.Background(), input, user, "127.0.0.1")
		if !errors.Is(callErr, ErrConflict) {
			t.Fatalf("同幂等键更换等级错误=%v，期望 ErrConflict", callErr)
		}
	})
}
