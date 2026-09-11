package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type smsBowerLifecycleReply struct {
	action string
	status int
	body   string
}

func smsBowerLifecycleServer(t *testing.T, replies []smsBowerLifecycleReply) *httptest.Server {
	t.Helper()
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		action := query.Get("action")
		actions = append(actions, action)
		if r.Method != http.MethodGet || query.Get("api_key") != "provider-secret" || query.Get("id") != "activation-1" {
			t.Error("unexpected lifecycle request parameters")
		}
		if action == "getStatus" && (query.Get("page") != "" || query.Get("size") != "") {
			t.Error("getStatus must not contain SMS pagination")
		}
		if action == "setStatus" && query.Get("status") != "6" {
			t.Error("only complete is expected")
		}
		index := len(actions) - 1
		if index >= len(replies) {
			t.Error("unexpected additional lifecycle request")
			http.Error(w, "UNEXPECTED_REQUEST", http.StatusInternalServerError)
			return
		}
		reply := replies[index]
		if action != reply.action {
			t.Errorf("action=%q want=%q", action, reply.action)
		}
		if reply.status != 0 {
			w.WriteHeader(reply.status)
		}
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(func() {
		server.Close()
		var want []string
		for _, reply := range replies {
			want = append(want, reply.action)
		}
		if !reflect.DeepEqual(actions, want) {
			t.Errorf("actions=%v want=%v", actions, want)
		}
	})
	return server
}

func TestSMSBowerPollConfirmsMissingActivation(t *testing.T) {
	tests := []struct {
		name, state, code string
		replies           []smsBowerLifecycleReply
	}{
		{
			name: "标准状态再次确认不存在", state: PollMissing,
			replies: []smsBowerLifecycleReply{{"getAllSms", 0, "NO_ACTIVATION"}, {"getStatus", 0, "NO_ACTIVATION"}, {"getStatus", 0, "NO_ACTIVATION"}},
		},
		{
			name: "兼容接口不支持但标准状态仍可收码", state: PollReceived,
			replies: []smsBowerLifecycleReply{{"getAllSms", 0, "NO_ACTIVATION"}, {"getStatus", 0, "STATUS_OK:123456"}},
		},
		{
			name: "再次查询已恢复则继续等待", state: PollWaiting,
			replies: []smsBowerLifecycleReply{{"getAllSms", 0, "BAD_ACTION"}, {"getStatus", 0, "NO_ACTIVATION"}, {"getStatus", 0, "STATUS_WAIT_CODE"}},
		},
		{
			name: "HTTP业务缺失需标准状态确认", state: PollMissing,
			replies: []smsBowerLifecycleReply{{"getAllSms", 404, "NO_ACTIVATION"}, {"getStatus", 404, "NO_ACTIVATION"}},
		},
		{
			name: "普通404不是订单缺失", code: "NOT_FOUND",
			replies: []smsBowerLifecycleReply{{"getAllSms", 404, "NOT_FOUND"}},
		},
		{
			name: "服务器错误不是订单缺失", code: "NO_ACTIVATION",
			replies: []smsBowerLifecycleReply{{"getAllSms", 503, "NO_ACTIVATION"}},
		},
		{
			name: "确认查询失败仍是错误", code: "NO_CONNECTION",
			replies: []smsBowerLifecycleReply{{"getAllSms", 0, "NO_ACTIVATION"}, {"getStatus", 0, "NO_ACTIVATION"}, {"getStatus", 503, "NO_CONNECTION"}},
		},
		{
			name: "历史短信正常返回不额外查询", state: PollReceived,
			replies: []smsBowerLifecycleReply{{"getAllSms", 0, `{"data":[{"id":"message-1","smsCode":"123456"}]}`}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := smsBowerLifecycleServer(t, tt.replies)
			result, err := NewSMSBower(server.URL).Poll(context.Background(), "provider-secret", "activation-1")
			if tt.code != "" {
				var upstream *ProviderError
				if !errors.As(err, &upstream) || upstream.Code != tt.code || result.State == PollMissing {
					t.Fatalf("result=%+v err=%v want error=%s", result, err, tt.code)
				}
				return
			}
			if err != nil || result.State != tt.state {
				t.Fatalf("result=%+v err=%v want state=%s", result, err, tt.state)
			}
			if result.State == PollMissing && (result.CanRequestAnother || len(result.Messages) != 0 || result.Raw != nil) {
				t.Fatalf("missing confirmation must not fabricate messages or preserve a raw response: %+v", result)
			}
		})
	}
}

func TestSMSBowerCompleteRequiresMissingConfirmation(t *testing.T) {
	tests := []struct {
		name, code string
		replies    []smsBowerLifecycleReply
	}{
		{
			name:    "完成成功不查询状态",
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "ACCESS_ACTIVATION"}},
		},
		{
			name: "确认缺失返回明确错误而非伪造远端成功", code: CodeActivationMissing,
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "NO_ACTIVATION"}, {"getStatus", 0, "NO_ACTIVATION"}},
		},
		{
			name: "标准状态仍有效不能收口", code: "NO_ACTIVATION",
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "NO_ACTIVATION"}, {"getStatus", 0, "STATUS_OK:123456"}},
		},
		{
			name: "确认取消返回实际终态而非完成成功", code: CodeActivationTerminal,
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "NO_ACTIVATION"}, {"getStatus", 0, "STATUS_CANCEL"}},
		},
		{
			name: "状态不匹配确认仍有效则保留错误", code: "BAD_STATUS",
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "BAD_STATUS"}, {"getStatus", 0, "STATUS_WAIT_CODE"}},
		},
		{
			name: "确认时限流不能收口", code: "NO_ACTIVATION",
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "NO_ACTIVATION"}, {"getStatus", 429, "NO_ACTIVATION"}},
		},
		{
			name: "确认时鉴权失败不能收口", code: "NO_ACTIVATION",
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "NO_ACTIVATION"}, {"getStatus", 401, "NO_ACTIVATION"}},
		},
		{
			name: "确认时服务器故障不能收口", code: "NO_ACTIVATION",
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "NO_ACTIVATION"}, {"getStatus", 503, "NO_ACTIVATION"}},
		},
		{
			name: "确认响应不合法不能收口", code: "INVALID_RESPONSE",
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "NO_ACTIVATION"}, {"getStatus", 0, "<html>unavailable</html>"}},
		},
		{
			name: "确认响应含凭证不能收口或泄露", code: "UPSTREAM_ERROR",
			replies: []smsBowerLifecycleReply{{"setStatus", 0, "NO_ACTIVATION"}, {"getStatus", 0, "provider-secret"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := smsBowerLifecycleServer(t, tt.replies)
			err := NewSMSBower(server.URL).Complete(context.Background(), "provider-secret", "activation-1")
			if tt.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var upstream *ProviderError
			if !errors.As(err, &upstream) || upstream.Code != tt.code {
				t.Fatalf("err=%v want code=%s", err, tt.code)
			}
			if strings.Contains(err.Error(), "provider-secret") {
				t.Fatal("error exposes API key")
			}
		})
	}
}

func TestLegacyHeroSMSDoesNotTreatMissingAsSMSBowerCompletion(t *testing.T) {
	server := smsBowerLifecycleServer(t, []smsBowerLifecycleReply{{"setStatus", 0, "NO_ACTIVATION"}})
	err := NewHeroSMS(server.URL+"/stubs/handler_api.php").Complete(context.Background(), "provider-secret", "activation-1")
	var upstream *ProviderError
	if !errors.As(err, &upstream) || upstream.Code != "NO_ACTIVATION" {
		t.Fatalf("HeroSMS missing handling changed: %v", err)
	}
}

func TestSMSBowerMissingConfirmationTimeoutIsNotCompletion(t *testing.T) {
	for _, operation := range []string{"poll", "complete"} {
		t.Run(operation, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("action") == "getStatus" {
					<-r.Context().Done()
					return
				}
				_, _ = w.Write([]byte("NO_ACTIVATION"))
			}))
			t.Cleanup(server.Close)
			client := NewSMSBower(server.URL, WithTimeout(100*time.Millisecond))
			var err error
			if operation == "complete" {
				err = client.Complete(context.Background(), "provider-secret", "activation-1")
			} else {
				var result PollResult
				result, err = client.Poll(context.Background(), "provider-secret", "activation-1")
				if result.State == PollMissing {
					t.Fatal("timeout must not confirm missing")
				}
			}
			var upstream *ProviderError
			if !errors.As(err, &upstream) || upstream.Code != "TIMEOUT" || !upstream.Retryable ||
				!errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout must remain retryable, got %v", err)
			}
		})
	}
}
