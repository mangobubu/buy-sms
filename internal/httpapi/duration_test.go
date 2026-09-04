package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"buysms/internal/application"
	"buysms/internal/domain"
)

func TestDurationCatalogRouteReturnsHeroSMSOptions(t *testing.T) {
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/classifiers/activations/custom-durations":
			_, _ = writer.Write([]byte(`{"data":{"tg":{"2":35}}}`))
		case "/stubs/handler_api.php":
			_, _ = writer.Write([]byte(`{"2":{"24":{"count":3,"price":1.25}}}`))
		case "/api/v1/activations/offers/sms":
			_, _ = writer.Write([]byte(`{"data":{"tg":{"2":{"prices":{"default":0.12,"min":0.12},"counts":{"defaultPrice":4},"map":{"0.12":4}}}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(providerServer.Close)

	repo := newMemoryRepository()
	router, _, vault := newTestRouter(t, repo)
	cipher, err := vault.Encrypt("duration-http-api-key")
	if err != nil {
		t.Fatal(err)
	}
	repo.putProvider(domain.Provider{
		ID: domain.ProviderHeroSMS, Name: "HeroSMS", Enabled: true,
		BaseURL: providerServer.URL + "/api/v1", APIKeyCipher: cipher, APIKeyConfigured: true,
	})
	user := domain.User{ID: "duration-http-user", Username: "operator", Role: "operator", Active: true}
	repo.putSession([]byte("router-test-session-pepper"), "duration-http-token", user)

	response := performAuthenticatedRequest(
		router, http.MethodGet,
		"/api/catalog/durations?provider=herosms&country=2&service=tg",
		"duration-http-token",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("时长目录状态码=%d，响应=%s", response.Code, response.Body.String())
	}
	var options []application.DurationOptionDTO
	if err = json.Unmarshal(response.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	if len(options) != 2 || options[0].Value != "" || options[0].Minutes != 35 ||
		options[0].Price != "0.12" || options[1].Value != "24" || options[1].Price != "1.25" {
		t.Fatalf("时长目录响应错误: %#v", options)
	}
}
