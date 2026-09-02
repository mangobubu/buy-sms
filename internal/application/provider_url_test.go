package application

import (
	"context"
	"testing"

	"buysms/internal/domain"
)

func TestValidateProviderURLProductionAllowlist(t *testing.T) {
	invalid := []struct{ id, url string }{
		{domain.ProviderHeroSMS, "http://hero-sms.com/api/v1"},
		{domain.ProviderHeroSMS, "https://hero-sms.com:8443/api/v1"},
		{domain.ProviderHeroSMS, "https://hero-sms.com.attacker.example/api/v1"},
		{domain.ProviderSMSBower, "https://127.0.0.1/handler_api.php"},
		{domain.ProviderSMSPool, "https://user:pass@api.smspool.net/"},
	}
	for _, tc := range invalid {
		if _, err := validateProviderURL(context.Background(), tc.id, tc.url, true); err == nil {
			t.Errorf("生产地址应被拒绝: %s", tc.url)
		}
	}
}

func TestValidateProviderURLDevelopmentAllowsMock(t *testing.T) {
	u, err := validateProviderURL(context.Background(), domain.ProviderHeroSMS, "http://127.0.0.1:19091/api/v1", false)
	if err != nil || u.Host != "127.0.0.1:19091" {
		t.Fatalf("开发 mock 地址应允许: %v, %v", u, err)
	}
}
