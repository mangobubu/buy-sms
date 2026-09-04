package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHeroSMSRenewDoesNotReadHistoryBeforePostAndUsesDelayedCharge(t *testing.T) {
	submittedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	oldAt := submittedAt.Add(-time.Second)
	baseline, err := encodeHeroProlongBaseline([]heroProlongHistoryEntry{{Duration: 24, Price: 0.11, CreatedAt: oldAt}})
	if err != nil {
		t.Fatal(err)
	}
	posted := false
	historyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/activations/rent-1/prolong":
			posted = true
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("解析续租请求失败: %v", err)
			}
			if body["duration"] != float64(24) {
				t.Errorf("续租时长=%#v", body["duration"])
			}
			_, _ = writer.Write([]byte(`{"data":[{"id":"rent-1","phone":"79990001122","price":999,"expiredAt":"2026-09-06T12:00:00Z"}]}`))
		case "/api/v1/activations/rent-1/prolong/history":
			if !posted {
				t.Error("HeroSMS 在提交续租 POST 前读取了 history")
			}
			historyCalls++
			if historyCalls == 1 {
				_, _ = writer.Write([]byte(`{"data":[{"duration":24,"price":0.11,"createdAt":"2026-09-04T11:59:59Z"}]}`))
				return
			}
			_, _ = writer.Write([]byte(`{"data":[{"duration":24,"price":0.11,"createdAt":"2026-09-04T11:59:59Z"},{"duration":24,"price":0.37,"createdAt":"2026-09-04T12:00:01Z"}]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	result, err := NewHeroSMS(server.URL+"/api/v1").Renew(context.Background(), "test-key", "rent-1", RenewalRequest{
		Mode: RenewalProlong, Value: 24, Unit: "hour", SubmittedAt: submittedAt, Baseline: baseline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if historyCalls != 2 || result.Cost != 0.37 || result.ExpiresAt == nil || !result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("续租确认结果=%+v historyCalls=%d", result, historyCalls)
	}
}

func TestHeroSMSReconcileProlongUsesNewHistoryPriceAndCurrentActivationExpiry(t *testing.T) {
	submittedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	baseline, err := encodeHeroProlongBaseline([]heroProlongHistoryEntry{{
		Duration: 24, Price: 0.10, CreatedAt: time.Date(2026, 9, 4, 11, 59, 30, 0, time.UTC),
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/activations/rent-1/prolong/history":
			_, _ = writer.Write([]byte(`{"data":[{"duration":24,"price":0.10,"createdAt":"2026-09-04T11:59:30Z"},{"hours":24,"userPrice":0.41,"createDate":"2026-09-04T12:00:02Z"}]}`))
		case "/api/v1/activations":
			if request.URL.Query().Get("sort") != "createdAt:desc" {
				t.Errorf("激活列表排序参数=%q", request.URL.Query().Get("sort"))
			}
			_, _ = writer.Write([]byte(`{"data":[{"id":"rent-1","phone":"79990001122","service":"full","country":2,"price":4.2,"createdAt":"2026-09-01T00:00:00Z","expiredAt":"2026-09-07T12:00:00Z"}]}`))
		default:
			if request.Method == http.MethodPost {
				t.Errorf("对账不应提交写请求: %s", request.URL.Path)
			}
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	result, found, err := NewHeroSMS(server.URL+"/api/v1").ReconcileRenewal(context.Background(), "test-key", "rent-1", RenewalRequest{
		Mode: RenewalProlong, Value: 24, Unit: "hour", SubmittedAt: submittedAt, Baseline: baseline,
	})
	if err != nil || !found || result.Cost != 0.41 || result.UpstreamID != "rent-1" || result.ExpiresAt == nil || !result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("HeroSMS prolong 对账=%+v found=%v err=%v", result, found, err)
	}
}

func TestHeroSMSReconcileReactivateMatchesNewActiveActivation(t *testing.T) {
	submittedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 9, 4, 12, 20, 2, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/activations" {
			t.Errorf("重新启用对账请求=%s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(`{"data":[
			{"id":"old-1","phone":"+7 9990001122","service":"tg","country":2,"price":0.11,"createdAt":"2026-09-04T11:59:30Z","expiredAt":"2026-09-04T12:10:00Z"},
			{"id":"wrong-country","phone":"79990001122","service":"tg","country":3,"price":8.8,"createdAt":"2026-09-04T12:00:01Z","expiredAt":"2026-09-04T12:20:01Z"},
			{"id":"new-2","phone":"79990001122","service":"tg","country":2,"price":0.29,"createdAt":"2026-09-04T12:00:02Z","expiredAt":"2026-09-04T12:20:02Z"}
		]}`))
	}))
	t.Cleanup(server.Close)

	result, found, err := NewHeroSMS(server.URL+"/api/v1").ReconcileRenewal(context.Background(), "test-key", "old-1", RenewalRequest{
		Mode: RenewalReactivate, Value: 20, Unit: "minute", SubmittedAt: submittedAt,
		PhoneNumber: "+7 (999) 000-11-22", Country: "2", Service: "tg",
	})
	if err != nil || !found || result.UpstreamID != "new-2" || result.Cost != 0.29 || result.ExpiresAt == nil || !result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("HeroSMS reactivate 对账=%+v found=%v err=%v", result, found, err)
	}
}

func TestSMSPoolReconcileRenewalUsesActiveAPIActualCostAndExpiry(t *testing.T) {
	expiresAt := time.Date(2026, 9, 4, 16, 10, 0, 0, time.UTC)
	activeCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("解析 SMSPool 表单失败: %v", err)
		}
		if request.URL.Path == "/sms/reactivate" {
			t.Error("续期对账不应重复调用 sms/reactivate")
		}
		if request.URL.Path != "/request/active" {
			http.NotFound(writer, request)
			return
		}
		activeCalls++
		_, _ = writer.Write([]byte(`[{"order_code":"POOL-1","phonenumber":"4155550123","cost":"0.63","expiry":1788538200,"status":"pending"}]`))
	}))
	t.Cleanup(server.Close)

	result, found, err := NewSMSPool(server.URL).ReconcileRenewal(context.Background(), "test-key", "POOL-1", RenewalRequest{
		Mode: RenewalReactivate, Value: 1, Unit: "activation", SubmittedAt: time.Now().UTC(),
	})
	if err != nil || !found || activeCalls != 1 || result.Cost != 0.63 || result.PhoneNumber != "4155550123" || result.ExpiresAt == nil || !result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("SMSPool 对账=%+v found=%v calls=%d err=%v", result, found, activeCalls, err)
	}
}
