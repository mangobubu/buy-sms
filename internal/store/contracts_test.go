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
	if len(dest) != 27 {
		return fmt.Errorf("订单扫描列数=%d，期望 27", len(dest))
	}
	*dest[5].(*string) = "10"
	*dest[6].(*string) = "目录国家名称"
	*dest[7].(*string) = "hc"
	*dest[8].(*string) = "MOMO"
	*dest[9].(*string) = "gold"
	return nil
}

func TestOrderNameSnapshotsAreIncludedInInsertAndScanContracts(t *testing.T) {
	order := domain.Order{
		CountryCode: "10", CountryName: "目录国家名称",
		ServiceCode: "hc", ServiceName: "MOMO", QualityTier: "gold",
	}
	args := orderInsertArgs(order)
	if len(args) != 16 {
		t.Fatalf("订单写入参数数=%d，期望 16", len(args))
	}
	if args[5] != "10" || args[6] != "目录国家名称" || args[7] != "hc" || args[8] != "MOMO" || args[9] != "gold" {
		t.Fatalf("订单名称快照或档位参数顺序错误: %#v", args[5:10])
	}

	const readableColumns = "o.country_code,COALESCE(o.country_name,''),o.service_code,COALESCE(o.service_name,''),o.quality_tier"
	if !strings.Contains(orderCols, readableColumns) {
		t.Fatalf("订单读取列未完整包含名称快照: %s", orderCols)
	}
	scanned, err := scanOrder(orderNameSnapshotRow{})
	if err != nil {
		t.Fatal(err)
	}
	if scanned.CountryName != "目录国家名称" || scanned.ServiceName != "MOMO" || scanned.QualityTier != "gold" {
		t.Fatalf("订单扫描结果错误: %+v", scanned)
	}

	insert := strings.ToLower(insertOrderSQL)
	for _, fragment := range []string{
		"country_code,country_name,service_code,service_name,quality_tier",
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

func TestDashboardTodayCostExcludesCanceledOrders(t *testing.T) {
	query := strings.Join(strings.Fields(strings.ToLower(dashboardSQL)), " ")
	if !strings.Contains(query, "sum(cost) filter ( where created_at >= date_trunc('day', now()) and status <> 'canceled' )") {
		t.Fatalf("今日支出 SQL 未排除取消号码: %s", query)
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
