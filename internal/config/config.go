package config

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"buysms/internal/identity"
)

type Config struct {
	Environment       string
	Address           string
	DatabaseURL       string
	PublicBaseURL     string
	SessionPepper     []byte
	EncryptionKey     []byte
	AdminPath         string
	AdminUsername     string
	AdminPassword     string
	TrustedProxies    []string
	HeroSMSBaseURL    string
	SMSBowerBaseURL   string
	SMSPoolBaseURL    string
	SessionTTL        time.Duration
	CaptchaTTL        time.Duration
	PollInterval      time.Duration
	ReconcileInterval time.Duration
}

func Load() (Config, error) {
	c := Config{
		Environment:       env("APP_ENV", "development"),
		Address:           env("APP_ADDR", ":8080"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		PublicBaseURL:     strings.TrimRight(env("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		AdminUsername:     env("ADMIN_USERNAME", "admin"),
		AdminPassword:     os.Getenv("ADMIN_PASSWORD"),
		HeroSMSBaseURL:    env("HERO_SMS_BASE_URL", "https://hero-sms.com/api/v1"),
		SMSBowerBaseURL:   env("SMSBOWER_BASE_URL", "https://smsbower.page/stubs/handler_api.php"),
		SMSPoolBaseURL:    env("SMSPOOL_BASE_URL", "https://api.smspool.net"),
		SessionTTL:        12 * time.Hour,
		CaptchaTTL:        5 * time.Minute,
		PollInterval:      10 * time.Second,
		ReconcileInterval: 75 * time.Second,
	}
	path := strings.TrimSpace(os.Getenv("ADMIN_ENTRY_PATH"))
	if path == "" && c.Environment == "production" {
		return c, errors.New("生产环境必须显式配置 ADMIN_ENTRY_PATH")
	}
	if path == "" {
		path = "/admin-" + strings.ToLower(identity.Token(12))
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	c.AdminPath = strings.TrimRight(path, "/")
	if strings.Contains(strings.TrimPrefix(c.AdminPath, "/"), "/") {
		return c, errors.New("ADMIN_ENTRY_PATH 仅允许单个路径段")
	}
	if v := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.TrustedProxies = append(c.TrustedProxies, p)
			}
		}
	}
	c.SessionPepper = derive(os.Getenv("JWT_SECRET"), "development-session-pepper")
	keyRaw := os.Getenv("DATA_ENCRYPTION_KEY")
	if decoded, err := base64.StdEncoding.DecodeString(keyRaw); err == nil && len(decoded) == 32 {
		c.EncryptionKey = decoded
	} else {
		c.EncryptionKey = derive(keyRaw, "development-data-key")
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL 未配置")
	}
	if c.Environment == "production" {
		jwt, dataKey := os.Getenv("JWT_SECRET"), os.Getenv("DATA_ENCRYPTION_KEY")
		if len(jwt) < 32 || placeholder(jwt) {
			return c, errors.New("生产环境 JWT_SECRET 至少需要 32 个字符且不可使用示例占位值")
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(dataKey)
		if err != nil || len(decoded) != 32 || placeholder(dataKey) {
			return c, errors.New("生产环境 DATA_ENCRYPTION_KEY 必须是严格 Base64 编码的 32 字节密钥")
		}
		c.EncryptionKey = decoded
		if len(c.AdminPassword) < 12 || placeholder(c.AdminPassword) {
			return c, errors.New("生产环境 ADMIN_PASSWORD 至少需要 12 个字符且不可使用示例占位值")
		}
		if len(strings.TrimPrefix(c.AdminPath, "/")) < 20 || c.AdminPath == "/admin" || placeholder(c.AdminPath) {
			return c, errors.New("生产环境 ADMIN_ENTRY_PATH 必须是至少 20 字符的随机路径")
		}
		u, err := url.Parse(c.PublicBaseURL)
		if err != nil || u.Host == "" || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || placeholder(c.PublicBaseURL) {
			return c, fmt.Errorf("生产环境 PUBLIC_BASE_URL 必须是有效的 https 绝对地址")
		}
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func derive(v, fallback string) []byte {
	if v == "" {
		v = fallback
	}
	h := sha256.Sum256([]byte(v))
	return h[:]
}
func placeholder(v string) bool {
	v = strings.ToLower(v)
	return strings.Contains(v, "replace_with_") || strings.Contains(v, "change_me") || strings.Contains(v, "changeme")
}
