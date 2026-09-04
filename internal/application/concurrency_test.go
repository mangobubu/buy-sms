package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type lifecycleRepository struct {
	store.Repository

	mu                sync.Mutex
	locks             map[string]bool
	provider          domain.Provider
	order             domain.Order
	orders            []domain.Order
	audits            int
	completed         int
	claims            int
	restores          int
	completeCalls     int
	completeErr       error
	failCompleteUntil int
	auditDone         chan struct{}
}

func (r *lifecycleRepository) ClaimDueOrders(context.Context, int, time.Time, time.Duration) ([]domain.Order, error) {
	return []domain.Order{}, nil
}

func (r *lifecycleRepository) Maintenance(context.Context, time.Time) error { return nil }

func (r *lifecycleRepository) ClaimDueRenewals(context.Context, int, time.Time, time.Duration) ([]domain.Order, error) {
	return []domain.Order{}, nil
}
func (r *lifecycleRepository) WithOrderLock(ctx context.Context, id string, callback func(context.Context) error) error {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = make(map[string]bool)
	}
	if r.locks[id] {
		r.mu.Unlock()
		return store.ErrConflict
	}
	r.locks[id] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.locks, id)
		r.mu.Unlock()
	}()
	return callback(ctx)
}

func (r *lifecycleRepository) GetOrder(_ context.Context, id, userID string) (domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != id || (userID != "" && r.order.UserID != userID) {
		return domain.Order{}, store.ErrNotFound
	}
	return r.order, nil
}

func (r *lifecycleRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.provider.ID != id {
		return domain.Provider{}, store.ErrNotFound
	}
	return r.provider, nil
}

func (r *lifecycleRepository) SetOrderStatus(_ context.Context, id, status, providerState string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != id || r.order.Terminal() {
		return store.ErrConflict
	}
	r.order.Status = status
	r.order.LastProviderState = providerState
	return nil
}

func (r *lifecycleRepository) CompleteRequestNext(_ context.Context, id string, charge float64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completeCalls++
	if r.completeCalls <= r.failCompleteUntil {
		return false, errors.New("temporary accounting failure")
	}
	if r.completeErr != nil {
		return false, r.completeErr
	}
	if r.order.ID != id || r.order.Terminal() || !r.order.RequestNextInflight {
		return false, nil
	}
	r.order.RequestNextInflight = false
	r.order.RequestNextInflightAt = nil
	r.order.RequestNextPending = r.order.RequestNextGeneration > r.order.RequestNextClaimGeneration
	r.order.RequestNextFailures = 0
	r.order.Cost += charge
	r.completed++
	return true, nil
}

func (r *lifecycleRepository) ClaimRequestNext(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != id || r.order.Terminal() || !r.order.RequestNextPending || r.order.RequestNextInflight {
		return false, nil
	}
	now := time.Now().UTC()
	r.order.RequestNextPending = false
	r.order.RequestNextInflight = true
	r.order.RequestNextInflightAt = &now
	r.order.RequestNextClaimGeneration = r.order.RequestNextGeneration
	r.claims++
	return true, nil
}

func (r *lifecycleRepository) RestoreRequestNext(_ context.Context, id string, failures int, next time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != id || r.order.Terminal() || !r.order.RequestNextInflight {
		return nil
	}
	r.order.RequestNextPending = true
	r.order.RequestNextInflight = false
	r.order.RequestNextInflightAt = nil
	r.order.RequestNextFailures = failures
	if next.Before(r.order.NextPollAt) {
		r.order.NextPollAt = next
	}
	r.restores++
	return nil
}

func (r *lifecycleRepository) Audit(context.Context, *string, string, string, string, string, json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits++
	if r.auditDone != nil {
		select {
		case <-r.auditDone:
		default:
			close(r.auditDone)
		}
	}
	return nil
}

func (r *lifecycleRepository) SearchOrders(_ context.Context, userID, status, providerID, keyword string, limit, offset int) ([]domain.Order, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	filtered := make([]domain.Order, 0, len(r.orders))
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	for _, order := range r.orders {
		if userID != "" && order.UserID != userID {
			continue
		}
		if status != "" && order.Status != status {
			continue
		}
		if providerID != "" && order.ProviderID != providerID {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(order.PhoneNumber+" "+order.ID+" "+order.UpstreamID), keyword) {
			continue
		}
		filtered = append(filtered, order)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].CreatedAt.After(filtered[j].CreatedAt) })
	total := len(filtered)
	if offset >= total {
		return []domain.Order{}, total, nil
	}
	filtered = filtered[offset:]
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return append([]domain.Order(nil), filtered...), total, nil
}

func (r *lifecycleRepository) snapshot() (domain.Order, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order, r.completed, r.audits
}

func TestFinishOrderConcurrentActionsCallProviderOnlyOnce(t *testing.T) {
	var remoteCalls atomic.Int32
	remoteEntered := make(chan struct{})
	secondRemote := make(chan struct{})
	releaseRemote := make(chan struct{})
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/stubs/handler_api.php" || request.URL.Query().Get("action") != "setStatus" {
			http.NotFound(writer, request)
			return
		}
		if remoteCalls.Add(1) == 1 {
			close(remoteEntered)
		} else {
			close(secondRemote)
		}
		<-releaseRemote
		switch request.URL.Query().Get("status") {
		case "6":
			_, _ = writer.Write([]byte("ACCESS_ACTIVATION"))
		case "8":
			_, _ = writer.Write([]byte("ACCESS_CANCEL"))
		default:
			http.Error(writer, "BAD_STATUS", http.StatusBadRequest)
		}
	}))
	t.Cleanup(providerServer.Close)

	vault, err := secure.NewVault([]byte("finish-order-concurrency-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := &lifecycleRepository{
		provider: domain.Provider{ID: domain.ProviderSMSBower, BaseURL: providerServer.URL + "/stubs/handler_api.php", APIKeyCipher: apiKey},
		order:    domain.Order{ID: "order-race", UserID: "operator-1", ProviderID: domain.ProviderSMSBower, UpstreamID: "upstream-race", Status: domain.OrderActive},
	}
	service := New(repo, nil, vault, config.Config{})
	user := domain.User{ID: "operator-1", Role: "operator", Active: true}
	completeResult := make(chan error, 1)
	go func() {
		_, callErr := service.FinishOrder(context.Background(), "order-race", "complete", user, "127.0.0.1")
		completeResult <- callErr
	}()
	select {
	case <-remoteEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("首个远端结算调用未进入")
	}
	cancelResult := make(chan error, 1)
	go func() {
		_, callErr := service.FinishOrder(context.Background(), "order-race", "cancel", user, "127.0.0.1")
		cancelResult <- callErr
	}()
	select {
	case cancelErr := <-cancelResult:
		if !errors.Is(cancelErr, ErrConflict) {
			close(releaseRemote)
			t.Fatalf("并发取消应立即返回冲突，实际=%v", cancelErr)
		}
	case <-secondRemote:
		close(releaseRemote)
		<-cancelResult
		<-completeResult
		t.Fatal("并发 complete/cancel 均触达了供应商")
	case <-time.After(2 * time.Second):
		close(releaseRemote)
		t.Fatal("并发取消未及时返回")
	}
	close(releaseRemote)
	if completeErr := <-completeResult; completeErr != nil {
		t.Fatalf("首个完成动作失败: %v", completeErr)
	}
	if calls := remoteCalls.Load(); calls != 1 {
		t.Fatalf("并发结算远端动作次数=%d，期望=1", calls)
	}
	order, _, audits := repo.snapshot()
	if !order.Terminal() || audits != 1 {
		t.Fatalf("最终订单或审计异常: order=%+v audits=%d", order, audits)
	}
}

func TestRequestAnotherIgnoresStaleQueuedOrderAfterCancellation(t *testing.T) {
	var remoteCalls atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		remoteCalls.Add(1)
		_, _ = writer.Write([]byte("ACCESS_RETRY_GET"))
	}))
	t.Cleanup(providerServer.Close)

	vault, err := secure.NewVault([]byte("request-another-stale-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := &lifecycleRepository{
		provider: domain.Provider{ID: domain.ProviderSMSBower, BaseURL: providerServer.URL + "/stubs/handler_api.php", APIKeyCipher: apiKey},
		order:    domain.Order{ID: "order-canceled", ProviderID: domain.ProviderSMSBower, UpstreamID: "upstream-canceled", Status: domain.OrderCanceled, RequestNextPending: true},
	}
	service := New(repo, nil, vault, config.Config{})
	stale := repo.order
	stale.Status = domain.OrderActive

	service.requestAnother(context.Background(), stale)

	if calls := remoteCalls.Load(); calls != 0 {
		t.Fatalf("数据库已取消时不应请求供应商，实际调用=%d", calls)
	}
	order, completed, audits := repo.snapshot()
	if order.Status != domain.OrderCanceled || completed != 0 || audits != 0 {
		t.Fatalf("旧队列任务不应修改终态订单: order=%+v completed=%d audits=%d", order, completed, audits)
	}
}

func TestRequestAnotherChargeIsAppliedOnlyOnceForDuplicateTask(t *testing.T) {
	var checkCalls, resendCalls atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/sms/check_resend":
			checkCalls.Add(1)
			_, _ = writer.Write([]byte(`{"success":1,"resendCost":"0.42"}`))
		case "/sms/resend":
			resendCalls.Add(1)
			_, _ = writer.Write([]byte(`{"success":1,"message":"requested"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(providerServer.Close)

	vault, err := secure.NewVault([]byte("request-another-charge-once-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := &lifecycleRepository{
		provider: domain.Provider{ID: domain.ProviderSMSPool, BaseURL: providerServer.URL, APIKeyCipher: apiKey},
		order: domain.Order{
			ID: "order-charge-once", ProviderID: domain.ProviderSMSPool, UpstreamID: "upstream-charge-once",
			Status: domain.OrderActive, RequestNextPending: true, RequestNextGeneration: 1, Cost: 1.25,
		},
	}
	service := New(repo, nil, vault, config.Config{})
	stale := repo.order

	service.requestAnother(context.Background(), stale)
	service.requestAnother(context.Background(), stale)

	order, completed, audits := repo.snapshot()
	if checkCalls.Load() != 1 || resendCalls.Load() != 1 {
		t.Fatalf("重复任务不应再次访问供应商: check=%d resend=%d", checkCalls.Load(), resendCalls.Load())
	}
	if completed != 1 || audits != 1 || order.RequestNextPending || order.Cost != 1.67 {
		t.Fatalf("续码费用应仅累加一次: order=%+v completed=%d audits=%d", order, completed, audits)
	}
}

func TestRequestAnotherKeepsInflightWhenRemoteSucceededButAccountingFails(t *testing.T) {
	var checkCalls, resendCalls atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/sms/check_resend":
			checkCalls.Add(1)
			_, _ = writer.Write([]byte(`{"success":1,"resendCost":"0.30"}`))
		case "/sms/resend":
			resendCalls.Add(1)
			_, _ = writer.Write([]byte(`{"success":1,"charge":"0.30"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(providerServer.Close)
	vault, _ := secure.NewVault([]byte("request-next-inflight-accounting-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := &lifecycleRepository{
		provider: domain.Provider{ID: domain.ProviderSMSPool, BaseURL: providerServer.URL, APIKeyCipher: apiKey},
		order: domain.Order{
			ID: "request-next-inflight", ProviderID: domain.ProviderSMSPool, UpstreamID: "inflight-upstream",
			Status: domain.OrderActive, RequestNextPending: true, RequestNextGeneration: 1, Cost: 2,
		},
		completeErr: errors.New("database unavailable"),
	}
	service := New(repo, nil, vault, config.Config{})
	stale := repo.order

	service.requestAnother(context.Background(), stale)
	service.requestAnother(context.Background(), stale)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if checkCalls.Load() != 1 || resendCalls.Load() != 1 {
		t.Fatalf("inflight 重复旧任务不应再次调用上游: check=%d resend=%d", checkCalls.Load(), resendCalls.Load())
	}
	if repo.claims != 1 || repo.restores != 0 || repo.completeCalls != 3 || repo.completed != 0 || repo.audits != 0 {
		t.Fatalf("记账失败后的 claim/complete 状态异常: claims=%d restores=%d completeCalls=%d completed=%d audits=%d", repo.claims, repo.restores, repo.completeCalls, repo.completed, repo.audits)
	}
	if repo.order.RequestNextPending || !repo.order.RequestNextInflight || repo.order.Cost != 2 {
		t.Fatalf("远端成功、本地记账失败后必须保留 inflight: %+v", repo.order)
	}
	if repo.order.RequestNextGeneration != 1 || repo.order.RequestNextClaimGeneration != 1 {
		t.Fatalf("inflight 必须保留已领取 generation: %+v", repo.order)
	}
}

func TestRequestAnotherRemoteFailureRestoresPending(t *testing.T) {
	var remoteCalls atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		remoteCalls.Add(1)
		http.Error(writer, "NO_BALANCE", http.StatusServiceUnavailable)
	}))
	t.Cleanup(providerServer.Close)
	vault, _ := secure.NewVault([]byte("request-next-restore-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	repo := &lifecycleRepository{
		provider: domain.Provider{ID: domain.ProviderSMSBower, BaseURL: providerServer.URL + "/stubs/handler_api.php", APIKeyCipher: apiKey},
		order: domain.Order{
			ID: "request-next-restore", ProviderID: domain.ProviderSMSBower, UpstreamID: "restore-upstream",
			Status: domain.OrderActive, RequestNextPending: true, RequestNextGeneration: 1, RequestNextFailures: 1, NextPollAt: now.Add(time.Hour),
		},
	}
	service := New(repo, nil, vault, config.Config{})
	service.now = func() time.Time { return now }

	service.requestAnother(context.Background(), repo.order)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if remoteCalls.Load() != 1 || repo.claims != 1 || repo.restores != 1 || repo.completeCalls != 0 {
		t.Fatalf("远端失败 claim/restore 次数异常: remote=%d claims=%d restores=%d complete=%d", remoteCalls.Load(), repo.claims, repo.restores, repo.completeCalls)
	}
	if !repo.order.RequestNextPending || repo.order.RequestNextInflight || repo.order.RequestNextFailures != 2 {
		t.Fatalf("远端失败应恢复 pending: %+v", repo.order)
	}
	if repo.order.RequestNextGeneration != 1 || repo.order.RequestNextClaimGeneration != 1 {
		t.Fatalf("恢复 pending 不应改变 generation: %+v", repo.order)
	}
	if !repo.order.NextPollAt.Equal(now.Add(5*time.Second)) || repo.order.Cost != 0 || repo.audits != 0 {
		t.Fatalf("恢复后的重试时间/费用/审计异常: order=%+v audits=%d", repo.order, repo.audits)
	}
}

func TestRunRecoversRequestNextAccountingWithoutRepeatingUpstream(t *testing.T) {
	var checkCalls, resendCalls atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/sms/check_resend":
			checkCalls.Add(1)
			_, _ = writer.Write([]byte(`{"success":1,"resendCost":"0.40"}`))
		case "/sms/resend":
			resendCalls.Add(1)
			_, _ = writer.Write([]byte(`{"success":1,"charge":"0.40"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(providerServer.Close)
	vault, _ := secure.NewVault([]byte("request-next-accounting-recovery-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := &lifecycleRepository{
		provider: domain.Provider{ID: domain.ProviderSMSPool, BaseURL: providerServer.URL, APIKeyCipher: apiKey},
		order: domain.Order{
			ID: "request-next-recovery", ProviderID: domain.ProviderSMSPool, UpstreamID: "recovery-upstream",
			Status: domain.OrderActive, RequestNextPending: true, RequestNextGeneration: 1, Cost: 3,
		},
		failCompleteUntil: 3,
		auditDone:         make(chan struct{}),
	}
	service := New(repo, nil, vault, config.Config{})
	stale := repo.order

	// 首次路径完成远端续码，但本地记账前三次均失败并进入专用恢复队列。
	service.requestAnother(context.Background(), stale)
	repo.mu.Lock()
	if repo.completeCalls != 3 || !repo.order.RequestNextInflight || repo.order.Cost != 3 {
		repo.mu.Unlock()
		t.Fatalf("进入恢复队列前状态异常: complete=%d order=%+v", repo.completeCalls, repo.order)
	}
	repo.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Run(ctx)
	}()
	select {
	case <-repo.auditDone:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("等待续码记账恢复完成超时")
	}
	// 即使旧任务再次投递，数据库已清 inflight/pending，不得重放上游副作用。
	service.requestAnother(context.Background(), stale)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Service.Run 取消后未退出")
	}

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if checkCalls.Load() != 1 || resendCalls.Load() != 1 {
		t.Fatalf("记账恢复不得再次调用上游: check=%d resend=%d", checkCalls.Load(), resendCalls.Load())
	}
	if repo.completeCalls != 4 || repo.completed != 1 || repo.audits != 1 || repo.claims != 1 || repo.restores != 0 {
		t.Fatalf("记账恢复调用次数异常: complete=%d completed=%d audits=%d claims=%d restores=%d", repo.completeCalls, repo.completed, repo.audits, repo.claims, repo.restores)
	}
	if repo.order.RequestNextPending || repo.order.RequestNextInflight || repo.order.RequestNextInflightAt != nil || math.Abs(repo.order.Cost-3.4) > 1e-9 {
		t.Fatalf("记账恢复后订单状态异常: %+v", repo.order)
	}
	if repo.order.RequestNextGeneration != 1 || repo.order.RequestNextClaimGeneration != 1 || repo.order.RequestNextFailures != 0 {
		t.Fatalf("记账恢复后的 generation/失败计数异常: %+v", repo.order)
	}
}

func TestOrdersCanReachBeyondFirstTwoHundred(t *testing.T) {
	const totalOrders = 235
	repo := &lifecycleRepository{provider: domain.Provider{ID: domain.ProviderHeroSMS}}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for index := 0; index < totalOrders; index++ {
		repo.orders = append(repo.orders, domain.Order{
			ID: fmt.Sprintf("order-%03d", index), UserID: "operator-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: strconv.Itoa(index), PhoneNumber: fmt.Sprintf("+1555%07d", index), Status: domain.OrderActive,
			CreatedAt: base.Add(time.Duration(index) * time.Minute), UpdatedAt: base.Add(time.Duration(index) * time.Minute),
		})
	}
	service := New(repo, nil, nil, config.Config{})
	page, err := service.Orders(context.Background(), OrderQuery{Page: 12, PageSize: 20}, domain.User{ID: "operator-1", Role: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != totalOrders || len(page.Items) != 15 {
		t.Fatalf("第 12 页不可达或总数被截断: total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].ID != "order-014" || page.Items[len(page.Items)-1].ID != "order-000" {
		t.Fatalf("超过 200 条后的订单范围错误: first=%s last=%s", page.Items[0].ID, page.Items[len(page.Items)-1].ID)
	}
}
