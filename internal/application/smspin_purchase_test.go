package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

const smspinOperatorsResponse = `{"operators":[
	{"operator":758,"label":"Operator 758","price":0.1765,"count":9998},
	{"operator":1,"label":"Operator 1","price":0.1800,"count":67},
	{"operator":6,"label":"Operator 6","price":0.1820,"count":185}
]}`

func newSMSPinApplicationService(t *testing.T, baseURL string) (*Service, *purchaseRepository) {
	t.Helper()
	vault, err := secure.NewVault([]byte("smspin-application-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, err := vault.Encrypt("smspin-secret")
	if err != nil {
		t.Fatal(err)
	}
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderSMSPin, BaseURL: baseURL + "/api/v1", APIKeyCipher: apiKey, Enabled: true,
	})
	return New(repo, nil, vault, config.Config{}), repo
}

// smspinUnconfirmedRepository exposes the optional persistence hook used when
// SMSPin has already created an upstream order but its actual price is above
// the selected cap. The purchase request remains unknown and therefore blocks
// retries, while the returned number is retained for reconciliation.
type smspinUnconfirmedRepository struct {
	*purchaseRepository
	saveCalls int
	saveErr   error
	saved     []domain.Order
	savedCode string
}

func (r *smspinUnconfirmedRepository) SaveUnconfirmedPurchase(ctx context.Context, recordID string, order domain.Order, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveCalls++
	if r.saveErr != nil {
		return r.saveErr
	}
	for key, record := range r.records {
		if record.ID != recordID || record.Status != "provisioning" {
			continue
		}
		record.Status = "unknown"
		record.ErrorCode = code
		record.OrderID = order.ID
		r.records[key] = record
		r.failCodes[key] = code
		r.saved = append(r.saved, order)
		r.savedCode = code
		r.orders[order.ID] = order
		return nil
	}
	return store.ErrConflict
}

func (r *smspinUnconfirmedRepository) savedSnapshot() ([]domain.Order, string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Order(nil), r.saved...), r.savedCode, r.saveCalls
}

var _ store.UnconfirmedPurchaseRepository = (*smspinUnconfirmedRepository)(nil)

func TestSMSPinQuoteMapsPerOperatorPrices(t *testing.T) {
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/operators" {
			http.NotFound(w, r)
			return
		}
		gets.Add(1)
		if r.URL.Query().Get("country") != "VN" || r.URL.Query().Get("service") != "momo" {
			t.Errorf("operators query=%v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(smspinOperatorsResponse))
	}))
	t.Cleanup(server.Close)
	service, _ := newSMSPinApplicationService(t, server.URL)

	quote, err := service.Quote(context.Background(), domain.ProviderSMSPin, "VN", "momo", "")
	if err != nil {
		t.Fatalf("Quote error=%v", err)
	}
	if gets.Load() != 1 {
		t.Fatalf("operators GET 次数=%d，期望 1", gets.Load())
	}
	if quote.Provider != domain.ProviderSMSPin || quote.CountryCode != "VN" || quote.ServiceCode != "momo" {
		t.Fatalf("quote identity=%+v", quote)
	}
	if quote.Price != "0.1765" || quote.Available != 10250 || len(quote.PriceOptions) != 3 {
		t.Fatalf("quote aggregate=%+v", quote)
	}
	want := []struct {
		operator int
		price    string
		stock    int
	}{
		{758, "0.1765", 9998}, {1, "0.18", 67}, {6, "0.182", 185},
	}
	for i, expected := range want {
		got := quote.PriceOptions[i]
		if got.Operator != expected.operator || got.Price != expected.price || got.Available != expected.stock {
			t.Fatalf("price option[%d]=%+v，期望 operator=%d price=%s stock=%d", i, got, expected.operator, expected.price, expected.stock)
		}
	}
}

func TestSMSPinPurchaseSubmitsSelectedOperator(t *testing.T) {
	var gets, posts atomic.Int32
	var submitted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/operators":
			gets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(smspinOperatorsResponse))
		case "POST /api/v1/orders":
			posts.Add(1)
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Errorf("order body decode: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"smspin-order-758","phone":"84991112233","price":0.1765}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	service, repo := newSMSPinApplicationService(t, server.URL)
	user := domain.User{ID: "operator-smspin-selected", Role: "operator"}
	const key = "idem-smspin-selected-12345"
	in := PurchaseInput{Provider: domain.ProviderSMSPin, CountryCode: "VN", ServiceCode: "momo", Operator: 758, MaxPrice: "0.2", IdempotencyKey: key}
	order, err := service.Purchase(context.Background(), in, user, "127.0.0.1")
	if err != nil {
		t.Fatalf("Purchase error=%v", err)
	}
	if gets.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("上游请求次数 GET=%d POST=%d", gets.Load(), posts.Load())
	}
	if submitted["country"] != "VN" || submitted["service"] != "momo" {
		t.Fatalf("order body=%v", submitted)
	}
	if operator, ok := submitted["operator"].(float64); !ok || operator != 758 {
		t.Fatalf("submitted operator=%v", submitted["operator"])
	}
	if order.PhoneNumber != "84991112233" || order.Price != "0.1765" || order.Provider != domain.ProviderSMSPin {
		t.Fatalf("order=%+v", order)
	}
	record, _, orders, _, completes := repo.snapshot(user.ID, key)
	if record.Operator != 758 || record.Status != "succeeded" || orders != 1 || completes != 1 {
		t.Fatalf("purchase record=%+v orders=%d completes=%d", record, orders, completes)
	}
}

func TestSMSPinPurchaseIdempotencyRejectsOperatorChange(t *testing.T) {
	var gets, posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/operators":
			gets.Add(1)
			_, _ = w.Write([]byte(smspinOperatorsResponse))
		case "POST /api/v1/orders":
			posts.Add(1)
			_, _ = w.Write([]byte(`{"id":"smspin-order-idem","phone":"84991110000","price":0.1765}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	service, _ := newSMSPinApplicationService(t, server.URL)
	user := domain.User{ID: "operator-smspin-idem", Role: "operator"}
	const key = "idem-smspin-operator-12345"
	first := PurchaseInput{Provider: domain.ProviderSMSPin, CountryCode: "VN", ServiceCode: "momo", Operator: 758, MaxPrice: "0.2", IdempotencyKey: key}
	if _, err := service.Purchase(context.Background(), first, user, "127.0.0.1"); err != nil {
		t.Fatalf("first Purchase error=%v", err)
	}
	second := first
	second.Operator = 1
	_, err := service.Purchase(context.Background(), second, user, "127.0.0.1")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("operator changed error=%v，期望冲突", err)
	}
	requirePurchaseError(t, err, "idempotency_mismatch", "该购买编号已用于其他条件，页面将生成新的购买请求")
	if gets.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("更换 operator 不应访问上游: GET=%d POST=%d", gets.Load(), posts.Load())
	}
}

func TestSMSPinPurchasePreflightBlocksOrderSubmission(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		maxPrice string
		wantCode string
	}{
		{name: "涨价", status: http.StatusOK, body: `{"operators":[{"operator":758,"price":0.3,"count":2}]}`, maxPrice: "0.2", wantCode: "price_exceeded"},
		{name: "404无库存", status: http.StatusNotFound, body: `{"error":"NO_NUMBERS"}`, maxPrice: "0.2", wantCode: "no_numbers"},
		{name: "空运营商列表", status: http.StatusOK, body: `{"operators":[]}`, maxPrice: "0.2", wantCode: "no_numbers"},
		{name: "全部零库存", status: http.StatusOK, body: `{"operators":[{"operator":758,"price":0.1765,"count":0},{"operator":1,"price":0.18,"count":0}]}`, maxPrice: "0.2", wantCode: "no_numbers"},
		{name: "GET失败", status: http.StatusBadGateway, body: `upstream unavailable`, maxPrice: "0.2", wantCode: "provider_preflight_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gets, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/operators":
					gets.Add(1)
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.body))
				case "POST /api/v1/orders":
					posts.Add(1)
					_, _ = w.Write([]byte(`{"id":"unexpected","phone":"84991112222","price":0.1}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			service, repo := newSMSPinApplicationService(t, server.URL)
			user := domain.User{ID: "operator-smspin-preflight-" + tt.name, Role: "operator"}
			key := "idem-smspin-preflight-" + tt.name + "-12345"
			in := PurchaseInput{Provider: domain.ProviderSMSPin, CountryCode: "VN", ServiceCode: "momo", Operator: 758, MaxPrice: tt.maxPrice, IdempotencyKey: key}
			_, err := service.Purchase(context.Background(), in, user, "127.0.0.1")
			if err == nil {
				t.Fatalf("期望错误，实际成功")
			}
			requirePurchaseError(t, err, tt.wantCode, map[string]string{
				"price_exceeded":           "供应商实际价格超过所选价格，购买已取消",
				"no_numbers":               "所选条件当前暂无可用号码，请稍后重试或调整条件",
				"provider_preflight_error": "购买前获取供应商资源失败，号码购买尚未提交；可以重新提交当前平台与购买条件",
			}[tt.wantCode])
			if gets.Load() != 1 || posts.Load() != 0 {
				t.Fatalf("前置失败仍提交订单: GET=%d POST=%d", gets.Load(), posts.Load())
			}
			record, code, orders, _, completes := repo.snapshot(user.ID, key)
			if record.Status != "failed" || code != tt.wantCode || orders != 0 || completes != 0 {
				t.Fatalf("前置失败状态异常: record=%+v code=%q orders=%d completes=%d", record, code, orders, completes)
			}
		})
	}
}

func TestSMSPinPurchaseWithoutOperatorUsesCheapestOption(t *testing.T) {
	var submitted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/operators":
			_, _ = w.Write([]byte(smspinOperatorsResponse))
		case "POST /api/v1/orders":
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Errorf("order body decode: %v", err)
			}
			_, _ = w.Write([]byte(`{"id":"smspin-order-default","phone":"84991113333","price":0.1765}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	service, _ := newSMSPinApplicationService(t, server.URL)
	in := PurchaseInput{Provider: domain.ProviderSMSPin, CountryCode: "VN", ServiceCode: "momo", MaxPrice: "0.2", IdempotencyKey: "idem-smspin-default-12345"}
	order, err := service.Purchase(context.Background(), in, domain.User{ID: "operator-smspin-default", Role: "operator"}, "127.0.0.1")
	if err != nil {
		t.Fatalf("Purchase error=%v", err)
	}
	if operator, ok := submitted["operator"].(float64); !ok || operator != 758 {
		t.Fatalf("默认 operator 未选择最低价档: %v", submitted["operator"])
	}
	if order.Price != "0.1765" {
		t.Fatalf("默认档位价格=%q", order.Price)
	}
}
