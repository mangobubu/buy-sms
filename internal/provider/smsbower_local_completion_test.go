package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSMSBowerCompleteWithConfirmationKeepsFinalMessages(t *testing.T) {
	for _, body := range []string{
		"STATUS_OK:987654",
		`{"status":"completed","data":[{"id":"final-message","smsCode":"987654"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := smsBowerLifecycleServer(t, []smsBowerLifecycleReply{{"setStatus", 0, "BAD_STATUS"}, {"getStatus", 0, body}})
			result, err := NewSMSBower(server.URL).CompleteWithConfirmation(context.Background(), "provider-secret", "activation-1")
			var upstream *ProviderError
			if !errors.As(err, &upstream) || len(result.Messages) != 1 || result.Messages[0].Code != "987654" {
				t.Fatalf("confirmation must preserve the final code in result: result=%+v error=%v", result, err)
			}
			if strings.Contains(err.Error(), "987654") || strings.Contains(err.Error(), "provider-secret") {
				t.Fatal("error string exposed confirmation data")
			}
			if result.State == PollCompleted && (upstream.Code != CodeActivationTerminal || upstream.ConfirmationState != PollCompleted) {
				t.Fatal("explicit terminal confirmation lost the actual terminal state")
			}
		})
	}
}

func TestSMSBowerCompleteWithConfirmationDoesNotTrustNumericTerminal(t *testing.T) {
	server := smsBowerLifecycleServer(t, []smsBowerLifecycleReply{{"setStatus", 0, "BAD_STATUS"}, {"getStatus", 0, `{"status":6,"data":[{"smsCode":"987654"}]}`}})
	result, err := NewSMSBower(server.URL).CompleteWithConfirmation(context.Background(), "provider-secret", "activation-1")
	var upstream *ProviderError
	if !errors.As(err, &upstream) || upstream.Code != "INVALID_RESPONSE" || upstream.Operation != "complete.confirm" || result.State != "" || len(result.Messages) != 0 {
		t.Fatalf("unverified terminal must not provide a recovery result: %+v, %v", result, err)
	}
}
