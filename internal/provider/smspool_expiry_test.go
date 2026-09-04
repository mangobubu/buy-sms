package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSMSPoolPollUsesTimeLeftOnlyWhenAbsoluteExpiryIsMissing(t *testing.T) {
	absolute := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			_, _ = writer.Write([]byte(`{"status":1,"time_left":"75"}`))
		case 2:
			_, _ = writer.Write([]byte(`{"status":1,"expiration":1893456000,"time_left":999}`))
		case 3:
			_, _ = writer.Write([]byte(`{"status":1,"time_left":0}`))
		default:
			_, _ = writer.Write([]byte(`{"status":1,"time_left":-1}`))
		}
	}))
	t.Cleanup(server.Close)
	client := NewSMSPool(server.URL)

	before := time.Now().UTC()
	result, err := client.Poll(context.Background(), "provider-secret", "order-time-left")
	after := time.Now().UTC()
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiresAt == nil ||
		result.ExpiresAt.Before(before.Add(75*time.Second)) ||
		result.ExpiresAt.After(after.Add(75*time.Second)) {
		t.Fatalf("time_left 换算期限=%v，不在预期区间 [%s,%s]", result.ExpiresAt, before.Add(75*time.Second), after.Add(75*time.Second))
	}

	result, err = client.Poll(context.Background(), "provider-secret", "order-absolute")
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiresAt == nil || !result.ExpiresAt.Equal(absolute) {
		t.Fatalf("绝对期限未优先使用: got=%v want=%s", result.ExpiresAt, absolute)
	}

	before = time.Now().UTC()
	result, err = client.Poll(context.Background(), "provider-secret", "order-zero")
	after = time.Now().UTC()
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiresAt == nil || result.ExpiresAt.Before(before) || result.ExpiresAt.After(after) {
		t.Fatalf("time_left=0 应表示即时到期: got=%v range=[%s,%s]", result.ExpiresAt, before, after)
	}

	result, err = client.Poll(context.Background(), "provider-secret", "order-negative")
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiresAt != nil {
		t.Fatalf("负数 time_left 不应生成期限: %v", result.ExpiresAt)
	}
}

func TestPurchaseExpiryInZeroMeansImmediateExpiry(t *testing.T) {
	before := time.Now().UTC()
	result, err := purchaseResultFromValue(map[string]any{
		"id": "purchase-zero", "phone": "14155550123", "expires_in": 0,
	}, nil)
	after := time.Now().UTC()
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiresAt == nil || result.ExpiresAt.Before(before) || result.ExpiresAt.After(after) {
		t.Fatalf("expires_in=0 应表示即时到期: got=%v range=[%s,%s]", result.ExpiresAt, before, after)
	}

	result, err = purchaseResultFromValue(map[string]any{
		"id": "purchase-negative", "phone": "14155550456", "expires_in": -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiresAt != nil {
		t.Fatalf("负数 expires_in 不应生成期限: %v", result.ExpiresAt)
	}
}
