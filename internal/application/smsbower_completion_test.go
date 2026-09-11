package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/domain"
	"buysms/internal/provider"
)

type smsBowerCompletionAuditRepository struct {
	*smsBowerMissingRepository
	auditEvents []string
	auditMeta   []json.RawMessage
}

func (r *smsBowerCompletionAuditRepository) Audit(_ context.Context, _ *string, event, _, _, _ string, meta json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits++
	r.auditEvents = append(r.auditEvents, event)
	r.auditMeta = append(r.auditMeta, append(json.RawMessage(nil), meta...))
	return nil
}

func TestSMSBowerBadStatusCompletionReconciliation(t *testing.T) {
	for _, source := range []string{"user", "auto"} {
		for _, tt := range []struct {
			name           string
			confirmBody    string
			historicalOnly bool
			timeout        bool
			wantStatus     string
			wantState      string
			wantCode       string
			wantLogCode    string
			wantConfirm    string
		}{
			{name: "missing with message", confirmBody: "NO_ACTIVATION", wantStatus: domain.OrderCompleted, wantState: "upstream_missing"},
			{name: "missing without current message", confirmBody: "NO_ACTIVATION", historicalOnly: true, wantStatus: domain.OrderActive, wantCode: OrderActionCodeCompleteStatusConflict, wantLogCode: provider.CodeActivationMissing, wantConfirm: provider.PollMissing},
			{name: "canceled upstream", confirmBody: "STATUS_CANCEL", wantStatus: domain.OrderCanceled, wantState: "upstream_canceled"},
			{name: "expired upstream", confirmBody: `{"status":"expired"}`, wantStatus: domain.OrderExpired, wantState: "upstream_expired"},
			{name: "completed upstream", confirmBody: `{"status":"completed"}`, wantStatus: domain.OrderCompleted, wantState: "upstream_completed"},
			{name: "refunded upstream", confirmBody: `{"status":"refunded"}`, wantStatus: domain.OrderCanceled, wantState: "upstream_refunded"},
			{name: "still waiting", confirmBody: "STATUS_WAIT_CODE", wantStatus: domain.OrderActive, wantCode: OrderActionCodeCompleteStatusConflict, wantLogCode: "BAD_STATUS", wantConfirm: provider.PollWaiting},
			{name: "still receiving", confirmBody: "STATUS_OK:987654", wantStatus: domain.OrderActive, wantCode: OrderActionCodeCompleteStatusConflict, wantLogCode: "BAD_STATUS", wantConfirm: provider.PollReceived},
			{name: "waiting for another message", confirmBody: "STATUS_WAIT_RETRY:987654", wantStatus: domain.OrderActive, wantCode: OrderActionCodeCompleteStatusConflict, wantLogCode: "BAD_STATUS", wantConfirm: provider.PollWaitingRetry},
			{name: "confirmation rejected", confirmBody: "BAD_KEY", wantStatus: domain.OrderActive, wantCode: OrderActionCodeProviderError, wantLogCode: "BAD_KEY"},
			{name: "confirmation timed out", timeout: true, wantStatus: domain.OrderActive, wantCode: OrderActionCodeProviderError, wantLogCode: "TIMEOUT"},
		} {
			t.Run(source+"/"+tt.name, func(t *testing.T) {
				var logs bytes.Buffer
				oldLogger := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(oldLogger) })
				order := smsBowerMissingOrder()
				if tt.historicalOnly {
					order.Messages[0].ReceivedAt = order.ActivationStartedAt.Add(-time.Minute)
				}
				var completes, confirms atomic.Int32
				service, baseRepo := newSMSBowerMissingService(t, order, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Query().Get("action") {
					case "getAllSms":
						_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
					case "setStatus":
						completes.Add(1)
						if r.URL.Query().Get("status") != "6" {
							t.Error("completion reconciliation must never submit cancellation or another SMS request")
						}
						_, _ = w.Write([]byte("BAD_STATUS"))
					case "getStatus":
						confirms.Add(1)
						if tt.timeout {
							<-r.Context().Done()
							return
						}
						_, _ = w.Write([]byte(tt.confirmBody))
					default:
						t.Errorf("unexpected action %q", r.URL.Query().Get("action"))
						http.NotFound(w, r)
					}
				}))
				repo := &smsBowerCompletionAuditRepository{smsBowerMissingRepository: baseRepo}
				service.repo = repo
				ctx := context.Background()
				if tt.timeout {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
					defer cancel()
				}
				if source == "user" {
					view, err := service.FinishOrder(ctx, order.ID, "complete", domain.User{ID: order.UserID, Role: "operator"}, "127.0.0.1")
					if tt.wantCode == "" {
						wantViewStatus := tt.wantStatus
						if wantViewStatus == domain.OrderCanceled {
							wantViewStatus = "cancelled"
						}
						if err != nil || view.Status != wantViewStatus {
							t.Fatalf("completed response status=%s error=%v", view.Status, err)
						}
					} else {
						var actionErr *OrderActionError
						if !errors.As(err, &actionErr) || actionErr.Code != tt.wantCode {
							t.Fatalf("completion error=%v, want code %s", err, tt.wantCode)
						}
						if tt.wantCode == OrderActionCodeCompleteStatusConflict && !errors.Is(err, ErrConflict) {
							t.Fatal("status refusal must retain conflict kind")
						}
						if !strings.Contains(actionErr.Message, tt.wantLogCode) || strings.Contains(actionErr.Message, "供应商暂时不可用") {
							t.Fatalf("completion diagnosis must name the safe failure: %s", actionErr.Message)
						}
						if tt.wantConfirm == "" && !strings.Contains(actionErr.Message, "状态确认失败") {
							t.Fatal("confirmation failure must identify the confirmation stage")
						}
						if tt.wantLogCode == "BAD_STATUS" && !strings.Contains(actionErr.Message, tt.wantConfirm) {
							t.Fatal("status conflict must include the normalized confirmation state")
						}
						if strings.Contains(actionErr.Message, "987654") || strings.Contains(actionErr.Message, order.PhoneNumber) {
							t.Fatal("provider confirmation leaked private values into the API message")
						}
					}
				} else {
					service.pollOne(ctx, order)
				}
				after, transitions, audits, locks := repo.snapshot()
				wantState := tt.wantState
				if source == "auto" && tt.wantStatus == domain.OrderActive {
					wantState = provider.PollWaiting
				}
				if after.Status != tt.wantStatus || after.LastProviderState != wantState || locks != 1 {
					t.Fatalf("status/state=%s/%s, want %s/%s; locks=%d", after.Status, after.LastProviderState, tt.wantStatus, wantState, locks)
				}
				if !reflect.DeepEqual(after.Messages, order.Messages) || after.Cost != order.Cost || after.Currency != order.Currency {
					t.Fatal("reconciliation changed saved messages or money")
				}
				if completes.Load() != 1 || confirms.Load() != 1 {
					t.Fatalf("confirmation calls: complete=%d confirm=%d", completes.Load(), confirms.Load())
				}
				if after.Terminal() {
					if len(transitions) != 1 || audits != 1 || repo.pollUpdates != 0 || logs.Len() != 0 {
						t.Fatal("confirmed terminal state must transition/audit once and stop failure retries")
					}
					wantAudit := "order.reconcile"
					if tt.wantState == "upstream_missing" {
						wantAudit = "order.complete"
						if source == "auto" {
							wantAudit = "order.auto_complete"
						}
					}
					if len(repo.auditEvents) != 1 || repo.auditEvents[0] != wantAudit {
						t.Fatalf("incorrect audit event: %v", repo.auditEvents)
					}
					if wantAudit == "order.reconcile" {
						var meta map[string]string
						if err := json.Unmarshal(repo.auditMeta[0], &meta); err != nil || meta["requestedAction"] != "complete" ||
							meta["source"] != source || meta["status"] != tt.wantStatus || meta["providerState"] != tt.wantState || len(meta) != 4 {
							t.Fatalf("incorrect reconciliation audit metadata: %s", repo.auditMeta[0])
						}
					}
					if source == "auto" {
						service.pollOne(context.Background(), order)
					} else {
						_, err := service.FinishOrder(context.Background(), order.ID, "complete", domain.User{ID: order.UserID, Role: "operator"}, "127.0.0.1")
						if !errors.Is(err, ErrConflict) {
							t.Fatal("terminal order must reject another manual completion")
						}
					}
					_, transitions, audits, _ = repo.snapshot()
					if completes.Load() != 1 || confirms.Load() != 1 || len(transitions) != 1 || audits != 1 {
						t.Fatal("terminal order retried completion or confirmation")
					}
				} else {
					if len(transitions) != 0 || audits != 0 {
						t.Fatal("nonterminal confirmation must not write a terminal state or audit")
					}
					if source == "auto" && (after.PollFailures != 1 || repo.pollUpdates != 1 || !after.NextPollAt.Equal(service.now().Add(5*time.Second))) {
						t.Fatal("automatic failure must keep backoff without resending the completion immediately")
					}
					var entry map[string]any
					if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
						t.Fatalf("invalid failure log: %v", err)
					}
					wantOperation := "complete"
					if tt.wantConfirm == "" {
						wantOperation = "complete.confirm"
					}
					if entry["provider"] != domain.ProviderSMSBower || entry["operation"] != wantOperation || entry["code"] != tt.wantLogCode ||
						entry["http_status"] != float64(0) || entry["retryable"] != tt.timeout || entry["order_id"] != order.ID {
						t.Fatalf("missing safe diagnostics: %v", entry)
					}
					if tt.wantConfirm != "" && entry["confirmation_state"] != tt.wantConfirm {
						t.Fatalf("missing normalized confirmation state: %v", entry)
					}
					if source == "auto" && (entry["action"] != "complete" || entry["msg"] != "SMSbower 倒计时结束处理失败，将重试") {
						t.Fatal("automatic log must retain its event name and action")
					}
					for _, secret := range []string{order.PhoneNumber, order.Messages[0].Code, "987654", "smsbower-provider-secret"} {
						if strings.Contains(logs.String(), secret) {
							t.Fatal("failure diagnostics leaked provider or message secrets")
						}
					}
				}
			})
		}
	}
}

func TestSMSBowerConfirmedTerminalRequiresStrictProof(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*domain.Order, *provider.ProviderError)
	}{
		{name: "other provider", change: func(o *domain.Order, _ *provider.ProviderError) { o.ProviderID = domain.ProviderHeroSMS }},
		{name: "other error provider", change: func(_ *domain.Order, e *provider.ProviderError) { e.Provider = domain.ProviderSMSPool }},
		{name: "terminal local order", change: func(o *domain.Order, _ *provider.ProviderError) { o.Status = domain.OrderExpired }},
		{name: "renewal inflight", change: func(o *domain.Order, _ *provider.ProviderError) { o.RenewalInflight = true }},
		{name: "wrong operation", change: func(_ *domain.Order, e *provider.ProviderError) { e.Operation = "cancel" }},
		{name: "confirmation failed", change: func(_ *domain.Order, e *provider.ProviderError) { e.Operation = "complete.confirm" }},
		{name: "unconfirmed original rejection", change: func(_ *domain.Order, e *provider.ProviderError) { e.Code = "BAD_STATUS" }},
		{name: "retryable", change: func(_ *domain.Order, e *provider.ProviderError) { e.Retryable = true }},
		{name: "HTTP error", change: func(_ *domain.Order, e *provider.ProviderError) { e.HTTPStatus = http.StatusBadRequest }},
		{name: "no state", change: func(_ *domain.Order, e *provider.ProviderError) { e.ConfirmationState = "" }},
		{name: "nonterminal state", change: func(_ *domain.Order, e *provider.ProviderError) { e.ConfirmationState = provider.PollReceived }},
		{name: "missing is not terminal proof", change: func(_ *domain.Order, e *provider.ProviderError) { e.ConfirmationState = provider.PollMissing }},
		{name: "unknown untrusted state", change: func(_ *domain.Order, e *provider.ProviderError) { e.ConfirmationState = "STATUS_CANCEL:654321" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			order := smsBowerMissingOrder()
			proof := provider.ProviderError{Provider: domain.ProviderSMSBower, Operation: "complete", Code: provider.CodeActivationTerminal, ConfirmationState: provider.PollCanceled}
			tt.change(&order, &proof)
			if _, _, confirmed := confirmedSMSBowerCompletion(order, &proof); confirmed {
				t.Fatal("untrusted or mismatched terminal proof must not change the local order")
			}
		})
	}
	order := smsBowerMissingOrder()
	order.Messages = nil
	proof := &provider.ProviderError{Provider: domain.ProviderSMSBower, Operation: "complete", Code: provider.CodeActivationTerminal, ConfirmationState: provider.PollCanceled}
	if status, state, confirmed := confirmedSMSBowerCompletion(order, proof); !confirmed || status != domain.OrderCanceled || state != "upstream_canceled" {
		t.Fatal("explicit terminal proof does not require SMS evidence")
	}
}

func TestSMSBowerCompletionConflictOnlyForBusinessRejection(t *testing.T) {
	for _, status := range []int{0, http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		err := orderActionProviderError("complete", &provider.ProviderError{
			Provider: domain.ProviderSMSBower, Operation: "complete", Code: "BAD_STATUS", ConfirmationState: provider.PollWaiting,
			HTTPStatus: status, Retryable: status == http.StatusTooManyRequests || status >= http.StatusInternalServerError,
		})
		wantConflict := status == 0 || status == http.StatusBadRequest || status == http.StatusConflict || status == http.StatusUnprocessableEntity
		if (err.Code == OrderActionCodeCompleteStatusConflict) != wantConflict || errors.Is(err, ErrConflict) != wantConflict {
			t.Fatalf("HTTP %d mapped to wrong completion error %s", status, err.Code)
		}
	}
	for _, tt := range []struct{ providerID, action string }{
		{providerID: domain.ProviderSMSBower, action: "cancel"},
		{providerID: domain.ProviderHeroSMS, action: "complete"},
		{providerID: domain.ProviderSMSPool, action: "complete"},
	} {
		err := orderActionProviderError(tt.action, &provider.ProviderError{Provider: tt.providerID, Operation: tt.action, Code: "BAD_STATUS", ConfirmationState: provider.PollCanceled})
		if err.Code != OrderActionCodeProviderError || err.Message != "供应商暂时不可用，请稍后重试" {
			t.Fatalf("changed cancellation or another provider's contract: %s/%s: %v", tt.providerID, tt.action, err)
		}
	}
}

func TestSMSBowerCompletionDiagnosticsWhitelistConfirmationState(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	upstream := &provider.ProviderError{
		Provider: domain.ProviderSMSBower, Operation: "complete.confirm", Code: "SECRET_84999000111_654321",
		HTTPStatus: http.StatusBadGateway, ConfirmationState: "STATUS_OK:654321",
	}
	logOrderCompleteFailure("diagnostic-order", upstream)
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["code"] != "UPSTREAM_ERROR" || entry["confirmation_state"] != nil {
		t.Fatal("untrusted code and state must be redacted")
	}
	err := orderActionProviderError("complete", upstream)
	for _, raw := range []string{"SECRET_", "84999000111", "654321", "STATUS_OK"} {
		if strings.Contains(logs.String(), raw) || strings.Contains(err.Message, raw) {
			t.Fatal("untrusted diagnostic content leaked into logs or API message")
		}
	}
}
