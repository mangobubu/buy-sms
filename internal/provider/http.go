package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 16 << 20

type baseClient struct {
	providerID string
	baseURL    *url.URL
	httpClient *http.Client
	timeout    time.Duration
}

func newBaseClient(providerID, rawBaseURL string, config clientConfig) *baseClient {
	u, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		u = &url.URL{}
	}
	return &baseClient{providerID: providerID, baseURL: u, httpClient: config.httpClient, timeout: config.timeout}
}

func (c *baseClient) endpoint(relative string) (*url.URL, error) {
	if c.baseURL == nil || c.baseURL.Scheme == "" || c.baseURL.Host == "" {
		return nil, ErrInvalidRequest
	}
	u := *c.baseURL
	if relative != "" {
		u.Path = path.Join(strings.TrimSuffix(u.Path, "/"), relative)
		u.RawQuery = ""
		u.Fragment = ""
	}
	return &u, nil
}

func (c *baseClient) do(ctx context.Context, operation, method string, endpoint *url.URL, headers http.Header, body []byte, secrets ...string) ([]byte, int, error) {
	if endpoint == nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, 0, c.failure(operation, "INVALID_BASE_URL", 0, false, nil)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, c.failure(operation, "INVALID_REQUEST", 0, false, nil)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9")
	req.Header.Set("User-Agent", "buy-sms/1.0")

	response, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return nil, 0, c.failure(operation, "TIMEOUT", 0, true, context.DeadlineExceeded)
		}
		if errors.Is(callCtx.Err(), context.Canceled) {
			return nil, 0, c.failure(operation, "CANCELED", 0, false, context.Canceled)
		}
		return nil, 0, c.failure(operation, "TRANSPORT_ERROR", 0, true, nil)
	}
	defer response.Body.Close()

	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, response.StatusCode, c.failure(operation, "READ_ERROR", response.StatusCode, true, nil)
	}
	if len(payload) > maxResponseBytes {
		return nil, response.StatusCode, c.failure(operation, "RESPONSE_TOO_LARGE", response.StatusCode, false, nil)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := safeCodeFromBody(payload, secrets...)
		if code == "" {
			code = http.StatusText(response.StatusCode)
		}
		return nil, response.StatusCode, c.failure(operation, code, response.StatusCode, response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500, nil)
	}
	return payload, response.StatusCode, nil
}

func (c *baseClient) get(ctx context.Context, operation, apiKey, relative string, query url.Values, nativeAuth bool) ([]byte, error) {
	u, err := c.endpoint(relative)
	if err != nil {
		return nil, c.failure(operation, "INVALID_BASE_URL", 0, false, nil)
	}
	if query == nil {
		query = make(url.Values)
	}
	headers := make(http.Header)
	if nativeAuth {
		headers.Set("Authorization", "ApiKey "+apiKey)
	} else {
		query.Set("api_key", apiKey)
	}
	u.RawQuery = query.Encode()
	payload, _, err := c.do(ctx, operation, http.MethodGet, u, headers, nil, apiKey)
	return payload, err
}

func (c *baseClient) json(ctx context.Context, operation, apiKey, method, relative string, value any) ([]byte, error) {
	u, err := c.endpoint(relative)
	if err != nil {
		return nil, c.failure(operation, "INVALID_BASE_URL", 0, false, nil)
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, c.failure(operation, "INVALID_REQUEST", 0, false, nil)
	}
	headers := make(http.Header)
	headers.Set("Authorization", "ApiKey "+apiKey)
	headers.Set("Content-Type", "application/json")
	payload, _, err := c.do(ctx, operation, method, u, headers, body, apiKey)
	return payload, err
}

func (c *baseClient) form(ctx context.Context, operation, apiKey, relative string, values url.Values) ([]byte, error) {
	u, err := c.endpoint(relative)
	if err != nil {
		return nil, c.failure(operation, "INVALID_BASE_URL", 0, false, nil)
	}
	if values == nil {
		values = make(url.Values)
	}
	values.Set("key", apiKey)
	var body bytes.Buffer
	formWriter := multipart.NewWriter(&body)
	for key, items := range values {
		for _, item := range items {
			if err := formWriter.WriteField(key, item); err != nil {
				return nil, c.failure(operation, "INVALID_REQUEST", 0, false, nil)
			}
		}
	}
	if err := formWriter.Close(); err != nil {
		return nil, c.failure(operation, "INVALID_REQUEST", 0, false, nil)
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+apiKey)
	headers.Set("Content-Type", formWriter.FormDataContentType())
	payload, _, err := c.do(ctx, operation, http.MethodPost, u, headers, body.Bytes(), apiKey)
	return payload, err
}

func (c *baseClient) failure(operation, code string, status int, retryable bool, cause error) error {
	return &ProviderError{
		Provider: c.providerID, Operation: operation, Code: sanitizeCode(code),
		HTTPStatus: status, Retryable: retryable, cause: cause,
	}
}

func safeCodeFromBody(payload []byte, secrets ...string) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		if object, ok := value.(map[string]any); ok {
			for _, key := range []string{"code", "type", "error", "status"} {
				if raw, found := lookup(object, key); found {
					if code := sanitizeCodeWithoutSecrets(stringValue(raw), secrets...); code != "" {
						return code
					}
				}
			}
		}
	}
	text := strings.TrimSpace(string(payload))
	if index := strings.IndexByte(text, ':'); index >= 0 {
		text = text[:index]
	}
	return sanitizeCodeWithoutSecrets(text, secrets...)
}

func sanitizeCodeWithoutSecrets(value string, secrets ...string) string {
	code := sanitizeCode(value)
	for _, secret := range secrets {
		secretCode := sanitizeCode(secret)
		if len(secretCode) >= 4 && strings.Contains(code, secretCode) {
			return "UPSTREAM_ERROR"
		}
	}
	return code
}

func sanitizeCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) > 64 {
		value = value[:64]
	}
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func decodeAny(payload []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func lookup(object map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, found := object[key]; found {
			return value, true
		}
	}
	for _, key := range keys {
		for actual, value := range object {
			if strings.EqualFold(actual, key) {
				return value, true
			}
		}
	}
	return nil, false
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

func floatValue(value any) (float64, bool) {
	text := stringValue(value)
	if text == "" {
		return 0, false
	}
	number, err := strconv.ParseFloat(text, 64)
	return number, err == nil
}

func intValue(value any) (int, bool) {
	number, ok := floatValue(value)
	return int(number), ok
}

func boolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case json.Number:
		number, err := typed.Int64()
		return number != 0, err == nil
	case float64:
		return typed != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "y":
			return true, true
		case "0", "false", "no", "n":
			return false, true
		}
	}
	return false, false
}

func parseTimeValue(value any) *time.Time {
	if number, ok := floatValue(value); ok {
		seconds := int64(number)
		if seconds > 1_000_000_000_000 {
			seconds /= 1000
		}
		if seconds > 0 {
			parsed := time.Unix(seconds, 0).UTC()
			return &parsed
		}
	}
	text := stringValue(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "02 Jan 2006 15:04:05"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			parsed = parsed.UTC()
			return &parsed
		}
	}
	return nil
}

func rawJSON(value any) json.RawMessage {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return payload
}

func fingerprint(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(hash[:])
}

func require(apiKey string, values ...string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("%w: api key 为空", ErrInvalidRequest)
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return ErrInvalidRequest
		}
	}
	return nil
}
