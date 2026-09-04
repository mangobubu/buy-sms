package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"buysms/internal/domain"
)

type maintenanceContract interface {
	Maintenance(context.Context, time.Time) error
}

type orderNameSnapshotRow struct{}

func (orderNameSnapshotRow) Scan(dest ...any) error {
	if len(dest) != 39 {
		return fmt.Errorf("订单扫描列数=%d，期望 39", len(dest))
	}
	*dest[5].(*string) = "10"
	*dest[6].(*string) = "目录国家名称"
	*dest[7].(*string) = "hc"
	*dest[8].(*string) = "MOMO"
	*dest[9].(*string) = "gold"
	*dest[10].(*string) = "24"
	*dest[25].(*string) = "renewal-request-1"
	*dest[28].(*string) = "prolong"
	*dest[29].(*int) = 24
	*dest[30].(*string) = "hour"
	*dest[31].(*float64) = 1.25
	*dest[32].(*string) = "{}"
	*dest[34].(*time.Time) = time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	*dest[35].(*bool) = true
	return nil
}
func TestOrderNameSnapshotsAreIncludedInInsertAndScanContracts(t *testing.T) {
	order := domain.Order{
		CountryCode: "10", CountryName: "目录国家名称",
		ServiceCode: "hc", ServiceName: "MOMO", QualityTier: "gold", Duration: "24",
		CreatedAt: time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC),
	}
	args := orderInsertArgs(order)
	if len(args) != 18 {
		t.Fatalf("订单写入参数数=%d，期望 18", len(args))
	}
	if args[5] != "10" || args[6] != "目录国家名称" || args[7] != "hc" || args[8] != "MOMO" || args[9] != "gold" || args[10] != "24" {
		t.Fatalf("订单名称快照、档位或时长参数顺序错误: %#v", args[5:11])
	}
	if args[17] != order.CreatedAt {
		t.Fatalf("订单购买发起时间参数错误: %#v", args[17])
	}

	const readableColumns = "o.country_code,COALESCE(o.country_name,''),o.service_code,COALESCE(o.service_name,''),o.quality_tier,o.duration"
	if !strings.Contains(orderCols, readableColumns) {
		t.Fatalf("订单读取列未完整包含名称快照: %s", orderCols)
	}
	scanned, err := scanOrder(orderNameSnapshotRow{})
	if err != nil {
		t.Fatal(err)
	}
	if scanned.CountryName != "目录国家名称" || scanned.ServiceName != "MOMO" || scanned.QualityTier != "gold" || scanned.Duration != "24" {
		t.Fatalf("订单扫描结果错误: %+v", scanned)
	}
	if scanned.RenewalRequestID != "renewal-request-1" || scanned.RenewalBaseline != "{}" || scanned.RenewalMode != "prolong" || scanned.RenewalValue != 24 || scanned.RenewalUnit != "hour" || scanned.RenewalQuotedPrice != 1.25 {
		t.Fatalf("订单续期认领上下文扫描错误: %+v", scanned)
	}
	if scanned.ActivationStartedAt.IsZero() || !scanned.NonRefundable {
		t.Fatalf("订单当前激活周期扫描错误: %+v", scanned)
	}
	insert := strings.ToLower(insertOrderSQL)
	for _, fragment := range []string{
		"country_code,country_name,service_code,service_name,quality_tier,duration",
		"expires_at,created_at",
		"pc.kind = 'country'",
		"pc.kind = 'service'",
		"pc.country in ($6, '')",
	} {
		if !strings.Contains(insert, fragment) {
			t.Fatalf("订单写入 SQL 缺少名称解析约束 %q", fragment)
		}
	}
}

func TestOrderNameMigrationAndCatalogBackfillContracts(t *testing.T) {
	migration, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := strings.ToLower(string(migration))
	for _, fragment := range []string{
		"alter table orders add column if not exists country_name text",
		"alter table orders add column if not exists service_name text",
		"alter table orders add column if not exists duration text not null default ''",
		"alter table purchase_requests add column if not exists duration text not null default ''",
		"update orders as o",
		"pc.kind = 'country'",
		"pc.kind = 'service'",
		"nullif(btrim(o.country_name), '') is null",
		"nullif(btrim(o.service_name), '') is null",
	} {
		if !strings.Contains(schema, fragment) {
			t.Fatalf("订单名称迁移缺少约束 %q", fragment)
		}
	}

	backfill := strings.ToLower(backfillOrderNamesSQL)
	if !strings.Contains(backfill, "where o.provider_id = $1") {
		t.Fatal("目录刷新后的历史回填未限制供应商")
	}
	if strings.Count(backfill, "and exists (") < 2 {
		t.Fatal("目录刷新回填应只更新已有名称匹配的历史订单")
	}
	if !strings.Contains(backfill, "else o.country_name") || !strings.Contains(backfill, "else o.service_name") {
		t.Fatal("目录刷新回填可能覆盖已有订单名称快照")
	}
}

func TestFilteredCatalogUpsertDoesNotDeleteOtherItems(t *testing.T) {
	sql := strings.ToLower(upsertCatalogItemSQL)
	for _, fragment := range []string{
		"insert into provider_catalog",
		"on conflict(provider_id,kind,code,country) do update",
		"name=excluded.name",
		"price=coalesce(excluded.price,provider_catalog.price)",
		"stock=coalesce(excluded.stock,provider_catalog.stock)",
		"raw=excluded.raw",
		"updated_at=now()",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("筛选目录 upsert SQL 缺少约束 %q", fragment)
		}
	}
	if strings.Contains(sql, "delete") {
		t.Fatal("筛选目录 upsert 不应删除未返回的既有目录项")
	}
}

func TestPurchaseAttemptNamesUseProviderCatalogContract(t *testing.T) {
	query := strings.ToLower(listPurchaseRequestsSQL)
	for _, fragment := range []string{
		"from purchase_requests as pr",
		"pc.provider_id = pr.provider_id",
		"pc.kind = 'country'",
		"pc.code = pr.country_code",
		"pc.kind = 'service'",
		"pc.code = pr.service_code",
		"pc.country in (pr.country_code,'')",
		"order by (pc.country = pr.country_code) desc,pc.updated_at desc,pc.country,pc.name",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("购买尝试名称查询缺少约束 %q", fragment)
		}
	}
}

func TestDashboardTodayCostIncludesOnlyCompletedOrders(t *testing.T) {
	query := strings.Join(strings.Fields(strings.ToLower(dashboardSQL)), " ")
	if !strings.Contains(query, "sum(cost) filter ( where created_at >= date_trunc('day', now()) and status = 'completed' )") {
		t.Fatalf("今日支出 SQL 未限定为已完成订单: %s", query)
	}
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
func TestMaintenanceOnlyReleasesUnsubmittedRenewalClaims(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	statements := maintenanceStatements(now)
	for _, statement := range statements {
		query := strings.Join(strings.Fields(strings.ToLower(statement.query)), " ")
		if !strings.Contains(query, "update renewal_requests set status='failed'") {
			continue
		}
		for _, fragment := range []string{
			"status='provisioning'", "submitted_at is null", "updated_at<$1",
			"update orders set renewal_request_id=null", "renewal_inflight=false",
			"renewal_request_id in (select id from stale)",
		} {
			if !strings.Contains(query, fragment) {
				t.Fatalf("续期认领回收缺少流水/订单原子约束 %q: %s", fragment, query)
			}
		}
		if !statement.cutoff.Equal(now.Add(-2 * time.Minute)) {
			t.Fatalf("未提交续期认领回收时间=%v", statement.cutoff)
		}
		return
	}
	t.Fatal("维护任务缺少未提交续期流水与订单认领的原子回收")
}
func TestOrderLifecycleColumnsBelongOnlyToOrders(t *testing.T) {
	migration, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := strings.ToLower(string(migration))
	authStart := strings.Index(schema, "create table if not exists auth_sessions")
	authEnd := strings.Index(schema[authStart:], ");")
	ordersStart := strings.Index(schema, "create table if not exists orders")
	ordersEnd := strings.Index(schema[ordersStart:], ");")
	if authStart < 0 || authEnd < 0 || ordersStart < 0 || ordersEnd < 0 {
		t.Fatal("迁移缺少 auth_sessions 或 orders 表定义")
	}
	authTable := schema[authStart : authStart+authEnd]
	ordersTable := schema[ordersStart : ordersStart+ordersEnd]
	for _, column := range []string{"activation_started_at", "non_refundable"} {
		if strings.Contains(authTable, column) {
			t.Fatalf("生命周期字段 %s 被误加到 auth_sessions", column)
		}
		if !strings.Contains(ordersTable, column) {
			t.Fatalf("orders 新建定义缺少生命周期字段 %s", column)
		}
	}
	if strings.Count(schema, "alter table orders add column if not exists activation_started_at") != 1 ||
		strings.Count(schema, "alter table orders add column if not exists non_refundable") != 1 {
		t.Fatal("订单生命周期 ALTER 迁移存在重复或缺失")
	}
}
func TestRenewalRequestMigrationProvidesPersistentIdempotency(t *testing.T) {
	migration, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := strings.Join(strings.Fields(strings.ToLower(string(migration))), " ")
	for _, fragment := range []string{
		"create table if not exists renewal_requests",
		"idempotency_key text not null",
		"status text not null check(status in ('provisioning','unknown','succeeded','failed'))",
		"unique(user_id,idempotency_key)",
		"create unique index if not exists renewal_requests_order_pending on renewal_requests(order_id) where status in ('provisioning','unknown')",
		"alter table orders add column if not exists renewal_request_id uuid",
	} {
		if !strings.Contains(schema, fragment) {
			t.Fatalf("续期幂等迁移缺少约束 %q", fragment)
		}
	}
}

func TestRenewalRepositorySQLKeepsRequestAndOrderTransitionsAtomic(t *testing.T) {
	// 生产实现中的 Start/Mark/Complete/Release 都显式开启事务；这里以源码
	// 契约锁定关键状态条件，避免未来把资金流水和订单更新拆开。
	implementation, err := os.ReadFile("postgres.go")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join(strings.Fields(strings.ToLower(string(implementation))), " ")
	for _, fragment := range []string{
		"func (s *postgres) startorderrenewal",
		"status='provisioning'",
		"func (s *postgres) markorderrenewalsubmitted",
		"status='unknown'",
		"func (s *postgres) completeorderrenewal",
		"status='succeeded'",
		"func (s *postgres) releaseorderrenewal",
		"status='failed'",
		"renewal_request_id=$2",
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("续期流水原子状态实现缺少约束 %q", fragment)
		}
	}
}
