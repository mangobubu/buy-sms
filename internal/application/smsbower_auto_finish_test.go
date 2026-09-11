package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type smsBowerAutoFinishRepository struct {
	*terminalLockRepository
	messageSaveErr error
	messageSaves   int
	advanceSaves   int
	resendClaims   int
	dueClaims      int
}

func (r *smsBowerAutoFinishRepository) SaveMessage(_ context.Context, message domain.SMSMessage, advance bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.messageSaveErr != nil {
		return false, r.messageSaveErr
	}
	for _, saved := range r.order.Messages {
		if saved.UpstreamFingerprint == message.UpstreamFingerprint {
			return false, nil
		}
	}
	r.order.Messages = append(r.order.Messages, message)
	r.messageSaves++
	if advance {
		r.advanceSaves++
		r.order.PollSequence++
		if r.order.CanGetAnotherSMS {
			r.order.RequestNextPending = true
		}
	}
	return true, nil
}

func (r *smsBowerAutoFinishRepository) UpdatePoll(_ context.Context, id, state string, next time.Time, failures int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID == id && r.order.Status == domain.OrderActive {
		r.order.LastProviderState = state
		r.order.NextPollAt = next
		r.order.PollFailures = failures
	}
	return nil
}

func (r *smsBowerAutoFinishRepository) UpdateOrderExpiresAt(_ context.Context, id string, expiry time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID == id && r.order.Status == domain.OrderActive {
		r.order.ExpiresAt = &expiry
	}
	return nil
}

func (r *smsBowerAutoFinishRepository) ClaimRequestNext(context.Context, string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resendClaims++
	if r.order.Status != domain.OrderActive || !r.order.RequestNextPending || r.order.RequestNextInflight {
		return false, nil
	}
	r.order.RequestNextPending = false
	r.order.RequestNextInflight = true
	return true, nil
}

func (r *smsBowerAutoFinishRepository) CompleteRequestNext(_ context.Context, id string, charge float64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.ID != id || r.order.Status != domain.OrderActive || !r.order.RequestNextInflight {
		return false, nil
	}
	r.order.RequestNextInflight = false
	r.order.Cost += charge
	r.order.UpdatedAt = r.order.UpdatedAt.Add(time.Nanosecond)
	return true, nil
}

func (r *smsBowerAutoFinishRepository) ClaimDueOrders(_ context.Context, _ int, now time.Time, lease time.Duration) ([]domain.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order.Status != domain.OrderActive || r.order.RenewalInflight || r.order.NextPollAt.After(now) {
		return nil, nil
	}
	r.dueClaims++
	r.order.NextPollAt = now.Add(lease)
	return []domain.Order{r.order}, nil
}

func newSMSBowerAutoFinishTestService(t *testing.T, handler http.HandlerFunc) (*Service, *smsBowerAutoFinishRepository, time.Time) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	vault, err := secure.NewVault([]byte("smsbower-auto-finish-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := vault.Encrypt("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	repo := &smsBowerAutoFinishRepository{terminalLockRepository: &terminalLockRepository{
		provider: domain.Provider{
			ID: domain.ProviderSMSBower, BaseURL: server.URL + "/stubs/handler_api.php",
			Enabled: true, APIKeyCipher: key, Config: json.RawMessage(`{"pollingIntervalSeconds":30}`),
		},
		order: domain.Order{
			ID: "auto-finish", UserID: "operator-1", ProviderID: domain.ProviderSMSBower,
			UpstreamID: "upstream-auto-finish", Status: domain.OrderActive,
			CreatedAt: now.Add(-25 * time.Minute), NextPollAt: now,
		},
	}}
	service := New(repo, nil, vault, config.Config{})
	service.now = func() time.Time { return now }
	return service, repo, now
}

func TestSMSBowerAutoFinishAtDeadline(t *testing.T) {
	tests := []struct {
		name         string
		response     string
		messages     []domain.SMSMessage
		wantStatus   string
		wantAction   string
		wantMessages int
	}{
		{name: "未收短信自动取消", response: "STATUS_WAIT_CODE", wantStatus: domain.OrderCanceled, wantAction: "8"},
		{name: "已保存回调短信自动完成", response: "STATUS_WAIT_CODE", messages: []domain.SMSMessage{{ID: "webhook-1", Code: "123456", Source: "webhook"}}, wantStatus: domain.OrderCompleted, wantAction: "6", wantMessages: 1},
		{name: "本轮刚收到短信先保存再完成", response: "STATUS_OK:24680", wantStatus: domain.OrderCompleted, wantAction: "6", wantMessages: 1},
		{name: "平台已取消优先", response: "STATUS_CANCEL", messages: []domain.SMSMessage{{ID: "sms-1", Code: "123456"}}, wantStatus: domain.OrderCanceled, wantMessages: 1},
		{name: "平台已完成优先", response: `{"status":"completed","otpList":[]}`, wantStatus: domain.OrderCompleted},
		{name: "平台退款优先并保存本轮短信", response: `{"status":"refunded","otpList":[{"id":"sms-1","code":"13579"}]}`, wantStatus: domain.OrderCanceled, wantMessages: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var actions []string
			var repo *smsBowerAutoFinishRepository
			service, createdRepo, _ := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Query().Get("action") {
				case "getAllSms":
					_, _ = w.Write([]byte(tt.response))
				case "setStatus":
					action := request.URL.Query().Get("status")
					actions = append(actions, action)
					order, _, _, _ := repo.snapshot()
					repo.mu.Lock()
					locked := repo.locks[order.ID]
					repo.mu.Unlock()
					if !locked {
						t.Error("自动远端动作必须持有订单锁")
					}
					if action == "6" && len(order.Messages) == 0 {
						t.Error("自动完成之前必须已经持久化短信")
					}
					if action == "6" {
						_, _ = w.Write([]byte("ACCESS_ACTIVATION"))
					} else {
						_, _ = w.Write([]byte("ACCESS_CANCEL"))
					}
				default:
					http.Error(w, "unexpected request", http.StatusBadRequest)
				}
			})
			repo = createdRepo
			repo.order.Messages = tt.messages
			repo.order.CanGetAnotherSMS = true
			repo.order.RequestNextPending = true
			service.pollOne(context.Background(), repo.order)
			order, transitions, audits, locks := repo.snapshot()
			if order.Status != tt.wantStatus || len(order.Messages) != tt.wantMessages || len(transitions) != 1 {
				t.Fatalf("自动处理结果错误: order=%+v transitions=%v", order, transitions)
			}
			if tt.wantAction == "" {
				if len(actions) != 0 || audits != 0 {
					t.Fatalf("平台已结束不得追加远端动作: actions=%v audits=%d", actions, audits)
				}
			} else if len(actions) != 1 || actions[0] != tt.wantAction || audits != 1 {
				t.Fatalf("远端动作错误: actions=%v audits=%d", actions, audits)
			}
			if order.ExpiresAt != nil || repo.resendClaims != 0 || locks != 1 {
				t.Fatalf("业务期限不得伪造供应商期限、嵌套锁或续码: expiry=%v claims=%d locks=%d", order.ExpiresAt, repo.resendClaims, locks)
			}
		})
	}
}

func TestSMSBowerAutoFinishSchedulesDeadlineAndSurvivesRestart(t *testing.T) {
	var finishCalls atomic.Int32
	service, repo, now := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "setStatus" {
			finishCalls.Add(1)
			_, _ = w.Write([]byte("ACCESS_CANCEL"))
			return
		}
		_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
	})
	repo.order.CreatedAt = now.Add(-25*time.Minute + 10*time.Second)
	deadline := repo.order.CreatedAt.Add(25 * time.Minute)
	service.pollBatch(context.Background(), func(job func()) { job() })
	order, _, _, _ := repo.snapshot()
	if order.Status != domain.OrderActive || !order.NextPollAt.Equal(deadline) || finishCalls.Load() != 0 {
		t.Fatalf("未到25分钟不得结束，调度必须截到deadline: order=%+v calls=%d", order, finishCalls.Load())
	}

	// 新服务实例仅凭数据库的创建时间和 next_poll_at，恢复关闭页面前的订单。
	restarted := New(repo, nil, service.vault, config.Config{})
	restarted.now = func() time.Time { return deadline.Add(time.Hour) }
	restarted.pollBatch(context.Background(), func(job func()) { job() })
	restarted.pollBatch(context.Background(), func(job func()) { job() })
	order, transitions, _, _ := repo.snapshot()
	if order.Status != domain.OrderCanceled || finishCalls.Load() != 1 || len(transitions) != 1 || repo.dueClaims != 2 {
		t.Fatalf("重启后应处理存量到期订单且终态不重试: order=%+v calls=%d transitions=%v claims=%d", order, finishCalls.Load(), transitions, repo.dueClaims)
	}
}

func TestSMSBowerAutoFinishFailureKeepsActiveAndBacksOff(t *testing.T) {
	for _, response := range []string{"BAD_STATUS", "ACCESS_RETRY_GET", "ACCESS_ACTIVATION"} {
		t.Run(response, func(t *testing.T) {
			var calls atomic.Int32
			service, repo, now := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("action") == "setStatus" {
					calls.Add(1)
					_, _ = w.Write([]byte(response))
					return
				}
				_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
			})
			repo.order.RequestNextPending = true
			repo.order.CreatedAt = now.Add(-time.Hour)
			service.pollOne(context.Background(), repo.order)
			order, transitions, audits, _ := repo.snapshot()
			if order.Status != domain.OrderActive || order.PollFailures != 1 || !order.NextPollAt.Equal(now.Add(5*time.Second)) || len(transitions) != 0 || audits != 0 || calls.Load() != 1 {
				t.Fatalf("自动取消失败必须保留活跃并退避: order=%+v transitions=%v audits=%d calls=%d", order, transitions, audits, calls.Load())
			}
			if repo.resendClaims != 0 || order.ExpiresAt != nil {
				t.Fatalf("失败后不得续码或写入假期限: claims=%d expiry=%v", repo.resendClaims, order.ExpiresAt)
			}
		})
	}
}

func TestSMSBowerAutoFinishPersistsRetryCodeBeforeFailedCompletion(t *testing.T) {
	var pollCalls, finishCalls atomic.Int32
	var actions []string
	service, repo, now := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "setStatus" {
			actions = append(actions, r.URL.Query().Get("status"))
			if finishCalls.Add(1) == 1 {
				http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			} else {
				_, _ = w.Write([]byte("ACCESS_ACTIVATION"))
			}
			return
		}
		if pollCalls.Add(1) == 1 {
			_, _ = w.Write([]byte("STATUS_WAIT_RETRY:86420"))
		} else {
			_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
		}
	})
	service.pollOne(context.Background(), repo.order)
	order, _, _, _ := repo.snapshot()
	if order.Status != domain.OrderActive || order.PollFailures != 1 || len(order.Messages) != 1 || order.Messages[0].Code != "86420" || repo.advanceSaves != 0 || repo.resendClaims != 0 {
		t.Fatalf("lastCode必须持久化且不产生新代次，失败仍活跃: order=%+v advance=%d claims=%d", order, repo.advanceSaves, repo.resendClaims)
	}
	service.now = func() time.Time { return now.Add(5 * time.Second) }
	service.pollOne(context.Background(), order)
	order, _, _, _ = repo.snapshot()
	if order.Status != domain.OrderCompleted || len(actions) != 2 || actions[0] != "6" || actions[1] != "6" || repo.messageSaves != 1 {
		t.Fatalf("下次WAIT_CODE仍必须自动完成，不得反向取消: order=%+v actions=%v saves=%d", order, actions, repo.messageSaves)
	}
}

func TestSMSBowerAutoFinishDoesNotDuplicateMessagesAcrossFailure(t *testing.T) {
	service, repo, now := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "setStatus" {
			_, _ = w.Write([]byte("BAD_STATUS"))
			return
		}
		_, _ = w.Write([]byte("STATUS_OK:24680"))
	})
	repo.order.CanGetAnotherSMS = true
	service.pollOne(context.Background(), repo.order)
	order, _, _, _ := repo.snapshot()
	service.now = func() time.Time { return now.Add(5 * time.Second) }
	service.pollOne(context.Background(), order)
	order, _, _, _ = repo.snapshot()
	if len(order.Messages) != 1 || order.PollSequence != 1 || order.PollFailures != 2 || repo.resendClaims != 0 {
		t.Fatalf("完成失败不得重复入库同码或重置失败次数: order=%+v claims=%d", order, repo.resendClaims)
	}
}

func TestSMSBowerAutoFinishRequiresSuccessfulMessageSync(t *testing.T) {
	for _, kind := range []string{"poll_error", "configuration", "message_save", "last_code_save"} {
		t.Run(kind, func(t *testing.T) {
			var finishCalls atomic.Int32
			service, repo, now := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("action") == "setStatus" {
					finishCalls.Add(1)
					_, _ = w.Write([]byte("ACCESS_CANCEL"))
					return
				}
				if kind == "poll_error" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				} else if kind == "last_code_save" {
					_, _ = w.Write([]byte("STATUS_WAIT_RETRY:123456"))
				} else {
					_, _ = w.Write([]byte("STATUS_OK:123456"))
				}
			})
			if kind == "configuration" {
				repo.provider.APIKeyCipher = nil
			}
			if kind == "message_save" || kind == "last_code_save" {
				repo.messageSaveErr = errors.New("temporary save error")
			}
			service.pollOne(context.Background(), repo.order)
			order, transitions, _, _ := repo.snapshot()
			if order.Status != domain.OrderActive || order.ExpiresAt != nil || order.PollFailures != 1 || !order.NextPollAt.Equal(now.Add(5*time.Second)) || finishCalls.Load() != 0 || len(transitions) != 0 {
				t.Fatalf("同步不成功不得自动结束，必须退避: order=%+v calls=%d transitions=%v", order, finishCalls.Load(), transitions)
			}
		})
	}
}

func TestSMSBowerAutoFinishSkipsManualTerminalAndStopsPendingResend(t *testing.T) {
	for _, status := range []string{domain.OrderCompleted, domain.OrderCanceled, domain.OrderActive} {
		t.Run(status, func(t *testing.T) {
			var finishCalls atomic.Int32
			service, repo, _ := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("action") == "setStatus" {
					finishCalls.Add(1)
				}
				_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
			})
			snapshot := repo.order
			repo.order.Status = status
			repo.order.RequestNextPending = true
			if status != domain.OrderActive {
				service.pollOne(context.Background(), snapshot)
			}
			service.requestAnother(context.Background(), snapshot)
			order, transitions, _, _ := repo.snapshot()
			if order.Status != status || len(transitions) != 0 || finishCalls.Load() != 0 || repo.resendClaims != 0 {
				t.Fatalf("终态或业务期限后的旧任务不得续码: order=%+v transitions=%v calls=%d claims=%d", order, transitions, finishCalls.Load(), repo.resendClaims)
			}
		})
	}
}

func TestSMSBowerAutoFinishAndManualFinishShareOrderLock(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	var finishCalls atomic.Int32
	service, repo, _ := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "setStatus" {
			if finishCalls.Add(1) == 1 {
				close(entered)
			}
			<-release
			_, _ = w.Write([]byte("ACCESS_CANCEL"))
			return
		}
		_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
	})
	t.Cleanup(unlock)
	snapshot := repo.order
	done := make(chan struct{})
	go func() {
		service.pollOne(context.Background(), snapshot)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("自动取消未进入远端调用")
	}
	service.pollOne(context.Background(), snapshot)
	_, err := service.FinishOrder(context.Background(), snapshot.ID, "complete", domain.User{Role: "admin"}, "")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("自动动作持锁期间人工完成应冲突，实际=%v", err)
	}
	unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("自动取消未结束")
	}
	service.pollOne(context.Background(), snapshot)
	order, transitions, audits, _ := repo.snapshot()
	if order.Status != domain.OrderCanceled || finishCalls.Load() != 1 || len(transitions) != 1 || audits != 1 {
		t.Fatalf("并发轮询/人工动作只能形成一次自动终态: order=%+v calls=%d transitions=%v audits=%d", order, finishCalls.Load(), transitions, audits)
	}
}

func TestSMSBowerAutoFinishDeadlineAndOtherProviders(t *testing.T) {
	now := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	order := domain.Order{ProviderID: domain.ProviderSMSBower, CreatedAt: now}
	deadline := now.Add(25 * time.Minute)
	if smsBowerAutoFinishDue(order, deadline.Add(-time.Nanosecond)) || !smsBowerAutoFinishDue(order, deadline) {
		t.Fatal("25分钟截止必须包含精确边界")
	}
	for _, pid := range []string{domain.ProviderHeroSMS, domain.ProviderSMSPool} {
		other := domain.Order{ProviderID: pid, CreatedAt: now.Add(-time.Hour)}
		if smsBowerAutoFinishDue(other, now) || !nextSuccessfulPollAt(other, now.Add(time.Minute)).Equal(now.Add(time.Minute)) {
			t.Fatalf("SMSbower规则不得影响其他供应商: %s", pid)
		}
	}
	unknownCreated := domain.Order{ProviderID: domain.ProviderSMSBower}
	if smsBowerAutoFinishDue(unknownCreated, now) {
		t.Fatal("缺失创建时间时不得猜测自动结束时间")
	}
	expiresAt := now.Add(20 * time.Minute)
	order.ExpiresAt = &expiresAt
	if !nextSuccessfulPollAt(order, now.Add(time.Hour)).Equal(expiresAt) {
		t.Fatal("供应商真实期限较早时仍应按较早时间轮询")
	}
	order.ExpiresAt = nil
	if !nextSuccessfulPollAt(order, now.Add(time.Hour)).Equal(deadline) {
		t.Fatal("普通成功轮询不得调度晚于25分钟")
	}
}

func TestSMSBowerAutoFinishLockConflictShortensOnlyClaimedLease(t *testing.T) {
	for _, pollFails := range []bool{false, true} {
		name := "同步成功"
		if pollFails {
			name = "同步失败"
		}
		t.Run(name, func(t *testing.T) {
			service, repo, now := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
				if pollFails {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
			})
			repo.order.LastProviderState = "received:fingerprint"
			repo.order.PollFailures = 3
			repo.order.UpdatedAt = now.Add(-time.Minute)
			before := repo.order
			err := repo.WithOrderLock(context.Background(), repo.order.ID, func(context.Context) error {
				service.pollBatch(context.Background(), func(job func()) { job() })
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			order, _, _, _ := repo.snapshot()
			if !order.NextPollAt.Equal(now.Add(5*time.Second)) || order.LastProviderState != before.LastProviderState || order.PollFailures != before.PollFailures || !order.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("锁冲突只能缩短认领租约，不能覆盖状态/退避次数: before=%+v after=%+v", before, order)
			}

			// 另一个操作已经改写 next_poll_at 时，旧租约不再具有调整权。
			claim := before
			claim.NextPollAt = now.Add(5 * time.Minute)
			repo.mu.Lock()
			repo.order.NextPollAt = now.Add(time.Minute)
			repo.mu.Unlock()
			service.rescheduleSMSBowerDeadlinePoll(context.Background(), claim)
			order, _, _, _ = repo.snapshot()
			if !order.NextPollAt.Equal(now.Add(time.Minute)) {
				t.Fatalf("旧claim不得覆盖其他操作的新退避: next=%v", order.NextPollAt)
			}
		})
	}
}

func TestSMSBowerAutoFinishRequestAnotherCrossingDeadline(t *testing.T) {
	resendEntered, releaseResend := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResend) }) }
	var pollCalls, resendCalls, completeCalls atomic.Int32
	service, repo, deadline := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "setStatus" {
			switch r.URL.Query().Get("status") {
			case "3":
				resendCalls.Add(1)
				close(resendEntered)
				<-releaseResend
				_, _ = w.Write([]byte("ACCESS_RETRY_GET"))
			case "6":
				completeCalls.Add(1)
				_, _ = w.Write([]byte("ACCESS_ACTIVATION"))
			default:
				t.Error("已存短信的跨截止续码不得转为取消")
				http.Error(w, "BAD_STATUS", http.StatusBadRequest)
			}
			return
		}
		if pollCalls.Add(1) == 1 {
			_, _ = w.Write([]byte("STATUS_OK:456789"))
		} else {
			_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
		}
	})
	t.Cleanup(release)
	var clock atomic.Int64
	clock.Store(deadline.Add(-time.Second).UnixNano())
	service.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	repo.order.NextPollAt = service.now()
	repo.order.CanGetAnotherSMS = true
	beforeDeadlineDone := make(chan struct{})
	go func() {
		service.pollBatch(context.Background(), func(job func()) { job() })
		close(beforeDeadlineDone)
	}()
	select {
	case <-resendEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("截止前续码未开始")
	}
	clock.Store(deadline.UnixNano())
	service.pollBatch(context.Background(), func(job func()) { job() })
	order, _, _, _ := repo.snapshot()
	if !order.NextPollAt.Equal(deadline.Add(5 * time.Second)) {
		t.Fatalf("到期轮询锁冲突不应留下5分钟租约: next=%v", order.NextPollAt)
	}
	clock.Store(deadline.Add(5 * time.Second).UnixNano())
	release()
	select {
	case <-beforeDeadlineDone:
	case <-time.After(2 * time.Second):
		t.Fatal("截止前续码未释放订单锁")
	}
	service.pollBatch(context.Background(), func(job func()) { job() })
	order, transitions, _, _ := repo.snapshot()
	if order.Status != domain.OrderCompleted || resendCalls.Load() != 1 || completeCalls.Load() != 1 || len(transitions) != 1 {
		t.Fatalf("跨截止续码释放后应按短重试自动完成: order=%+v resend=%d complete=%d transitions=%v", order, resendCalls.Load(), completeCalls.Load(), transitions)
	}
}

func TestSMSBowerAutoFinishStaleActiveSnapshotRestoresDeadlinePoll(t *testing.T) {
	var repo *smsBowerAutoFinishRepository
	service, createdRepo, now := newSMSBowerAutoFinishTestService(t, func(w http.ResponseWriter, r *http.Request) {
		repo.mu.Lock()
		repo.order.UpdatedAt = repo.order.UpdatedAt.Add(time.Second)
		repo.mu.Unlock()
		_, _ = w.Write([]byte("STATUS_WAIT_CODE"))
	})
	repo = createdRepo
	repo.order.UpdatedAt = now.Add(-time.Minute)
	service.pollBatch(context.Background(), func(job func()) { job() })
	order, transitions, _, _ := repo.snapshot()
	if order.Status != domain.OrderActive || len(transitions) != 0 || !order.NextPollAt.Equal(now.Add(5*time.Second)) {
		t.Fatalf("同一activation的陈旧快照必须重新同步，不得遗留5min租约: order=%+v transitions=%v", order, transitions)
	}
}

// 保留接口断言，防止测试仓库新增方法遮蔽造成真实应用路径使用空接口。
var _ store.Repository = (*smsBowerAutoFinishRepository)(nil)
