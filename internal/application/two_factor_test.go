package application

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"buysms/internal/auth"
	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type twoFactorUserRepository struct {
	store.Repository
	users   map[string]domain.User
	writes  int
	revokes int
}

func (r *twoFactorUserRepository) CreateUser(_ context.Context, user domain.User) error {
	r.users[user.ID] = user
	r.writes++
	return nil
}
func (r *twoFactorUserRepository) GetUser(_ context.Context, id string) (domain.User, error) {
	user, ok := r.users[id]
	if !ok {
		return domain.User{}, store.ErrNotFound
	}
	return user, nil
}
func (r *twoFactorUserRepository) UpdateUser(_ context.Context, user domain.User) error {
	r.users[user.ID] = user
	r.writes++
	return nil
}
func (r *twoFactorUserRepository) RevokeUserSessions(context.Context, string) error {
	r.revokes++
	return nil
}
func (r *twoFactorUserRepository) Audit(context.Context, *string, string, string, string, string, json.RawMessage) error {
	return nil
}

func twoFactorUserService(t *testing.T) (*Service, *twoFactorUserRepository, *secure.Vault, domain.User) {
	t.Helper()
	repo := &twoFactorUserRepository{users: map[string]domain.User{}}
	vault, _ := secure.NewVault([]byte("user two factor test key"))
	authentication := auth.New(repo, []byte("user test pepper"), "/entry", time.Minute, time.Hour)
	service := New(repo, authentication, vault, config.Config{})
	return service, repo, vault, domain.User{ID: "admin", Username: "admin", Role: "admin", Active: true}
}
func bindingCode(secret string) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(time.Now().Unix()/30))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff)%1000000)
}
func boolPtr(value bool) *bool { return &value }

func TestCreateUserTwoFactorRequiresVerifiedSetup(t *testing.T) {
	service, repo, vault, actor := twoFactorUserService(t)
	setup, err := service.TwoFactorSetup(context.Background(), TwoFactorSetupInput{Username: "new-user"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	input := SaveUserInput{Username: "new-user", Role: "operator", Enabled: true, Password: "long enough password", TwoFactorEnabled: boolPtr(true), TwoFactorSetupToken: setup.SetupToken, TwoFactorCode: "abcdef"}
	if _, err = service.CreateUser(context.Background(), input, actor, ""); !errors.Is(err, auth.ErrTwoFactorInvalid) || repo.writes != 0 {
		t.Fatalf("invalid proof changed user: %v writes=%d", err, repo.writes)
	}
	input.TwoFactorCode = bindingCode(setup.Secret)
	created, err := service.CreateUser(context.Background(), input, actor, "")
	if err != nil || !created.TwoFactorEnabled {
		t.Fatalf("create: %+v %v", created, err)
	}
	stored := repo.users[created.ID]
	secret, err := vault.Decrypt(stored.TwoFactorSecretCipher)
	if err != nil || secret != setup.Secret || stored.TwoFactorLastStep < 1 {
		t.Fatalf("missing encrypted enrollment: %v", err)
	}
	serialized, _ := json.Marshal(created)
	if strings.Contains(string(serialized), secret) || strings.Contains(string(serialized), "secret") {
		t.Fatalf("DTO leaked key: %s", serialized)
	}
}

func TestUserTwoFactorEnablePreserveAndDisable(t *testing.T) {
	service, repo, vault, actor := twoFactorUserService(t)
	repo.users["user"] = domain.User{ID: "user", Username: "member", DisplayName: "Member", PasswordHash: "oldhash", Role: "operator", Active: true, AuthVersion: 9}
	setup, err := service.TwoFactorSetup(context.Background(), TwoFactorSetupInput{Username: "member", UserID: "user"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	input := SaveUserInput{Username: "member", DisplayName: "Member", Role: "operator", Enabled: true, TwoFactorEnabled: boolPtr(true), TwoFactorSetupToken: setup.SetupToken, TwoFactorCode: bindingCode(setup.Secret)}
	enabled, err := service.UpdateUser(context.Background(), "user", input, actor, "")
	if err != nil || !enabled.TwoFactorEnabled {
		t.Fatalf("enable: %v", err)
	}
	encrypted := append([]byte(nil), repo.users["user"].TwoFactorSecretCipher...)
	details, err := service.UserTwoFactorDetails(context.Background(), "user", actor, "")
	if err != nil || details.Secret != setup.Secret {
		t.Fatalf("read: %v", err)
	}
	input.TwoFactorEnabled = nil
	input.TwoFactorSetupToken = ""
	input.TwoFactorCode = ""
	input.DisplayName = "renamed display"
	if _, err = service.UpdateUser(context.Background(), "user", input, actor, ""); err != nil {
		t.Fatal(err)
	}
	unchanged := repo.users["user"]
	if !unchanged.TwoFactorEnabled || string(unchanged.TwoFactorSecretCipher) != string(encrypted) {
		t.Fatal("ordinary edit changed key")
	}
	input.TwoFactorEnabled = boolPtr(true)
	if _, err = service.UpdateUser(context.Background(), "user", input, actor, ""); err != nil {
		t.Fatalf("enabled edit requested rebinding: %v", err)
	}
	if _, err = service.UserTwoFactorDetails(context.Background(), "user", domain.User{Role: "operator"}, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator read key: %v", err)
	}
	input.TwoFactorEnabled = boolPtr(false)
	disabled, err := service.UpdateUser(context.Background(), "user", input, actor, "")
	if err != nil || disabled.TwoFactorEnabled {
		t.Fatalf("disable: %v", err)
	}
	value, err := vault.Decrypt(repo.users["user"].TwoFactorSecretCipher)
	if err != nil || value != "" || repo.users["user"].TwoFactorLastStep != -1 {
		t.Fatal("disabled key retained")
	}
	if _, err = service.UserTwoFactorDetails(context.Background(), "user", actor, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("disabled details: %v", err)
	}
}

func TestUpdateUserValidatesPasswordAndEnrollmentBeforeWriting(t *testing.T) {
	for _, scenario := range []string{"password", "missing setup", "wrong setup", "renamed username"} {
		t.Run(scenario, func(t *testing.T) {
			service, repo, _, actor := twoFactorUserService(t)
			original := domain.User{ID: "user", Username: "member", DisplayName: "Member", PasswordHash: "oldhash", Role: "operator", Active: true}
			repo.users["user"] = original
			input := SaveUserInput{Username: "member", DisplayName: "mutated", Role: "operator", Enabled: false}
			switch scenario {
			case "password":
				input.Password = "short"
			case "missing setup":
				input.TwoFactorEnabled = boolPtr(true)
			case "wrong setup", "renamed username":
				setup, err := service.TwoFactorSetup(context.Background(), TwoFactorSetupInput{Username: "member", UserID: "user"}, actor)
				if err != nil {
					t.Fatal(err)
				}
				input.TwoFactorEnabled = boolPtr(true)
				input.TwoFactorSetupToken = setup.SetupToken
				input.TwoFactorCode = "bad"
				if scenario == "renamed username" {
					input.Username = "changed"
					input.TwoFactorCode = bindingCode(setup.Secret)
				}
			}
			if _, err := service.UpdateUser(context.Background(), "user", input, actor, ""); err == nil {
				t.Fatal("expected invalid credentials")
			}
			if repo.writes != 0 || repo.revokes != 0 || repo.users["user"].DisplayName != original.DisplayName {
				t.Fatal("failed request mutated account or sessions")
			}
		})
	}
}

func TestUpdateUserRejectsStaleEnrollmentAfterAnotherAdminEnabled(t *testing.T) {
	service, repo, _, actor := twoFactorUserService(t)
	repo.users["user"] = domain.User{ID: "user", Username: "member", PasswordHash: "hash", Role: "operator", Active: true}
	stale, err := service.TwoFactorSetup(context.Background(), TwoFactorSetupInput{Username: "member", UserID: "user"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	other := domain.User{ID: "other-admin", Role: "admin", Active: true}
	current, err := service.TwoFactorSetup(context.Background(), TwoFactorSetupInput{Username: "member", UserID: "user"}, other)
	if err != nil {
		t.Fatal(err)
	}
	input := SaveUserInput{Username: "member", Role: "operator", Enabled: true, TwoFactorEnabled: boolPtr(true), TwoFactorSetupToken: current.SetupToken, TwoFactorCode: bindingCode(current.Secret)}
	if _, err = service.UpdateUser(context.Background(), "user", input, other, ""); err != nil {
		t.Fatal(err)
	}
	before := repo.writes
	input.TwoFactorSetupToken = stale.SetupToken
	input.TwoFactorCode = bindingCode(stale.Secret)
	if _, err = service.UpdateUser(context.Background(), "user", input, actor, ""); !errors.Is(err, auth.ErrTwoFactorSetupInvalid) {
		t.Fatalf("stale enrollment returned %v", err)
	}
	details, err := service.UserTwoFactorDetails(context.Background(), "user", actor, "")
	if err != nil || repo.writes != before || details.Secret != current.Secret {
		t.Fatal("stale enrollment changed account")
	}
}
