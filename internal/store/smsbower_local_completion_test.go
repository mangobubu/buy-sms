package store

import (
	"strings"
	"testing"
)

func TestLocalCompletionAtomicallyRecordsConfirmationWithoutChangingMoney(t *testing.T) {
	query := strings.Join(strings.Fields(strings.ToLower(completeOrderLocallySQL)), " ")
	for _, part := range []string{
		"with closed as ( update orders set status='completed'",
		"last_provider_state='user_local_complete'",
		"request_next_pending=false", "request_next_inflight=false",
		"where id=$1 and provider_id='smsbower' and status='active'",
		"and renewal_inflight=false and request_next_inflight=false",
		"returning id ) insert into audit_logs",
		"select $2,'order.local_complete','order',id::text,nullif($3,'')::inet,$4 from closed",
	} {
		if !strings.Contains(query, part) {
			t.Fatalf("missing local completion constraint %q", part)
		}
	}
	for _, forbidden := range []string{"cost=", "currency=", "expires_at=", "delete", "sms_messages"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("local completion changes unrelated data: %q", forbidden)
		}
	}
}
