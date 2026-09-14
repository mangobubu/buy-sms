package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"buysms/internal/domain"
	"buysms/internal/provider"
	"buysms/internal/secure"
)

// TestSMSPinIsRegisteredInProviderFactory guards the application/provider
// wiring separately from the transport tests.  A provider that is implemented
// but omitted from provider.New would otherwise only fail at runtime when an
// administrator enables it.
func TestSMSPinIsRegisteredInProviderFactory(t *testing.T) {
	client, err := provider.New(domain.ProviderSMSPin, "https://smspin.io")
	if err != nil {
		t.Fatalf("SMSPin provider factory returned an error: %v", err)
	}
	if client == nil {
		t.Fatal("SMSPin provider factory returned a nil client")
	}
	if got := client.ID(); got != domain.ProviderSMSPin {
		t.Fatalf("SMSPin client ID=%q, want %q", got, domain.ProviderSMSPin)
	}
}

func TestSMSPinProviderFactoryRejectsUnknownProvider(t *testing.T) {
	if _, err := provider.New("unknown-provider", "https://example.invalid"); !errors.Is(err, provider.ErrInvalidRequest) {
		t.Fatalf("unknown provider error=%v, want ErrInvalidRequest", err)
	}
}

func TestSMSPinProductionURLAllowlistRejectsLookalikeHosts(t *testing.T) {
	for _, rawURL := range []string{
		"https://smspin.io.attacker.example/api",
		"https://attacker-smspin.io/api",
		"https://127.0.0.1/api",
		"http://smspin.io/api",
	} {
		t.Run(rawURL, func(t *testing.T) {
			if _, err := validateProviderURL(context.Background(), domain.ProviderSMSPin, rawURL, true); err == nil {
				t.Fatalf("lookalike/insecure SMSPin URL was accepted: %s", rawURL)
			}
		})
	}
}

func TestSMSPinDevelopmentURLAllowsLocalMock(t *testing.T) {
	server := httptest.NewServer(nil)
	t.Cleanup(server.Close)
	u, err := validateProviderURL(context.Background(), domain.ProviderSMSPin, server.URL, false)
	if err != nil {
		t.Fatalf("development SMSPin mock URL rejected: %v", err)
	}
	if u.String() != server.URL {
		t.Fatalf("development SMSPin URL=%q, want %q", u.String(), server.URL)
	}
}
func TestSMSPinProviderViewIsPollingOnly(t *testing.T) {
	vault, err := secure.NewVault([]byte("smspin-provider-view-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	tokenCipher, err := vault.Encrypt("webhook-token-for-test-1234567890")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{vault: vault}
	view, err := service.providerView(domain.Provider{
		ID: domain.ProviderSMSPin, Name: "SMSPin", BaseURL: "https://smspin.io/api/v1",
		WebhookConfigured: true, WebhookTokenCipher: tokenCipher,
		Config: json.RawMessage(`{"pollingIntervalSeconds":10,"webhookEnabled":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.WebhookSupported || view.WebhookEnabled || view.HasWebhookToken || view.WebhookURL != "" {
		t.Fatalf("SMSPin should be polling-only: %+v", view)
	}
	if view.PollingIntervalSeconds != 10 {
		t.Fatalf("polling interval=%d, want 10", view.PollingIntervalSeconds)
	}
}
