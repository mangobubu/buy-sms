package application

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/provider"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type renewalRepository struct {
	store.Repository
	mu       sync.Mutex
	provider domain.Provider
	order    domain.Order
	records  map[string]store.RenewalRecord
	claimed  bool
	releases int
	audits   []string
}

func (r *renewalRepository) GetOrder(_ context.Context, id, userID string) (domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != id || (userID != "" && r.order.UserID != userID) {
		return domain.Order{}, store.ErrNotFound
	}
	return r.order, nil
}

func (r *renewalRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	if r.provider.ID != id {
		return domain.Provider{}, store.ErrNotFound
	}
	return r.provider, nil
}

func (r *renewalRepository) WithOrderLock(ctx context.Context, _ string, callback func(context.Context) error) error {
	return callback(ctx)
}

func (r *renewalRepository) GetRenewalRequest(_ context.Context, userID, key string) (store.RenewalRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[userID+"\x00"+key]
	if !ok {
		return store.RenewalRecord{}, store.ErrNotFound
	}
	return record, nil
}

func (r *renewalRepository) StartOrderRenewal(_ context.Context, record store.RenewalRecord) (store.RenewalRecord, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lookup := record.UserID + "\x00" + record.IdempotencyKey
	if existing, ok := r.records[lookup]; ok {
		return existing, false, nil
	}
	if r.order.RenewalInflight {
		return store.RenewalRecord{}, false, store.ErrConflict
	}
	now := time.Now().UTC()
	record.Status = "provisioning"
	record.CreatedAt, record.UpdatedAt = now, now
	r.records[lookup] = record
	r.order.RenewalRequestID = record.ID
	r.claimed = true
	r.order.RenewalInflight = true
	r.order.RenewalInflightAt = &now
	r.order.RenewalMode = record.Mode
	r.order.RenewalValue = record.Value
	r.order.RenewalUnit = record.Unit
	r.order.RenewalQuotedPrice = record.QuotedPrice
	r.order.RenewalBaseline = record.Baseline
	return record, true, nil
}

func (r *renewalRepository) MarkOrderRenewalSubmitted(_ context.Context, requestID, orderID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != orderID || r.order.RenewalRequestID != requestID || r.order.RenewalSubmittedAt != nil {
		return false, nil
	}
	for lookup, record := range r.records {
		if record.ID != requestID || record.Status != "provisioning" {
			continue
		}
		now := time.Now().UTC()
		record.Status, record.SubmittedAt, record.UpdatedAt = "unknown", &now, now
		r.records[lookup] = record
		r.order.RenewalSubmittedAt = &now
		return true, nil
	}
	return false, nil
}

func (r *renewalRepository) CompleteOrderRenewal(_ context.Context, requestID, id, upstreamID, phoneNumber, duration string, expiresAt time.Time, totalCost, chargedPrice float64, activationStartedAt time.Time, nonRefundable bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != id || r.order.RenewalRequestID != requestID || r.order.RenewalSubmittedAt == nil {
		return store.ErrConflict
	}
	var recordLookup string
	var record store.RenewalRecord
	for lookup, candidate := range r.records {
		if candidate.ID == requestID && candidate.Status == "unknown" {
			recordLookup, record = lookup, candidate
			break
		}
	}
	if recordLookup == "" {
		return store.ErrConflict
	}
	r.order.UpstreamID = upstreamID
	if phoneNumber != "" {
		r.order.PhoneNumber = phoneNumber
	}
	r.order.Duration = duration
	r.order.Status = domain.OrderActive
	r.order.Cost = totalCost
	r.order.ExpiresAt = &expiresAt
	r.order.ActivationStartedAt = activationStartedAt
	r.order.NonRefundable = nonRefundable
	r.clearRenewalLocked()
	r.claimed = false
	record.Status, record.ChargedPrice, record.ResultExpiresAt = "succeeded", chargedPrice, &expiresAt
	r.records[recordLookup] = record
	return nil
}

func (r *renewalRepository) ReleaseOrderRenewal(_ context.Context, requestID, orderID, errorCode string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != orderID || r.order.RenewalRequestID != requestID {
		return store.ErrConflict
	}
	for lookup, record := range r.records {
		if record.ID == requestID && (record.Status == "provisioning" || record.Status == "unknown") {
			record.Status, record.ErrorCode = "failed", errorCode
			r.records[lookup] = record
		}
	}
	r.clearRenewalLocked()
	r.releases++
	return nil
}

func (r *renewalRepository) clearRenewalLocked() {
	r.claimed = false
	r.order.RenewalRequestID = ""
	r.order.RenewalInflight = false
	r.order.RenewalInflightAt = nil
	r.order.RenewalMode = ""
	r.order.RenewalValue = 0
	r.order.RenewalUnit = ""
	r.order.RenewalQuotedPrice = 0
	r.order.RenewalBaseline = ""
	r.order.RenewalSubmittedAt = nil
}

func (r *renewalRepository) ClaimDueRenewals(context.Context, int, time.Time, time.Duration) ([]domain.Order, error) {
	return nil, nil
}
func (r *renewalRepository) Audit(_ context.Context, _ *string, action, _, _, _ string, _ json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, action)
	return nil
}

func newRenewalService(t *testing.T, repo *renewalRepository) *Service {
	t.Helper()
	vault, err := secure.NewVault([]byte("renewal-test-encryption-key"))
	if err != nil {
		t.Fatal(err)
	}
	repo.provider.APIKeyCipher, err = vault.Encrypt("test-key")
	if err != nil {
		t.Fatal(err)
	}
	repo.provider.APIKeyConfigured = true
	if repo.records == nil {
		repo.records = make(map[string]store.RenewalRecord)
	}
	repo.provider.Enabled = true
	return New(repo, nil, vault, config.Config{})
}

func TestRenewOrderRequiresIdempotencyKey(t *testing.T) {
	_, err := (&Service{}).RenewOrder(context.Background(), "order-1", RenewalInput{
		Value: 24, Unit: "hour", QuotedPrice: "0.25",
	}, domain.User{ID: "user-1"}, "127.0.0.1")
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("缺少续期幂等键错误=%v，期望 ErrBadRequest", err)
	}
}
func TestRenewOrderUsesHeroAPIQuoteAndChargedPrice(t *testing.T) {
	expiresAt := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	var optionCalls, renewCalls int
	prolongDone := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/activations/hero-1/prolong/options":
			optionCalls++
			_, _ = writer.Write([]byte(`{"data":{"options":[{"duration":{"value":24,"unit":"hour"},"price":0.25}]}}`))
		case "/api/v1/activations/hero-1/prolong":
			renewCalls++
			prolongDone = true
			_, _ = writer.Write([]byte(`{"data":[{"id":"hero-1","phone":"79990001122","price":0.27,"expiredAt":"2026-09-07T08:00:00Z"}]}`))
		case "/api/v1/activations/hero-1/prolong/history":
			if !prolongDone {
				_, _ = writer.Write([]byte(`{"data":[{"duration":24,"price":0.10,"createdAt":"2026-09-03T12:00:01Z"}]}`))
				break
			}
			_, _ = writer.Write([]byte(`{"data":[{"duration":24,"price":0.10,"createdAt":"2026-09-03T12:00:01Z"},{"duration":24,"price":0.25,"createdAt":"2026-09-04T12:00:01Z"}]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	repo := &renewalRepository{
		provider: domain.Provider{ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1"},
		order: domain.Order{ID: "order-1", UserID: "user-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "hero-1", PhoneNumber: "79990001122", Duration: "24", Status: domain.OrderActive,
			Cost: 1, Currency: "USD"},
	}
	service := newRenewalService(t, repo)
	user := domain.User{ID: "user-1", Role: "operator"}

	quote, err := service.RenewalOptions(context.Background(), "order-1", user)
	if err != nil || quote.Mode != "prolong" || len(quote.Options) != 1 || quote.Options[0].Price != "0.25" {
		t.Fatalf("续期报价=%#v err=%v", quote, err)
	}
	view, err := service.RenewOrder(context.Background(), "order-1", RenewalInput{
		Value: 24, Unit: "hour", QuotedPrice: "0.25", IdempotencyKey: "renewal-test-key-0001",
	}, user, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if optionCalls != 2 || renewCalls != 1 || view.Price != "1.25" || repo.order.Cost != 1.25 ||
		repo.order.ExpiresAt == nil || !repo.order.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("应重新校验报价并按动作 API 实价入账: options=%d renew=%d view=%#v order=%#v", optionCalls, renewCalls, view, repo.order)
	}
	replayed, err := service.RenewOrder(context.Background(), "order-1", RenewalInput{
		Value: 24, Unit: "hour", QuotedPrice: "0.25", IdempotencyKey: "renewal-test-key-0001",
	}, user, "127.0.0.1")
	if err != nil || replayed.Price != "1.25" || renewCalls != 1 || optionCalls != 2 {
		t.Fatalf("同一幂等键应直接重放成功订单且不再次请求供应商: view=%#v options=%d renew=%d err=%v", replayed, optionCalls, renewCalls, err)
	}
	_, err = service.RenewOrder(context.Background(), "order-1", RenewalInput{
		Value: 48, Unit: "hour", QuotedPrice: "0.25", IdempotencyKey: "renewal-test-key-0001",
	}, user, "127.0.0.1")
	var mismatch *OrderActionError
	if !errors.As(err, &mismatch) || mismatch.Code != OrderActionCodeRenewalIdempotencyMismatch || renewCalls != 1 {
		t.Fatalf("同键不同参数必须被拒绝且不再次扣费: err=%v renew=%d", err, renewCalls)
	}
}

func TestRenewOrderRejectsChangedQuoteBeforeCharge(t *testing.T) {
	var renewCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/activations/hero-1/prolong/options":
			_, _ = writer.Write([]byte(`{"data":{"options":[{"duration":{"value":24,"unit":"hour"},"price":0.30}]}}`))
		case "/api/v1/activations/hero-1/prolong/history":
			_, _ = writer.Write([]byte(`{"data":[]}`))
		default:
			renewCalls++
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	repo := &renewalRepository{
		provider: domain.Provider{ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1"},
		order: domain.Order{ID: "order-1", UserID: "user-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "hero-1", PhoneNumber: "79990001122", Duration: "24", Status: domain.OrderActive, Currency: "USD"},
	}
	_, err := newRenewalService(t, repo).RenewOrder(context.Background(), "order-1", RenewalInput{
		Value: 24, Unit: "hour", QuotedPrice: "0.25", IdempotencyKey: "renewal-test-key-0001",
	}, domain.User{ID: "user-1", Role: "operator"}, "127.0.0.1")
	var actionErr *OrderActionError
	if !errors.As(err, &actionErr) || actionErr.Code != OrderActionCodeRenewalPriceChanged || renewCalls != 0 || repo.claimed {
		t.Fatalf("价格变化应在扣费前终止: err=%v renew=%d claimed=%v", err, renewCalls, repo.claimed)
	}
}

func TestRenewOrderUnknownResultKeepsKeyAndBlocksNewKey(t *testing.T) {
	var renewCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/activations/hero-1/prolong/options":
			_, _ = writer.Write([]byte(`{"data":{"options":[{"duration":{"value":24,"unit":"hour"},"price":0.25}]}}`))
		case "/api/v1/activations/hero-1/prolong/history":
			_, _ = writer.Write([]byte(`{"data":[]}`))
		case "/api/v1/activations/hero-1/prolong":
			renewCalls++
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"title":"SERVER_ERROR"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	repo := &renewalRepository{
		provider: domain.Provider{ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1"},
		order: domain.Order{ID: "order-1", UserID: "user-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "hero-1", PhoneNumber: "79990001122", Duration: "24", Status: domain.OrderActive,
			Cost: 1, Currency: "USD"},
	}
	service := newRenewalService(t, repo)
	user := domain.User{ID: "user-1", Role: "operator"}
	input := RenewalInput{Value: 24, Unit: "hour", QuotedPrice: "0.25", IdempotencyKey: "renewal-unknown-key-0001"}
	_, err := service.RenewOrder(context.Background(), "order-1", input, user, "127.0.0.1")
	var actionErr *OrderActionError
	if !errors.As(err, &actionErr) || actionErr.Code != OrderActionCodeRenewalResultUnknown || renewCalls != 1 {
		t.Fatalf("供应商 5xx 应保留未知结果且只提交一次: err=%v calls=%d", err, renewCalls)
	}
	_, err = service.RenewOrder(context.Background(), "order-1", input, user, "127.0.0.1")
	if !errors.As(err, &actionErr) || actionErr.Code != OrderActionCodeRenewalResultUnknown || renewCalls != 1 {
		t.Fatalf("同键重试未知结果不应再次提交: err=%v calls=%d", err, renewCalls)
	}
	input.IdempotencyKey = "renewal-other-key-0002"
	_, err = service.RenewOrder(context.Background(), "order-1", input, user, "127.0.0.1")
	if !errors.As(err, &actionErr) || actionErr.Code != OrderActionCodeRenewalInProgress || renewCalls != 1 {
		t.Fatalf("新键必须被同订单未知流水阻止: err=%v calls=%d", err, renewCalls)
	}
	record, getErr := repo.GetRenewalRequest(context.Background(), user.ID, "renewal-unknown-key-0001")
	if getErr != nil || record.Status != "unknown" || record.SubmittedAt == nil || !repo.order.RenewalInflight {
		t.Fatalf("未知流水/订单认领未持久保留: record=%#v order=%#v err=%v", record, repo.order, getErr)
	}
}
func TestRenewOrderUsesSMSPoolActiveAPIPriceInsteadOfLocalCalculation(t *testing.T) {
	expiresAt := time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/request/history":
			_, _ = writer.Write([]byte(`{"data":[{"order_code":"POOL-1","short_name":"US","pool":7,"status":"refunded","code":"0","cost":"0.42","expiry":1788858000}]}`))
		case "/sms/reactivate":
			_, _ = writer.Write([]byte(`{"success":1,"message":"reactivated"}`))
		case "/request/active":
			_, _ = fmt.Fprintf(writer, `[{"order_code":"POOL-1","short_name":"US","pool":7,"status":"pending","cost":"0.57","expiry":%d}]`, expiresAt.Unix())
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	repo := &renewalRepository{
		provider: domain.Provider{ID: domain.ProviderSMSPool, BaseURL: server.URL},
		order: domain.Order{ID: "order-1", UserID: "user-1", ProviderID: domain.ProviderSMSPool,
			UpstreamID: "POOL-1", PhoneNumber: "14155550123", Status: domain.OrderExpired,
			Cost: 0.42, Currency: "USD"},
	}
	service := newRenewalService(t, repo)
	user := domain.User{ID: "user-1", Role: "operator"}
	quote, err := service.RenewalOptions(context.Background(), "order-1", user)
	if err != nil || len(quote.Options) != 1 || quote.Options[0].Price != "0.42" {
		t.Fatalf("SMSPool 历史 API 报价=%#v err=%v", quote, err)
	}
	view, err := service.RenewOrder(context.Background(), "order-1", RenewalInput{
		Value: 1, Unit: "activation", QuotedPrice: "0.42", IdempotencyKey: "renewal-test-key-0001",
	}, user, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Price != "0.57" || repo.order.Cost != 0.57 || repo.order.ExpiresAt == nil || !repo.order.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("SMSPool 应以 Active API 返回价格替换已退款旧金额: view=%#v order=%#v", view, repo.order)
	}
}
func testHeroProlongBaseline(submittedAt time.Time) string {
	entry := fmt.Sprintf("%d\x00%s\x00%s", 24, "0.1", submittedAt.Add(-time.Minute).UTC().Format(time.RFC3339Nano))
	digest := sha256.Sum256([]byte(entry))
	return fmt.Sprintf(`{"%x":1}`, digest[:])
}
func TestReconcileRenewalCompletesUnknownHeroProlongWithoutRepeatingPost(t *testing.T) {
	submittedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	var postCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/activations/hero-1/prolong/history":
			_, _ = writer.Write([]byte(`{"data":[{"duration":24,"price":0.25,"createdAt":"2026-09-04T12:00:01Z"}]}`))
		case "/api/v1/activations":
			_, _ = writer.Write([]byte(`{"data":[{"id":"hero-1","phone":"79990001122","country":2,"service":"tg","price":1.25,"createdAt":"2026-09-03T12:00:00Z","expiredAt":"2026-09-06T12:00:00Z"}]}`))
		case "/api/v1/activations/hero-1/prolong":
			postCalls++
			http.Error(writer, "unexpected", http.StatusInternalServerError)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	baseline := testHeroProlongBaseline(submittedAt)
	record := store.RenewalRecord{
		ID: "renewal-1", UserID: "user-1", OrderID: "order-1", IdempotencyKey: "renewal-reconcile-key-1",
		ProviderID: domain.ProviderHeroSMS, UpstreamID: "hero-1", Mode: provider.RenewalProlong,
		Value: 24, Unit: "hour", QuotedPrice: 0.25, Baseline: baseline, Status: "unknown", SubmittedAt: &submittedAt,
	}
	repo := &renewalRepository{
		provider: domain.Provider{ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1"},
		order: domain.Order{ID: "order-1", UserID: "user-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "hero-1", PhoneNumber: "79990001122", CountryCode: "2", ServiceCode: "tg",
			Duration: "24", Status: domain.OrderActive, Cost: 1, Currency: "USD",
			RenewalRequestID: "renewal-1", RenewalInflight: true, RenewalInflightAt: &submittedAt,
			RenewalMode: provider.RenewalProlong, RenewalValue: 24, RenewalUnit: "hour",
			RenewalQuotedPrice: 0.25, RenewalBaseline: baseline, RenewalSubmittedAt: &submittedAt},
		records: map[string]store.RenewalRecord{"user-1\x00renewal-reconcile-key-1": record},
	}
	service := newRenewalService(t, repo)
	service.reconcileRenewal(context.Background(), repo.order)
	if postCalls != 0 || repo.order.RenewalInflight || repo.order.Cost != 1.25 ||
		repo.order.ExpiresAt == nil || !repo.order.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("对账只能读取供应商并原子恢复订单: post=%d order=%#v", postCalls, repo.order)
	}
	stored, err := repo.GetRenewalRequest(context.Background(), "user-1", "renewal-reconcile-key-1")
	if err != nil || stored.Status != "succeeded" || stored.ChargedPrice != 0.25 ||
		stored.ResultExpiresAt == nil || !stored.ResultExpiresAt.Equal(expiresAt) {
		t.Fatalf("对账未完成幂等流水: record=%#v err=%v", stored, err)
	}
}
func TestFinishOrderBlockedWhileRenewalPending(t *testing.T) {
	submittedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repo := &renewalRepository{
		provider: domain.Provider{ID: domain.ProviderHeroSMS, BaseURL: "https://hero-sms.com/api/v1"},
		order: domain.Order{ID: "order-1", UserID: "user-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "hero-1", Status: domain.OrderActive, RenewalRequestID: "renewal-1",
			RenewalInflight: true, RenewalSubmittedAt: &submittedAt},
	}
	service := newRenewalService(t, repo)
	user := domain.User{ID: "user-1", Role: "operator"}
	for _, action := range []string{"complete", "cancel"} {
		t.Run(action, func(t *testing.T) {
			_, err := service.FinishOrder(context.Background(), "order-1", action, user, "127.0.0.1")
			var actionErr *OrderActionError
			if !errors.As(err, &actionErr) || actionErr.Code != OrderActionCodeRenewalInProgress {
				t.Fatalf("续期待确认时 %s 错误=%v", action, err)
			}
			if repo.order.Status != domain.OrderActive {
				t.Fatalf("续期待确认时 %s 改变了订单状态: %s", action, repo.order.Status)
			}
		})
	}
}
func TestPollSnapshotCurrentRejectsRenewalChanges(t *testing.T) {
	updatedAt := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	snapshot := domain.Order{ID: "order-1", UpstreamID: "upstream-1", Status: domain.OrderActive, UpdatedAt: updatedAt}
	if !pollSnapshotCurrent(snapshot, snapshot) {
		t.Fatal("未变化的活动订单应接受轮询结果")
	}
	claimed := snapshot
	claimed.RenewalInflight = true
	if pollSnapshotCurrent(snapshot, claimed) {
		t.Fatal("续期认领后应丢弃旧轮询结果")
	}
	reactivated := snapshot
	reactivated.UpstreamID = "upstream-2"
	reactivated.UpdatedAt = updatedAt.Add(time.Second)
	if pollSnapshotCurrent(snapshot, reactivated) {
		t.Fatal("重新启用产生新上游订单后应丢弃旧轮询结果")
	}
	prolonged := snapshot
	prolonged.UpdatedAt = updatedAt.Add(time.Second)
	if pollSnapshotCurrent(snapshot, prolonged) {
		t.Fatal("同一上游订单续租后也应丢弃续租前的轮询结果")
	}
}
