package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type webhookRepository struct {
	store.Repository

	provider domain.Provider
	findErr  error
	saveErr  error
	saved    []store.WebhookRecord
}

func (r *webhookRepository) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	if id != r.provider.ID {
		return domain.Provider{}, store.ErrNotFound
	}
	return r.provider, nil
}

func (r *webhookRepository) FindOrderByUpstream(context.Context, string, string) (domain.Order, error) {
	return domain.Order{}, r.findErr
}

func (r *webhookRepository) SaveWebhookEvent(_ context.Context, record store.WebhookRecord) (bool, error) {
	r.saved = append(r.saved, record)
	return r.saveErr == nil, r.saveErr
}

func newWebhookService(t *testing.T, repo *webhookRepository, token string, now time.Time) *Service {
	t.Helper()
	vault, err := secure.NewVault([]byte("webhook-order-lookup-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := vault.Encrypt(token)
	if err != nil {
		t.Fatal(err)
	}
	repo.provider = domain.Provider{ID: domain.ProviderSMSBower, WebhookTokenCipher: cipher}
	service := New(repo, nil, vault, config.Config{})
	service.now = func() time.Time { return now }
	return service
}

func TestWebhookUnknownOrderIsAuditedAndAcknowledged(t *testing.T) {
	const token = "webhook-token"
	now := time.Date(2026, time.September, 2, 13, 3, 26, 0, time.FixedZone("CST", 8*60*60))
	repo := &webhookRepository{findErr: fmt.Errorf("lookup order: %w", store.ErrNotFound)}
	service := newWebhookService(t, repo, token, now)
	payload := json.RawMessage(`{"activationId":"missing-activation","code":"123456"}`)

	if err := service.Webhook(context.Background(), domain.ProviderSMSBower, token, payload, json.RawMessage(`{"X-Request-ID":"req-1"}`)); err != nil {
		t.Fatalf("未知订单回调应成功 ACK，实际错误=%v", err)
	}
	if len(repo.saved) != 1 {
		t.Fatalf("Webhook 审计记录数=%d，期望=1", len(repo.saved))
	}
	record := repo.saved[0]
	if record.ProviderID != domain.ProviderSMSBower || record.UpstreamID != "missing-activation" {
		t.Fatalf("Webhook 审计标识不正确: %+v", record)
	}
	if record.Status != "ignored" || record.Error != "order_not_found" {
		t.Fatalf("未知订单审计状态=(%q, %q)，期望=(ignored, order_not_found)", record.Status, record.Error)
	}
	if !record.ReceivedAt.Equal(now.UTC()) || string(record.Payload) != string(payload) {
		t.Fatalf("Webhook 审计内容不完整: %+v", record)
	}
}

func TestWebhookOrderLookupFailureKeepsCauseAndFails(t *testing.T) {
	const token = "webhook-token"
	cause := errors.New("database unavailable")
	repo := &webhookRepository{findErr: cause}
	service := newWebhookService(t, repo, token, time.Now())

	err := service.Webhook(context.Background(), domain.ProviderSMSBower, token, json.RawMessage(`{"activationId":"activation-1","code":"123456"}`), json.RawMessage(`{}`))
	if !errors.Is(err, cause) {
		t.Fatalf("查单失败未保留底层原因: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "find webhook order") {
		t.Fatalf("查单失败缺少稳定上下文: %v", err)
	}
	if len(repo.saved) != 0 {
		t.Fatalf("真实查单错误不应伪造 ignored 审计，实际记录数=%d", len(repo.saved))
	}
}

func TestWebhookUnknownOrderAuditFailureKeepsCauseAndFails(t *testing.T) {
	const token = "webhook-token"
	cause := errors.New("audit insert failed")
	repo := &webhookRepository{findErr: store.ErrNotFound, saveErr: cause}
	service := newWebhookService(t, repo, token, time.Now())

	err := service.Webhook(context.Background(), domain.ProviderSMSBower, token, json.RawMessage(`{"activationId":"missing-activation","code":"123456"}`), json.RawMessage(`{}`))
	if !errors.Is(err, cause) {
		t.Fatalf("审计写入失败未保留底层原因: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "save unknown webhook event") {
		t.Fatalf("审计写入失败缺少稳定上下文: %v", err)
	}
	if len(repo.saved) != 1 || repo.saved[0].Status != "ignored" || repo.saved[0].Error != "order_not_found" {
		t.Fatalf("审计写入尝试内容不正确: %+v", repo.saved)
	}
}
