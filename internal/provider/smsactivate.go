package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"buysms/internal/domain"
)

type smsActivateClient struct {
	providerID string
	http       *baseClient
}

func newSMSActivateClient(providerID, baseURL string, config clientConfig) *smsActivateClient {
	return &smsActivateClient{providerID: providerID, http: newBaseClient(providerID, baseURL, config)}
}

func (c *smsActivateClient) ID() string { return c.providerID }

func (c *smsActivateClient) Catalog(ctx context.Context, apiKey string, request CatalogRequest) ([]domain.CatalogItem, error) {
	if err := require(apiKey); err != nil {
		return nil, err
	}
	kind, err := normalizeCatalogKind(request.Kind)
	if err != nil {
		return nil, err
	}
	query := make(url.Values)
	switch kind {
	case CatalogCountry:
		query.Set("action", "getCountries")
	case CatalogService:
		query.Set("action", "getServicesList")
		if request.Country != "" {
			query.Set("country", request.Country)
		}
	case CatalogPrice:
		query.Set("action", "getPrices")
		if request.Country != "" {
			query.Set("country", request.Country)
		}
		if request.Service != "" {
			query.Set("service", request.Service)
		}
	}
	payload, err := c.http.get(ctx, "catalog."+kind, apiKey, "", query, false)
	if err != nil {
		return nil, err
	}
	if businessErr := c.businessError("catalog."+kind, apiKey, payload); businessErr != nil {
		return nil, businessErr
	}
	if kind == CatalogPrice {
		items, parseErr := parsePriceCatalog(payload, c.providerID, request.Country, request.Service)
		if parseErr != nil {
			return nil, c.http.failure("catalog."+kind, "INVALID_RESPONSE", 0, false, nil)
		}
		return items, nil
	}
	items, parseErr := parseSimpleCatalog(payload, c.providerID, kind, request.Country)
	if parseErr != nil {
		return nil, c.http.failure("catalog."+kind, "INVALID_RESPONSE", 0, false, nil)
	}
	return items, nil
}

func (c *smsActivateClient) Purchase(ctx context.Context, apiKey string, request PurchaseRequest) (PurchaseResult, error) {
	if err := require(apiKey, request.Country, request.Service); err != nil {
		return PurchaseResult{}, err
	}
	query := c.purchaseQuery(request)
	query.Set("action", "getNumberV2")
	payload, err := c.http.get(ctx, "purchase", apiKey, "", query, false)
	if err != nil {
		return PurchaseResult{}, err
	}
	if token := legacyToken(payload); token == "BAD_ACTION" {
		// getNumberV2 未执行购买时可以安全降级到基础接口。
		query.Set("action", "getNumber")
		payload, err = c.http.get(ctx, "purchase", apiKey, "", query, false)
		if err != nil {
			return PurchaseResult{}, err
		}
	}
	if businessErr := c.businessError("purchase", apiKey, payload); businessErr != nil {
		return PurchaseResult{}, businessErr
	}
	result, parseErr := parseActivationPurchase(payload)
	if parseErr != nil {
		return PurchaseResult{}, c.http.failure("purchase", "INVALID_RESPONSE", 0, false, nil)
	}
	return result, nil
}

func (c *smsActivateClient) purchaseQuery(request PurchaseRequest) url.Values {
	query := make(url.Values)
	query.Set("country", request.Country)
	query.Set("service", request.Service)
	if request.MaxPrice != nil {
		query.Set("maxPrice", strconv.FormatFloat(*request.MaxPrice, 'f', -1, 64))
	}
	if request.FixedPrice != nil {
		query.Set("fixedPrice", strconv.FormatBool(*request.FixedPrice))
	}
	if request.Operator != "" {
		query.Set("operator", request.Operator)
	}
	if request.ResellerUserID != "" {
		query.Set("userID", request.ResellerUserID)
	}
	for key, value := range request.Extra {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "", "api_key", "key", "action", "country", "service", "id", "status":
			continue
		}
		query.Set(key, value)
	}
	return query
}

func (c *smsActivateClient) Poll(ctx context.Context, apiKey, upstreamID string) (PollResult, error) {
	if err := require(apiKey, upstreamID); err != nil {
		return PollResult{}, err
	}
	query := url.Values{"action": {"getAllSms"}, "id": {upstreamID}, "size": {"100"}, "page": {"0"}}
	payload, err := c.http.get(ctx, "poll", apiKey, "", query, false)
	if err != nil {
		return PollResult{}, err
	}
	token := legacyToken(payload)
	if token == "BAD_ACTION" || token == "NO_ACTIVATION" {
		// SMSBower 的部分兼容节点尚未开放 getAllSms；NO_ACTIVATION 也可能是
		// 旧节点对未知 action 的兼容返回，此处再用标准 getStatus 确认。
		query.Set("action", "getStatus")
		query.Del("size")
		query.Del("page")
		payload, err = c.http.get(ctx, "poll", apiKey, "", query, false)
		if err != nil {
			return PollResult{}, err
		}
	}
	if businessErr := c.businessError("poll", apiKey, payload); businessErr != nil {
		return PollResult{}, businessErr
	}
	return c.parsePoll(payload)
}

func (c *smsActivateClient) parsePoll(payload []byte) (PollResult, error) {
	text := strings.TrimSpace(string(payload))
	result := PollResult{Raw: rawObject(payload)}
	parts := strings.SplitN(text, ":", 2)
	switch strings.ToUpper(parts[0]) {
	case "STATUS_WAIT_CODE":
		result.State = PollWaiting
		return result, nil
	case "STATUS_WAIT_RETRY":
		result.State = PollWaitingRetry
		result.CanRequestAnother = false
		if len(parts) == 2 {
			result.LastCode = strings.TrimSpace(parts[1])
		}
		return result, nil
	case "STATUS_WAIT_RESEND":
		result.State = PollWaitingRetry
		return result, nil
	case "STATUS_CANCEL":
		result.State = PollCanceled
		return result, nil
	case "STATUS_OK":
		result.State = PollReceived
		result.CanRequestAnother = true
		if len(parts) == 2 {
			code := strings.TrimSpace(parts[1])
			result.Messages = []OTPMessage{{Code: code}}
			applyPollConvenience(&result)
		}
		return result, nil
	}

	messages, state, expiresAt, err := parseOTPList(payload)
	if err != nil {
		return PollResult{}, c.http.failure("poll", "INVALID_RESPONSE", 0, false, nil)
	}
	result.State = state
	result.Messages = messages
	result.ExpiresAt = expiresAt
	result.CanRequestAnother = len(messages) > 0
	applyPollConvenience(&result)
	return result, nil
}

func (c *smsActivateClient) Complete(ctx context.Context, apiKey, upstreamID string) error {
	return c.setStatus(ctx, apiKey, upstreamID, "6", "complete")
}

func (c *smsActivateClient) Cancel(ctx context.Context, apiKey, upstreamID string) error {
	return c.setStatus(ctx, apiKey, upstreamID, "8", "cancel")
}

func (c *smsActivateClient) RequestAnother(ctx context.Context, apiKey, upstreamID string) (RequestAnotherResult, error) {
	err := c.setStatus(ctx, apiKey, upstreamID, "3", "request_another")
	return RequestAnotherResult{}, err
}

func (c *smsActivateClient) setStatus(ctx context.Context, apiKey, upstreamID, status, operation string) error {
	if err := require(apiKey, upstreamID); err != nil {
		return err
	}
	query := url.Values{"action": {"setStatus"}, "id": {upstreamID}, "status": {status}}
	payload, err := c.http.get(ctx, operation, apiKey, "", query, false)
	if err != nil {
		return err
	}
	if businessErr := c.businessError(operation, apiKey, payload); businessErr != nil {
		return businessErr
	}
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(string(payload))), "ACCESS_") {
		return c.http.failure(operation, "INVALID_RESPONSE", 0, false, nil)
	}
	return nil
}

func (c *smsActivateClient) businessError(operation, apiKey string, payload []byte) error {
	token := legacyToken(payload)
	if sanitizeCodeWithoutSecrets(token, apiKey) == "UPSTREAM_ERROR" {
		return c.http.failure(operation, "UPSTREAM_ERROR", 0, false, nil)
	}
	if token == "" {
		return nil
	}
	switch token {
	case "BAD_KEY", "BAD_ACTION", "BAD_SERVICE", "BAD_COUNTRY", "BAD_STATUS",
		"NO_NUMBERS", "NO_BALANCE", "NO_ACTIVATION", "EARLY_CANCEL_DENIED",
		"WRONG_MAX_PRICE", "MAX_PRICE_EXCEEDED", "ACCOUNT_INACTIVE", "BANNED",
		"ERROR_SQL", "NO_CONNECTION", "RATE_LIMIT":
		return c.http.failure(operation, token, 0, token == "RATE_LIMIT" || token == "NO_CONNECTION", nil)
	}
	return nil
}

func legacyToken(payload []byte) string {
	text := strings.TrimSpace(string(payload))
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		var object map[string]any
		if json.Unmarshal(payload, &object) == nil {
			status := strings.ToLower(firstScalar(object, "status"))
			if status == "error" || status == "failed" || status == "fail" {
				return sanitizeCode(firstScalar(object, "code", "type", "error"))
			}
		}
		return ""
	}
	if index := strings.IndexByte(text, ':'); index >= 0 {
		text = text[:index]
	}
	return sanitizeCode(text)
}

func parseActivationPurchase(payload []byte) (PurchaseResult, error) {
	text := strings.TrimSpace(string(payload))
	if strings.HasPrefix(strings.ToUpper(text), "ACCESS_NUMBER:") {
		parts := strings.SplitN(text, ":", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
			return PurchaseResult{}, errors.New("购买响应格式无效")
		}
		return PurchaseResult{
			UpstreamID: strings.TrimSpace(parts[1]), PhoneNumber: strings.TrimSpace(parts[2]),
			Currency: "USD", CanGetAnotherSMS: true, Raw: rawObject(payload),
		}, nil
	}
	value, err := decodeAny(payload)
	if err != nil {
		return PurchaseResult{}, err
	}
	return purchaseResultFromValue(value, cloneRaw(payload))
}

func purchaseResultFromValue(value any, raw json.RawMessage) (PurchaseResult, error) {
	if object, ok := value.(map[string]any); ok {
		if nested, found := lookup(object, "data", "activation", "result"); found {
			value = nested
		}
	}
	if list, ok := value.([]any); ok {
		if len(list) == 0 {
			return PurchaseResult{}, errors.New("购买响应为空")
		}
		value = list[0]
	}
	object, ok := value.(map[string]any)
	if !ok {
		return PurchaseResult{}, errors.New("购买响应格式无效")
	}
	result := PurchaseResult{
		UpstreamID:  firstScalar(object, "activationId", "activation_id", "order_id", "orderId", "id"),
		PhoneNumber: firstScalar(object, "number", "phoneNumber", "phone_number", "phonenumber", "phone"),
		CountryCode: firstScalar(object, "countryCode", "countryPhoneCode", "country_code", "cc"),
		Currency:    firstScalar(object, "currency"),
		Raw:         raw,
	}
	if result.Currency == "" {
		result.Currency = "USD"
	}
	if value, found := lookup(object, "activationCost", "activation_cost", "cost", "price"); found {
		result.Cost, _ = floatValue(value)
	}
	if value, found := lookup(object, "canGetAnotherSms", "can_get_another_sms", "resend"); found {
		result.CanGetAnotherSMS, _ = boolValue(value)
	}
	if value, found := lookup(object, "expiresAt", "expiredAt", "expires_at", "expiration", "expiry"); found {
		result.ExpiresAt = parseTimeValue(value)
	}
	if result.ExpiresAt == nil {
		if value, found := lookup(object, "expiresIn", "expires_in"); found {
			if seconds, ok := intValue(value); ok && seconds > 0 {
				expiresAt := time.Now().UTC().Add(time.Duration(seconds) * time.Second)
				result.ExpiresAt = &expiresAt
			}
		}
	}
	if result.UpstreamID == "" || result.PhoneNumber == "" {
		return PurchaseResult{}, fmt.Errorf("购买响应缺少号码或订单 ID")
	}
	return result, nil
}
