package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"buysms/internal/domain"
	"buysms/internal/identity"
	"buysms/internal/store"
)

var (
	ErrTwoFactorInvalid      = errors.New("2FA 验证码错误或已使用，请使用当前或下一个验证码")
	ErrTwoFactorExpired      = errors.New("二次验证已过期，请重新登录")
	ErrTwoFactorSetupInvalid = errors.New("2FA 绑定信息无效，请重新生成")
	ErrTwoFactorSetupExpired = errors.New("2FA 绑定信息已过期，请重新生成")
)

const twoFactorIssuer = "Buy SMS"

type TwoFactorDetails struct {
	Secret     string `json:"secret"`
	OTPAuthURL string `json:"otpauthUrl"`
}

type TwoFactorSetup struct {
	TwoFactorDetails
	SetupToken string    `json:"setupToken"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type twoFactorSetupClaims struct {
	Purpose     string    `json:"purpose"`
	ActorID     string    `json:"actorId"`
	UserID      string    `json:"userId"`
	Username    string    `json:"username"`
	AuthVersion int64     `json:"authVersion"`
	Secret      string    `json:"secret"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type TwoFactorLoginInput struct {
	ChallengeToken string `json:"challengeToken"`
	Code           string `json:"code"`
	AdminPath      string `json:"adminPath"`
	IP             string `json:"-"`
	UserAgent      string `json:"-"`
}

func (s *Service) NewTwoFactorSetup(actorID, userID, username string, authVersion int64) (TwoFactorSetup, error) {
	if s.vault == nil {
		return TwoFactorSetup{}, errors.New("2FA 加密服务未配置")
	}
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return TwoFactorSetup{}, err
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	exp := s.now().Add(10 * time.Minute)
	claims := twoFactorSetupClaims{Purpose: "two-factor-setup-v1", ActorID: actorID, UserID: userID, Username: strings.ToLower(strings.TrimSpace(username)), AuthVersion: authVersion, Secret: secret, ExpiresAt: exp}
	payload, err := json.Marshal(claims)
	if err != nil {
		return TwoFactorSetup{}, err
	}
	cipher, err := s.vault.Encrypt(string(payload))
	if err != nil {
		return TwoFactorSetup{}, err
	}
	return TwoFactorSetup{TwoFactorDetails: twoFactorDetails(username, secret), SetupToken: base64.RawURLEncoding.EncodeToString(cipher), ExpiresAt: exp}, nil
}

// VerifyTwoFactorSetup only returns encrypted key material after proving possession.
func (s *Service) VerifyTwoFactorSetup(actorID, userID, username string, authVersion int64, token, code string) ([]byte, int64, error) {
	if s.vault == nil {
		return nil, 0, errors.New("2FA 加密服务未配置")
	}
	if len(token) == 0 || len(token) > 4096 {
		return nil, 0, ErrTwoFactorSetupInvalid
	}
	cipher, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, 0, ErrTwoFactorSetupInvalid
	}
	payload, err := s.vault.Decrypt(cipher)
	if err != nil {
		return nil, 0, ErrTwoFactorSetupInvalid
	}
	var claims twoFactorSetupClaims
	if json.Unmarshal([]byte(payload), &claims) != nil || claims.Purpose != "two-factor-setup-v1" || claims.ActorID != actorID || claims.UserID != userID || claims.Username != strings.ToLower(strings.TrimSpace(username)) || claims.AuthVersion != authVersion {
		return nil, 0, ErrTwoFactorSetupInvalid
	}
	if !claims.ExpiresAt.After(s.now()) {
		return nil, 0, ErrTwoFactorSetupExpired
	}
	step, ok := matchTOTP(claims.Secret, code, s.now())
	if !ok {
		return nil, 0, ErrTwoFactorInvalid
	}
	encrypted, err := s.vault.Encrypt(claims.Secret)
	return encrypted, step, err
}

func (s *Service) TwoFactorDetails(user domain.User) (TwoFactorDetails, error) {
	if !user.TwoFactorEnabled || len(user.TwoFactorSecretCipher) == 0 {
		return TwoFactorDetails{}, ErrTwoFactorSetupInvalid
	}
	if s.vault == nil {
		return TwoFactorDetails{}, errors.New("2FA 加密服务未配置")
	}
	secret, err := s.vault.Decrypt(user.TwoFactorSecretCipher)
	if err != nil {
		return TwoFactorDetails{}, err
	}
	return twoFactorDetails(user.Username, secret), nil
}

func twoFactorDetails(username, secret string) TwoFactorDetails {
	params := url.Values{"secret": {secret}, "issuer": {twoFactorIssuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return TwoFactorDetails{Secret: secret, OTPAuthURL: "otpauth://totp/" + url.PathEscape(twoFactorIssuer+":"+strings.TrimSpace(username)) + "?" + params.Encode()}
}

func totpCode(secret string, step int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil || len(key) < 20 || step < 0 {
		return "", ErrTwoFactorSetupInvalid
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000), nil
}

func matchTOTP(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}
	for _, digit := range code {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	current := now.Unix() / 30
	for _, offset := range []int64{0, -1, 1} {
		expected, err := totpCode(secret, current+offset)
		if err == nil && hmac.Equal([]byte(code), []byte(expected)) {
			return current + offset, true
		}
	}
	return 0, false
}

func (s *Service) LoginTwoFactor(ctx context.Context, in TwoFactorLoginInput) (LoginResult, error) {
	attemptID, allowed, err := s.repo.ReserveLoginAttempt(ctx, in.IP, "two-factor", s.now(), 15*time.Minute, 8)
	if err != nil {
		return LoginResult{}, err
	}
	if !allowed {
		return LoginResult{}, ErrRateLimited
	}
	path := strings.TrimRight(in.AdminPath, "/")
	if path == "" || !hmac.Equal([]byte(path), []byte(s.adminPath)) || len(in.ChallengeToken) != 43 {
		return LoginResult{}, ErrTwoFactorExpired
	}
	hash := s.digest("two-factor:" + in.ChallengeToken)
	challenge, user, err := s.repo.ReserveTwoFactorChallenge(ctx, hash, in.IP, s.now(), 5)
	if err != nil {
		return LoginResult{}, mapTwoFactorStoreError(err)
	}
	details, err := s.TwoFactorDetails(user)
	if err != nil {
		return LoginResult{}, err
	}
	step, ok := matchTOTP(details.Secret, in.Code, s.now())
	if !ok || step <= user.TwoFactorLastStep {
		return LoginResult{}, ErrTwoFactorInvalid
	}
	token := identity.Token(32)
	exp := s.now().Add(s.sessionTTL)
	sess := domain.Session{ID: identity.UUID(), UserID: user.ID, AuthVersion: user.AuthVersion, TokenHash: s.digest("session:" + token), IP: in.IP, UserAgent: truncate(in.UserAgent, 512), ExpiresAt: exp}
	user, err = s.repo.RedeemTwoFactorChallenge(ctx, hash, in.IP, user.AuthVersion, step, sess, s.now())
	if err != nil {
		return LoginResult{}, mapTwoFactorStoreError(err)
	}
	_ = s.repo.CompleteLoginAttempt(ctx, attemptID, true)
	_ = s.repo.CompleteLoginAttempt(ctx, challenge.LoginAttemptID, true)
	_ = s.repo.Audit(ctx, &user.ID, "login", "user", user.ID, in.IP, nil)
	_ = s.repo.TouchLastLogin(ctx, user.ID)
	return LoginResult{Token: token, User: &user, ExpiresAt: exp}, nil
}

func mapTwoFactorStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrChallengeExpired), errors.Is(err, store.ErrNotFound):
		return ErrTwoFactorExpired
	case errors.Is(err, store.ErrChallengeLimited):
		return ErrTwoFactorExpired
	case errors.Is(err, store.ErrCodeUsed):
		return ErrTwoFactorInvalid
	default:
		return err
	}
}
