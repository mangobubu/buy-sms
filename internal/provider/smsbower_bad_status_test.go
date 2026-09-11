package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSMSBowerCompleteConfirmsBadStatus(t *testing.T) {
	for _, tt := range []struct {
		name, code, state, operation string
		finishHTTP, confirmHTTP      int
		body                         string
	}{
		{name: "不存在", body: "NO_ACTIVATION", code: CodeActivationMissing, state: PollMissing},
		{name: "JSON明确不存在", body: `{"status":"error","code":"NO_ACTIVATION"}`, code: CodeActivationMissing, state: PollMissing},
		{name: "已取消不冒充完成", body: "STATUS_CANCEL", code: CodeActivationTerminal, state: PollCanceled},
		{name: "明确已完成", body: `{"status":"completed"}`, code: CodeActivationTerminal, state: PollCompleted},
		{name: "明确已过期", body: `{"status":"expired"}`, code: CodeActivationTerminal, state: PollExpired},
		{name: "明确已退款不再次退款", body: `{"status":"refunded"}`, code: CodeActivationTerminal, state: PollRefunded},
		{name: "已收到短信不证明结束", body: "STATUS_OK:123456", code: "BAD_STATUS", state: PollReceived},
		{name: "继续接码不证明结束", body: "STATUS_WAIT_RETRY:123456", code: "BAD_STATUS", state: PollWaitingRetry},
		{name: "等待重发不证明结束", body: "STATUS_WAIT_RESEND", code: "BAD_STATUS", state: PollWaitingRetry},
		{name: "等待短信不结束", body: "STATUS_WAIT_CODE", code: "BAD_STATUS", state: PollWaiting},
		{name: "HTTP状态冲突也需确认", finishHTTP: http.StatusConflict, body: "STATUS_WAIT_CODE", code: "BAD_STATUS", state: PollWaiting},
		{name: "HTTP422状态冲突可确认缺失", finishHTTP: http.StatusUnprocessableEntity, body: "NO_ACTIVATION", code: CodeActivationMissing, state: PollMissing},
		{name: "确认时拒绝状态不伪造终态", body: "BAD_STATUS", code: "BAD_STATUS", operation: "complete.confirm"},
		{name: "确认时鉴权失败", body: "BAD_KEY", code: "BAD_KEY", operation: "complete.confirm"},
		{name: "确认时服务器错误不能信任正文", body: "NO_ACTIVATION", confirmHTTP: http.StatusServiceUnavailable, code: "NO_ACTIVATION", operation: "complete.confirm"},
		{name: "确认时限流不能信任正文", body: "NO_ACTIVATION", confirmHTTP: http.StatusTooManyRequests, code: "NO_ACTIVATION", operation: "complete.confirm"},
		{name: "确认时HTTP401不能信任正文", body: "NO_ACTIVATION", confirmHTTP: http.StatusUnauthorized, code: "NO_ACTIVATION", operation: "complete.confirm"},
		{name: "确认解析错误保留确认阶段", body: "<html>unavailable</html>", code: "INVALID_RESPONSE", operation: "complete.confirm"},
		{name: "不套用其他供应商数字退款状态", body: `{"status":6}`, code: "INVALID_RESPONSE", operation: "complete.confirm"},
		{name: "不套用其他供应商数字过期状态", body: `{"status":"2"}`, code: "INVALID_RESPONSE", operation: "complete.confirm"},
		{name: "不套用其他供应商数字取消状态", body: `{"activationStatus":5}`, code: "INVALID_RESPONSE", operation: "complete.confirm"},
		{name: "确认含未持久化短信不提前关闭", body: `{"status":"completed","data":[{"id":"latest-message","smsCode":"123456"}]}`, code: "INVALID_RESPONSE", operation: "complete.confirm"},
		{name: "取消文本异常后缀不确认", body: "STATUS_CANCEL:not-a-status", code: "INVALID_RESPONSE", operation: "complete.confirm"},
		{name: "确认响应含密钥不泄漏", body: "provider-secret", code: "UPSTREAM_ERROR", operation: "complete.confirm"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := smsBowerLifecycleServer(t, []smsBowerLifecycleReply{
				{"setStatus", tt.finishHTTP, "BAD_STATUS"}, {"getStatus", tt.confirmHTTP, tt.body},
			})
			err := NewSMSBower(server.URL).Complete(context.Background(), "provider-secret", "activation-1")
			var upstream *ProviderError
			if !errors.As(err, &upstream) || upstream.Code != tt.code || upstream.ConfirmationState != tt.state {
				t.Fatalf("error=%+v want code=%s state=%s", err, tt.code, tt.state)
			}
			operation := tt.operation
			if operation == "" {
				operation = "complete"
			}
			if upstream.Operation != operation {
				t.Fatalf("operation=%s want=%s", upstream.Operation, operation)
			}
			if tt.code == CodeActivationMissing || tt.code == CodeActivationTerminal {
				if upstream.HTTPStatus != 0 || upstream.Retryable {
					t.Fatalf("confirmed state has invalid proof: %+v", upstream)
				}
			}
			for _, secret := range []string{"123456", "provider-secret"} {
				if strings.Contains(err.Error()+upstream.ConfirmationState, secret) {
					t.Fatal("confirmation error exposes SMS or API key")
				}
			}
		})
	}
}

func TestSMSBowerBadStatusOnTransportFailureIsNotBusinessRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := smsBowerLifecycleServer(t, []smsBowerLifecycleReply{{"setStatus", status, "BAD_STATUS"}})
			err := NewSMSBower(server.URL).Complete(context.Background(), "provider-secret", "activation-1")
			var upstream *ProviderError
			if !errors.As(err, &upstream) || upstream.HTTPStatus != status || upstream.ConfirmationState != "" {
				t.Fatalf("transport failure was treated as confirmed state: %+v", err)
			}
		})
	}
}

func TestSMSBowerBadStatusConfirmationTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "getStatus" {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("BAD_STATUS"))
	}))
	t.Cleanup(server.Close)
	err := NewSMSBower(server.URL, WithTimeout(100*time.Millisecond)).Complete(context.Background(), "provider-secret", "activation-1")
	var upstream *ProviderError
	if !errors.As(err, &upstream) || upstream.Code != "TIMEOUT" || !upstream.Retryable ||
		upstream.Operation != "complete.confirm" || upstream.ConfirmationState != "" {
		t.Fatalf("confirmation timeout must not be a terminal result: %v", err)
	}
}
