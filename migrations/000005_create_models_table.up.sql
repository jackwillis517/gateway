CREATE TABLE IF NOT EXISTS models (
    id bigserial PRIMARY KEY,
    name text NOT NULL UNIQUE,   -- 'anthropic/<model-id>'
    alias text UNIQUE,            -- optional short name
    provider text NOT NULL CHECK (
        provider IN ('anthropic', 'openai', 'ollama')
    ),
    upstream_model text NOT NULL,          -- id sent to the provider
    -- USD per 1M tokens
    input_price_per_mtok numeric(12, 6) NOT NULL DEFAULT 0,
    output_price_per_mtok numeric(12, 6) NOT NULL DEFAULT 0,
    cache_read_price_per_mtok numeric(12, 6) NOT NULL DEFAULT 0,
    cache_write_price_per_mtok numeric(12, 6) NOT NULL DEFAULT 0,
    max_output_tokens integer NOT NULL DEFAULT 4096,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    version integer NOT NULL DEFAULT 1
);
