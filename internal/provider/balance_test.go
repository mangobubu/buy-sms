package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderBalances(t *testing.T) {
	t.Run("HeroSMS", func(t *testing.T) {
		tests := []struct {
			name       string
			body       string
			wantAmount string
			wantCode   string
		}{
			{name: "成功并保留尾随零", body: "ACCESS_BALANCE:12.3400", wantAmount: "12.3400"},
			{name: "零余额", body: "ACCESS_BALANCE:0.000", wantAmount: "0.000"},
			{name: "供应商错误", body: "BAD_KEY", wantCode: "BAD_KEY"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				server := newLegacyBalanceServer(t, test.body)
				client := NewHeroSMS(server.URL + "/api/v1")
				assertBalanceResult(t, client, test.wantAmount, test.wantCode)
			})
		}
	})

	t.Run("SMSBower", func(t *testing.T) {
		tests := []struct {
			name       string
			body       string
			wantAmount string
			wantCode   string
		}{
			{name: "成功并保留尾随零", body: "ACCESS_BALANCE:7.500", wantAmount: "7.500"},
			{name: "零余额", body: "ACCESS_BALANCE:0", wantAmount: "0"},
			{name: "供应商错误", body: "NO_BALANCE", wantCode: "NO_BALANCE"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				server := newLegacyBalanceServer(t, test.body)
				client := NewSMSBower(server.URL + "/stubs/handler_api.php")
				assertBalanceResult(t, client, test.wantAmount, test.wantCode)
			})
		}
	})

	t.Run("SMSPool", func(t *testing.T) {
		tests := []struct {
			name       string
			body       string
			wantAmount string
			wantCode   string
		}{
			{name: "成功并保留尾随零", body: `{"success":1,"balance":"9.8700"}`, wantAmount: "9.8700"},
			{name: "零余额及数字原文", body: `{"balance":0.00}`, wantAmount: "0.00"},
			{name: "供应商错误", body: `{"success":0,"type":"RATE_LIMIT"}`, wantCode: "RATE_LIMIT"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method != http.MethodPost || request.URL.Path != "/request/balance" {
						t.Errorf("SMSPool 余额请求=%s %s", request.Method, request.URL.Path)
					}
					if err := request.ParseMultipartForm(1 << 20); err != nil {
						t.Errorf("SMSPool 余额表单解析失败: %v", err)
					}
					if request.Form.Get("key") != testAPIKey {
						t.Errorf("SMSPool 余额密钥=%q", request.Form.Get("key"))
					}
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(test.body))
				}))
				t.Cleanup(server.Close)
				assertBalanceResult(t, NewSMSPool(server.URL), test.wantAmount, test.wantCode)
			})
		}
	})
}

func TestBalanceRejectsInvalidDecimalFormats(t *testing.T) {
	invalid := []string{"", "-1", "-0", "+1", ".5", "1.", "1e3", "NaN", "Inf", "Infinity", "1,000", " 1.00", "1.00 ", strings.Repeat("9", 65)}
	for _, value := range invalid {
		t.Run(strings.ReplaceAll(value, " ", "space"), func(t *testing.T) {
			if amount, ok := validBalanceAmount(value); ok {
				t.Fatalf("非法余额 %q 被接受为 %q", value, amount)
			}
		})
	}
}

func newLegacyBalanceServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/stubs/handler_api.php" {
			t.Errorf("兼容余额请求=%s %s", request.Method, request.URL.Path)
		}
		if request.URL.Query().Get("action") != "getBalance" || request.URL.Query().Get("api_key") != testAPIKey {
			t.Errorf("兼容余额参数=%s", request.URL.RawQuery)
		}
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func assertBalanceResult(t *testing.T, client Client, wantAmount, wantCode string) {
	t.Helper()
	result, err := client.Balance(context.Background(), testAPIKey)
	if wantCode == "" {
		if err != nil || result.Amount != wantAmount || result.Currency != "USD" {
			t.Fatalf("余额=%#v err=%v，期望 amount=%q currency=USD", result, err, wantAmount)
		}
		return
	}
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code != wantCode || providerErr.Operation != "balance" {
		t.Fatalf("余额错误=%#v err=%v，期望 code=%s operation=balance", providerErr, err, wantCode)
	}
}
