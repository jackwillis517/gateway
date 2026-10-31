## Overview and scope

The gateway is a single-user, OpenAI-compatible Go API that proxies `/v1/chat/completions` to OpenAI and Ollama (both OpenAI-compatible, so one client serves both), authenticates every call with an API key, rate-limits per key with `golang.org/x/time/rate`, and writes one usage row per request to Postgres for Grafana.

**Assumptions**

- One user (me), seeded by a migration. There is no signup, password or login flow; I am treated as already authenticated and only need an API key.
- Requests authenticate with `Authorization: Bearer <api key>` only.
- Plain PostgreSQL, run and migrated the way Let's Go Further does it (`database/sql`, `lib/pq`, `golang-migrate`). This replaces the plan's "Cloud SQL or Neon" note for Oct 29.
- Cloud Run deployment is for practice. It is not offered to other people, but nothing in the schema blocks adding users later.

**In scope for W1:** chat completions (non-streaming and SSE streaming), API-key auth, per-key rate limits, usage and cost metering, a Grafana dashboard, deploy to Cloud Run.

**Not in scope:** user signup, billing, tool/function calling passthrough, embeddings, image inputs, multiple choices (`n > 1`). Later W1 days add retries and fallback (Nov 4), response cache (Nov 11), `GET /v1/usage` (Nov 13) and key-management endpoints (Nov 18); the schema below already has the columns they need.

## Architecture and layout

The app follows the Let's Go Further shape: one `application` struct holding config, logger, `data.Models` and the provider registry, with `httprouter` routes wrapped in middleware.
![[Pasted image 20261023175115.png]]
The response streams back along the same path; the usage row is written in a background goroutine after the response finishes, so it never adds latency.

**Project layout**

gateway/
├── cmd/api/
│ ├── main.go config flags, openDB(), app wiring
│ ├── server.go http.Server + graceful shutdown (LGF Ch. 12)
│ ├── routes.go httprouter routes + middleware chain
│ ├── middleware.go recoverPanic, authenticate, requireAuthenticatedUser, rateLimit
│ ├── chat.go createChatCompletionHandler (JSON + SSE)
│ ├── llm_models.go listModelsHandler (GET /v1/models)
│ ├── usage.go showUsageHandler (GET /v1/usage)
│ ├── api_keys.go createAPIKeyHandler, revokeAPIKeyHandler (/v1/admin/api-keys)
│ ├── healthcheck.go healthcheckHandler
│ ├── errors.go OpenAI-style error responses
│ ├── helpers.go readJSON, writeJSON, background()
│ └── context.go contextSetUser / contextGetUser / contextGetAPIKey
├── cmd/keygen/ CLI: create an API key for the seeded user, print it once
├── internal/data/ models.go, users.go, api_keys.go, llm_models.go, requests.go
├── internal/provider/ provider.go (interface), openai.go, ollama.go, types.go
├── internal/validator/ copied from LGF
├── internal/vcs/ build version (LGF Ch. 20)
├── migrations/
├── Dockerfile
└── Makefile
jj
**Middleware order**

```go
return app.recoverPanic(app.authenticate(app.rateLimit(router)))
```

Let's Go Further runs `rateLimit` before `authenticate` because it limits by IP. Here limits are per API key, so `authenticate` must run first. The chat route is also wrapped in `requireAuthenticatedUser`; `/v1/healthcheck` stays open.

**Config (flags, LGF style)**

|                                                                   |                   |                                                             |
| ----------------------------------------------------------------- | ----------------- | ----------------------------------------------------------- |
| Flag                                                              | Default           | Purpose                                                     |
| `-port`                                                           | 4000              | Listen port (Cloud Run sets `PORT`; read it as the default) |
| `-env`                                                            | development       | development / production                                    |
| `-db-dsn`                                                         | `$GATEWAY_DB_DSN` | Postgres DSN                                                |
| `-db-max-open-conns` / `-db-max-idle-conns` / `-db-max-idle-time` | 25 / 25 / 15m     | Pool settings from LGF                                      |
| `-limiter-enabled`                                                | true              | Turn rate limiting off for local tests                      |
| `-limiter-rps` / `-limiter-burst`                                 | 1 / 5             | Defaults when a key has no override                         |
| `-upstream-timeout`                                               | 120s              | Max time for one provider call                              |
| `-version`                                                        | false             | Print version and exit                                      |

Provider keys come only from environment variables (`OPENAI_API_KEY`, `OLLAMA_BASE_URL`), never flags, so they do not show in process lists. On Cloud Run they come from Secret Manager (Nov 6).

## Endpoints and request shapes

`POST /v1/chat/completions` accepts the OpenAI request body and returns the OpenAI response body, so the official OpenAI SDKs work by changing only `base_url` and `api_key`.

|               |                        |                      |                                         |
| ------------- | ---------------------- | -------------------- | --------------------------------------- |
| Method        | Path                   | Auth                 | Built on                                |
| GET           | `/v1/healthcheck`      | none                 | Oct 24                                  |
| POST          | `/v1/chat/completions` | API key              | Oct 28                                  |
| GET           | `/v1/models`           | API key              | Oct 30 (lists enabled rows of `models`) |
| GET           | `/v1/usage`            | API key              | Nov 13                                  |
| POST / DELETE | `/v1/admin/api-keys`   | API key, admin scope | Nov 18                                  |

### Request

```json
{
  "model": "openai/<model-id>",
  "messages": [
    { "role": "system", "content": "You are terse." },
    { "role": "user", "content": "Explain SSE in one sentence." }
  ],
  "max_tokens": 512,
  "temperature": 0.7,
  "top_p": 1,
  "stop": ["\n\n"],
  "stream": false,
  "stream_options": { "include_usage": true },
  "user": "optional-end-user-tag"
}
```

**Model naming:** `<provider>/<model-id>`. The prefix picks the provider; the rest is sent upstream unchanged. A bare name is looked up in the `models` table's `alias` column so clients can send short names. Unknown or disabled models return 404 `model_not_found`.

**Validation (internal/validator)**

- `model` required, `messages` required with 1 to 500 items.
- `role` in system, user, assistant. `content` is a non-empty string (content arrays are out of scope; reject with 400).
- `max_tokens` 1 to the model's `max_output_tokens`; default 1024 when absent.
- `temperature` 0 to 2, `top_p` 0 to 1, `stop` up to 4 strings.
- `n` other than 1, `tools`, `functions`, `response_format` → 400 `unsupported_parameter`.

**readJSON change from LGF:** keep `http.MaxBytesReader` (raise to 4 MB) and the detailed error mapping, but do **not** call `DisallowUnknownFields`. OpenAI clients send fields such as `presence_penalty` or `seed`; unknown fields are ignored instead of rejected.

### Response (non-streaming)

```json
{
  "id": "chatcmpl-01JB...",
  "object": "chat.completion",
  "created": 1761256800,
  "model": "openai/<model-id>",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Server-Sent Events is ..."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": { "prompt_tokens": 31, "completion_tokens": 18, "total_tokens": 49 }
}
```

`id` is the gateway's request ID (also the `requests.request_id` column and the `X-Request-Id` response header), not the provider's ID.

### Response (stream: true)

Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`. Each chunk is one `data:` line followed by a blank line, flushed with `http.Flusher`:

```
data: {"id":"chatcmpl-01JB...","object":"chat.completion.chunk","created":1761256800,"model":"openai/<model-id>","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-01JB...","object":"chat.completion.chunk",...,"choices":[{"index":0,"delta":{"content":"Server-Sent"},"finish_reason":null}]}

data: {"id":"chatcmpl-01JB...","object":"chat.completion.chunk",...,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-01JB...","object":"chat.completion.chunk",...,"choices":[],"usage":{"prompt_tokens":31,"completion_tokens":18,"total_tokens":49}}

data: [DONE]
```

The usage chunk is sent only when `stream_options.include_usage` is true, but the gateway always records usage either way.

**Streaming gotcha:** LGF's `http.Server` has `WriteTimeout: 10s`, which cuts long streams. Keep it for other routes and extend it per request in the chat handler with `http.NewResponseController(w).SetWriteDeadline(time.Now().Add(app.config.upstreamTimeout))`. If the client disconnects (`r.Context().Done()`), cancel the upstream call and record the row with `status = 499`.

### Errors

Errors use the OpenAI envelope instead of LGF's `{"error": "..."}` so SDKs parse them. `errors.go` keeps LGF's helper names (`badRequestResponse`, `rateLimitExceededResponse`, `invalidAuthenticationTokenResponse`, ...) and only changes the body:

```json
{
  "error": {
    "message": "Rate limit exceeded, retry in 2s",
    "type": "rate_limit_error",
    "code": "rate_limit_exceeded",
    "param": null
  }
}
```

|        |                                           |                                                     |
| ------ | ----------------------------------------- | --------------------------------------------------- |
| Status | type                                      | When                                                |
| 400    | invalid_request_error                     | bad JSON, failed validation, unsupported parameter  |
| 401    | authentication_error                      | missing, malformed, unknown, revoked or expired key |
| 404    | invalid_request_error (`model_not_found`) | unknown or disabled model                           |
| 429    | rate_limit_error                          | per-key limiter denied the request                  |
| 502    | api_error (`upstream_error`)              | provider returned 5xx or bad data                   |
| 504    | api_error (`upstream_timeout`)            | provider exceeded `-upstream-timeout`               |
| 500    | api_error                                 | anything else (`serverErrorResponse`)               |

A provider 429 is passed through as 429 with `code: upstream_rate_limited` so it is not confused with the gateway's own limit.

## API key authentication

API keys reuse LGF's stateful-token pattern (Ch. 15 to 16): a random plaintext shown once, only its SHA-256 hash stored, looked up on every request with a join to `users`.

**Key format:** `gw_` + base32 (no padding) of 32 bytes from `crypto/rand`, 55 characters in total. The `gw_` prefix makes keys easy to spot in logs and secret scanners. The first 10 characters are stored as `key_prefix` so keys can be told apart in Grafana and listings without exposing them.

```go
// internal/data/api_keys.go
type APIKey struct {
    Plaintext      string    `json:"key,omitempty"`  // only set at creation
    Hash           []byte    `json:"-"`
    ID             int64     `json:"id"`
    UserID         int64     `json:"-"`
    Name           string    `json:"name"`
    Prefix         string    `json:"prefix"`
    Scopes         []string  `json:"scopes"`
    RateLimitRPS   *float64  `json:"rate_limit_rps"`
    RateLimitBurst *int      `json:"rate_limit_burst"`
    ExpiresAt      *time.Time `json:"expires_at"`
}

func generateAPIKey(userID int64, name string, scopes []string) (*APIKey, error)
func (m APIKeyModel) Insert(key *APIKey) error
func (m APIKeyModel) GetForPlaintext(plaintext string) (*User, *APIKey, error)
func (m APIKeyModel) Touch(id int64) error   // last_used_at, at most once a minute
func (m APIKeyModel) Revoke(id, userID int64) error
```

**authenticate middleware**

1. Add `Vary: Authorization`.
2. No `Authorization` header → `contextSetUser(r, data.AnonymousUser)` and continue (healthcheck works without a key).
3. Header not `Bearer <key>`, or key fails the format check (prefix + length) → 401 with `WWW-Authenticate: Bearer`.
4. `GetForPlaintext` hashes the key and runs one query: key matches, `revoked_at IS NULL`, `expires_at` null or in the future, user `activated`. Not found → 401 `invalid_api_key`.
5. Put the user and the key in the request context (`contextSetUser`, `contextSetAPIKey`).
6. Call `Touch` in `app.background()` only if `last_used_at` is older than a minute, so a busy key does not write on every request.

`requireAuthenticatedUser` wraps the chat, models and usage routes. `requireScope("admin", ...)` (LGF's `requirePermission`, renamed) guards the Nov 18 admin endpoints.

**The single user:** a migration seeds one row in `users` (me). There is no signup, password or activation flow. Keys are created with a small CLI that talks to the database directly:

```
go run ./cmd/keygen -db-dsn=$GATEWAY_DB_DSN -name=laptop -scopes=chat,admin
# prints: gw_Q3K7... (shown once, store it in your password manager)
```

Testing: `curl -H "Authorization: Bearer $GW_KEY" localhost:4000/v1/chat/completions -d @req.json`, and the OpenAI Python SDK with `base_url="http://localhost:4000/v1"`.

## Rate limiting

Each API key gets its own `rate.Limiter` token bucket (default 1 request/second refill, burst 5), kept in an in-memory map exactly like LGF Ch. 11's per-IP limiter, but keyed by `api_keys.id`.

```go
// cmd/api/middleware.go
func (app *application) rateLimit(next http.Handler) http.Handler {
    type client struct {
        limiter  *rate.Limiter
        lastSeen time.Time
    }
    var (
        mu      sync.Mutex
        clients = make(map[int64]*client)
    )

    // LGF cleanup goroutine: drop limiters idle for 3 minutes, every minute.
    go func() { /* same as LGF, keyed by int64 */ }()

    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !app.config.limiter.enabled {
            next.ServeHTTP(w, r); return
        }
        key := app.contextGetAPIKey(r)
        if key == nil { // anonymous: only /v1/healthcheck reaches here
            next.ServeHTTP(w, r); return
        }
        rps, burst := app.limitsFor(key) // key override, else flag defaults

        mu.Lock()
        c, ok := clients[key.ID]
        if !ok {
            c = &client{limiter: rate.NewLimiter(rate.Limit(rps), burst)}
            clients[key.ID] = c
        }
        c.lastSeen = time.Now()
        if c.limiter.Limit() != rate.Limit(rps) { c.limiter.SetLimit(rate.Limit(rps)) }
        if c.limiter.Burst() != burst { c.limiter.SetBurst(burst) }
        res := c.limiter.Reserve()
        remaining := int(c.limiter.Tokens())
        mu.Unlock()

        w.Header().Set("X-RateLimit-Limit-Requests", strconv.Itoa(burst))
        w.Header().Set("X-RateLimit-Remaining-Requests", strconv.Itoa(max(remaining, 0)))

        if d := res.Delay(); d > 0 {
            res.Cancel() // give the token back; we are rejecting, not waiting
            w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
            app.recordRejected(r, http.StatusTooManyRequests) // usage row, 0 tokens
            app.rateLimitExceededResponse(w, r)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

**Why `Reserve()` instead of LGF's `Allow()`:** `Allow()` only says yes or no. `Reserve().Delay()` also tells how long until a token is free, which fills the `Retry-After` header that OpenAI SDKs use for their own backoff. `Cancel()` returns the token so a rejected call does not cost one.

**Limits come from the key row.** `api_keys.rate_limit_rps` and `rate_limit_burst` override the `-limiter-rps` / `-limiter-burst` flags when set. `SetLimit` / `SetBurst` apply a changed value without restarting the server.

**Rejected calls are recorded.** A 429 writes a `requests` row with `status = 429`, zero tokens and zero cost, so Grafana can chart how often the limit fires.

**Cloud Run caveat:** the map lives in one process. With several instances each enforces its own bucket, so the real limit becomes limit × instances. Deploy with `--max-instances=1` for W1; a shared limiter (Postgres or Redis) is only needed if other people ever use it.

**Optional, if time allows: tokens per minute.** A second `rate.Limiter` per key with `rate.Limit(tpm/60)` and burst `tpm` (`api_keys.tokens_per_minute`). Before the call, reject if `limiter.Tokens() <= 0`; after the call, debit the real total with `limiter.ReserveN(time.Now(), min(total, tpm))`, which may push the bucket into debt and delay the next request. This caps spend, not just request count.

## Postgres schema

Four tables: `users` (just me), `api_keys`, `models` (catalog and prices), and `requests` (one usage row per call). Migrations use `golang-migrate` with LGF's numbered up/down files.

|                                  |                                         |
| -------------------------------- | --------------------------------------- |
| Migration                        | Creates                                 |
| `000001_create_citext_extension` | `citext` for case-insensitive email     |
| `000002_create_users_table`      | `users`                                 |
| `000003_seed_user`               | the single user row (me)                |
| `000004_create_api_keys_table`   | `api_keys`                              |
| `000005_create_models_table`     | `models` + seed rows                    |
| `000006_create_requests_table`   | `requests`, indexes, `daily_usage` view |

### users

```sql
CREATE TABLE IF NOT EXISTS users (
    id         bigserial PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    name       text NOT NULL,
    email      citext UNIQUE NOT NULL,
    activated  boolean NOT NULL DEFAULT true,
    version    integer NOT NULL DEFAULT 1   -- LGF optimistic concurrency
);

-- 000003_seed_user.up.sql
INSERT INTO users (name, email) VALUES ('Me', 'me@example.com')
ON CONFLICT (email) DO NOTHING;
```

No `password_hash` column: I am assumed authenticated, and API keys are the only credential.

### api_keys

```sql
CREATE TABLE IF NOT EXISTS api_keys (
    id                bigserial PRIMARY KEY,
    user_id           bigint NOT NULL REFERENCES users ON DELETE CASCADE,
    name              text NOT NULL,                 -- 'laptop', 'ci'
    key_prefix        text NOT NULL,                 -- first 10 chars, for display
    hash              bytea NOT NULL UNIQUE,         -- sha256(plaintext)
    scopes            text[] NOT NULL DEFAULT '{chat}',
    rate_limit_rps    numeric(8,3) CHECK (rate_limit_rps > 0),     -- NULL = flag default
    rate_limit_burst  integer CHECK (rate_limit_burst > 0),
    tokens_per_minute integer CHECK (tokens_per_minute > 0),     -- optional TPM limit
    created_at        timestamptz NOT NULL DEFAULT now(),
    last_used_at      timestamptz,
    expires_at        timestamptz,                   -- NULL = never
    revoked_at        timestamptz                    -- NULL = active
);
CREATE INDEX IF NOT EXISTS api_keys_user_id_idx ON api_keys (user_id);
```

Unlike LGF's `tokens` table, the hash is not the primary key: `requests` needs a small stable `id` to reference.

### models

```sql
CREATE TABLE IF NOT EXISTS models (
    id                         bigserial PRIMARY KEY,
    name                       text NOT NULL UNIQUE,   -- 'openai/<model-id>' or 'ollama/<model-id>'
    alias                      text UNIQUE,            -- optional short name
    provider                   text NOT NULL CHECK (provider IN ('openai', 'ollama')),
    upstream_model             text NOT NULL,          -- id sent to the provider
    input_price_per_mtok       numeric(12,6) NOT NULL DEFAULT 0,  -- USD per 1M tokens
    output_price_per_mtok      numeric(12,6) NOT NULL DEFAULT 0,
    cache_read_price_per_mtok  numeric(12,6) NOT NULL DEFAULT 0,
    cache_write_price_per_mtok numeric(12,6) NOT NULL DEFAULT 0,
    max_output_tokens          integer NOT NULL DEFAULT 4096,
    enabled                    boolean NOT NULL DEFAULT true,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    version                    integer NOT NULL DEFAULT 1
);
```

Seed one row per model you use, copying prices from each provider's pricing page on the day you seed. Ollama rows get price 0. Prices live here, not in code, so a price change is an `UPDATE`, not a deploy.

### requests (usage)

The fields you listed (user, model, input tokens, output tokens, cost, latency, version, date) plus the columns the Grafana panels and later W1 days need.

```sql
CREATE TABLE IF NOT EXISTS requests (
    id                  bigserial PRIMARY KEY,
    request_id          text NOT NULL UNIQUE,               -- 'chatcmpl-<ulid>', also X-Request-Id
    created_at          timestamptz NOT NULL DEFAULT now(), -- request start, Grafana time column
    usage_date          date GENERATED ALWAYS AS ((created_at AT TIME ZONE 'UTC')::date) STORED,
    user_id             bigint NOT NULL REFERENCES users,
    api_key_id          bigint REFERENCES api_keys ON DELETE SET NULL,
    model               text,                -- as requested; NULL when rejected before parsing (429)
    provider            text,
    upstream_model      text,
    status_code         smallint NOT NULL,   -- 200, 400, 429, 499, 502, 504 ...
    error_code          text,                -- 'rate_limit_exceeded', 'upstream_timeout' ...
    streamed            boolean NOT NULL DEFAULT false,
    finish_reason       text,                -- stop, length
    input_tokens        integer NOT NULL DEFAULT 0,
    output_tokens       integer NOT NULL DEFAULT 0,
    cache_read_tokens   integer NOT NULL DEFAULT 0,
    cache_write_tokens  integer NOT NULL DEFAULT 0,
    cost_usd            numeric(12,6) NOT NULL DEFAULT 0,
    latency_ms          integer NOT NULL,    -- total time inside the gateway
    upstream_latency_ms integer,             -- time spent waiting on the provider
    ttft_ms             integer,             -- time to first token, streaming only
    retry_count         smallint NOT NULL DEFAULT 0,       -- Nov 4
    fallback_used       boolean NOT NULL DEFAULT false,    -- Nov 4
    cache_hit           boolean NOT NULL DEFAULT false,    -- Nov 11
    gateway_version     text NOT NULL,       -- vcs.Version() build string (LGF Ch. 20)
    prompt_version      text                 -- W4 prompt registry, NULL until then
);

CREATE INDEX IF NOT EXISTS requests_created_at_idx      ON requests (created_at);
CREATE INDEX IF NOT EXISTS requests_user_created_at_idx ON requests (user_id, created_at);
CREATE INDEX IF NOT EXISTS requests_model_created_at_idx ON requests (model, created_at);

CREATE OR REPLACE VIEW daily_usage AS
SELECT usage_date, user_id, model,
       count(*)                                  AS requests,
       count(*) FILTER (WHERE status_code >= 400) AS errors,
       sum(input_tokens)                         AS input_tokens,
       sum(output_tokens)                        AS output_tokens,
       sum(cost_usd)                             AS cost_usd
FROM requests
GROUP BY usage_date, user_id, model;
```

**Notes**

- `cost_usd` is computed and stored at insert time, so later price changes do not rewrite past costs.
- `usage_date` is the UTC day, derived from `created_at` so the two cannot disagree. Grafana filters on `created_at` and shows it in local time.
- 401s are not stored (there is no user to attach); they go to the structured log only.

### Grafana read-only role

Run once by hand (it holds a password, so it stays out of migrations):

```sql
CREATE ROLE grafana_ro LOGIN PASSWORD '<generate one>';
GRANT CONNECT ON DATABASE gateway TO grafana_ro;
GRANT USAGE ON SCHEMA public TO grafana_ro;
GRANT SELECT ON requests, models, users, daily_usage TO grafana_ro;
GRANT SELECT (id, user_id, name, key_prefix, created_at, last_used_at, revoked_at) ON api_keys TO grafana_ro;  -- never the hash
```

## Usage recording and cost

Every request that passes authentication writes exactly one `requests` row after the response ends, success or failure, with cost computed in SQL from the `models` price columns.

```latex
\text{cost\_usd} = \frac{\text{input} \cdot p_{in} + \text{output} \cdot p_{out} + \text{cache\_read} \cdot p_{cr} + \text{cache\_write} \cdot p_{cw}}{1{,}000{,}000}
```

Prices are USD per million tokens. OpenAI and Ollama report `prompt_tokens`/`completion_tokens`; the cache read/write counts stay 0 until the Nov 11 response cache, so the four price terms add without double counting. A cache hit never reaches the provider and costs 0.

```go
// internal/data/requests.go
type Request struct {
    RequestID         string
    UserID            int64
    APIKeyID          *int64
    Model, Provider, UpstreamModel, ErrorCode, FinishReason *string
    StatusCode        int
    Streamed          bool
    InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int
    LatencyMs         int
    UpstreamLatencyMs *int
    TTFTMs            *int
    RetryCount        int
    FallbackUsed      bool
    CacheHit          bool
    GatewayVersion    string
}

func (m RequestModel) Insert(ctx context.Context, r *Request) error
```

The insert computes cost from the matching `models` row in the same statement (numeric math in Postgres, no float rounding in Go):

```sql
INSERT INTO requests (request_id, user_id, api_key_id, model, provider, upstream_model,
    status_code, error_code, streamed, finish_reason,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
    latency_ms, upstream_latency_ms, ttft_ms, retry_count, fallback_used, cache_hit,
    gateway_version, cost_usd)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
    $15, $16, $17, $18, $19, $20, $21,
    CASE WHEN $20 THEN 0 ELSE COALESCE((
        SELECT ($11 * input_price_per_mtok + $12 * output_price_per_mtok
              + $13 * cache_read_price_per_mtok + $14 * cache_write_price_per_mtok) / 1000000
        FROM models WHERE name = $4), 0) END)
```

**Timing**

|                       |                                                |                                                           |
| --------------------- | ---------------------------------------------- | --------------------------------------------------------- |
| Column                | Start                                          | End                                                       |
| `latency_ms`          | handler entry (`time.Now()` before `readJSON`) | last byte written, or error response sent                 |
| `upstream_latency_ms` | provider request sent                          | provider response fully read                              |
| `ttft_ms`             | handler entry                                  | first `data:` chunk with content flushed (streaming only) |

**When and how it is written**

- The handler builds a `Request` as it goes and calls `app.recordUsage(rec)` in a `defer`, so early returns (validation errors, upstream failures) are recorded too.
- `recordUsage` runs the insert inside `app.background()` (LGF Ch. 14) with `context.WithTimeout(context.Background(), 3*time.Second)`. It must not use `r.Context()`: that is cancelled when a streaming client disconnects, and the row would be lost.
- Graceful shutdown (LGF Ch. 12) already waits on `app.wg`, so in-flight usage rows are written before Cloud Run stops the instance.
- The same fields go to one `slog` line per request (`msg="request completed"`), which Cloud Logging indexes for free.

## Grafana dashboard

Grafana reads `requests` directly through its PostgreSQL data source as `grafana_ro`; ten panels cover traffic, spend, speed and health, and every query below uses the built-in `$__timeFilter` and `$__timeGroupAlias` macros so the dashboard time picker works.

|     |                             |                      |                                                       |
| --- | --------------------------- | -------------------- | ----------------------------------------------------- |
| #   | Panel                       | Type                 | Answers                                               |
| 1   | Requests by outcome         | time series, stacked | How much traffic, and how much is failing or limited? |
| 2   | Month-to-date cost          | stat (USD)           | What have I spent this month?                         |
| 3   | Cost per day by model       | bar, stacked         | Which model drives spend?                             |
| 4   | Tokens per day by model     | bar, input vs output | Where do tokens go?                                   |
| 5   | Latency p50 / p95           | time series (ms)     | Is the gateway or provider getting slower?            |
| 6   | Time to first token p95     | time series (ms)     | How fast do streams start?                            |
| 7   | Error and 429 rate          | stat (%)             | Is anything broken right now?                         |
| 8   | Usage by API key            | table                | Which key made which calls?                           |
| 9   | By gateway version          | table                | Did a deploy change latency, errors or cost?          |
| 10  | Cache hit and fallback rate | time series (%)      | Do Nov 4 and Nov 11 features pay off?                 |

```sql
-- 1 Requests by outcome
SELECT $__timeGroupAlias(created_at, $__interval),
       count(*) FILTER (WHERE status_code < 400)                       AS ok,
       count(*) FILTER (WHERE status_code = 429)                       AS rate_limited,
       count(*) FILTER (WHERE status_code >= 400 AND status_code <> 429) AS errors
FROM requests WHERE $__timeFilter(created_at) GROUP BY 1 ORDER BY 1;

-- 2 Month-to-date cost
SELECT sum(cost_usd) FROM requests WHERE created_at >= date_trunc('month', now());

-- 3 Cost per day by model (format: time series, one series per model)
SELECT $__timeGroupAlias(created_at, '1d'), model AS metric, sum(cost_usd) AS value
FROM requests WHERE $__timeFilter(created_at) AND model IS NOT NULL
GROUP BY 1, 2 ORDER BY 1;

-- 4 Tokens per day by model
SELECT $__timeGroupAlias(created_at, '1d'), model AS metric,
       sum(input_tokens) AS input, sum(output_tokens) AS output
FROM requests WHERE $__timeFilter(created_at) AND model IS NOT NULL
GROUP BY 1, 2 ORDER BY 1;

-- 5 Latency p50 / p95 (successful calls only)
SELECT $__timeGroupAlias(created_at, $__interval),
       percentile_cont(0.5)  WITHIN GROUP (ORDER BY latency_ms) AS p50,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) AS p95,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY upstream_latency_ms) AS upstream_p95
FROM requests WHERE $__timeFilter(created_at) AND status_code = 200
GROUP BY 1 ORDER BY 1;

-- 6 Time to first token p95
SELECT $__timeGroupAlias(created_at, $__interval),
       percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms) AS ttft_p95
FROM requests WHERE $__timeFilter(created_at) AND streamed AND ttft_ms IS NOT NULL
GROUP BY 1 ORDER BY 1;

-- 7 Error and 429 rate
SELECT 100.0 * count(*) FILTER (WHERE status_code >= 500) / nullif(count(*), 0) AS error_pct,
       100.0 * count(*) FILTER (WHERE status_code = 429)  / nullif(count(*), 0) AS limited_pct
FROM requests WHERE $__timeFilter(created_at);

-- 8 Usage by API key
SELECT k.name, k.key_prefix, count(*) AS requests,
       sum(r.input_tokens) AS input_tokens, sum(r.output_tokens) AS output_tokens,
       sum(r.cost_usd) AS cost_usd, max(r.created_at) AS last_request
FROM requests r JOIN api_keys k ON k.id = r.api_key_id
WHERE $__timeFilter(r.created_at) GROUP BY k.name, k.key_prefix ORDER BY cost_usd DESC;

-- 9 By gateway version
SELECT gateway_version, min(created_at) AS first_seen, count(*) AS requests,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) AS p95_ms,
       100.0 * count(*) FILTER (WHERE status_code >= 500) / count(*) AS error_pct,
       sum(cost_usd) / nullif(count(*), 0) * 1000 AS cost_per_1k_requests
FROM requests WHERE $__timeFilter(created_at)
GROUP BY gateway_version ORDER BY first_seen DESC;

-- 10 Cache hit and fallback rate
SELECT $__timeGroupAlias(created_at, $__interval),
       100.0 * avg(cache_hit::int)     AS cache_hit_pct,
       100.0 * avg(fallback_used::int) AS fallback_pct
FROM requests WHERE $__timeFilter(created_at) AND status_code < 500
GROUP BY 1 ORDER BY 1;
```

Add a dashboard variable `model` (`SELECT DISTINCT model FROM requests WHERE model IS NOT NULL`) and append `AND model IN ($model)` to panels 3 to 6 to filter by model. Export the dashboard JSON into the repo at `deploy/grafana/gateway.json` so it is versioned with the code.

## Deployment, open questions and done criteria

Cloud Run runs one practice instance of the gateway; Postgres and Grafana run outside it, because Cloud Run containers are stateless and scale to zero.

**Container image (multi-stage Dockerfile)**

- Two stages. Build stage on `golang:1.27-alpine`: copy `go.mod`, vendored deps (`make vendor` output) and source, then `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X gateway.jackwillis517.net/internal/vcs.Version=$VERSION" -o /api ./cmd/api` (LGF Ch. 20). `$VERSION` comes from `git describe --always --dirty` in a new `make build/image` target, so `-version`, `/v1/healthcheck` and `requests.gateway_version` all report the real build.
- Runtime stage: `gcr.io/distroless/static-debian12:nonroot`, which already runs as UID 65532, ships CA certs for upstream TLS, and has no shell or package surface — so no `USER` directive is needed and the container is never root. If a debug shell is ever needed, the fallback is `alpine:3` + `USER appuser` (created with `adduser`); distroless stays the default for size and attack surface.
- Cloud Run honours the image's non-root user; listening on `PORT` (default 4000) stays unprivileged.

**Cloud Run settings**

- `--max-instances=1` (in-memory rate limiter, single user), `--min-instances=0`.
- `--timeout=300s`, at least as long as `-upstream-timeout`, so long streams are not cut by Cloud Run.
- Read `PORT` from the environment as the default for `-port`.
- `/v1/healthcheck` as the startup probe; it returns status, environment and `gateway_version`.
- Provider keys and the DB DSN from Secret Manager as environment variables (Nov 6).
- Migrations run from the laptop with LGF's `make db/migrations/up` against the target DSN before each deploy, never at container start.

**Open questions**

- Where does Postgres live for the Cloud Run deploy? Locally it is Postgres in Docker, as in LGF. Cloud Run needs a reachable host, for example a small Compute Engine VM running Postgres with SSL and a firewall rule. Needs an answer by Oct 29, when the plan sets up the database.
- `version` in the usage row is read as the gateway build version (LGF Ch. 20). If you meant prompt version, `prompt_version` is already there for W4.
- Grafana: local Docker against the local DB for now, or on the same VM as Postgres once deployed?
