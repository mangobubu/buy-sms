package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"buysms/internal/domain"
)

const defaultSMSPoolBaseURL = "https://api.smspool.net"

const smsPoolCancelNotAvailableMessage = "Your order cannot be cancelled yet, please try again later."

type SMSPool struct {
	http *baseClient
}

func NewSMSPool(baseURL string, options ...Option) *SMSPool {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultSMSPoolBaseURL
	}
	return &SMSPool{http: newBaseClient(domain.ProviderSMSPool, baseURL, resolveOptions(options...))}
}

func (c *SMSPool) ID() string { return domain.ProviderSMSPool }

func (c *SMSPool) Balance(ctx context.Context, apiKey string) (BalanceResult, error) {
	if err := require(apiKey); err != nil {
		return BalanceResult{}, err
	}
	payload, err := c.http.form(ctx, "balance", apiKey, "request/balance", make(url.Values))
	if err != nil {
		return BalanceResult{}, err
	}
	if businessErr := c.ensureSuccess("balance", apiKey, payload, false); businessErr != nil {
		return BalanceResult{}, businessErr
	}
	amount, ok := smsPoolBalanceAmount(payload)
	if !ok {
		return BalanceResult{}, c.http.failure("balance", "INVALID_RESPONSE", 0, false, nil)
	}
	return BalanceResult{Amount: amount, Currency: "USD"}, nil
}

func smsPoolBalanceAmount(payload []byte) (string, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return "", false
	}
	var raw json.RawMessage
	for key, value := range object {
		if strings.EqualFold(key, "balance") {
			raw = value
			break
		}
	}
	if len(raw) == 0 {
		return "", false
	}
	var amount string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &amount); err != nil {
			return "", false
		}
	} else {
		amount = string(raw)
	}
	return validBalanceAmount(amount)
}

func (c *SMSPool) Catalog(ctx context.Context, apiKey string, request CatalogRequest) ([]domain.CatalogItem, error) {
	if strings.TrimSpace(request.QualityTier) != "" {
		return nil, ErrInvalidRequest
	}
	if err := require(apiKey); err != nil {
		return nil, err
	}
	kind, err := normalizeCatalogKind(request.Kind)
	if err != nil {
		return nil, err
	}
	values := make(url.Values)
	if request.Country != "" {
		values.Set("country", request.Country)
	}
	if request.Service != "" {
		values.Set("service", request.Service)
	}
	endpoint := ""
	switch kind {
	case CatalogCountry:
		endpoint = "country/retrieve_all"
	case CatalogService:
		endpoint = "service/retrieve_all"
	case CatalogPrice:
		endpoint = "request/pricing"
	}
	payload, err := c.http.form(ctx, "catalog."+kind, apiKey, endpoint, values)
	if err != nil {
		return nil, err
	}
	if businessErr := c.ensureSuccess("catalog."+kind, apiKey, payload, false); businessErr != nil {
		return nil, businessErr
	}
	if kind == CatalogPrice {
		items, parseErr := parsePriceCatalog(payload, domain.ProviderSMSPool, request.Country, request.Service)
		if parseErr != nil {
			return nil, c.http.failure("catalog.price", "INVALID_RESPONSE", 0, false, nil)
		}
		return items, nil
	}
	items, parseErr := parseSimpleCatalog(payload, domain.ProviderSMSPool, kind, request.Country)
	if parseErr != nil {
		return nil, c.http.failure("catalog."+kind, "INVALID_RESPONSE", 0, false, nil)
	}
	return items, nil
}

func (c *SMSPool) RenewalOptions(ctx context.Context, apiKey, upstreamID, mode string) ([]RenewalOption, error) {
	if err := require(apiKey, upstreamID); err != nil {
		return nil, err
	}
	if mode != RenewalReactivate {
		return nil, ErrInvalidRequest
	}
	order, found, err := c.smsPoolOrder(ctx, apiKey, "renewal.options.reactivate", "request/history", url.Values{
		"start": {"0"}, "length": {"100"}, "search": {upstreamID},
	}, upstreamID)
	if err != nil {
		return nil, err
	}
	if !found || !smsPoolReactivationEligible(order) {
		return []RenewalOption{}, nil
	}
	price, ok := firstFloat(order, "reactivation_cost", "reactivate_cost", "cost", "price")
	if !ok || price <= 0 {
		return nil, c.http.failure("renewal.options.reactivate", "INVALID_RESPONSE", 0, false, nil)
	}
	return []RenewalOption{{Value: 1, Unit: "activation", Price: price}}, nil
}

func (c *SMSPool) Renew(ctx context.Context, apiKey, upstreamID string, request RenewalRequest) (RenewalResult, error) {
	if err := require(apiKey, upstreamID); err != nil {
		return RenewalResult{}, err
	}
	if request.Mode != RenewalReactivate || request.Value != 1 || request.Unit != "activation" {
		return RenewalResult{}, ErrInvalidRequest
	}
	payload, err := c.http.form(ctx, "renewal.reactivate", apiKey, "sms/reactivate", url.Values{"orderid": {upstreamID}})
	if err != nil {
		return RenewalResult{}, err
	}
	if err = c.ensureSuccess("renewal.reactivate", apiKey, payload, true); err != nil {
		return RenewalResult{}, err
	}

	order, found, lookupErr := c.lookupSMSPoolActiveWithRetry(ctx, apiKey, upstreamID, "renewal.result.active")
	if lookupErr != nil {
		return RenewalResult{}, lookupErr
	}
	if !found {
		return RenewalResult{}, c.http.failure("renewal.reactivate", "RESULT_NOT_CONFIRMED", 0, true, nil)
	}
	result, resultErr := renewalResultFromSMSPoolOrder(order, upstreamID, "renewal.reactivate")
	if resultErr != nil {
		return RenewalResult{}, resultErr
	}
	return result, nil
}

func (c *SMSPool) smsPoolOrder(ctx context.Context, apiKey, operation, endpoint string, values url.Values, matchID string) (map[string]any, bool, error) {
	payload, err := c.http.form(ctx, operation, apiKey, endpoint, values)
	if err != nil {
		return nil, false, err
	}
	if err = c.ensureSuccess(operation, apiKey, payload, false); err != nil {
		return nil, false, err
	}
	value, err := decodeAny(payload)
	if err != nil {
		return nil, false, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
	}
	objects := make([]map[string]any, 0)
	collectSMSPoolOrders(value, &objects)
	if len(objects) == 0 {
		return nil, false, nil
	}
	searchID := strings.TrimSpace(matchID)
	if searchID == "" && len(objects) == 1 {
		return objects[0], true, nil
	}
	for _, object := range objects {
		if searchID == "" || smsPoolOrderMatches(object, searchID) {
			return object, true, nil
		}
	}
	return nil, false, nil
}

func collectSMSPoolOrders(value any, output *[]map[string]any) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectSMSPoolOrders(item, output)
		}
	case map[string]any:
		if firstScalar(typed, "order_code", "orderCode", "order_id", "orderid", "id") != "" {
			*output = append(*output, typed)
			return
		}
		for _, key := range []string{"data", "orders", "items", "rows", "result"} {
			if nested, found := lookup(typed, key); found {
				collectSMSPoolOrders(nested, output)
			}
		}
	}
}

func smsPoolOrderMatches(order map[string]any, upstreamID string) bool {
	return strings.EqualFold(firstScalar(order, "order_code", "orderCode", "order_id", "orderid", "id"), strings.TrimSpace(upstreamID))
}

func smsPoolReactivationEligible(order map[string]any) bool {
	pool := strings.ToLower(firstIdentity(order, "pool", "pool_id", "poolId", "pool_name", "poolName"))
	if pool != "7" && pool != "foxtrot" {
		return false
	}
	country := strings.ToLower(firstIdentity(order, "short_name", "shortName", "country_code", "countryCode", "country", "country_id", "countryId", "country_name", "countryName"))
	country = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(country)
	switch country {
	case "1", "us", "usa", "unitedstates", "unitedstatesofamerica":
	default:
		return false
	}
	explicitAllowed := false
	if raw, found := lookup(order, "can_reactivate", "canReactivate", "reactivate_available"); found {
		allowed, valid := boolValue(raw)
		if !valid || !allowed {
			return false
		}
		explicitAllowed = true
	}
	if smsPoolOrderHasCode(order) {
		return false
	}
	status := strings.ToLower(firstScalar(order, "status", "state"))
	switch status {
	case "5", "6", "cancelled", "canceled", "refunded":
		return true
	case "2", "expired":
		return explicitAllowed
	default:
		return false
	}
}
func smsPoolOrderHasCode(order map[string]any) bool {
	for _, key := range []string{"code", "sms", "full_code", "fullCode", "full_sms", "fullSms"} {
		raw, found := lookup(order, key)
		if !found {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(stringValue(raw)))
		if value != "" && value != "0" && value != "null" {
			return true
		}
	}
	return false
}

func smsPoolOrderExpiry(order map[string]any) *time.Time {
	if raw, found := lookup(order, "expiry", "expiration", "expiresAt", "expires_at"); found {
		if expiresAt := parseTimeValue(raw); expiresAt != nil {
			return expiresAt
		}
	}
	if raw, found := lookup(order, "time_left", "timeLeft"); found {
		if seconds, ok := intValue(raw); ok && seconds >= 0 {
			expiresAt := time.Now().UTC().Add(time.Duration(seconds) * time.Second)
			return &expiresAt
		}
	}
	return nil
}
func (c *SMSPool) Purchase(ctx context.Context, apiKey string, request PurchaseRequest) (PurchaseResult, error) {
	if strings.TrimSpace(request.QualityTier) != "" {
		return PurchaseResult{}, ErrInvalidRequest
	}
	if err := require(apiKey, request.Country, request.Service); err != nil {
		return PurchaseResult{}, err
	}
	values := url.Values{
		"country":         {request.Country},
		"service":         {request.Service},
		"activation_type": {"SMS"},
	}
	if request.Pool != "" {
		values.Set("pool", request.Pool)
	}
	if request.MaxPrice != nil {
		values.Set("max_price", strconv.FormatFloat(*request.MaxPrice, 'f', -1, 64))
	}
	for key, value := range request.Extra {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "", "key", "api_key", "country", "service", "orderid":
			continue
		}
		values.Set(key, value)
	}
	payload, err := c.http.form(ctx, "purchase", apiKey, "purchase/sms", values)
	if err != nil {
		return PurchaseResult{}, err
	}
	if businessErr := c.ensureSuccess("purchase", apiKey, payload, true); businessErr != nil {
		return PurchaseResult{}, businessErr
	}
	value, parseErr := decodeAny(payload)
	if parseErr != nil {
		return PurchaseResult{}, c.http.failure("purchase", "INVALID_RESPONSE", 0, false, nil)
	}
	result, parseErr := purchaseResultFromValue(value, cloneRaw(payload))
	if parseErr != nil {
		return PurchaseResult{}, c.http.failure("purchase", "INVALID_RESPONSE", 0, false, nil)
	}
	result.CanGetAnotherSMS = true
	return result, nil
}

func (c *SMSPool) Poll(ctx context.Context, apiKey, upstreamID string) (PollResult, error) {
	if err := require(apiKey, upstreamID); err != nil {
		return PollResult{}, err
	}
	payload, err := c.http.form(ctx, "poll", apiKey, "sms/check", url.Values{"orderid": {upstreamID}})
	if err != nil {
		return PollResult{}, err
	}
	value, parseErr := decodeAny(payload)
	if parseErr != nil {
		return PollResult{}, c.http.failure("poll", "INVALID_RESPONSE", 0, false, nil)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return PollResult{}, c.http.failure("poll", "INVALID_RESPONSE", 0, false, nil)
	}
	state := normalizePollState(firstScalar(object, "status", "state"))
	result := PollResult{State: state, Raw: cloneRaw(payload)}
	if raw, found := lookup(object, "expiration", "expiry", "expiresAt"); found {
		result.ExpiresAt = parseTimeValue(raw)
	}
	if result.ExpiresAt == nil {
		if raw, found := lookup(object, "time_left", "timeLeft"); found {
			if seconds, ok := intValue(raw); ok && seconds >= 0 {
				expiresAt := time.Now().UTC().Add(time.Duration(seconds) * time.Second)
				result.ExpiresAt = &expiresAt
			}
		}
	}
	code := firstScalar(object, "sms", "code")
	text := firstScalar(object, "full_sms", "fullSms", "text", "message_text")
	if code != "" || text != "" {
		if !isTerminalPollState(result.State) {
			result.State = PollReceived
		}
		var receivedAt time.Time
		if raw, found := lookup(object, "received_at", "receivedAt", "timestamp", "sms_date"); found {
			if parsed := parseTimeValue(raw); parsed != nil {
				receivedAt = *parsed
			}
		}
		generation, _ := firstInt(object, "resend", "resends", "generation")
		message := singleOTP(code, text, receivedAt)
		message.Generation = generation
		message.Fingerprint = fingerprint(code, text, strconv.Itoa(generation), receivedAt.UTC().Format(time.RFC3339Nano))
		if id := firstScalar(object, "sms_id", "message_id", "id"); id != "" {
			message.UpstreamID = id
			message.Fingerprint = id
		}
		result.Messages = []OTPMessage{message}
		result.CanRequestAnother = true
	}
	if result.State == PollUnknown {
		return PollResult{}, c.http.failure("poll", "UNKNOWN_STATUS", 0, true, nil)
	}
	applyPollConvenience(&result)
	return result, nil
}

func (c *SMSPool) Complete(ctx context.Context, apiKey, upstreamID string) error {
	if err := require(apiKey, upstreamID); err != nil {
		return err
	}
	// SMSPool 在收到验证码时自动完成订单，没有单独的结算端点。
	return ctx.Err()
}

func (c *SMSPool) Cancel(ctx context.Context, apiKey, upstreamID string) error {
	return c.action(ctx, apiKey, upstreamID, "sms/cancel", "cancel")
}

func (c *SMSPool) RequestAnother(ctx context.Context, apiKey, upstreamID string) (RequestAnotherResult, error) {
	if err := require(apiKey, upstreamID); err != nil {
		return RequestAnotherResult{}, err
	}
	values := url.Values{"orderid": {upstreamID}}
	check, err := c.http.form(ctx, "check_resend", apiKey, "sms/check_resend", values)
	if err != nil {
		return RequestAnotherResult{}, err
	}
	if err = c.ensureSuccess("check_resend", apiKey, check, true); err != nil {
		return RequestAnotherResult{}, err
	}
	quoted := responseNumber(check, "resendCost")
	resend, err := c.http.form(ctx, "request_another", apiKey, "sms/resend", values)
	if err != nil {
		return RequestAnotherResult{}, err
	}
	if err = c.ensureSuccess("request_another", apiKey, resend, true); err != nil {
		return RequestAnotherResult{}, err
	}
	charge, found := responseNumberOK(resend, "charge")
	if !found {
		charge = quoted
	}
	if charge < 0 {
		charge = 0
	}
	return RequestAnotherResult{Charge: charge}, nil
}

func responseNumber(payload []byte, key string) float64 {
	v, _ := responseNumberOK(payload, key)
	return v
}
func responseNumberOK(payload []byte, key string) (float64, bool) {
	v, err := decodeAny(payload)
	if err != nil {
		return 0, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	raw, found := lookup(m, key)
	if !found {
		return 0, false
	}
	return floatValue(raw)
}

func (c *SMSPool) action(ctx context.Context, apiKey, upstreamID, endpoint, operation string) error {
	if err := require(apiKey, upstreamID); err != nil {
		return err
	}
	payload, err := c.http.form(ctx, operation, apiKey, endpoint, url.Values{"orderid": {upstreamID}})
	if err != nil {
		return err
	}
	return c.ensureSuccess(operation, apiKey, payload, true)
}

func (c *SMSPool) ensureSuccess(operation, apiKey string, payload []byte, required bool) error {
	value, err := decodeAny(payload)
	if err != nil {
		if required {
			return c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
		}
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	rawSuccess, found := lookup(object, "success")
	if !found {
		if required {
			return c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
		}
		return nil
	}
	success, valid := boolValue(rawSuccess)
	if valid && success {
		return nil
	}
	code := firstScalar(object, "type", "code", "error")
	if operation == "cancel" && smsPoolCancelNotAvailable(object) {
		code = CodeCancelNotAvailableYet
	}
	code = sanitizeCodeWithoutSecrets(code, apiKey)
	if code == "" {
		code = "UPSTREAM_REJECTED"
	}
	retryable := strings.EqualFold(code, "OUT_OF_STOCK") || strings.EqualFold(code, CodeCancelNotAvailableYet)
	return c.http.failure(operation, code, 0, retryable, nil)
}

func smsPoolCancelNotAvailable(object map[string]any) bool {
	message := firstScalar(object, "message")
	return strings.EqualFold(strings.TrimSpace(message), smsPoolCancelNotAvailableMessage)
}

var _ Client = (*SMSPool)(nil)
var _ RenewalClient = (*SMSPool)(nil)

// ReconcileRenewal 查询 SMSPool 当前活跃订单，恢复已经提交的重新启用
// 结果；整个流程只读，不会再次调用 sms/reactivate。
func (c *SMSPool) ReconcileRenewal(ctx context.Context, apiKey, upstreamID string, request RenewalRequest) (RenewalResult, bool, error) {
	if err := require(apiKey, upstreamID); err != nil {
		return RenewalResult{}, false, err
	}
	if request.Mode != RenewalReactivate || request.Value != 1 || request.Unit != "activation" {
		return RenewalResult{}, false, ErrInvalidRequest
	}
	order, found, err := c.lookupSMSPoolActiveWithRetry(ctx, apiKey, upstreamID, "renewal.reconcile.reactivate")
	if err != nil {
		return RenewalResult{}, false, err
	}
	if !found {
		return RenewalResult{}, false, nil
	}
	result, err := renewalResultFromSMSPoolOrder(order, upstreamID, "renewal.reconcile.reactivate")
	if err != nil {
		return RenewalResult{}, false, err
	}
	return result, true, nil
}

func (c *SMSPool) lookupSMSPoolActiveWithRetry(ctx context.Context, apiKey, upstreamID, operation string) (map[string]any, bool, error) {
	var order map[string]any
	var found bool
	var lookupErr error
	for attempt, delay := range []time.Duration{0, 300 * time.Millisecond, 700 * time.Millisecond, 1500 * time.Millisecond} {
		if attempt > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, false, c.http.failure(operation, "RESULT_NOT_CONFIRMED", 0, true, ctx.Err())
			case <-timer.C:
			}
		}
		order, found, lookupErr = c.smsPoolOrder(ctx, apiKey, operation, "request/active", nil, upstreamID)
		if lookupErr == nil && found && smsPoolOrderMatches(order, upstreamID) {
			return order, true, nil
		}
		if lookupErr != nil {
			var providerErr *ProviderError
			if !errors.As(lookupErr, &providerErr) || !providerErr.Retryable {
				return nil, false, lookupErr
			}
		}
	}
	if lookupErr != nil {
		return nil, false, lookupErr
	}
	return order, found, nil
}

func renewalResultFromSMSPoolOrder(order map[string]any, fallbackID, operation string) (RenewalResult, error) {
	price, ok := firstFloat(order, "cost", "price", "reactivation_cost", "reactivate_cost", "charged_cost", "amount")
	if !ok || price < 0 {
		return RenewalResult{}, &ProviderError{Provider: domain.ProviderSMSPool, Operation: operation, Code: "INVALID_RESPONSE"}
	}
	expiresAt := smsPoolOrderExpiry(order)
	if expiresAt == nil {
		return RenewalResult{}, &ProviderError{Provider: domain.ProviderSMSPool, Operation: operation, Code: "INVALID_RESPONSE"}
	}
	upstreamID := firstScalar(order, "order_code", "orderCode", "order_id", "orderid", "id")
	if upstreamID == "" {
		upstreamID = fallbackID
	}
	phone := firstScalar(order, "phonenumber", "phone_number", "phoneNumber", "phone")
	currency := firstScalar(order, "currency")
	if currency == "" {
		currency = "USD"
	}
	return RenewalResult{UpstreamID: upstreamID, PhoneNumber: phone, Cost: price, Currency: currency,
		CanGetAnotherSMS: true, ExpiresAt: expiresAt, Raw: rawJSON(order)}, nil
}

var _ RenewalReconcileClient = (*SMSPool)(nil)
