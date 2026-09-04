package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"buysms/internal/domain"
	"buysms/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct {
	pool           *pgxpool.Pool
	orderLockSlots chan struct{}
}

func Open(ctx context.Context, url string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("数据库配置无效: %w", err)
	}
	cfg.MaxConns = 12
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	s := &Postgres{pool: p, orderLockSlots: make(chan struct{}, 4)}
	if err = s.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("数据库不可用: %w", err)
	}
	return s, nil
}
func (s *Postgres) Close()                         { s.pool.Close() }
func (s *Postgres) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *Postgres) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	const migrationLock int64 = 108977388657907
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLock)
	}()
	_, err = conn.Exec(ctx, migrations.Initial)
	return err
}

func (s *Postgres) PutCaptcha(ctx context.Context, id string, hash []byte, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO captcha_challenges(id,code_hash,expires_at) VALUES($1,$2,$3)`, id, hash, expires)
	return err
}
func (s *Postgres) CaptchaAllowed(ctx context.Context, ip string, now time.Time, window time.Duration, max int) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,38821))`, ip).Scan(&locked); err != nil || !locked {
		return false, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM captcha_issuances WHERE ip=NULLIF($1,'')::inet AND issued_at>$2`, ip, now.Add(-window)).Scan(&count); err != nil {
		return false, err
	}
	if count >= max {
		return false, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO captcha_issuances(ip,issued_at) VALUES(NULLIF($1,'')::inet,$2)`, ip, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
func (s *Postgres) ConsumeCaptcha(ctx context.Context, id string, hash []byte, now time.Time) (bool, error) {
	ct, err := s.pool.Exec(ctx, `UPDATE captcha_challenges SET consumed_at=$3 WHERE id=$1 AND code_hash=$2 AND consumed_at IS NULL AND expires_at>$3`, id, hash, now)
	return ct.RowsAffected() == 1, err
}
func (s *Postgres) LoginAllowed(ctx context.Context, ip string, now time.Time, window time.Duration, max int) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE ip=NULLIF($1,'')::inet AND success=false AND attempted_at>$2`, ip, now.Add(-window)).Scan(&n)
	return n < max, err
}
func (s *Postgres) ReserveLoginAttempt(ctx context.Context, ip, user string, now time.Time, window time.Duration, max int) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,73241))`, ip).Scan(&locked); err != nil {
		return 0, false, err
	}
	if !locked {
		return 0, false, nil
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE ip=NULLIF($1,'')::inet AND success=false AND attempted_at>$2`, ip, now.Add(-window)).Scan(&count); err != nil {
		return 0, false, err
	}
	if count >= max {
		return 0, false, tx.Commit(ctx)
	}
	var id int64
	if err = tx.QueryRow(ctx, `INSERT INTO login_attempts(ip,username_normalized,success,attempted_at) VALUES(NULLIF($1,'')::inet,$2,false,$3) RETURNING id`, ip, strings.ToLower(strings.TrimSpace(user)), now).Scan(&id); err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit(ctx)
}
func (s *Postgres) CompleteLoginAttempt(ctx context.Context, id int64, success bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE login_attempts SET success=$2 WHERE id=$1`, id, success)
	return err
}
func (s *Postgres) RecordLoginAttempt(ctx context.Context, ip, user string, success bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO login_attempts(ip,username_normalized,success) VALUES(NULLIF($1,'')::inet,$2,$3)`, ip, strings.ToLower(strings.TrimSpace(user)), success)
	return err
}

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &u.Active, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return u, err
}

const userCols = `id,username,display_name,password_hash,role,active,last_login_at,created_at,updated_at`

func (s *Postgres) FindUserByUsername(ctx context.Context, name string) (domain.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username_normalized=$1`, strings.ToLower(strings.TrimSpace(name))))
}
func (s *Postgres) GetUser(ctx context.Context, id string) (domain.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}
func (s *Postgres) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.User{}
	for rows.Next() {
		u, e := scanUser(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
func (s *Postgres) CreateUser(ctx context.Context, u domain.User) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO users(id,username,username_normalized,display_name,password_hash,role,active) VALUES($1,$2,$3,$4,$5,$6,$7)`, u.ID, u.Username, strings.ToLower(strings.TrimSpace(u.Username)), u.DisplayName, u.PasswordHash, u.Role, u.Active)
	if unique(err) {
		return ErrConflict
	}
	return err
}
func (s *Postgres) UpdateUser(ctx context.Context, u domain.User) error {
	ct, err := s.pool.Exec(ctx, `UPDATE users SET username=$2,username_normalized=$3,display_name=$4,role=$5,active=$6,updated_at=now() WHERE id=$1`, u.ID, u.Username, strings.ToLower(strings.TrimSpace(u.Username)), u.DisplayName, u.Role, u.Active)
	if unique(err) {
		return ErrConflict
	}
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
func (s *Postgres) UpdatePassword(ctx context.Context, id, hash string) error {
	ct, err := s.pool.Exec(ctx, `UPDATE users SET password_hash=$2,updated_at=now() WHERE id=$1`, id, hash)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
func (s *Postgres) UpdatePasswordAndRevoke(ctx context.Context, id, hash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2,updated_at=now() WHERE id=$1`, id, hash)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Postgres) CreateSession(ctx context.Context, x domain.Session) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO auth_sessions(id,user_id,token_hash,ip,user_agent,expires_at) VALUES($1,$2,$3,NULLIF($4,'')::inet,$5,$6)`, x.ID, x.UserID, x.TokenHash, x.IP, x.UserAgent, x.ExpiresAt)
	return err
}
func (s *Postgres) FindSession(ctx context.Context, h []byte, now time.Time) (domain.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT u.id,u.username,u.display_name,u.password_hash,u.role,u.active,u.last_login_at,u.created_at,u.updated_at FROM auth_sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at>$2 AND u.active`, h, now))
}
func (s *Postgres) RevokeUserSessions(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE auth_sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, id)
	return err
}
func (s *Postgres) RevokeSession(ctx context.Context, h []byte) error {
	_, err := s.pool.Exec(ctx, `UPDATE auth_sessions SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, h)
	return err
}
func (s *Postgres) TouchLastLogin(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET last_login_at=now() WHERE id=$1`, id)
	return err
}

func (s *Postgres) EnsureProviders(ctx context.Context, ps []domain.Provider) error {
	for _, p := range ps {
		_, err := s.pool.Exec(ctx, `INSERT INTO providers(id,name,base_url,webhook_token_cipher,config) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO NOTHING`, p.ID, p.Name, p.BaseURL, p.WebhookTokenCipher, p.Config)
		if err != nil {
			return err
		}
	}
	return nil
}
func scanProvider(row pgx.Row) (domain.Provider, error) {
	var p domain.Provider
	err := row.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKeyCipher, &p.Enabled, &p.WebhookTokenCipher, &p.Config, &p.CreatedAt, &p.UpdatedAt)
	p.APIKeyConfigured = len(p.APIKeyCipher) > 0
	p.WebhookConfigured = len(p.WebhookTokenCipher) > 0
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return p, err
}
func (s *Postgres) ListProviders(ctx context.Context) ([]domain.Provider, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,base_url,api_key_cipher,enabled,webhook_token_cipher,config,created_at,updated_at FROM providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Provider{}
	for rows.Next() {
		p, e := scanProvider(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Postgres) GetProvider(ctx context.Context, id string) (domain.Provider, error) {
	return scanProvider(s.pool.QueryRow(ctx, `SELECT id,name,base_url,api_key_cipher,enabled,webhook_token_cipher,config,created_at,updated_at FROM providers WHERE id=$1`, domain.NormalizeProvider(id)))
}
func (s *Postgres) UpdateProvider(ctx context.Context, p domain.Provider) error {
	ct, err := s.pool.Exec(ctx, `UPDATE providers SET name=$2,base_url=$3,api_key_cipher=COALESCE($4,api_key_cipher),enabled=$5,webhook_token_cipher=COALESCE($6,webhook_token_cipher),config=$7,updated_at=now() WHERE id=$1`, p.ID, p.Name, p.BaseURL, p.APIKeyCipher, p.Enabled, p.WebhookTokenCipher, p.Config)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Postgres) ReplaceCatalog(ctx context.Context, pid, kind string, items []domain.CatalogItem) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	country := ""
	if len(items) > 0 {
		country = items[0].Country
		for _, x := range items {
			if x.Country != country {
				country = ""
				break
			}
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM provider_catalog WHERE provider_id=$1 AND kind=$2 AND ($3='' OR country=$3)`, pid, kind, country); err != nil {
		return err
	}
	for _, x := range items {
		_, err = tx.Exec(ctx, `INSERT INTO provider_catalog(provider_id,kind,code,country,name,price,stock,raw) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, pid, kind, x.Code, x.Country, x.Name, x.Price, x.Stock, x.Raw)
		if err != nil {
			return err
		}
	}
	// 目录刷新后仅补齐缺少快照的历史订单。已有名称必须保持不变，
	// 避免上游改名导致历史订单展示漂移。
	if _, err = tx.Exec(ctx, backfillOrderNamesSQL, pid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const upsertCatalogItemSQL = `
INSERT INTO provider_catalog(provider_id,kind,code,country,name,price,stock,raw,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,now())
ON CONFLICT(provider_id,kind,code,country) DO UPDATE SET
    name=EXCLUDED.name,
    price=COALESCE(EXCLUDED.price,provider_catalog.price),
    stock=COALESCE(EXCLUDED.stock,provider_catalog.stock),
    raw=EXCLUDED.raw,
    updated_at=now()`

// UpsertCatalog 保存筛选目录中的可读名称，但不删除未出现在本次筛选
// 结果中的目录项，避免不同服务或档位的请求互相覆盖。
func (s *Postgres) UpsertCatalog(ctx context.Context, pid string, items []domain.CatalogItem) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, x := range items {
		if _, err = tx.Exec(ctx, upsertCatalogItemSQL, pid, x.Kind, x.Code, x.Country, x.Name, x.Price, x.Stock, x.Raw); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, backfillOrderNamesSQL, pid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) ListCatalog(ctx context.Context, pid, kind, country string) ([]domain.CatalogItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT provider_id,kind,code,country,name,price,stock,raw,updated_at FROM provider_catalog WHERE provider_id=$1 AND kind=$2 AND ($3='' OR country=$3 OR country='') ORDER BY name`, pid, kind, country)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.CatalogItem{}
	for rows.Next() {
		var x domain.CatalogItem
		if err = rows.Scan(&x.ProviderID, &x.Kind, &x.Code, &x.Country, &x.Name, &x.Price, &x.Stock, &x.Raw, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

const backfillOrderNamesSQL = `
UPDATE orders AS o
SET country_name = CASE
        WHEN NULLIF(BTRIM(o.country_name), '') IS NULL THEN COALESCE((
            SELECT pc.name
            FROM provider_catalog AS pc
            WHERE pc.provider_id = o.provider_id
              AND pc.kind = 'country'
              AND pc.code = o.country_code
              AND NULLIF(BTRIM(pc.name), '') IS NOT NULL
            ORDER BY (pc.country = '') DESC, pc.updated_at DESC, pc.name
            LIMIT 1
        ), o.country_name)
        ELSE o.country_name
    END,
    service_name = CASE
        WHEN NULLIF(BTRIM(o.service_name), '') IS NULL THEN COALESCE((
            SELECT pc.name
            FROM provider_catalog AS pc
            WHERE pc.provider_id = o.provider_id
              AND pc.kind = 'service'
              AND pc.code = o.service_code
              AND pc.country IN (o.country_code, '')
              AND NULLIF(BTRIM(pc.name), '') IS NOT NULL
            ORDER BY (pc.country = o.country_code) DESC, pc.updated_at DESC, pc.name
            LIMIT 1
        ), o.service_name)
        ELSE o.service_name
    END
WHERE o.provider_id = $1
  AND (
      (
          NULLIF(BTRIM(o.country_name), '') IS NULL
          AND EXISTS (
              SELECT 1
              FROM provider_catalog AS pc
              WHERE pc.provider_id = o.provider_id
                AND pc.kind = 'country'
                AND pc.code = o.country_code
                AND NULLIF(BTRIM(pc.name), '') IS NOT NULL
          )
      )
      OR (
          NULLIF(BTRIM(o.service_name), '') IS NULL
          AND EXISTS (
              SELECT 1
              FROM provider_catalog AS pc
              WHERE pc.provider_id = o.provider_id
                AND pc.kind = 'service'
                AND pc.code = o.service_code
                AND pc.country IN (o.country_code, '')
                AND NULLIF(BTRIM(pc.name), '') IS NOT NULL
          )
      )
  )`

const insertOrderSQL = `
INSERT INTO orders(
    id,user_id,provider_id,upstream_id,phone_number,
    country_code,country_name,service_code,service_name,quality_tier,duration,
    status,cost,currency,can_get_another_sms,next_poll_at,expires_at,created_at
)
SELECT
    $1,$2,$3,$4,$5,
    $6,
    COALESCE(NULLIF(BTRIM($7), ''), (
        SELECT pc.name
        FROM provider_catalog AS pc
        WHERE pc.provider_id = $3
          AND pc.kind = 'country'
          AND pc.code = $6
          AND NULLIF(BTRIM(pc.name), '') IS NOT NULL
        ORDER BY (pc.country = '') DESC, pc.updated_at DESC, pc.name
        LIMIT 1
    ), ''),
    $8,
    COALESCE(NULLIF(BTRIM($9), ''), (
        SELECT pc.name
        FROM provider_catalog AS pc
        WHERE pc.provider_id = $3
          AND pc.kind = 'service'
          AND pc.code = $8
          AND pc.country IN ($6, '')
          AND NULLIF(BTRIM(pc.name), '') IS NOT NULL
        ORDER BY (pc.country = $6) DESC, pc.updated_at DESC, pc.name
        LIMIT 1
    ), ''),
    $10,$11,$12,$13,$14,$15,$16,$17,COALESCE($18::timestamptz,now())`

func orderInsertArgs(o domain.Order) []any {
	var createdAt any
	if !o.CreatedAt.IsZero() {
		createdAt = o.CreatedAt
	}
	return []any{
		o.ID, o.UserID, o.ProviderID, o.UpstreamID, o.PhoneNumber,
		o.CountryCode, o.CountryName, o.ServiceCode, o.ServiceName, o.QualityTier, o.Duration,
		o.Status, o.Cost, o.Currency, o.CanGetAnotherSMS, o.NextPollAt, o.ExpiresAt, createdAt,
	}
}

func (s *Postgres) CreateOrder(ctx context.Context, o domain.Order) error {
	_, err := s.pool.Exec(ctx, insertOrderSQL, orderInsertArgs(o)...)
	if unique(err) {
		return ErrConflict
	}
	return err
}
func (s *Postgres) ReservePurchase(ctx context.Context, r PurchaseRecord) (PurchaseRecord, bool, error) {
	ct, err := s.pool.Exec(ctx, `INSERT INTO purchase_requests(id,user_id,idempotency_key,provider_id,country_code,service_code,quality_tier,duration,max_price,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'provisioning') ON CONFLICT(user_id,idempotency_key) DO NOTHING`, r.ID, r.UserID, r.IdempotencyKey, r.ProviderID, r.CountryCode, r.ServiceCode, r.QualityTier, r.Duration, r.MaxPrice)
	if err != nil {
		return PurchaseRecord{}, false, err
	}
	if ct.RowsAffected() == 1 {
		r.Status = "provisioning"
		return r, true, nil
	}
	var existing PurchaseRecord
	err = s.pool.QueryRow(ctx, `SELECT id,user_id,idempotency_key,provider_id,country_code,service_code,quality_tier,duration,max_price::float8,status,COALESCE(order_id::text,''),COALESCE(error_code,''),created_at,updated_at FROM purchase_requests WHERE user_id=$1 AND idempotency_key=$2`, r.UserID, r.IdempotencyKey).Scan(&existing.ID, &existing.UserID, &existing.IdempotencyKey, &existing.ProviderID, &existing.CountryCode, &existing.ServiceCode, &existing.QualityTier, &existing.Duration, &existing.MaxPrice, &existing.Status, &existing.OrderID, &existing.ErrorCode, &existing.CreatedAt, &existing.UpdatedAt)
	return existing, false, err
}

const listPurchaseRequestsSQL = `SELECT
    pr.id,pr.user_id,pr.idempotency_key,pr.provider_id,
    pr.country_code,
    COALESCE((
        SELECT pc.name
        FROM provider_catalog AS pc
        WHERE pc.provider_id = pr.provider_id
          AND pc.kind = 'country'
          AND pc.code = pr.country_code
          AND NULLIF(BTRIM(pc.name), '') IS NOT NULL
        ORDER BY (pc.country = '') DESC,pc.updated_at DESC,pc.country,pc.name
        LIMIT 1
    ),''),
    pr.service_code,
    COALESCE((
        SELECT pc.name
        FROM provider_catalog AS pc
        WHERE pc.provider_id = pr.provider_id
          AND pc.kind = 'service'
          AND pc.code = pr.service_code
          AND pc.country IN (pr.country_code,'')
          AND NULLIF(BTRIM(pc.name), '') IS NOT NULL
        ORDER BY (pc.country = pr.country_code) DESC,pc.updated_at DESC,pc.country,pc.name
        LIMIT 1
    ),''),
    pr.quality_tier,pr.duration,pr.max_price::float8,pr.status,
    COALESCE(pr.order_id::text,''),COALESCE(pr.error_code,''),pr.created_at,pr.updated_at
FROM purchase_requests AS pr
WHERE pr.user_id=$1
ORDER BY pr.created_at DESC,pr.id DESC
LIMIT $2`

func (s *Postgres) ListPurchaseRequests(ctx context.Context, userID string, limit int) ([]PurchaseRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, listPurchaseRequestsSQL, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]PurchaseRecord, 0, limit)
	for rows.Next() {
		var record PurchaseRecord
		if err = rows.Scan(&record.ID, &record.UserID, &record.IdempotencyKey, &record.ProviderID, &record.CountryCode, &record.CountryName, &record.ServiceCode, &record.ServiceName, &record.QualityTier, &record.Duration, &record.MaxPrice, &record.Status, &record.OrderID, &record.ErrorCode, &record.CreatedAt, &record.UpdatedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Postgres) CompletePurchase(ctx context.Context, id string, o domain.Order) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, insertOrderSQL, orderInsertArgs(o)...); err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, `UPDATE purchase_requests SET status='succeeded',order_id=$2,updated_at=now() WHERE id=$1 AND status='provisioning'`, id, o.ID)
	if err != nil || ct.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return err
	}
	return tx.Commit(ctx)
}
func (s *Postgres) FailPurchase(ctx context.Context, id, status, code string) error {
	if status != "unknown" && status != "failed" {
		status = "unknown"
	}
	_, err := s.pool.Exec(ctx, `UPDATE purchase_requests SET status=$2,error_code=$3,updated_at=now() WHERE id=$1 AND status='provisioning'`, id, status, code)
	return err
}

const orderCols = `o.id,o.user_id,o.provider_id,o.upstream_id,o.phone_number,o.country_code,COALESCE(o.country_name,''),o.service_code,COALESCE(o.service_name,''),o.quality_tier,o.duration,o.status,o.cost::float8,o.currency,o.can_get_another_sms,o.poll_sequence,o.last_provider_state,o.next_poll_at,o.poll_failures,o.request_next_pending,o.request_next_inflight,o.request_next_inflight_at,o.request_next_generation,o.request_next_claim_generation,o.request_next_failures,COALESCE(o.renewal_request_id::text,''),o.renewal_inflight,o.renewal_inflight_at,o.renewal_mode,o.renewal_value,o.renewal_unit,o.renewal_quoted_price::float8,o.renewal_baseline,o.renewal_submitted_at,o.activation_started_at,o.non_refundable,o.expires_at,o.created_at,o.updated_at`

func scanOrder(row pgx.Row) (domain.Order, error) {
	var o domain.Order
	err := row.Scan(&o.ID, &o.UserID, &o.ProviderID, &o.UpstreamID, &o.PhoneNumber, &o.CountryCode, &o.CountryName, &o.ServiceCode, &o.ServiceName, &o.QualityTier, &o.Duration, &o.Status, &o.Cost, &o.Currency, &o.CanGetAnotherSMS, &o.PollSequence, &o.LastProviderState, &o.NextPollAt, &o.PollFailures, &o.RequestNextPending, &o.RequestNextInflight, &o.RequestNextInflightAt, &o.RequestNextGeneration, &o.RequestNextClaimGeneration, &o.RequestNextFailures, &o.RenewalRequestID, &o.RenewalInflight, &o.RenewalInflightAt, &o.RenewalMode, &o.RenewalValue, &o.RenewalUnit, &o.RenewalQuotedPrice, &o.RenewalBaseline, &o.RenewalSubmittedAt, &o.ActivationStartedAt, &o.NonRefundable, &o.ExpiresAt, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return o, err
}
func (s *Postgres) GetOrder(ctx context.Context, id, user string) (domain.Order, error) {
	q := `SELECT ` + orderCols + ` FROM orders o WHERE o.id=$1 AND ($2='' OR o.user_id=NULLIF($2,'')::uuid)`
	o, err := scanOrder(s.pool.QueryRow(ctx, q, id, user))
	if err == nil {
		o.Messages, err = s.messages(ctx, o.ID)
	}
	return o, err
}
func (s *Postgres) FindOrderByUpstream(ctx context.Context, pid, up string) (domain.Order, error) {
	return scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderCols+` FROM orders o WHERE o.provider_id=$1 AND o.upstream_id=$2`, pid, up))
}
func (s *Postgres) ListOrders(ctx context.Context, user string, limit, offset int) ([]domain.Order, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+orderCols+` FROM orders o WHERE ($1='' OR o.user_id=NULLIF($1,'')::uuid) ORDER BY o.created_at DESC LIMIT $2 OFFSET $3`, user, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Order{}
	for rows.Next() {
		o, e := scanOrder(rows)
		if e != nil {
			return nil, e
		}
		o.Messages, e = s.messages(ctx, o.ID)
		if e != nil {
			return nil, e
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (s *Postgres) SearchOrders(ctx context.Context, user, status, pid, keyword string, limit, offset int) ([]domain.Order, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	where := ` WHERE ($1='' OR o.user_id=NULLIF($1,'')::uuid) AND ($2='' OR o.status=$2) AND ($3='' OR o.provider_id=$3) AND ($4='' OR o.phone_number ILIKE '%'||$4||'%' OR o.id::text ILIKE '%'||$4||'%' OR o.upstream_id ILIKE '%'||$4||'%')`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM orders o`+where, user, status, pid, keyword).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+orderCols+` FROM orders o`+where+` ORDER BY o.created_at DESC LIMIT $5 OFFSET $6`, user, status, pid, keyword, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.Order{}
	for rows.Next() {
		o, e := scanOrder(rows)
		if e != nil {
			return nil, 0, e
		}
		o.Messages, e = s.messages(ctx, o.ID)
		if e != nil {
			return nil, 0, e
		}
		out = append(out, o)
	}
	return out, total, rows.Err()
}
func (s *Postgres) WithOrderLock(ctx context.Context, id string, fn func(context.Context) error) error {
	select {
	case s.orderLockSlots <- struct{}{}:
		defer func() { <-s.orderLockSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,91237))`, id).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrConflict
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1,91237))`, id)
	}()
	return fn(ctx)
}
func (s *Postgres) messages(ctx context.Context, id string) ([]domain.SMSMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,order_id,provider_id,code,message_text,source,upstream_fingerprint,received_at,created_at FROM sms_messages WHERE order_id=$1 ORDER BY received_at,created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.SMSMessage{}
	for rows.Next() {
		var m domain.SMSMessage
		if err = rows.Scan(&m.ID, &m.OrderID, &m.ProviderID, &m.Code, &m.Text, &m.Source, &m.UpstreamFingerprint, &m.ReceivedAt, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Postgres) SetOrderStatus(ctx context.Context, id, status, state string) error {
	ct, err := s.pool.Exec(ctx, `UPDATE orders SET status=$2,last_provider_state=$3,updated_at=now() WHERE id=$1 AND status='active'`, id, status, state)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}
func (s *Postgres) ClaimDueOrders(ctx context.Context, limit int, now time.Time, lease time.Duration) ([]domain.Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+orderCols+` FROM orders o WHERE status='active' AND renewal_inflight=false AND next_poll_at<=$1 ORDER BY next_poll_at FOR UPDATE SKIP LOCKED LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	out := []domain.Order{}
	for rows.Next() {
		o, e := scanOrder(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, o)
	}
	rows.Close()
	for _, o := range out {
		if _, err = tx.Exec(ctx, `UPDATE orders SET next_poll_at=$2 WHERE id=$1`, o.ID, now.Add(lease)); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Postgres) UpdatePoll(ctx context.Context, id, state string, next time.Time, fail int) error {
	_, err := s.pool.Exec(ctx, `UPDATE orders SET last_provider_state=$2,next_poll_at=$3,poll_failures=$4,updated_at=now() WHERE id=$1 AND status='active'`, id, state, next, fail)
	return err
}
func (s *Postgres) UpdateOrderExpiresAt(ctx context.Context, id string, expiresAt time.Time) error {
	ct, err := s.pool.Exec(ctx, `UPDATE orders SET expires_at=$2,updated_at=now() WHERE id=$1 AND status='active'`, id, expiresAt)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}

const renewalRecordColumns = `id,user_id,order_id,idempotency_key,provider_id,upstream_id,mode,value,unit,quoted_price::float8,provider_baseline,COALESCE(charged_price::float8,0),result_expires_at,status,COALESCE(error_code,''),submitted_at,created_at,updated_at`

func scanRenewalRecord(row pgx.Row) (RenewalRecord, error) {
	var record RenewalRecord
	err := row.Scan(
		&record.ID, &record.UserID, &record.OrderID, &record.IdempotencyKey,
		&record.ProviderID, &record.UpstreamID, &record.Mode, &record.Value,
		&record.Unit, &record.QuotedPrice, &record.Baseline, &record.ChargedPrice,
		&record.ResultExpiresAt, &record.Status, &record.ErrorCode,
		&record.SubmittedAt, &record.CreatedAt, &record.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return record, err
}

func (s *Postgres) GetRenewalRequest(ctx context.Context, userID, idempotencyKey string) (RenewalRecord, error) {
	return scanRenewalRecord(s.pool.QueryRow(ctx,
		`SELECT `+renewalRecordColumns+` FROM renewal_requests WHERE user_id=$1 AND idempotency_key=$2`,
		userID, idempotencyKey,
	))
}

func (s *Postgres) StartOrderRenewal(ctx context.Context, record RenewalRecord) (RenewalRecord, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RenewalRecord{}, false, err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx, `INSERT INTO renewal_requests(
		id,user_id,order_id,idempotency_key,provider_id,upstream_id,mode,value,unit,quoted_price,provider_baseline,status
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'provisioning')
	ON CONFLICT(user_id,idempotency_key) DO NOTHING`,
		record.ID, record.UserID, record.OrderID, record.IdempotencyKey,
		record.ProviderID, record.UpstreamID, record.Mode, record.Value,
		record.Unit, record.QuotedPrice, record.Baseline,
	)
	if unique(err) {
		return RenewalRecord{}, false, ErrConflict
	}
	if err != nil {
		return RenewalRecord{}, false, err
	}
	if ct.RowsAffected() == 0 {
		existing, getErr := scanRenewalRecord(tx.QueryRow(ctx,
			`SELECT `+renewalRecordColumns+` FROM renewal_requests WHERE user_id=$1 AND idempotency_key=$2`,
			record.UserID, record.IdempotencyKey,
		))
		return existing, false, getErr
	}
	ct, err = tx.Exec(ctx, `UPDATE orders SET
		renewal_request_id=$2,
		renewal_inflight=true,
		renewal_inflight_at=now(),
		renewal_mode=$3,
		renewal_value=$4,
		renewal_unit=$5,
		renewal_quoted_price=$6,
		renewal_baseline=$7,
		renewal_submitted_at=NULL,
		updated_at=now()
	WHERE id=$1 AND renewal_inflight=false`,
		record.OrderID, record.ID, record.Mode, record.Value, record.Unit, record.QuotedPrice, record.Baseline,
	)
	if err != nil {
		return RenewalRecord{}, false, err
	}
	if ct.RowsAffected() != 1 {
		return RenewalRecord{}, false, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return RenewalRecord{}, false, err
	}
	record.Status = "provisioning"
	return record, true, nil
}

func (s *Postgres) MarkOrderRenewalSubmitted(ctx context.Context, requestID, orderID string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx, `UPDATE renewal_requests SET status='unknown',submitted_at=now(),updated_at=now()
		WHERE id=$1 AND order_id=$2 AND status='provisioning'`, requestID, orderID)
	if err != nil || ct.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return false, err
	}
	ct, err = tx.Exec(ctx, `UPDATE orders SET renewal_submitted_at=now(),renewal_inflight_at=now(),updated_at=now()
		WHERE id=$1 AND renewal_request_id=$2 AND renewal_inflight=true AND renewal_submitted_at IS NULL`, orderID, requestID)
	if err != nil || ct.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Postgres) CompleteOrderRenewal(ctx context.Context, requestID, id, upstreamID, phoneNumber, duration string, expiresAt time.Time, totalCost, chargedPrice float64, activationStartedAt time.Time, nonRefundable bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx, `UPDATE orders SET
		upstream_id=$3,
		phone_number=CASE WHEN BTRIM($4)='' THEN phone_number ELSE $4 END,
		duration=$5,
		status='active',
		cost=$7,
		can_get_another_sms=true,
		poll_sequence=CASE WHEN upstream_id<>$3 THEN 0 ELSE poll_sequence END,
		last_provider_state='',
		next_poll_at=now(),
		poll_failures=0,
		request_next_pending=false,
		request_next_inflight=false,
		request_next_inflight_at=NULL,
		request_next_generation=CASE WHEN upstream_id<>$3 THEN 0 ELSE request_next_generation END,
		request_next_claim_generation=CASE WHEN upstream_id<>$3 THEN 0 ELSE request_next_claim_generation END,
		request_next_failures=0,
		expires_at=$6,
		activation_started_at=$8,
		non_refundable=$9,
		renewal_inflight=false,
		renewal_inflight_at=NULL,
		renewal_request_id=NULL,
		renewal_mode='',
		renewal_value=0,
		renewal_unit='',
		renewal_quoted_price=0,
		renewal_baseline='',
		renewal_submitted_at=NULL,
		updated_at=now()
	WHERE id=$2 AND renewal_request_id=$1 AND renewal_inflight=true AND renewal_submitted_at IS NOT NULL`,
		requestID, id, upstreamID, phoneNumber, duration, expiresAt, totalCost, activationStartedAt, nonRefundable)
	if err != nil || ct.RowsAffected() != 1 {
		if unique(err) {
			return ErrConflict
		}
		if err == nil {
			err = ErrConflict
		}
		return err
	}
	ct, err = tx.Exec(ctx, `UPDATE renewal_requests SET
		status='succeeded',charged_price=$2,result_expires_at=$3,error_code='',updated_at=now()
	WHERE id=$1 AND order_id=$4 AND status='unknown'`, requestID, chargedPrice, expiresAt, id)
	if err != nil || ct.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) ReleaseOrderRenewal(ctx context.Context, requestID, orderID, errorCode string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx, `UPDATE orders SET
		renewal_inflight=false,
		renewal_inflight_at=NULL,
		renewal_request_id=NULL,
		renewal_mode='',
		renewal_value=0,
		renewal_unit='',
		renewal_quoted_price=0,
		renewal_baseline='',
		renewal_submitted_at=NULL,
		updated_at=now()
	WHERE id=$1 AND renewal_request_id=$2 AND renewal_inflight=true`, orderID, requestID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return ErrConflict
	}
	ct, err = tx.Exec(ctx, `UPDATE renewal_requests SET status='failed',error_code=$2,updated_at=now()
		WHERE id=$1 AND order_id=$3 AND status IN ('provisioning','unknown')`, requestID, errorCode, orderID)
	if err != nil || ct.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) ClaimDueRenewals(ctx context.Context, limit int, now time.Time, lease time.Duration) ([]domain.Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+orderCols+` FROM orders o
		WHERE renewal_inflight=true AND renewal_submitted_at IS NOT NULL AND renewal_inflight_at<=$1
		ORDER BY renewal_inflight_at FOR UPDATE SKIP LOCKED LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	orders := make([]domain.Order, 0, limit)
	for rows.Next() {
		order, scanErr := scanOrder(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		orders = append(orders, order)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, order := range orders {
		if _, err = tx.Exec(ctx, `UPDATE orders SET renewal_inflight_at=$2 WHERE id=$1`, order.ID, now.Add(lease)); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return orders, nil
}
func (s *Postgres) UpdateRequestNext(ctx context.Context, id string, pending bool, failures int, next time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE orders SET request_next_pending=$2,request_next_failures=$3,next_poll_at=CASE WHEN $2 THEN LEAST(next_poll_at,$4) ELSE next_poll_at END,updated_at=now() WHERE id=$1 AND status='active'`, id, pending, failures, next)
	return err
}
func (s *Postgres) ClaimRequestNext(ctx context.Context, id string) (bool, error) {
	ct, err := s.pool.Exec(ctx, `UPDATE orders SET request_next_pending=false,request_next_inflight=true,request_next_inflight_at=now(),request_next_claim_generation=request_next_generation,updated_at=now() WHERE id=$1 AND status='active' AND request_next_pending=true AND request_next_inflight=false`, id)
	return err == nil && ct.RowsAffected() == 1, err
}
func (s *Postgres) RestoreRequestNext(ctx context.Context, id string, failures int, next time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE orders SET request_next_pending=true,request_next_inflight=false,request_next_inflight_at=NULL,request_next_failures=$2,next_poll_at=LEAST(next_poll_at,$3),updated_at=now() WHERE id=$1 AND status='active' AND request_next_inflight=true`, id, failures, next)
	return err
}
func (s *Postgres) CompleteRequestNext(ctx context.Context, id string, charge float64) (bool, error) {
	ct, err := s.pool.Exec(ctx, `UPDATE orders SET request_next_inflight=false,request_next_inflight_at=NULL,request_next_pending=(request_next_generation>request_next_claim_generation),request_next_failures=0,cost=cost+$2,updated_at=now() WHERE id=$1 AND status='active' AND request_next_inflight=true`, id, charge)
	return err == nil && ct.RowsAffected() == 1, err
}
func (s *Postgres) SaveMessage(ctx context.Context, m domain.SMSMessage, advance bool) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var orderStatus string
	if err = tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1 FOR UPDATE`, m.OrderID).Scan(&orderStatus); err != nil {
		return false, err
	}
	if orderStatus != domain.OrderActive {
		return false, tx.Commit(ctx)
	}
	ct, err := tx.Exec(ctx, `INSERT INTO sms_messages(id,order_id,provider_id,code,message_text,source,upstream_fingerprint,received_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8 WHERE NOT EXISTS(SELECT 1 FROM sms_messages WHERE order_id=$2 AND source<>$6 AND received_at=$8 AND code=$4 AND message_text=$5) ON CONFLICT(order_id,upstream_fingerprint) DO NOTHING`, m.ID, m.OrderID, m.ProviderID, m.Code, m.Text, m.Source, m.UpstreamFingerprint, m.ReceivedAt)
	if err != nil {
		return false, err
	}
	inserted := ct.RowsAffected() == 1
	if inserted && advance {
		_, err = tx.Exec(ctx, `UPDATE orders SET poll_sequence=poll_sequence+1,poll_failures=0,request_next_generation=request_next_generation+CASE WHEN can_get_another_sms THEN 1 ELSE 0 END,request_next_pending=CASE WHEN can_get_another_sms AND request_next_inflight=false THEN true ELSE request_next_pending END,next_poll_at=CASE WHEN can_get_another_sms THEN LEAST(next_poll_at,now()) ELSE next_poll_at END,updated_at=now() WHERE id=$1`, m.OrderID)
		if err != nil {
			return false, err
		}
	}
	return inserted, tx.Commit(ctx)
}
func (s *Postgres) SaveWebhookMessage(ctx context.Context, w WebhookRecord, m domain.SMSMessage) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var orderStatus string
	if err = tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1 FOR UPDATE`, m.OrderID).Scan(&orderStatus); err != nil {
		return false, err
	}
	if orderStatus != domain.OrderActive {
		w.Status = "ignored"
		w.Error = "order_terminal"
	}
	ct, err := tx.Exec(ctx, `INSERT INTO webhook_events(id,provider_id,upstream_id,fingerprint,headers,payload,processing_status,processing_error,received_at,processed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now()) ON CONFLICT(provider_id,fingerprint) DO NOTHING`, w.ID, w.ProviderID, w.UpstreamID, w.Fingerprint, w.Headers, w.Payload, w.Status, w.Error, w.ReceivedAt)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	if orderStatus != domain.OrderActive {
		return false, tx.Commit(ctx)
	}
	ct, err = tx.Exec(ctx, `INSERT INTO sms_messages(id,order_id,provider_id,code,message_text,source,upstream_fingerprint,received_at) SELECT $1,$2,$3,$4,$5,'webhook',$6,$7 WHERE NOT EXISTS(SELECT 1 FROM sms_messages WHERE order_id=$2 AND source<>'webhook' AND received_at=$7 AND code=$4 AND message_text=$5) ON CONFLICT(order_id,upstream_fingerprint) DO NOTHING`, m.ID, m.OrderID, m.ProviderID, m.Code, m.Text, m.UpstreamFingerprint, m.ReceivedAt)
	if err != nil {
		return false, err
	}
	inserted := ct.RowsAffected() == 1
	if inserted {
		_, err = tx.Exec(ctx, `UPDATE orders SET poll_sequence=poll_sequence+1,last_provider_state=$2,poll_failures=0,request_next_generation=request_next_generation+CASE WHEN can_get_another_sms THEN 1 ELSE 0 END,request_next_pending=CASE WHEN can_get_another_sms AND request_next_inflight=false THEN true ELSE request_next_pending END,request_next_failures=0,next_poll_at=LEAST(next_poll_at,now()),updated_at=now() WHERE id=$1 AND status='active'`, m.OrderID, w.ProviderState)
		if err != nil {
			return false, err
		}
	}
	return inserted, tx.Commit(ctx)
}
func (s *Postgres) SaveWebhookEvent(ctx context.Context, w WebhookRecord) (bool, error) {
	ct, err := s.pool.Exec(ctx, `INSERT INTO webhook_events(id,provider_id,upstream_id,fingerprint,headers,payload,processing_status,processing_error,received_at,processed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now()) ON CONFLICT(provider_id,fingerprint) DO NOTHING`, w.ID, w.ProviderID, w.UpstreamID, w.Fingerprint, w.Headers, w.Payload, w.Status, w.Error, w.ReceivedAt)
	return err == nil && ct.RowsAffected() == 1, err
}

const dashboardSQL = `
SELECT
	count(*) FILTER (WHERE status = 'active'),
	count(*) FILTER (WHERE created_at >= date_trunc('day', now())),
	COALESCE(sum(cost) FILTER (
		WHERE created_at >= date_trunc('day', now())
			AND status = 'completed'
	), 0)::float8,
	(
		SELECT count(*)
		FROM sms_messages m
		JOIN orders mo ON mo.id = m.order_id
		WHERE m.created_at >= date_trunc('day', now())
			AND ($1 = '' OR mo.user_id = NULLIF($1, '')::uuid)
	)
FROM orders
WHERE ($1 = '' OR user_id = NULLIF($1, '')::uuid)`

func (s *Postgres) Dashboard(ctx context.Context, user string) (domain.Dashboard, error) {
	var d domain.Dashboard
	err := s.pool.QueryRow(ctx, dashboardSQL, user).Scan(&d.ActiveOrders, &d.TodayOrders, &d.TodayCost, &d.TodaySMS)
	return d, err
}
func (s *Postgres) Audit(ctx context.Context, user *string, action, typ, target, ip string, meta json.RawMessage) error {
	if len(meta) == 0 {
		meta = []byte(`{}`)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_logs(user_id,action,target_type,target_id,ip,metadata) VALUES($1,$2,$3,$4,NULLIF($5,'')::inet,$6)`, user, action, typ, target, ip, meta)
	return err
}

type maintenanceStatement struct {
	query  string
	cutoff time.Time
}

func maintenanceStatements(now time.Time) []maintenanceStatement {
	return []maintenanceStatement{
		{`WITH stale AS (UPDATE renewal_requests SET status='failed',error_code='abandoned_before_submit',updated_at=now() WHERE status='provisioning' AND submitted_at IS NULL AND updated_at<$1 RETURNING id) UPDATE orders SET renewal_request_id=NULL,renewal_inflight=false,renewal_inflight_at=NULL,renewal_mode='',renewal_value=0,renewal_unit='',renewal_quoted_price=0,renewal_baseline='',renewal_submitted_at=NULL,updated_at=now() WHERE renewal_request_id IN (SELECT id FROM stale)`, now.Add(-2 * time.Minute)},
		{`DELETE FROM captcha_challenges WHERE expires_at<$1`, now.Add(-time.Hour)},
		{`DELETE FROM captcha_issuances WHERE issued_at<$1`, now.Add(-24 * time.Hour)},
		{`DELETE FROM auth_sessions WHERE expires_at<$1 OR revoked_at<$1`, now.Add(-7 * 24 * time.Hour)},
		{`DELETE FROM login_attempts WHERE attempted_at<$1`, now.Add(-7 * 24 * time.Hour)},
		{`DELETE FROM webhook_events WHERE received_at<$1`, now.Add(-90 * 24 * time.Hour)},
	}
}

func (s *Postgres) Maintenance(ctx context.Context, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, statement := range maintenanceStatements(now) {
		if _, err = tx.Exec(ctx, statement.query, statement.cutoff); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func unique(err error) bool { var e *pgconn.PgError; return errors.As(err, &e) && e.Code == "23505" }
