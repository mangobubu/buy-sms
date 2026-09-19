package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buysms/internal/application"
	"buysms/internal/auth"
	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
	"github.com/gin-gonic/gin"
)

type twoFactorHTTPRepo struct {
	store.Repository
	actor  domain.User
	target domain.User
}

func (r *twoFactorHTTPRepo) FindSession(context.Context, []byte, time.Time) (domain.User, error) {
	return r.actor, nil
}
func (r *twoFactorHTTPRepo) GetUser(context.Context, string) (domain.User, error) {
	return r.target, nil
}
func (r *twoFactorHTTPRepo) ReserveLoginAttempt(context.Context, string, string, time.Time, time.Duration, int) (int64, bool, error) {
	return 1, true, nil
}
func (r *twoFactorHTTPRepo) Audit(context.Context, *string, string, string, string, string, json.RawMessage) error {
	return nil
}

func TestTwoFactorRoutesEnforceAdminAndNoStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	vault, _ := secure.NewVault([]byte("HTTP two factor key"))
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	encrypted, _ := vault.Encrypt(secret)
	repo := &twoFactorHTTPRepo{target: domain.User{ID: "target", Username: "member", TwoFactorEnabled: true, TwoFactorSecretCipher: encrypted}}
	cfg := config.Config{AdminPath: "/entry", SessionPepper: []byte("test pepper"), SessionTTL: time.Hour}
	authentication := auth.New(repo, cfg.SessionPepper, cfg.AdminPath, time.Minute, time.Hour)
	app := application.New(repo, authentication, vault, cfg)
	router := New(app, authentication, cfg)
	for _, role := range []string{"anonymous", "operator", "admin"} {
		for _, route := range []struct{ method, path, body string }{
			{"POST", "/api/users/two-factor/setup", `{"username":"new-user"}`},
			{"GET", "/api/users/target/two-factor", ""},
		} {
			t.Run(role+route.path, func(t *testing.T) {
				repo.actor = domain.User{ID: "actor", Role: role, Active: true}
				request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
				if role != "anonymous" {
					request.Header.Set("Authorization", "Bearer test")
				}
				result := httptest.NewRecorder()
				router.ServeHTTP(result, request)
				want := http.StatusOK
				if role == "anonymous" {
					want = http.StatusUnauthorized
				}
				if role == "operator" {
					want = http.StatusForbidden
				}
				if result.Code != want {
					t.Fatalf("status=%d body=%s", result.Code, result.Body.String())
				}
				if !strings.Contains(result.Header().Get("Cache-Control"), "no-store") {
					t.Fatal("sensitive response can be cached")
				}
				var value map[string]any
				_ = json.Unmarshal(result.Body.Bytes(), &value)
				if role == "admin" {
					if value["secret"] == nil || value["otpauthUrl"] == nil {
						t.Fatalf("missing binding details: %v", value)
					}
					if route.method == "POST" && (value["setupToken"] == nil || value["expiresAt"] == nil) {
						t.Fatal("missing setup token or expiry")
					}
				} else if value["secret"] != nil {
					t.Fatal("secret disclosed without admin role")
				}
			})
		}
	}
	request := httptest.NewRequest("POST", "/api/public/login/two-factor", strings.NewReader(`{"challengeToken":"bad","code":"123456","adminPath":"/entry"}`))
	result := httptest.NewRecorder()
	router.ServeHTTP(result, request)
	if result.Code != http.StatusUnauthorized || !strings.Contains(result.Body.String(), `"two_factor_expired"`) || !strings.Contains(result.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("invalid challenge: %d %s", result.Code, result.Body.String())
	}
}

func TestTwoFactorErrorContracts(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{auth.ErrTwoFactorInvalid, 400, "two_factor_invalid"},
		{auth.ErrTwoFactorExpired, 401, "two_factor_expired"},
		{auth.ErrTwoFactorSetupInvalid, 400, "two_factor_setup_invalid"},
		{auth.ErrTwoFactorSetupExpired, 400, "two_factor_setup_expired"},
		{auth.ErrRateLimited, 429, "rate_limited"},
	} {
		t.Run(test.code, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			respondError(ctx, test.err)
			var value map[string]string
			_ = json.Unmarshal(recorder.Body.Bytes(), &value)
			if recorder.Code != test.status || value["code"] != test.code || value["message"] == "" {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
