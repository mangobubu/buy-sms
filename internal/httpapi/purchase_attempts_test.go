package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"buysms/internal/application"
	"buysms/internal/domain"
	"buysms/internal/store"
)

func TestPurchaseAttemptsOnlyReturnCurrentUsersRecords(t *testing.T) {
	repo := newMemoryRepository()
	router, _, _ := newTestRouter(t, repo)
	operator := domain.User{ID: "operator-1", Username: "operator", Role: "operator", Active: true}
	repo.putSession([]byte("router-test-session-pepper"), "operator-token", operator)
	now := time.Date(2026, time.September, 2, 8, 30, 0, 0, time.UTC)
	repo.putPurchase(store.PurchaseRecord{
		ID: "own-attempt", UserID: operator.ID, IdempotencyKey: "secret-own-key",
		ProviderID: domain.ProviderSMSBower, CountryCode: "10", CountryName: "Vietnam", ServiceCode: "hc", ServiceName: "MOMO", QualityTier: "gold",
		MaxPrice: 0.091, Status: "failed", ErrorCode: "price_exceeded", CreatedAt: now, UpdatedAt: now.Add(time.Second),
	})
	repo.putPurchase(store.PurchaseRecord{
		ID: "other-attempt", UserID: "operator-2", IdempotencyKey: "secret-other-key",
		ProviderID: domain.ProviderHeroSMS, CountryCode: "1", ServiceCode: "other-private-service",
		MaxPrice: 9, Status: "unknown", ErrorCode: "provider_error", CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute),
	})

	response := performAuthenticatedRequest(router, http.MethodGet, "/api/purchase-attempts", "operator-token")
	if response.Code != http.StatusOK {
		t.Fatalf("查询最近购买尝试状态码=%d，响应=%s", response.Code, response.Body.String())
	}
	var attempts []application.PurchaseAttemptDTO
	if err := json.Unmarshal(response.Body.Bytes(), &attempts); err != nil {
		t.Fatalf("解析购买尝试响应失败: %v", err)
	}
	if len(attempts) != 1 || attempts[0].CountryCode != "10" || attempts[0].CountryName != "Vietnam" || attempts[0].ServiceCode != "hc" || attempts[0].ServiceName != "MOMO" || attempts[0].ErrorCode != "price_exceeded" || attempts[0].Message != "供应商实际价格超过所选价格，购买已取消" {
		t.Fatalf("当前用户购买尝试错误: %+v", attempts)
	}
	encoded := response.Body.String()
	for _, forbidden := range []string{"other-private-service", "secret-own-key", "secret-other-key", "own-attempt", "other-attempt", "operator-1", "operator-2", "idempotencyKey", "userID", "orderId"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("购买尝试响应暴露了内部或其他用户数据 %q: %s", forbidden, encoded)
		}
	}
}
