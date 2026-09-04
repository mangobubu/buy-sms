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
	nativeURL := baseURL
	if !isLegacy {
		legacyURL = heroLegacyURL(baseURL)
	} else {
		nativeURL = heroNativeURL(baseURL)
	}
	return &HeroSMS{
		native: !isLegacy,
		http:   newBaseClient(domain.ProviderHeroSMS, nativeURL, config),
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

func heroNativeURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return defaultHeroSMSBaseURL
	}
	u.Path = "/api/v1"
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

func (c *HeroSMS) Durations(ctx context.Context, apiKey string, request CatalogRequest) ([]DurationOption, error) {
	if err := require(apiKey, request.Country, request.Service); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.QualityTier) != "" {
		return nil, ErrInvalidRequest
	}

	payload, defaultErr := c.http.publicGet(ctx, "catalog.duration.default", "classifiers/activations/custom-durations", nil)
	minutes, parseErr := parseHeroSMSDefaultMinutes(payload, request.Service, request.Country)
	if defaultErr == nil && parseErr != nil {
		defaultErr = c.http.failure("catalog.duration.default", "INVALID_RESPONSE", 0, false, nil)
	}
	rentals, rentalErr := c.RentalDurations(ctx, apiKey, request)
	if defaultErr != nil && rentalErr != nil {
		return nil, rentalErr
	}
	options := make([]DurationOption, 0, len(rentals)+1)
	if defaultErr == nil {
		options = append(options, DurationOption{Minutes: minutes})
	}
	rentalStart := len(options)
	if rentalErr != nil {
		return options, nil
	}
	options = append(options, rentals...)
	sort.Slice(options[rentalStart:], func(i, j int) bool {
		return options[rentalStart+i].Hours < options[rentalStart+j].Hours
	})
	return options, nil
}

func (c *HeroSMS) RentalDurations(ctx context.Context, apiKey string, request CatalogRequest) ([]DurationOption, error) {
	if strings.TrimSpace(request.QualityTier) != "" {
		return nil, ErrInvalidRequest
	}
	return c.legacy.RentDurations(ctx, apiKey, request.Country, request.Service)
}

func parseHeroSMSDefaultMinutes(payload []byte, service, country string) (int, error) {
	value, err := decodeAny(payload)
	if err != nil {
		return 0, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("时长响应格式无效")
	}
	if nested, found := lookup(root, "data"); found {
		var nestedOK bool
		root, nestedOK = nested.(map[string]any)
		if !nestedOK {
			return 0, fmt.Errorf("时长响应格式无效")
		}
	}
	serviceValue, found := lookup(root, service)
	if !found {
		return 20, nil
	}
	countries, ok := serviceValue.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("时长响应格式无效")
	}
	rawMinutes, found := lookup(countries, country)
	if !found {
		return 20, nil
	}
	minutes, ok := intValue(rawMinutes)
	if !ok || minutes <= 0 {
		return 0, fmt.Errorf("时长响应格式无效")
	}
	return minutes, nil
}

func (c *HeroSMS) RenewalOptions(ctx context.Context, apiKey, upstreamID, mode string) ([]RenewalOption, error) {
	if err := require(apiKey, upstreamID); err != nil {
		return nil, err
	}
	if !validRenewalMode(mode) {
		return nil, ErrInvalidRequest
	}
	operation := "renewal.options." + mode
	payload, err := c.http.get(
		ctx,
		operation,
		apiKey,
		"activations/"+url.PathEscape(upstreamID)+"/"+mode+"/options",
		nil,
		true,
	)
	if err != nil {
		return nil, err
	}
	options, parseErr := parseHeroSMSRenewalOptions(payload)
	if parseErr != nil {
		return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
	}
	if mode == RenewalProlong {
		for _, option := range options {
			if option.Unit != "hour" {
				return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
			}
		}
		history, historyErr := c.prolongHistory(ctx, apiKey, upstreamID, operation+".history")
		if historyErr != nil {
			return nil, historyErr
		}
		baseline, baselineErr := encodeHeroProlongBaseline(history)
		if baselineErr != nil {
			return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, baselineErr)
		}
		for index := range options {
			options[index].Baseline = baseline
		}
	}
	return options, nil
}
func (c *HeroSMS) Renew(ctx context.Context, apiKey, upstreamID string, request RenewalRequest) (RenewalResult, error) {
	if err := require(apiKey, upstreamID); err != nil {
		return RenewalResult{}, err
	}
	if err := validateRenewalRequest(request); err != nil {
		return RenewalResult{}, err
	}
	if request.Mode == RenewalProlong {
		if _, err := decodeHeroProlongBaseline(request.Baseline); err != nil {
			return RenewalResult{}, ErrInvalidRequest
		}
	}

	submittedAt := request.SubmittedAt.UTC()
	if submittedAt.IsZero() {
		submittedAt = time.Now().UTC()
	}

	body := map[string]any{}
	if request.Mode == RenewalProlong || request.Unit == "hour" {
		body["duration"] = request.Value
	}
	operation := "renewal." + request.Mode
	payload, err := c.http.json(
		ctx,
		operation,
		apiKey,
		http.MethodPost,
		"activations/"+url.PathEscape(upstreamID)+"/"+request.Mode,
		body,
	)
	if err != nil {
		return RenewalResult{}, err
	}
	value, parseErr := decodeAny(payload)
	if parseErr != nil {
		return RenewalResult{}, c.http.failure(operation, "RESULT_NOT_CONFIRMED", 0, true, parseErr)
	}
	activation, parseErr := purchaseResultFromValue(value, cloneRaw(payload))
	if parseErr != nil || activation.ExpiresAt == nil {
		return RenewalResult{}, c.http.failure(operation, "RESULT_NOT_CONFIRMED", 0, true, parseErr)
	}
	result := renewalResultFromPurchase(activation)
	if request.Mode == RenewalProlong {
		result.Cost, err = c.newProlongCharge(ctx, apiKey, upstreamID, request.Value, submittedAt, request.Baseline)
		if err != nil {
			return RenewalResult{}, err
		}
	}
	return result, nil
}

type heroProlongHistoryEntry struct {
	Duration  int
	Price     float64
	CreatedAt time.Time
}

func (c *HeroSMS) prolongHistory(ctx context.Context, apiKey, upstreamID, operation string) ([]heroProlongHistoryEntry, error) {
	payload, err := c.http.get(ctx, operation, apiKey,
		"activations/"+url.PathEscape(upstreamID)+"/prolong/history", nil, true)
	if err != nil {
		return nil, err
	}
	value, err := decodeAny(payload)
	if err != nil {
		return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
	}
	if root, ok := value.(map[string]any); ok {
		if data, found := lookup(root, "data"); found {
			value = data
		}
	}
	items, ok := value.([]any)
	if !ok {
		return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
	}
	entries := make([]heroProlongHistoryEntry, 0, len(items))
	for _, item := range items {
		object, itemOK := item.(map[string]any)
		if !itemOK {
			return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
		}
		duration, durationOK := firstInt(object, "duration", "hours")
		price, priceOK := firstFloat(object, "price", "userPrice", "user_price")
		createdValue, createdOK := lookup(object, "createdAt", "created_at", "createDate", "create_date")
		createdAt := parseTimeValue(createdValue)
		if !durationOK || duration <= 0 || !priceOK || price < 0 || math.IsNaN(price) || math.IsInf(price, 0) ||
			!createdOK || createdAt == nil {
			return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
		}
		entries = append(entries, heroProlongHistoryEntry{
			Duration: duration, Price: price, CreatedAt: *createdAt,
		})
	}
	return entries, nil
}

const heroRenewalClockSkew = 5 * time.Second

func (c *HeroSMS) newProlongCharge(ctx context.Context, apiKey, upstreamID string, duration int, submittedAt time.Time, baseline string) (float64, error) {
	operation := "renewal.result.prolong"
	for attempt, delay := range []time.Duration{0, 300 * time.Millisecond, 700 * time.Millisecond, 1500 * time.Millisecond} {
		if attempt > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return 0, c.http.failure(operation, "RESULT_NOT_CONFIRMED", 0, true, ctx.Err())
			case <-timer.C:
			}
		}
		history, err := c.prolongHistory(ctx, apiKey, upstreamID, operation)
		if err != nil {
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || !providerErr.Retryable {
				return 0, c.http.failure(operation, "RESULT_NOT_CONFIRMED", 0, true, err)
			}
			continue
		}
		if entry, found := matchingHeroProlong(history, duration, submittedAt, baseline); found {
			return entry.Price, nil
		}
	}
	return 0, c.http.failure(operation, "RESULT_NOT_CONFIRMED", 0, true, nil)
}
func validRenewalMode(mode string) bool {
	return mode == RenewalProlong || mode == RenewalReactivate
}

func validateRenewalRequest(request RenewalRequest) error {
	if !validRenewalMode(request.Mode) || request.Value <= 0 {
		return ErrInvalidRequest
	}
	if request.Mode == RenewalProlong {
		if request.Unit != "hour" {
			return ErrInvalidRequest
		}
		return nil
	}
	if request.Unit != "minute" && request.Unit != "hour" {
		return ErrInvalidRequest
	}
	return nil
}

func renewalResultFromPurchase(result PurchaseResult) RenewalResult {
	return RenewalResult{
		UpstreamID:       result.UpstreamID,
		PhoneNumber:      result.PhoneNumber,
		Cost:             result.Cost,
		Currency:         result.Currency,
		CanGetAnotherSMS: true,
		ExpiresAt:        result.ExpiresAt,
		Raw:              result.Raw,
	}
}

func parseHeroSMSRenewalOptions(payload []byte) ([]RenewalOption, error) {
	value, err := decodeAny(payload)
	if err != nil {
		return nil, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("续期选项响应格式无效")
	}
	dataValue, found := root["data"]
	if !found {
		return nil, fmt.Errorf("续期选项响应缺少 data")
	}
	data, ok := dataValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("续期选项响应 data 格式无效")
	}
	optionsValue, found := data["options"]
	if !found {
		return nil, fmt.Errorf("续期选项响应缺少 options")
	}
	items, ok := optionsValue.([]any)
	if !ok {
		return nil, fmt.Errorf("续期选项响应 options 格式无效")
	}
	options := make([]RenewalOption, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("续期选项格式无效")
		}
		durationValue, found := object["duration"]
		if !found {
			return nil, fmt.Errorf("续期选项缺少 duration")
		}
		duration, ok := durationValue.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("续期选项 duration 格式无效")
		}
		optionValue, ok := positiveJSONInt(duration["value"])
		if !ok {
			return nil, fmt.Errorf("续期选项 duration.value 格式无效")
		}
		unit, ok := duration["unit"].(string)
		if !ok || (unit != "minute" && unit != "hour") {
			return nil, fmt.Errorf("续期选项 duration.unit 格式无效")
		}
		price, ok := nonNegativeJSONNumber(object["price"])
		if !ok {
			return nil, fmt.Errorf("续期选项 price 格式无效")
		}
		options = append(options, RenewalOption{Value: optionValue, Unit: unit, Price: price})
	}
	return options, nil
}

func positiveJSONInt(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	if err != nil || parsed <= 0 {
		return 0, false
	}
	converted := int(parsed)
	if int64(converted) != parsed {
		return 0, false
	}
	return converted, true
}

func nonNegativeJSONNumber(value any) (float64, bool) {
	parsed, ok := floatValue(value)
	if !ok || parsed < 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

func (c *HeroSMS) Purchase(ctx context.Context, apiKey string, request PurchaseRequest) (PurchaseResult, error) {
	if strings.TrimSpace(request.QualityTier) != "" {
		return PurchaseResult{}, ErrInvalidRequest
	}
	if request.MaxPrice != nil && request.FixedPrice == nil {
		fixedPrice := true
		request.FixedPrice = &fixedPrice
	}
	duration, err := normalizeRentalDuration(request.Duration)
	if err != nil {
		return PurchaseResult{}, err
	}
	request.Duration = duration
	// 长租即使从兼容地址初始化，也统一走同域原生接口。兼容
	// getRentNumber 不接受 fixedPrice/maxPrice，目录查询与下单之间涨价时
	// 无法保证用户选择的价格上限；原生接口可以原子锁定价格。
	if !c.native && request.Duration == "" {
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
	return c.PollDuration(ctx, apiKey, upstreamID)
}

func (c *HeroSMS) PollDuration(ctx context.Context, apiKey, upstreamID string) (PollResult, error) {
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
	return c.CompleteDuration(ctx, apiKey, upstreamID)
}

func (c *HeroSMS) CompleteDuration(ctx context.Context, apiKey, upstreamID string) error {
	return c.nativeAction(ctx, apiKey, upstreamID, "finish", http.MethodPost, "complete")
}

func (c *HeroSMS) Cancel(ctx context.Context, apiKey, upstreamID string) error {
	if !c.native {
		return c.legacy.Cancel(ctx, apiKey, upstreamID)
	}
	return c.CancelDuration(ctx, apiKey, upstreamID)
}

func (c *HeroSMS) CancelDuration(ctx context.Context, apiKey, upstreamID string) error {
	return c.nativeAction(ctx, apiKey, upstreamID, "", http.MethodDelete, "cancel")
}

func (c *HeroSMS) RequestAnother(ctx context.Context, apiKey, upstreamID string) (RequestAnotherResult, error) {
	if !c.native {
		return c.legacy.RequestAnother(ctx, apiKey, upstreamID)
	}
	return c.RequestAnotherDuration(ctx, apiKey, upstreamID)
}

func (c *HeroSMS) RequestAnotherDuration(ctx context.Context, apiKey, upstreamID string) (RequestAnotherResult, error) {
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
var _ DurationCatalogClient = (*HeroSMS)(nil)
var _ RentalDurationCatalogClient = (*HeroSMS)(nil)
var _ DurationLifecycleClient = (*HeroSMS)(nil)
var _ RenewalClient = (*HeroSMS)(nil)

func heroProlongFingerprint(entry heroProlongHistoryEntry) string {
	return fingerprint(
		strconv.Itoa(entry.Duration),
		strconv.FormatFloat(entry.Price, 'g', -1, 64),
		entry.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
}

func encodeHeroProlongBaseline(history []heroProlongHistoryEntry) (string, error) {
	counts := make(map[string]int, len(history))
	for _, entry := range history {
		counts[heroProlongFingerprint(entry)]++
	}
	payload, err := json.Marshal(counts)
	return string(payload), err
}

func decodeHeroProlongBaseline(raw string) (map[string]int, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("续期历史基线为空")
	}
	var counts map[string]int
	if err := json.Unmarshal([]byte(raw), &counts); err != nil || counts == nil {
		return nil, fmt.Errorf("续期历史基线格式无效")
	}
	for key, count := range counts {
		if strings.TrimSpace(key) == "" || count <= 0 {
			return nil, fmt.Errorf("续期历史基线内容无效")
		}
	}
	return counts, nil
}

func matchingHeroProlong(history []heroProlongHistoryEntry, duration int, submittedAt time.Time, baseline string) (heroProlongHistoryEntry, bool) {
	known, err := decodeHeroProlongBaseline(baseline)
	if err != nil {
		return heroProlongHistoryEntry{}, false
	}
	seen := make(map[string]int, len(history))
	submittedAt = submittedAt.UTC()
	notBefore := submittedAt.Add(-heroRenewalClockSkew)
	var after, skewed heroProlongHistoryEntry
	var hasAfter, hasSkewed bool
	for _, entry := range history {
		fingerprint := heroProlongFingerprint(entry)
		seen[fingerprint]++
		if seen[fingerprint] <= known[fingerprint] {
			continue
		}
		createdAt := entry.CreatedAt.UTC()
		if entry.Duration != duration || createdAt.Before(notBefore) {
			continue
		}
		if !createdAt.Before(submittedAt) {
			if !hasAfter || createdAt.Before(after.CreatedAt) {
				after, hasAfter = entry, true
			}
			continue
		}
		if !hasSkewed || createdAt.After(skewed.CreatedAt) {
			skewed, hasSkewed = entry, true
		}
	}
	if hasAfter {
		return after, true
	}
	return skewed, hasSkewed
}

// ReconcileRenewal only reads provider state. It must never repeat the upstream
// renewal POST because the first request may already have charged the account.
func (c *HeroSMS) ReconcileRenewal(ctx context.Context, apiKey, upstreamID string, request RenewalRequest) (RenewalResult, bool, error) {
	if err := require(apiKey, upstreamID); err != nil {
		return RenewalResult{}, false, err
	}
	if err := validateRenewalRequest(request); err != nil || request.SubmittedAt.IsZero() {
		return RenewalResult{}, false, ErrInvalidRequest
	}
	if request.Mode == RenewalProlong {
		if _, err := decodeHeroProlongBaseline(request.Baseline); err != nil {
			return RenewalResult{}, false, ErrInvalidRequest
		}
	}

	operation := "renewal.reconcile." + request.Mode
	if request.Mode == RenewalProlong {
		history, err := c.prolongHistory(ctx, apiKey, upstreamID, operation)
		if err != nil {
			return RenewalResult{}, false, err
		}
		entry, found := matchingHeroProlong(history, request.Value, request.SubmittedAt, request.Baseline)
		if !found {
			return RenewalResult{}, false, nil
		}
		activation, err := c.findRenewedHeroActivation(ctx, apiKey, upstreamID, request, operation)
		if err != nil {
			return RenewalResult{}, false, err
		}
		result := renewalResultFromHeroActivation(activation)
		result.Cost = entry.Price
		return result, true, nil
	}

	if err := require(apiKey, upstreamID, request.PhoneNumber, request.Country, request.Service); err != nil {
		return RenewalResult{}, false, err
	}
	activation, err := c.findRenewedHeroActivation(ctx, apiKey, upstreamID, request, operation)
	if err != nil {
		return RenewalResult{}, false, err
	}
	if !activation.HasCost {
		return RenewalResult{}, false, nil
	}
	return renewalResultFromHeroActivation(activation), true, nil
}

type heroActiveActivation struct {
	ID, PhoneNumber, Country, Service string
	Cost                              float64
	HasCost                           bool
	Currency                          string
	CanGetAnotherSMS                  bool
	CreatedAt, ExpiresAt              *time.Time
	Raw                               json.RawMessage
}

func (c *HeroSMS) findRenewedHeroActivation(ctx context.Context, apiKey, upstreamID string, request RenewalRequest, operation string) (heroActiveActivation, error) {
	query := url.Values{"page": {"1"}, "size": {"100"}, "sort": {"createdAt:desc"}}
	if phone := strings.TrimSpace(request.PhoneNumber); phone != "" {
		query.Set("search", phone)
	}
	payload, err := c.http.get(ctx, operation, apiKey, "activations", query, true)
	if err != nil {
		return heroActiveActivation{}, err
	}
	activations, err := c.parseHeroActiveActivations(payload, operation)
	if err != nil {
		return heroActiveActivation{}, err
	}

	var selected heroActiveActivation
	found := false
	for _, activation := range activations {
		if activation.ExpiresAt == nil {
			continue
		}
		if request.Mode == RenewalProlong {
			if activation.ID == upstreamID {
				return activation, nil
			}
			continue
		}
		if activation.ID == upstreamID || !heroActivationMatchesSnapshot(activation, request) || activation.CreatedAt == nil ||
			activation.CreatedAt.Before(request.SubmittedAt.UTC().Add(-heroRenewalClockSkew)) {
			continue
		}
		if !found || betterHeroRenewalCandidate(activation, selected, request.SubmittedAt) {
			selected, found = activation, true
		}
	}
	if !found {
		return heroActiveActivation{}, c.http.failure(operation, "RESULT_NOT_CONFIRMED", 0, true, nil)
	}
	return selected, nil
}

func (c *HeroSMS) parseHeroActiveActivations(payload []byte, operation string) ([]heroActiveActivation, error) {
	value, err := decodeAny(payload)
	if err != nil {
		return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, err)
	}
	if root, ok := value.(map[string]any); ok {
		data, found := lookup(root, "data", "activations")
		if !found {
			return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
		}
		value = data
	}
	items, ok := value.([]any)
	if !ok {
		return nil, c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
	}
	result := make([]heroActiveActivation, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		activation := heroActiveActivation{
			ID:          firstScalar(object, "id", "activationId", "activation_id"),
			PhoneNumber: firstScalar(object, "phone", "phoneNumber", "phone_number"),
			Country:     firstScalar(object, "country", "countryCode", "country_code"),
			Service:     firstScalar(object, "service", "serviceCode", "service_code"),
			Currency:    firstScalar(object, "currency"), CanGetAnotherSMS: true, Raw: rawJSON(object),
		}
		if activation.ID == "" {
			continue
		}
		activation.Cost, activation.HasCost = firstFloat(object, "price", "activationCost", "activation_cost", "cost")
		if raw, found := lookup(object, "canGetAnotherSms", "can_get_another_sms"); found {
			if allowed, valid := boolValue(raw); valid {
				activation.CanGetAnotherSMS = allowed
			}
		}
		if raw, found := lookup(object, "createdAt", "created_at", "activationTime", "activation_time"); found {
			activation.CreatedAt = parseTimeValue(raw)
		}
		if raw, found := lookup(object, "expiredAt", "expiresAt", "expires_at", "expiration", "expiry"); found {
			activation.ExpiresAt = parseTimeValue(raw)
		}
		if activation.Currency == "" || activation.Currency == "840" {
			activation.Currency = "USD"
		}
		result = append(result, activation)
	}
	return result, nil
}

func heroActivationMatchesSnapshot(activation heroActiveActivation, request RenewalRequest) bool {
	return normalizedPhone(activation.PhoneNumber) == normalizedPhone(request.PhoneNumber) &&
		strings.EqualFold(strings.TrimSpace(activation.Country), strings.TrimSpace(request.Country)) &&
		strings.EqualFold(strings.TrimSpace(activation.Service), strings.TrimSpace(request.Service))
}

func normalizedPhone(value string) string {
	var digits strings.Builder
	for _, char := range value {
		if char >= '0' && char <= '9' {
			digits.WriteRune(char)
		}
	}
	return digits.String()
}

func betterHeroRenewalCandidate(candidate, current heroActiveActivation, submittedAt time.Time) bool {
	candidateAfter := !candidate.CreatedAt.Before(submittedAt.UTC())
	currentAfter := !current.CreatedAt.Before(submittedAt.UTC())
	if candidateAfter != currentAfter {
		return candidateAfter
	}
	if candidateAfter {
		return candidate.CreatedAt.Before(*current.CreatedAt)
	}
	return candidate.CreatedAt.After(*current.CreatedAt)
}

func renewalResultFromHeroActivation(activation heroActiveActivation) RenewalResult {
	return RenewalResult{UpstreamID: activation.ID, PhoneNumber: activation.PhoneNumber, Cost: activation.Cost,
		Currency: activation.Currency, CanGetAnotherSMS: activation.CanGetAnotherSMS, ExpiresAt: activation.ExpiresAt, Raw: activation.Raw}
}

var _ RenewalReconcileClient = (*HeroSMS)(nil)
