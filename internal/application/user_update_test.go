package application

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"buysms/internal/auth"
	"buysms/internal/config"
	"buysms/internal/domain"
	"buysms/internal/secure"
	"buysms/internal/store"
)

type userUpdateRepository struct {
	store.Repository

	mu        sync.Mutex
	user      domain.User
	revokeErr error
	calls     []string
}

func (r *userUpdateRepository) GetUser(_ context.Context, id string) (domain.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "get")
	if r.user.ID != id {
		return domain.User{}, store.ErrNotFound
	}
	return r.user, nil
}

func (r *userUpdateRepository) RevokeUserSessions(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "revoke")
	if r.user.ID != id {
		return store.ErrNotFound
	}
	return r.revokeErr
}

func (r *userUpdateRepository) UpdateUser(_ context.Context, user domain.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "update")
	r.user = user
	return nil
}

func (r *userUpdateRepository) Audit(context.Context, *string, string, string, string, string, json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "audit")
	return nil
}

func (r *userUpdateRepository) snapshot() (domain.User, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.user, append([]string(nil), r.calls...)
}

func TestUpdateUserDisablingRequiresSessionRevocationBeforeMutation(t *testing.T) {
	disable := SaveUserInput{Username: "operator", DisplayName: "已禁用操作员", Role: "operator", Enabled: false}
	actor := domain.User{ID: "admin-1", Role: "admin", Active: true}
	for _, test := range []struct {
		name       string
		revokeErr  error
		wantErr    bool
		wantActive bool
		wantCalls  []string
	}{
		{
			name: "撤销失败保持启用且不更新", revokeErr: errors.New("revoke failed"), wantErr: true,
			wantActive: true, wantCalls: []string{"get", "revoke"},
		},
		{
			name: "撤销成功后更新为禁用", wantActive: false,
			wantCalls: []string{"get", "revoke", "update", "audit", "get"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &userUpdateRepository{
				user:      domain.User{ID: "operator-1", Username: "operator", DisplayName: "操作员", Role: "operator", Active: true},
				revokeErr: test.revokeErr,
			}
			vault, _ := secure.NewVault([]byte("user-update-test-encryption-key"))
			authentication := auth.New(repo, []byte("user-update-test-pepper"), "/secret", 0, 0)
			service := New(repo, authentication, vault, config.Config{})

			_, err := service.UpdateUser(context.Background(), "operator-1", disable, actor, "127.0.0.1")
			if (err != nil) != test.wantErr {
				t.Fatalf("UpdateUser 错误=%v，wantErr=%v", err, test.wantErr)
			}
			user, calls := repo.snapshot()
			if user.Active != test.wantActive {
				t.Fatalf("用户 active=%v，期望=%v", user.Active, test.wantActive)
			}
			if !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatalf("调用顺序=%v，期望=%v", calls, test.wantCalls)
			}
		})
	}
}
