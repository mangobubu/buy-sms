package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSMSPoolCancelNormalizesTemporaryLockResponse(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/sms/cancel" {
					t.Errorf("SMSPool取消请求=%s %s", request.Method, request.URL.Path)
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(status)
				_, _ = writer.Write([]byte(`{"success":0,"message":"Your order cannot be cancelled yet, please try again later."}`))
			}))
			t.Cleanup(server.Close)

			const secret = "provider-secret-cancel-lock"
			err := NewSMSPool(server.URL).Cancel(context.Background(), secret, "order-123")
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) {
				t.Fatalf("错误类型=%T，期望 ProviderError: %v", err, err)
			}
			if providerErr.Code != CodeCancelNotAvailableYet || providerErr.Operation != "cancel" {
				t.Fatalf("取消等待错误未规范化: %+v", providerErr)
			}
			if !providerErr.Retryable {
				t.Fatalf("取消等待错误应标记为可重试: %+v", providerErr)
			}
			if status == http.StatusBadRequest && providerErr.HTTPStatus != status {
				t.Fatalf("HTTP 状态=%d，期望=%d", providerErr.HTTPStatus, status)
			}
			text := providerErr.Error()
			if strings.Contains(text, secret) || strings.Contains(text, smsPoolCancelNotAvailableMessage) {
				t.Fatalf("错误文本泄漏密钥或上游消息: %q", text)
			}
		})
	}
}

func TestSMSBowerEarlyCancelDeniedIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("action") != "setStatus" || request.URL.Query().Get("status") != "8" {
			t.Errorf("SMSBower取消参数错误: %s", request.URL.RawQuery)
		}
		_, _ = writer.Write([]byte("EARLY_CANCEL_DENIED"))
	}))
	t.Cleanup(server.Close)

	err := NewSMSBower(server.URL+"/stubs/handler_api.php").Cancel(context.Background(), "provider-secret", "order-123")
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("错误类型=%T，期望 ProviderError: %v", err, err)
	}
	if providerErr.Code != "EARLY_CANCEL_DENIED" || !providerErr.Retryable {
		t.Fatalf("SMSBower早期取消错误应可重试: %+v", providerErr)
	}
}

func TestSMSPoolCancelKeepsOrdinaryFailureGeneric(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"success":0,"message":"account-specific internal detail"}`))
	}))
	t.Cleanup(server.Close)

	err := NewSMSPool(server.URL).Cancel(context.Background(), "provider-secret", "order-123")
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code != "UPSTREAM_REJECTED" {
		t.Fatalf("普通失败错误=%v，期望通用 ProviderError", err)
	}
	if strings.Contains(providerErr.Error(), "account-specific") {
		t.Fatalf("普通失败泄漏任意消息: %q", providerErr.Error())
	}
}
