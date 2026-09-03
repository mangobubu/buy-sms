package provider

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"buysms/internal/domain"
)

const (
	defaultHeroSMSBaseURL   = "https://hero-sms.com/api/v1"
	defaultHeroSMSLegacyURL = "https://hero-sms.com/stubs/handler_api.php"
)

type HeroSMS struct {
	native bool
	http   *baseClient
	legacy *smsActivateClient
}

func NewHeroSMS(baseURL string, options ...Option) *HeroSMS {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultHeroSMSBaseURL
	}
	config := resolveOptions(options...)
	isLegacy := strings.Contains(strings.ToLower(baseURL), "handler_api")
	legacyURL := baseURL
	if !isLegacy {
		legacyURL = heroLegacyURL(baseURL)
	}
	return &HeroSMS{
		native: !isLegacy,
		http:   newBaseClient(domain.ProviderHeroSMS, baseURL, config),
		legacy: newSMSActivateClient(domain.ProviderHeroSMS, legacyURL, config),
	}
}

func heroLegacyURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return defaultHeroSMSLegacyURL
	}
	u.Path = "/stubs/handler_api.php"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (c *HeroSMS) ID() string { return domain.ProviderHeroSMS }

func (c *HeroSMS) Balance(ctx context.Context, apiKey string) (BalanceResult, error) {
	return c.legacy.Balance(ctx, apiKey)
}

func (c *HeroSMS) Catalog(ctx context.Context, apiKey string, request CatalogRequest) ([]domain.CatalogItem, error) {
	if strings.TrimSpace(request.QualityTier) != "" {
		return nil, ErrInvalidRequest
	}
	if !c.native {
		return c.legacy.Catalog(ctx, apiKey, request)
	}
	if err := require(apiKey); err != nil {
		return nil, err
	}
	kind, err := normalizeCatalogKind(request.Kind)
	if err != nil {
		return nil, err
	}
	// 国家和服务的静态字典继续使用官方兼容接口；实时价格走原生 offers。
	if kind != CatalogPrice {
		return c.legacy.Catalog(ctx, apiKey, request)
	}
	query := make(url.Values)
	if request.Service != "" {
		query.Set("services", request.Service)
	}
	if request.Country != "" {
		query.Set("countries", request.Country)
	}
	payload, err := c.http.get(ctx, "catalog.price", apiKey, "activations/offers/sms", query, true)
	if err != nil {
		return nil, err
	}
	items, parseErr := parsePriceCatalog(payload, domain.ProviderHeroSMS, request.Country, request.Service)
	if parseErr != nil {
		return nil, c.http.failure("catalog.price", "INVALID_RESPONSE", 0, false, nil)
	}
	return items, nil
}

func (c *HeroSMS) Purchase(ctx context.Context, apiKey string, request PurchaseRequest) (PurchaseResult, error) {
	if strings.TrimSpace(request.QualityTier) != "" {
		return PurchaseResult{}, ErrInvalidRequest
	}
	if request.MaxPrice != nil && request.FixedPrice == nil {
		fixedPrice := true
		request.FixedPrice = &fixedPrice
	}
	if !c.native {
		return c.legacy.Purchase(ctx, apiKey, request)
	}
	if err := require(apiKey, request.Country, request.Service); err != nil {
		return PurchaseResult{}, err
	}
	body := map[string]any{
		"amount":           1,
		"country":          jsonScalar(request.Country),
		"service":          request.Service,
		"verificationType": "sms",
	}
	if request.Duration != "" {
		body["duration"] = jsonScalar(request.Duration)
	}
	if request.FixedPrice != nil {
		body["fixedPrice"] = *request.FixedPrice
	}
	if request.MaxPrice != nil {
		body["maxPrice"] = *request.MaxPrice
	}
	if request.Operator != "" {
		body["operator"] = request.Operator
	}
	if request.ResellerUserID != "" {
		body["resellerUserId"] = request.ResellerUserID
	}
	for key, value := range request.Extra {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "", "amount", "country", "service", "verificationtype", "fixedprice", "maxprice":
			continue
		}
		body[key] = value
	}
	payload, err := c.http.json(ctx, "purchase", apiKey, http.MethodPost, "activations", body)
	if err != nil {
		return PurchaseResult{}, err
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

func (c *HeroSMS) Poll(ctx context.Context, apiKey, upstreamID string) (PollResult, error) {
	if !c.native {
		return c.legacy.Poll(ctx, apiKey, upstreamID)
	}
	if err := require(apiKey, upstreamID); err != nil {
		return PollResult{}, err
	}
	payload, err := c.http.get(ctx, "poll", apiKey, "activations/"+url.PathEscape(upstreamID)+"/otp", nil, true)
	if err != nil {
		return PollResult{}, err
	}
	messages, state, expiresAt, parseErr := parseOTPList(payload)
	if parseErr != nil {
		return PollResult{}, c.http.failure("poll", "INVALID_RESPONSE", 0, false, nil)
	}
	result := PollResult{
		State: state, Messages: messages, ExpiresAt: expiresAt,
		CanRequestAnother: false, Raw: cloneRaw(payload),
	}
	applyPollConvenience(&result)
	return result, nil
}

func (c *HeroSMS) Complete(ctx context.Context, apiKey, upstreamID string) error {
	if !c.native {
		return c.legacy.Complete(ctx, apiKey, upstreamID)
	}
	return c.nativeAction(ctx, apiKey, upstreamID, "finish", http.MethodPost, "complete")
}

func (c *HeroSMS) Cancel(ctx context.Context, apiKey, upstreamID string) error {
	if !c.native {
		return c.legacy.Cancel(ctx, apiKey, upstreamID)
	}
	return c.nativeAction(ctx, apiKey, upstreamID, "", http.MethodDelete, "cancel")
}

func (c *HeroSMS) RequestAnother(ctx context.Context, apiKey, upstreamID string) (RequestAnotherResult, error) {
	if !c.native {
		return c.legacy.RequestAnother(ctx, apiKey, upstreamID)
	}
	if err := require(apiKey, upstreamID); err != nil {
		return RequestAnotherResult{}, err
	}
	// 原生接口的 OTP 列表会持续累积，官方没有显式 resend 动作；这里保持
	// no-op，避免猜测接口并意外产生额外费用。
	return RequestAnotherResult{}, ctx.Err()
}

func (c *HeroSMS) nativeAction(ctx context.Context, apiKey, upstreamID, suffix, method, operation string) error {
	if err := require(apiKey, upstreamID); err != nil {
		return err
	}
	relative := "activations/" + url.PathEscape(upstreamID)
	if suffix != "" {
		relative += "/" + suffix
	}
	endpoint, err := c.http.endpoint(relative)
	if err != nil {
		return c.http.failure(operation, "INVALID_BASE_URL", 0, false, nil)
	}
	headers := make(http.Header)
	headers.Set("Authorization", "ApiKey "+apiKey)
	_, _, err = c.http.do(ctx, operation, method, endpoint, headers, nil, apiKey)
	return err
}

func jsonScalar(value string) any {
	if number, err := strconv.ParseInt(value, 10, 64); err == nil {
		return number
	}
	if number, err := strconv.ParseFloat(value, 64); err == nil {
		return number
	}
	return value
}

var _ Client = (*HeroSMS)(nil)
