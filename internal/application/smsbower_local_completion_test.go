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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/domain"
	"buysms/internal/store"
)

type smsBowerLocalCompletionRepository struct {
	*smsBowerAutoFinishRepository
	auditEvents      []string
	auditMeta        []json.RawMessage
	auditActors      []string
	localCompleteErr error
}

func (r *smsBowerLocalCompletionRepository) CompleteOrderLocally(_ context.Context, id, actorID, _ string, meta json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.localCompleteErr != nil {
		return r.localCompleteErr
	}
	if r.order.ID != id || r.order.ProviderID != domain.ProviderSMSBower || r.order.Status != domain.OrderActive || r.order.RenewalInflight || r.order.RequestNextInflight {
		return store.ErrConflict
	}
	r.order.Status = domain.OrderCompleted
	r.order.LastProviderState = "user_local_complete"
	r.order.RequestNextPending = false
	r.order.RequestNextInflight = false
	r.order.RequestNextInflightAt = nil
	r.order.RequestNextFailures = 0
	r.order.PollFailures = 0
	r.transitions = append(r.transitions, domain.OrderCompleted)
	r.audits++
	r.auditEvents = append(r.auditEvents, "order.local_complete")
	r.auditMeta = append(r.auditMeta, append(json.RawMessage(nil), meta...))
	r.auditActors = append(r.auditActors, actorID)
	return nil
}

func (r *smsBowerLocalCompletionRepository) Audit(_ context.Context, actor *string, event, _, _, _ string, meta json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits++
	r.auditEvents = append(r.auditEvents, event)
	r.auditMeta = append(r.auditMeta, append(json.RawMessage(nil), meta...))
	actorID := ""
	if actor != nil {
		actorID = *actor
	}
	r.auditActors = append(r.auditActors, actorID)
	return nil
}

func newSMSBowerLocalCompletionService(t *testing.T, handler http.HandlerFunc) (*Service, *smsBowerLocalCompletionRepository) {
	t.Helper()
	service, base, _ := newSMSBowerAutoFinishTestService(t, handler)
	order := smsBowerMissingOrder()
	base.order = order
	service.now = func() time.Time { return order.CreatedAt.Add(time.Hour) }
	repo := &smsBowerLocalCompletionRepository{smsBowerAutoFinishRepository: base}
	service.repo = repo
	return service, repo
}

func TestSMSBowerLocalCompletionRequiresExplicitEligibleOwner(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*domain.Order)
		actor  domain.User
		input  LocalCompleteInput
	}{
		{name: "confirmation omitted", input: LocalCompleteInput{}},
		{name: "another operator cannot close", actor: domain.User{ID: "another-operator", Role: "operator"}, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "other provider", change: func(o *domain.Order) { o.ProviderID = domain.ProviderHeroSMS }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "already completed", change: func(o *domain.Order) { o.Status = domain.OrderCompleted }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "already canceled", change: func(o *domain.Order) { o.Status = domain.OrderCanceled }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "already expired", change: func(o *domain.Order) { o.Status = domain.OrderExpired }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "no messages", change: func(o *domain.Order) { o.Messages = nil }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "historical messages only", change: func(o *domain.Order) { o.Messages[0].ReceivedAt = o.ActivationStartedAt.Add(-time.Minute) }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "before deadline", change: func(o *domain.Order) { o.CreatedAt = o.CreatedAt.Add(36 * time.Minute) }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "missing creation time", change: func(o *domain.Order) { o.CreatedAt = time.Time{} }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "renewal in flight", change: func(o *domain.Order) { o.RenewalInflight = true }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
		{name: "next message in flight", change: func(o *domain.Order) { o.RequestNextInflight = true }, input: LocalCompleteInput{UpstreamMissingConfirmed: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			service, repo := newSMSBowerLocalCompletionService(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				http.Error(w, "UNEXPECTED_REQUEST", http.StatusInternalServerError)
			})
			if tt.change != nil {
				tt.change(&repo.order)
			}
			before := repo.order
			actor := tt.actor
			if actor.ID == "" {
				actor = domain.User{ID: before.UserID, Role: "operator"}
			}
			_, err := service.LocalCompleteOrder(context.Background(), before.ID, tt.input, actor, "127.0.0.1")
			if err == nil {
				t.Fatal("ineligible or unconfirmed local completion succeeded")
			}
			after, transitions, audits, _ := repo.snapshot()
			if !reflect.DeepEqual(before, after) || len(transitions) != 0 || audits != 0 || requests.Load() != 0 {
				t.Fatalf("ineligible request caused side effects: requests=%d transitions=%v audits=%d", requests.Load(), transitions, audits)
			}
		})
	}
}

func TestSMSBowerLocalCompletionRechecksProviderAndRetainsEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, finishBody, confirmBody string
		finishHTTP, confirmHTTP       int
		wantStatus, wantState         string
		wantError, wantNewSMS         bool
	}{
		{name: "confirmed manual recovery", finishBody: "BAD_STATUS", confirmBody: "STATUS_OK:987654", wantStatus: domain.OrderCompleted, wantState: "user_local_complete", wantNewSMS: true},
		{name: "HTTP409 business rejection", finishHTTP: http.StatusConflict, finishBody: "BAD_STATUS", confirmBody: "STATUS_OK:987654", wantStatus: domain.OrderCompleted, wantState: "user_local_complete", wantNewSMS: true},
		{name: "ordinary completion now succeeds", finishBody: "ACCESS_ACTIVATION", wantStatus: domain.OrderCompleted, wantState: "user_complete"},
		{name: "upstream explicitly canceled", finishBody: "BAD_STATUS", confirmBody: "STATUS_CANCEL", wantStatus: domain.OrderCanceled, wantState: "upstream_canceled"},
		{name: "upstream explicitly completed", finishBody: "BAD_STATUS", confirmBody: `{"status":"completed"}`, wantStatus: domain.OrderCompleted, wantState: "upstream_completed"},
		{name: "upstream expired", finishBody: "BAD_STATUS", confirmBody: `{"status":"expired"}`, wantStatus: domain.OrderExpired, wantState: "upstream_expired"},
		{name: "upstream refunded", finishBody: "BAD_STATUS", confirmBody: `{"status":"refunded"}`, wantStatus: domain.OrderCanceled, wantState: "upstream_refunded"},
		{name: "upstream missing uses normal proof", finishBody: "BAD_STATUS", confirmBody: "NO_ACTIVATION", wantStatus: domain.OrderCompleted, wantState: "upstream_missing"},
		{name: "still waiting", finishBody: "BAD_STATUS", confirmBody: "STATUS_WAIT_CODE", wantStatus: domain.OrderActive, wantError: true},
		{name: "waiting retry is not manual recovery proof", finishBody: "BAD_STATUS", confirmBody: "STATUS_WAIT_RETRY:654321", wantStatus: domain.OrderActive, wantError: true},
		{name: "confirmation authentication failure", finishBody: "BAD_STATUS", confirmBody: "BAD_KEY", wantStatus: domain.OrderActive, wantError: true},
		{name: "confirmation rate limited", finishBody: "BAD_STATUS", confirmHTTP: http.StatusTooManyRequests, confirmBody: "STATUS_OK:987654", wantStatus: domain.OrderActive, wantError: true},
		{name: "confirmation HTTP401", finishBody: "BAD_STATUS", confirmHTTP: http.StatusUnauthorized, confirmBody: "STATUS_OK:987654", wantStatus: domain.OrderActive, wantError: true},
		{name: "completion server failure", finishHTTP: http.StatusServiceUnavailable, finishBody: "BAD_STATUS", wantStatus: domain.OrderActive, wantError: true},
		{name: "confirmation malformed", finishBody: "BAD_STATUS", confirmBody: "<html>unavailable</html>", wantStatus: domain.OrderActive, wantError: true},
		{name: "missing rejection with received is not BAD_STATUS", finishBody: "NO_ACTIVATION", confirmBody: "STATUS_OK:987654", wantStatus: domain.OrderActive, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var completes, confirms atomic.Int32
			var logs bytes.Buffer
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(oldLogger) })
			service, repo := newSMSBowerLocalCompletionService(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Query().Get("action") {
				case "setStatus":
					completes.Add(1)
					if r.URL.Query().Get("status") != "6" {
						t.Error("local completion must not cancel, refund, or request another SMS")
					}
					if tt.finishHTTP != 0 {
						w.WriteHeader(tt.finishHTTP)
					}
					_, _ = w.Write([]byte(tt.finishBody))
				case "getStatus":
					confirms.Add(1)
					if tt.confirmHTTP != 0 {
						w.WriteHeader(tt.confirmHTTP)
					}
					_, _ = w.Write([]byte(tt.confirmBody))
				default:
					t.Errorf("unexpected action %q", r.URL.Query().Get("action"))
					http.NotFound(w, r)
				}
			})
			before := repo.order
			view, err := service.LocalCompleteOrder(context.Background(), before.ID, LocalCompleteInput{UpstreamMissingConfirmed: true, Reason: "已在供应商页面核对号码不存在"}, domain.User{ID: "admin-1", Role: "admin"}, "127.0.0.1")
			if (err != nil) != tt.wantError {
				t.Fatalf("local completion err=%v, want error=%t", err, tt.wantError)
			}
			after, transitions, audits, locks := repo.snapshot()
			if after.Status != tt.wantStatus || after.Cost != before.Cost || after.Currency != before.Currency || locks != 1 {
				t.Fatalf("status=%s want=%s; money=%v/%s locks=%d", after.Status, tt.wantStatus, after.Cost, after.Currency, locks)
			}
			if len(after.Messages) < len(before.Messages) || !reflect.DeepEqual(after.Messages[:len(before.Messages)], before.Messages) {
				t.Fatal("local completion changed saved SMS history")
			}
			if tt.wantNewSMS {
				found := false
				for _, message := range after.Messages {
					if message.Code == "987654" {
						found = true
					}
				}
				if !found {
					t.Fatal("last confirmation SMS was lost when closing the local order")
				}
			}
			wantConfirms := int32(1)
			if tt.confirmBody == "" {
				wantConfirms = 0
			}
			if completes.Load() != 1 || confirms.Load() != wantConfirms {
				t.Fatalf("requests complete=%d confirm=%d, want 1/%d", completes.Load(), confirms.Load(), wantConfirms)
			}
			if tt.wantError {
				if after.Terminal() || len(transitions) != 0 || audits != 0 {
					t.Fatal("provider uncertainty became local completion")
				}
			} else {
				wantView := tt.wantStatus
				if wantView == domain.OrderCanceled {
					wantView = "cancelled"
				}
				if after.LastProviderState != tt.wantState || view.Status != wantView || len(transitions) != 1 || audits != 1 {
					t.Fatalf("incorrect terminal state/view/audit: state=%s view=%s transitions=%v audits=%d", after.LastProviderState, view.Status, transitions, audits)
				}
				if tt.wantState == "user_local_complete" {
					if len(repo.auditEvents) != 1 || repo.auditEvents[0] != "order.local_complete" || repo.auditActors[0] != "admin-1" {
						t.Fatal("manual recovery must be distinct and attributable in audit")
					}
					var meta map[string]any
					if json.Unmarshal(repo.auditMeta[0], &meta) != nil || meta["upstreamMissingConfirmed"] != true {
						t.Fatal("manual confirmation must be recorded in audit")
					}
				}
			}
			for _, secret := range []string{before.PhoneNumber, before.Messages[0].Code, "987654", "provider-secret"} {
				auditText := ""
				for _, meta := range repo.auditMeta {
					auditText += string(meta)
				}
				if strings.Contains(logs.String(), secret) || strings.Contains(auditText, secret) {
					t.Fatal("diagnostics or audit exposed provider/SMS secrets")
				}
			}
		})
	}
}

func TestSMSBowerLocalCompletionCannotLoseUnsavedConfirmationSMS(t *testing.T) {
	service, repo := newSMSBowerLocalCompletionService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "setStatus" {
			_, _ = w.Write([]byte("BAD_STATUS"))
			return
		}
		_, _ = w.Write([]byte("STATUS_OK:987654"))
	})
	repo.messageSaveErr = errors.New("temporary message save failure")
	before := repo.order
	_, err := service.LocalCompleteOrder(context.Background(), before.ID, LocalCompleteInput{UpstreamMissingConfirmed: true}, domain.User{ID: before.UserID, Role: "operator"}, "127.0.0.1")
	after, transitions, audits, _ := repo.snapshot()
	if err == nil || after.Status != domain.OrderActive || len(transitions) != 0 || audits != 0 || !reflect.DeepEqual(after.Messages, before.Messages) {
		t.Fatal("failed confirmation SMS persistence must leave the order active")
	}
}

func TestSMSBowerLocalCompletionSerializesDuplicateRequests(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var completes atomic.Int32
	service, repo := newSMSBowerLocalCompletionService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "setStatus" {
			if completes.Add(1) == 1 {
				close(entered)
			}
			<-release
			_, _ = w.Write([]byte("BAD_STATUS"))
			return
		}
		_, _ = w.Write([]byte("STATUS_OK:654321"))
	})
	actor := domain.User{ID: repo.order.UserID, Role: "operator"}
	in := LocalCompleteInput{UpstreamMissingConfirmed: true}
	first := make(chan error, 1)
	go func() {
		_, err := service.LocalCompleteOrder(context.Background(), repo.order.ID, in, actor, "127.0.0.1")
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("local completion did not acquire the provider request")
	}
	if _, err := service.LocalCompleteOrder(context.Background(), repo.order.ID, in, actor, "127.0.0.1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent completion should conflict before provider mutation: %v", err)
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := service.LocalCompleteOrder(context.Background(), repo.order.ID, in, actor, "127.0.0.1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("already closed order should reject repeated local completion: %v", err)
	}
	_, transitions, audits, _ := repo.snapshot()
	if completes.Load() != 1 || len(transitions) != 1 || audits != 1 {
		t.Fatalf("duplicate request repeated external action or transition: calls=%d transitions=%v audits=%d", completes.Load(), transitions, audits)
	}
}

func TestSMSBowerLocalCompletionPendingResendDoesNotBlockRecovery(t *testing.T) {
	service, repo := newSMSBowerLocalCompletionService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "setStatus" {
			if r.URL.Query().Get("status") != "6" {
				t.Error("unexpected resend/cancel")
			}
			_, _ = w.Write([]byte("BAD_STATUS"))
			return
		}
		_, _ = w.Write([]byte("STATUS_OK:654321"))
	})
	repo.order.RequestNextPending = true
	repo.order.RequestNextFailures = 3
	view, err := service.LocalCompleteOrder(context.Background(), repo.order.ID, LocalCompleteInput{UpstreamMissingConfirmed: true}, domain.User{ID: repo.order.UserID, Role: "operator"}, "127.0.0.1")
	after, _, _, _ := repo.snapshot()
	if err != nil || !view.LocalCompleted || after.RequestNextPending || after.RequestNextFailures != 0 {
		t.Fatalf("pending-only resend blocked recovery or was not cleared: err=%v", err)
	}
}

func TestSMSBowerLocalCompletionAuditFailureDoesNotCloseOrder(t *testing.T) {
	service, repo := newSMSBowerLocalCompletionService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "setStatus" {
			_, _ = w.Write([]byte("BAD_STATUS"))
			return
		}
		_, _ = w.Write([]byte("STATUS_OK:987654"))
	})
	repo.localCompleteErr = errors.New("audit write failed")
	_, err := service.LocalCompleteOrder(context.Background(), repo.order.ID, LocalCompleteInput{UpstreamMissingConfirmed: true}, domain.User{ID: repo.order.UserID, Role: "operator"}, "127.0.0.1")
	after, transitions, audits, _ := repo.snapshot()
	if err == nil || after.Terminal() || len(transitions) != 0 || audits != 0 {
		t.Fatal("audit failure must leave the order active")
	}
	found := false
	for _, message := range after.Messages {
		if message.Code == "987654" {
			found = true
		}
	}
	if !found {
		t.Fatal("audit failure must not lose saved confirmation SMS")
	}
}
