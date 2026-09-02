package provider

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"buysms/internal/domain"
)

func normalizeCatalogKind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case CatalogCountry, "countries":
		return CatalogCountry, nil
	case CatalogService, "services":
		return CatalogService, nil
	case CatalogPrice, "prices", "offer", "offers":
		return CatalogPrice, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedKind, kind)
	}
}

func parseSimpleCatalog(payload []byte, providerID, kind, country string) ([]domain.CatalogItem, error) {
	value, err := decodeAny(payload)
	if err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
		for _, wrapper := range []string{"data", "countries", "services", "items"} {
			if nested, found := lookup(object, wrapper); found {
				value = nested
				break
			}
		}
	}

	entries := make([]struct {
		fallback string
		value    any
	}, 0)
	switch typed := value.(type) {
	case []any:
		for _, entry := range typed {
			entries = append(entries, struct {
				fallback string
				value    any
			}{value: entry})
		}
	case map[string]any:
		for key, entry := range typed {
			entries = append(entries, struct {
				fallback string
				value    any
			}{fallback: key, value: entry})
		}
	default:
		return nil, fmt.Errorf("目录响应格式无效")
	}

	items := make([]domain.CatalogItem, 0, len(entries))
	for _, entry := range entries {
		object, ok := entry.value.(map[string]any)
		if !ok {
			continue
		}
		code := firstScalar(object, "code", "id", "ID", "short_name", "shortName", "iso")
		if code == "" {
			code = strings.TrimSpace(entry.fallback)
		}
		if code == "" {
			continue
		}
		name := firstScalar(object, "eng", "name", "title", "rus", "chn")
		if name == "" {
			name = code
		}
		itemCountry := ""
		if kind != CatalogCountry {
			itemCountry = strings.TrimSpace(country)
			if itemCountry == "" {
				itemCountry = firstScalar(object, "country", "country_id", "countryId", "short_name")
			}
		}
		items = append(items, domain.CatalogItem{
			ProviderID: providerID,
			Kind:       kind,
			Code:       code,
			Country:    itemCountry,
			Name:       name,
			Raw:        rawJSON(object),
		})
	}
	sortCatalog(items)
	return items, nil
}

func parsePriceCatalog(payload []byte, providerID, fallbackCountry, fallbackService string) ([]domain.CatalogItem, error) {
	value, err := decodeAny(payload)
	if err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
		if nested, found := lookup(object, "data", "offers", "prices", "items"); found {
			value = nested
		}
	}

	candidates := make([]domain.CatalogItem, 0)
	var walk func(any, []string)
	walk = func(current any, route []string) {
		switch typed := current.(type) {
		case []any:
			for index, child := range typed {
				walk(child, append(route, fmt.Sprintf("%d", index)))
			}
		case map[string]any:
			if item, ok := priceItemFromMap(typed, route, providerID, fallbackCountry, fallbackService); ok {
				candidates = append(candidates, item)
				return
			}
			for key, child := range typed {
				if _, nested := child.(map[string]any); nested {
					walk(child, append(route, key))
					continue
				}
				if _, nested := child.([]any); nested {
					walk(child, append(route, key))
				}
			}
		}
	}
	walk(value, nil)

	// 多个号码池可能为同一国家、服务返回不同报价；数据库目录以
	// country+service 唯一，因此保留最低报价和最大的可见库存。
	coalesced := make(map[string]domain.CatalogItem)
	for _, item := range candidates {
		key := item.Country + "\x00" + item.Code
		previous, found := coalesced[key]
		if !found {
			coalesced[key] = item
			continue
		}
		if item.Price != nil && (previous.Price == nil || *item.Price < *previous.Price) {
			previous.Price = item.Price
			previous.Raw = item.Raw
		}
		if item.Stock != nil && (previous.Stock == nil || *item.Stock > *previous.Stock) {
			previous.Stock = item.Stock
		}
		coalesced[key] = previous
	}
	items := make([]domain.CatalogItem, 0, len(coalesced))
	for _, item := range coalesced {
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("价格目录响应格式无效")
	}
	sortCatalog(items)
	return items, nil
}

func priceItemFromMap(object map[string]any, route []string, providerID, fallbackCountry, fallbackService string) (domain.CatalogItem, bool) {
	// HeroSMS 原生 offers 的层级是 data[service][country]，价格和库存又分别
	// 位于 prices/counts 对象中。
	if rawPrices, found := lookup(object, "prices"); found {
		prices, pricesOK := rawPrices.(map[string]any)
		rawCounts, _ := lookup(object, "counts")
		counts, _ := rawCounts.(map[string]any)
		if pricesOK {
			price, hasPrice := firstFloat(prices, "default", "retail", "min", "price", "cost")
			stock, hasStock := firstInt(counts, "total", "defaultPrice", "physical", "count", "stock")
			if hasPrice || hasStock {
				service := strings.TrimSpace(fallbackService)
				country := strings.TrimSpace(fallbackCountry)
				if service == "" && len(route) > 0 {
					service = route[0]
				}
				if country == "" && len(route) > 1 {
					country = route[1]
				}
				if service != "" {
					item := domain.CatalogItem{
						ProviderID: providerID, Kind: CatalogPrice, Code: service,
						Country: country, Name: service, Raw: rawJSON(object),
					}
					if hasPrice {
						value := price
						item.Price = &value
					}
					if hasStock {
						value := stock
						item.Stock = &value
					}
					return item, true
				}
			}
		}
	}

	price, hasPrice := firstFloat(object, "price", "cost", "low_price", "lowPrice", "fixedPrice", "high_price")
	stock, hasStock := firstInt(object, "count", "stock", "quantity", "available")
	if !hasPrice && !hasStock {
		return domain.CatalogItem{}, false
	}

	service := firstIdentity(object, "service", "serviceCode", "service_code", "serviceId", "service_id", "code")
	country := firstIdentity(object, "country", "countryCode", "country_code", "countryId", "country_id")
	if service == "" {
		service = strings.TrimSpace(fallbackService)
	}
	if country == "" {
		country = strings.TrimSpace(fallbackCountry)
	}
	if service == "" && len(route) > 0 {
		service = route[len(route)-1]
	}
	if country == "" && len(route) > 1 {
		country = route[len(route)-2]
	}
	if service == "" {
		return domain.CatalogItem{}, false
	}
	name := firstScalar(object, "serviceName", "service_name", "name", "title")
	if name == "" {
		name = service
	}
	item := domain.CatalogItem{
		ProviderID: providerID,
		Kind:       CatalogPrice,
		Code:       service,
		Country:    country,
		Name:       name,
		Raw:        rawJSON(object),
	}
	if hasPrice {
		value := price
		item.Price = &value
	}
	if hasStock {
		value := stock
		item.Stock = &value
	}
	return item, true
}

func firstScalar(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, found := lookup(object, key); found {
			if text := stringValue(value); text != "" {
				return text
			}
		}
	}
	return ""
}

func firstIdentity(object map[string]any, keys ...string) string {
	for _, key := range keys {
		value, found := lookup(object, key)
		if !found {
			continue
		}
		if text := stringValue(value); text != "" {
			return text
		}
		if nested, ok := value.(map[string]any); ok {
			if text := firstScalar(nested, "code", "id", "ID", "short_name", "name"); text != "" {
				return text
			}
		}
	}
	return ""
}

func firstFloat(object map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, found := lookup(object, key); found {
			if number, ok := floatValue(value); ok {
				return number, true
			}
		}
	}
	return 0, false
}

func firstInt(object map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if value, found := lookup(object, key); found {
			if number, ok := intValue(value); ok {
				return number, true
			}
		}
	}
	return 0, false
}

func sortCatalog(items []domain.CatalogItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Country != items[j].Country {
			return items[i].Country < items[j].Country
		}
		return items[i].Code < items[j].Code
	})
}

// rawObject 保留结构化原始数据，同时确保输出始终是有效 JSON。
func rawObject(payload []byte) json.RawMessage {
	if json.Valid(payload) {
		return append(json.RawMessage(nil), payload...)
	}
	encoded, _ := json.Marshal(strings.TrimSpace(string(payload)))
	return encoded
}
