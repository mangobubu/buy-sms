package application

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/provider"
	"buysms/internal/secure"
)

func TestEvaluateCancelPolicyAcrossProviders(t *testing.T) {
	base := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		order            domain.Order
		now              time.Time
		allowed          bool
		code             string
		waitSeconds      int
		hasAvailableTime bool
	}{
		{
			name: "HeroSMS在119点999秒时拒绝",
			order: domain.Order{
				ProviderID: domain.ProviderHeroSMS, Status: domain.OrderActive, CreatedAt: base,
			},
			now: base.Add(119*time.Second + 999*time.Millisecond), code: OrderActionCodeCancelNotAvailableYet,
			waitSeconds: 1, hasAvailableTime: true,
		},
		{
			name: "HeroSMS在120秒边界允许",
			order: domain.Order{
				ProviderID: domain.ProviderHeroSMS, Status: domain.OrderActive, CreatedAt: base,
			},
			now: base.Add(120 * time.Second), allowed: true, hasAvailableTime: true,
		},
		{
			name: "HeroSMS二十四小时长租在20分钟前最后一毫秒仍允许",
			order: domain.Order{
				ProviderID: domain.ProviderHeroSMS, Status: domain.OrderActive, CreatedAt: base, Duration: "24",
			},
			now: base.Add(20*time.Minute - time.Millisecond), allowed: true, hasAvailableTime: true,
		},
		{
			name: "HeroSMS二十四小时长租恰好20分钟起拒绝",
			order: domain.Order{
				ProviderID: domain.ProviderHeroSMS, Status: domain.OrderActive, CreatedAt: base, Duration: "24",
			},
			now: base.Add(20 * time.Minute), code: OrderActionCodeCancelNotAllowed,
		},
		{
			name: "HeroSMS长租取消窗口按供应商期限反推的起点计算",
			order: domain.Order{
				ProviderID: domain.ProviderHeroSMS, Status: domain.OrderActive,
				CreatedAt: base.Add(10 * time.Second), Duration: "24",
				ExpiresAt: timePointer(base.Add(24 * time.Hour)),
			},
			now: base.Add(20 * time.Minute), code: OrderActionCodeCancelNotAllowed,
		},
		{
			name: "HeroSMS十二小时租期不应用二十分钟截止",
			order: domain.Order{
				ProviderID: domain.ProviderHeroSMS, Status: domain.OrderActive, CreatedAt: base, Duration: "12",
			},
			now: base.Add(20 * time.Minute), allowed: true, hasAvailableTime: true,
		},
		{
			name: "SMSBower刚购买即可尝试取消",
			order: domain.Order{
				ProviderID: domain.ProviderSMSBower, Status: domain.OrderActive, CreatedAt: base,
			},
			now: base, allowed: true,
		},
		{
			name: "SMSPool不写死本地等待时间",
			order: domain.Order{
				ProviderID: domain.ProviderSMSPool, Status: domain.OrderActive, CreatedAt: base,
			},
			now: base, allowed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := EvaluateCancelPolicy(tt.order, tt.now)
			if decision.Allowed != tt.allowed || decision.ErrorCode != tt.code || decision.WaitSeconds != tt.waitSeconds {
				t.Fatalf("取消策略=%+v，期望 allowed=%v code=%q wait=%d", decision, tt.allowed, tt.code, tt.waitSeconds)
			}
			if (decision.AvailableAt != nil) != tt.hasAvailableTime {
				t.Fatalf("可取消时间=%v，期望存在=%v", decision.AvailableAt, tt.hasAvailableTime)
			}
			if decision.AvailableAt != nil && !decision.AvailableAt.Equal(base.Add(timedCancelDelay)) {
				t.Fatalf("可取消时间=%v，期望=%v", decision.AvailableAt, base.Add(timedCancelDelay))
			}
		})
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func TestEvaluateCancelPolicyRejectsMessagesTerminalAndExpiredOrders(t *testing.T) {
	now := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	withMessage := domain.Order{
		ProviderID: domain.ProviderSMSPool, Status: domain.OrderActive,
		Messages: []domain.SMSMessage{{ID: "message-1"}},
	}
	decision := EvaluateCancelPolicy(withMessage, now)
	if decision.Allowed || decision.ErrorCode != OrderActionCodeCancelNotAllowed || decision.UnavailableReason != cancelReasonMessageExists {
		t.Fatalf("已有短信的取消策略错误: %+v", decision)
	}

	for _, status := range []string{domain.OrderCompleted, domain.OrderCanceled, domain.OrderExpired} {
		t.Run(status, func(t *testing.T) {
			decision := EvaluateCancelPolicy(domain.Order{ProviderID: domain.ProviderHeroSMS, Status: status}, now)
			if decision.Allowed || decision.ErrorCode != OrderActionCodeCancelNotAllowed || decision.UnavailableReason != cancelReasonOrderFinished {
				t.Fatalf("终态 %s 的取消策略错误: %+v", status, decision)
			}
		})
	}

	expiresAt := now
	decision = EvaluateCancelPolicy(domain.Order{
		ProviderID: domain.ProviderSMSPool, Status: domain.OrderActive, ExpiresAt: &expiresAt,
	}, now)
	if decision.Allowed || decision.ErrorCode != OrderActionCodeCancelNotAllowed || decision.UnavailableReason != cancelReasonOrderExpired {
		t.Fatalf("已到期 active 订单的取消策略错误: %+v", decision)
	}
}

func TestHeroSMSDeterministicCancelErrorsAreNotReportedAsProviderFailures(t *testing.T) {
	tests := []struct {
		code, message string
	}{
		{"FREE_CANCELLATION_EXPIRED", "长租号码的免费取消期限已过，当前号码不能取消"},
		{"OTP_RECEIVED", "号码已收到短信，不能取消"},
		{"NEW_OTP_RECEIVED", "号码已收到短信，不能取消"},
		{"ACTIVATION_NOT_ACTIVE", "号码已结束，不能取消"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			actionErr := orderActionProviderError("cancel", &provider.ProviderError{
				Provider: domain.ProviderHeroSMS, Operation: "cancel", Code: tt.code,
				HTTPStatus: http.StatusConflict,
			})
			if actionErr.Code != OrderActionCodeCancelNotAllowed || actionErr.Message != tt.message ||
				!errors.Is(actionErr, ErrConflict) {
				t.Fatalf("取消错误映射=%+v，期望确定性的不可取消冲突", actionErr)
			}
		})
	}
}

func TestOrderViewExposesCancelDecision(t *testing.T) {
	createdAt := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	view := OrderView(domain.Order{
		ID: "order-view", ProviderID: domain.ProviderHeroSMS, Status: domain.OrderActive,
		CreatedAt: createdAt,
	}, false, createdAt.Add(119*time.Second))
	if view.CanCancel || view.CancelAvailableAt == nil || !view.CancelAvailableAt.Equal(createdAt.Add(2*time.Minute)) {
		t.Fatalf("订单视图缺少取消时间信息: %+v", view)
	}
	if view.CancelWaitSeconds == nil || *view.CancelWaitSeconds != 1 || view.CancelUnavailableReason == "" {
		t.Fatalf("订单视图缺少取消等待信息: %+v", view)
	}
}

func TestFinishOrderAppliesCancelPolicyInsideOrderLock(t *testing.T) {
	now := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		providerID     string
		createdAt      time.Time
		response       string
		responseStatus int
		wantCode       string
		wantCalls      int32
		wantCanceled   bool
	}{
		{
			name: "HeroSMS未满120秒不调用上游", providerID: domain.ProviderHeroSMS,
			createdAt:      now.Add(-119*time.Second - 999*time.Millisecond),
			responseStatus: http.StatusNoContent, wantCode: OrderActionCodeCancelNotAvailableYet,
		},
		{
			name: "HeroSMS恰好120秒调用上游", providerID: domain.ProviderHeroSMS,
			createdAt: now.Add(-120 * time.Second), responseStatus: http.StatusNoContent,
			wantCalls: 1, wantCanceled: true,
		},
		{
			name: "SMSBower刚购买即调用上游", providerID: domain.ProviderSMSBower,
			createdAt: now, response: "ACCESS_CANCEL", responseStatus: http.StatusOK,
			wantCalls: 1, wantCanceled: true,
		},
		{
			name: "SMSBower上游偶发早期拒绝映射为可重试冲突", providerID: domain.ProviderSMSBower,
			createdAt: now, response: "EARLY_CANCEL_DENIED", responseStatus: http.StatusOK,
			wantCode: OrderActionCodeCancelNotAvailableYet, wantCalls: 1,
		},
		{
			name: "SMSPool上游短暂锁定映射为可重试冲突", providerID: domain.ProviderSMSPool,
			createdAt: now, response: `{"success":0,"message":"Your order cannot be cancelled yet, please try again later."}`,
			responseStatus: http.StatusOK, wantCode: OrderActionCodeCancelNotAvailableYet, wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if tt.providerID == domain.ProviderSMSBower {
					if request.URL.Query().Get("action") != "setStatus" || request.URL.Query().Get("status") != "8" {
						t.Errorf("SMSBower取消参数错误: %s", request.URL.RawQuery)
					}
				}
				writer.WriteHeader(tt.responseStatus)
				_, _ = writer.Write([]byte(tt.response))
			}))
			t.Cleanup(server.Close)

			baseURL := server.URL + "/api/v1"
			if tt.providerID == domain.ProviderSMSBower {
				baseURL = server.URL + "/stubs/handler_api.php"
			} else if tt.providerID == domain.ProviderSMSPool {
				baseURL = server.URL
			}
			vault, err := secure.NewVault([]byte("cancel-policy-finish-order-key"))
			if err != nil {
				t.Fatal(err)
			}
			apiKey, err := vault.Encrypt("provider-secret")
			if err != nil {
				t.Fatal(err)
			}
			repo := &terminalLockRepository{
				provider: domain.Provider{ID: tt.providerID, BaseURL: baseURL, APIKeyCipher: apiKey},
				order: domain.Order{
					ID: "cancel-policy-order", UserID: "operator-1", ProviderID: tt.providerID,
					UpstreamID: "upstream-order", Status: domain.OrderActive, CreatedAt: tt.createdAt,
				},
			}
			service := New(repo, nil, vault, config.Config{})
			service.now = func() time.Time { return now }

			_, actionErr := service.FinishOrder(context.Background(), repo.order.ID, "cancel", domain.User{ID: "operator-1", Role: "operator"}, "127.0.0.1")
			if tt.wantCode == "" && actionErr != nil {
				t.Fatalf("取消失败: %v", actionErr)
			}
			if tt.wantCode != "" {
				var contract *OrderActionError
				if !errors.As(actionErr, &contract) || contract.Code != tt.wantCode || !errors.Is(actionErr, ErrConflict) {
					t.Fatalf("取消错误=%v，期望 code=%q 且兼容 ErrConflict", actionErr, tt.wantCode)
				}
			}
			order, transitions, _, lockCalls := repo.snapshot()
			if calls.Load() != tt.wantCalls || lockCalls != 1 {
				t.Fatalf("调用次数错误: provider=%d lock=%d", calls.Load(), lockCalls)
			}
			if tt.wantCanceled {
				if order.Status != domain.OrderCanceled || len(transitions) != 1 {
					t.Fatalf("取消成功后状态错误: order=%+v transitions=%v", order, transitions)
				}
			} else if order.Status != domain.OrderActive || len(transitions) != 0 {
				t.Fatalf("取消拒绝后状态被修改: order=%+v transitions=%v", order, transitions)
			}
		})
	}
}

func TestFinishOrderRejectsUnknownActionBeforeLock(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	vault, err := secure.NewVault([]byte("unknown-order-action-key"))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, err := vault.Encrypt("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	repo := &terminalLockRepository{
		provider: domain.Provider{ID: domain.ProviderHeroSMS, BaseURL: server.URL + "/api/v1", APIKeyCipher: apiKey},
		order: domain.Order{
			ID: "unknown-action-order", UserID: "operator-1", ProviderID: domain.ProviderHeroSMS,
			UpstreamID: "unknown-action-upstream", Status: domain.OrderActive,
		},
	}
	service := New(repo, nil, vault, config.Config{})

	_, actionErr := service.FinishOrder(context.Background(), repo.order.ID, "archive", domain.User{ID: "operator-1", Role: "operator"}, "127.0.0.1")
	if !errors.Is(actionErr, ErrBadRequest) {
		t.Fatalf("非法动作错误=%v，期望 ErrBadRequest", actionErr)
	}
	order, transitions, audits, lockCalls := repo.snapshot()
	if providerCalls.Load() != 0 || lockCalls != 0 || len(transitions) != 0 || audits != 0 {
		t.Fatalf("非法动作产生副作用: provider=%d lock=%d transitions=%v audits=%d", providerCalls.Load(), lockCalls, transitions, audits)
	}
	if order.Status != domain.OrderActive {
		t.Fatalf("非法动作修改了订单状态: %+v", order)
	}
}
func TestEvaluateCancelPolicyUsesCurrentActivationLifecycle(t *testing.T) {
	startedAt := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	order := domain.Order{
		ProviderID: domain.ProviderHeroSMS, Status: domain.OrderActive,
		CreatedAt: startedAt.Add(-24 * time.Hour), ActivationStartedAt: startedAt,
		Messages: []domain.SMSMessage{{ID: "old", ReceivedAt: startedAt.Add(-time.Hour)}},
	}
	before := EvaluateCancelPolicy(order, startedAt.Add(119*time.Second))
	if before.Allowed || before.ErrorCode != OrderActionCodeCancelNotAvailableYet {
		t.Fatalf("重新启用后的取消窗口应从新周期起点计算: %+v", before)
	}
	after := EvaluateCancelPolicy(order, startedAt.Add(2*time.Minute))
	if !after.Allowed {
		t.Fatalf("上一周期短信不应阻止新周期取消: %+v", after)
	}
	order.Messages = append(order.Messages, domain.SMSMessage{ID: "current", ReceivedAt: startedAt})
	withCurrent := EvaluateCancelPolicy(order, startedAt.Add(2*time.Minute))
	if withCurrent.Allowed || withCurrent.UnavailableReason != cancelReasonMessageExists {
		t.Fatalf("当前周期短信必须关闭取消入口: %+v", withCurrent)
	}
}

func TestEvaluateCancelPolicyRejectsNonRefundableReactivation(t *testing.T) {
	order := domain.Order{ProviderID: domain.ProviderSMSPool, Status: domain.OrderActive, NonRefundable: true}
	decision := EvaluateCancelPolicy(order, time.Now())
	if decision.Allowed || decision.ErrorCode != OrderActionCodeCancelNotAllowed || decision.UnavailableReason == "" {
		t.Fatalf("SMSPool 重新启用订单必须明确禁止退款取消: %+v", decision)
	}
}
func TestEvaluateCancelPolicyBlocksRenewalPending(t *testing.T) {
	decision := EvaluateCancelPolicy(domain.Order{
		ProviderID: domain.ProviderHeroSMS, Status: domain.OrderActive, RenewalInflight: true,
	}, time.Now())
	if decision.Allowed || decision.ErrorCode != OrderActionCodeRenewalInProgress || decision.UnavailableReason == "" {
		t.Fatalf("续期待确认期间取消策略=%+v", decision)
	}
}
