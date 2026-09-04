package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHeroSMSDurationsUsesPublicLifetimeAndRealtimeRentCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/classifiers/activations/custom-durations":
			if request.Header.Get("Authorization") != "" || request.URL.Query().Get("api_key") != "" {
				t.Errorf("公开时长字典不应携带密钥")
			}
			_, _ = writer.Write([]byte(`{"data":{"tg":{"2":"35"}}}`))
		case "/stubs/handler_api.php":
			query := request.URL.Query()
			if query.Get("action") != "serviceCountRent" || query.Get("service") != "tg" ||
				query.Get("country") != "2" || query.Get("api_key") != "test-key" {
				t.Errorf("长租目录请求参数错误: %s", request.URL.String())
			}
			_, _ = writer.Write([]byte(`{"2":{"48":{"count":0,"price":2},"24":{"count":"3","price":"1.25"},"012":{"count":2,"price":1},"bad":{"count":2,"price":1}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	options, err := NewHeroSMS(server.URL+"/api/v1").Durations(
		context.Background(), "test-key", CatalogRequest{Country: "2", Service: "tg"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 2 {
		t.Fatalf("时长选项=%#v，期望普通激活和一个有库存的长租选项", options)
	}
	if options[0] != (DurationOption{Minutes: 35}) {
		t.Fatalf("普通激活时长=%#v，期望35分钟", options[0])
	}
	want := DurationOption{Value: "24", Minutes: 1440, Hours: 24, Price: 1.25, Available: 3}
	if options[1] != want {
		t.Fatalf("长租选项=%#v，期望=%#v", options[1], want)
	}
}

func TestHeroSMSDefaultMinutesFallsBackToTwenty(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte(`{"data":{}}`),
		[]byte(`{"tg":{"10":45}}`),
	} {
		minutes, err := parseHeroSMSDefaultMinutes(payload, "tg", "2")
		if err != nil || minutes != 20 {
			t.Fatalf("缺省普通时长=%d err=%v，期望20分钟", minutes, err)
		}
	}
}

func TestHeroSMSDurationsReturnsAvailableSideWhenOtherCatalogFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/classifiers/activations/custom-durations":
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
		case "/stubs/handler_api.php":
			_, _ = writer.Write([]byte(`{"2":{"24":{"count":3,"price":1.25}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	options, err := NewHeroSMS(server.URL+"/api/v1").Durations(
		context.Background(), "test-key", CatalogRequest{Country: "2", Service: "tg"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0].Value != "24" {
		t.Fatalf("默认寿命接口失败时应保留实时长租: %#v", options)
	}
}

func TestHeroSMSDurationsKeepsDefaultWhenRentalCatalogFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/classifiers/activations/custom-durations":
			_, _ = writer.Write([]byte(`{"data":{"tg":{"2":35}}}`))
		case "/stubs/handler_api.php":
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	options, err := NewHeroSMS(server.URL+"/api/v1").Durations(
		context.Background(), "test-key", CatalogRequest{Country: "2", Service: "tg"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0] != (DurationOption{Minutes: 35}) {
		t.Fatalf("长租目录失败时应保留普通激活时长: %#v", options)
	}
}

func TestHeroSMSLegacyConfigurationPurchasesRentalThroughNativeAPI(t *testing.T) {
	expiresAt := time.Date(2026, 9, 5, 12, 30, 0, 0, time.UTC)
	maxPrice := 2.0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/activations" {
			t.Errorf("兼容配置的长租未走原生购买接口: %s %s", request.Method, request.URL.String())
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["duration"] != float64(24) || body["maxPrice"] != maxPrice || body["fixedPrice"] != true {
			t.Errorf("兼容配置的原生长租参数错误: %#v", body)
		}
		_, _ = writer.Write([]byte(`{"status":"success","phone":{"id":1049,"number":"79990001122","endDate":"2026-09-05T12:30:00Z"}}`))
	}))
	t.Cleanup(server.Close)

	result, err := NewHeroSMS(server.URL+"/stubs/handler_api.php").Purchase(
		context.Background(), "test-key",
		PurchaseRequest{Country: "2", Service: "tg", Duration: "24", MaxPrice: &maxPrice},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.UpstreamID != "1049" || result.Cost != 0 || result.ExpiresAt == nil || !result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("长租购买响应解析错误: %#v", result)
	}
}

func TestParseHeroSMSLegacyRentalNestedPhoneResponse(t *testing.T) {
	expiresAt := time.Date(2026, 9, 5, 12, 30, 0, 0, time.UTC)
	result, err := parseActivationPurchase([]byte(
		`{"status":"success","phone":{"id":1049,"number":"79990001122","endDate":"2026-09-05T12:30:00Z"}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if result.UpstreamID != "1049" || result.PhoneNumber != "79990001122" ||
		result.Cost != 0 || result.ExpiresAt == nil || !result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("兼容长租嵌套响应解析错误: %#v", result)
	}
}

func TestHeroSMSRejectsNonCanonicalRentalDurationBeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(server.Close)

	_, err := NewHeroSMS(server.URL+"/stubs/handler_api.php").Purchase(
		context.Background(), "test-key",
		PurchaseRequest{Country: "2", Service: "tg", Duration: "024"},
	)
	if err == nil {
		t.Fatal("非规范租期应被拒绝")
	}
	if called {
		t.Fatal("参数无效时不应请求供应商")
	}
}

func TestHeroSMSLegacyConfigurationUsesNativeLifecycleForDurationOrders(t *testing.T) {
	requests := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "ApiKey test-key" {
			t.Errorf("原生生命周期认证头错误: %q", request.Header.Get("Authorization"))
		}
		requests = append(requests, request.Method+" "+request.URL.Path)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/activations/rent-1/otp":
			_, _ = writer.Write([]byte(`{"data":[]}`))
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/activations/rent-1/finish":
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && request.URL.Path == "/api/v1/activations/rent-1":
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	client := NewHeroSMS(server.URL + "/stubs/handler_api.php")
	lifecycle := DurationLifecycleClient(client)
	if _, err := lifecycle.PollDuration(context.Background(), "test-key", "rent-1"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.CompleteDuration(context.Background(), "test-key", "rent-1"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.CancelDuration(context.Background(), "test-key", "rent-1"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("原生生命周期请求=%v，期望轮询、完成、取消各一次", requests)
	}
	for _, request := range requests {
		if strings.Contains(request, "handler_api") {
			t.Fatalf("带时长订单后续动作误走兼容接口: %v", requests)
		}
	}
}
