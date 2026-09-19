package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"buysms/internal/domain"
	"buysms/internal/identity"
	"buysms/internal/secure"
)

// This opt-in suite exercises actual PostgreSQL locks, migrations and commits.
// Point BUY_SMS_2FA_TEST_DATABASE_URL at a disposable PostgreSQL database.
func TestTwoFactorPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("BUY_SMS_2FA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BUY_SMS_2FA_TEST_DATABASE_URL to run PostgreSQL 2FA integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repo, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	for i := 0; i < 2; i++ {
		if err := repo.Migrate(ctx); err != nil {
			t.Fatalf("migration run %d: %v", i+1, err)
		}
	}
	vault, err := secure.NewVault([]byte(identity.Token(32)))
	if err != nil {
		t.Fatal(err)
	}
	f := twoFactorDatabaseFixture{repo: repo, ctx: ctx, vault: vault, now: time.Now().UTC().Truncate(time.Microsecond)}

	t.Run("encrypted user round trip and legacy defaults", func(t *testing.T) {
		u := f.user(t, true)
		var stored []byte
		if err := repo.pool.QueryRow(ctx, `SELECT two_factor_secret_cipher FROM users WHERE id=$1`, u.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored, u.TwoFactorSecretCipher) || bytes.Contains(stored, []byte(twoFactorFixtureSecret)) {
			t.Fatal("2FA secret was not persisted as ciphertext")
		}
		plain, err := vault.Decrypt(stored)
		if err != nil || plain != twoFactorFixtureSecret {
			t.Fatalf("persisted secret cannot be decrypted correctly: %v", err)
		}
		byName, err := repo.FindUserByUsername(ctx, strings.ToUpper(u.Username))
		if err != nil || byName.ID != u.ID || !byName.TwoFactorEnabled || !bytes.Equal(byName.TwoFactorSecretCipher, stored) {
			t.Fatalf("username lookup lost 2FA state: %+v, %v", byName, err)
		}
		users, err := repo.ListUsers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, listed := range users {
			if listed.ID == u.ID {
				found = listed.TwoFactorEnabled && bytes.Equal(listed.TwoFactorSecretCipher, stored)
			}
		}
		if !found {
			t.Fatal("user listing lost the persisted 2FA state")
		}
		encoded, err := json.Marshal(byName)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["twoFactorEnabled"] != true || fields["twoFactorSecretCipher"] != nil || fields["authVersion"] != nil || fields["twoFactorLastStep"] != nil {
			t.Fatalf("public user JSON exposes private 2FA state: %s", encoded)
		}
		legacy := f.user(t, false)
		if legacy.TwoFactorEnabled || len(legacy.TwoFactorSecretCipher) != 0 || legacy.TwoFactorLastStep != -1 || legacy.AuthVersion != 0 {
			t.Fatalf("unexpected defaults for existing login flow: %+v", legacy)
		}
	})

	t.Run("password-only login rejects stale credential snapshots", func(t *testing.T) {
		u := f.user(t, false)
		session := f.session(u)
		if err := repo.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
		f.authenticated(t, session, u.ID)
		stale := u
		u.TwoFactorEnabled = true
		u.TwoFactorSecretCipher = f.cipher(t)
		if err := repo.UpdateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		u = f.readUser(t, u.ID)
		if u.AuthVersion <= stale.AuthVersion {
			t.Fatal("enabling 2FA did not change the credential version")
		}
		f.revoked(t, session)
		if err := repo.CreateSession(ctx, f.session(stale)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("stale password-only login accepted after enabling 2FA: %v", err)
		}
		if err := repo.CreateSession(ctx, f.session(u)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("enabled 2FA allowed password-only session creation: %v", err)
		}
		u.TwoFactorEnabled = false
		u.TwoFactorSecretCipher = nil
		if err := repo.UpdateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		u = f.readUser(t, u.ID)
		if err := repo.CreateSession(ctx, f.session(stale)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old snapshot accepted after a full 2FA enable/disable cycle: %v", err)
		}
		freshSession := f.session(u)
		if err := repo.CreateSession(ctx, freshSession); err != nil {
			t.Fatal(err)
		}
		f.authenticated(t, freshSession, u.ID)
		if err := repo.UpdateUser(ctx, stale); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale editor overwrote the changed 2FA state: %v", err)
		}
	})

	t.Run("enabling 2FA races safely with password-only session creation", func(t *testing.T) {
		u := f.user(t, false)
		session := f.session(u)
		changed := u
		changed.TwoFactorEnabled, changed.TwoFactorSecretCipher = true, f.cipher(t)
		results := twoFactorConcurrent(2, func(i int) error {
			if i == 0 {
				return repo.CreateSession(ctx, session)
			}
			return repo.UpdateUser(ctx, changed)
		})
		if results[1] != nil {
			t.Fatalf("enable 2FA: %v", results[1])
		}
		if results[0] != nil && !errors.Is(results[0], ErrNotFound) {
			t.Fatalf("password login during 2FA enable: %v", results[0])
		}
		if _, err := repo.FindSession(ctx, session.TokenHash, f.now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("concurrent enable left a usable password-only session: %v", err)
		}
		if results[0] == nil {
			f.revoked(t, session)
		}
	})

	t.Run("disabling 2FA races safely with challenge redemption", func(t *testing.T) {
		u := f.user(t, true)
		c := f.challenge(t, u)
		f.reserve(t, c)
		session := f.session(u)
		changed := u
		changed.TwoFactorEnabled, changed.TwoFactorSecretCipher = false, nil
		results := twoFactorConcurrent(2, func(i int) error {
			if i == 0 {
				_, err := repo.RedeemTwoFactorChallenge(ctx, c.TokenHash, c.IP, u.AuthVersion, 100, session, f.now)
				return err
			}
			return repo.UpdateUser(ctx, changed)
		})
		if results[1] != nil {
			t.Fatalf("disable 2FA: %v", results[1])
		}
		if results[0] != nil && !errors.Is(results[0], ErrChallengeExpired) {
			t.Fatalf("challenge redemption during 2FA disable: %v", results[0])
		}
		if _, err := repo.FindSession(ctx, session.TokenHash, f.now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("concurrent disable left a usable session from the old challenge: %v", err)
		}
		if results[0] == nil {
			f.revoked(t, session)
		}
	})

	t.Run("failed profile update rolls back credential changes and revocations", func(t *testing.T) {
		u := f.user(t, true)
		other := f.user(t, false)
		session := f.login(t, u, 100)
		changed := u
		changed.Username = other.Username
		changed.TwoFactorEnabled, changed.TwoFactorSecretCipher = false, nil
		if err := repo.UpdateUser(ctx, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate username did not report conflict: %v", err)
		}
		got := f.readUser(t, u.ID)
		if !got.TwoFactorEnabled || got.AuthVersion != u.AuthVersion || got.TwoFactorLastStep != 100 || !bytes.Equal(got.TwoFactorSecretCipher, u.TwoFactorSecretCipher) {
			t.Fatalf("failed profile update changed credentials: %+v", got)
		}
		f.authenticated(t, session, u.ID)
	})
	t.Run("attempts are durable limited and bound to the IP", func(t *testing.T) {
		u := f.user(t, true)
		c := f.challenge(t, u)
		if _, _, err := repo.ReserveTwoFactorChallenge(ctx, c.TokenHash, "192.0.2.99", f.now, 5); !errors.Is(err, ErrChallengeExpired) {
			t.Fatalf("challenge accepted from another IP: %v", err)
		}
		f.attempts(t, c, 0)
		for i := 1; i <= 5; i++ {
			got, who, err := repo.ReserveTwoFactorChallenge(ctx, c.TokenHash, c.IP, f.now, 5)
			if err != nil || got.Attempts != i || who.ID != u.ID {
				t.Fatalf("attempt %d: challenge=%+v user=%s err=%v", i, got, who.ID, err)
			}
			f.attempts(t, c, i)
		}
		if _, _, err := repo.ReserveTwoFactorChallenge(ctx, c.TokenHash, c.IP, f.now, 5); !errors.Is(err, ErrChallengeLimited) {
			t.Fatalf("exhausted challenge accepted another attempt: %v", err)
		}
		f.attempts(t, c, 5)
		f.sessionCount(t, u.ID, 0)
	})

	t.Run("IPv6 challenge accepts equivalent address representations", func(t *testing.T) {
		u := f.user(t, true)
		c := domain.TwoFactorChallenge{TokenHash: twoFactorHash(), UserID: u.ID, IP: "2001:0DB8:0000:0000:0000:0000:0000:ABCD", AuthVersion: u.AuthVersion, ExpiresAt: f.now.Add(5 * time.Minute)}
		if err := repo.pool.QueryRow(ctx, `INSERT INTO login_attempts(ip,username_normalized,success,attempted_at) VALUES($1::inet,$2,false,$3) RETURNING id`, c.IP, u.Username, f.now).Scan(&c.LoginAttemptID); err != nil {
			t.Fatal(err)
		}
		if err := repo.CreateTwoFactorChallenge(ctx, c); err != nil {
			t.Fatal(err)
		}
		const differentIP = "2001:db8::abce"
		if _, _, err := repo.ReserveTwoFactorChallenge(ctx, c.TokenHash, differentIP, f.now, 5); !errors.Is(err, ErrChallengeExpired) {
			t.Fatalf("IPv6 challenge accepted another address during reservation: %v", err)
		}
		f.attempts(t, c, 0)
		for _, equivalentIP := range []string{"2001:db8::abcd", "2001:DB8:0:0:0:0:0:ABCD"} {
			got, who, err := repo.ReserveTwoFactorChallenge(ctx, c.TokenHash, equivalentIP, f.now, 5)
			if err != nil || who.ID != u.ID || got.IP != "2001:db8::abcd" {
				t.Fatalf("equivalent IPv6 reservation %q: stored IP=%q user=%s err=%v", equivalentIP, got.IP, who.ID, err)
			}
		}
		f.attempts(t, c, 2)
		session := f.session(u)
		session.IP = differentIP
		if _, err := repo.RedeemTwoFactorChallenge(ctx, c.TokenHash, differentIP, u.AuthVersion, 100, session, f.now); !errors.Is(err, ErrChallengeExpired) {
			t.Fatalf("IPv6 challenge accepted another address during redemption: %v", err)
		}
		f.sessionCount(t, u.ID, 0)
		session.IP = "2001:0db8:0:0::abcd"
		if _, err := repo.RedeemTwoFactorChallenge(ctx, c.TokenHash, session.IP, u.AuthVersion, 100, session, f.now); err != nil {
			t.Fatalf("equivalent IPv6 challenge redemption: %v", err)
		}
		f.authenticated(t, session, u.ID)
		f.sessionCount(t, u.ID, 1)
	})
	t.Run("concurrent reservations obey the attempt limit", func(t *testing.T) {
		u := f.user(t, true)
		c := f.challenge(t, u)
		results := twoFactorConcurrent(12, func(_ int) error {
			_, _, err := repo.ReserveTwoFactorChallenge(ctx, c.TokenHash, c.IP, f.now, 3)
			return err
		})
		successes := 0
		for _, err := range results {
			if err == nil {
				successes++
			} else if !errors.Is(err, ErrChallengeLimited) {
				t.Errorf("unexpected reservation error: %v", err)
			}
		}
		if successes != 3 {
			t.Fatalf("concurrent successful reservations=%d, want 3", successes)
		}
		f.attempts(t, c, 3)
	})

	t.Run("expiry is enforced at reserve and redeem boundaries", func(t *testing.T) {
		u := f.user(t, true)
		c := f.challenge(t, u)
		f.reserve(t, c)
		if _, _, err := repo.ReserveTwoFactorChallenge(ctx, c.TokenHash, c.IP, c.ExpiresAt, 5); !errors.Is(err, ErrChallengeExpired) {
			t.Fatalf("challenge accepted at its expiry: %v", err)
		}
		if _, err := repo.RedeemTwoFactorChallenge(ctx, c.TokenHash, c.IP, u.AuthVersion, 100, f.session(u), c.ExpiresAt); !errors.Is(err, ErrChallengeExpired) {
			t.Fatalf("reserved challenge redeemed after expiry: %v", err)
		}
		f.sessionCount(t, u.ID, 0)
	})

	for _, mutation := range []string{"password", "password and revoke", "disable 2FA", "rotate secret", "deactivate"} {
		t.Run(mutation+" invalidates pending and reserved challenges", func(t *testing.T) {
			u := f.user(t, true)
			c := f.challenge(t, u)
			f.reserve(t, c)
			pending := f.challenge(t, u)
			session := f.login(t, u, 99)
			oldVersion := u.AuthVersion
			var err error
			switch mutation {
			case "password":
				err = repo.UpdatePassword(ctx, u.ID, "updated-password-hash")
			case "password and revoke":
				err = repo.UpdatePasswordAndRevoke(ctx, u.ID, "updated-password-hash")
			case "disable 2FA":
				u.TwoFactorEnabled, u.TwoFactorSecretCipher = false, nil
				err = repo.UpdateUser(ctx, u)
			case "rotate secret":
				u.TwoFactorSecretCipher, err = vault.Encrypt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
				if err == nil {
					err = repo.UpdateUser(ctx, u)
				}
			case "deactivate":
				u.Active = false
				err = repo.UpdateUser(ctx, u)
			}
			if err != nil {
				t.Fatal(err)
			}
			changed := f.readUser(t, u.ID)
			if changed.AuthVersion <= oldVersion {
				t.Fatal("credential mutation did not advance the authentication version")
			}
			f.revoked(t, session)
			if _, _, err := repo.ReserveTwoFactorChallenge(ctx, pending.TokenHash, pending.IP, f.now, 5); !errors.Is(err, ErrChallengeExpired) {
				t.Fatalf("pending challenge remained usable: %v", err)
			}
			if _, err := repo.RedeemTwoFactorChallenge(ctx, c.TokenHash, c.IP, oldVersion, 100, f.session(u), f.now); !errors.Is(err, ErrChallengeExpired) {
				t.Fatalf("reserved challenge remained redeemable: %v", err)
			}
			if err := repo.CreateTwoFactorChallenge(ctx, domain.TwoFactorChallenge{TokenHash: twoFactorHash(), UserID: u.ID, IP: c.IP, AuthVersion: oldVersion, LoginAttemptID: c.LoginAttemptID, ExpiresAt: c.ExpiresAt}); !errors.Is(err, ErrChallengeExpired) {
				t.Fatalf("stale password verification created a new challenge: %v", err)
			}
		})
	}

	for _, distinctChallenges := range []bool{false, true} {
		name := "same challenge redeems once under concurrency"
		if distinctChallenges {
			name = "different challenges cannot replay the same TOTP step"
		}
		t.Run(name, func(t *testing.T) {
			u := f.user(t, true)
			const workers = 8
			challenges := make([]domain.TwoFactorChallenge, workers)
			sessions := make([]domain.Session, workers)
			challenges[0] = f.challenge(t, u)
			f.reserve(t, challenges[0])
			for i := range challenges {
				if i > 0 {
					challenges[i] = challenges[0]
					if distinctChallenges {
						challenges[i] = f.challenge(t, u)
						f.reserve(t, challenges[i])
					}
				}
				sessions[i] = f.session(u)
			}
			results := twoFactorConcurrent(workers, func(i int) error {
				c := challenges[i]
				_, err := repo.RedeemTwoFactorChallenge(ctx, c.TokenHash, c.IP, u.AuthVersion, 100, sessions[i], f.now)
				return err
			})
			successes, winner := 0, -1
			for i, err := range results {
				if err == nil {
					successes++
					winner = i
				} else if !errors.Is(err, ErrChallengeExpired) && !errors.Is(err, ErrCodeUsed) {
					t.Errorf("unexpected redemption error: %v", err)
				}
			}
			if successes != 1 {
				t.Fatalf("successful redemptions=%d, want exactly 1", successes)
			}
			f.authenticated(t, sessions[winner], u.ID)
			f.sessionCount(t, u.ID, 1)
			u = f.readUser(t, u.ID)
			if u.TwoFactorLastStep != 100 {
				t.Fatalf("successful redemption did not persist the consumed step: %+v", u)
			}
			if _, err := repo.RedeemTwoFactorChallenge(ctx, challenges[winner].TokenHash, challenges[winner].IP, u.AuthVersion, 101, f.session(u), f.now); !errors.Is(err, ErrChallengeExpired) {
				t.Fatalf("consumed challenge reused with a later step: %v", err)
			}
		})
	}

	t.Run("profile edit preserves consumed TOTP steps and existing sessions", func(t *testing.T) {
		u := f.user(t, true)
		staleProfile := u
		session := f.login(t, u, 100)
		staleProfile.DisplayName = "Updated display name"
		if err := repo.UpdateUser(ctx, staleProfile); err != nil {
			t.Fatal(err)
		}
		u = f.readUser(t, u.ID)
		if u.TwoFactorLastStep != 100 || u.AuthVersion != staleProfile.AuthVersion || u.DisplayName != staleProfile.DisplayName {
			t.Fatalf("profile update changed credential state: %+v", u)
		}
		f.authenticated(t, session, u.ID)
		c := f.challenge(t, u)
		f.reserve(t, c)
		if _, err := repo.RedeemTwoFactorChallenge(ctx, c.TokenHash, c.IP, u.AuthVersion, 100, f.session(u), f.now); !errors.Is(err, ErrCodeUsed) {
			t.Fatalf("profile edit allowed reuse of a consumed TOTP step: %v", err)
		}
	})

	t.Run("failed session insert rolls back challenge and step consumption", func(t *testing.T) {
		u := f.user(t, true)
		existing := f.login(t, u, 99)
		c := f.challenge(t, u)
		f.reserve(t, c)
		duplicate := f.session(u)
		duplicate.ID = existing.ID
		if _, err := repo.RedeemTwoFactorChallenge(ctx, c.TokenHash, c.IP, u.AuthVersion, 100, duplicate, f.now); err == nil {
			t.Fatal("duplicate session ID unexpectedly accepted")
		}
		if got := f.readUser(t, u.ID).TwoFactorLastStep; got != 99 {
			t.Fatalf("failed transaction consumed TOTP step %d", got)
		}
		var consumed *time.Time
		if err := repo.pool.QueryRow(ctx, `SELECT consumed_at FROM two_factor_challenges WHERE token_hash=$1`, c.TokenHash).Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		if consumed != nil {
			t.Fatal("failed session insert consumed the challenge")
		}
		var attemptSuccess bool
		if err := repo.pool.QueryRow(ctx, `SELECT success FROM login_attempts WHERE id=$1`, c.LoginAttemptID).Scan(&attemptSuccess); err != nil {
			t.Fatal(err)
		}
		if attemptSuccess {
			t.Fatal("failed session insert completed its login attempt")
		}
		valid := f.session(u)
		if _, err := repo.RedeemTwoFactorChallenge(ctx, c.TokenHash, c.IP, u.AuthVersion, 100, valid, f.now); err != nil {
			t.Fatalf("rolled-back redemption cannot be retried: %v", err)
		}
		f.authenticated(t, valid, u.ID)
		f.sessionCount(t, u.ID, 2)
	})
}

const twoFactorFixtureSecret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"

type twoFactorDatabaseFixture struct {
	repo  *Postgres
	ctx   context.Context
	vault *secure.Vault
	now   time.Time
}

func (f twoFactorDatabaseFixture) cipher(t *testing.T) []byte {
	t.Helper()
	ciphertext, err := f.vault.Encrypt(twoFactorFixtureSecret)
	if err != nil {
		t.Fatal(err)
	}
	return ciphertext
}

func (f twoFactorDatabaseFixture) user(t *testing.T, enabled bool) domain.User {
	t.Helper()
	u := domain.User{ID: identity.UUID(), Username: "twofa_test_" + identity.UUID(), DisplayName: "2FA integration fixture", PasswordHash: "fixture-password-hash", Role: "operator", Active: true, TwoFactorEnabled: enabled, TwoFactorLastStep: -1}
	if enabled {
		u.TwoFactorSecretCipher = f.cipher(t)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := f.repo.pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, u.ID); err != nil {
			t.Errorf("clean up test user: %v", err)
		}
		if _, err := f.repo.pool.Exec(ctx, `DELETE FROM login_attempts WHERE username_normalized=$1`, u.Username); err != nil {
			t.Errorf("clean up test login attempts: %v", err)
		}
	})
	if err := f.repo.CreateUser(f.ctx, u); err != nil {
		t.Fatal(err)
	}
	return f.readUser(t, u.ID)
}

func (f twoFactorDatabaseFixture) readUser(t *testing.T, id string) domain.User {
	t.Helper()
	u, err := f.repo.GetUser(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (f twoFactorDatabaseFixture) challenge(t *testing.T, u domain.User) domain.TwoFactorChallenge {
	t.Helper()
	c := domain.TwoFactorChallenge{TokenHash: twoFactorHash(), UserID: u.ID, IP: "192.0.2.10", AuthVersion: u.AuthVersion, ExpiresAt: f.now.Add(5 * time.Minute)}
	if err := f.repo.pool.QueryRow(f.ctx, `INSERT INTO login_attempts(ip,username_normalized,success,attempted_at) VALUES($1::inet,$2,false,$3) RETURNING id`, c.IP, u.Username, f.now).Scan(&c.LoginAttemptID); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.CreateTwoFactorChallenge(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (f twoFactorDatabaseFixture) reserve(t *testing.T, c domain.TwoFactorChallenge) {
	t.Helper()
	if _, _, err := f.repo.ReserveTwoFactorChallenge(f.ctx, c.TokenHash, c.IP, f.now, 5); err != nil {
		t.Fatal(err)
	}
}

func (f twoFactorDatabaseFixture) session(u domain.User) domain.Session {
	return domain.Session{ID: identity.UUID(), UserID: u.ID, AuthVersion: u.AuthVersion, TokenHash: twoFactorHash(), IP: "192.0.2.10", UserAgent: "2FA PostgreSQL integration test", ExpiresAt: f.now.Add(time.Hour)}
}

func (f twoFactorDatabaseFixture) login(t *testing.T, u domain.User, step int64) domain.Session {
	t.Helper()
	c := f.challenge(t, u)
	f.reserve(t, c)
	session := f.session(u)
	if _, err := f.repo.RedeemTwoFactorChallenge(f.ctx, c.TokenHash, c.IP, u.AuthVersion, step, session, f.now); err != nil {
		t.Fatal(err)
	}
	f.authenticated(t, session, u.ID)
	return session
}

func (f twoFactorDatabaseFixture) authenticated(t *testing.T, session domain.Session, userID string) {
	t.Helper()
	u, err := f.repo.FindSession(f.ctx, session.TokenHash, f.now)
	if err != nil || u.ID != userID {
		t.Fatalf("committed session does not authenticate expected user: %s, %v", u.ID, err)
	}
}

func (f twoFactorDatabaseFixture) revoked(t *testing.T, session domain.Session) {
	t.Helper()
	if _, err := f.repo.FindSession(f.ctx, session.TokenHash, f.now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("credential change left an active session: %v", err)
	}
	var revokedAt *time.Time
	if err := f.repo.pool.QueryRow(f.ctx, `SELECT revoked_at FROM auth_sessions WHERE id=$1`, session.ID).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt == nil {
		t.Fatal("credential change did not persist the session revocation")
	}
}

func (f twoFactorDatabaseFixture) attempts(t *testing.T, c domain.TwoFactorChallenge, want int) {
	t.Helper()
	var got int
	if err := f.repo.pool.QueryRow(f.ctx, `SELECT attempts FROM two_factor_challenges WHERE token_hash=$1`, c.TokenHash).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("persisted challenge attempts=%d, want %d", got, want)
	}
}

func (f twoFactorDatabaseFixture) sessionCount(t *testing.T, userID string, want int) {
	t.Helper()
	var got int
	if err := f.repo.pool.QueryRow(f.ctx, `SELECT count(*) FROM auth_sessions WHERE user_id=$1`, userID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("committed sessions=%d, want %d", got, want)
	}
}

func twoFactorHash() []byte {
	hash := sha256.Sum256([]byte(identity.Token(32)))
	return hash[:]
}

func twoFactorConcurrent(n int, run func(int) error) []error {
	start := make(chan struct{})
	results := make([]error, n)
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	for i := range results {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			results[i] = run(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results
}
