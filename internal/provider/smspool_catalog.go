package provider

import (
	"context"
	"math"
	"net/url"
	"sort"
	"strconv"

	"buysms/internal/domain"
)

// SMSPool 的 pricing 不带库存，必须在合并报价前按号码池查询 sms/stock。
// 仅定向报价补全库存，避免全量目录触发大量上游请求。
func (c *SMSPool) directedPriceCatalog(ctx context.Context, apiKey string, payload []byte, request CatalogRequest) ([]domain.CatalogItem, error) {
	invalid := func() error { return c.http.failure("catalog.price", "INVALID_RESPONSE", 0, false, nil) }
	value, err := decodeAny(payload)
	if err != nil {
		return nil, invalid()
	}
	if object, ok := value.(map[string]any); ok {
		if nested, found := lookup(object, "data", "offers", "prices", "items"); found {
			value = nested
		}
	}
	rows, ok := value.([]any)
	if !ok {
		return nil, invalid()
	}
	if len(rows) == 0 {
		return []domain.CatalogItem{}, nil
	}
	var result *domain.CatalogItem
	byPrice := make(map[float64]int)
	stockByPool := make(map[string]int)
	seen := make(map[string]bool)
	for _, row := range rows {
		object, ok := row.(map[string]any)
		if !ok {
			return nil, invalid()
		}
		item, ok := priceItemFromMap(object, nil, domain.ProviderSMSPool, request.Country, request.Service)
		if !ok || item.Price == nil || *item.Price <= 0 || math.IsNaN(*item.Price) || math.IsInf(*item.Price, 0) {
			return nil, invalid()
		}
		if item.Country != request.Country || item.Code != request.Service {
			continue
		}
		pool := firstIdentity(object, "pool", "pool_id", "poolId")
		identity := pool + ":" + strconv.FormatFloat(*item.Price, 'f', -1, 64)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		stock := 0
		if item.Stock != nil {
			stock = *item.Stock
		} else {
			if pool == "" {
				return nil, invalid()
			}
			var cached bool
			stock, cached = stockByPool[pool]
			if !cached {
				stock, err = c.catalogPoolStock(ctx, apiKey, item.Country, item.Code, pool)
				if err != nil {
					return nil, err
				}
				stockByPool[pool] = stock
			}
		}
		if stock < 0 || stock > int(^uint(0)>>1)-byPrice[*item.Price] {
			return nil, invalid()
		}
		byPrice[*item.Price] += stock
		if result == nil || *item.Price < *result.Price {
			copy := item
			result = &copy
		}
	}
	if result == nil {
		return []domain.CatalogItem{}, nil
	}
	options := make([]domain.CatalogPriceOption, 0, len(byPrice))
	for price, stock := range byPrice {
		if stock > 0 {
			options = append(options, domain.CatalogPriceOption{Price: price, Available: stock})
		}
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Price < options[j].Price })
	stock := 0
	if len(options) > 0 {
		price := options[0].Price
		result.Price = &price
		stock = options[0].Available
	}
	result.Stock = &stock
	result.PriceOptions = options
	return []domain.CatalogItem{*result}, nil
}

func (c *SMSPool) catalogPoolStock(ctx context.Context, apiKey, country, service, pool string) (int, error) {
	const operation = "catalog.stock"
	payload, err := c.http.form(ctx, operation, apiKey, "sms/stock", url.Values{
		"country": {country}, "service": {service}, "pool": {pool},
	})
	if err != nil {
		return 0, err
	}
	if err = c.ensureSuccess(operation, apiKey, payload, true); err != nil {
		return 0, err
	}
	value, err := decodeAny(payload)
	if err == nil {
		if object, ok := value.(map[string]any); ok {
			if raw, found := lookup(object, "amount"); found {
				if amount, ok := floatValue(raw); ok && amount == 0 {
					return 0, nil
				}
				if amount, ok := positiveInteger(raw); ok && float64(amount) < float64(int(^uint(0)>>1)) {
					return amount, nil
				}
			}
		}
	}
	return 0, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
}
