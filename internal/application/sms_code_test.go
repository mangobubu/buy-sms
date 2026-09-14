package application

import (
	"testing"
	"time"

	"buysms/internal/domain"
)

func TestOrderViewReconcilesHeroSMSCodeWithText(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		code     string
		text     string
		want     string
	}{
		{name: "reported extra three", code: "28953", text: "2895 la ma xac thuc OTP dang ky vi. Ma co hieu luc trong 3 phut.", want: "2895"},
		{name: "body without expiry", code: "28953", text: "2895 la ma xac thuc OTP dang ky vi", want: "2895"},
		{name: "different expiry digit", code: "28955", text: "Your OTP is 2895. Valid for 5 minutes.", want: "2895"},
		{name: "expiry before code", code: "32895", text: "Valid for 3 minutes. Your OTP is 2895.", want: "2895"},
		{name: "leading zeroes", code: "028953", text: "02895 is your OTP. Expires in 3 minutes.", want: "02895"},
		{name: "Chinese text without spaces", code: "28953", text: "验证码2895，有效期3分钟。", want: "2895"},
		{name: "repeated same code", code: "28953", text: "Your OTP is 2895. Enter 2895 within 3 minutes.", want: "2895"},
		{name: "unrelated reference", code: "28953", text: "Request 2026: OTP 2895. Expires in 3 minutes.", want: "2895"},
		{name: "provider alias", provider: "hero-sms", code: "28953", text: "OTP: 2895. Valid for 3 minutes.", want: "2895"},
		{name: "correct five digit code ending in three", code: "28953", text: "OTP: 28953. Valid for 3 minutes.", want: "28953"},
		{name: "correct six digit code ending in three", code: "128953", text: "OTP: 128953. Valid for 3 minutes.", want: "128953"},
		{name: "exact code after shorter number", code: "28953", text: "Reference 2895. Your OTP is 28953.", want: "28953"},
		{name: "exact code after ASCII label", code: "28953", text: "Reference 2895. OTP28953", want: "28953"},
		{name: "ambiguous body", code: "12345678", text: "Old OTP: 1234. New OTP: 5678.", want: "12345678"},
		{name: "unrelated code", code: "98763", text: "Your OTP is 2895. Valid for 3 minutes.", want: "98763"},
		{name: "missing text", code: "28953", want: "28953"},
		{name: "text without numbers", code: "28953", text: "Waiting for your verification message.", want: "28953"},
		{name: "missing upstream code", text: "OTP: 2895. Valid for 3 minutes."},
		{name: "alphanumeric upstream code", code: "AB28953", text: "OTP: 2895. Valid for 3 minutes.", want: "AB28953"},
		{name: "alphanumeric text token", code: "28953", text: "Reference A2895B. Expires in 3 minutes.", want: "28953"},
		{name: "long numeric identifier", code: "28953", text: "Reference 912895123456. Expires in 3 minutes.", want: "28953"},
		{name: "short number is not an OTP", code: "1233", text: "Request 123 expires in 3 minutes.", want: "1233"},
		{name: "hyphenated code", code: "28953", text: "Your OTP is 2895-3.", want: "28953"},
		{name: "space separated code", code: "28953", text: "Your OTP is 2895 3.", want: "28953"},
		{name: "split six digit code", code: "123456", text: "Your OTP is 123 456.", want: "123456"},
		{name: "SMSBower remains unchanged", provider: domain.ProviderSMSBower, code: "28953", text: "OTP: 2895. Valid for 3 minutes.", want: "28953"},
		{name: "SMSPool remains unchanged", provider: domain.ProviderSMSPool, code: "28953", text: "OTP: 2895. Valid for 3 minutes.", want: "28953"},
	}
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid := tt.provider
			if pid == "" {
				pid = domain.ProviderHeroSMS
			}
			for _, source := range []string{"poll", "webhook"} {
				for _, status := range []string{domain.OrderActive, domain.OrderCompleted} {
					t.Run(source+"/"+status, func(t *testing.T) {
						original := domain.SMSMessage{
							ID: "message-1", OrderID: "order-1", ProviderID: pid,
							Code: tt.code, Text: tt.text, Source: source,
							UpstreamFingerprint: "original-upstream-fingerprint", ReceivedAt: now,
						}
						order := domain.Order{
							ID: original.OrderID, ProviderID: pid, Status: status,
							LastProviderState: "original-provider-state", PollSequence: 2,
							Messages: []domain.SMSMessage{original}, CreatedAt: now,
						}
						view := OrderView(order, true, now)
						if len(view.Messages) != 1 {
							t.Fatalf("message count = %d, want 1", len(view.Messages))
						}
						message := view.Messages[0]
						if message.Code != tt.want {
							t.Errorf("display/copy code = %q, want %q", message.Code, tt.want)
						}
						if message.ID != original.ID || message.Content != tt.text || !message.ReceivedAt.Equal(now) {
							t.Errorf("message metadata or original text changed: %+v", message)
						}
						if order.Messages[0] != original || order.LastProviderState != "original-provider-state" || order.PollSequence != 2 {
							t.Fatal("presentation changed stored message or deduplication state")
						}
					})
				}
			}
		})
	}
}
