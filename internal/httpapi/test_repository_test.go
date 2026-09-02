package httpapi_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"buysms/internal/domain"
	"buysms/internal/store"
)

// memoryRepository 仅用于 HTTP 和接码生命周期测试。嵌入接口可让每个测试
// 只实现会实际触发的方法；若生产代码意外调用了其他仓储方法，测试会立即
// 暴露该调用，而不会连接宿主机上的 PostgreSQL。
type memoryRepository struct {
	store.Repository

	mu sync.Mutex

	captchas  map[string]memoryCaptcha
	providers map[string]domain.Provider
	orders    map[string]domain.Order
	purchases []store.PurchaseRecord
	messages  []domain.SMSMessage
	webhooks  map[string]store.WebhookRecord
	claimed   map[string]bool
	sessions  map[string]domain.User

	statusTransitions []string
	pollUpdated       chan struct{}
	providerReads     int
}

type memoryCaptcha struct {
	digest    []byte
	expiresAt time.Time
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{
		captchas:    make(map[string]memoryCaptcha),
		providers:   make(map[string]domain.Provider),
		orders:      make(map[string]domain.Order),
		webhooks:    make(map[string]store.WebhookRecord),
		claimed:     make(map[string]bool),
		sessions:    make(map[string]domain.User),
		pollUpdated: make(chan struct{}, 1),
	}
}

func (r *memoryRepository) Ping(context.Context) error { return nil }

func (r *memoryRepository) CaptchaAllowed(context.Context, string, time.Time, time.Duration, int) (bool, error) {
	return true, nil
}

func (r *memoryRepository) PutCaptcha(_ context.Context, id string, digest []byte, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.captchas[id] = memoryCaptcha{digest: append([]byte(nil), digest...), expiresAt: expiresAt}
	return nil
}

func (r *memoryRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providerReads++
	p, ok := r.providers[id]
	if !ok {
		return domain.Provider{}, store.ErrNotFound
	}
	return p, nil
}

func (r *memoryRepository) providerReadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerReads
}

func (r *memoryRepository) ListProviders(context.Context) ([]domain.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	providers := make([]domain.Provider, 0, len(r.providers))
	for _, provider := range r.providers {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return providers, nil
}

func (r *memoryRepository) FindSession(_ context.Context, digest []byte, _ time.Time) (domain.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	user, ok := r.sessions[string(digest)]
	if !ok || !user.Active {
		return domain.User{}, store.ErrNotFound
	}
	return user, nil
}

func (r *memoryRepository) FindOrderByUpstream(_ context.Context, providerID, upstreamID string) (domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, order := range r.orders {
		if order.ProviderID == providerID && order.UpstreamID == upstreamID {
			return order, nil
		}
	}
	return domain.Order{}, store.ErrNotFound
}

func (r *memoryRepository) SaveWebhookMessage(_ context.Context, record store.WebhookRecord, message domain.SMSMessage) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	eventKey := record.ProviderID + "\x00" + record.Fingerprint
	if _, exists := r.webhooks[eventKey]; exists {
		return false, nil
	}
	r.webhooks[eventKey] = cloneWebhookRecord(record)

	for _, existing := range r.messages {
		if existing.OrderID == message.OrderID && existing.UpstreamFingerprint == message.UpstreamFingerprint {
			return false, nil
		}
	}
	r.messages = append(r.messages, message)

	order := r.orders[message.OrderID]
	order.PollSequence++
	order.LastProviderState = record.ProviderState
	order.PollFailures = 0
	order.RequestNextPending = order.CanGetAnotherSMS
	order.RequestNextFailures = 0
	order.UpdatedAt = time.Now().UTC()
	r.orders[message.OrderID] = order
	return true, nil
}

func (r *memoryRepository) SaveWebhookEvent(_ context.Context, record store.WebhookRecord) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := record.ProviderID + "\x00" + record.Fingerprint
	if _, exists := r.webhooks[key]; exists {
		return false, nil
	}
	r.webhooks[key] = cloneWebhookRecord(record)
	return true, nil
}

func (r *memoryRepository) ClaimDueOrders(_ context.Context, limit int, now time.Time, _ time.Duration) ([]domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	claimed := make([]domain.Order, 0, limit)
	for id, order := range r.orders {
		if len(claimed) == limit {
			break
		}
		if order.Status != domain.OrderActive || order.NextPollAt.After(now) || r.claimed[id] {
			continue
		}
		r.claimed[id] = true
		claimed = append(claimed, order)
	}
	return claimed, nil
}

func (r *memoryRepository) SaveMessage(_ context.Context, message domain.SMSMessage, advance bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.messages {
		if existing.OrderID == message.OrderID && existing.UpstreamFingerprint == message.UpstreamFingerprint {
			return false, nil
		}
	}
	r.messages = append(r.messages, message)
	if advance {
		order := r.orders[message.OrderID]
		order.PollSequence++
		order.UpdatedAt = time.Now().UTC()
		r.orders[message.OrderID] = order
	}
	return true, nil
}

func (r *memoryRepository) UpdatePoll(_ context.Context, id, state string, next time.Time, failures int) error {
	r.mu.Lock()
	order := r.orders[id]
	order.LastProviderState = state
	order.NextPollAt = next
	order.PollFailures = failures
	r.orders[id] = order
	r.mu.Unlock()
	select {
	case r.pollUpdated <- struct{}{}:
	default:
	}
	return nil
}

func (r *memoryRepository) UpdateRequestNext(_ context.Context, id string, pending bool, failures int, next time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	order := r.orders[id]
	order.RequestNextPending = pending
	order.RequestNextFailures = failures
	if pending && next.Before(order.NextPollAt) {
		order.NextPollAt = next
	}
	r.orders[id] = order
	return nil
}

func (r *memoryRepository) SetOrderStatus(_ context.Context, id, status, providerState string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	order := r.orders[id]
	order.Status = status
	order.LastProviderState = providerState
	r.orders[id] = order
	r.statusTransitions = append(r.statusTransitions, status)
	return nil
}

func (r *memoryRepository) Dashboard(_ context.Context, userID string) (domain.Dashboard, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var dashboard domain.Dashboard
	for _, order := range r.orders {
		if userID != "" && order.UserID != userID {
			continue
		}
		if order.Status == domain.OrderActive {
			dashboard.ActiveOrders++
		}
		dashboard.TodayOrders++
		if order.Status != domain.OrderCanceled {
			dashboard.TodayCost += order.Cost
		}
		for _, message := range r.messages {
			if message.OrderID == order.ID {
				dashboard.TodaySMS++
			}
		}
	}
	return dashboard, nil
}

func (r *memoryRepository) ListOrders(_ context.Context, userID string, limit, offset int) ([]domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	orders := make([]domain.Order, 0, len(r.orders))
	for _, order := range r.orders {
		if userID != "" && order.UserID != userID {
			continue
		}
		order.Messages = nil
		for _, message := range r.messages {
			if message.OrderID == order.ID {
				order.Messages = append(order.Messages, message)
			}
		}
		orders = append(orders, order)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt.After(orders[j].CreatedAt) })
	if offset >= len(orders) {
		return []domain.Order{}, nil
	}
	orders = orders[offset:]
	if limit > 0 && len(orders) > limit {
		orders = orders[:limit]
	}
	return orders, nil
}

func (r *memoryRepository) ListPurchaseRequests(_ context.Context, userID string, limit int) ([]store.PurchaseRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records := make([]store.PurchaseRecord, 0, len(r.purchases))
	for _, record := range r.purchases {
		if record.UserID == userID {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID > records[j].ID
		}
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func (r *memoryRepository) putProvider(provider domain.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[provider.ID] = provider
}

func (r *memoryRepository) putOrder(order domain.Order) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.orders[order.ID] = order
}

func (r *memoryRepository) putPurchase(record store.PurchaseRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purchases = append(r.purchases, record)
}

func (r *memoryRepository) putSession(pepper []byte, token string, user domain.User) {
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte("session:" + token))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[string(mac.Sum(nil))] = user
}

func (r *memoryRepository) snapshot() (map[string]domain.Order, []domain.SMSMessage, []store.WebhookRecord, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	orders := make(map[string]domain.Order, len(r.orders))
	for id, order := range r.orders {
		orders[id] = order
	}
	messages := append([]domain.SMSMessage(nil), r.messages...)
	webhooks := make([]store.WebhookRecord, 0, len(r.webhooks))
	for _, record := range r.webhooks {
		webhooks = append(webhooks, cloneWebhookRecord(record))
	}
	transitions := append([]string(nil), r.statusTransitions...)
	return orders, messages, webhooks, transitions
}

func (r *memoryRepository) captchaCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.captchas)
}

func cloneWebhookRecord(record store.WebhookRecord) store.WebhookRecord {
	record.Headers = append(json.RawMessage(nil), record.Headers...)
	record.Payload = append(json.RawMessage(nil), record.Payload...)
	return record
}
