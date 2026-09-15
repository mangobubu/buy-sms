package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"buysms/internal/domain"
)

const defaultSMSPinBaseURL = "https://smspin.io/api/v1"

type SMSPin struct{ http *baseClient }

func NewSMSPin(baseURL string, options ...Option) *SMSPin {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultSMSPinBaseURL
	}
	return &SMSPin{http: newBaseClient(domain.ProviderSMSPin, baseURL, resolveOptions(options...))}
}
func (c *SMSPin) ID() string { return domain.ProviderSMSPin }

func (c *SMSPin) apiGet(ctx context.Context, operation, key, relative string, query url.Values) ([]byte, error) {
	if err := require(key); err != nil {
		return nil, err
	}
	u, err := c.http.endpoint(relative)
	if err != nil {
		return nil, c.http.failure(operation, "INVALID_BASE_URL", 0, false, nil)
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	h := make(http.Header)
	h.Set("X-API-Key", key)
	payload, _, callErr := c.http.do(ctx, operation, http.MethodGet, u, h, nil, key)
	callErr = smsPinNormalizeError(callErr, operation)
	return payload, callErr
}
func (c *SMSPin) apiJSON(ctx context.Context, operation, key, method, relative string, value any) ([]byte, error) {
	if err := require(key); err != nil {
		return nil, err
	}
	u, err := c.http.endpoint(relative)
	if err != nil {
		return nil, c.http.failure(operation, "INVALID_BASE_URL", 0, false, nil)
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, c.http.failure(operation, "INVALID_REQUEST", 0, false, nil)
	}
	h := make(http.Header)
	h.Set("X-API-Key", key)
	h.Set("Content-Type", "application/json")
	payload, _, callErr := c.http.do(ctx, operation, method, u, h, body, key)
	callErr = smsPinNormalizeError(callErr, operation)
	return payload, callErr
}

func (c *SMSPin) Balance(ctx context.Context, key string) (BalanceResult, error) {
	payload, err := c.apiGet(ctx, "balance", key, "account", nil)
	if err != nil {
		return BalanceResult{}, err
	}
	value, e := decodeAny(payload)
	if e != nil {
		return BalanceResult{}, c.http.failure("balance", "INVALID_RESPONSE", 0, false, nil)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return BalanceResult{}, c.http.failure("balance", "INVALID_RESPONSE", 0, false, nil)
	}
	raw, found := lookup(obj, "balance")
	if !found {
		return BalanceResult{}, c.http.failure("balance", "INVALID_RESPONSE", 0, false, nil)
	}
	amount := stringValue(raw)
	if amount == "" {
		return BalanceResult{}, c.http.failure("balance", "INVALID_RESPONSE", 0, false, nil)
	}
	return BalanceResult{Amount: amount, Currency: "USD"}, nil
}

func (c *SMSPin) Catalog(ctx context.Context, key string, req CatalogRequest) ([]domain.CatalogItem, error) {
	if strings.TrimSpace(req.QualityTier) != "" {
		return nil, ErrInvalidRequest
	}
	if err := require(key); err != nil {
		return nil, err
	}
	kind, err := normalizeCatalogKind(req.Kind)
	if err != nil {
		return nil, err
	}
	switch kind {
	case CatalogCountry:
		payload, e := c.apiGet(ctx, "catalog.country", key, "countries", nil)
		if e != nil {
			return nil, e
		}
		items, e := parseSimpleCatalog(payload, domain.ProviderSMSPin, CatalogCountry, "")
		if e != nil {
			return nil, c.http.failure("catalog.country", "INVALID_RESPONSE", 0, false, nil)
		}
		return items, nil
	case CatalogService:
		payload, e := c.apiGet(ctx, "catalog.service", key, "services", nil)
		if e != nil {
			return nil, e
		}
		items, e := parseSimpleCatalog(payload, domain.ProviderSMSPin, CatalogService, req.Country)
		if e != nil {
			return nil, c.http.failure("catalog.service", "INVALID_RESPONSE", 0, false, nil)
		}
		return items, nil
	case CatalogPrice:
		// SMSPin exposes per-operator price/stock through /operators whenever a
		// concrete country+service is requested. The aggregate /numbers route is
		// retained only for non-directed catalogue refreshes.
		query := make(url.Values)
		if req.Country != "" {
			query.Set("country", req.Country)
		}
		if req.Service != "" {
			query.Set("service", req.Service)
		}
		if req.Country != "" && req.Service != "" {
			payload, e := c.apiGet(ctx, "catalog.price", key, "operators", query)
			if e != nil {
				return nil, e
			}
			items, parseErr := c.parseOperatorsCatalog(payload, req)
			if parseErr != nil {
				return nil, c.http.failure("catalog.price", "INVALID_RESPONSE", 0, false, nil)
			}
			return items, nil
		}
		payload, e := c.apiGet(ctx, "catalog.price", key, "numbers", query)
		if e != nil {
			return nil, e
		}
		return c.parseNumbersCatalog(payload, req)
	}
	return nil, ErrUnsupportedKind
}

func (c *SMSPin) parseOperatorsCatalog(payload []byte, req CatalogRequest) ([]domain.CatalogItem, error) {
	value, err := decodeAny(payload)
	if err != nil {
		return nil, err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("operators response")
	}
	raw, found := lookup(obj, "operators")
	if !found {
		return nil, fmt.Errorf("operators response missing operators")
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("operators response operators type")
	}
	options := make([]domain.CatalogPriceOption, 0, len(list))
	seen := make(map[int]int, len(list))
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		rawOperator, _ := lookup(m, "operator")
		op, ok := smsPinPositiveInteger(rawOperator, 2147483647)
		if !ok {
			continue
		}
		price, ok := firstFloat(m, "price")
		if !ok || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			continue
		}
		rawAvailable, _ := lookup(m, "count")
		available, ok := smsPinPositiveInteger(rawAvailable, int64(^uint(0)>>1))
		if !ok {
			continue
		}
		label := firstScalar(m, "label")
		option := domain.CatalogPriceOption{Price: price, Available: available, Operator: op, Label: label}
		if index, exists := seen[op]; exists {
			// One operator id is one selectable route. Conflicting prices are
			// ambiguous; repeated rows must not inflate that route's inventory.
			if option.Price != options[index].Price {
				return nil, fmt.Errorf("operator has conflicting prices")
			}
			if option.Available > options[index].Available {
				options[index].Available = option.Available
			}
			if options[index].Label == "" {
				options[index].Label = option.Label
			}
			continue
		}
		seen[op] = len(options)
		options = append(options, option)
	}
	sort.Slice(options, func(i, j int) bool {
		if options[i].Price == options[j].Price {
			return options[i].Operator < options[j].Operator
		}
		return options[i].Price < options[j].Price
	})
	var price *float64
	if len(options) > 0 {
		value := options[0].Price
		price = &value
	}
	stock := 0
	for _, option := range options {
		if option.Available <= int(^uint(0)>>1)-stock {
			stock += option.Available
		}
	}
	return []domain.CatalogItem{{ProviderID: domain.ProviderSMSPin, Kind: CatalogPrice, Code: req.Service, Country: req.Country, Name: req.Service, Price: price, Stock: &stock, PriceOptions: options, Raw: cloneRaw(payload)}}, nil
}

// smsPinPositiveInteger deliberately rejects decimal spellings such as
// "2.0"; operator display ids and inventory counts are integer API fields.
func smsPinPositiveInteger(value any, max int64) (int, bool) {
	var number int64
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	case int:
		number = int64(typed)
	case int64:
		number = typed
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed > float64(max) {
			return 0, false
		}
		number = int64(typed)
	default:
		return 0, false
	}
	if number <= 0 || number > max || number > int64(^uint(0)>>1) {
		return 0, false
	}
	return int(number), true
}

func (c *SMSPin) parseNumbersCatalog(payload []byte, req CatalogRequest) ([]domain.CatalogItem, error) {
	value, e := decodeAny(payload)
	if e != nil {
		return nil, e
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("numbers response")
	}
	price, hasPrice := firstFloat(obj, "price")
	countriesRaw, _ := lookup(obj, "countries")
	items := make([]domain.CatalogItem, 0)
	if req.Country != "" && req.Service != "" {
		stock := 0
		if list, ok := countriesRaw.([]any); ok {
			for _, entry := range list {
				if m, ok := entry.(map[string]any); ok && strings.EqualFold(firstScalar(m, "code", "id"), req.Country) {
					stock, _ = firstInt(m, "available", "stock", "count")
					break
				}
			}
		}
		item := domain.CatalogItem{ProviderID: domain.ProviderSMSPin, Kind: CatalogPrice, Code: req.Service, Country: req.Country, Name: req.Service, Raw: cloneRaw(payload)}
		if hasPrice {
			item.Price = &price
		}
		item.Stock = &stock
		items = append(items, item)
		return items, nil
	}
	if list, ok := countriesRaw.([]any); ok {
		for _, entry := range list {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			country := firstScalar(m, "code", "id")
			if country == "" {
				continue
			}
			stock, _ := firstInt(m, "available", "stock", "count")
			item := domain.CatalogItem{ProviderID: domain.ProviderSMSPin, Kind: CatalogPrice, Code: req.Service, Country: country, Name: req.Service, Stock: &stock, Raw: rawJSON(m)}
			if hasPrice {
				v := price
				item.Price = &v
			}
			items = append(items, item)
		}
	}
	if len(items) == 0 && hasPrice && req.Service != "" && req.Country != "" {
		item := domain.CatalogItem{ProviderID: domain.ProviderSMSPin, Kind: CatalogPrice, Code: req.Service, Country: req.Country, Name: req.Service, Price: &price, Raw: cloneRaw(payload)}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("numbers response empty")
	}
	sortCatalog(items)
	return items, nil
}

func (c *SMSPin) Purchase(ctx context.Context, key string, req PurchaseRequest) (PurchaseResult, error) {
	if strings.TrimSpace(req.QualityTier) != "" || req.Duration != "" || req.Pool != "" {
		return PurchaseResult{}, ErrInvalidRequest
	}
	if err := require(key, req.Country, req.Service); err != nil {
		return PurchaseResult{}, err
	}
	body := map[string]any{"country": req.Country, "service": req.Service}
	if strings.TrimSpace(req.Operator) != "" {
		op, e := strconv.Atoi(strings.TrimSpace(req.Operator))
		if e != nil || op <= 0 || op > 2147483647 {
			return PurchaseResult{}, ErrInvalidRequest
		}
		body["operator"] = op
	}
	payload, err := c.apiJSON(ctx, "purchase", key, http.MethodPost, "orders", body)
	if err != nil {
		return PurchaseResult{}, err
	}
	value, e := decodeAny(payload)
	if e != nil {
		return PurchaseResult{}, c.http.failure("purchase", "INVALID_RESPONSE", 0, false, nil)
	}
	result, e := purchaseResultFromValue(value, cloneRaw(payload))
	if e != nil {
		return PurchaseResult{}, c.http.failure("purchase", "INVALID_RESPONSE", 0, false, nil)
	}
	result.CanGetAnotherSMS = false
	return result, nil
}

func (c *SMSPin) Poll(ctx context.Context, key, id string) (PollResult, error) {
	if err := require(key, id); err != nil {
		return PollResult{}, err
	}
	payload, err := c.apiGet(ctx, "poll", key, "orders/"+url.PathEscape(id), nil)
	if err != nil {
		return PollResult{}, err
	}
	value, e := decodeAny(payload)
	if e != nil {
		return PollResult{}, c.http.failure("poll", "INVALID_RESPONSE", 0, false, nil)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return PollResult{}, c.http.failure("poll", "INVALID_RESPONSE", 0, false, nil)
	}
	status := strings.ToLower(firstScalar(obj, "status", "state"))
	state := PollUnknown
	switch status {
	case "active", "pending", "waiting":
		state = PollWaiting
	case "completed", "success", "received":
		state = PollCompleted
	case "cancelled", "canceled":
		state = PollCanceled
	case "expired":
		state = PollExpired
	default:
		state = normalizePollState(status)
	}
	result := PollResult{State: state, Raw: cloneRaw(payload)}
	if raw, found := lookup(obj, "expiresAt", "expires_at", "expiry"); found {
		result.ExpiresAt = parseTimeValue(raw)
	}
	code := firstScalar(obj, "otpCode", "otp_code", "code")
	text := firstScalar(obj, "message", "text", "sms")
	if code != "" || text != "" {
		msg := singleOTP(code, text, time.Time{})
		if mid := firstScalar(obj, "messageId", "message_id", "smsId", "sms_id"); mid != "" {
			msg.UpstreamID = mid
			msg.Fingerprint = mid
		}
		result.Messages = []OTPMessage{msg}
		result.Code = code
		result.Text = text
		if state == PollWaiting {
			result.State = PollReceived
		}
	}
	return result, nil
}
func (c *SMSPin) Complete(ctx context.Context, key, id string) error {
	if err := require(key, id); err != nil {
		return err
	}
	return ctx.Err()
}
func (c *SMSPin) Cancel(ctx context.Context, key, id string) error {
	if err := require(key, id); err != nil {
		return err
	}
	return ctx.Err()
}
func (c *SMSPin) RequestAnother(ctx context.Context, key, id string) (RequestAnotherResult, error) {
	if err := require(key, id); err != nil {
		return RequestAnotherResult{}, err
	}
	return RequestAnotherResult{}, c.http.failure("request_another", "UNSUPPORTED", 0, false, nil)
}

func smsPinNormalizeError(err error, operation string) error {
	var upstream *ProviderError
	if !errors.As(err, &upstream) {
		return err
	}
	code := upstream.Code
	switch upstream.HTTPStatus {
	case http.StatusPaymentRequired:
		code = "INSUFFICIENT_BALANCE"
	case http.StatusNotFound:
		if operation == "purchase" || strings.HasPrefix(operation, "catalog.") {
			code = "NO_NUMBERS"
		}
	case http.StatusTooManyRequests:
		code = "RATE_LIMIT"
	}
	if code == upstream.Code {
		return err
	}
	return &ProviderError{Provider: upstream.Provider, Operation: upstream.Operation, Code: code, HTTPStatus: upstream.HTTPStatus, Retryable: upstream.Retryable}
}

var _ Client = (*SMSPin)(nil)
