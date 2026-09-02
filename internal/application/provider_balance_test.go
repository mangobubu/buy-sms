package application

import (
	"net/http"
	"testing"

	"buysms/internal/provider"
)

func TestProviderBalanceConfigurationError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "API Key 错误", err: &provider.ProviderError{Code: "BAD_KEY"}, want: true},
		{name: "HTTP 未认证", err: &provider.ProviderError{HTTPStatus: http.StatusUnauthorized}, want: true},
		{name: "HTTP 禁止访问", err: &provider.ProviderError{HTTPStatus: http.StatusForbidden}, want: true},
		{name: "供应商限流", err: &provider.ProviderError{Code: "RATE_LIMIT", HTTPStatus: http.StatusTooManyRequests}, want: false},
		{name: "供应商超时", err: &provider.ProviderError{Code: "TIMEOUT", Retryable: true}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := providerBalanceConfigurationError(test.err); got != test.want {
				t.Fatalf("配置错误判断=%v，期望=%v", got, test.want)
			}
		})
	}
}
