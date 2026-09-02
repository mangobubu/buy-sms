package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

type maintenanceContract interface {
	Maintenance(context.Context, time.Time) error
}

var _ maintenanceContract = (*Postgres)(nil)

// Maintenance 的 SQL 执行依赖真实 pgx 连接，当前 Postgres 没有可注入的窄
// 执行接口。这里锁定迁移与清理契约，真实删除行为留给隔离 PostgreSQL 集成测。
func TestMaintenanceSchemaContractCoversEveryRetainedDataset(t *testing.T) {
	migration, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := strings.ToLower(string(migration))
	for _, table := range []string{
		"captcha_challenges",
		"captcha_issuances",
		"auth_sessions",
		"login_attempts",
		"webhook_events",
	} {
		if !strings.Contains(schema, "create table if not exists "+table) {
			t.Fatalf("维护目标 %s 未在迁移中定义", table)
		}
	}

}
