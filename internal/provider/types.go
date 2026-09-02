package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"buysms/internal/domain"
)

const (
	CatalogCountry = "country"
	CatalogService = "service"
	CatalogPrice   = "price"

	PollWaiting      = "waiting"
	PollWaitingRetry = "waiting_retry"
	PollProcessing   = "processing"
	PollReceived     = "received"
	PollCompleted    = "completed"
	PollCanceled     = "canceled"
	PollExpired      = "expired"
	PollRefunded     = "refunded"
	PollUnknown      = "unknown"

	defaultTimeout = 15 * time.Second
)

var (
	ErrInvalidRequest  = errors.New("供应商请求参数无效")
	ErrUnsupportedKind = errors.New("供应商目录类型不受支持")
)

// Client 是所有短信号码供应商必须实现的最小能力集合。API key 由服务层在
// 每次调用时传入，以便密钥始终只存在于服务端内存中。
type Client interface {
	ID() string
	Balance(context.Context, string) (BalanceResult, error)
	Catalog(context.Context, string, CatalogRequest) ([]domain.CatalogItem, error)
	Purchase(context.Context, string, PurchaseRequest) (PurchaseResult, error)
	Poll(context.Context, string, string) (PollResult, error)
	Complete(context.Context, string, string) error
	Cancel(context.Context, string, string) error
	RequestAnother(context.Context, string, string) (RequestAnotherResult, error)
}

// BalanceResult 保留供应商返回的十进制文本，避免展示时丢失精度或尾随零。
type BalanceResult struct {
	Amount   string
	Currency string
}

type CatalogRequest struct {
	Kind        string
	Country     string
	Service     string
	QualityTier string
}

type PurchaseRequest struct {
	Country        string
	Service        string
	QualityTier    string
	MaxPrice       *float64
	FixedPrice     *bool
	Pool           string
	Operator       string
	Duration       string
	ResellerUserID string
	Extra          map[string]string
}

type PurchaseResult struct {
	UpstreamID       string
	PhoneNumber      string
	CountryCode      string
	Cost             float64
	Currency         string
	CanGetAnotherSMS bool
	ExpiresAt        *time.Time
	Raw              json.RawMessage
}

// OTPMessage 表示供应商返回的一条短信。Poll 允许一次返回整个历史列表，
// 调用方应依靠 Fingerprint 做幂等入库，而不是只保留最后一条验证码。
type OTPMessage struct {
	UpstreamID  string
	Code        string
	Text        string
	Type        string
	PhoneFrom   string
	Generation  int
	ReceivedAt  time.Time
	Fingerprint string
}

type PollResult struct {
	State             string
	Code              string
	Text              string
	LastCode          string
	CanRequestAnother bool
	ExpiresAt         *time.Time
	Messages          []OTPMessage
	Raw               json.RawMessage
}

type RequestAnotherResult struct {
	Charge float64
}

// ProviderError 只保留经过清洗的错误码和 HTTP 状态。它有意不保存请求 URL、
// 响应正文和底层 transport 错误文本，防止 query/form 中的 API key 进入日志。
type ProviderError struct {
	Provider   string
	Operation  string
	Code       string
	HTTPStatus int
	Retryable  bool
	cause      error
}

func (e *ProviderError) Error() string {
	parts := []string{e.Provider, e.Operation, "供应商请求失败"}
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if e.HTTPStatus != 0 {
		parts = append(parts, fmt.Sprintf("http=%d", e.HTTPStatus))
	}
	return strings.Join(parts, " ")
}

func (e *ProviderError) Unwrap() error { return e.cause }

type clientConfig struct {
	httpClient *http.Client
	timeout    time.Duration
}

type Option func(*clientConfig)

func WithHTTPClient(client *http.Client) Option {
	return func(c *clientConfig) {
		if client != nil {
			c.httpClient = client
		}
	}
}

func WithTimeout(timeout time.Duration) Option {
	return func(c *clientConfig) {
		if timeout > 0 {
			c.timeout = timeout
		}
	}
}

func resolveOptions(options ...Option) clientConfig {
	c := clientConfig{httpClient: &http.Client{Transport: http.DefaultTransport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, timeout: defaultTimeout}
	for _, option := range options {
		if option != nil {
			option(&c)
		}
	}
	return c
}

// New 根据规范化后的供应商 ID 创建客户端。
func New(providerID, baseURL string, options ...Option) (Client, error) {
	switch domain.NormalizeProvider(providerID) {
	case domain.ProviderHeroSMS:
		return NewHeroSMS(baseURL, options...), nil
	case domain.ProviderSMSBower:
		return NewSMSBower(baseURL, options...), nil
	case domain.ProviderSMSPool:
		return NewSMSPool(baseURL, options...), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, providerID)
	}
}
