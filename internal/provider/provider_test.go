package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testAPIKey = "test-key-7Yw3Zp"

func TestHeroSMSNativeLifecycleAndFullOTPHistory(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/stubs/handler_api.php":
			if request.URL.Query().Get("api_key") != testAPIKey || request.URL.Query().Get("action") != "getCountries" {
				http.Error(writer, `{"code":"BAD_REQUEST"}`, http.StatusBadRequest)
				return
			}
			_, _ = writer.Write([]byte(`{"2":{"id":2,"eng":"Kazakhstan","chn":"哈萨克斯坦"}}`))
		case "/api/v1/activations/offers/sms":
			assertHeader(t, request, "Authorization", "ApiKey "+testAPIKey)
			if request.Method != http.MethodGet || request.URL.Query().Get("services") != "tg" || request.URL.Query().Get("countries") != "2" {
				http.Error(writer, `{"code":"BAD_REQUEST"}`, http.StatusBadRequest)
				return
			}
			_, _ = writer.Write([]byte(`{"data":{"tg":{"2":{"prices":{"default":0.14,"retail":0.17,"min":0.11},"counts":{"total":7,"physical":5}}}}}`))
		case "/api/v1/activations":
			assertHeader(t, request, "Authorization", "ApiKey "+testAPIKey)
			if request.Method != http.MethodPost {
				http.Error(writer, `{"code":"BAD_METHOD"}`, http.StatusMethodNotAllowed)
				return
			}
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("HeroSMS 购买请求不是有效 JSON: %v", err)
			}
			if body["country"] != float64(2) || body["service"] != "tg" || body["verificationType"] != "sms" || body["fixedPrice"] != true {
				t.Errorf("HeroSMS 购买请求字段错误: %#v", body)
			}
			_, _ = writer.Write([]byte(`{"data":[{"id":"act-1","phone":"77001234567","countryPhoneCode":"7","price":"0.14","expiredAt":"2026-09-01T13:00:00Z","otpList":[]}]}`))
		case "/api/v1/activations/act-1/otp":
			assertHeader(t, request, "Authorization", "ApiKey "+testAPIKey)
			_, _ = writer.Write([]byte(`{"data":[{"id":"otp-1","smsCode":"12345","smsText":"first 12345","receivedAt":"2026-09-01T12:00:01Z","type":"sms","phoneFrom":"Telegram"},{"id":"otp-2","smsCode":"67890","smsText":"second 67890","receivedAt":"2026-09-01T12:00:09Z","type":"sms","phoneFrom":"Telegram"}]}`))
		case "/api/v1/activations/act-1/finish":
			assertHeader(t, request, "Authorization", "ApiKey "+testAPIKey)
			if request.Method != http.MethodPost {
				t.Errorf("完成动作方法=%s，期望 POST", request.Method)
			}
			writer.WriteHeader(http.StatusNoContent)
		case "/api/v1/activations/act-1":
			assertHeader(t, request, "Authorization", "ApiKey "+testAPIKey)
			if request.Method != http.MethodDelete {
				t.Errorf("取消动作方法=%s，期望 DELETE", request.Method)
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewHeroSMS(server.URL + "/api/v1")
	if client.ID() != "herosms" {
		t.Fatalf("供应商 ID=%s", client.ID())
	}

	countries, err := client.Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogCountry})
	if err != nil || len(countries) != 1 || countries[0].Code != "2" || countries[0].Name != "Kazakhstan" {
		t.Fatalf("国家目录解析错误: items=%#v err=%v", countries, err)
	}
	prices, err := client.Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogPrice, Country: "2", Service: "tg"})
	if err != nil || len(prices) != 1 || prices[0].Code != "tg" || prices[0].Country != "2" || prices[0].Price == nil || *prices[0].Price != 0.14 || prices[0].Stock == nil || *prices[0].Stock != 7 {
		t.Fatalf("HeroSMS 原生报价解析错误: items=%#v err=%v", prices, err)
	}
	fixedPrice := true
	purchase, err := client.Purchase(context.Background(), testAPIKey, PurchaseRequest{Country: "2", Service: "tg", FixedPrice: &fixedPrice})
	if err != nil || purchase.UpstreamID != "act-1" || purchase.PhoneNumber != "77001234567" || purchase.CountryCode != "7" || purchase.Cost != 0.14 || purchase.ExpiresAt == nil || !purchase.CanGetAnotherSMS {
		t.Fatalf("HeroSMS 购买解析错误: result=%#v err=%v", purchase, err)
	}
	poll, err := client.Poll(context.Background(), testAPIKey, "act-1")
	if err != nil || poll.State != PollReceived || len(poll.Messages) != 2 || poll.Code != "67890" || poll.Messages[0].Fingerprint != "otp-1" || poll.Messages[1].Fingerprint != "otp-2" {
		t.Fatalf("HeroSMS 全量 OTP 解析错误: result=%#v err=%v", poll, err)
	}
	beforeNoop := requestCount.Load()
	if _, err := client.RequestAnother(context.Background(), testAPIKey, "act-1"); err != nil {
		t.Fatalf("HeroSMS 原生 RequestAnother no-op 返回错误: %v", err)
	}
	if requestCount.Load() != beforeNoop {
		t.Fatal("HeroSMS 原生 RequestAnother 发起了未定义的外部请求")
	}
	if err := client.Complete(context.Background(), testAPIKey, "act-1"); err != nil {
		t.Fatalf("HeroSMS 完成失败: %v", err)
	}
	if err := client.Cancel(context.Background(), testAPIKey, "act-1"); err != nil {
		t.Fatalf("HeroSMS 取消失败: %v", err)
	}
}

func TestSMSBowerCompatibilityLifecycle(t *testing.T) {
	var numberV2Calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Query().Get("api_key") != testAPIKey {
			http.Error(writer, "BAD_KEY", http.StatusBadRequest)
			return
		}
		action := request.URL.Query().Get("action")
		switch action {
		case "getServicesList":
			_, _ = writer.Write([]byte(`{"status":"success","services":[{"code":"kt","name":"KakaoTalk"}]}`))
		case "getPrices":
			_, _ = writer.Write([]byte(`{"2":{"kt":{"cost":"0.12","count":"10"}}}`))
		case "getNumberV2":
			numberV2Calls.Add(1)
			if request.URL.Query().Get("service") == "old" {
				_, _ = writer.Write([]byte("BAD_ACTION"))
				return
			}
			_, _ = writer.Write([]byte(`{"activationId":"44","phoneNumber":"79990001122","activationCost":0.12,"countryCode":"7","canGetAnotherSms":true}`))
		case "getNumber":
			_, _ = writer.Write([]byte("ACCESS_NUMBER:45:78880001122"))
		case "getAllSms":
			if request.URL.Query().Get("id") == "fallback" {
				_, _ = writer.Write([]byte("BAD_ACTION"))
				return
			}
			if request.URL.Query().Get("size") != "100" || request.URL.Query().Get("page") != "0" {
				t.Errorf("getAllSms 缺少分页参数: %s", request.URL.RawQuery)
			}
			_, _ = writer.Write([]byte(`{"data":[{"id":"m1","smsCode":"1111","smsText":"first"},{"id":"m2","smsCode":"2222","smsText":"second"}]}`))
		case "getStatus":
			if request.URL.Query().Get("size") != "" || request.URL.Query().Get("page") != "" {
				t.Errorf("getStatus 意外携带分页参数: %s", request.URL.RawQuery)
			}
			_, _ = writer.Write([]byte("STATUS_OK:8899"))
		case "setStatus":
			switch request.URL.Query().Get("status") {
			case "3":
				_, _ = writer.Write([]byte("ACCESS_RETRY_GET"))
			case "6":
				_, _ = writer.Write([]byte("ACCESS_ACTIVATION"))
			case "8":
				_, _ = writer.Write([]byte("ACCESS_CANCEL"))
			default:
				_, _ = writer.Write([]byte("BAD_STATUS"))
			}
		default:
			_, _ = writer.Write([]byte("BAD_ACTION"))
		}
	}))
	defer server.Close()

	client := NewSMSBower(server.URL + "/stubs/handler_api.php")
	services, err := client.Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogService})
	if err != nil || len(services) != 1 || services[0].Code != "kt" || services[0].Name != "KakaoTalk" {
		t.Fatalf("SMSBower 服务目录错误: %#v, %v", services, err)
	}
	prices, err := client.Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogPrice, Country: "2", Service: "kt"})
	if err != nil || len(prices) != 1 || prices[0].Price == nil || *prices[0].Price != 0.12 || prices[0].Stock == nil || *prices[0].Stock != 10 {
		t.Fatalf("SMSBower 价格目录错误: %#v, %v", prices, err)
	}
	purchase, err := client.Purchase(context.Background(), testAPIKey, PurchaseRequest{Country: "2", Service: "kt"})
	if err != nil || purchase.UpstreamID != "44" || purchase.CanGetAnotherSMS != true {
		t.Fatalf("SMSBower V2 购买错误: %#v, %v", purchase, err)
	}
	fallbackPurchase, err := client.Purchase(context.Background(), testAPIKey, PurchaseRequest{Country: "2", Service: "old"})
	if err != nil || fallbackPurchase.UpstreamID != "45" || fallbackPurchase.PhoneNumber != "78880001122" || !fallbackPurchase.CanGetAnotherSMS || numberV2Calls.Load() != 2 {
		t.Fatalf("SMSBower V2 降级错误: %#v, %v", fallbackPurchase, err)
	}
	poll, err := client.Poll(context.Background(), testAPIKey, "44")
	if err != nil || len(poll.Messages) != 2 || poll.Messages[0].Fingerprint != "m1" || poll.Code != "2222" {
		t.Fatalf("SMSBower 多短信轮询错误: %#v, %v", poll, err)
	}
	fallbackPoll, err := client.Poll(context.Background(), testAPIKey, "fallback")
	if err != nil || fallbackPoll.Code != "8899" || fallbackPoll.State != PollReceived {
		t.Fatalf("SMSBower getStatus 降级错误: %#v, %v", fallbackPoll, err)
	}
	if _, err := client.RequestAnother(context.Background(), testAPIKey, "44"); err != nil {
		t.Fatalf("SMSBower 再收短信失败: %v", err)
	}
	if err := client.Complete(context.Background(), testAPIKey, "44"); err != nil {
		t.Fatalf("SMSBower 完成失败: %v", err)
	}
	if err := client.Cancel(context.Background(), testAPIKey, "44"); err != nil {
		t.Fatalf("SMSBower 取消失败: %v", err)
	}
}

func TestSMSPoolFormAPIAndLifecycle(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		if request.Method != http.MethodPost {
			http.Error(writer, `{"type":"BAD_METHOD"}`, http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data;") {
			t.Errorf("SMSPool Content-Type=%q", request.Header.Get("Content-Type"))
		}
		assertHeader(t, request, "Authorization", "Bearer "+testAPIKey)
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("SMSPool form 解析失败: %v", err)
		}
		if request.Form.Get("key") != testAPIKey {
			t.Errorf("SMSPool body key=%q", request.Form.Get("key"))
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/country/retrieve_all":
			_, _ = writer.Write([]byte(`[{"ID":1,"name":"United States","short_name":"US","region":"North America"}]`))
		case "/service/retrieve_all":
			if request.Form.Get("country") != "1" {
				t.Errorf("SMSPool service country=%q", request.Form.Get("country"))
			}
			_, _ = writer.Write([]byte(`[{"ID":395,"name":"Discord","favourite":0}]`))
		case "/request/pricing":
			_, _ = writer.Write([]byte(`[{"country":1,"service":395,"name":"Discord","price":"0.24","stock":9}]`))
		case "/purchase/sms":
			if request.Form.Get("country") != "1" || request.Form.Get("service") != "395" || request.Form.Get("pool") != "7" || request.Form.Get("activation_type") != "SMS" {
				t.Errorf("SMSPool 购买 form 错误: %v", request.Form)
			}
			_, _ = writer.Write([]byte(`{"success":1,"number":14155552671,"cc":"1","phonenumber":"4155552671","order_id":"ORDER-1","expires_in":1200,"cost":"0.24"}`))
		case "/sms/check":
			_, _ = writer.Write([]byte(`{"status":3,"sms":"12345","full_sms":"Code 12345","resend":2,"received_at":"2026-09-01T12:00:05Z","expiration":1788265200}`))
		case "/sms/check_resend":
			_, _ = writer.Write([]byte(`{"success":1,"message":"available","resends":2,"resendCost":0}`))
		case "/sms/resend":
			_, _ = writer.Write([]byte(`{"success":1,"message":"requested","order_id":"ORDER-1","charge":0}`))
		case "/sms/cancel":
			_, _ = writer.Write([]byte(`{"success":1,"message":"cancelled"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewSMSPool(server.URL)
	countries, err := client.Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogCountry})
	if err != nil || len(countries) != 1 || countries[0].Code != "1" || countries[0].Name != "United States" {
		t.Fatalf("SMSPool 国家目录错误: %#v, %v", countries, err)
	}
	services, err := client.Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogService, Country: "1"})
	if err != nil || len(services) != 1 || services[0].Code != "395" {
		t.Fatalf("SMSPool 服务目录错误: %#v, %v", services, err)
	}
	prices, err := client.Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: CatalogPrice, Country: "1", Service: "395"})
	if err != nil || len(prices) != 1 || prices[0].Price == nil || *prices[0].Price != 0.24 || prices[0].Stock == nil || *prices[0].Stock != 9 {
		t.Fatalf("SMSPool 价格目录错误: %#v, %v", prices, err)
	}
	purchase, err := client.Purchase(context.Background(), testAPIKey, PurchaseRequest{Country: "1", Service: "395", Pool: "7"})
	if err != nil || purchase.UpstreamID != "ORDER-1" || purchase.PhoneNumber != "14155552671" || purchase.Cost != 0.24 || purchase.ExpiresAt == nil || !purchase.CanGetAnotherSMS {
		t.Fatalf("SMSPool 购买错误: %#v, %v", purchase, err)
	}
	poll, err := client.Poll(context.Background(), testAPIKey, "ORDER-1")
	if err != nil || poll.State != PollReceived || poll.Code != "12345" || len(poll.Messages) != 1 || poll.Messages[0].Generation != 2 || poll.Messages[0].ReceivedAt.IsZero() {
		t.Fatalf("SMSPool 轮询错误: %#v, %v", poll, err)
	}
	firstFingerprint := poll.Messages[0].Fingerprint
	if firstFingerprint == "" {
		t.Fatal("SMSPool 消息缺少幂等指纹")
	}
	beforeComplete := requestCount.Load()
	if err := client.Complete(context.Background(), testAPIKey, "ORDER-1"); err != nil {
		t.Fatalf("SMSPool 本地结算 no-op 错误: %v", err)
	}
	if requestCount.Load() != beforeComplete {
		t.Fatal("SMSPool Complete 调用了未定义的远端端点")
	}
	if _, err := client.RequestAnother(context.Background(), testAPIKey, "ORDER-1"); err != nil {
		t.Fatalf("SMSPool 检查并请求下一条失败: %v", err)
	}
	if err := client.Cancel(context.Background(), testAPIKey, "ORDER-1"); err != nil {
		t.Fatalf("SMSPool 取消失败: %v", err)
	}
}

func TestSMSPoolRequestAnotherParsesChargeAndFallsBackToQuotedCost(t *testing.T) {
	tests := []struct {
		name         string
		resendBody   string
		expectedCost float64
	}{
		{name: "使用 resend charge", resendBody: `{"success":1,"charge":"0.37"}`, expectedCost: 0.37},
		{name: "缺少 charge 使用预报价", resendBody: `{"success":1,"message":"requested"}`, expectedCost: 0.42},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var checkCalls, resendCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if err := request.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("解析 SMSPool 请求失败: %v", err)
				}
				if request.Form.Get("key") != testAPIKey || request.Form.Get("orderid") != "ORDER-CHARGE" {
					t.Errorf("SMSPool 请求字段错误: %v", request.Form)
				}
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/sms/check_resend":
					checkCalls.Add(1)
					_, _ = writer.Write([]byte(`{"success":1,"resends":1,"resendCost":"0.42"}`))
				case "/sms/resend":
					resendCalls.Add(1)
					_, _ = writer.Write([]byte(test.resendBody))
				default:
					http.NotFound(writer, request)
				}
			}))
			t.Cleanup(server.Close)

			result, err := NewSMSPool(server.URL).RequestAnother(context.Background(), testAPIKey, "ORDER-CHARGE")
			if err != nil {
				t.Fatal(err)
			}
			if result.Charge != test.expectedCost {
				t.Fatalf("续码费用=%v，期望=%v", result.Charge, test.expectedCost)
			}
			if checkCalls.Load() != 1 || resendCalls.Load() != 1 {
				t.Fatalf("预检/续码调用次数异常: check=%d resend=%d", checkCalls.Load(), resendCalls.Load())
			}
		})
	}
}

func TestProviderTimeoutAndAPIKeyRedaction(t *testing.T) {
	t.Run("超时受控", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			select {
			case <-request.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
		}))
		defer server.Close()
		client := NewSMSPool(server.URL, WithTimeout(25*time.Millisecond))
		_, err := client.Poll(context.Background(), testAPIKey, "ORDER-1")
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("期望 DeadlineExceeded，实际 %v", err)
		}
		var providerErr *ProviderError
		if !errors.As(err, &providerErr) || !providerErr.Retryable || providerErr.Code != "TIMEOUT" {
			t.Fatalf("超时错误分类错误: %#v", providerErr)
		}
	})

	t.Run("HTTP错误不泄漏密钥", func(t *testing.T) {
		secret := "Secret-Key-ABC123"
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(fmt.Sprintf(`{"error":"upstream echoed %s"}`, secret)))
		}))
		defer server.Close()
		client := NewSMSBower(server.URL + "/handler_api.php")
		_, err := client.Poll(context.Background(), secret, "1")
		if err == nil {
			t.Fatal("期望供应商错误")
		}
		visible := strings.ToLower(fmt.Sprintf("%+v", err))
		if strings.Contains(visible, strings.ToLower(secret)) || strings.Contains(visible, strings.ToLower(sanitizeCode(secret))) {
			t.Fatalf("错误泄漏 API key: %s", visible)
		}
		var providerErr *ProviderError
		if !errors.As(err, &providerErr) || providerErr.Code != "UPSTREAM_ERROR" {
			t.Fatalf("密钥回显未安全归类: %#v", providerErr)
		}
	})

	t.Run("业务错误不泄漏密钥", func(t *testing.T) {
		secret := "Business-Key-XYZ789"
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_, _ = writer.Write([]byte(`{"success":0,"type":"Business-Key-XYZ789"}`))
		}))
		defer server.Close()
		client := NewSMSPool(server.URL)
		_, err := client.Purchase(context.Background(), secret, PurchaseRequest{Country: "1", Service: "2"})
		if err == nil || strings.Contains(strings.ToLower(err.Error()), strings.ToLower(secret)) {
			t.Fatalf("业务错误密钥脱敏失败: %v", err)
		}
	})
}

func TestProviderFactoryAndValidation(t *testing.T) {
	aliases := map[string]string{
		"hero-sms":  "herosms",
		"smsbrower": "smsbower",
		"sms-pool":  "smspool",
	}
	for alias, expected := range aliases {
		client, err := New(alias, "http://example.invalid")
		if err != nil || client.ID() != expected {
			t.Fatalf("New(%q)=%v,%v，期望 %q", alias, client, err, expected)
		}
	}
	if _, err := New("unknown", "http://example.invalid"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("未知供应商错误=%v", err)
	}
	client := NewSMSPool("http://example.invalid")
	if _, err := client.Catalog(context.Background(), testAPIKey, CatalogRequest{Kind: "unknown"}); !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("未知目录错误=%v", err)
	}
	if _, err := client.Purchase(context.Background(), "", PurchaseRequest{Country: "1", Service: "2"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("空密钥错误=%v", err)
	}
}

func assertHeader(t *testing.T, request *http.Request, key, expected string) {
	t.Helper()
	if actual := request.Header.Get(key); actual != expected {
		t.Errorf("请求头 %s=%q，期望 %q", key, actual, expected)
	}
}
