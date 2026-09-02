package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"buysms/internal/domain"
)

var (
	ErrNotFound = errors.New("记录不存在")
	ErrConflict = errors.New("记录已存在")
)

type Repository interface {
	Ping(context.Context) error
	PutCaptcha(context.Context, string, []byte, time.Time) error
	CaptchaAllowed(context.Context, string, time.Time, time.Duration, int) (bool, error)
	ConsumeCaptcha(context.Context, string, []byte, time.Time) (bool, error)
	ReserveLoginAttempt(context.Context, string, string, time.Time, time.Duration, int) (int64, bool, error)
	CompleteLoginAttempt(context.Context, int64, bool) error
	LoginAllowed(context.Context, string, time.Time, time.Duration, int) (bool, error)
	RecordLoginAttempt(context.Context, string, string, bool) error
	FindUserByUsername(context.Context, string) (domain.User, error)
	GetUser(context.Context, string) (domain.User, error)
	ListUsers(context.Context) ([]domain.User, error)
	CreateUser(context.Context, domain.User) error
	UpdateUser(context.Context, domain.User) error
	UpdatePassword(context.Context, string, string) error
	UpdatePasswordAndRevoke(context.Context, string, string) error
	CreateSession(context.Context, domain.Session) error
	FindSession(context.Context, []byte, time.Time) (domain.User, error)
	RevokeUserSessions(context.Context, string) error
	RevokeSession(context.Context, []byte) error
	TouchLastLogin(context.Context, string) error
	EnsureProviders(context.Context, []domain.Provider) error
	ListProviders(context.Context) ([]domain.Provider, error)
	GetProvider(context.Context, string) (domain.Provider, error)
	UpdateProvider(context.Context, domain.Provider) error
	ReplaceCatalog(context.Context, string, string, []domain.CatalogItem) error
	ListCatalog(context.Context, string, string, string) ([]domain.CatalogItem, error)
	CreateOrder(context.Context, domain.Order) error
	ReservePurchase(context.Context, PurchaseRecord) (PurchaseRecord, bool, error)
	CompletePurchase(context.Context, string, domain.Order) error
	FailPurchase(context.Context, string, string, string) error
	GetOrder(context.Context, string, string) (domain.Order, error)
	FindOrderByUpstream(context.Context, string, string) (domain.Order, error)
	ListOrders(context.Context, string, int, int) ([]domain.Order, error)
	SearchOrders(context.Context, string, string, string, string, int, int) ([]domain.Order, int, error)
	WithOrderLock(context.Context, string, func(context.Context) error) error
	SetOrderStatus(context.Context, string, string, string) error
	ClaimDueOrders(context.Context, int, time.Time, time.Duration) ([]domain.Order, error)
	UpdatePoll(context.Context, string, string, time.Time, int) error
	UpdateRequestNext(context.Context, string, bool, int, time.Time) error
	ClaimRequestNext(context.Context, string) (bool, error)
	RestoreRequestNext(context.Context, string, int, time.Time) error
	CompleteRequestNext(context.Context, string, float64) (bool, error)
	SaveMessage(context.Context, domain.SMSMessage, bool) (bool, error)
	SaveWebhookMessage(context.Context, WebhookRecord, domain.SMSMessage) (bool, error)
	SaveWebhookEvent(context.Context, WebhookRecord) (bool, error)
	Dashboard(context.Context, string) (domain.Dashboard, error)
	Audit(context.Context, *string, string, string, string, string, json.RawMessage) error
	Maintenance(context.Context, time.Time) error
	Close()
}

type PurchaseRecord struct {
	ID, UserID, IdempotencyKey, ProviderID, CountryCode, ServiceCode, Status, OrderID string
	MaxPrice                                                                          float64
}

type WebhookRecord struct {
	ID, ProviderID, UpstreamID, Fingerprint string
	Headers, Payload                        json.RawMessage
	Status, Error                           string
	ProviderState                           string
	ReceivedAt                              time.Time
}
