package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSMSPinAccountCatalogPurchaseAndPoll(t *testing.T) {
	const apiKey = "sk_test_sms_pin"
	pollCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != apiKey {
			t.Errorf("X-API-Key=%q, want %q", got, apiKey)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/account":
			_, _ = w.Write([]byte(`{"balance":12.30}`))
		case "GET /api/v1/countries":
			_, _ = w.Write([]byte(`{"countries":[{"code":"US","name":"United States","flag":"🇺🇸"}]}`))
		case "GET /api/v1/services":
			_, _ = w.Write([]byte(`{"services":[{"code":"wa","name":"WhatsApp"}]}`))
		case "GET /api/v1/numbers":
			if r.URL.Query().Get("country") != "US" || r.URL.Query().Get("service") != "wa" {
				t.Errorf("numbers query=%v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{"countries":[{"code":"US","available":7}],"price":0.15}`))
		case "POST /api/v1/orders":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("order body: %v", err)
			}
			if body["country"] != "US" || body["service"] != "wa" {
				t.Errorf("order body=%v", body)
			}
			_, _ = w.Write([]byte(`{"id":"order-1","phoneNumber":"+15551234567","country":"US","service":"wa","status":"active","price":0.15,"expiresAt":"2030-01-01T00:00:00Z"}`))
		case "GET /api/v1/orders/order-1":
			pollCount++
			if pollCount == 1 {
				_, _ = w.Write([]byte(`{"id":"order-1","status":"active","expiresAt":"2030-01-01T00:00:00Z"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"order-1","status":"completed","otpCode":"123456","message":"Your code is 123456"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := NewSMSPin(server.URL + "/api/v1")

	balance, err := client.Balance(context.Background(), apiKey)
	if err != nil || balance.Amount != "12.30" || balance.Currency != "USD" {
		t.Fatalf("balance=%+v err=%v", balance, err)
	}
	countries, err := client.Catalog(context.Background(), apiKey, CatalogRequest{Kind: CatalogCountry})
	if err != nil || len(countries) != 1 || countries[0].Code != "US" {
		t.Fatalf("countries=%+v err=%v", countries, err)
	}
	services, err := client.Catalog(context.Background(), apiKey, CatalogRequest{Kind: CatalogService})
	if err != nil || len(services) != 1 || services[0].Code != "wa" {
		t.Fatalf("services=%+v err=%v", services, err)
	}
	prices, err := client.Catalog(context.Background(), apiKey, CatalogRequest{Kind: CatalogPrice, Country: "US", Service: "wa"})
	if err != nil || len(prices) != 1 || prices[0].Price == nil || *prices[0].Price != 0.15 || prices[0].Stock == nil || *prices[0].Stock != 7 {
		t.Fatalf("prices=%+v err=%v", prices, err)
	}
	purchase, err := client.Purchase(context.Background(), apiKey, PurchaseRequest{Country: "US", Service: "wa"})
	if err != nil || purchase.UpstreamID != "order-1" || purchase.PhoneNumber != "+15551234567" || purchase.Cost != 0.15 || purchase.ExpiresAt == nil {
		t.Fatalf("purchase=%+v err=%v", purchase, err)
	}
	waiting, err := client.Poll(context.Background(), apiKey, "order-1")
	if err != nil || waiting.State != PollWaiting {
		t.Fatalf("waiting=%+v err=%v", waiting, err)
	}
	received, err := client.Poll(context.Background(), apiKey, "order-1")
	if err != nil || received.State != PollCompleted || received.Code != "123456" || len(received.Messages) != 1 {
		t.Fatalf("received=%+v err=%v", received, err)
	}
}
