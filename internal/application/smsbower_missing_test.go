package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/provider"
	"buysms/internal/secure"
)

type smsBowerMissingRepository struct {
	*terminalLockRepository
	pollUpdates int
}

func (r *smsBowerMissingRepository) UpdatePoll(_ context.Context, id, state string, next time.Time, failures int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID == id {
		r.order.LastProviderState = state
		r.order.NextPollAt = next
		r.order.PollFailures = failures
	}
	r.pollUpdates++
	return nil
}

func smsBowerMissingOrder() domain.Order {
	started := time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC)
	return domain.Order{
		ID: "missing-order", UserID: "operator-1", ProviderID: domain.ProviderSMSBower,
		UpstreamID: "missing-upstream", Status: domain.OrderActive, PhoneNumber: "+84999000111",
		Cost: 0.091, Currency: "USD", CreatedAt: started, ActivationStartedAt: started,
		Messages: []domain.SMSMessage{{ID: "received-message", Code: "654321", Text: "Your code is 654321", ReceivedAt: started.Add(time.Minute)}},
	}
}

func newSMSBowerMissingService(t *testing.T, order domain.Order, handler http.Handler) (*Service, *smsBowerMissingRepository) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	vault, err := secure.NewVault([]byte("smsbower-missing-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := vault.Encrypt("smsbower-provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	repo := &smsBowerMissingRepository{terminalLockRepository: &terminalLockRepository{
		provider: domain.Provider{ID: domain.ProviderSMSBower, BaseURL: server.URL + "/stubs/handler_api.php", APIKeyCipher: key},
		order:    order,
	}}
	service := New(repo, nil, vault, config.Config{})
	service.now = func() time.Time { return order.CreatedAt.Add(time.Hour) }
	return service, repo
}

func TestSMSBowerFinishMissingActivation(t *testing.T) {
	for _, tt := range []struct {
		name         string
		message      string
		finishBody   string
		confirmBody  string
		finishHTTP   int
		wantStatus   string
		wantState    string
		wantConfirms int32
		wantError    bool
	}{
		{name: "confirmed missing with current message", message: "current", finishBody: "NO_ACTIVATION", confirmBody: "NO_ACTIVATION", wantStatus: domain.OrderCompleted, wantState: "upstream_missing", wantConfirms: 1},
		{name: "confirmed missing without messages", finishBody: "NO_ACTIVATION", confirmBody: "NO_ACTIVATION", wantStatus: domain.OrderActive, wantConfirms: 1, wantError: true},
		{name: "historical message does not complete new activation", message: "historical", finishBody: "NO_ACTIVATION", confirmBody: "NO_ACTIVATION", wantStatus: domain.OrderActive, wantConfirms: 1, wantError: true},
		{name: "raw missing but activation still exists", message: "current", finishBody: "NO_ACTIVATION", confirmBody: "STATUS_WAIT_CODE", wantStatus: domain.OrderActive, wantConfirms: 1, wantError: true},
		{name: "confirmation failed", message: "current", finishBody: "NO_ACTIVATION", confirmBody: "BAD_KEY", wantStatus: domain.OrderActive, wantConfirms: 1, wantError: true},
		{name: "ordinary provider failure", message: "current", finishBody: "BAD_STATUS", wantStatus: domain.OrderActive, wantError: true},
		{name: "HTTP failure is not missing proof", message: "current", finishBody: "NO_ACTIVATION", finishHTTP: http.StatusServiceUnavailable, wantStatus: domain.OrderActive, wantError: true},
		{name: "regular completion keeps user state", message: "current", finishBody: "ACCESS_ACTIVATION", wantStatus: domain.OrderCompleted, wantState: "user_complete"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			order := smsBowerMissingOrder()
			if tt.message == "" {
				order.Messages = nil
			} else if tt.message == "historical" {
				order.Messages[0].ReceivedAt = order.ActivationStartedAt.Add(-time.Minute)
			}
			var completes, confirms atomic.Int32
			service, repo := newSMSBowerMissingService(t, order, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("id") != order.UpstreamID {
					t.Errorf("unexpected upstream activation")
				}
				switch r.URL.Query().Get("action") {
				case "setStatus":
					completes.Add(1)
					if r.URL.Query().Get("status") != "6" {
						t.Errorf("unexpected completion status")
					}
					if tt.finishHTTP != 0 {
						w.WriteHeader(tt.finishHTTP)
					}
					_, _ = w.Write([]byte(tt.finishBody))
				case "getStatus":
					confirms.Add(1)
					_, _ = w.Write([]byte(tt.confirmBody))
				default:
					t.Errorf("unexpected action %q", r.URL.Query().Get("action"))
					http.NotFound(w, r)
				}
			}))
			view, err := service.FinishOrder(context.Background(), order.ID, "complete", domain.User{ID: order.UserID, Role: "operator"}, "127.0.0.1")
			if (err != nil) != tt.wantError || tt.wantError && !errors.Is(err, ErrProvider) {
				t.Fatalf("completion error=%v, wantError=%v", err, tt.wantError)
			}
			after, transitions, audits, locks := repo.snapshot()
			if after.Status != tt.wantStatus || after.LastProviderState != tt.wantState {
				t.Fatalf("status/state=%s/%s, want %s/%s", after.Status, after.LastProviderState, tt.wantStatus, tt.wantState)
			}
			if after.Cost != order.Cost || after.Currency != order.Currency || !reflect.DeepEqual(after.Messages, order.Messages) {
				t.Fatal("completion changed cost, currency or saved messages")
			}
			if completes.Load() != 1 || confirms.Load() != tt.wantConfirms || locks != 1 {
				t.Fatalf("calls: complete=%d confirm=%d locks=%d", completes.Load(), confirms.Load(), locks)
			}
			if tt.wantError {
				if len(transitions) != 0 || audits != 0 {
					t.Fatal("unconfirmed completion changed local state or audit")
				}
			} else if len(transitions) != 1 || audits != 1 || view.Status != domain.OrderCompleted || len(view.Messages) != len(order.Messages) {
				t.Fatal("successful completion must persist and return the completed order with messages")
			}
		})
	}
}

func TestSMSBowerAutoFinishMissingActivation(t *testing.T) {
	for _, tt := range []struct {
		name        string
		confirmBody string
		wantStatus  string
		wantState   string
	}{
		{name: "confirmed missing completes after deadline", confirmBody: "NO_ACTIVATION", wantStatus: domain.OrderCompleted, wantState: "upstream_missing"},
		{name: "failed confirmation remains active", confirmBody: "BAD_KEY", wantStatus: domain.OrderActive, wantState: provider.PollWaiting},
	} {
		t.Run(tt.name, func(t *testing.T) {
			order := smsBowerMissingOrder()
			var historyCalls, completes, confirms atomic.Int32
			service, repo := newSMSBowerMissingService(t, order, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Query().Get("action") {
				case "getAllSms":
					historyCalls.Add(1)
					_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
				case "setStatus":
					completes.Add(1)
					if r.URL.Query().Get("status") != "6" {
						t.Error("received-message order must complete, never cancel")
					}
					_, _ = w.Write([]byte("NO_ACTIVATION"))
				case "getStatus":
					confirms.Add(1)
					_, _ = w.Write([]byte(tt.confirmBody))
				default:
					t.Errorf("unexpected action %q", r.URL.Query().Get("action"))
					http.NotFound(w, r)
				}
			}))
			service.pollOne(context.Background(), order)
			after, transitions, audits, locks := repo.snapshot()
			if after.Status != tt.wantStatus || after.LastProviderState != tt.wantState ||
				after.Cost != order.Cost || after.Currency != order.Currency || !reflect.DeepEqual(after.Messages, order.Messages) {
				t.Fatalf("unexpected auto completion status/state=%s/%s or changed saved order", after.Status, after.LastProviderState)
			}
			if historyCalls.Load() != 1 || completes.Load() != 1 || confirms.Load() != 1 || locks != 1 {
				t.Fatalf("unexpected automatic action counts: history=%d complete=%d confirm=%d locks=%d", historyCalls.Load(), completes.Load(), confirms.Load(), locks)
			}
			if tt.wantStatus == domain.OrderCompleted {
				if len(transitions) != 1 || audits != 1 || repo.pollUpdates != 0 {
					t.Fatal("confirmed automatic completion must transition and audit exactly once")
				}
				// A stale queued poll may still read history, but must never repeat completion or confirmation.
				service.pollOne(context.Background(), order)
				_, transitions, audits, _ = repo.snapshot()
				if completes.Load() != 1 || confirms.Load() != 1 || len(transitions) != 1 || audits != 1 {
					t.Fatal("stale automatic poll repeated the terminal action")
				}
			} else if len(transitions) != 0 || audits != 0 || repo.pollUpdates != 1 || after.PollFailures != order.PollFailures+1 {
				t.Fatal("unconfirmed automatic completion must retain active state and failure backoff")
			}
		})
	}
}

func TestSMSBowerMissingCompletionRequiresStrictProof(t *testing.T) {
	confirmed := provider.ProviderError{Provider: domain.ProviderSMSBower, Operation: "complete", Code: provider.CodeActivationMissing}
	for _, tt := range []struct {
		name   string
		change func(*domain.Order, *provider.ProviderError)
	}{
		{name: "other order provider", change: func(o *domain.Order, _ *provider.ProviderError) { o.ProviderID = domain.ProviderHeroSMS }},
		{name: "terminal order", change: func(o *domain.Order, _ *provider.ProviderError) { o.Status = domain.OrderCanceled }},
		{name: "renewal inflight", change: func(o *domain.Order, _ *provider.ProviderError) { o.RenewalInflight = true }},
		{name: "no current messages", change: func(o *domain.Order, _ *provider.ProviderError) { o.Messages = nil }},
		{name: "other error provider", change: func(_ *domain.Order, e *provider.ProviderError) { e.Provider = domain.ProviderSMSPool }},
		{name: "other operation", change: func(_ *domain.Order, e *provider.ProviderError) { e.Operation = "poll" }},
		{name: "raw missing error", change: func(_ *domain.Order, e *provider.ProviderError) { e.Code = "NO_ACTIVATION" }},
		{name: "HTTP failure", change: func(_ *domain.Order, e *provider.ProviderError) { e.HTTPStatus = http.StatusBadGateway }},
		{name: "retryable error", change: func(_ *domain.Order, e *provider.ProviderError) { e.Retryable = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			order, upstream := smsBowerMissingOrder(), confirmed
			tt.change(&order, &upstream)
			if canCompleteMissingSMSBowerActivation(order, &upstream) {
				t.Fatal("insufficient proof must not allow local completion")
			}
		})
	}
	if !canCompleteMissingSMSBowerActivation(smsBowerMissingOrder(), fmt.Errorf("wrapped: %w", &confirmed)) {
		t.Fatal("wrapped confirmed proof should allow completion")
	}
	if canCompleteMissingSMSBowerActivation(smsBowerMissingOrder(), errors.New("ACTIVATION_MISSING")) {
		t.Fatal("untyped error must not allow completion")
	}
}

func TestSMSBowerPollMissingActivation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		message    string
		expired    bool
		pending    bool
		wantStatus string
		wantState  string
	}{
		{name: "current message completes missing activation", message: "current", wantStatus: domain.OrderCompleted, wantState: "upstream_missing"},
		{name: "current message takes precedence over local expiry", message: "current", expired: true, wantStatus: domain.OrderCompleted, wantState: "upstream_missing"},
		{name: "no messages and unknown expiry stays active", wantStatus: domain.OrderActive, wantState: "upstream_missing"},
		{name: "old activation message stays active", message: "historical", wantStatus: domain.OrderActive, wantState: "upstream_missing"},
		{name: "missing without message does not request another", pending: true, wantStatus: domain.OrderActive, wantState: "upstream_missing"},
		{name: "known expiry retains previous policy", expired: true, wantStatus: domain.OrderExpired, wantState: "local_expired"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			order := smsBowerMissingOrder()
			order.PollFailures = 3
			order.RequestNextPending = tt.pending
			if tt.message == "" {
				order.Messages = nil
			} else if tt.message == "historical" {
				order.Messages[0].ReceivedAt = order.ActivationStartedAt.Add(-time.Minute)
			}
			if tt.expired {
				expiresAt := order.CreatedAt.Add(30 * time.Minute)
				order.ExpiresAt = &expiresAt
			}
			var statusChecks atomic.Int32
			service, repo := newSMSBowerMissingService(t, order, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Query().Get("action") {
				case "getAllSms":
				case "getStatus":
					statusChecks.Add(1)
				default:
					t.Errorf("unexpected action %q", r.URL.Query().Get("action"))
				}
				_, _ = w.Write([]byte("NO_ACTIVATION"))
			}))
			service.pollOne(context.Background(), order)
			after, transitions, audits, locks := repo.snapshot()
			if after.Status != tt.wantStatus || after.LastProviderState != tt.wantState {
				t.Fatalf("status/state=%s/%s, want %s/%s", after.Status, after.LastProviderState, tt.wantStatus, tt.wantState)
			}
			if after.Cost != order.Cost || after.Currency != order.Currency || !reflect.DeepEqual(after.Messages, order.Messages) {
				t.Fatal("poll reconciliation changed cost, currency or saved messages")
			}
			if statusChecks.Load() != 2 || locks != 1 || audits != 0 {
				t.Fatalf("status checks=%d locks=%d audits=%d", statusChecks.Load(), locks, audits)
			}
			if tt.wantStatus == domain.OrderActive {
				if len(transitions) != 0 || repo.pollUpdates != 1 {
					t.Fatal("missing activation without current message must continue polling without a terminal transition")
				}
				if after.PollFailures != order.PollFailures+1 || !after.NextPollAt.Equal(service.now().Add(16*time.Second)) ||
					after.RequestNextPending != order.RequestNextPending {
					t.Fatal("missing without current message must retain failure backoff without requesting another SMS")
				}
			} else if len(transitions) != 1 || repo.pollUpdates != 0 {
				t.Fatal("terminal reconciliation must transition exactly once")
			}
		})
	}
}

func TestSMSBowerPollMissingUsesLockedSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name       string
		change     func(*domain.Order)
		wantStatus string
	}{
		{name: "message appeared after poll snapshot", change: func(o *domain.Order) {}, wantStatus: domain.OrderCompleted},
		{name: "terminal state wins", change: func(o *domain.Order) { o.Status = domain.OrderCanceled }, wantStatus: domain.OrderCanceled},
		{name: "renewal in progress", change: func(o *domain.Order) { o.RenewalInflight = true }, wantStatus: domain.OrderActive},
		{name: "activation identity changed", change: func(o *domain.Order) { o.UpstreamID = "new-upstream" }, wantStatus: domain.OrderActive},
		{name: "newer snapshot wins", change: func(o *domain.Order) { o.UpdatedAt = o.UpdatedAt.Add(time.Minute) }, wantStatus: domain.OrderActive},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fresh := smsBowerMissingOrder()
			fresh.UpdatedAt = fresh.CreatedAt
			snapshot := fresh
			snapshot.Messages = nil
			tt.change(&fresh)
			service, repo := newSMSBowerMissingService(t, fresh, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("NO_ACTIVATION"))
			}))
			service.pollOne(context.Background(), snapshot)
			after, transitions, _, locks := repo.snapshot()
			if after.Status != tt.wantStatus || locks != 1 || !reflect.DeepEqual(after.Messages, fresh.Messages) {
				t.Fatalf("locked snapshot status=%s, want %s; locks=%d", after.Status, tt.wantStatus, locks)
			}
			if tt.wantStatus != domain.OrderCompleted && (len(transitions) != 0 || repo.pollUpdates != 0) {
				t.Fatal("stale poll result modified the current order")
			}
		})
	}
}

func TestMissingPollDoesNotCompleteOtherProviders(t *testing.T) {
	order := smsBowerMissingOrder()
	order.ProviderID = domain.ProviderHeroSMS
	repo := &smsBowerMissingRepository{terminalLockRepository: &terminalLockRepository{order: order}}
	service := New(repo, nil, nil, config.Config{})
	_, err := service.applyPollResultLocked(context.Background(), order, domain.Provider{ID: order.ProviderID}, "", nil, provider.PollResult{State: provider.PollMissing})
	if err != nil {
		t.Fatal(err)
	}
	after, transitions, _, _ := repo.snapshot()
	if after.Status != domain.OrderActive || len(transitions) != 0 {
		t.Fatal("SMSBower missing proof must not complete another provider's order")
	}
}

func TestSMSBowerCompletionFailureLogOnlyContainsSafeFields(t *testing.T) {
	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	order := smsBowerMissingOrder()
	service, _ := newSMSBowerMissingService(t, order, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"BAD_KEY","api_key":"smsbower-provider-secret","phone":"+84999000111","sms":"Your code is 654321"}`))
	}))
	_, err := service.FinishOrder(context.Background(), order.ID, "complete", domain.User{ID: order.UserID, Role: "operator"}, "127.0.0.1")
	if !errors.Is(err, ErrProvider) {
		t.Fatalf("unexpected completion result: %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("invalid failure log: %v", err)
	}
	if entry["order_id"] != order.ID || entry["provider"] != domain.ProviderSMSBower || entry["operation"] != "complete" ||
		entry["code"] != "BAD_KEY" || entry["http_status"] != float64(http.StatusBadGateway) || entry["retryable"] != true {
		t.Fatalf("missing safe diagnostic fields: %v", entry)
	}
	for _, forbidden := range []string{"smsbower-provider-secret", order.PhoneNumber, order.Messages[0].Code, order.Messages[0].Text, "api_key", "missing-upstream"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("failure log contains sensitive field %q", forbidden)
		}
	}
	for field := range entry {
		switch field {
		case "time", "level", "msg", "order_id", "provider", "operation", "code", "http_status", "retryable":
		default:
			t.Fatalf("unexpected failure log field %q", field)
		}
	}
	output.Reset()
	logOrderCompleteFailure(order.ID, errors.New("raw secret error smsbower-provider-secret"))
	if strings.Contains(output.String(), "smsbower-provider-secret") || strings.Contains(output.String(), "raw secret error") {
		t.Fatal("untyped error leaked into failure log")
	}
}

func TestSMSBowerCompletionLogRedactsUntrustedErrorCode(t *testing.T) {
	for _, tt := range []struct {
		name      string
		payload   string
		forbidden string
	}{
		{name: "phone in error field", payload: `{"error":"+84999000111"}`, forbidden: "84999000111"},
		{name: "numeric OTP in error field", payload: `{"error":654321}`, forbidden: "654321"},
		{name: "unknown alphabetic secret in error field", payload: `{"error":"UNRELATED_PRIVATE_SECRET"}`, forbidden: "UNRELATED_PRIVATE_SECRET"},
		{name: "phone in code field", payload: `{"code":"+84999000111"}`, forbidden: "84999000111"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			order := smsBowerMissingOrder()
			service, _ := newSMSBowerMissingService(t, order, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(tt.payload))
			}))
			_, err := service.FinishOrder(context.Background(), order.ID, "complete", domain.User{ID: order.UserID, Role: "operator"}, "127.0.0.1")
			if !errors.Is(err, ErrProvider) {
				t.Fatalf("unexpected completion result: %v", err)
			}
			var entry map[string]any
			if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
				t.Fatalf("invalid failure log: %v", err)
			}
			if entry["code"] != "UPSTREAM_ERROR" || entry["http_status"] != float64(http.StatusBadGateway) {
				t.Fatal("untrusted error code must be replaced while retaining HTTP diagnostics")
			}
			if strings.Contains(output.String(), tt.forbidden) {
				t.Fatal("upstream error field leaked into completion log")
			}
		})
	}
}
