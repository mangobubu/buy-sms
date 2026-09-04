package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseOTPListPreservesTerminalStateWhenMessagesExist(t *testing.T) {
	tests := []struct {
		state string
		want  string
	}{
		{state: "completed", want: PollCompleted},
		{state: "canceled", want: PollCanceled},
		{state: "expired", want: PollExpired},
		{state: "refunded", want: PollRefunded},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			payload := []byte(`{"status":"` + tt.state + `","otpList":[{"id":"message-1","smsCode":"24680","smsText":"code 24680"}]}`)
			messages, state, _, err := parseOTPList(payload)
			if err != nil {
				t.Fatal(err)
			}
			if state != tt.want || len(messages) != 1 || messages[0].Code != "24680" {
				t.Fatalf("终态短信解析异常: state=%q messages=%+v want=%q", state, messages, tt.want)
			}
		})
	}
}

func TestSMSPoolPollPreservesTerminalStateWhenMessageExists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"completed","sms":"13579","full_sms":"code 13579"}`))
	}))
	t.Cleanup(server.Close)

	result, err := NewSMSPool(server.URL).Poll(context.Background(), "provider-secret", "order-terminal")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != PollCompleted || len(result.Messages) != 1 || result.Code != "13579" {
		t.Fatalf("SMSPool 终态短信解析异常: %+v", result)
	}
}
