CREATE TABLE IF NOT EXISTS users (
    id uuid PRIMARY KEY,
    username text NOT NULL,
    username_normalized text NOT NULL UNIQUE,
    display_name text NOT NULL DEFAULT '',
    password_hash text NOT NULL,
    role text NOT NULL DEFAULT 'operator' CHECK (role IN ('admin','operator')),
    active boolean NOT NULL DEFAULT true,
    last_login_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS auth_sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    ip inet,
    user_agent text NOT NULL DEFAULT '',
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS auth_sessions_lookup ON auth_sessions(token_hash) WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS captcha_challenges (
    id uuid PRIMARY KEY,
    code_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS captcha_issuances (
    id bigserial PRIMARY KEY,
    ip inet NOT NULL,
    issued_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS captcha_issuances_rate ON captcha_issuances(ip,issued_at DESC);

CREATE TABLE IF NOT EXISTS login_attempts (
    id bigserial PRIMARY KEY,
    ip inet NOT NULL,
    username_normalized text NOT NULL DEFAULT '',
    success boolean NOT NULL,
    attempted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS login_attempts_rate ON login_attempts(ip, attempted_at DESC) WHERE success = false;

CREATE TABLE IF NOT EXISTS providers (
    id text PRIMARY KEY,
    name text NOT NULL,
    base_url text NOT NULL,
    api_key_cipher bytea,
    enabled boolean NOT NULL DEFAULT false,
    webhook_token_cipher bytea,
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS provider_catalog (
    provider_id text NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('country','service','price')),
    code text NOT NULL,
    country text NOT NULL DEFAULT '',
    name text NOT NULL,
    price numeric(18,6),
    stock integer,
    raw jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(provider_id, kind, code, country)
);

CREATE TABLE IF NOT EXISTS orders (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    provider_id text NOT NULL REFERENCES providers(id),
    upstream_id text NOT NULL,
    phone_number text NOT NULL,
    country_code text NOT NULL,
    country_name text,
    service_code text NOT NULL,
    service_name text,
    quality_tier text NOT NULL DEFAULT '',
    status text NOT NULL CHECK (status IN ('active','completed','canceled','expired')),
    cost numeric(18,6) NOT NULL DEFAULT 0,
    currency text NOT NULL DEFAULT 'USD',
    can_get_another_sms boolean NOT NULL DEFAULT true,
    poll_sequence bigint NOT NULL DEFAULT 0,
    last_provider_state text NOT NULL DEFAULT '',
    next_poll_at timestamptz NOT NULL DEFAULT now(),
    poll_failures integer NOT NULL DEFAULT 0,
    request_next_pending boolean NOT NULL DEFAULT false,
    request_next_inflight boolean NOT NULL DEFAULT false,
    request_next_inflight_at timestamptz,
    request_next_generation bigint NOT NULL DEFAULT 0,
    request_next_claim_generation bigint NOT NULL DEFAULT 0,
    request_next_failures integer NOT NULL DEFAULT 0,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(provider_id, upstream_id)
);
ALTER TABLE orders ADD COLUMN IF NOT EXISTS country_name text;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS service_name text;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS request_next_inflight boolean NOT NULL DEFAULT false;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS request_next_inflight_at timestamptz;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS request_next_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS request_next_claim_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS quality_tier text NOT NULL DEFAULT '';
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'orders_quality_tier_check'
          AND conrelid = 'orders'::regclass
    ) THEN
        ALTER TABLE orders ADD CONSTRAINT orders_quality_tier_check
            CHECK (quality_tier IN ('','gold','silver','bronze'));
    END IF;
END $$;
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
WHERE (
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
    );
CREATE INDEX IF NOT EXISTS orders_poll_due ON orders(next_poll_at) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS orders_user_created ON orders(user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS purchase_requests (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    idempotency_key text NOT NULL,
    provider_id text NOT NULL REFERENCES providers(id),
    country_code text NOT NULL,
    service_code text NOT NULL,
    quality_tier text NOT NULL DEFAULT '',
    max_price numeric(18,6) NOT NULL,
    status text NOT NULL CHECK(status IN ('provisioning','succeeded','unknown','failed')),
    order_id uuid REFERENCES orders(id),
    error_code text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(user_id,idempotency_key)
);
ALTER TABLE purchase_requests ADD COLUMN IF NOT EXISTS quality_tier text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS purchase_requests_user_created ON purchase_requests(user_id, created_at DESC);
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'purchase_requests_quality_tier_check'
          AND conrelid = 'purchase_requests'::regclass
    ) THEN
        ALTER TABLE purchase_requests ADD CONSTRAINT purchase_requests_quality_tier_check
            CHECK (quality_tier IN ('','gold','silver','bronze'));
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS sms_messages (
    id uuid PRIMARY KEY,
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    provider_id text NOT NULL REFERENCES providers(id),
    code text NOT NULL DEFAULT '',
    message_text text NOT NULL DEFAULT '',
    source text NOT NULL CHECK (source IN ('webhook','poll')),
    upstream_fingerprint text NOT NULL,
    received_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(order_id, upstream_fingerprint)
);
CREATE INDEX IF NOT EXISTS sms_messages_order_received ON sms_messages(order_id, received_at);

CREATE TABLE IF NOT EXISTS webhook_events (
    id uuid PRIMARY KEY,
    provider_id text NOT NULL REFERENCES providers(id),
    upstream_id text NOT NULL DEFAULT '',
    fingerprint text NOT NULL,
    headers jsonb NOT NULL DEFAULT '{}'::jsonb,
    payload jsonb NOT NULL,
    processing_status text NOT NULL,
    processing_error text NOT NULL DEFAULT '',
    received_at timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    UNIQUE(provider_id, fingerprint)
);

CREATE TABLE IF NOT EXISTS audit_logs (
    id bigserial PRIMARY KEY,
    user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    action text NOT NULL,
    target_type text NOT NULL DEFAULT '',
    target_id text NOT NULL DEFAULT '',
    ip inet,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_logs_created ON audit_logs(created_at DESC);
