package application

import (
	"strconv"
	"time"

	"buysms/internal/domain"
)

type ProviderDTO struct {
	ID                     string    `json:"id"`
	Code                   string    `json:"code"`
	Name                   string    `json:"name"`
	APIBaseURL             string    `json:"apiBaseUrl"`
	Enabled                bool      `json:"enabled"`
	PollingIntervalSeconds int       `json:"pollingIntervalSeconds"`
	WebhookSupported       bool      `json:"webhookSupported"`
	WebhookEnabled         bool      `json:"webhookEnabled"`
	HasAPIKey              bool      `json:"hasApiKey"`
	Purchasable            bool      `json:"purchasable"`
	HasWebhookToken        bool      `json:"hasWebhookToken"`
	WebhookURL             string    `json:"webhookUrl,omitempty"`
	UpdatedAt              time.Time `json:"updatedAt"`
}

type ProviderBalanceDTO struct {
	Code          string     `json:"code"`
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	Purchasable   bool       `json:"purchasable"`
	Status        string     `json:"status"`
	Balance       string     `json:"balance,omitempty"`
	Currency      string     `json:"currency,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	Stale         bool       `json:"stale,omitempty"`
	Message       string     `json:"message,omitempty"`
}

type UpdateProviderInput struct {
	APIBaseURL             string `json:"apiBaseUrl"`
	APIKey                 string `json:"apiKey"`
	WebhookToken           string `json:"webhookToken"`
	WebhookEnabled         bool   `json:"webhookEnabled"`
	Enabled                bool   `json:"enabled"`
	PollingIntervalSeconds int    `json:"pollingIntervalSeconds"`
}

type SMSDTO struct {
	ID         string    `json:"id"`
	Code       string    `json:"code,omitempty"`
	Content    string    `json:"content"`
	ReceivedAt time.Time `json:"receivedAt"`
}
type OrderDTO struct {
	ID                           string     `json:"id"`
	Provider                     string     `json:"provider"`
	ProviderName                 string     `json:"providerName,omitempty"`
	PhoneNumber                  string     `json:"phoneNumber"`
	CountryCode                  string     `json:"countryCode"`
	CountryName                  string     `json:"countryName,omitempty"`
	ServiceCode                  string     `json:"serviceCode"`
	ServiceName                  string     `json:"serviceName,omitempty"`
	QualityTier                  string     `json:"tier,omitempty"`
	Duration                     string     `json:"duration,omitempty"`
	Status                       string     `json:"status"`
	Price                        string     `json:"price"`
	Currency                     string     `json:"currency"`
	Messages                     []SMSDTO   `json:"messages"`
	CurrentActivationHasMessages bool       `json:"currentActivationHasMessages"`
	RenewalPending               bool       `json:"renewalPending,omitempty"`
	WebhookEnabled               bool       `json:"webhookEnabled"`
	ExpiresAt                    *time.Time `json:"expiresAt,omitempty"`
	CanCancel                    bool       `json:"canCancel"`
	CancelAvailableAt            *time.Time `json:"cancelAvailableAt,omitempty"`
	CancelWaitSeconds            *int       `json:"cancelWaitSeconds,omitempty"`
	CancelUnavailableReason      string     `json:"cancelUnavailableReason,omitempty"`
	CreatedAt                    time.Time  `json:"createdAt"`
	UpdatedAt                    time.Time  `json:"updatedAt"`
}

type PurchaseInput struct {
	Provider       string `json:"provider"`
	CountryCode    string `json:"countryCode"`
	ServiceCode    string `json:"serviceCode"`
	QualityTier    string `json:"tier"`
	Duration       string `json:"duration"`
	MaxPrice       string `json:"maxPrice"`
	IdempotencyKey string `json:"-"`
}

type PurchaseAttemptDTO struct {
	Provider    string    `json:"provider"`
	CountryCode string    `json:"countryCode"`
	CountryName string    `json:"countryName,omitempty"`
	ServiceCode string    `json:"serviceCode"`
	ServiceName string    `json:"serviceName,omitempty"`
	QualityTier string    `json:"tier,omitempty"`
	Duration    string    `json:"duration,omitempty"`
	MaxPrice    string    `json:"maxPrice"`
	Status      string    `json:"status"`
	ErrorCode   string    `json:"errorCode"`
	Message     string    `json:"message"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type OrderQuery struct {
	Page, PageSize            int
	Status, Provider, Keyword string
}
type Page[T any] struct {
	Items    []T `json:"items"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"pageSize"`
}

type CountryDTO struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Flag      string `json:"flag,omitempty"`
	Available *int   `json:"available,omitempty"`
	PriceFrom string `json:"priceFrom,omitempty"`
}
type ServiceDTO struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Available *int   `json:"available,omitempty"`
	Price     string `json:"price,omitempty"`
}

type QuotePriceOptionDTO struct {
	Price     string `json:"price"`
	Available int    `json:"available"`
}

type QuoteDTO struct {
	Provider     string                `json:"provider"`
	ProviderName string                `json:"providerName"`
	CountryCode  string                `json:"countryCode"`
	ServiceCode  string                `json:"serviceCode"`
	QualityTier  string                `json:"tier,omitempty"`
	Price        string                `json:"price"`
	Currency     string                `json:"currency"`
	Available    int                   `json:"available"`
	PriceOptions []QuotePriceOptionDTO `json:"priceOptions,omitempty"`
}

type DurationOptionDTO struct {
	Value        string                `json:"value"`
	Minutes      int                   `json:"minutes"`
	Hours        int                   `json:"hours,omitempty"`
	Price        string                `json:"price"`
	Available    int                   `json:"available"`
	PriceOptions []QuotePriceOptionDTO `json:"priceOptions,omitempty"`
}

type RenewalOptionDTO struct {
	Value    int    `json:"value"`
	Unit     string `json:"unit"`
	Minutes  int    `json:"minutes"`
	Price    string `json:"price"`
	Currency string `json:"currency"`
}

type RenewalOptionsDTO struct {
	Mode    string             `json:"mode"`
	Options []RenewalOptionDTO `json:"options"`
}

type RenewalInput struct {
	Value          int    `json:"value"`
	Unit           string `json:"unit"`
	QuotedPrice    string `json:"quotedPrice"`
	IdempotencyKey string `json:"-"`
}
type ProviderHealthDTO struct {
	Code          string     `json:"code"`
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	Healthy       bool       `json:"healthy"`
	Balance       string     `json:"balance,omitempty"`
	Currency      string     `json:"currency,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	Message       string     `json:"message,omitempty"`
}
type DashboardDTO struct {
	ActiveOrders    int                 `json:"activeOrders"`
	TodayOrders     int                 `json:"todayOrders"`
	TodayMessages   int                 `json:"todayMessages"`
	TodaySpend      string              `json:"todaySpend"`
	Currency        string              `json:"currency"`
	ProviderHealthy int                 `json:"providerHealthy"`
	ProviderTotal   int                 `json:"providerTotal"`
	RecentOrders    []OrderDTO          `json:"recentOrders"`
	Providers       []ProviderHealthDTO `json:"providers"`
}

type UserDTO struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"displayName,omitempty"`
	Role        string     `json:"role"`
	Enabled     bool       `json:"enabled"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
}
type SaveUserInput struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	Password    string `json:"password"`
}

func OrderView(o domain.Order, webhook bool, now time.Time) OrderDTO {
	status := o.Status
	if status == domain.OrderCanceled {
		status = "cancelled"
	}
	messages := make([]SMSDTO, 0, len(o.Messages))
	for _, m := range o.Messages {
		messages = append(messages, SMSDTO{ID: m.ID, Code: m.Code, Content: m.Text, ReceivedAt: m.ReceivedAt})
	}
	cancel := EvaluateCancelPolicy(o, now)
	var waitSeconds *int
	if cancel.WaitSeconds > 0 {
		waitSeconds = &cancel.WaitSeconds
	}
	return OrderDTO{ID: o.ID, Provider: o.ProviderID, ProviderName: providerName(o.ProviderID), PhoneNumber: o.PhoneNumber, CountryCode: o.CountryCode, CountryName: o.CountryName, ServiceCode: o.ServiceCode, ServiceName: o.ServiceName, QualityTier: o.QualityTier, Duration: o.Duration, Status: status, Price: strconv.FormatFloat(o.Cost, 'f', -1, 64), Currency: o.Currency, Messages: messages, CurrentActivationHasMessages: hasCurrentActivationMessage(o), RenewalPending: o.RenewalInflight, WebhookEnabled: webhook, ExpiresAt: o.ExpiresAt, CanCancel: cancel.Allowed, CancelAvailableAt: cancel.AvailableAt, CancelWaitSeconds: waitSeconds, CancelUnavailableReason: cancel.UnavailableReason, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}
}
func UserView(u domain.User) UserDTO {
	return UserDTO{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role, Enabled: u.Active, LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt}
}
func providerName(id string) string {
	switch id {
	case domain.ProviderHeroSMS:
		return "HeroSMS"
	case domain.ProviderSMSBower:
		return "SMSBower"
	case domain.ProviderSMSPool:
		return "SMSPool"
	default:
		return id
	}
}
