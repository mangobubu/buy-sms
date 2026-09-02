package provider

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"buysms/internal/domain"
)

const defaultSMSBowerBaseURL = "https://smsbower.page/stubs/handler_api.php"

type SMSBower struct {
	*smsActivateClient
	vitrine         *baseClient
	serviceFallback *baseClient
}

type smsBowerService struct {
	ID   int
	Code string
	Name string
	Raw  map[string]any
}

func NewSMSBower(baseURL string, options ...Option) *SMSBower {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultSMSBowerBaseURL
	}
	config := resolveOptions(options...)
	vitrineURL, fallbackURL := smsBowerVitrineURLs(baseURL)
	client := &SMSBower{
		smsActivateClient: newSMSActivateClient(domain.ProviderSMSBower, baseURL, config),
		vitrine:           newBaseClient(domain.ProviderSMSBower, vitrineURL, config),
	}
	if fallbackURL != "" {
		client.serviceFallback = newBaseClient(domain.ProviderSMSBower, fallbackURL, config)
	}
	return client
}

// NewSMSBrower 兼容用户侧常见的历史拼写。
func NewSMSBrower(baseURL string, options ...Option) *SMSBower {
	return NewSMSBower(baseURL, options...)
}

func (c *SMSBower) Catalog(ctx context.Context, apiKey string, request CatalogRequest) ([]domain.CatalogItem, error) {
	if err := require(apiKey); err != nil {
		return nil, err
	}
	kind, err := normalizeCatalogKind(request.Kind)
	if err != nil {
		return nil, err
	}
	tier, rank, err := smsBowerTierRank(request.QualityTier)
	if err != nil {
		return nil, err
	}
	request.QualityTier = tier

	// 服务目录继续使用账户 API，避免普通目录加载依赖公开购买页；只有
	// 解析等级报价时才额外读取包含内部 service id 的 vitrine 目录。
	if kind == CatalogService {
		// 等级不会改变服务字典；合法等级在上面完成校验后不向 handler
		// 透传，服务选择仍保持同一份稳定列表。
		request.QualityTier = ""
		return c.smsActivateClient.Catalog(ctx, apiKey, request)
	}
	if tier == "" {
		return c.smsActivateClient.Catalog(ctx, apiKey, request)
	}
	if strings.TrimSpace(request.Service) == "" {
		return nil, ErrInvalidRequest
	}

	service, err := c.resolveVitrineService(ctx, request.Service)
	if err != nil {
		return nil, err
	}
	countries, err := c.loadTierCountries(ctx, service.ID, rank)
	if err != nil {
		return nil, err
	}
	items := make([]domain.CatalogItem, 0, len(countries))
	for _, country := range countries {
		code := firstScalar(country, "activate_org_code")
		if code == "" || (kind == CatalogPrice && code != request.Country) {
			continue
		}
		priceOptions := smsBowerCountryPriceOptions(country, rank)
		price, hasPrice, stock := smsBowerCountrySummary(priceOptions)
		item := domain.CatalogItem{
			ProviderID:   domain.ProviderSMSBower,
			Kind:         kind,
			Code:         code,
			Name:         firstScalar(country, "title", "name"),
			PriceOptions: priceOptions,
			Raw:          rawJSON(country),
		}
		if kind == CatalogPrice {
			item.Code = service.Code
			item.Country = code
			item.Name = service.Name
		}
		if hasPrice {
			item.Price = &price
		}
		item.Stock = &stock
		items = append(items, item)
	}
	sortCatalog(items)
	return items, nil
}

func (c *SMSBower) Purchase(ctx context.Context, apiKey string, request PurchaseRequest) (PurchaseResult, error) {
	if err := require(apiKey, request.Country, request.Service); err != nil {
		return PurchaseResult{}, err
	}
	tier, rank, err := smsBowerTierRank(request.QualityTier)
	if err != nil {
		return PurchaseResult{}, err
	}
	request.QualityTier = tier
	if tier == "" {
		return c.smsActivateClient.Purchase(ctx, apiKey, request)
	}
	if request.MaxPrice == nil || *request.MaxPrice < 0 || math.IsNaN(*request.MaxPrice) || math.IsInf(*request.MaxPrice, 0) {
		return PurchaseResult{}, ErrInvalidRequest
	}

	// 购买前重新读取该等级的实时位置，避免沿用报价阶段已经变化的代理列表。
	service, err := c.resolveVitrineService(ctx, request.Service)
	if err != nil {
		return PurchaseResult{}, err
	}
	countries, err := c.loadTierCountries(ctx, service.ID, rank)
	if err != nil {
		return PurchaseResult{}, err
	}
	var selected map[string]any
	for _, country := range countries {
		if firstScalar(country, "activate_org_code") == request.Country {
			selected = country
			break
		}
	}
	selectedPrice := *request.MaxPrice
	providerIDs := smsBowerProviderIDs(selected, rank, selectedPrice)
	if len(providerIDs) == 0 {
		return PurchaseResult{}, c.vitrine.failure("purchase.tier", "NO_NUMBERS", 0, false, nil)
	}
	extra := make(map[string]string, len(request.Extra)+2)
	for key, value := range request.Extra {
		extra[key] = value
	}
	values := make([]string, len(providerIDs))
	for index, id := range providerIDs {
		values[index] = strconv.Itoa(id)
	}
	extra["providerIds"] = strings.Join(values, ",")
	extra["minPrice"] = strconv.FormatFloat(selectedPrice, 'f', -1, 64)
	request.Extra = extra
	result, err := c.smsActivateClient.Purchase(ctx, apiKey, request)
	if err == nil && result.Cost <= 0 {
		result.Cost = selectedPrice
	}
	return result, err
}

func smsBowerTierRank(value string) (string, int, error) {
	tier := strings.ToLower(strings.TrimSpace(value))
	switch tier {
	case "":
		return "", 0, nil
	case "gold":
		return tier, 1, nil
	case "silver":
		return tier, 2, nil
	case "bronze":
		return tier, 3, nil
	default:
		return "", 0, ErrInvalidRequest
	}
}

func smsBowerVitrineURLs(handlerURL string) (string, string) {
	parsed, err := url.Parse(strings.TrimSpace(handlerURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", ""
	}
	parsed.Path = "/"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	origin := parsed.String()

	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if parsed.Port() != "" || (host != "smsbower.page" && !strings.HasSuffix(host, ".smsbower.page")) {
		return origin, ""
	}
	fallback := *parsed
	fallback.Host = strings.TrimSuffix(host, ".page") + ".app"
	return origin, fallback.String()
}

func (c *SMSBower) publicGet(ctx context.Context, client *baseClient, operation, relative string, query url.Values) ([]byte, error) {
	endpoint, err := client.endpoint(relative)
	if err != nil {
		return nil, client.failure(operation, "INVALID_BASE_URL", 0, false, nil)
	}
	endpoint.RawQuery = query.Encode()
	payload, _, err := client.do(ctx, operation, http.MethodGet, endpoint, nil, nil)
	return payload, err
}

func (c *SMSBower) loadVitrineServices(ctx context.Context) ([]smsBowerService, error) {
	payload, err := c.publicGet(ctx, c.vitrine, "catalog.services", "services/getList", make(url.Values))
	if err != nil && c.serviceFallback != nil {
		var providerErr *ProviderError
		if errors.As(err, &providerErr) && providerErr.HTTPStatus == http.StatusNotFound {
			payload, err = c.publicGet(ctx, c.serviceFallback, "catalog.services", "services/getList", make(url.Values))
		}
	}
	if err != nil {
		return nil, err
	}
	value, err := decodeAny(payload)
	if err != nil {
		return nil, c.vitrine.failure("catalog.services", "INVALID_RESPONSE", 0, false, nil)
	}
	if object, ok := value.(map[string]any); ok {
		if nested, found := lookup(object, "services", "data"); found {
			value = nested
		}
	}
	list, ok := value.([]any)
	if !ok {
		return nil, c.vitrine.failure("catalog.services", "INVALID_RESPONSE", 0, false, nil)
	}
	services := make([]smsBowerService, 0, len(list))
	for _, raw := range list {
		object, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, idOK := intValue(object["id"])
		code := firstScalar(object, "activate_org_code", "code")
		name := firstScalar(object, "title", "name")
		if !idOK || id <= 0 || code == "" || name == "" {
			continue
		}
		services = append(services, smsBowerService{ID: id, Code: code, Name: name, Raw: object})
	}
	if len(services) == 0 {
		return nil, c.vitrine.failure("catalog.services", "INVALID_RESPONSE", 0, false, nil)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Code < services[j].Code })
	return services, nil
}

func (c *SMSBower) resolveVitrineService(ctx context.Context, code string) (smsBowerService, error) {
	services, err := c.loadVitrineServices(ctx)
	if err != nil {
		return smsBowerService{}, err
	}
	code = strings.TrimSpace(code)
	for _, service := range services {
		if service.Code == code {
			return service, nil
		}
	}
	return smsBowerService{}, c.vitrine.failure("catalog.services", "BAD_SERVICE", 0, false, nil)
}

func (c *SMSBower) loadTierCountries(ctx context.Context, serviceID, rank int) ([]map[string]any, error) {
	query := url.Values{
		"serviceId": {strconv.Itoa(serviceID)},
		"rank":      {strconv.Itoa(rank)},
	}
	payload, err := c.publicGet(ctx, c.vitrine, "catalog.tier", "activations/getPricesByService", query)
	if err != nil {
		return nil, err
	}
	value, err := decodeAny(payload)
	if err != nil {
		return nil, c.vitrine.failure("catalog.tier", "INVALID_RESPONSE", 0, false, nil)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, c.vitrine.failure("catalog.tier", "INVALID_RESPONSE", 0, false, nil)
	}
	servicesValue, found := lookup(root, "services")
	services, ok := servicesValue.(map[string]any)
	if !found || !ok {
		return nil, c.vitrine.failure("catalog.tier", "INVALID_RESPONSE", 0, false, nil)
	}
	serviceValue, found := services[strconv.Itoa(serviceID)]
	if !found {
		return nil, c.vitrine.failure("catalog.tier", "BAD_SERVICE", 0, false, nil)
	}
	service, ok := serviceValue.(map[string]any)
	if !ok {
		return nil, c.vitrine.failure("catalog.tier", "INVALID_RESPONSE", 0, false, nil)
	}
	countriesValue, found := lookup(service, "countries")
	countryMap, ok := countriesValue.(map[string]any)
	if !found || !ok {
		return nil, c.vitrine.failure("catalog.tier", "INVALID_RESPONSE", 0, false, nil)
	}
	countries := make([]map[string]any, 0, len(countryMap))
	for _, value := range countryMap {
		if country, ok := value.(map[string]any); ok {
			countries = append(countries, country)
		}
	}
	sort.Slice(countries, func(i, j int) bool {
		return firstScalar(countries[i], "activate_org_code") < firstScalar(countries[j], "activate_org_code")
	})
	return countries, nil
}

func smsBowerCountryPriceOptions(country map[string]any, rank int) []domain.CatalogPriceOption {
	positions := smsBowerPositions(country)
	byPrice := make(map[float64]int)
	for _, position := range positions {
		price, count, ok := smsBowerAvailablePosition(position, rank)
		if !ok {
			continue
		}
		byPrice[price] += count
	}
	if len(positions) == 0 {
		count, countOK := intValue(country["count"])
		price, priceOK := floatValue(country["min_price"])
		if countOK && count > 0 && priceOK && price >= 0 && !math.IsNaN(price) && !math.IsInf(price, 0) {
			byPrice[price] = count
		}
	}
	options := make([]domain.CatalogPriceOption, 0, len(byPrice))
	for price, available := range byPrice {
		options = append(options, domain.CatalogPriceOption{Price: price, Available: available})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Price < options[j].Price })
	return options
}

func smsBowerCountrySummary(options []domain.CatalogPriceOption) (float64, bool, int) {
	if len(options) == 0 {
		return 0, false, 0
	}
	return options[0].Price, true, options[0].Available
}

func smsBowerAvailablePosition(position map[string]any, rank int) (float64, int, bool) {
	if !smsBowerPositionHasRank(position, rank) {
		return 0, 0, false
	}
	price, priceOK := floatValue(position["price"])
	count, countOK := intValue(position["count"])
	if !priceOK || !countOK || price < 0 || count <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, 0, false
	}
	return price, count, true
}

func smsBowerPositions(country map[string]any) []map[string]any {
	value, found := lookup(country, "positions")
	positionsMap, ok := value.(map[string]any)
	if !found || !ok {
		return nil
	}
	positions := make([]map[string]any, 0, len(positionsMap))
	for _, raw := range positionsMap {
		if position, ok := raw.(map[string]any); ok {
			positions = append(positions, position)
		}
	}
	return positions
}

func smsBowerPositionHasRank(position map[string]any, expected int) bool {
	value, found := lookup(position, "rank")
	if !found {
		return true
	}
	rank, ok := value.(map[string]any)
	if !ok {
		return true
	}
	id, hasID := intValue(rank["id"])
	return !hasID || id == expected
}

func smsBowerProviderIDs(country map[string]any, rank int, selectedPrice float64) []int {
	if country == nil {
		return nil
	}
	unique := make(map[int]struct{})
	for _, position := range smsBowerPositions(country) {
		positionPrice, _, available := smsBowerAvailablePosition(position, rank)
		if !available || !smsBowerSamePrice(positionPrice, selectedPrice) {
			continue
		}
		rawIDs, _ := position["agent_ids"].([]any)
		for _, rawID := range rawIDs {
			id, ok := smsBowerNumericID(rawID)
			if !ok {
				continue
			}
			unique[id] = struct{}{}
		}
	}
	ids := make([]int, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func smsBowerSamePrice(left, right float64) bool {
	return !math.IsNaN(left) && !math.IsNaN(right) && !math.IsInf(left, 0) && !math.IsInf(right, 0) && math.Abs(left-right) <= 0.000001
}

func smsBowerNumericID(value any) (int, bool) {
	text := strings.TrimSpace(stringValue(value))
	parsed, err := strconv.ParseInt(text, 10, 32)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return int(parsed), true
}
