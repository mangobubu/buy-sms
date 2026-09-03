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

type terminalLockRepository struct {
	store.Repository

	mu          sync.Mutex
	locks       map[string]bool
	provider    domain.Provider
	order       domain.Order
	transitions []string
	audits      int
	lockCalls   int
}

func (r *terminalLockRepository) WithOrderLock(ctx context.Context, id string, callback func(context.Context) error) error {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = make(map[string]bool)
	}
	r.lockCalls++
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

func (r *terminalLockRepository) GetOrder(_ context.Context, id, userID string) (domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != id || (userID != "" && r.order.UserID != userID) {
		return domain.Order{}, store.ErrNotFound
	}
	return r.order, nil
}

func (r *terminalLockRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.provider.ID != id {
		return domain.Provider{}, store.ErrNotFound
	}
	return r.provider, nil
}

func (r *terminalLockRepository) SetOrderStatus(_ context.Context, id, status, providerState string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != id || r.order.Terminal() {
		return store.ErrConflict
	}
	r.order.Status = status
	r.order.LastProviderState = providerState
	r.transitions = append(r.transitions, status)
	return nil
}

func (r *terminalLockRepository) Audit(context.Context, *string, string, string, string, string, json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits++
	return nil
}

func (r *terminalLockRepository) snapshot() (domain.Order, []string, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.order, append([]string(nil), r.transitions...), r.audits, r.lockCalls
}

func TestFinishOrderRejectsCancelAfterReceivingMessage(t *testing.T) {
	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	vault, _ := secure.NewVault([]byte("cancel-after-message-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := &terminalLockRepository{
		provider: domain.Provider{ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey},
		order: domain.Order{
			ID: "received-message", UserID: "operator-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "received-message-upstream", Status: domain.OrderActive,
			Messages: []domain.SMSMessage{{ID: "message-1", Code: "123456"}},
		},
	}
	service := New(repo, nil, vault, config.Config{})

	_, err := service.FinishOrder(context.Background(), repo.order.ID, "cancel", domain.User{ID: "operator-1", Role: "operator"}, "127.0.0.1")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("已有验证码时取消应返回 ErrConflict，实际为: %v", err)
	}

	order, transitions, audits, lockCalls := repo.snapshot()
	if providerRequests.Load() != 0 {
		t.Fatalf("已有验证码时不应请求供应商，实际请求次数: %d", providerRequests.Load())
	}
	if order.Status != domain.OrderActive {
		t.Fatalf("取消被拒绝后订单应保持 active，实际状态: %s", order.Status)
	}
	if len(transitions) != 0 || audits != 0 {
		t.Fatalf("取消被拒绝后不应产生状态变更或审计: transitions=%v audits=%d", transitions, audits)
	}
	if lockCalls != 1 {
		t.Fatalf("取消检查应在订单锁内执行一次，实际锁调用次数: %d", lockCalls)
	}
}

func TestPollLocalExpiryAndFinishOrderShareTerminalLock(t *testing.T) {
	now := time.Now().UTC()
	expiresAt := now.Add(-time.Minute)
	finishEntered := make(chan struct{})
	releaseFinish := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFinish) }) }
	t.Cleanup(release)
	var finishCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/activations/expiry-upstream/finish" {
			http.NotFound(writer, request)
			return
		}
		finishCalls.Add(1)
		close(finishEntered)
		<-releaseFinish
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	vault, _ := secure.NewVault([]byte("local-expiry-finish-lock-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := &terminalLockRepository{
		provider: domain.Provider{ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey},
		order: domain.Order{
			ID: "local-expiry-race", UserID: "operator-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "expiry-upstream", Status: domain.OrderActive, ExpiresAt: &expiresAt,
		},
	}
	service := New(repo, nil, vault, config.Config{})
	service.now = func() time.Time { return now }
	finishResult := make(chan error, 1)
	go func() {
		_, err := service.FinishOrder(context.Background(), repo.order.ID, "complete", domain.User{ID: "operator-1", Role: "operator"}, "127.0.0.1")
		finishResult <- err
	}()
	select {
	case <-finishEntered:
	case <-time.After(time.Second):
		t.Fatal("人工完成未进入远端动作")
	}

	// FinishOrder 正持有订单锁。本地过期轮询必须立即放弃，不能绕锁写 expired。
	service.pollOne(context.Background(), repo.order)
	beforeRelease, beforeTransitions, _, _ := repo.snapshot()
	if beforeRelease.Status != domain.OrderActive || len(beforeTransitions) != 0 {
		release()
		t.Fatalf("人工完成持锁期间本地过期修改了状态: order=%+v transitions=%v", beforeRelease, beforeTransitions)
	}
	release()
	if finishErr := <-finishResult; finishErr != nil {
		t.Fatalf("人工完成失败: %v", finishErr)
	}

	order, transitions, audits, lockCalls := repo.snapshot()
	if order.Status != domain.OrderCompleted || len(transitions) != 1 || transitions[0] != domain.OrderCompleted {
		t.Fatalf("人工完成与本地过期只能提交 completed: order=%+v transitions=%v", order, transitions)
	}
	if finishCalls.Load() != 1 || audits != 1 || lockCalls != 2 {
		t.Fatalf("终态动作次数异常: finish=%d audits=%d locks=%d", finishCalls.Load(), audits, lockCalls)
	}
}

func TestProviderTerminalAndConcurrentFinishChooseSingleTerminalState(t *testing.T) {
	providerEntered := make(chan struct{})
	releaseProvider := make(chan struct{})
	var pollCalls, finishCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Get("action") == "getAllSms":
			pollCalls.Add(1)
			close(providerEntered)
			<-releaseProvider
			_, _ = writer.Write([]byte("STATUS_CANCEL"))
		case request.Method == http.MethodGet && request.URL.Query().Get("action") == "setStatus":
			finishCalls.Add(1)
			_, _ = writer.Write([]byte("ACCESS_ACTIVATION"))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	vault, _ := secure.NewVault([]byte("terminal-lock-provider-key"))
	apiKey, _ := vault.Encrypt("provider-secret")
	repo := &terminalLockRepository{
		provider: domain.Provider{ID: domain.ProviderSMSBower, BaseURL: server.URL + "/stubs/handler_api.php", APIKeyCipher: apiKey},
		order:    domain.Order{ID: "provider-terminal-race", UserID: "operator-1", ProviderID: domain.ProviderSMSBower, UpstreamID: "terminal-upstream", Status: domain.OrderActive},
	}
	service := New(repo, nil, vault, config.Config{})
	pollDone := make(chan struct{})
	go func() {
		service.pollOne(context.Background(), repo.order)
		close(pollDone)
	}()
	select {
	case <-providerEntered:
	case <-time.After(time.Second):
		t.Fatal("供应商终态轮询未开始")
	}

	_, finishErr := service.FinishOrder(context.Background(), repo.order.ID, "complete", domain.User{ID: "operator-1", Role: "operator"}, "127.0.0.1")
	if finishErr != nil {
		close(releaseProvider)
		t.Fatalf("人工完成应在轮询进入终态锁前成功: %v", finishErr)
	}
	close(releaseProvider)
	select {
	case <-pollDone:
	case <-time.After(time.Second):
		t.Fatal("供应商终态轮询未退出")
	}

	order, transitions, audits, lockCalls := repo.snapshot()
	if order.Status != domain.OrderCompleted || len(transitions) != 1 || transitions[0] != domain.OrderCompleted {
		t.Fatalf("人工完成胜出后供应商 canceled 不得覆盖: order=%+v transitions=%v", order, transitions)
	}
	if finishCalls.Load() != 1 || audits != 1 {
		t.Fatalf("人工完成胜出时远端/审计次数异常: finish=%d audits=%d", finishCalls.Load(), audits)
	}
	if pollCalls.Load() != 1 || lockCalls != 2 {
		t.Fatalf("轮询或锁调用不足: poll=%d locks=%d", pollCalls.Load(), lockCalls)
	}
}
