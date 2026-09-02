package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"buysms/internal/domain"
	"buysms/internal/identity"
	"buysms/internal/store"
)

type authRepo struct {
	store.Repository
	mu          sync.Mutex
	captchaID   string
	captchaHash []byte
	expires     time.Time
	used        bool
	user        domain.User
	session     domain.Session
	attempts    int
	nextAttempt int64
	reserved    map[int64]struct{}
}

func (r *authRepo) PutCaptcha(_ context.Context, id string, h []byte, exp time.Time) error {
	r.captchaID = id
	r.captchaHash = append([]byte(nil), h...)
	r.expires = exp
	return nil
}
func (r *authRepo) ConsumeCaptcha(_ context.Context, id string, h []byte, now time.Time) (bool, error) {
	if r.used || id != r.captchaID || now.After(r.expires) || !equal(h, r.captchaHash) {
		return false, nil
	}
	r.used = true
	return true, nil
}
func (r *authRepo) LoginAllowed(context.Context, string, time.Time, time.Duration, int) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts < 8, nil
}
func (r *authRepo) RecordLoginAttempt(_ context.Context, _ string, _ string, success bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !success {
		r.attempts++
	}
	return nil
}
func (r *authRepo) ReserveLoginAttempt(_ context.Context, _ string, _ string, _ time.Time, _ time.Duration, max int) (int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attempts >= max {
		return 0, false, nil
	}
	r.attempts++
	r.nextAttempt++
	if r.reserved == nil {
		r.reserved = make(map[int64]struct{})
	}
	r.reserved[r.nextAttempt] = struct{}{}
	return r.nextAttempt, true, nil
}
func (r *authRepo) CompleteLoginAttempt(_ context.Context, attemptID int64, success bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.reserved[attemptID]; !exists {
		return nil
	}
	delete(r.reserved, attemptID)
	if success {
		r.attempts--
	}
	return nil
}
func (r *authRepo) FindUserByUsername(_ context.Context, name string) (domain.User, error) {
	if strings.EqualFold(name, r.user.Username) {
		return r.user, nil
	}
	return domain.User{}, store.ErrNotFound
}
func (r *authRepo) CreateSession(_ context.Context, x domain.Session) error {
	r.session = x
	return nil
}
func (r *authRepo) FindSession(_ context.Context, h []byte, now time.Time) (domain.User, error) {
	if equal(h, r.session.TokenHash) && r.session.ExpiresAt.After(now) {
		return r.user, nil
	}
	return domain.User{}, store.ErrNotFound
}
func (r *authRepo) TouchLastLogin(context.Context, string) error { return nil }
func (r *authRepo) Audit(context.Context, *string, string, string, string, string, json.RawMessage) error {
	return nil
}

func TestCaptchaCaseInsensitiveAndSingleUse(t *testing.T) {
	r := &authRepo{}
	s := New(r, []byte("test pepper"), "/entry-abcdefghijklmnop", time.Minute, time.Hour)
	s.captchaCode = func() (string, error) { return "A2B3C", nil }
	hash, err := s.HashPassword("strong-password")
	if err != nil {
		t.Fatal(err)
	}
	r.user = domain.User{ID: identity.UUID(), Username: "Admin", PasswordHash: hash, Role: "admin", Active: true}
	c, err := s.Captcha(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	code := "A2B3C"
	if !strings.HasPrefix(c.Image, "data:image/png;base64,") {
		t.Fatalf("验证码应为 PNG data URI: %s", c.Image[:min(len(c.Image), 40)])
	}
	res, err := s.Login(context.Background(), LoginInput{Username: "admin", Password: "strong-password", CaptchaID: c.ID, Captcha: strings.ToLower(code), AdminPath: "/entry-abcdefghijklmnop", IP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Token == "" || res.User.ID != r.user.ID {
		t.Fatalf("登录结果异常: %+v", res)
	}
	if _, err = s.Login(context.Background(), LoginInput{Username: "admin", Password: "strong-password", CaptchaID: c.ID, Captcha: code, AdminPath: "/entry-abcdefghijklmnop", IP: "127.0.0.1"}); err != ErrCredentials {
		t.Fatalf("验证码应只能使用一次，得到 %v", err)
	}
	if _, err = s.Authenticate(context.Background(), res.Token); err != nil {
		t.Fatalf("会话鉴权失败: %v", err)
	}
}

func TestLoginRejectsWrongEntryAndRateLimits(t *testing.T) {
	r := &authRepo{}
	s := New(r, []byte("test pepper"), "/secret", time.Minute, time.Hour)
	s.captchaCode = func() (string, error) { return "Z9Y8X", nil }
	for i := 0; i < 8; i++ {
		c, _ := s.Captcha(context.Background())
		code := "Z9Y8X"
		_, _ = s.Login(context.Background(), LoginInput{CaptchaID: c.ID, Captcha: code, AdminPath: "/wrong", IP: "127.0.0.1"})
	}
	c, _ := s.Captcha(context.Background())
	code := "Z9Y8X"
	if _, err := s.Login(context.Background(), LoginInput{CaptchaID: c.ID, Captcha: code, AdminPath: "/secret", IP: "127.0.0.1"}); err != ErrRateLimited {
		t.Fatalf("期望限速错误，得到 %v", err)
	}
}

func TestConcurrentLoginAttemptsRespectAtomicFailureLimit(t *testing.T) {
	const (
		parallel = 64
		maximum  = 8
	)
	repo := &authRepo{}
	service := New(repo, []byte("atomic-login-attempt-pepper"), "/secret-entry", time.Minute, time.Hour)
	start := make(chan struct{})
	results := make(chan error, parallel)
	var workers sync.WaitGroup
	workers.Add(parallel)
	for index := 0; index < parallel; index++ {
		go func() {
			defer workers.Done()
			<-start
			_, err := service.Login(context.Background(), LoginInput{
				Username: "admin", AdminPath: "/wrong-entry", IP: "198.51.100.42",
			})
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	credentials, limited := 0, 0
	for err := range results {
		switch {
		case errors.Is(err, ErrCredentials):
			credentials++
		case errors.Is(err, ErrRateLimited):
			limited++
		default:
			t.Fatalf("并发登录返回意外错误: %v", err)
		}
	}
	if credentials != maximum {
		t.Fatalf("并发失败请求获准数=%d，期望最多且恰好=%d", credentials, maximum)
	}
	if limited != parallel-maximum {
		t.Fatalf("并发限速请求数=%d，期望=%d", limited, parallel-maximum)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.attempts != maximum {
		t.Fatalf("原子仓储失败记录=%d，期望=%d", repo.attempts, maximum)
	}
	if repo.nextAttempt != maximum || len(repo.reserved) != maximum {
		t.Fatalf("原子预留记录异常: next=%d reserved=%d", repo.nextAttempt, len(repo.reserved))
	}
	if repo.session.ID != "" {
		t.Fatalf("失败登录不应创建会话: %+v", repo.session)
	}
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
