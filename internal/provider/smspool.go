package provider

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"buysms/internal/domain"
)

const defaultSMSPoolBaseURL = "https://api.smspool.net"

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

func (c *SMSPool) Catalog(ctx context.Context, apiKey string, request CatalogRequest) ([]domain.CatalogItem, error) {
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

func (c *SMSPool) Purchase(ctx context.Context, apiKey string, request PurchaseRequest) (PurchaseResult, error) {
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
	code := firstScalar(object, "sms", "code")
	text := firstScalar(object, "full_sms", "fullSms", "text", "message_text")
	if code != "" || text != "" {
		result.State = PollReceived
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
	code = sanitizeCodeWithoutSecrets(code, apiKey)
	if code == "" {
		code = "UPSTREAM_REJECTED"
	}
	return c.http.failure(operation, code, 0, strings.EqualFold(code, "OUT_OF_STOCK"), nil)
}

var _ Client = (*SMSPool)(nil)
