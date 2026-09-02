package config

import "testing"

func TestProductionRequiresExplicitAdminPath(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("JWT_SECRET", "01234567890123456789012345678901")
	t.Setenv("DATA_ENCRYPTION_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	t.Setenv("ADMIN_PASSWORD", "long-admin-password")
	t.Setenv("PUBLIC_BASE_URL", "https://sms.example.com")
	t.Setenv("ADMIN_ENTRY_PATH", "")
	if _, err := Load(); err == nil {
		t.Fatal("生产环境缺失 ADMIN_ENTRY_PATH 应报错")
	}
}

func TestAdminPathMustBeSingleSegment(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("ADMIN_ENTRY_PATH", "/one/two")
	if _, err := Load(); err == nil {
		t.Fatal("多段后台入口应报错")
	}
}
