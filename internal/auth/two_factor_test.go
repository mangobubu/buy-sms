package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"buysms/internal/domain"
	"buysms/internal/identity"
	"buysms/internal/secure"
	"buysms/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestTOTPRFC6238AndWindow(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for _, test := range []struct {
		seconds int64
		code    string
	}{
		{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"},
	} {
		code, err := totpCode(secret, test.seconds/30)
		if err != nil || code != test.code {
			t.Fatalf("at %d: %s, %v", test.seconds, code, err)
		}
	}
	now := time.Unix(1234567890, 0)
	for _, offset := range []int64{-1, 0, 1} {
		code, _ := totpCode(secret, now.Unix()/30+offset)
		if step, ok := matchTOTP(secret, code, now); !ok || step != now.Unix()/30+offset {
			t.Fatalf("offset %d rejected", offset)
		}
	}
	code, _ := totpCode(secret, now.Unix()/30-2)
	for _, invalid := range []string{code, "", "00000", "0000000", "12a456", "１２３４５６"} {
		if _, ok := matchTOTP(secret, invalid, now); ok {
			t.Fatalf("accepted invalid code %q", invalid)
		}
	}
}

func TestTwoFactorSetupBindingIntegrityAndExpiry(t *testing.T) {
	vault, _ := secure.NewVault([]byte("test encryption key"))
	service := New(nil, []byte("pepper"), "/entry", time.Minute, time.Hour, vault)
	now := time.Unix(1800000000, 0)
	service.now = func() time.Time { return now }
	setup, err := service.NewTwoFactorSetup("actor", "target", "Name + @", 7)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(setup.OTPAuthURL)
	if err != nil || uri.Scheme != "otpauth" || uri.Query().Get("secret") != setup.Secret || uri.Query().Get("digits") != "6" || uri.Query().Get("period") != "30" {
		t.Fatalf("bad OTP URI: %q", setup.OTPAuthURL)
	}
	code, _ := totpCode(setup.Secret, now.Unix()/30)
	cipher, step, err := service.VerifyTwoFactorSetup("actor", "target", "name + @", 7, setup.SetupToken, code)
	if err != nil || step != now.Unix()/30 {
		t.Fatalf("verify: %v", err)
	}
	secret, err := vault.Decrypt(cipher)
	if err != nil || secret != setup.Secret || strings.Contains(string(cipher), secret) {
		t.Fatal("secret was not encrypted")
	}
	for _, test := range []struct {
		name, actor, target, username, token string
		version                              int64
		expected                             error
	}{
		{"wrong actor", "other", "target", "name + @", setup.SetupToken, 7, ErrTwoFactorSetupInvalid},
		{"wrong target", "actor", "other", "name + @", setup.SetupToken, 7, ErrTwoFactorSetupInvalid},
		{"wrong username", "actor", "target", "other", setup.SetupToken, 7, ErrTwoFactorSetupInvalid},
		{"wrong version", "actor", "target", "name + @", setup.SetupToken, 8, ErrTwoFactorSetupInvalid},
		{"tampered", "actor", "target", "name + @", "A" + setup.SetupToken[1:len(setup.SetupToken)-1], 7, ErrTwoFactorSetupInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := service.VerifyTwoFactorSetup(test.actor, test.target, test.username, test.version, test.token, code)
			if !errors.Is(err, test.expected) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if _, _, err := service.VerifyTwoFactorSetup("actor", "target", "name + @", 7, setup.SetupToken, "abcdef"); !errors.Is(err, ErrTwoFactorInvalid) {
		t.Fatalf("wrong code: %v", err)
	}
	now = setup.ExpiresAt
	if _, _, err := service.VerifyTwoFactorSetup("actor", "target", "name + @", 7, setup.SetupToken, code); !errors.Is(err, ErrTwoFactorSetupExpired) {
		t.Fatalf("expired: %v", err)
	}
}

type twoFactorAuthRepo struct {
	*authRepo
	challengeMu sync.Mutex
	challenges  map[string]domain.TwoFactorChallenge
	consumed    map[string]bool
	sessions    int
}

func (r *twoFactorAuthRepo) CreateTwoFactorChallenge(_ context.Context, value domain.TwoFactorChallenge) error {
	r.challengeMu.Lock()
	defer r.challengeMu.Unlock()
	r.challenges[string(value.TokenHash)] = value
	return nil
}
func (r *twoFactorAuthRepo) ReserveTwoFactorChallenge(_ context.Context, hash []byte, ip string, now time.Time, max int) (domain.TwoFactorChallenge, domain.User, error) {
	r.challengeMu.Lock()
	defer r.challengeMu.Unlock()
	value, ok := r.challenges[string(hash)]
	if !ok || r.consumed[string(hash)] || value.IP != ip || !value.ExpiresAt.After(now) || value.AuthVersion != r.user.AuthVersion || !r.user.Active || !r.user.TwoFactorEnabled {
		return value, domain.User{}, store.ErrChallengeExpired
	}
	if value.Attempts >= max {
		return value, domain.User{}, store.ErrChallengeLimited
	}
	value.Attempts++
	r.challenges[string(hash)] = value
	return value, r.user, nil
}
func (r *twoFactorAuthRepo) RedeemTwoFactorChallenge(_ context.Context, hash []byte, ip string, version, step int64, session domain.Session, now time.Time) (domain.User, error) {
	r.challengeMu.Lock()
	defer r.challengeMu.Unlock()
	value, ok := r.challenges[string(hash)]
	if !ok || r.consumed[string(hash)] || value.IP != ip || !value.ExpiresAt.After(now) || version != r.user.AuthVersion || !r.user.Active || !r.user.TwoFactorEnabled {
		return domain.User{}, store.ErrChallengeExpired
	}
	if step <= r.user.TwoFactorLastStep {
		return domain.User{}, store.ErrCodeUsed
	}
	r.user.TwoFactorLastStep = step
	r.consumed[string(hash)] = true
	r.session = session
	r.sessions++
	return r.user, nil
}

func newTwoFactorAuthTest(t *testing.T) (*Service, *twoFactorAuthRepo, *time.Time, string) {
	t.Helper()
	vault, _ := secure.NewVault([]byte("test encryption key"))
	hash, _ := bcrypt.GenerateFromPassword([]byte("strong-password"), bcrypt.MinCost)
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	cipher, _ := vault.Encrypt(secret)
	repo := &twoFactorAuthRepo{authRepo: &authRepo{user: domain.User{ID: identity.UUID(), Username: "admin", PasswordHash: string(hash), Role: "admin", Active: true, TwoFactorEnabled: true, TwoFactorSecretCipher: cipher, TwoFactorLastStep: -1, AuthVersion: 3}}, challenges: map[string]domain.TwoFactorChallenge{}, consumed: map[string]bool{}}
	service := New(repo, []byte("test pepper"), "/entry", time.Minute, time.Hour, vault)
	now := time.Unix(1800000000, 0)
	service.now = func() time.Time { return now }
	service.captchaCode = func() (string, error) { return "A2B3C", nil }
	return service, repo, &now, secret
}

func startTwoFactorLogin(t *testing.T, service *Service, repo *twoFactorAuthRepo) LoginResult {
	t.Helper()
	repo.used = false
	captcha, err := service.Captcha(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Login(context.Background(), LoginInput{Username: "admin", Password: "strong-password", CaptchaID: captcha.ID, Captcha: "A2B3C", AdminPath: "/entry", IP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.TwoFactorRequired || result.ChallengeToken == "" || result.Token != "" || result.User != nil {
		t.Fatalf("password stage leaked session: %+v", result)
	}
	return result
}

func TestTwoFactorLoginChallengeAndRestart(t *testing.T) {
	service, repo, now, secret := newTwoFactorAuthTest(t)
	challenge := startTwoFactorLogin(t, service, repo)
	if repo.session.ID != "" {
		t.Fatal("session created before OTP")
	}
	payload, _ := json.Marshal(challenge)
	if strings.Contains(string(payload), `"token"`) || strings.Contains(string(payload), `"user"`) {
		t.Fatalf("challenge leaked user/session: %s", payload)
	}
	input := TwoFactorLoginInput{ChallengeToken: challenge.ChallengeToken, Code: "abcdef", AdminPath: "/entry", IP: "127.0.0.1"}
	if _, err := service.LoginTwoFactor(context.Background(), input); !errors.Is(err, ErrTwoFactorInvalid) {
		t.Fatalf("wrong code: %v", err)
	}
	input.Code, _ = totpCode(secret, now.Unix()/30)
	restarted := New(repo, service.pepper, service.adminPath, time.Minute, time.Hour, service.vault)
	restarted.now = service.now
	result, err := restarted.LoginTwoFactor(context.Background(), input)
	if err != nil || result.Token == "" || result.User == nil || repo.sessions != 1 {
		t.Fatalf("valid OTP: %+v / %v", result, err)
	}
	if _, err = restarted.LoginTwoFactor(context.Background(), input); !errors.Is(err, ErrTwoFactorExpired) {
		t.Fatalf("reused challenge: %v", err)
	}
	next := startTwoFactorLogin(t, service, repo)
	input.ChallengeToken = next.ChallengeToken
	if _, err = service.LoginTwoFactor(context.Background(), input); !errors.Is(err, ErrTwoFactorInvalid) {
		t.Fatalf("reused time step: %v", err)
	}
	*now = now.Add(30 * time.Second)
	input.Code, _ = totpCode(secret, now.Unix()/30)
	if _, err = service.LoginTwoFactor(context.Background(), input); err != nil || repo.sessions != 2 {
		t.Fatalf("next time step: %v", err)
	}
}

func TestTwoFactorLoginRejectsExpiredAndChangedAccounts(t *testing.T) {
	for _, scenario := range []string{"expired", "ip", "entry", "password change", "disabled", "2fa disabled"} {
		t.Run(scenario, func(t *testing.T) {
			service, repo, now, secret := newTwoFactorAuthTest(t)
			challenge := startTwoFactorLogin(t, service, repo)
			input := TwoFactorLoginInput{ChallengeToken: challenge.ChallengeToken, AdminPath: "/entry", IP: "127.0.0.1"}
			input.Code, _ = totpCode(secret, now.Unix()/30)
			switch scenario {
			case "expired":
				*now = challenge.ExpiresAt
			case "ip":
				input.IP = "127.0.0.2"
			case "entry":
				input.AdminPath = "/wrong"
			case "password change":
				repo.user.AuthVersion++
			case "disabled":
				repo.user.Active = false
			case "2fa disabled":
				repo.user.TwoFactorEnabled = false
			}
			if _, err := service.LoginTwoFactor(context.Background(), input); !errors.Is(err, ErrTwoFactorExpired) {
				t.Fatalf("got %v", err)
			}
			if repo.sessions != 0 {
				t.Fatal("rejected verification created session")
			}
		})
	}
}

func TestTwoFactorLoginLimitsAttempts(t *testing.T) {
	service, repo, _, _ := newTwoFactorAuthTest(t)
	challenge := startTwoFactorLogin(t, service, repo)
	input := TwoFactorLoginInput{ChallengeToken: challenge.ChallengeToken, Code: "abcdef", AdminPath: "/entry", IP: "127.0.0.1"}
	for i := 0; i < 5; i++ {
		if _, err := service.LoginTwoFactor(context.Background(), input); !errors.Is(err, ErrTwoFactorInvalid) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := service.LoginTwoFactor(context.Background(), input); !errors.Is(err, ErrTwoFactorExpired) {
		t.Fatalf("limit: %v", err)
	}
	if repo.sessions != 0 {
		t.Fatal("limited verification created session")
	}
}

func TestUserJSONDoesNotExposeTwoFactorKey(t *testing.T) {
	service, repo, _, _ := newTwoFactorAuthTest(t)
	value, err := service.TwoFactorDetails(repo.user)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(repo.user)
	var fields map[string]any
	_ = json.Unmarshal(payload, &fields)
	if fields["twoFactorEnabled"] != true {
		t.Fatal("missing enabled state")
	}
	for _, field := range []string{"PasswordHash", "passwordHash", "TwoFactorSecretCipher", "twoFactorSecretCipher", "TwoFactorLastStep", "AuthVersion", "secret"} {
		if _, ok := fields[field]; ok {
			t.Fatalf("exposed %s", field)
		}
	}
	if strings.Contains(string(payload), value.Secret) {
		t.Fatal("exposed secret")
	}
}

type staleTwoFactorRepo struct{ *twoFactorAuthRepo }

func (r *staleTwoFactorRepo) CreateTwoFactorChallenge(context.Context, domain.TwoFactorChallenge) error {
	return store.ErrChallengeExpired
}

func TestLoginRejectsCredentialsChangedBeforeChallengeIssuance(t *testing.T) {
	service, repo, _, _ := newTwoFactorAuthTest(t)
	service.repo = &staleTwoFactorRepo{repo}
	captcha, err := service.Captcha(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Login(context.Background(), LoginInput{Username: "admin", Password: "strong-password", CaptchaID: captcha.ID, Captcha: "A2B3C", AdminPath: "/entry", IP: "127.0.0.1"})
	if !errors.Is(err, ErrCredentials) || repo.session.ID != "" {
		t.Fatalf("stale credentials: %v", err)
	}
}
