package provider

import (
	"context"
	"encoding/json"
	"errors"
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
			_, _ = w.Write([]byte(`{"countries":[{"code":"US","available":7}],"price":0.15}`))
		case "GET /api/v1/operators":
			if r.URL.Query().Get("country") != "US" || r.URL.Query().Get("service") != "wa" {
				t.Errorf("operators query=%v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{"operators":[{"operator":2,"label":"Operator 2","price":0.15,"count":7}]}`))
		case "POST /api/v1/orders":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("order body: %v", err)
			}
			if body["country"] != "US" || body["service"] != "wa" {
				t.Errorf("order body=%v", body)
			}
			if operator, ok := body["operator"].(float64); !ok || operator != 2 {
				t.Errorf("order operator=%v", body["operator"])
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
	if err != nil || len(prices) != 1 || prices[0].Price == nil || *prices[0].Price != 0.15 || prices[0].Stock == nil || *prices[0].Stock != 7 || len(prices[0].PriceOptions) != 1 || prices[0].PriceOptions[0].Operator != 2 {
		t.Fatalf("prices=%+v err=%v", prices, err)
	}
	purchase, err := client.Purchase(context.Background(), apiKey, PurchaseRequest{Country: "US", Service: "wa", Operator: "2"})
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

func TestSMSPinOperatorsCatalogFiltersInvalidInventoryAndKeepsOperators(t *testing.T) {
	client := NewSMSPin("https://smspin.io/api/v1")
	items, err := client.parseOperatorsCatalog([]byte(`{"operators":[
		{"operator":2,"label":"Operator 2","price":0.15,"count":3},
		{"operator":1,"label":"Operator 1","price":0.20,"count":4},
		{"operator":3,"label":"empty","price":0.10,"count":0},
		{"operator":"2.0","label":"decimal","price":0.01,"count":5},
		{"operator":2,"label":"Operator 2 duplicate","price":0.15,"count":2}
	]}`), CatalogRequest{Kind: CatalogPrice, Country: "US", Service: "wa"})
	if err != nil {
		t.Fatalf("parse operators err=%v", err)
	}
	if len(items) != 1 || len(items[0].PriceOptions) != 2 {
		t.Fatalf("items=%+v", items)
	}
	if got := items[0].PriceOptions[0]; got.Operator != 2 || got.Available != 3 || got.Label != "Operator 2" {
		t.Fatalf("operator 2=%+v", got)
	}
	if got := items[0].PriceOptions[1]; got.Operator != 1 || got.Available != 4 {
		t.Fatalf("operator 1=%+v", got)
	}
	if items[0].Stock == nil || *items[0].Stock != 7 {
		t.Fatalf("aggregate stock=%v", items[0].Stock)
	}
}

func TestSMSPinOperatorsCatalogKeepsEmptyInventory(t *testing.T) {
	client := NewSMSPin("https://smspin.io/api/v1")
	for _, payload := range []string{`{"operators":[]}`, `{"operators":[{"operator":1,"price":0.1,"count":0}]}`} {
		items, err := client.parseOperatorsCatalog([]byte(payload), CatalogRequest{Kind: CatalogPrice, Country: "US", Service: "wa"})
		if err != nil || len(items) != 1 || items[0].Price != nil || items[0].Stock == nil || *items[0].Stock != 0 || len(items[0].PriceOptions) != 0 {
			t.Fatalf("empty inventory items=%+v err=%v", items, err)
		}
	}
}

func TestSMSPinOperatorsCatalogEmptyInventoryDoesNotFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/operators" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"operators":[{"operator":1,"price":0.1,"count":0}]}`))
	}))
	t.Cleanup(server.Close)
	client := NewSMSPin(server.URL)
	items, err := client.Catalog(context.Background(), "key", CatalogRequest{Kind: CatalogPrice, Country: "US", Service: "wa"})
	if err != nil || len(items) != 1 || items[0].Stock == nil || *items[0].Stock != 0 {
		t.Fatalf("empty inventory items=%+v err=%v", items, err)
	}
}

func TestSMSPinOperatorCatalogRejectsMalformedAndConflictingResponses(t *testing.T) {
	for _, payload := range []string{`{}`, `{"operators":null}`, `{"operators":{}}`, `{"operators":[{"operator":1,"price":0.1,"count":2},{"operator":1,"price":0.2,"count":3}]}`} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/operators" {
					t.Errorf("unexpected fallback: %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(payload))
			}))
			t.Cleanup(server.Close)
			_, err := NewSMSPin(server.URL).Catalog(context.Background(), "key", CatalogRequest{Kind: CatalogPrice, Country: "VN", Service: "hc"})
			var upstream *ProviderError
			if !errors.As(err, &upstream) || upstream.Code != "INVALID_RESPONSE" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestSMSPinOperatorsKeepSamePriceDistinctRoutes(t *testing.T) {
	items, err := NewSMSPin("").parseOperatorsCatalog([]byte(`{"operators":[{"operator":758,"label":"Operator 758","price":0.1765,"count":99988},{"operator":1,"label":"Operator 1","price":0.1765,"count":67}]}`), CatalogRequest{Kind: CatalogPrice, Country: "VN", Service: "hc"})
	if err != nil || len(items) != 1 || len(items[0].PriceOptions) != 2 || items[0].PriceOptions[0].Operator != 1 || items[0].PriceOptions[1].Operator != 758 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}
