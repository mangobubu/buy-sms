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
	HasWebhookToken        bool      `json:"hasWebhookToken"`
	WebhookURL             string    `json:"webhookUrl,omitempty"`
	UpdatedAt              time.Time `json:"updatedAt"`
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
	ID             string    `json:"id"`
	Provider       string    `json:"provider"`
	ProviderName   string    `json:"providerName,omitempty"`
	PhoneNumber    string    `json:"phoneNumber"`
	CountryCode    string    `json:"countryCode"`
	ServiceCode    string    `json:"serviceCode"`
	Status         string    `json:"status"`
	Price          string    `json:"price"`
	Currency       string    `json:"currency"`
	Messages       []SMSDTO  `json:"messages"`
	WebhookEnabled bool      `json:"webhookEnabled"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type PurchaseInput struct {
	Provider       string `json:"provider"`
	CountryCode    string `json:"countryCode"`
	ServiceCode    string `json:"serviceCode"`
	MaxPrice       string `json:"maxPrice"`
	IdempotencyKey string `json:"-"`
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
type QuoteDTO struct {
	Provider     string `json:"provider"`
	ProviderName string `json:"providerName"`
	CountryCode  string `json:"countryCode"`
	ServiceCode  string `json:"serviceCode"`
	Price        string `json:"price"`
	Currency     string `json:"currency"`
	Available    int    `json:"available"`
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

func OrderView(o domain.Order, webhook bool) OrderDTO {
	status := o.Status
	if status == domain.OrderCanceled {
		status = "cancelled"
	}
	messages := make([]SMSDTO, 0, len(o.Messages))
	for _, m := range o.Messages {
		messages = append(messages, SMSDTO{ID: m.ID, Code: m.Code, Content: m.Text, ReceivedAt: m.ReceivedAt})
	}
	return OrderDTO{ID: o.ID, Provider: o.ProviderID, ProviderName: providerName(o.ProviderID), PhoneNumber: o.PhoneNumber, CountryCode: o.CountryCode, ServiceCode: o.ServiceCode, Status: status, Price: strconv.FormatFloat(o.Cost, 'f', -1, 64), Currency: o.Currency, Messages: messages, WebhookEnabled: webhook, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}
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
