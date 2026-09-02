package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"time"

	"buysms/internal/domain"
	"buysms/internal/identity"
	"buysms/internal/store"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrCredentials  = errors.New("用户名、密码或验证码错误")
	ErrRateLimited  = errors.New("登录尝试过于频繁，请稍后再试")
	ErrUnauthorized = errors.New("登录状态无效或已过期")
)

type Service struct {
	repo                   store.Repository
	pepper                 []byte
	adminPath              string
	captchaTTL, sessionTTL time.Duration
	now                    func() time.Time
	captchaCode            func() (string, error)
}

type Captcha struct {
	ID        string    `json:"id"`
	Image     string    `json:"image"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type LoginInput struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	CaptchaID string `json:"captchaId"`
	Captcha   string `json:"captcha"`
	AdminPath string `json:"adminPath"`
	IP        string `json:"-"`
	UserAgent string `json:"-"`
}
type LoginResult struct {
	Token     string      `json:"token"`
	User      domain.User `json:"user"`
	ExpiresAt time.Time   `json:"expiresAt"`
}

func New(repo store.Repository, pepper []byte, path string, captchaTTL, sessionTTL time.Duration) *Service {
	return &Service{repo: repo, pepper: pepper, adminPath: strings.TrimRight(path, "/"), captchaTTL: captchaTTL, sessionTTL: sessionTTL, now: time.Now, captchaCode: randomCaptchaCode}
}

func (s *Service) Captcha(ctx context.Context) (Captcha, error) {
	code, err := s.captchaCode()
	if err != nil {
		return Captcha{}, err
	}
	id := identity.UUID()
	exp := s.now().Add(s.captchaTTL)
	if err := s.repo.PutCaptcha(ctx, id, s.digest("captcha:"+id+":"+strings.ToUpper(code)), exp); err != nil {
		return Captcha{}, err
	}
	imageBytes, err := renderCaptchaPNG(code)
	if err != nil {
		return Captcha{}, err
	}
	return Captcha{ID: id, Image: "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes), ExpiresAt: exp}, nil
}

func randomCaptchaCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, 5)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i := range raw {
		raw[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(raw), nil
}

var pixelGlyphs = map[byte][7]byte{
	'A': {14, 17, 17, 31, 17, 17, 17}, 'B': {30, 17, 17, 30, 17, 17, 30}, 'C': {14, 17, 16, 16, 16, 17, 14}, 'D': {30, 17, 17, 17, 17, 17, 30},
	'E': {31, 16, 16, 30, 16, 16, 31}, 'F': {31, 16, 16, 30, 16, 16, 16}, 'G': {14, 17, 16, 23, 17, 17, 15}, 'H': {17, 17, 17, 31, 17, 17, 17},
	'J': {7, 2, 2, 2, 2, 18, 12}, 'K': {17, 18, 20, 24, 20, 18, 17}, 'L': {16, 16, 16, 16, 16, 16, 31}, 'M': {17, 27, 21, 21, 17, 17, 17},
	'N': {17, 25, 21, 19, 17, 17, 17}, 'P': {30, 17, 17, 30, 16, 16, 16}, 'Q': {14, 17, 17, 17, 21, 18, 13}, 'R': {30, 17, 17, 30, 20, 18, 17},
	'S': {15, 16, 16, 14, 1, 1, 30}, 'T': {31, 4, 4, 4, 4, 4, 4}, 'U': {17, 17, 17, 17, 17, 17, 14}, 'V': {17, 17, 17, 17, 17, 10, 4},
	'W': {17, 17, 17, 21, 21, 21, 10}, 'X': {17, 17, 10, 4, 10, 17, 17}, 'Y': {17, 17, 10, 4, 4, 4, 4}, 'Z': {31, 1, 2, 4, 8, 16, 31},
	'2': {14, 17, 1, 2, 4, 8, 31}, '3': {30, 1, 1, 14, 1, 1, 30}, '4': {2, 6, 10, 18, 31, 2, 2}, '5': {31, 16, 16, 30, 1, 1, 30},
	'6': {14, 16, 16, 30, 17, 17, 14}, '7': {31, 1, 2, 4, 8, 8, 8}, '8': {14, 17, 17, 14, 17, 17, 14}, '9': {14, 17, 17, 15, 1, 1, 14},
}

func renderCaptchaPNG(code string) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 150, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 150; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 244, G: 246, B: 250, A: 255})
		}
	}
	noise := make([]byte, 180)
	_, _ = rand.Read(noise)
	for i := 0; i < len(noise); i += 3 {
		x := int(noise[i]) % 150
		y := int(noise[i+1]) % 48
		img.SetRGBA(x, y, color.RGBA{R: 150 + noise[i+2]%50, G: 160 + noise[i]%50, B: 180 + noise[i+1]%50, A: 255})
	}
	for index, ch := range []byte(code) {
		glyph := pixelGlyphs[ch]
		offsetX := 10 + index*27 + int(noise[index])%3
		offsetY := 8 + int(noise[index+8])%4
		ink := color.RGBA{R: 28 + noise[index+20]%30, G: 42 + noise[index+30]%30, B: 65 + noise[index+40]%35, A: 255}
		for row, bits := range glyph {
			for col := 0; col < 5; col++ {
				if bits&(1<<uint(4-col)) == 0 {
					continue
				}
				for dy := 0; dy < 4; dy++ {
					for dx := 0; dx < 4; dx++ {
						img.SetRGBA(offsetX+col*4+dx, offsetY+row*4+dy, ink)
					}
				}
			}
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (s *Service) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	attemptID, allowed, err := s.repo.ReserveLoginAttempt(ctx, in.IP, in.Username, s.now(), 15*time.Minute, 8)
	if err != nil {
		return LoginResult{}, err
	}
	if !allowed {
		return LoginResult{}, ErrRateLimited
	}
	path := strings.TrimRight(in.AdminPath, "/")
	if path == "" || !hmac.Equal([]byte(path), []byte(s.adminPath)) {
		return LoginResult{}, ErrCredentials
	}
	ok, err := s.repo.ConsumeCaptcha(ctx, in.CaptchaID, s.digest("captcha:"+in.CaptchaID+":"+strings.ToUpper(strings.TrimSpace(in.Captcha))), s.now())
	if err != nil {
		return LoginResult{}, err
	}
	if !ok {
		return LoginResult{}, ErrCredentials
	}
	u, err := s.repo.FindUserByUsername(ctx, in.Username)
	if err != nil || !u.Active || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)) != nil {
		return LoginResult{}, ErrCredentials
	}
	token := identity.Token(32)
	exp := s.now().Add(s.sessionTTL)
	sess := domain.Session{ID: identity.UUID(), UserID: u.ID, TokenHash: s.digest("session:" + token), IP: in.IP, UserAgent: truncate(in.UserAgent, 512), ExpiresAt: exp}
	if err = s.repo.CreateSession(ctx, sess); err != nil {
		return LoginResult{}, err
	}
	_ = s.repo.CompleteLoginAttempt(ctx, attemptID, true)
	_ = s.repo.Audit(ctx, &u.ID, "login", "user", u.ID, in.IP, nil)
	_ = s.repo.TouchLastLogin(ctx, u.ID)
	return LoginResult{Token: token, User: u, ExpiresAt: exp}, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (domain.User, error) {
	if token == "" {
		return domain.User{}, ErrUnauthorized
	}
	u, err := s.repo.FindSession(ctx, s.digest("session:"+token), s.now())
	if err != nil {
		return domain.User{}, ErrUnauthorized
	}
	return u, nil
}
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.repo.RevokeSession(ctx, s.digest("session:"+token))
}
func (s *Service) HashPassword(password string) (string, error) {
	if len(password) < 10 || len(password) > 256 {
		return "", errors.New("密码长度应为 10 到 256 个字符")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(h), err
}
func (s *Service) CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
func (s *Service) digest(value string) []byte {
	m := hmac.New(sha256.New, s.pepper)
	_, _ = m.Write([]byte(value))
	return m.Sum(nil)
}
func truncate(v string, n int) string {
	if len(v) > n {
		return v[:n]
	}
	return v
}
