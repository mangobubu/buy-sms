package application

import (
	"context"
	"strings"

	"buysms/internal/auth"
	"buysms/internal/domain"
)

type TwoFactorSetupInput struct {
	Username string `json:"username"`
	UserID   string `json:"userId"`
}

func (s *Service) TwoFactorSetup(ctx context.Context, in TwoFactorSetupInput, actor domain.User) (auth.TwoFactorSetup, error) {
	if actor.Role != "admin" {
		return auth.TwoFactorSetup{}, ErrForbidden
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || len(in.Username) > 256 {
		return auth.TwoFactorSetup{}, ErrBadRequest
	}
	var version int64
	if in.UserID != "" {
		user, err := s.repo.GetUser(ctx, in.UserID)
		if err != nil {
			return auth.TwoFactorSetup{}, mapStore(err)
		}
		if user.TwoFactorEnabled {
			return auth.TwoFactorSetup{}, ErrConflict
		}
		version = user.AuthVersion
	}
	return s.auth.NewTwoFactorSetup(actor.ID, in.UserID, in.Username, version)
}

func (s *Service) UserTwoFactorDetails(ctx context.Context, id string, actor domain.User, ip string) (auth.TwoFactorDetails, error) {
	if actor.Role != "admin" {
		return auth.TwoFactorDetails{}, ErrForbidden
	}
	user, err := s.repo.GetUser(ctx, id)
	if err != nil {
		return auth.TwoFactorDetails{}, mapStore(err)
	}
	if !user.TwoFactorEnabled {
		return auth.TwoFactorDetails{}, ErrConflict
	}
	details, err := s.auth.TwoFactorDetails(user)
	if err != nil {
		return auth.TwoFactorDetails{}, err
	}
	_ = s.repo.Audit(ctx, &actor.ID, "user.two_factor.read", "user", id, ip, nil)
	return details, nil
}

func (s *Service) prepareTwoFactor(user *domain.User, targetID string, in SaveUserInput, actor domain.User) error {
	if in.TwoFactorEnabled == nil {
		return nil
	}
	if !*in.TwoFactorEnabled {
		user.TwoFactorEnabled = false
		user.TwoFactorSecretCipher = nil
		user.TwoFactorLastStep = -1
		return nil
	}
	if user.TwoFactorEnabled {
		if in.TwoFactorSetupToken != "" || in.TwoFactorCode != "" {
			return auth.ErrTwoFactorSetupInvalid
		}
		return nil
	}
	cipher, step, err := s.auth.VerifyTwoFactorSetup(actor.ID, targetID, user.Username, user.AuthVersion, in.TwoFactorSetupToken, in.TwoFactorCode)
	if err != nil {
		return err
	}
	user.TwoFactorEnabled = true
	user.TwoFactorSecretCipher = cipher
	user.TwoFactorLastStep = step
	return nil
}
