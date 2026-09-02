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
	"net/http"
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

// PurchaseError 是购买接口的稳定错误契约。Kind 保留对通用错误的
// errors.Is 兼容，Code 则供 HTTP 客户端作稳定判断。
type PurchaseError struct {
	Code    string
	Message string
	Kind    error
	Cause   error
}

const (
	purchaseUnknownProviderTimeout    = "provider_timeout_unknown"
	purchaseUnknownProviderConnection = "provider_connection_unknown"
	purchaseUnknownProviderHTTP       = "provider_http_unknown"
	purchaseUnknownProviderRead       = "provider_read_unknown"
	purchaseUnknownProviderResponse   = "provider_response_unknown"
	purchaseUnknownStaleProvisioning  = "stale_provisioning_unknown"
)

func (e *PurchaseError) Error() string { return e.Message }

func (e *PurchaseError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.Kind != nil {
		errs = append(errs, e.Kind)
	}
	if e.Cause != nil && !errors.Is(e.Cause, e.Kind) {
		errs = append(errs, e.Cause)
	}
	return errs
}

func purchaseError(code string, cause error) *PurchaseError {
	err := &PurchaseError{Code: code, Cause: cause}
	switch code {
	case "idempotency_mismatch":
		err.Message, err.Kind = "该购买编号已用于其他条件，页面将生成新的购买请求", ErrConflict
	case "purchase_in_progress":
		err.Message, err.Kind = "购买请求仍在处理中，系统尚未收到最终结果；请等待最多2分钟，再使用当前请求重试确认；为避免重复扣费，仅暂停该请求的重复提交", ErrConflict
	case "purchase_result_unknown":
		return purchaseResultUnknownError("", cause)
	case "price_exceeded":
		err.Message, err.Kind = "供应商实际价格超过所选价格，购买已取消", ErrConflict
	case "no_numbers":
		err.Message, err.Kind = "所选条件当前暂无可用号码，请稍后重试或调整条件", ErrConflict
	case "insufficient_balance":
		err.Message, err.Kind = "供应商账户余额不足，请联系管理员充值", ErrProvider
	case "invalid_selection":
		err.Message, err.Kind = "供应商不支持所选国家或服务，请重新选择", ErrBadRequest
	case "provider_rate_limited":
		err.Message, err.Kind = "供应商请求过于频繁，请稍后重试", ErrProvider
	case "provider_disabled":
		err.Message, err.Kind = "所选供应商已停用，请选择其他供应商", ErrConflict
	case "provider_error":
		return providerPurchaseError("provider_error", cause)
	case "provider_preflight_error":
		err.Message, err.Kind = "购买前获取供应商资源失败，号码购买尚未提交；可以重新提交当前平台与购买条件", ErrProvider
	case "configuration":
		err.Message, err.Kind = "供应商配置不完整，请联系管理员", ErrProvider
	case "purchase_setup_failed":
		err.Message = "购买准备失败，请稍后重试"
	case "database_error":
		err.Message = purchaseResultUnknownError("database_error", nil).Message
	default:
		err.Code = "purchase_failed"
		err.Message, err.Kind = "购买请求已失败，请刷新页面后重试", ErrConflict
	}
	return err
}

func purchaseResultUnknownError(reason string, cause error) *PurchaseError {
	err := &PurchaseError{Code: "purchase_result_unknown", Kind: ErrConflict, Cause: cause}
	switch reason {
	case purchaseUnknownProviderTimeout:
		err.Message = "购买结果尚未确认：供应商请求超时，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	case purchaseUnknownProviderConnection:
		err.Message = "购买结果尚未确认：与供应商的连接中断或请求被取消，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	case purchaseUnknownProviderHTTP:
		err.Message = "购买结果尚未确认：供应商返回服务异常或限流响应，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	case purchaseUnknownProviderRead:
		err.Message = "购买结果尚未确认：读取供应商响应时中断，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	case purchaseUnknownProviderResponse:
		err.Message = "购买结果尚未确认：供应商响应格式异常或内容过大，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	case purchaseUnknownStaleProvisioning:
		err.Message = "购买结果尚未确认：上一次请求处理已中断或状态更新失败，系统无法确认供应商是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	case "provider_error":
		err.Message = "购买结果尚未确认：供应商请求超时、连接中断或响应异常，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	case "price_cancel_unknown":
		err.Message = "购买结果尚未确认：供应商返回的价格超过所选价格，但取消结果未确认；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	case "database_error":
		err.Message = "购买结果尚未确认：供应商已返回号码，但本地订单保存结果未确认，系统无法判断订单是否已记录或号码是否已取消；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	default:
		err.Message = "购买结果尚未确认：系统未能确定供应商是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交"
	}
	return err
}

func providerPurchaseError(reason string, cause error) *PurchaseError {
	err := purchaseResultUnknownError(reason, cause)
	err.Code = "provider_error"
	err.Kind = ErrProvider
	return err
}

func providerPurchaseUnknownReason(err error) string {
	var upstream *provider.ProviderError
	if errors.As(err, &upstream) {
		if upstream.HTTPStatus >= http.StatusInternalServerError || upstream.HTTPStatus == http.StatusTooManyRequests {
			return purchaseUnknownProviderHTTP
		}
		switch strings.ToUpper(strings.TrimSpace(upstream.Code)) {
		case "TIMEOUT":
			return purchaseUnknownProviderTimeout
		case "TRANSPORT_ERROR", "NO_CONNECTION", "CANCELED":
			return purchaseUnknownProviderConnection
		case "READ_ERROR":
			return purchaseUnknownProviderRead
		case "INVALID_RESPONSE", "RESPONSE_TOO_LARGE":
			return purchaseUnknownProviderResponse
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return purchaseUnknownProviderTimeout
	}
	if errors.Is(err, context.Canceled) {
		return purchaseUnknownProviderConnection
	}
	return "provider_error"
}

func purchaseAttemptErrorCode(status, reason string) string {
	if status != "unknown" {
		return reason
	}
	switch reason {
	case purchaseUnknownProviderTimeout, purchaseUnknownProviderConnection, purchaseUnknownProviderHTTP, purchaseUnknownProviderRead, purchaseUnknownProviderResponse:
		return "provider_error"
	default:
		return reason
	}
}

type Service struct {
	repo                    store.Repository
	auth                    *auth.Service
	vault                   *secure.Vault
	config                  config.Config
	afterMessage            chan domain.Order
	accountingRecovery      chan accountingRecovery
	accountingFallbackSlots chan struct{}
	now                     func() time.Time
	balanceMu               sync.Mutex
	balanceCache            map[string]providerBalanceCacheEntry
	balanceCalls            map[string]*providerBalanceCall
	balanceEpoch            map[string]uint64
}

type providerBalanceCacheEntry struct {
	result    provider.BalanceResult
	checkedAt time.Time
	err       error
	expiresAt time.Time
}

type providerBalanceCall struct {
	done      chan struct{}
	result    provider.BalanceResult
	checkedAt time.Time
	err       error
}

type accountingRecovery struct {
	OrderID string
	Charge  float64
}

func New(repo store.Repository, authentication *auth.Service, vault *secure.Vault, cfg config.Config) *Service {
	return &Service{
		repo: repo, auth: authentication, vault: vault, config: cfg,
		afterMessage: make(chan domain.Order, 128), accountingRecovery: make(chan accountingRecovery, 128),
		accountingFallbackSlots: make(chan struct{}, 2), now: time.Now,
		balanceCache: make(map[string]providerBalanceCacheEntry), balanceCalls: make(map[string]*providerBalanceCall),
		balanceEpoch: make(map[string]uint64),
	}
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

func (s *Service) ProviderBalances(ctx context.Context, user domain.User) ([]ProviderBalanceDTO, error) {
	ps, err := s.repo.ListProviders(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]ProviderBalanceDTO, len(ps))
	var wait sync.WaitGroup
	for index := range ps {
		p := ps[index]
		configured := p.APIKeyConfigured || len(p.APIKeyCipher) > 0
		out[index] = ProviderBalanceDTO{Code: p.ID, Name: p.Name, Enabled: p.Enabled, Purchasable: p.Enabled && configured}
		if !p.Enabled {
			out[index].Status = "disabled"
			out[index].Message = "接口未启用"
			continue
		}
		if !configured {
			if user.Role == "admin" {
				out[index].Status = "unconfigured"
				out[index].Message = "尚未配置 API Key"
			} else {
				checkedAt := s.now().UTC()
				out[index].LastCheckedAt = &checkedAt
				out[index].Status = "unavailable"
				out[index].Message = "余额暂不可用"
			}
			continue
		}

		wait.Add(1)
		go func() {
			defer wait.Done()
			balance, checkedAt, balanceErr := s.providerBalance(ctx, p)
			out[index].LastCheckedAt = &checkedAt
			if balanceErr != nil {
				if providerBalanceConfigurationError(balanceErr) {
					out[index].Purchasable = false
					if user.Role == "admin" {
						out[index].Status = "unconfigured"
						out[index].Message = "供应商配置不可用"
					} else {
						out[index].Status = "unavailable"
						out[index].Message = "余额暂不可用"
					}
					return
				}
				if errors.Is(balanceErr, context.DeadlineExceeded) {
					out[index].Status = "timeout"
					out[index].Message = "余额查询超时"
				} else {
					out[index].Status = "unavailable"
					out[index].Message = "余额获取失败"
				}
				return
			}
			out[index].Status = "ok"
			out[index].Balance = balance.Amount
			out[index].Currency = balance.Currency
		}()
	}
	wait.Wait()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func providerBalanceConfigurationError(err error) bool {
	if errors.Is(err, ErrBadRequest) || errors.Is(err, ErrConflict) || errors.Is(err, provider.ErrInvalidRequest) {
		return true
	}
	var upstream *provider.ProviderError
	if !errors.As(err, &upstream) {
		return false
	}
	if upstream.HTTPStatus == http.StatusUnauthorized || upstream.HTTPStatus == http.StatusForbidden {
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(upstream.Code)) {
	case "BAD_KEY", "ACCOUNT_INACTIVE", "BANNED", "INVALID_BASE_URL", "INVALID_REQUEST":
		return true
	default:
		return false
	}
}

const (
	providerBalanceFreshTTL   = 5 * time.Second
	providerBalanceFailureTTL = 2 * time.Second
	providerBalanceTimeout    = 5 * time.Second
)

func (s *Service) providerBalance(ctx context.Context, p domain.Provider) (provider.BalanceResult, time.Time, error) {
	now := s.now().UTC()
	s.balanceMu.Lock()
	epoch := s.balanceEpoch[p.ID]
	cacheKey := fmt.Sprintf("%s|%d|%d", p.ID, epoch, p.UpdatedAt.UnixNano())
	if cached, found := s.balanceCache[cacheKey]; found && now.Before(cached.expiresAt) {
		s.balanceMu.Unlock()
		return cached.result, cached.checkedAt, cached.err
	}
	if call, found := s.balanceCalls[cacheKey]; found {
		s.balanceMu.Unlock()
		select {
		case <-call.done:
			return call.result, call.checkedAt, call.err
		case <-ctx.Done():
			return provider.BalanceResult{}, now, ctx.Err()
		}
	}
	call := &providerBalanceCall{done: make(chan struct{})}
	s.balanceCalls[cacheKey] = call
	s.balanceMu.Unlock()

	checkedAt := s.now().UTC()
	queryCtx, cancel := context.WithTimeout(context.Background(), providerBalanceTimeout)
	result, queryErr := s.fetchProviderBalance(queryCtx, p)
	cancel()
	ttl := providerBalanceFreshTTL
	if queryErr != nil {
		ttl = providerBalanceFailureTTL
	}

	s.balanceMu.Lock()
	call.result, call.checkedAt, call.err = result, checkedAt, queryErr
	if s.balanceEpoch[p.ID] == epoch {
		s.balanceCache[cacheKey] = providerBalanceCacheEntry{
			result: result, checkedAt: checkedAt, err: queryErr, expiresAt: s.now().UTC().Add(ttl),
		}
	}
	delete(s.balanceCalls, cacheKey)
	close(call.done)
	s.balanceMu.Unlock()
	return result, checkedAt, queryErr
}

func (s *Service) fetchProviderBalance(ctx context.Context, p domain.Provider) (provider.BalanceResult, error) {
	key, err := s.vault.Decrypt(p.APIKeyCipher)
	if err != nil || strings.TrimSpace(key) == "" {
		return provider.BalanceResult{}, ErrConflict
	}
	if s.config.Environment == "production" {
		if _, err = validateProviderURL(ctx, p.ID, p.BaseURL, true); err != nil {
			return provider.BalanceResult{}, ErrBadRequest
		}
	}
	client, err := provider.New(p.ID, p.BaseURL, provider.WithTimeout(providerBalanceTimeout))
	if err != nil {
		return provider.BalanceResult{}, err
	}
	return client.Balance(ctx, key)
}

func (s *Service) invalidateProviderBalance(providerID string) {
	providerID = domain.NormalizeProvider(providerID)
	s.balanceMu.Lock()
	s.balanceEpoch[providerID]++
	prefix := providerID + "|"
	for key := range s.balanceCache {
		if strings.HasPrefix(key, prefix) {
			delete(s.balanceCache, key)
		}
	}
	s.balanceMu.Unlock()
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
	s.invalidateProviderBalance(p.ID)
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
	configured := p.APIKeyConfigured || len(p.APIKeyCipher) > 0
	return ProviderDTO{ID: p.ID, Code: p.ID, Name: p.Name, APIBaseURL: p.BaseURL, Enabled: p.Enabled, PollingIntervalSeconds: settings.PollingIntervalSeconds, WebhookSupported: true, WebhookEnabled: settings.WebhookEnabled, HasAPIKey: configured, Purchasable: p.Enabled && configured, HasWebhookToken: p.WebhookConfigured, WebhookURL: s.config.PublicBaseURL + "/api/webhooks/" + p.ID + "/" + url.PathEscape(token), UpdatedAt: p.UpdatedAt}, nil
}

func (s *Service) Countries(ctx context.Context, pid, service, tier string) ([]CountryDTO, error) {
	service = strings.TrimSpace(service)
	tier, err := normalizeQualityTier(pid, tier)
	if err != nil || (tier != "" && service == "") {
		return nil, ErrBadRequest
	}
	request := provider.CatalogRequest{Kind: provider.CatalogCountry, Service: service, QualityTier: tier}
	items, err := s.catalog(ctx, pid, request)
	if err != nil {
		return nil, err
	}
	out := make([]CountryDTO, 0, len(items))
	for _, x := range items {
		out = append(out, CountryDTO{Code: x.Code, Name: x.Name, Available: x.Stock, PriceFrom: formatPrice(x.Price)})
	}
	return out, nil
}
func (s *Service) Services(ctx context.Context, pid, country, tier string) ([]ServiceDTO, error) {
	country = strings.TrimSpace(country)
	tier, err := normalizeQualityTier(pid, tier)
	if err != nil {
		return nil, err
	}
	items, err := s.catalog(ctx, pid, provider.CatalogRequest{Kind: provider.CatalogService, Country: country, QualityTier: tier})
	if err != nil {
		return nil, err
	}
	out := make([]ServiceDTO, 0, len(items))
	for _, x := range items {
		out = append(out, ServiceDTO{Code: x.Code, Name: x.Name, Available: x.Stock, Price: formatPrice(x.Price)})
	}
	return out, nil
}
func (s *Service) Quote(ctx context.Context, pid, country, service, tier string) (QuoteDTO, error) {
	country = strings.TrimSpace(country)
	service = strings.TrimSpace(service)
	if country == "" || service == "" {
		return QuoteDTO{}, ErrBadRequest
	}
	tier, err := normalizeQualityTier(pid, tier)
	if err != nil {
		return QuoteDTO{}, err
	}
	items, err := s.catalog(ctx, pid, provider.CatalogRequest{Kind: provider.CatalogPrice, Country: country, Service: service, QualityTier: tier})
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
			priceOptions := quotePriceOptions(x, price, stock)
			return QuoteDTO{Provider: domain.NormalizeProvider(pid), ProviderName: providerName(domain.NormalizeProvider(pid)), CountryCode: country, ServiceCode: service, QualityTier: tier, Price: price, Currency: "USD", Available: stock, PriceOptions: priceOptions}, nil
		}
	}
	return QuoteDTO{}, ErrNotFound
}

func quotePriceOptions(item domain.CatalogItem, fallbackPrice string, fallbackStock int) []QuotePriceOptionDTO {
	options := make([]QuotePriceOptionDTO, 0, len(item.PriceOptions))
	for _, option := range item.PriceOptions {
		options = append(options, QuotePriceOptionDTO{
			Price:     strconv.FormatFloat(option.Price, 'f', -1, 64),
			Available: option.Available,
		})
	}
	if len(options) == 0 {
		options = append(options, QuotePriceOptionDTO{Price: fallbackPrice, Available: fallbackStock})
	}
	return options
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
	if callErr == nil {
		if len(items) > 0 {
			for i := range items {
				items[i].ProviderID = pid
				items[i].Kind = req.Kind
				if items[i].Country == "" && req.Kind != provider.CatalogCountry {
					items[i].Country = req.Country
				}
				items[i].UpdatedAt = s.now()
			}
			switch catalogPersistence(req) {
			case catalogReplace:
				if err = s.repo.ReplaceCatalog(ctx, pid, req.Kind, items); err != nil {
					return nil, err
				}
			case catalogUpsert:
				if err = s.repo.UpsertCatalog(ctx, pid, catalogItemsForPersistence(req, items)); err != nil {
					return nil, err
				}
			}
			return items, nil
		}
		if req.Service != "" || req.QualityTier != "" {
			return []domain.CatalogItem{}, nil
		}
	}
	if req.QualityTier == "" && (req.Kind != provider.CatalogPrice || req.Service == "") {
		cached, cacheErr := s.repo.ListCatalog(ctx, pid, req.Kind, req.Country)
		if cacheErr == nil && len(cached) > 0 {
			return cached, nil
		}
	}
	return nil, ErrProvider
}

type catalogPersistenceMode uint8

const (
	catalogSkip catalogPersistenceMode = iota
	catalogReplace
	catalogUpsert
)

func catalogPersistence(req provider.CatalogRequest) catalogPersistenceMode {
	if req.Kind == provider.CatalogPrice {
		if req.Country != "" || req.Service != "" || req.QualityTier != "" {
			return catalogSkip
		}
		return catalogReplace
	}
	if req.Service != "" || req.QualityTier != "" {
		return catalogUpsert
	}
	return catalogReplace
}

func catalogItemsForPersistence(req provider.CatalogRequest, items []domain.CatalogItem) []domain.CatalogItem {
	if req.QualityTier == "" {
		return items
	}
	// provider_catalog 没有 tier 维度。等级国家/服务目录仅保存可读名称，
	// 不能让某档位的价格或库存覆盖通用目录。
	cached := append([]domain.CatalogItem(nil), items...)
	for i := range cached {
		cached[i].Price = nil
		cached[i].Stock = nil
		cached[i].Raw = json.RawMessage(`{}`)
	}
	return cached
}

func normalizeQualityTier(providerID, tier string) (string, error) {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" {
		return "", nil
	}
	if domain.NormalizeProvider(providerID) != domain.ProviderSMSBower {
		return "", ErrBadRequest
	}
	switch tier {
	case "gold", "silver", "bronze":
		return tier, nil
	default:
		return "", ErrBadRequest
	}
}

func (s *Service) Purchase(ctx context.Context, in PurchaseInput, user domain.User, ip string) (OrderDTO, error) {
	pid := domain.NormalizeProvider(in.Provider)
	if pid == "" || in.CountryCode == "" || in.ServiceCode == "" {
		return OrderDTO{}, ErrBadRequest
	}
	tier, err := normalizeQualityTier(pid, in.QualityTier)
	if err != nil {
		return OrderDTO{}, err
	}
	in.QualityTier = tier
	max, err := strconv.ParseFloat(in.MaxPrice, 64)
	if err != nil || max <= 0 || max > 1_000_000 || math.IsNaN(max) || math.IsInf(max, 0) || len(in.IdempotencyKey) < 16 || len(in.IdempotencyKey) > 128 {
		return OrderDTO{}, ErrBadRequest
	}
	record, created, err := s.repo.ReservePurchase(ctx, store.PurchaseRecord{ID: identity.UUID(), UserID: user.ID, IdempotencyKey: in.IdempotencyKey, ProviderID: pid, CountryCode: in.CountryCode, ServiceCode: in.ServiceCode, QualityTier: in.QualityTier, MaxPrice: max})
	if err != nil {
		return OrderDTO{}, err
	}
	if !created {
		if record.ProviderID != pid || record.CountryCode != in.CountryCode || record.ServiceCode != in.ServiceCode || record.QualityTier != in.QualityTier || math.Abs(record.MaxPrice-max) > .000001 {
			return OrderDTO{}, purchaseError("idempotency_mismatch", nil)
		}
		if record.Status == "succeeded" && record.OrderID != "" {
			return s.Order(ctx, record.OrderID, user)
		}
		switch record.Status {
		case "provisioning":
			updatedAt := record.UpdatedAt
			if updatedAt.IsZero() {
				updatedAt = record.CreatedAt
			}
			if !updatedAt.IsZero() && !s.now().Before(updatedAt.Add(2*time.Minute)) {
				s.failPurchase(record.ID, "unknown", purchaseUnknownStaleProvisioning)
				return OrderDTO{}, purchaseResultUnknownError(purchaseUnknownStaleProvisioning, nil)
			}
			return OrderDTO{}, purchaseError("purchase_in_progress", nil)
		case "unknown":
			return OrderDTO{}, purchaseResultUnknownError(record.ErrorCode, nil)
		case "failed":
			return OrderDTO{}, purchaseError(record.ErrorCode, nil)
		default:
			return OrderDTO{}, purchaseResultUnknownError(record.ErrorCode, nil)
		}
	}
	p, key, client, err := s.providerClient(ctx, pid)
	if err != nil {
		code := "purchase_setup_failed"
		if errors.Is(err, ErrBadRequest) || errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			code = "configuration"
		}
		s.failPurchase(record.ID, "failed", code)
		return OrderDTO{}, purchaseError(code, err)
	}
	if !p.Enabled {
		s.failPurchase(record.ID, "failed", "provider_disabled")
		return OrderDTO{}, purchaseError("provider_disabled", nil)
	}
	result, err := client.Purchase(ctx, key, provider.PurchaseRequest{Country: in.CountryCode, Service: in.ServiceCode, QualityTier: in.QualityTier, MaxPrice: &max})
	s.invalidateProviderBalance(pid)
	if err != nil {
		status, code := classifyProviderPurchaseError(err)
		if status == "unknown" && code == "provider_error" {
			reason := providerPurchaseUnknownReason(err)
			s.failPurchase(record.ID, status, reason)
			return OrderDTO{}, providerPurchaseError(reason, err)
		}
		s.failPurchase(record.ID, status, code)
		return OrderDTO{}, purchaseError(code, err)
	}
	if result.Cost > max+0.000001 {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cancelErr := client.Cancel(cancelCtx, key, result.UpstreamID); cancelErr != nil {
			s.failPurchase(record.ID, "unknown", "price_cancel_unknown")
			return OrderDTO{}, purchaseResultUnknownError("price_cancel_unknown", cancelErr)
		}
		s.invalidateProviderBalance(pid)
		s.failPurchase(record.ID, "failed", "price_exceeded")
		return OrderDTO{}, purchaseError("price_exceeded", nil)
	}
	o := domain.Order{ID: identity.UUID(), UserID: user.ID, ProviderID: pid, UpstreamID: result.UpstreamID, PhoneNumber: result.PhoneNumber, CountryCode: in.CountryCode, ServiceCode: in.ServiceCode, QualityTier: in.QualityTier, Status: domain.OrderActive, Cost: result.Cost, Currency: result.Currency, CanGetAnotherSMS: result.CanGetAnotherSMS, NextPollAt: s.now(), ExpiresAt: result.ExpiresAt}
	if o.Currency == "" {
		o.Currency = "USD"
	}
	if err = s.repo.CompletePurchase(ctx, record.ID, o); err != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cancelErr := client.Cancel(cancelCtx, key, result.UpstreamID); cancelErr == nil {
			s.invalidateProviderBalance(pid)
		}
		s.failPurchase(record.ID, "unknown", "database_error")
		return OrderDTO{}, purchaseError("database_error", mapStore(err))
	}
	_ = s.repo.Audit(ctx, &user.ID, "order.create", "order", o.ID, ip, nil)
	fresh, err := s.repo.GetOrder(ctx, o.ID, "")
	if err != nil {
		return OrderDTO{}, err
	}
	return OrderView(fresh, readSettings(p.Config).WebhookEnabled), nil
}

func classifyProviderPurchaseError(err error) (status, code string) {
	// 参数在调用供应商前即被拒绝，没有产生上游订单。
	if errors.Is(err, provider.ErrInvalidRequest) {
		return "failed", "invalid_selection"
	}
	var upstream *provider.ProviderError
	if !errors.As(err, &upstream) {
		return "unknown", "provider_error"
	}
	providerCode := strings.ToUpper(strings.TrimSpace(upstream.Code))
	operation := strings.ToLower(strings.TrimSpace(upstream.Operation))
	if strings.HasPrefix(operation, "catalog.") || operation == "purchase.tier" {
		switch providerCode {
		case "NO_NUMBERS", "OUT_OF_STOCK":
			return "failed", "no_numbers"
		case "BAD_SERVICE", "BAD_COUNTRY":
			return "failed", "invalid_selection"
		case "BAD_KEY", "BAD_ACTION", "BAD_STATUS", "NO_ACTIVATION", "EARLY_CANCEL_DENIED", "ACCOUNT_INACTIVE", "BANNED", "INVALID_BASE_URL", "INVALID_REQUEST":
			return "failed", "configuration"
		case "RATE_LIMIT":
			return "failed", "provider_rate_limited"
		default:
			// SMSBower 等级购买的目录与位置查询发生在真正下单之前，
			// 即使这里超时或返回 5xx，也可以确定尚未生成号码。
			return "failed", "provider_preflight_error"
		}
	}
	// HTTP 5xx 和 429 可能发生在上游已创建号码之后。
	// 即使响应正文包含看似确定的业务码，也不能据此允许新幂等键重购。
	if upstream.HTTPStatus >= http.StatusInternalServerError || upstream.HTTPStatus == http.StatusTooManyRequests {
		return "unknown", "provider_error"
	}
	switch providerCode {
	case "MAX_PRICE_EXCEEDED", "WRONG_MAX_PRICE":
		return "failed", "price_exceeded"
	case "NO_NUMBERS", "OUT_OF_STOCK":
		return "failed", "no_numbers"
	case "NO_BALANCE", "INSUFFICIENT_BALANCE", "INSUFFICIENTBALANCE", "INSUFFICIENT_FUNDS", "INSUFFICIENTFUNDS", "NOT_ENOUGH_BALANCE", "NOTENOUGHBALANCE", "LOW_BALANCE", "LOWBALANCE":
		return "failed", "insufficient_balance"
	case "BAD_SERVICE", "BAD_COUNTRY":
		return "failed", "invalid_selection"
	case "BAD_KEY", "BAD_ACTION", "BAD_STATUS", "NO_ACTIVATION", "EARLY_CANCEL_DENIED", "ACCOUNT_INACTIVE", "BANNED", "INVALID_BASE_URL", "INVALID_REQUEST":
		return "failed", "configuration"
	case "RATE_LIMIT":
		return "failed", "provider_rate_limited"
	case "TIMEOUT", "TRANSPORT_ERROR", "READ_ERROR", "NO_CONNECTION", "CANCELED", "RESPONSE_TOO_LARGE", "INVALID_RESPONSE":
		return "unknown", "provider_error"
	default:
		// 超时、断线、读取失败、5xx 和无法解析的成功响应，都可能发生在
		// 上游已经创建号码之后，必须保留原幂等键阻止重复扣费。
		return "unknown", "provider_error"
	}
}

func (s *Service) failPurchase(id, status, code string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.repo.FailPurchase(cleanupCtx, id, status, code); err != nil {
		slog.Error("更新购买意图失败", "purchase_id", id, "status", status, "error", err)
	}
}

func (s *Service) PurchaseAttempts(ctx context.Context, user domain.User) ([]PurchaseAttemptDTO, error) {
	records, err := s.repo.ListPurchaseRequests(ctx, user.ID, 20)
	if err != nil {
		return nil, mapStore(err)
	}
	attempts := make([]PurchaseAttemptDTO, 0, len(records))
	for _, record := range records {
		attempts = append(attempts, PurchaseAttemptDTO{
			Provider:    record.ProviderID,
			CountryCode: record.CountryCode,
			CountryName: record.CountryName,
			ServiceCode: record.ServiceCode,
			ServiceName: record.ServiceName,
			QualityTier: record.QualityTier,
			MaxPrice:    strconv.FormatFloat(record.MaxPrice, 'f', -1, 64),
			Status:      record.Status,
			ErrorCode:   purchaseAttemptErrorCode(record.Status, record.ErrorCode),
			Message:     purchaseAttemptMessage(record.Status, record.ErrorCode),
			CreatedAt:   record.CreatedAt,
			UpdatedAt:   record.UpdatedAt,
		})
	}
	return attempts, nil
}

func purchaseAttemptMessage(status, errorCode string) string {
	switch status {
	case "succeeded":
		return "购买成功"
	case "provisioning":
		return purchaseError("purchase_in_progress", nil).Message
	case "unknown":
		return purchaseResultUnknownError(errorCode, nil).Message
	case "failed":
		return purchaseError(errorCode, nil).Message
	default:
		return purchaseResultUnknownError(errorCode, nil).Message
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
			err = client.Cancel(lockCtx, key, o.UpstreamID)
			s.invalidateProviderBalance(o.ProviderID)
			if err != nil {
				return ErrProvider
			}
			status = domain.OrderCanceled
		} else {
			err = client.Complete(lockCtx, key, o.UpstreamID)
			s.invalidateProviderBalance(o.ProviderID)
			if err != nil {
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
		s.invalidateProviderBalance(fresh.ProviderID)
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
