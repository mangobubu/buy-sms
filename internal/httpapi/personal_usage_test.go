package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"buysms/internal/application"
	"buysms/internal/domain"
	"buysms/internal/store"
)

// SearchOrdersWithPersonalUsed is intentionally implemented here rather than
// changing the shared lifecycle test repository.  It exercises the optional
// order-search capability used by the HTTP application while preserving the
// existing Repository test double.
func (r *memoryRepository) SearchOrdersWithPersonalUsed(_ context.Context, user, status, providerID, keyword string, personalUsed *bool, limit, offset int) ([]domain.Order, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	keyword = strings.ToLower(strings.TrimSpace(keyword))
	filtered := make([]domain.Order, 0, len(r.orders))
	for _, order := range r.orders {
		if user != "" && order.UserID != user {
			continue
		}
		if status != "" && order.Status != status {
			continue
		}
		if providerID != "" && order.ProviderID != providerID {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(order.PhoneNumber+" "+order.ID+" "+order.UpstreamID), keyword) {
			continue
		}
		// The personal marker is meaningful only for completed orders.  A
		// personalUsed filter therefore excludes every non-completed order,
		// even if a malformed fixture happens to carry the marker as true.
		if personalUsed != nil {
			if order.Status != domain.OrderCompleted || order.PersonalUsed != *personalUsed {
				continue
			}
		}
		order.Messages = nil
		for _, message := range r.messages {
			if message.OrderID == order.ID {
				order.Messages = append(order.Messages, message)
			}
		}
		filtered = append(filtered, order)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].CreatedAt.Equal(filtered[j].CreatedAt) {
			return filtered[i].ID > filtered[j].ID
		}
		return filtered[i].CreatedAt.After(filtered[j].CreatedAt)
	})
	total := len(filtered)
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []domain.Order{}, total, nil
	}
	filtered = filtered[offset:]
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return append([]domain.Order(nil), filtered...), total, nil
}

func (r *memoryRepository) SetOrderPersonalUsed(_ context.Context, id, user string, used bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	order, ok := r.orders[id]
	if !ok || (user != "" && order.UserID != user) {
		return store.ErrNotFound
	}
	if order.Status != domain.OrderCompleted {
		return store.ErrConflict
	}
	order.PersonalUsed = used
	r.orders[id] = order
	return nil
}

// SetOrderPersonalUsed audits the mutation in production.  The in-memory
// test repository only needs to acknowledge the call so the HTTP response can
// be exercised without a database-backed audit log.
func (r *memoryRepository) Audit(context.Context, *string, string, string, string, string, json.RawMessage) error {
	return nil
}

func performJSONAuthenticatedRequest(router http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestPersonalUsedToggleValidationPermissionsAndPersistence(t *testing.T) {
	repo := newMemoryRepository()
	router, _, _ := newTestRouter(t, repo)
	operator := domain.User{ID: "personal-operator", Username: "operator", Role: "operator", Active: true}
	other := domain.User{ID: "personal-other", Username: "other", Role: "operator", Active: true}
	admin := domain.User{ID: "personal-admin", Username: "admin", Role: "admin", Active: true}
	for token, user := range map[string]domain.User{
		"personal-operator-token": operator,
		"personal-other-token":    other,
		"personal-admin-token":    admin,
	} {
		repo.putSession([]byte("router-test-session-pepper"), token, user)
	}
	now := time.Now().UTC()
	completed := domain.Order{
		ID: "personal-completed", UserID: operator.ID, ProviderID: domain.ProviderHeroSMS,
		PhoneNumber: "+15550001001", Status: domain.OrderCompleted, PersonalUsed: false,
		Cost: 7.25, Currency: "USD", CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	}
	active := domain.Order{
		ID: "personal-active", UserID: operator.ID, ProviderID: domain.ProviderHeroSMS,
		PhoneNumber: "+15550001002", Status: domain.OrderActive, PersonalUsed: false,
		Cost: 3.5, Currency: "USD", CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-2 * time.Minute),
	}
	otherCompleted := domain.Order{
		ID: "personal-other-completed", UserID: other.ID, ProviderID: domain.ProviderHeroSMS,
		PhoneNumber: "+15550001003", Status: domain.OrderCompleted, PersonalUsed: false,
		Cost: 9.5, Currency: "USD", CreatedAt: now.Add(-3 * time.Minute), UpdatedAt: now.Add(-3 * time.Minute),
	}
	repo.putOrder(completed)
	repo.putOrder(active)
	repo.putOrder(otherCompleted)
	repo.messages = append(repo.messages, domain.SMSMessage{ID: "personal-message", OrderID: completed.ID, Code: "1234", Text: "code 1234", ReceivedAt: now})

	for _, testCase := range []struct {
		name, token, orderID, body string
		status                     int
	}{
		{name: "缺少字段", token: "personal-operator-token", orderID: completed.ID, body: `{}`, status: http.StatusBadRequest},
		{name: "字段为null", token: "personal-operator-token", orderID: completed.ID, body: `{"personalUsed":null}`, status: http.StatusBadRequest},
		{name: "字段类型错误", token: "personal-operator-token", orderID: completed.ID, body: `{"personalUsed":"true"}`, status: http.StatusBadRequest},
		{name: "未知字段", token: "personal-operator-token", orderID: completed.ID, body: `{"personalUsed":true,"extra":1}`, status: http.StatusBadRequest},
		{name: "未登录", token: "", orderID: completed.ID, body: `{"personalUsed":true}`, status: http.StatusUnauthorized},
		{name: "操作员修改他人订单", token: "personal-operator-token", orderID: otherCompleted.ID, body: `{"personalUsed":true}`, status: http.StatusNotFound},
		{name: "非完成订单", token: "personal-operator-token", orderID: active.ID, body: `{"personalUsed":true}`, status: http.StatusConflict},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := performJSONAuthenticatedRequest(router, http.MethodPut, "/api/orders/"+testCase.orderID+"/personal-used", testCase.token, testCase.body)
			if response.Code != testCase.status {
				t.Fatalf("状态码=%d，期望=%d；响应=%s", response.Code, testCase.status, response.Body.String())
			}
		})
	}

	beforeOrders, beforeMessages, _, beforeTransitions := repo.snapshot()
	response := performJSONAuthenticatedRequest(router, http.MethodPut, "/api/orders/"+completed.ID+"/personal-used", "personal-operator-token", `{"personalUsed":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("切换为已用状态码=%d，响应=%s", response.Code, response.Body.String())
	}
	var updated application.OrderDTO
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatalf("解析切换响应失败: %v", err)
	}
	if updated.ID != completed.ID || !updated.PersonalUsed || updated.Status != "completed" || updated.Price != "7.25" || len(updated.Messages) != 1 {
		t.Fatalf("切换响应未保持订单字段: %+v", updated)
	}
	readBack := performAuthenticatedRequest(router, http.MethodGet, "/api/orders/"+completed.ID, "personal-operator-token")
	if readBack.Code != http.StatusOK {
		t.Fatalf("读取已用标记状态码=%d，响应=%s", readBack.Code, readBack.Body.String())
	}
	var readDTO application.OrderDTO
	if err := json.Unmarshal(readBack.Body.Bytes(), &readDTO); err != nil {
		t.Fatal(err)
	}
	if !readDTO.PersonalUsed {
		t.Fatalf("切换后的标记未持久化: %+v", readDTO)
	}

	response = performJSONAuthenticatedRequest(router, http.MethodPut, "/api/orders/"+completed.ID+"/personal-used", "personal-operator-token", `{"personalUsed":false}`)
	if response.Code != http.StatusOK {
		t.Fatalf("切换回未用状态码=%d，响应=%s", response.Code, response.Body.String())
	}
	readBack = performAuthenticatedRequest(router, http.MethodGet, "/api/orders/"+completed.ID, "personal-operator-token")
	if readBack.Code != http.StatusOK {
		t.Fatalf("读取未用标记状态码=%d，响应=%s", readBack.Code, readBack.Body.String())
	}
	if err := json.Unmarshal(readBack.Body.Bytes(), &readDTO); err != nil {
		t.Fatal(err)
	}
	if readDTO.PersonalUsed {
		t.Fatalf("true→false 未持久化: %+v", readDTO)
	}

	afterOrders, afterMessages, _, afterTransitions := repo.snapshot()
	if got := afterOrders[completed.ID]; got.Status != beforeOrders[completed.ID].Status || got.Cost != beforeOrders[completed.ID].Cost || got.PersonalUsed {
		t.Fatalf("切换不应影响 status/cost，最终订单=%+v，切换前=%+v", got, beforeOrders[completed.ID])
	}
	if len(afterMessages) != len(beforeMessages) || len(afterTransitions) != len(beforeTransitions) {
		t.Fatalf("切换不应新增短信或状态迁移，消息 %d→%d，迁移 %d→%d", len(beforeMessages), len(afterMessages), len(beforeTransitions), len(afterTransitions))
	}

	adminUpdate := performJSONAuthenticatedRequest(router, http.MethodPut, "/api/orders/"+otherCompleted.ID+"/personal-used", "personal-admin-token", `{"personalUsed":true}`)
	if adminUpdate.Code != http.StatusOK {
		t.Fatalf("管理员修改他人订单状态码=%d，响应=%s", adminUpdate.Code, adminUpdate.Body.String())
	}
}

func TestPersonalUsedOrderFilterAliasesIntersectionsAndPagination(t *testing.T) {
	repo := newMemoryRepository()
	router, _, _ := newTestRouter(t, repo)
	operator := domain.User{ID: "filter-operator", Username: "operator", Role: "operator", Active: true}
	repo.putSession([]byte("router-test-session-pepper"), "filter-operator-token", operator)
	now := time.Now().UTC()
	orders := []domain.Order{
		{ID: "used-hero-new", UserID: operator.ID, ProviderID: domain.ProviderHeroSMS, PhoneNumber: "+155501001", UpstreamID: "up-used-hero-new", Status: domain.OrderCompleted, PersonalUsed: true, Cost: 1, Currency: "USD", CreatedAt: now, UpdatedAt: now},
		{ID: "used-pool", UserID: operator.ID, ProviderID: domain.ProviderSMSPool, PhoneNumber: "+155501002", UpstreamID: "up-used-pool", Status: domain.OrderCompleted, PersonalUsed: true, Cost: 2, Currency: "USD", CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)},
		{ID: "unused-hero", UserID: operator.ID, ProviderID: domain.ProviderHeroSMS, PhoneNumber: "+155501003", UpstreamID: "up-unused-hero", Status: domain.OrderCompleted, PersonalUsed: false, Cost: 3, Currency: "USD", CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "active-marker", UserID: operator.ID, ProviderID: domain.ProviderHeroSMS, PhoneNumber: "+155501004", UpstreamID: "up-active-marker", Status: domain.OrderActive, PersonalUsed: true, Cost: 4, Currency: "USD", CreatedAt: now.Add(-3 * time.Minute), UpdatedAt: now.Add(-3 * time.Minute)},
		{ID: "cancelled-marker", UserID: operator.ID, ProviderID: domain.ProviderHeroSMS, PhoneNumber: "+155501005", UpstreamID: "up-cancelled-marker", Status: domain.OrderCanceled, PersonalUsed: true, Cost: 5, Currency: "USD", CreatedAt: now.Add(-4 * time.Minute), UpdatedAt: now.Add(-4 * time.Minute)},
		{ID: "other-used", UserID: "filter-other", ProviderID: domain.ProviderHeroSMS, PhoneNumber: "+155501006", UpstreamID: "up-other-used", Status: domain.OrderCompleted, PersonalUsed: true, Cost: 6, Currency: "USD", CreatedAt: now.Add(-5 * time.Minute), UpdatedAt: now.Add(-5 * time.Minute)},
	}
	for _, order := range orders {
		repo.putOrder(order)
	}

	assertFilter := func(name, query string, wantTotal int, wantIDs ...string) {
		t.Helper()
		response := performAuthenticatedRequest(router, http.MethodGet, "/api/orders?"+query, "filter-operator-token")
		if response.Code != http.StatusOK {
			t.Fatalf("%s 状态码=%d，响应=%s", name, response.Code, response.Body.String())
		}
		var page struct {
			Items []application.OrderDTO `json:"items"`
			Total int                    `json:"total"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatalf("%s 响应解析失败: %v", name, err)
		}
		if page.Total != wantTotal || len(page.Items) != len(wantIDs) {
			t.Fatalf("%s total/items=%d/%d，期望=%d/%d；响应=%s", name, page.Total, len(page.Items), wantTotal, len(wantIDs), response.Body.String())
		}
		for i, wantID := range wantIDs {
			if page.Items[i].ID != wantID {
				t.Errorf("%s 第%d项=%q，期望=%q", name, i, page.Items[i].ID, wantID)
			}
			if query != "" && (strings.Contains(query, "personalUsed=true") || strings.Contains(query, "personalUsed=used")) {
				if page.Items[i].Status != "completed" || !page.Items[i].PersonalUsed {
					t.Errorf("%s 返回了非已用完成订单: %+v", name, page.Items[i])
				}
			}
		}
	}

	assertFilter("true", "personalUsed=true&pageSize=20", 2, "used-hero-new", "used-pool")
	assertFilter("used 别名", "personalUsed=used&pageSize=20", 2, "used-hero-new", "used-pool")
	assertFilter("false", "personalUsed=false&pageSize=20", 1, "unused-hero")
	assertFilter("unused 别名", "personalUsed=unused&pageSize=20", 1, "unused-hero")
	assertFilter("省略筛选", "pageSize=20", 5, "used-hero-new", "used-pool", "unused-hero", "active-marker", "cancelled-marker")
	assertFilter("空筛选", "personalUsed=&pageSize=20", 5, "used-hero-new", "used-pool", "unused-hero", "active-marker", "cancelled-marker")
	assertFilter("provider 交叉", "personalUsed=true&provider=sms-pool&pageSize=20", 1, "used-pool")
	assertFilter("status 交叉", "personalUsed=true&status=completed&pageSize=20", 2, "used-hero-new", "used-pool")
	assertFilter("keyword 交叉", "personalUsed=true&keyword=up-used-hero&pageSize=20", 1, "used-hero-new")
	assertFilter("分页 total 在筛选后计算", "personalUsed=true&page=2&pageSize=1", 2, "used-pool")

	response := performAuthenticatedRequest(router, http.MethodGet, "/api/orders?personalUsed=maybe", "filter-operator-token")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("非法 personalUsed 状态码=%d，响应=%s", response.Code, response.Body.String())
	}
}
