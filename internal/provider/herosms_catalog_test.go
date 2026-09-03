package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"buysms/internal/domain"
)

func TestHeroSMSCatalogReturnsOnlyPurchasablePriceOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/activations/offers/sms" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":{"hc":{"10":{"prices":{"default":0.108,"retail":0.108,"min":0.108},"counts":{"total":6800,"physical":3406,"defaultPrice":460},"map":{"0.2048":3406,"0.1576":1360,"0.1080":460,"0.1055":460,"0.0957":106,"0.3000":0,"invalid":12}}}}}`))
	}))
	t.Cleanup(server.Close)

	items, err := NewHeroSMS(server.URL+"/api/v1").Catalog(context.Background(), testAPIKey, CatalogRequest{
		Kind: CatalogPrice, Country: "10", Service: "hc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("HeroSMS 报价数量=%d，期望 1: %#v", len(items), items)
	}
	item := items[0]
	if item.Price == nil || *item.Price != 0.108 || item.Stock == nil || *item.Stock != 460 {
		t.Fatalf("HeroSMS 摘要报价错误: %#v", item)
	}
	want := []domain.CatalogPriceOption{
		{Price: 0.108, Available: 460},
		{Price: 0.1576, Available: 1360},
		{Price: 0.2048, Available: 3406},
	}
	if len(item.PriceOptions) != len(want) {
		t.Fatalf("HeroSMS 可购档位=%#v，期望 %#v", item.PriceOptions, want)
	}
	for index := range want {
		if item.PriceOptions[index] != want[index] {
			t.Fatalf("HeroSMS 第 %d 个可购档位=%#v，期望 %#v", index, item.PriceOptions[index], want[index])
		}
	}
}

func TestHeroSMSCatalogFallsBackToAvailableDefaultPrice(t *testing.T) {
	payload := []byte(`{"data":{"hc":{"10":{"prices":{"default":0.108,"min":0.108},"counts":{"total":999,"defaultPrice":12}}}}}`)
	items, err := parsePriceCatalog(payload, domain.ProviderHeroSMS, "10", "hc")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].PriceOptions) != 1 || items[0].PriceOptions[0].Price != 0.108 || items[0].PriceOptions[0].Available != 12 {
		t.Fatalf("HeroSMS 默认可购报价回退错误: %#v", items)
	}
}

func TestHeroSMSCatalogUsesDefaultOnlyWhenPersonalMinimumIsMissing(t *testing.T) {
	missingMinimum := []byte(`{"data":{"hc":{"10":{"prices":{"default":0.108},"counts":{"defaultPrice":4},"map":{"0.1079999999999":9,"0.108":4}}}}}`)
	items, err := parsePriceCatalog(missingMinimum, domain.ProviderHeroSMS, "10", "hc")
	if err != nil || len(items) != 1 || len(items[0].PriceOptions) != 1 || items[0].PriceOptions[0].Price != 0.108 {
		t.Fatalf("HeroSMS 缺少个人最低价时未正确使用默认价: items=%#v err=%v", items, err)
	}

	invalidMinimum := []byte(`{"data":{"hc":{"10":{"prices":{"default":0.108,"min":"invalid"},"counts":{"defaultPrice":4},"map":{"0.108":4}}}}}`)
	if items, err = parsePriceCatalog(invalidMinimum, domain.ProviderHeroSMS, "10", "hc"); err == nil {
		t.Fatalf("HeroSMS 非法个人最低价不应回退默认价: %#v", items)
	}
}

func TestHeroSMSCatalogIgnoresFractionalInventory(t *testing.T) {
	payload := []byte(`{"data":{"hc":{"10":{"prices":{"default":0.108,"min":0.108},"counts":{"defaultPrice":1.5},"map":{"0.108":1.5,"0.1576":2}}}}}`)
	items, err := parsePriceCatalog(payload, domain.ProviderHeroSMS, "10", "hc")
	if err != nil || len(items) != 1 || len(items[0].PriceOptions) != 1 || items[0].PriceOptions[0] != (domain.CatalogPriceOption{Price: 0.1576, Available: 2}) {
		t.Fatalf("HeroSMS 小数库存过滤错误: items=%#v err=%v", items, err)
	}
}

func TestHeroSMSCatalogMergesEquivalentPriceKeys(t *testing.T) {
	payload := []byte(`{"data":{"hc":{"10":{"prices":{"default":0.108,"min":0.108},"counts":{"defaultPrice":4},"map":{"0.108":4,"0.1080":6}}}}}`)
	items, err := parsePriceCatalog(payload, domain.ProviderHeroSMS, "10", "hc")
	if err != nil || len(items) != 1 || len(items[0].PriceOptions) != 1 || items[0].PriceOptions[0] != (domain.CatalogPriceOption{Price: 0.108, Available: 10}) {
		t.Fatalf("HeroSMS 等价价格库存合并错误: items=%#v err=%v", items, err)
	}
}

func TestHeroSMSLegacyCatalogKeepsCompatiblePriceResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/handler_api.php" || request.URL.Query().Get("action") != "getPrices" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"10":{"hc":{"cost":0.24,"count":9}}}`))
	}))
	t.Cleanup(server.Close)

	items, err := NewHeroSMS(server.URL+"/handler_api.php").Catalog(context.Background(), testAPIKey, CatalogRequest{
		Kind: CatalogPrice, Country: "10", Service: "hc",
	})
	if err != nil || len(items) != 1 || items[0].Price == nil || *items[0].Price != 0.24 || items[0].Stock == nil || *items[0].Stock != 9 {
		t.Fatalf("HeroSMS 兼容报价解析错误: items=%#v err=%v", items, err)
	}
}
