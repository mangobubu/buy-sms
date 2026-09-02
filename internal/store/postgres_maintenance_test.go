package store

import (
	"strings"
	"testing"
	"time"
)

func TestMaintenanceStatementsUsePrecomputedCutoffs(t *testing.T) {
	now := time.Date(2026, time.September, 2, 17, 57, 31, 123456789, time.FixedZone("CST", 8*60*60))
	want := []maintenanceStatement{
		{`DELETE FROM captcha_challenges WHERE expires_at<$1`, now.Add(-time.Hour)},
		{`DELETE FROM captcha_issuances WHERE issued_at<$1`, now.Add(-24 * time.Hour)},
		{`DELETE FROM auth_sessions WHERE expires_at<$1 OR revoked_at<$1`, now.Add(-7 * 24 * time.Hour)},
		{`DELETE FROM login_attempts WHERE attempted_at<$1`, now.Add(-7 * 24 * time.Hour)},
		{`DELETE FROM webhook_events WHERE received_at<$1`, now.Add(-90 * 24 * time.Hour)},
	}

	got := maintenanceStatements(now)
	if len(got) != len(want) {
		t.Fatalf("维护语句数=%d，期望=%d", len(got), len(want))
	}
	for i := range want {
		if got[i].query != want[i].query {
			t.Errorf("第 %d 条维护 SQL=%q，期望=%q", i, got[i].query, want[i].query)
		}
		if strings.Contains(strings.ToLower(got[i].query), "interval") {
			t.Errorf("第 %d 条维护 SQL 仍在数据库端计算 interval: %q", i, got[i].query)
		}
		if !got[i].cutoff.Equal(want[i].cutoff) {
			t.Errorf("第 %d 条截止时间=%s，期望=%s", i, got[i].cutoff, want[i].cutoff)
		}
	}
}
