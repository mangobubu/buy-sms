package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/store"
)

type purchaseAttemptsRepository struct {
	store.Repository
	records []store.PurchaseRecord
	userID  string
	limit   int
}

func (r *purchaseAttemptsRepository) ListPurchaseRequests(_ context.Context, userID string, limit int) ([]store.PurchaseRecord, error) {
	r.userID = userID
	r.limit = limit
	return append([]store.PurchaseRecord(nil), r.records...), nil
}

func TestPurchaseAttemptsUseCurrentUserScopeAndSafeDTO(t *testing.T) {
	now := time.Date(2026, time.September, 2, 9, 0, 0, 0, time.UTC)
	repo := &purchaseAttemptsRepository{records: []store.PurchaseRecord{
		{
			ID: "internal-id", UserID: "admin-id", IdempotencyKey: "internal-idempotency-key", OrderID: "internal-order-id",
			ProviderID: domain.ProviderSMSBower, CountryCode: "10", CountryName: "Vietnam", ServiceCode: "hc", ServiceName: "MOMO", QualityTier: "silver",
			MaxPrice: 0.091, Status: "failed", ErrorCode: "price_exceeded", CreatedAt: now, UpdatedAt: now.Add(time.Second),
		},
		{
			ProviderID: domain.ProviderSMSPool, CountryCode: "US", ServiceCode: "wa",
			MaxPrice: 1, Status: "unknown", ErrorCode: purchaseUnknownProviderTimeout, CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
		},
	}}
	service := New(repo, nil, nil, config.Config{})
	attempts, err := service.PurchaseAttempts(context.Background(), domain.User{ID: "admin-id", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if repo.userID != "admin-id" || repo.limit != 20 {
		t.Fatalf("购买尝试查询范围=(%q,%d)，期望=(admin-id,20)", repo.userID, repo.limit)
	}
	if len(attempts) != 2 {
		t.Fatalf("购买尝试数量=%d，期望 2", len(attempts))
	}
	failed := attempts[0]
	if failed.Provider != domain.ProviderSMSBower || failed.CountryCode != "10" || failed.CountryName != "Vietnam" || failed.ServiceCode != "hc" || failed.ServiceName != "MOMO" || failed.QualityTier != "silver" || failed.MaxPrice != "0.091" || failed.Status != "failed" || failed.ErrorCode != "price_exceeded" {
		t.Fatalf("失败购买尝试 DTO 错误: %+v", failed)
	}
	if failed.Message != "供应商实际价格超过所选价格，购买已取消" || !failed.CreatedAt.Equal(now) || !failed.UpdatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("失败购买尝试消息或时间错误: %+v", failed)
	}
	unknown := attempts[1]
	if unknown.ErrorCode != "provider_error" || unknown.Message != "购买结果尚未确认：供应商请求超时，系统无法确认是否已生成号码；为避免重复扣费，仅暂停当前平台与购买条件的重复提交" {
		t.Fatalf("未知结果消息未说明供应商响应异常: %q", unknown.Message)
	}
	if strings.Contains(unknown.Message, "最近购买尝试") {
		t.Fatalf("未知结果消息引用了已移除的板块: %q", unknown.Message)
	}
}
