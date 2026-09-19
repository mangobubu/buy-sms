package store

import (
	"context"
	"errors"
	"net"
	"time"

	"buysms/internal/domain"
	"github.com/jackc/pgx/v5"
)

const twoFactorChallengeCols = `token_hash,user_id,host(ip),auth_version,login_attempt_id,attempts,expires_at,consumed_at IS NOT NULL`

func scanTwoFactorChallenge(row pgx.Row) (domain.TwoFactorChallenge, bool, error) {
	var challenge domain.TwoFactorChallenge
	var consumed bool
	err := row.Scan(&challenge.TokenHash, &challenge.UserID, &challenge.IP, &challenge.AuthVersion, &challenge.LoginAttemptID, &challenge.Attempts, &challenge.ExpiresAt, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrChallengeExpired
	}
	return challenge, consumed, err
}

func (s *Postgres) CreateTwoFactorChallenge(ctx context.Context, challenge domain.TwoFactorChallenge) error {
	ct, err := s.pool.Exec(ctx, `INSERT INTO two_factor_challenges(token_hash,user_id,ip,auth_version,login_attempt_id,expires_at) SELECT $1,id,NULLIF($3,'')::inet,auth_version,$5,$6 FROM users WHERE id=$2 AND active AND two_factor_enabled AND auth_version=$4`, challenge.TokenHash, challenge.UserID, challenge.IP, challenge.AuthVersion, challenge.LoginAttemptID, challenge.ExpiresAt)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrChallengeExpired
	}
	return err
}

func (s *Postgres) ReserveTwoFactorChallenge(ctx context.Context, hash []byte, ip string, now time.Time, maxAttempts int) (domain.TwoFactorChallenge, domain.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.TwoFactorChallenge{}, domain.User{}, err
	}
	defer tx.Rollback(ctx)
	challenge, consumed, err := scanTwoFactorChallenge(tx.QueryRow(ctx, `SELECT `+twoFactorChallengeCols+` FROM two_factor_challenges WHERE token_hash=$1 FOR UPDATE`, hash))
	if err != nil {
		return challenge, domain.User{}, err
	}
	if consumed || !challenge.ExpiresAt.After(now) || !sameIP(challenge.IP, ip) {
		return challenge, domain.User{}, ErrChallengeExpired
	}
	user, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, challenge.UserID))
	if err != nil {
		return challenge, domain.User{}, err
	}
	if !user.Active || !user.TwoFactorEnabled || user.AuthVersion != challenge.AuthVersion {
		return challenge, domain.User{}, ErrChallengeExpired
	}
	if challenge.Attempts >= maxAttempts {
		return challenge, domain.User{}, ErrChallengeLimited
	}
	if _, err = tx.Exec(ctx, `UPDATE two_factor_challenges SET attempts=attempts+1 WHERE token_hash=$1`, hash); err != nil {
		return challenge, domain.User{}, err
	}
	challenge.Attempts++
	if err = tx.Commit(ctx); err != nil {
		return challenge, domain.User{}, err
	}
	return challenge, user, nil
}

func (s *Postgres) RedeemTwoFactorChallenge(ctx context.Context, hash []byte, ip string, version, step int64, session domain.Session, now time.Time) (domain.User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.User{}, err
	}
	defer tx.Rollback(ctx)
	// Lock users before challenges, consistently with profile/password updates.
	user, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1 FOR UPDATE`, session.UserID))
	if err != nil {
		return domain.User{}, err
	}
	challenge, consumed, err := scanTwoFactorChallenge(tx.QueryRow(ctx, `SELECT `+twoFactorChallengeCols+` FROM two_factor_challenges WHERE token_hash=$1 FOR UPDATE`, hash))
	if err != nil {
		return domain.User{}, err
	}
	if consumed || !challenge.ExpiresAt.After(now) || !sameIP(challenge.IP, ip) || challenge.UserID != user.ID || !user.Active || !user.TwoFactorEnabled || user.AuthVersion != version || challenge.AuthVersion != version || challenge.Attempts < 1 || challenge.Attempts > 5 {
		return domain.User{}, ErrChallengeExpired
	}
	if step <= user.TwoFactorLastStep {
		return domain.User{}, ErrCodeUsed
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET two_factor_last_step=$2 WHERE id=$1`, user.ID, step); err != nil {
		return domain.User{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE two_factor_challenges SET consumed_at=$2 WHERE token_hash=$1`, hash, now); err != nil {
		return domain.User{}, err
	}
	if err = insertSession(ctx, tx, session); err != nil {
		return domain.User{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.User{}, err
	}
	user.TwoFactorLastStep = step
	return user, nil
}

// PostgreSQL inet normalizes IPv6 text while trusted proxy headers may not.
func sameIP(left, right string) bool {
	a, b := net.ParseIP(left), net.ParseIP(right)
	return a != nil && b != nil && a.Equal(b)
}
