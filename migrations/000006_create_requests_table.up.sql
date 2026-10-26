CREATE TABLE IF NOT EXISTS requests (
    id bigserial PRIMARY KEY,
    -- 'chatcmpl-<ulid>', also X-Request-Id
    request_id text NOT NULL UNIQUE,
    -- request start, Grafana time column
    created_at timestamptz NOT NULL DEFAULT now(),
    usage_date date GENERATED ALWAYS AS (
        (created_at AT TIME ZONE 'UTC')::date
    ) STORED,
    user_id bigint NOT NULL REFERENCES users,
    api_key_id bigint REFERENCES api_keys ON DELETE SET NULL,
    -- as requested; NULL when rejected before parsing (429)
    model text,
    provider text,
    upstream_model text,
    status_code smallint NOT NULL,   -- 200, 400, 429, 499, 502, 504 ...
    -- 'rate_limit_exceeded', 'upstream_timeout' ...
    error_code text,
    streamed boolean NOT NULL DEFAULT false,
    finish_reason text,                -- stop, length
    input_tokens integer NOT NULL DEFAULT 0,
    output_tokens integer NOT NULL DEFAULT 0,
    cache_read_tokens integer NOT NULL DEFAULT 0,
    cache_write_tokens integer NOT NULL DEFAULT 0,
    cost_usd numeric(12, 6) NOT NULL DEFAULT 0,
    latency_ms integer NOT NULL,    -- total time inside the gateway
    -- time spent waiting on the provider
    upstream_latency_ms integer,
    ttft_ms integer,             -- time to first token, streaming only
    retry_count smallint NOT NULL DEFAULT 0,       -- Nov 4
    fallback_used boolean NOT NULL DEFAULT false,    -- Nov 4
    cache_hit boolean NOT NULL DEFAULT false,    -- Nov 11
    -- vcs.Version() build string (LGF Ch. 20)
    gateway_version text NOT NULL,
    prompt_version text                 -- W4 prompt registry, NULL until then
);

CREATE INDEX IF NOT EXISTS requests_created_at_idx ON requests (created_at);
CREATE INDEX IF NOT EXISTS requests_user_created_at_idx ON requests (
    user_id, created_at
);
CREATE INDEX IF NOT EXISTS requests_model_created_at_idx ON requests (
    model, created_at
);

CREATE OR REPLACE VIEW daily_usage AS
SELECT
    usage_date,
    user_id,
    model,
    count(*) AS requests,
    count(*) FILTER (WHERE status_code >= 400) AS errors,
    sum(input_tokens) AS input_tokens,
    sum(output_tokens) AS output_tokens,
    sum(cost_usd) AS cost_usd
FROM requests
GROUP BY usage_date, user_id, model;
