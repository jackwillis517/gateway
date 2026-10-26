CREATE TABLE IF NOT EXISTS api_keys (
    id bigserial PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users ON DELETE CASCADE,
    name text NOT NULL,                 -- 'laptop', 'ci'
    key_prefix text NOT NULL,                 -- first 10 chars, for display
    hash bytea NOT NULL UNIQUE,         -- sha256(plaintext)
    scopes text[] NOT NULL DEFAULT '{chat}',
    -- NULL = flag default
    rate_limit_rps numeric(8, 3) CHECK (rate_limit_rps > 0),
    rate_limit_burst integer CHECK (rate_limit_burst > 0),
    -- optional TPM limit
    tokens_per_minute integer CHECK (tokens_per_minute > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    expires_at timestamptz,                   -- NULL = never
    revoked_at timestamptz                    -- NULL = active
);
CREATE INDEX IF NOT EXISTS api_keys_user_id_idx ON api_keys (user_id);
