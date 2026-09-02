package domain

import (
	"encoding/json"
	"time"
)

const (
	ProviderHeroSMS  = "herosms"
	ProviderSMSBower = "smsbower"
	ProviderSMSPool  = "smspool"

	OrderActive    = "active"
	OrderCompleted = "completed"
	OrderCanceled  = "canceled"
	OrderExpired   = "expired"
)

type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	DisplayName  string     `json:"displayName,omitempty"`
	PasswordHash string     `json:"-"`
	Role         string     `json:"role"`
	Active       bool       `json:"active"`
	LastLoginAt  *time.Time `json:"lastLoginAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

type Session struct {
	ID        string
	UserID    string
	TokenHash []byte
	IP        string
	UserAgent string
	ExpiresAt time.Time
}

type Provider struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	BaseURL            string          `json:"baseUrl"`
	APIKeyCipher       []byte          `json:"-"`
	APIKeyConfigured   bool            `json:"apiKeyConfigured"`
	Enabled            bool            `json:"enabled"`
	WebhookTokenCipher []byte          `json:"-"`
	WebhookConfigured  bool            `json:"webhookConfigured"`
	Config             json.RawMessage `json:"config"`
	CreatedAt          time.Time       `json:"createdAt"`
	UpdatedAt          time.Time       `json:"updatedAt"`
}

type CatalogItem struct {
	ProviderID string          `json:"providerId"`
	Kind       string          `json:"kind"`
	Code       string          `json:"code"`
	Country    string          `json:"country,omitempty"`
	Name       string          `json:"name"`
	Price      *float64        `json:"price,omitempty"`
	Stock      *int            `json:"stock,omitempty"`
	Raw        json.RawMessage `json:"-"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

type Order struct {
	ID                         string       `json:"id"`
	UserID                     string       `json:"userId"`
	ProviderID                 string       `json:"providerId"`
	UpstreamID                 string       `json:"upstreamId"`
	PhoneNumber                string       `json:"phoneNumber"`
	CountryCode                string       `json:"countryCode"`
	ServiceCode                string       `json:"serviceCode"`
	Status                     string       `json:"status"`
	Cost                       float64      `json:"cost"`
	Currency                   string       `json:"currency"`
	CanGetAnotherSMS           bool         `json:"canGetAnotherSms"`
	PollSequence               int64        `json:"-"`
	LastProviderState          string       `json:"providerState,omitempty"`
	NextPollAt                 time.Time    `json:"-"`
	PollFailures               int          `json:"-"`
	RequestNextPending         bool         `json:"-"`
	RequestNextInflight        bool         `json:"-"`
	RequestNextInflightAt      *time.Time   `json:"-"`
	RequestNextGeneration      int64        `json:"-"`
	RequestNextClaimGeneration int64        `json:"-"`
	RequestNextFailures        int          `json:"-"`
	ExpiresAt                  *time.Time   `json:"expiresAt,omitempty"`
	CreatedAt                  time.Time    `json:"createdAt"`
	UpdatedAt                  time.Time    `json:"updatedAt"`
	Messages                   []SMSMessage `json:"messages"`
}

type SMSMessage struct {
	ID                  string    `json:"id"`
	OrderID             string    `json:"orderId"`
	ProviderID          string    `json:"providerId"`
	Code                string    `json:"code"`
	Text                string    `json:"text"`
	Source              string    `json:"source"`
	UpstreamFingerprint string    `json:"-"`
	ReceivedAt          time.Time `json:"receivedAt"`
	CreatedAt           time.Time `json:"createdAt"`
}

type Dashboard struct {
	ActiveOrders int     `json:"activeOrders"`
	TodayOrders  int     `json:"todayOrders"`
	TodayCost    float64 `json:"todayCost"`
	TodaySMS     int     `json:"todaySms"`
}

func NormalizeProvider(v string) string {
	switch v {
	case "smsbrower", "sms-bower", "sms_bower", ProviderSMSBower:
		return ProviderSMSBower
	case "hero-sms", "hero_sms", ProviderHeroSMS:
		return ProviderHeroSMS
	case "sms-pool", "sms_pool", ProviderSMSPool:
		return ProviderSMSPool
	default:
		return v
	}
}

func (o Order) Terminal() bool {
	return o.Status == OrderCompleted || o.Status == OrderCanceled || o.Status == OrderExpired
}
