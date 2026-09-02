package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"buysms/internal/auth"
	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/identity"
	"buysms/internal/provider"
	"buysms/internal/secure"
	"buysms/internal/store"
)

var (
	ErrBadRequest = errors.New("请求参数不正确")
	ErrNotFound   = errors.New("记录不存在")
	ErrConflict   = errors.New("当前状态不允许此操作")
	ErrForbidden  = errors.New("权限不足")
	ErrProvider   = errors.New("供应商暂时不可用")
)

type Service struct {
	repo                    store.Repository
	auth                    *auth.Service
	vault                   *secure.Vault
	config                  config.Config
	afterMessage            chan domain.Order
	accountingRecovery      chan accountingRecovery
	accountingFallbackSlots chan struct{}
	now                     func() time.Time
}

type accountingRecovery struct {
	OrderID string
	Charge  float64
}

func New(repo store.Repository, authentication *auth.Service, vault *secure.Vault, cfg config.Config) *Service {
	return &Service{repo: repo, auth: authentication, vault: vault, config: cfg, afterMessage: make(chan domain.Order, 128), accountingRecovery: make(chan accountingRecovery, 128), accountingFallbackSlots: make(chan struct{}, 2), now: time.Now}
}

func (s *Service) Bootstrap(ctx context.Context) error {
	defaults := []struct {
		id, name, url string
		interval      int
	}{
		{domain.ProviderHeroSMS, "HeroSMS", s.config.HeroSMSBaseURL, int(s.config.ReconcileInterval.Seconds())},
		{domain.ProviderSMSBower, "SMSBower", s.config.SMSBowerBaseURL, int(s.config.ReconcileInterval.Seconds())},
		{domain.ProviderSMSPool, "SMSPool", s.config.SMSPoolBaseURL, int(s.config.PollInterval.Seconds())},
	}
	ps := make([]domain.Provider, 0, len(defaults))
	for _, d := range defaults {
		token, err := s.vault.Encrypt(identity.Token(32))
		if err != nil {
			return err
		}
		settings, _ := json.Marshal(providerSettings{PollingIntervalSeconds: clampInterval(d.interval)})
		ps = append(ps, domain.Provider{ID: d.id, Name: d.name, BaseURL: d.url, WebhookTokenCipher: token, Config: settings})
	}
	if err := s.repo.EnsureProviders(ctx, ps); err != nil {
		return err
	}
	existing, err := s.repo.ListProviders(ctx)
	if err != nil {
		return err
	}
	for _, p := range existing {
		if len(p.WebhookTokenCipher) == 0 {
			p.WebhookTokenCipher, err = s.vault.Encrypt(identity.Token(32))
			if err != nil {
				return err
			}
			if err = s.repo.UpdateProvider(ctx, p); err != nil {
				return err
			}
		}
	}
	if strings.TrimSpace(s.config.AdminPassword) != "" {
		_, err = s.repo.FindUserByUsername(ctx, s.config.AdminUsername)
		if errors.Is(err, store.ErrNotFound) {
			hash, e := s.auth.HashPassword(s.config.AdminPassword)
			if e != nil {
				return e
			}
			u := domain.User{ID: identity.UUID(), Username: s.config.AdminUsername, DisplayName: "系统管理员", PasswordHash: hash, Role: "admin", Active: true}
			if e = s.repo.CreateUser(ctx, u); e != nil && !errors.Is(e, store.ErrConflict) {
				return e
			}
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

type providerSettings struct {
	PollingIntervalSeconds int  `json:"pollingIntervalSeconds"`
	WebhookEnabled         bool `json:"webhookEnabled"`
}

func (s *Service) Providers(ctx context.Context) ([]ProviderDTO, error) {
	ps, err := s.repo.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderDTO, 0, len(ps))
	for _, p := range ps {
		v, e := s.providerView(p)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *Service) UpdateProvider(ctx context.Context, id string, in UpdateProviderInput, user domain.User, ip string) (ProviderDTO, error) {
	id = domain.NormalizeProvider(id)
	p, err := s.repo.GetProvider(ctx, id)
	if err != nil {
		return ProviderDTO{}, mapStore(err)
	}
	if in.APIBaseURL != "" {
		u, e := validateProviderURL(ctx, id, strings.TrimSpace(in.APIBaseURL), s.config.Environment == "production")
		if e != nil {
			return ProviderDTO{}, ErrBadRequest
		}
		p.BaseURL = strings.TrimRight(u.String(), "/")
	}
	if in.APIKey != "" {
		p.APIKeyCipher, err = s.vault.Encrypt(strings.TrimSpace(in.APIKey))
		if err != nil {
			return ProviderDTO{}, err
		}
	}
	if in.WebhookToken != "" {
		if len(in.WebhookToken) < 24 {
			return ProviderDTO{}, fmt.Errorf("%w: webhook token 至少需要 24 个字符", ErrBadRequest)
		}
		p.WebhookTokenCipher, err = s.vault.Encrypt(in.WebhookToken)
		if err != nil {
			return ProviderDTO{}, err
		}
	}
	settings := readSettings(p.Config)
	if in.PollingIntervalSeconds != 0 {
		settings.PollingIntervalSeconds = clampInterval(in.PollingIntervalSeconds)
	}
	settings.WebhookEnabled = in.WebhookEnabled && len(p.WebhookTokenCipher) > 0
	p.Config, _ = json.Marshal(settings)
	p.Enabled = in.Enabled
	if err = s.repo.UpdateProvider(ctx, p); err != nil {
		return ProviderDTO{}, mapStore(err)
	}
	_ = s.repo.Audit(ctx, &user.ID, "provider.update", "provider", p.ID, ip, nil)
	p, err = s.repo.GetProvider(ctx, id)
	if err != nil {
		return ProviderDTO{}, err
	}
	return s.providerView(p)
}

func (s *Service) providerView(p domain.Provider) (ProviderDTO, error) {
	settings := readSettings(p.Config)
	token, err := s.vault.Decrypt(p.WebhookTokenCipher)
	if err != nil {
		return ProviderDTO{}, err
	}
	return ProviderDTO{ID: p.ID, Code: p.ID, Name: p.Name, APIBaseURL: p.BaseURL, Enabled: p.Enabled, PollingIntervalSeconds: settings.PollingIntervalSeconds, WebhookSupported: true, WebhookEnabled: settings.WebhookEnabled, HasAPIKey: p.APIKeyConfigured, HasWebhookToken: p.WebhookConfigured, WebhookURL: s.config.PublicBaseURL + "/api/webhooks/" + p.ID + "/" + url.PathEscape(token), UpdatedAt: p.UpdatedAt}, nil
}

func (s *Service) Countries(ctx context.Context, pid string) ([]CountryDTO, error) {
	items, err := s.catalog(ctx, pid, provider.CatalogRequest{Kind: provider.CatalogCountry})
	if err != nil {
		return nil, err
	}
	out := make([]CountryDTO, 0, len(items))
	for _, x := range items {
		out = append(out, CountryDTO{Code: x.Code, Name: x.Name, Available: x.Stock, PriceFrom: formatPrice(x.Price)})
	}
	return out, nil
}
func (s *Service) Services(ctx context.Context, pid, country string) ([]ServiceDTO, error) {
	if strings.TrimSpace(country) == "" {
		return nil, ErrBadRequest
	}
	items, err := s.catalog(ctx, pid, provider.CatalogRequest{Kind: provider.CatalogService, Country: country})
	if err != nil {
		return nil, err
	}
	out := make([]ServiceDTO, 0, len(items))
	for _, x := range items {
		out = append(out, ServiceDTO{Code: x.Code, Name: x.Name, Available: x.Stock, Price: formatPrice(x.Price)})
	}
	return out, nil
}
func (s *Service) Quote(ctx context.Context, pid, country, service string) (QuoteDTO, error) {
	if country == "" || service == "" {
		return QuoteDTO{}, ErrBadRequest
	}
	items, err := s.catalog(ctx, pid, provider.CatalogRequest{Kind: provider.CatalogPrice, Country: country, Service: service})
	if err != nil {
		return QuoteDTO{}, err
	}
	for _, x := range items {
		if x.Code == service && (x.Country == "" || x.Country == country) {
			price := "0"
			if x.Price != nil {
				price = strconv.FormatFloat(*x.Price, 'f', -1, 64)
			}
			stock := 0
			if x.Stock != nil {
				stock = *x.Stock
			}
			return QuoteDTO{Provider: domain.NormalizeProvider(pid), ProviderName: providerName(domain.NormalizeProvider(pid)), CountryCode: country, ServiceCode: service, Price: price, Currency: "USD", Available: stock}, nil
		}
	}
	return QuoteDTO{}, ErrNotFound
}

func (s *Service) catalog(ctx context.Context, pid string, req provider.CatalogRequest) ([]domain.CatalogItem, error) {
	pid = domain.NormalizeProvider(pid)
	p, key, client, err := s.providerClient(ctx, pid)
	if err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, ErrConflict
	}
	items, callErr := client.Catalog(ctx, key, req)
	if callErr == nil && len(items) > 0 {
		for i := range items {
			items[i].ProviderID = pid
			items[i].Kind = req.Kind
			if items[i].Country == "" && req.Kind != provider.CatalogCountry {
				items[i].Country = req.Country
			}
			items[i].UpdatedAt = s.now()
		}
		if err = s.repo.ReplaceCatalog(ctx, pid, req.Kind, items); err != nil {
			return nil, err
		}
		return items, nil
	}
	cached, cacheErr := s.repo.ListCatalog(ctx, pid, req.Kind, req.Country)
	if cacheErr == nil && len(cached) > 0 {
		return cached, nil
	}
	return nil, ErrProvider
}

func (s *Service) Purchase(ctx context.Context, in PurchaseInput, user domain.User, ip string) (OrderDTO, error) {
	pid := domain.NormalizeProvider(in.Provider)
	if pid == "" || in.CountryCode == "" || in.ServiceCode == "" {
		return OrderDTO{}, ErrBadRequest
	}
	max, err := strconv.ParseFloat(in.MaxPrice, 64)
	if err != nil || max <= 0 || max > 1_000_000 || math.IsNaN(max) || math.IsInf(max, 0) || len(in.IdempotencyKey) < 16 || len(in.IdempotencyKey) > 128 {
		return OrderDTO{}, ErrBadRequest
	}
	record, created, err := s.repo.ReservePurchase(ctx, store.PurchaseRecord{ID: identity.UUID(), UserID: user.ID, IdempotencyKey: in.IdempotencyKey, ProviderID: pid, CountryCode: in.CountryCode, ServiceCode: in.ServiceCode, MaxPrice: max})
	if err != nil {
		return OrderDTO{}, err
	}
	if !created {
		if record.ProviderID != pid || record.CountryCode != in.CountryCode || record.ServiceCode != in.ServiceCode || math.Abs(record.MaxPrice-max) > .000001 {
			return OrderDTO{}, ErrConflict
		}
		if record.Status == "succeeded" && record.OrderID != "" {
			return s.Order(ctx, record.OrderID, user)
		}
		return OrderDTO{}, ErrConflict
	}
	p, key, client, err := s.providerClient(ctx, pid)
	if err != nil {
		s.failPurchase(record.ID, "failed", "configuration")
		return OrderDTO{}, err
	}
	if !p.Enabled {
		s.failPurchase(record.ID, "failed", "provider_disabled")
		return OrderDTO{}, ErrConflict
	}
	result, err := client.Purchase(ctx, key, provider.PurchaseRequest{Country: in.CountryCode, Service: in.ServiceCode, MaxPrice: &max})
	if err != nil {
		// 上游可能已经完成购买后才发生超时或断线。此时原请求上下文通常
		// 已取消，必须用独立短时上下文持久化 unknown，阻止同一幂等键重购。
		s.failPurchase(record.ID, "unknown", "provider_error")
		return OrderDTO{}, ErrProvider
	}
	if result.Cost > max+0.000001 {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = client.Cancel(cancelCtx, key, result.UpstreamID)
		s.failPurchase(record.ID, "failed", "price_exceeded")
		return OrderDTO{}, ErrConflict
	}
	o := domain.Order{ID: identity.UUID(), UserID: user.ID, ProviderID: pid, UpstreamID: result.UpstreamID, PhoneNumber: result.PhoneNumber, CountryCode: in.CountryCode, ServiceCode: in.ServiceCode, Status: domain.OrderActive, Cost: result.Cost, Currency: result.Currency, CanGetAnotherSMS: result.CanGetAnotherSMS, NextPollAt: s.now(), ExpiresAt: result.ExpiresAt}
	if o.Currency == "" {
		o.Currency = "USD"
	}
	if err = s.repo.CompletePurchase(ctx, record.ID, o); err != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = client.Cancel(cancelCtx, key, result.UpstreamID)
		s.failPurchase(record.ID, "unknown", "database_error")
		return OrderDTO{}, mapStore(err)
	}
	_ = s.repo.Audit(ctx, &user.ID, "order.create", "order", o.ID, ip, nil)
	fresh, err := s.repo.GetOrder(ctx, o.ID, "")
	if err != nil {
		return OrderDTO{}, err
	}
	return OrderView(fresh, readSettings(p.Config).WebhookEnabled), nil
}

func (s *Service) failPurchase(id, status, code string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.repo.FailPurchase(cleanupCtx, id, status, code); err != nil {
		slog.Error("更新购买意图失败", "purchase_id", id, "status", status, "error", err)
	}
}

func (s *Service) Orders(ctx context.Context, q OrderQuery, user domain.User) (Page[OrderDTO], error) {
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.PageSize <= 0 || q.PageSize > 100 {
		q.PageSize = 20
	}
	scope := ""
	if user.Role != "admin" {
		scope = user.ID
	}
	status := q.Status
	if status == "cancelled" {
		status = domain.OrderCanceled
	}
	providerID := ""
	if q.Provider != "" {
		providerID = domain.NormalizeProvider(q.Provider)
	}
	orders, total, err := s.repo.SearchOrders(ctx, scope, status, providerID, strings.TrimSpace(q.Keyword), q.PageSize, (q.Page-1)*q.PageSize)
	if err != nil {
		return Page[OrderDTO]{}, err
	}
	out := make([]OrderDTO, 0, len(orders))
	for _, o := range orders {
		p, _ := s.repo.GetProvider(ctx, o.ProviderID)
		out = append(out, OrderView(o, readSettings(p.Config).WebhookEnabled))
	}
	return Page[OrderDTO]{Items: out, Total: total, Page: q.Page, PageSize: q.PageSize}, nil
}
func (s *Service) Order(ctx context.Context, id string, user domain.User) (OrderDTO, error) {
	scope := ""
	if user.Role != "admin" {
		scope = user.ID
	}
	o, err := s.repo.GetOrder(ctx, id, scope)
	if err != nil {
		return OrderDTO{}, mapStore(err)
	}
	p, _ := s.repo.GetProvider(ctx, o.ProviderID)
	return OrderView(o, readSettings(p.Config).WebhookEnabled), nil
}
func (s *Service) FinishOrder(ctx context.Context, id, action string, user domain.User, ip string) (OrderDTO, error) {
	scope := ""
	if user.Role != "admin" {
		scope = user.ID
	}
	var view OrderDTO
	err := s.repo.WithOrderLock(ctx, id, func(lockCtx context.Context) error {
		o, err := s.repo.GetOrder(lockCtx, id, scope)
		if err != nil {
			return mapStore(err)
		}
		if o.Terminal() {
			return ErrConflict
		}
		p, key, client, err := s.providerClient(lockCtx, o.ProviderID)
		if err != nil {
			return err
		}
		status := domain.OrderCompleted
		if action == "cancel" {
			if err = client.Cancel(lockCtx, key, o.UpstreamID); err != nil {
				return ErrProvider
			}
			status = domain.OrderCanceled
		} else {
			if err = client.Complete(lockCtx, key, o.UpstreamID); err != nil {
				return ErrProvider
			}
		}
		if err = s.repo.SetOrderStatus(lockCtx, o.ID, status, "user_"+action); err != nil {
			return mapStore(err)
		}
		_ = s.repo.Audit(lockCtx, &user.ID, "order."+action, "order", o.ID, ip, nil)
		o, err = s.repo.GetOrder(lockCtx, o.ID, scope)
		if err != nil {
			return err
		}
		view = OrderView(o, readSettings(p.Config).WebhookEnabled)
		return nil
	})
	if err != nil {
		return OrderDTO{}, mapStore(err)
	}
	return view, nil
}

func (s *Service) Dashboard(ctx context.Context, user domain.User) (DashboardDTO, error) {
	scope := ""
	if user.Role != "admin" {
		scope = user.ID
	}
	d, err := s.repo.Dashboard(ctx, scope)
	if err != nil {
		return DashboardDTO{}, err
	}
	ps, err := s.repo.ListProviders(ctx)
	if err != nil {
		return DashboardDTO{}, err
	}
	orders, err := s.repo.ListOrders(ctx, scope, 5, 0)
	if err != nil {
		return DashboardDTO{}, err
	}
	out := DashboardDTO{ActiveOrders: d.ActiveOrders, TodayOrders: d.TodayOrders, TodayMessages: d.TodaySMS, TodaySpend: strconv.FormatFloat(d.TodayCost, 'f', -1, 64), Currency: "USD", ProviderTotal: len(ps), RecentOrders: make([]OrderDTO, 0, len(orders)), Providers: make([]ProviderHealthDTO, 0, len(ps))}
	for _, p := range ps {
		healthy := p.Enabled && p.APIKeyConfigured
		if healthy {
			out.ProviderHealthy++
		}
		message := ""
		if p.Enabled && !p.APIKeyConfigured {
			message = "尚未配置 API Key"
		}
		out.Providers = append(out.Providers, ProviderHealthDTO{Code: p.ID, Name: p.Name, Enabled: p.Enabled, Healthy: healthy, Message: message})
	}
	for _, o := range orders {
		p, _ := s.repo.GetProvider(ctx, o.ProviderID)
		out.RecentOrders = append(out.RecentOrders, OrderView(o, readSettings(p.Config).WebhookEnabled))
	}
	return out, nil
}

func (s *Service) Users(ctx context.Context) ([]UserDTO, error) {
	us, err := s.repo.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UserDTO, 0, len(us))
	for _, u := range us {
		out = append(out, UserView(u))
	}
	return out, nil
}
func (s *Service) CreateUser(ctx context.Context, in SaveUserInput, actor domain.User, ip string) (UserDTO, error) {
	if strings.TrimSpace(in.Username) == "" || !validRole(in.Role) || strings.TrimSpace(in.Password) == "" {
		return UserDTO{}, ErrBadRequest
	}
	hash, err := s.auth.HashPassword(in.Password)
	if err != nil {
		return UserDTO{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	u := domain.User{ID: identity.UUID(), Username: strings.TrimSpace(in.Username), DisplayName: strings.TrimSpace(in.DisplayName), PasswordHash: hash, Role: in.Role, Active: in.Enabled}
	if err = s.repo.CreateUser(ctx, u); err != nil {
		return UserDTO{}, mapStore(err)
	}
	_ = s.repo.Audit(ctx, &actor.ID, "user.create", "user", u.ID, ip, nil)
	u, err = s.repo.GetUser(ctx, u.ID)
	return UserView(u), err
}
func (s *Service) UpdateUser(ctx context.Context, id string, in SaveUserInput, actor domain.User, ip string) (UserDTO, error) {
	u, err := s.repo.GetUser(ctx, id)
	if err != nil {
		return UserDTO{}, mapStore(err)
	}
	if strings.TrimSpace(in.Username) == "" || !validRole(in.Role) {
		return UserDTO{}, ErrBadRequest
	}
	if u.ID == actor.ID && !in.Enabled {
		return UserDTO{}, ErrConflict
	}
	u.Username = strings.TrimSpace(in.Username)
	u.DisplayName = strings.TrimSpace(in.DisplayName)
	u.Role = in.Role
	disabling := u.Active && !in.Enabled
	if disabling {
		if err = s.repo.RevokeUserSessions(ctx, u.ID); err != nil {
			return UserDTO{}, err
		}
	}
	u.Active = in.Enabled
	if err = s.repo.UpdateUser(ctx, u); err != nil {
		return UserDTO{}, mapStore(err)
	}
	if in.Password != "" {
		hash, e := s.auth.HashPassword(in.Password)
		if e != nil {
			return UserDTO{}, fmt.Errorf("%w: %v", ErrBadRequest, e)
		}
		if e = s.repo.UpdatePasswordAndRevoke(ctx, u.ID, hash); e != nil {
			return UserDTO{}, e
		}
	}
	_ = s.repo.Audit(ctx, &actor.ID, "user.update", "user", u.ID, ip, nil)
	u, err = s.repo.GetUser(ctx, id)
	return UserView(u), err
}
func (s *Service) ChangePassword(ctx context.Context, user domain.User, current, next, ip string) error {
	fresh, err := s.repo.GetUser(ctx, user.ID)
	if err != nil {
		return mapStore(err)
	}
	if !s.auth.CheckPassword(fresh.PasswordHash, current) {
		return ErrBadRequest
	}
	hash, err := s.auth.HashPassword(next)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if err = s.repo.UpdatePasswordAndRevoke(ctx, user.ID, hash); err != nil {
		return err
	}
	return s.repo.Audit(ctx, &user.ID, "password.change", "user", user.ID, ip, nil)
}
func (s *Service) Health(ctx context.Context) error { return s.repo.Ping(ctx) }
func (s *Service) CaptchaAllowed(ctx context.Context, ip string) (bool, error) {
	return s.repo.CaptchaAllowed(ctx, ip, s.now(), time.Minute, 30)
}

func (s *Service) providerClient(ctx context.Context, id string) (domain.Provider, string, provider.Client, error) {
	p, err := s.repo.GetProvider(ctx, domain.NormalizeProvider(id))
	if err != nil {
		return domain.Provider{}, "", nil, mapStore(err)
	}
	if s.config.Environment == "production" {
		if _, err = validateProviderURL(ctx, p.ID, p.BaseURL, true); err != nil {
			return domain.Provider{}, "", nil, ErrBadRequest
		}
	}
	key, err := s.vault.Decrypt(p.APIKeyCipher)
	if err != nil {
		return domain.Provider{}, "", nil, err
	}
	if key == "" {
		return domain.Provider{}, "", nil, ErrConflict
	}
	client, err := provider.New(p.ID, p.BaseURL)
	if err != nil {
		return domain.Provider{}, "", nil, ErrBadRequest
	}
	return p, key, client, nil
}
func readSettings(raw json.RawMessage) providerSettings {
	var v providerSettings
	_ = json.Unmarshal(raw, &v)
	if v.PollingIntervalSeconds == 0 {
		v.PollingIntervalSeconds = 30
	}
	return v
}
func clampInterval(v int) int {
	if v < 5 {
		return 5
	}
	if v > 600 {
		return 600
	}
	return v
}
func formatPrice(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}
func validRole(v string) bool { return v == "admin" || v == "operator" }
func mapStore(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, store.ErrConflict) {
		return ErrConflict
	}
	return err
}

func validateProviderURL(ctx context.Context, providerID, raw string, production bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, ErrBadRequest
	}
	if !production {
		return u, nil
	}
	if u.Scheme != "https" || u.Port() != "" {
		return nil, ErrBadRequest
	}
	allowed := map[string]string{domain.ProviderHeroSMS: "hero-sms.com", domain.ProviderSMSBower: "smsbower.page", domain.ProviderSMSPool: "api.smspool.net"}[domain.NormalizeProvider(providerID)]
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if allowed == "" || (host != allowed && !strings.HasSuffix(host, "."+allowed)) {
		return nil, ErrBadRequest
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil || len(addresses) == 0 {
		return nil, ErrBadRequest
	}
	for _, address := range addresses {
		ip := address.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return nil, ErrBadRequest
		}
	}
	return u, nil
}

// Webhook 校验供应商级随机 token，并在同一数据库事务中保存原始事件与短信。
// 重复回调始终成功应答，避免供应商无意义重试。
func (s *Service) Webhook(ctx context.Context, pid, token string, payload, headers json.RawMessage) error {
	pid = domain.NormalizeProvider(pid)
	p, err := s.repo.GetProvider(ctx, pid)
	if err != nil {
		return ErrNotFound
	}
	expected, err := s.vault.Decrypt(p.WebhookTokenCipher)
	if err != nil {
		return err
	}
	if len(expected) == 0 || len(token) != len(expected) || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		return ErrNotFound
	}
	now := s.now().UTC()
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	decodeErr := decoder.Decode(&object)
	canonical := payload
	if decodeErr == nil {
		canonical, _ = json.Marshal(object)
	}
	eventFP := digestHex(pid, string(canonical))
	w := store.WebhookRecord{ID: identity.UUID(), ProviderID: pid, Fingerprint: eventFP, Headers: headers, Payload: append(json.RawMessage(nil), payload...), Status: "processed", ReceivedAt: now}
	if decodeErr != nil {
		w.Status = "rejected"
		w.Error = "invalid_json"
		_, _ = s.repo.SaveWebhookEvent(ctx, w)
		return ErrBadRequest
	}
	upstream := scalar(object, "activationId", "activation_id", "orderid", "orderId", "id")
	w.UpstreamID = upstream
	if upstream == "" {
		w.Status = "rejected"
		w.Error = "missing_order_id"
		_, _ = s.repo.SaveWebhookEvent(ctx, w)
		return ErrBadRequest
	}
	code := scalar(object, "code", "sms", "smsCode")
	text := scalar(object, "text", "full_sms", "fullSms", "smsText")
	if code == "" && text == "" {
		w.Status = "rejected"
		w.Error = "missing_message"
		_, _ = s.repo.SaveWebhookEvent(ctx, w)
		return ErrBadRequest
	}
	o, err := s.repo.FindOrderByUpstream(ctx, pid, upstream)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.Status = "ignored"
			w.Error = "order_not_found"
			if _, saveErr := s.repo.SaveWebhookEvent(ctx, w); saveErr != nil {
				return fmt.Errorf("save unknown webhook event: %w", saveErr)
			}
			return nil
		}
		return fmt.Errorf("find webhook order: %w", err)
	}
	if o.Terminal() {
		w.Status = "ignored"
		w.Error = "order_terminal"
		_, _ = s.repo.SaveWebhookEvent(ctx, w)
		return nil
	}
	if service := scalar(object, "service"); service != "" && service != o.ServiceCode {
		w.Status = "rejected"
		w.Error = "service_mismatch"
		_, _ = s.repo.SaveWebhookEvent(ctx, w)
		return nil
	}
	if country := scalar(object, "country"); country != "" && country != o.CountryCode {
		w.Status = "rejected"
		w.Error = "country_mismatch"
		_, _ = s.repo.SaveWebhookEvent(ctx, w)
		return nil
	}
	received, hasTime := parseWebhookTime(value(object, "receivedAt", "received_at", "timestamp"))
	if !hasTime {
		received = now
	}
	state := messageState(code, text)
	// SMSPool/SMS-Activate 的状态轮询有时不返回消息 ID 或接收时间。若同一
	// 内容刚刚由轮询入库，后到的 webhook 只保留审计事件；当下一轮同码确实
	// 再次到达时，其供应商时间会晚于上一轮更新时间，仍可作为新消息保存。
	if state == o.LastProviderState && (!hasTime || !received.After(o.UpdatedAt)) {
		w.Status = "ignored"
		w.Error = "message_already_reconciled"
		_, err = s.repo.SaveWebhookEvent(ctx, w)
		return err
	}
	messageFP := messageFingerprint(o, received, hasTime, code, text)
	w.ProviderState = state
	m := domain.SMSMessage{ID: identity.UUID(), OrderID: o.ID, ProviderID: pid, Code: code, Text: text, Source: "webhook", UpstreamFingerprint: messageFP, ReceivedAt: received}
	inserted, err := s.repo.SaveWebhookMessage(ctx, w, m)
	if err != nil {
		return err
	}
	if inserted {
		select {
		case s.afterMessage <- o:
		default:
			slog.Warn("后续接码通知队列已满", "order_id", o.ID)
		}
	}
	return nil
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	maintenanceTicker := time.NewTicker(time.Hour)
	defer maintenanceTicker.Stop()
	jobs := make(chan func(), 128)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-jobs:
					if job != nil {
						job()
					}
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case recovery := <-s.accountingRecovery:
				s.recoverAccounting(ctx, recovery)
			}
		}
	}()
	submit := func(job func()) {
		select {
		case jobs <- job:
		case <-ctx.Done():
		default:
			slog.Warn("后台任务队列已满，任务将由数据库租约重试")
		}
	}
	s.pollBatch(ctx, submit)
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case <-jobs:
					continue
				default:
					goto drained
				}
			}
		drained:
			workers.Wait()
			return
		case o := <-s.afterMessage:
			order := o
			submit(func() { s.requestAnother(ctx, order) })
		case <-ticker.C:
			s.pollBatch(ctx, submit)
		case <-maintenanceTicker.C:
			submit(func() {
				maintenanceCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				if err := s.repo.Maintenance(maintenanceCtx, s.now()); err != nil {
					slog.Warn("后台数据维护失败", "error", err)
				}
			})
		}
	}
}
func (s *Service) pollBatch(ctx context.Context, submit func(func())) {
	orders, err := s.repo.ClaimDueOrders(ctx, 20, s.now(), 5*time.Minute)
	if err != nil {
		slog.Error("领取轮询任务失败", "error", err)
		return
	}
	for _, o := range orders {
		if ctx.Err() != nil {
			return
		}
		order := o
		submit(func() { s.pollOne(ctx, order) })
	}
}
func (s *Service) pollOne(ctx context.Context, o domain.Order) {
	if o.ExpiresAt != nil && !o.ExpiresAt.After(s.now()) {
		s.transitionPolledOrder(ctx, o.ID, domain.OrderExpired, "local_expired")
		return
	}
	p, key, client, err := s.providerClient(ctx, o.ProviderID)
	if err != nil {
		s.pollFailure(ctx, o, "configuration")
		return
	}
	result, err := client.Poll(ctx, key, o.UpstreamID)
	if err != nil {
		s.pollFailure(ctx, o, "provider_error")
		return
	}
	terminalStatus := ""
	switch result.State {
	case provider.PollCanceled, provider.PollRefunded:
		terminalStatus = domain.OrderCanceled
	case provider.PollExpired:
		terminalStatus = domain.OrderExpired
	case provider.PollCompleted:
		terminalStatus = domain.OrderCompleted
	}
	messages := result.Messages
	if len(messages) == 0 && (result.Code != "" || result.Text != "") {
		messages = []provider.OTPMessage{{Code: result.Code, Text: result.Text}}
	}
	state := result.State
	if result.State == provider.PollReceived && o.LastProviderState != "" {
		state = o.LastProviderState
	}
	if result.State == provider.PollWaitingRetry && result.LastCode != "" {
		state = provider.PollWaitingRetry + ":" + digestHex(result.LastCode)
	}
	sequence := o.PollSequence
	insertedAny := false
	for _, up := range messages {
		received := up.ReceivedAt
		hasTime := !received.IsZero()
		if !hasTime {
			received = s.now().UTC()
		}
		signature := messageState(up.Code, up.Text)
		if !hasTime && signature == o.LastProviderState && strings.TrimSpace(up.Fingerprint) == "" && strings.TrimSpace(up.UpstreamID) == "" {
			continue
		}
		fp := ""
		upstreamFingerprint := strings.TrimSpace(up.Fingerprint)
		if upstreamFingerprint == "" {
			upstreamFingerprint = strings.TrimSpace(up.UpstreamID)
		}
		if upstreamFingerprint != "" {
			fp = digestHex(o.ProviderID, o.UpstreamID, "upstream_message", upstreamFingerprint, strconv.Itoa(up.Generation))
		} else {
			fp = messageFingerprintWithSequence(o, received, hasTime, up.Code, up.Text, sequence)
		}
		m := domain.SMSMessage{ID: identity.UUID(), OrderID: o.ID, ProviderID: o.ProviderID, Code: up.Code, Text: up.Text, Source: "poll", UpstreamFingerprint: fp, ReceivedAt: received}
		inserted, e := s.repo.SaveMessage(ctx, m, true)
		if e != nil {
			s.pollFailure(ctx, o, "database_error")
			return
		}
		if inserted {
			insertedAny = true
			sequence++
			state = signature
		}
	}
	if terminalStatus != "" {
		s.transitionPolledOrder(ctx, o.ID, terminalStatus, result.State)
		return
	}
	interval := time.Duration(readSettings(p.Config).PollingIntervalSeconds) * time.Second
	if o.ProviderID != domain.ProviderSMSPool && interval < 30*time.Second {
		interval = 30 * time.Second
	}
	_ = s.repo.UpdatePoll(ctx, o.ID, state, s.now().Add(interval), 0)
	if insertedAny && result.CanRequestAnother {
		o.PollSequence = sequence
		o.RequestNextPending = true
		o.RequestNextFailures = 0
	}
	if o.RequestNextPending || insertedAny && result.CanRequestAnother {
		s.requestAnother(ctx, o)
	}
}

func (s *Service) transitionPolledOrder(ctx context.Context, id, status, state string) {
	err := s.repo.WithOrderLock(ctx, id, func(lockCtx context.Context) error {
		fresh, err := s.repo.GetOrder(lockCtx, id, "")
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if fresh.Status != domain.OrderActive {
			return nil
		}
		err = s.repo.SetOrderStatus(lockCtx, id, status, state)
		if errors.Is(err, store.ErrConflict) {
			return nil
		}
		return err
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		slog.Warn("轮询终态写入失败", "order_id", id, "state", state, "error", err)
	}
}

func (s *Service) requestAnother(parent context.Context, o domain.Order) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	err := s.repo.WithOrderLock(ctx, o.ID, func(lockCtx context.Context) error {
		fresh, err := s.repo.GetOrder(lockCtx, o.ID, "")
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if fresh.Terminal() || !fresh.RequestNextPending || fresh.RequestNextInflight {
			return nil
		}
		claimed, err := s.repo.ClaimRequestNext(lockCtx, fresh.ID)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
		_, key, client, err := s.providerClient(lockCtx, fresh.ProviderID)
		if err != nil {
			s.restoreRequestNext(fresh)
			return nil
		}
		result, err := client.RequestAnother(lockCtx, key, fresh.UpstreamID)
		if err != nil {
			slog.Warn("请求继续接收短信失败", "provider", fresh.ProviderID, "order_id", fresh.ID)
			s.restoreRequestNext(fresh)
			return nil
		}
		applied, err := s.finalizeRequestNext(fresh.ID, result.Charge)
		if err != nil {
			// 远端已成功时绝不恢复 pending；inflight 留在数据库中，阻止再次收费。
			slog.Error("续码成功但本地记账失败，订单保持 inflight", "order_id", fresh.ID, "error", err)
			s.enqueueAccountingRecovery(accountingRecovery{OrderID: fresh.ID, Charge: result.Charge})
			return nil
		}
		if applied {
			s.auditRequestNext(fresh.ID, result.Charge)
		}
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		slog.Warn("继续接码任务加锁失败", "order_id", o.ID, "error", err)
	}
}

func (s *Service) enqueueAccountingRecovery(recovery accountingRecovery) {
	select {
	case s.accountingRecovery <- recovery:
		return
	default:
		slog.Error("续码记账恢复队列已满，启动受控兜底", "order_id", recovery.OrderID)
	}
	select {
	case s.accountingFallbackSlots <- struct{}{}:
		go func() {
			defer func() { <-s.accountingFallbackSlots }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			s.recoverAccounting(ctx, recovery)
		}()
	default:
		slog.Error("续码记账兜底已满，订单保持 inflight 等待人工处理", "order_id", recovery.OrderID)
	}
}

func (s *Service) recoverAccounting(ctx context.Context, recovery accountingRecovery) {
	delay := 250 * time.Millisecond
	for {
		operationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		applied, err := s.repo.CompleteRequestNext(operationCtx, recovery.OrderID, recovery.Charge)
		cancel()
		if err == nil {
			if applied {
				s.auditRequestNext(recovery.OrderID, recovery.Charge)
			}
			return
		}
		slog.Warn("续码记账恢复重试失败", "order_id", recovery.OrderID, "error", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func (s *Service) auditRequestNext(orderID string, charge float64) {
	meta, _ := json.Marshal(map[string]any{"charge": charge})
	auditCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.repo.Audit(auditCtx, nil, "order.request_next", "order", orderID, "", meta); err != nil {
		slog.Warn("续码记账审计写入失败", "order_id", orderID, "error", err)
	}
}
func (s *Service) restoreRequestNext(o domain.Order) {
	fail := o.RequestNextFailures + 1
	delay := time.Duration(1<<min(fail, 6)) * time.Second
	if delay < 5*time.Second {
		delay = 5 * time.Second
	}
	if delay > 2*time.Minute {
		delay = 2 * time.Minute
	}
	restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.repo.RestoreRequestNext(restoreCtx, o.ID, fail, s.now().Add(delay)); err != nil {
		slog.Error("续码失败状态恢复失败，订单保持 inflight", "order_id", o.ID, "error", err)
	}
}

func (s *Service) finalizeRequestNext(id string, charge float64) (bool, error) {
	finalizeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		applied, err := s.repo.CompleteRequestNext(finalizeCtx, id, charge)
		if err == nil {
			return applied, nil
		}
		last = err
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
			select {
			case <-finalizeCtx.Done():
				timer.Stop()
				return false, finalizeCtx.Err()
			case <-timer.C:
			}
		}
	}
	return false, last
}
func (s *Service) pollFailure(ctx context.Context, o domain.Order, state string) {
	fail := o.PollFailures + 1
	delay := time.Duration(1<<min(fail, 6)) * time.Second
	if delay < 5*time.Second {
		delay = 5 * time.Second
	}
	if delay > 2*time.Minute {
		delay = 2 * time.Minute
	}
	_ = s.repo.UpdatePoll(ctx, o.ID, state, s.now().Add(delay), fail)
}

func scalar(m map[string]any, keys ...string) string {
	v := value(m, keys...)
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	default:
		return ""
	}
}
func value(m map[string]any, keys ...string) any {
	for _, k := range keys {
		for actual, v := range m {
			if strings.EqualFold(actual, k) {
				return v
			}
		}
	}
	return nil
}
func parseWebhookTime(v any) (time.Time, bool) {
	if n, ok := v.(json.Number); ok {
		seconds, e := n.Int64()
		if e == nil {
			if seconds > 1_000_000_000_000 {
				seconds /= 1000
			}
			return time.Unix(seconds, 0).UTC(), true
		}
	}
	text := ""
	if x, ok := v.(string); ok {
		text = strings.TrimSpace(x)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 03:04:05pm", "2006-01-02 03:04:05PM"} {
		if t, e := time.Parse(layout, text); e == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
func messageState(code, text string) string {
	return provider.PollReceived + ":" + digestHex(code, text)
}
func messageFingerprint(o domain.Order, received time.Time, hasTime bool, code, text string) string {
	return messageFingerprintWithSequence(o, received, hasTime, code, text, o.PollSequence)
}
func messageFingerprintWithSequence(o domain.Order, received time.Time, hasTime bool, code, text string, sequence int64) string {
	generation := strconv.FormatInt(sequence, 10)
	if hasTime {
		generation = received.UTC().Format(time.RFC3339Nano)
	}
	return digestHex(o.ProviderID, o.UpstreamID, generation, code, text)
}
func digestHex(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}
