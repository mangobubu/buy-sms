package application

import (
	"reflect"
	"testing"

	"buysms/internal/domain"
)

func TestQuotePriceOptionsExposeProviderTiersAndGenericFallback(t *testing.T) {
	t.Run("供应商多价格档位", func(t *testing.T) {
		item := domain.CatalogItem{PriceOptions: []domain.CatalogPriceOption{
			{Price: 0.12, Available: 4},
			{Price: 0.3, Available: 2},
		}}
		want := []QuotePriceOptionDTO{
			{Price: "0.12", Available: 4},
			{Price: "0.3", Available: 2},
		}
		if got := quotePriceOptions(item, "0.12", 4); !reflect.DeepEqual(got, want) {
			t.Fatalf("价格档位=%+v，期望=%+v", got, want)
		}
	})

	t.Run("通用供应商单项兼容", func(t *testing.T) {
		want := []QuotePriceOptionDTO{{Price: "0.24", Available: 9}}
		if got := quotePriceOptions(domain.CatalogItem{}, "0.24", 9); !reflect.DeepEqual(got, want) {
			t.Fatalf("兼容价格档位=%+v，期望=%+v", got, want)
		}
	})
}
