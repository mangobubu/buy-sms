package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHeroSMSRenewalOptionsAndActionsUseNativeAPI(t *testing.T) {
	expiresAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	prolongDone := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "ApiKey test-key" {
			t.Errorf("HeroSMS 续期请求认证头错误: %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/activations/rent-1/prolong/options":
			if request.Method != http.MethodGet {
				t.Errorf("续租选项方法=%s", request.Method)
			}
			_, _ = writer.Write([]byte(`{"data":{"options":[{"duration":{"value":24,"unit":"hour"},"price":"1.25"}]}}`))
		case "/api/v1/activations/rent-1/prolong":
			prolongDone = true
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["duration"] != float64(24) {
				t.Errorf("续租时长=%#v", body["duration"])
			}
			_, _ = writer.Write([]byte(`{"data":[{"id":"rent-1","phone":"79990001122","price":1.25,"expiredAt":"2026-09-06T12:00:00Z"}]}`))
		case "/api/v1/activations/rent-1/prolong/history":
			if !prolongDone {
				_, _ = writer.Write([]byte(`{"data":[]}`))
				break
			}
			_, _ = writer.Write([]byte(`{"data":[{"duration":24,"price":1.25,"createdAt":"2026-09-04T12:00:01Z"}]}`))
		case "/api/v1/activations/act-1/reactivate/options":
			_, _ = writer.Write([]byte(`{"data":{"options":[{"duration":{"value":20,"unit":"minute"},"price":0.22}]}}`))
		case "/api/v1/activations/act-1/reactivate":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 0 {
				t.Errorf("普通时长重新启用不应提交 duration: %#v", body)
			}
			_, _ = writer.Write([]byte(`{"data":[{"id":"act-2","phone":"79990002233","price":0.22,"expiredAt":"2026-09-06T12:00:00Z"}]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	client := NewHeroSMS(server.URL + "/api/v1")
	options, err := client.RenewalOptions(context.Background(), "test-key", "rent-1", RenewalProlong)
	if err != nil || len(options) != 1 || options[0].Value != 24 || options[0].Unit != "hour" || options[0].Price != 1.25 || options[0].Baseline == "" {
		t.Fatalf("HeroSMS 续租选项=%#v err=%v", options, err)
	}
	prolonged, err := client.Renew(context.Background(), "test-key", "rent-1", RenewalRequest{Mode: RenewalProlong, Value: 24, Unit: "hour", SubmittedAt: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC), Baseline: options[0].Baseline})
	if err != nil || prolonged.UpstreamID != "rent-1" || prolonged.Cost != 1.25 || prolonged.ExpiresAt == nil || !prolonged.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("HeroSMS 续租结果=%#v err=%v", prolonged, err)
	}
	options, err = client.RenewalOptions(context.Background(), "test-key", "act-1", RenewalReactivate)
	if err != nil || len(options) != 1 || options[0].Unit != "minute" || options[0].Price != 0.22 {
		t.Fatalf("HeroSMS 重新启用选项=%#v err=%v", options, err)
	}
	reactivated, err := client.Renew(context.Background(), "test-key", "act-1", RenewalRequest{Mode: RenewalReactivate, Value: 20, Unit: "minute"})
	if err != nil || reactivated.UpstreamID != "act-2" || reactivated.Cost != 0.22 {
		t.Fatalf("HeroSMS 重新启用结果=%#v err=%v", reactivated, err)
	}
}

func TestHeroSMSRenewalRejectsInvalidOptionPayloadAndRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"data":{"options":[{"duration":{"value":20,"unit":"day"},"price":1}]}}`))
	}))
	t.Cleanup(server.Close)
	client := NewHeroSMS(server.URL + "/api/v1")
	if _, err := client.RenewalOptions(context.Background(), "key", "1", RenewalReactivate); err == nil {
		t.Fatal("非法续期单位响应应失败")
	}
	if _, err := client.Renew(context.Background(), "key", "1", RenewalRequest{Mode: RenewalProlong, Value: 20, Unit: "minute"}); err == nil {
		t.Fatal("续租仅应接受小时")
	}
}

func TestSMSPoolRenewalUsesHistoryQuoteAndActiveResultPrice(t *testing.T) {
	expiresAt := time.Date(2026, 9, 4, 16, 10, 0, 0, time.UTC)
	var reactivateCalls int
	var activeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("key") != "test-key" {
			t.Errorf("SMSPool key=%q", request.Form.Get("key"))
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/request/history":
			if request.Form.Get("search") != "POOL-1" {
				t.Errorf("历史报价未按订单查询: %#v", request.Form)
			}
			_, _ = writer.Write([]byte(`{"data":[{"order_code":"POOL-1","short_name":"US","pool":7,"status":"refunded","code":"0","cost":"0.42","expiry":1788538200}]}`))
		case "/sms/reactivate":
			reactivateCalls++
			if request.Form.Get("orderid") != "POOL-1" {
				t.Errorf("重新启用订单号=%q", request.Form.Get("orderid"))
			}
			_, _ = writer.Write([]byte(`{"success":1,"message":"reactivated","cost":"999"}`))
		case "/request/active":
			activeCalls++
			if activeCalls < 3 {
				_, _ = writer.Write([]byte(`[]`))
				break
			}
			_, _ = writer.Write([]byte(`[{"order_code":"POOL-1","short_name":"US","pool":7,"status":"pending","phonenumber":"14155550123","cost":"0.57","expiry":1788538200}]`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	client := NewSMSPool(server.URL)
	options, err := client.RenewalOptions(context.Background(), "test-key", "POOL-1", RenewalReactivate)
	if err != nil || len(options) != 1 || options[0] != (RenewalOption{Value: 1, Unit: "activation", Price: 0.42}) {
		t.Fatalf("SMSPool API 历史报价=%#v err=%v", options, err)
	}
	result, err := client.Renew(context.Background(), "test-key", "POOL-1", RenewalRequest{Mode: RenewalReactivate, Value: 1, Unit: "activation"})
	if err != nil || reactivateCalls != 1 || activeCalls != 3 || result.Cost != 0.57 || result.ExpiresAt == nil || !result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("SMSPool 应使用重新启用后 Active API 的价格/期限: result=%#v calls=%d err=%v", result, reactivateCalls, err)
	}
}

func TestSMSPoolRenewalHidesIneligibleHistory(t *testing.T) {
	tests := []struct {
		name string
		row  string
	}{
		{name: "非 Foxtrot", row: `{"order_code":"POOL-1","short_name":"US","pool":2,"status":"refunded","code":"0","cost":"0.42"}`},
		{name: "已经收码", row: `{"order_code":"POOL-1","short_name":"US","pool":7,"status":"refunded","code":"1234","cost":"0.42"}`},
		{name: "仍在活跃", row: `{"order_code":"POOL-1","short_name":"US","pool":7,"status":"pending","code":"0","cost":"0.42"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if err := request.ParseMultipartForm(1 << 20); err != nil {
					t.Fatal(err)
				}
				_, _ = writer.Write([]byte(`{"data":[` + test.row + `]}`))
			}))
			t.Cleanup(server.Close)
			options, err := NewSMSPool(server.URL).RenewalOptions(context.Background(), "test-key", "POOL-1", RenewalReactivate)
			if err != nil || len(options) != 0 {
				t.Fatalf("不符合资格时 options=%#v err=%v", options, err)
			}
		})
	}
}
