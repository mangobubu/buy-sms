package application

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/provider"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type catalogCacheRepository struct {
	store.Repository
	provider     domain.Provider
	upsertErr    error
	replaceCalls int
	upsertCalls  int
	catalog      map[string]domain.CatalogItem
}

func (r *catalogCacheRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	if id != r.provider.ID {
		return domain.Provider{}, store.ErrNotFound
	}
	return r.provider, nil
}

func (r *catalogCacheRepository) ReplaceCatalog(_ context.Context, _ string, kind string, items []domain.CatalogItem) error {
	r.replaceCalls++
	return nil
}

func (r *catalogCacheRepository) UpsertCatalog(_ context.Context, _ string, items []domain.CatalogItem) error {
	r.upsertCalls++
	if r.upsertErr != nil {
		return r.upsertErr
	}
	if r.catalog == nil {
		r.catalog = make(map[string]domain.CatalogItem)
	}
	for _, item := range items {
		key := item.Kind + "\x00" + item.Code + "\x00" + item.Country
		existing := r.catalog[key]
		if item.Price == nil {
			item.Price = existing.Price
		}
		if item.Stock == nil {
			item.Stock = existing.Stock
		}
		r.catalog[key] = item
	}
	return nil
}

func TestFilteredReadableCatalogsArePersistedButDirectedPricesAreNot(t *testing.T) {
	for _, test := range []struct {
		name string
		req  provider.CatalogRequest
		want catalogPersistenceMode
	}{
		{name: "按服务和档位筛选的国家", req: provider.CatalogRequest{Kind: provider.CatalogCountry, Service: "hc", QualityTier: "gold"}, want: catalogUpsert},
		{name: "按国家和档位筛选的服务", req: provider.CatalogRequest{Kind: provider.CatalogService, Country: "10", QualityTier: "gold"}, want: catalogUpsert},
		{name: "仅按服务筛选的国家", req: provider.CatalogRequest{Kind: provider.CatalogCountry, Service: "hc"}, want: catalogUpsert},
		{name: "定向价格", req: provider.CatalogRequest{Kind: provider.CatalogPrice, Country: "10", Service: "hc", QualityTier: "gold"}, want: catalogSkip},
		{name: "仅按国家定向的价格", req: provider.CatalogRequest{Kind: provider.CatalogPrice, Country: "10"}, want: catalogSkip},
		{name: "全局价格", req: provider.CatalogRequest{Kind: provider.CatalogPrice}, want: catalogReplace},
		{name: "完整国家目录", req: provider.CatalogRequest{Kind: provider.CatalogCountry}, want: catalogReplace},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := catalogPersistence(test.req); got != test.want {
				t.Fatalf("catalogPersistence()=%v，期望 %v", got, test.want)
			}
		})
	}
}

func TestTierCatalogPersistenceKeepsNamesButDropsScopedPriceAndStock(t *testing.T) {
	price, stock := 1.25, 7
	items := []domain.CatalogItem{{
		Kind: provider.CatalogCountry, Code: "10", Name: "Readable Country",
		Price: &price, Stock: &stock,
	}}
	req := provider.CatalogRequest{
		Kind: provider.CatalogCountry, Service: "hc", QualityTier: "gold",
	}
	if got := catalogPersistence(req); got != catalogUpsert {
		t.Fatalf("等级目录持久化模式=%v，期望名称 upsert", got)
	}
	cached := catalogItemsForPersistence(req, items)
	if len(cached) != 1 || cached[0].Name != "Readable Country" || cached[0].Price != nil || cached[0].Stock != nil {
		t.Fatalf("等级目录缓存副本应仅保留可读名称: %+v", cached)
	}
	if items[0].Price == nil || items[0].Stock == nil {
		t.Fatalf("缓存清洗不应改写当前请求的等级报价: %+v", items[0])
	}
}

func TestFilteredCountryCatalogPersistsNamesAndPropagatesStoreFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/stubs/handler_api.php" || request.URL.Query().Get("action") != "getCountries" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[{"id":"10","eng":"Readable Country"}]`))
	}))
	t.Cleanup(server.Close)

	vault, err := secure.NewVault([]byte("catalog-cache-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, err := vault.Encrypt("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	repo := &catalogCacheRepository{
		provider: domain.Provider{
			ID: domain.ProviderSMSBower, BaseURL: server.URL + "/stubs/handler_api.php",
			APIKeyCipher: apiKey, Enabled: true,
		},
		catalog: map[string]domain.CatalogItem{
			provider.CatalogCountry + "\x00" + "20" + "\x00": {
				Kind: provider.CatalogCountry, Code: "20", Name: "Existing Country",
			},
		},
	}
	service := New(repo, nil, vault, config.Config{})
	req := provider.CatalogRequest{Kind: provider.CatalogCountry, Service: "hc"}

	items, err := service.catalog(context.Background(), domain.ProviderSMSBower, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Code != "10" || items[0].Name != "Readable Country" {
		t.Fatalf("国家目录解析错误: %+v", items)
	}
	if repo.upsertCalls != 1 || repo.replaceCalls != 0 {
		t.Fatalf("筛选国家目录持久化模式错误: upsert=%d replace=%d", repo.upsertCalls, repo.replaceCalls)
	}
	if saved := repo.catalog[provider.CatalogCountry+"\x00"+"10"+"\x00"]; saved.Name != "Readable Country" {
		t.Fatalf("筛选国家名称未写入: %+v", saved)
	}
	if saved := repo.catalog[provider.CatalogCountry+"\x00"+"20"+"\x00"]; saved.Name != "Existing Country" {
		t.Fatalf("筛选目录覆盖了未返回的既有国家: %+v", saved)
	}

	repo.upsertErr = errors.New("catalog store failed")
	_, err = service.catalog(context.Background(), domain.ProviderSMSBower, req)
	if !errors.Is(err, repo.upsertErr) {
		t.Fatalf("目录写入错误=%v，期望原样返回 %v", err, repo.upsertErr)
	}
}
