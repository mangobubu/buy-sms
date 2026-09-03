package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHeroSMSPurchaseUsesSelectedPriceExactly(t *testing.T) {
	const maxPrice = 0.1055

	t.Run("原生接口默认固定所选价格", func(t *testing.T) {
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodPost || request.URL.Path != "/api/v1/activations" {
				http.NotFound(writer, request)
				return
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("HeroSMS 原生购买请求不是有效 JSON: %v", err)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":[{"id":"native-fixed-price","phone":"77001234567","countryPhoneCode":"7","price":0.1055}]}`))
		}))
		t.Cleanup(server.Close)

		client := NewHeroSMS(server.URL + "/api/v1")
		if _, err := client.Purchase(context.Background(), "test-key", PurchaseRequest{
			Country: "2", Service: "tg", MaxPrice: float64Pointer(maxPrice),
		}); err != nil {
			t.Fatalf("HeroSMS 原生购买失败: %v", err)
		}
		if body["fixedPrice"] != true || body["maxPrice"] != maxPrice {
			t.Fatalf("HeroSMS 原生购买未固定所选价格: %#v", body)
		}
	})

	t.Run("兼容接口默认固定所选价格", func(t *testing.T) {
		var fixedPrice, receivedMaxPrice string
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodGet || request.URL.Path != "/handler_api.php" {
				http.NotFound(writer, request)
				return
			}
			fixedPrice = request.URL.Query().Get("fixedPrice")
			receivedMaxPrice = request.URL.Query().Get("maxPrice")
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"activationId":"legacy-fixed-price","phoneNumber":"79990001122","activationCost":0.1055,"countryCode":"7"}`))
		}))
		t.Cleanup(server.Close)

		client := NewHeroSMS(server.URL + "/handler_api.php")
		if _, err := client.Purchase(context.Background(), "test-key", PurchaseRequest{
			Country: "2", Service: "tg", MaxPrice: float64Pointer(maxPrice),
		}); err != nil {
			t.Fatalf("HeroSMS 兼容接口购买失败: %v", err)
		}
		if fixedPrice != "true" || receivedMaxPrice != "0.1055" {
			t.Fatalf("HeroSMS 兼容购买参数错误: fixedPrice=%q maxPrice=%q", fixedPrice, receivedMaxPrice)
		}
	})

	t.Run("显式价格策略优先", func(t *testing.T) {
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("HeroSMS 原生购买请求不是有效 JSON: %v", err)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":[{"id":"native-price-cap","phone":"77001234567","countryPhoneCode":"7","price":0.1055}]}`))
		}))
		t.Cleanup(server.Close)

		fixedPrice := false
		client := NewHeroSMS(server.URL + "/api/v1")
		if _, err := client.Purchase(context.Background(), "test-key", PurchaseRequest{
			Country: "2", Service: "tg", MaxPrice: float64Pointer(maxPrice), FixedPrice: &fixedPrice,
		}); err != nil {
			t.Fatalf("HeroSMS 显式上限价格购买失败: %v", err)
		}
		if body["fixedPrice"] != false {
			t.Fatalf("HeroSMS 覆盖了显式价格策略: %#v", body)
		}
	})

	t.Run("未选择价格时不注入固定价格", func(t *testing.T) {
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("HeroSMS 原生购买请求不是有效 JSON: %v", err)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":[{"id":"native-no-price","phone":"77001234567","countryPhoneCode":"7","price":0.1055}]}`))
		}))
		t.Cleanup(server.Close)

		client := NewHeroSMS(server.URL + "/api/v1")
		if _, err := client.Purchase(context.Background(), "test-key", PurchaseRequest{
			Country: "2", Service: "tg",
		}); err != nil {
			t.Fatalf("HeroSMS 未指定价格购买失败: %v", err)
		}
		if _, exists := body["fixedPrice"]; exists {
			t.Fatalf("HeroSMS 未指定价格时注入了 fixedPrice: %#v", body)
		}
	})
}

func float64Pointer(value float64) *float64 {
	return &value
}
