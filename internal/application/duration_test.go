package application

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

func heroDurationService(t *testing.T, baseURL string) (*Service, *purchaseRepository) {
	t.Helper()
	vault, err := secure.NewVault([]byte("duration-test-encryption-key"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := vault.Encrypt("duration-test-api-key")
	if err != nil {
		t.Fatal(err)
	}
	repo := newPurchaseRepository(domain.Provider{
		ID: domain.ProviderHeroSMS, Name: "HeroSMS", BaseURL: baseURL,
		APIKeyCipher: cipher, APIKeyConfigured: true, Enabled: true,
	})
	return New(repo, nil, vault, config.Config{}), repo
}

func TestDurationsCombinesDefaultQuoteAndRealtimeRentals(t *testing.T) {
	var defaultCalls, rentalCalls, quoteCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/classifiers/activations/custom-durations":
			defaultCalls.Add(1)
			_, _ = writer.Write([]byte(`{"data":{"tg":{"2":35}}}`))
		case "/stubs/handler_api.php":
			rentalCalls.Add(1)
			_, _ = writer.Write([]byte(`{"2":{"24":{"count":3,"price":1.25}}}`))
		case "/api/v1/activations/offers/sms":
			quoteCalls.Add(1)
			_, _ = writer.Write([]byte(`{"data":{"tg":{"2":{"prices":{"default":0.12,"min":0.12},"counts":{"defaultPrice":4},"map":{"0.12":4,"0.15":8}}}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	service, _ := heroDurationService(t, server.URL+"/api/v1")

	options, err := service.Durations(context.Background(), "hero-sms", "2", "tg")
	if err != nil {
		t.Fatal(err)
	}
	if defaultCalls.Load() != 1 || rentalCalls.Load() != 1 || quoteCalls.Load() != 1 {
		t.Fatalf("上游调用次数 default=%d rental=%d quote=%d", defaultCalls.Load(), rentalCalls.Load(), quoteCalls.Load())
	}
	if len(options) != 2 || options[0].Value != "" || options[0].Minutes != 35 ||
		options[0].Price != "0.12" || options[0].Available != 4 || len(options[0].PriceOptions) != 2 {
		t.Fatalf("普通时长选项未携带完整报价: %#v", options)
	}
	if options[1].Value != "24" || options[1].Minutes != 1440 || options[1].Hours != 24 ||
		options[1].Price != "1.25" || options[1].Available != 3 || len(options[1].PriceOptions) != 0 {
		t.Fatalf("长租选项错误: %#v", options[1])
	}
}

func TestDurationsKeepsRealtimeRentalsWhenDefaultQuoteIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/classifiers/activations/custom-durations":
			_, _ = writer.Write([]byte(`{"data":{}}`))
		case "/stubs/handler_api.php":
			_, _ = writer.Write([]byte(`{"2":{"24":{"count":3,"price":1.25}}}`))
		case "/api/v1/activations/offers/sms":
			http.Error(writer, "not found", http.StatusNotFound)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	service, _ := heroDurationService(t, server.URL+"/api/v1")

	options, err := service.Durations(context.Background(), "herosms", "2", "tg")
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0].Value != "24" || options[0].Price != "1.25" {
		t.Fatalf("普通报价不可用时应保留长租选项: %#v", options)
	}
}

func TestPurchaseRentalRevalidatesAndLocksRealtimePrice(t *testing.T) {
	var purchased atomic.Bool
	expiresAt := time.Date(2026, 9, 5, 12, 30, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/stubs/handler_api.php":
			if request.URL.Query().Get("action") != "serviceCountRent" {
				t.Errorf("购买前目录动作=%q", request.URL.Query().Get("action"))
			}
			_, _ = writer.Write([]byte(`{"2":{"24":{"count":3,"price":1.25}}}`))
		case "/api/v1/activations":
			purchased.Store(true)
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["duration"] != float64(24) || math.Abs(body["maxPrice"].(float64)-1.25) > 0.000001 || body["fixedPrice"] != true {
				t.Errorf("原生长租购买未锁定实时目录价: %#v", body)
			}
			_, _ = writer.Write([]byte(`{"status":"success","phone":{"id":1049,"number":"79990001122","endDate":"2026-09-05T12:30:00Z"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	service, repo := heroDurationService(t, server.URL+"/api/v1")
	purchaseStartedAt := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	var clockCalls atomic.Int32
	service.now = func() time.Time {
		if clockCalls.Add(1) == 1 {
			return purchaseStartedAt
		}
		return purchaseStartedAt.Add(10 * time.Second)
	}
	user := domain.User{ID: "duration-user"}
	input := purchaseInput("2", "duration-idempotency-key")
	input.Duration = "24"

	order, err := service.Purchase(context.Background(), input, user, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !purchased.Load() || order.Duration != "24" || order.Price != "1.25" ||
		order.ExpiresAt == nil || !order.ExpiresAt.Equal(expiresAt) ||
		!order.CreatedAt.Equal(purchaseStartedAt) {
		t.Fatalf("长租订单错误: %+v", order)
	}
	record, _, _, _, _ := repo.snapshot(user.ID, input.IdempotencyKey)
	if record.Duration != "24" || record.MaxPrice != 2 {
		t.Fatalf("购买意图未持久化原始时长和价格上限: %+v", record)
	}
}

func TestPurchaseRentalWithLegacyConfigurationUsesNativePriceLock(t *testing.T) {
	expiresAt := "2026-09-05T12:30:00Z"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/stubs/handler_api.php":
			if request.URL.Query().Get("action") != "serviceCountRent" {
				t.Errorf("长租预检动作=%q", request.URL.Query().Get("action"))
			}
			_, _ = writer.Write([]byte(`{"2":{"24":{"count":3,"price":1.25}}}`))
		case "/api/v1/activations":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["duration"] != float64(24) || body["maxPrice"] != 1.25 || body["fixedPrice"] != true {
				t.Errorf("原生长租没有锁定实时目录价: %#v", body)
			}
			_, _ = writer.Write([]byte(`{"status":"success","phone":{"id":1050,"number":"79990001122","endDate":"` + expiresAt + `"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	service, _ := heroDurationService(t, server.URL+"/stubs/handler_api.php")
	input := purchaseInput("2", "duration-legacy-cost-key")
	input.Duration = "24"

	order, err := service.Purchase(
		context.Background(), input, domain.User{ID: "duration-legacy-user"}, "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if order.Duration != "24" || order.Price != "1.25" || order.ExpiresAt == nil ||
		order.ExpiresAt.UTC().Format(time.RFC3339) != expiresAt {
		t.Fatalf("兼容配置的长租订单未锁定实时价格或供应商截止时间: %+v", order)
	}
}

func TestPurchaseIdempotencyIncludesDuration(t *testing.T) {
	service, repo := heroDurationService(t, "https://provider.invalid/api/v1")
	user := domain.User{ID: "duration-idempotency-user"}
	key := "duration-mismatch-key"
	repo.records[user.ID+"\x00"+key] = storePurchaseRecordForDuration(user.ID, key, "24")
	input := purchaseInput("1.25", key)
	input.Duration = "48"

	_, err := service.Purchase(context.Background(), input, user, "127.0.0.1")
	requirePurchaseError(t, err, "idempotency_mismatch", "该购买编号已用于其他条件，页面将生成新的购买请求")
}

func storePurchaseRecordForDuration(userID, key, duration string) store.PurchaseRecord {
	return store.PurchaseRecord{
		ID: "stored-duration", UserID: userID, IdempotencyKey: key,
		ProviderID: domain.ProviderHeroSMS, CountryCode: "2", ServiceCode: "tg",
		Duration: duration, MaxPrice: 1.25, Status: "provisioning",
	}
}
