# Plan: Per-token rate limiting (spec §Rate limiting) + GET /v1/models

Implements: `spec.md` §Rate limiting (lines 236-291) + `spec.md` routes table line 81 (`GET /v1/models`) | Builds on: `plans/stateful-token-auth.md` (auth middleware + `tokens` table landed, uncommitted) | Type: rewrite+feature | Complexity: M | Systems: `cmd/api` only (no `internal/` changes), `curl.md`

Deps: `golang.org/x/time/rate` v0.16.0, `github.com/oklog/ulid/v2` v2.1.2 — both already in `go.mod`. Nothing new.

## Goal

As the single user (Jack), I want the rate limiter keyed by `tokens.id` with per-token overrides, OpenAI-style `X-RateLimit-*`/`Retry-After` headers and a recorded `requests` row on every 429, plus a `GET /v1/models` endpoint listing enabled models OpenAI-compatibly, so that SDK backoff works, rejected calls show up in Grafana, and clients can discover the model catalog.

Problem: `rateLimit` is still LGF Ch. 11's per-IP `Allow()` limiter (`cmd/api/middleware.go:97-166`): keyed `map[string]*client` by `r.RemoteAddr` (middleware.go:106,132), no headers, no 429 row, limits every token together by IP. `GET /v1/models` is a commented TODO (`cmd/api/routes.go:18`); `cmd/api/llm_models.go` is an empty `package main` stub.
Solution: rewrite `rateLimit` per `spec.md:249-290` (the pseudo-code there is the contract): keyed `map[int64]*client` by `tokens.id`, `Reserve().Delay()/Cancel()`, headers, `limitsFor(token)` override-vs-flags, `recordRejected` writing the whole row; new `listModelsHandler` wrapping `GetAllEnabled()` into the OpenAI list shape.

## Out of scope

- Not: tokens-per-minute (TPM) limiter (spec ~297) — optional in spec, stays out.
- Not: flag defaults `-limiter-rps 2` / `-limiter-burst 4` (`cmd/api/main.go:69-70`) — spec table (spec.md:65) says 1/5, but 2/4 is live code reality; NOT changing (see Open/assumed).
- Not: brute-force protection on `POST /v1/tokens/authentication` — it is anonymous (no header → skips limiter, spec 257 requires anonymous pass-through). Accepted W1 risk, spec debt (see Open/assumed).
- Not: shared/Postgres limiter for multi-instance Cloud Run — spec 293 caveat: deploy `--max-instances=1`; in-memory map is per-process.
- Not: usage rows for `/v1/models` or `/v1/healthcheck` themselves (only the chat handler records; middleware records only 429s).
- Not changing: `cmd/api/chat.go`, `recordUsage` (reused as-is), `authenticate` chain order (`routes.go:25`: `app.recoverPanic(app.authenticate(app.rateLimit(router)))` — authenticate already outermost, which the per-token limiter requires), spec.md.

## Read first (required)

- `spec.md:236-291` — §Rate limiting: the reference pseudo-code (249-290) IS the contract; `Reserve()` vs `Allow()` rationale (289); SetLimit/SetBurst without restart (291); Cloud Run caveat (293).
- `spec.md:80-82` — routes table: `GET /v1/models` "lists enabled rows of `models`", Token auth. spec.md:222: `requireAuthenticatedUser` wraps the chat, models and usage routes.
- `cmd/api/middleware.go:97-166` — the per-IP limiter being replaced (cleanup goroutine shape to keep, keyed differently).
- `cmd/api/chat.go:313-342` — `recordUsage(rec)` + the `"request completed"` log line + `strOrNull`/`intOrNull` (340-342, 349) — `recordRejected` reuses `recordUsage` wholesale.
- `cmd/api/context.go:45-52` — `contextGetToken` (nil for anonymous, no panic); `contextGetUser` panics if missing (authenticate guarantees one always, middleware.go:33/50).
- `internal/data/tokens.go:30-44` — `Token.RateLimitRPS *float64`, `RateLimitBurst *int`, `TokensPerMinute *int`; scanned by `GetForPlaintext` (tokens.go:157-162) on EVERY request — overrides are always fresh, no cache.
- `internal/data/requests.go:12-38` — `Request` struct (`RequestID string`, `UserID int64`, `TokenID *int64`, `ErrorCode *string`, `GatewayVersion string`) + `Insert(ctx, r)` (41-84): all non-listed columns default (tokens 0, cost computed as 0 for `model IS NULL` via the `COALESCE((SELECT ... FROM models WHERE name = $4), 0)`).
- `internal/data/llm_models.go:16-33` — `LLMModel` struct: has `CreatedAt time.Time` (line 31) → usable for `"created"`. `GetAllEnabled()` (95-122) exists, `ORDER BY name`.
- `cmd/api/routes.go:15-25` — `router.Handler(method, path, http.Handler)` pattern for wrapped routes; commented TODO at line 18.
- `cmd/api/errors.go:181-186` — existing `rateLimitExceededResponse` (429, `rate_limit_error`/`rate_limit_exceeded` envelope, matches spec 429 row) — unchanged.
- `migrations/000007_seed_models.up.sql` — the seeded row: `name 'ollama/qwen3.5:4b'`, `alias 'qwen-local'`, `provider 'ollama'`, `upstream_model 'qwen3.5:4b'`, prices 0, `max_output_tokens 32768`.

## Create

- Nothing. All tasks update existing files (`cmd/api/llm_models.go` exists as a stub — fill it).

## Patterns

Existing per-IP limiter being replaced (`cmd/api/middleware.go:104-127` — keep this cleanup shape, re-key):

```go
var (
 mu      sync.Mutex
 clients = make(map[string]*client)
)
go func() {
 for {
  time.Sleep(time.Minute)
  mu.Lock()
  for ip, client := range clients {
   if time.Since(client.lastSeen) > 3*time.Minute {
    delete(clients, ip)
   }
  }
  mu.Unlock()
 }
}()
```

Row-build + background insert + log (`cmd/api/chat.go:315` + chat.go:25-33 — `recordRejected` mirrors this, then delegates to `recordUsage`):

```go
rec := &data.Request{
 RequestID:      "chatcmpl-" + ulid.Make().String(),
 UserID:         app.contextGetUser(r).ID,
 CreatedAt:      start,
 GatewayVersion: app.config.version,
 StatusCode:     http.StatusOK, // overwritten on every failure path
}
if t := app.contextGetToken(r); t != nil {
 rec.TokenID = &t.ID
}
...
app.recordUsage(rec)   // chat.go:43 — does Insert in app.background + "request completed" log
```

Wrapped-route registration (`cmd/api/routes.go:15-16`):

```go
router.Handler(http.MethodPost, "/v1/chat/completions",
 app.requireAuthenticatedUser(http.HandlerFunc(app.createChatCompletionHandler)))
```

## Tasks

### 1. UPDATE `cmd/api/main.go:60` — port default 4005 → 4006

- DO: `flag.IntVar(&cfg.port, "port", 4005, "API server port")` → `4006`.
- GOTCHA: the brief says the port was already changed; the working tree still says `4005` (verified `cmd/api/main.go:60`). This task makes reality match. `probeHealthcheck(cfg.port)` (main.go:82) reads the flag, no other edit needed. `spec.md` mentions 4005 in a few curl examples but spec.md is out of bounds — leave.
- VALIDATE: `go build ./... && grep -n '"port", 4006' cmd/api/main.go`

### 2. REWRITE `cmd/api/middleware.go:97-166` — per-token `rateLimit` + `limitsFor` + `recordRejected`

- DO: replace the whole `rateLimit` function with the spec contract (`spec.md:249-290`), keyed by `int64`:

```go
func (app *application) rateLimit(next http.Handler) http.Handler {
 type client struct {
  limiter  *rate.Limiter
  lastSeen time.Time
 }

 var (
  mu      sync.Mutex
  clients = make(map[int64]*client) // keyed by tokens.id, not IP
 )

 // LGF cleanup goroutine: drop limiters idle for 3 minutes, every minute.
 go func() {
  for {
   time.Sleep(time.Minute)
   mu.Lock()
   for id, c := range clients {
    if time.Since(c.lastSeen) > 3*time.Minute {
     delete(clients, id)
    }
   }
   mu.Unlock()
  }
 }()

 return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
  if !app.config.limiter.enabled {
   next.ServeHTTP(w, r)
   return
  }

  // Anonymous (no Authorization header): healthcheck and token mint
  // pass through unlimited (spec 257).
  token := app.contextGetToken(r)
  if token == nil {
   next.ServeHTTP(w, r)
   return
  }

  start := time.Now()
  rps, burst := app.limitsFor(token)

  mu.Lock()
  c, ok := clients[token.ID]
  if !ok {
   c = &client{limiter: rate.NewLimiter(rate.Limit(rps), burst)}
   clients[token.ID] = c
  }
  c.lastSeen = time.Now()
  // A DB override changed since this limiter was created: apply it
  // without a restart (spec 291).
  if c.limiter.Limit() != rate.Limit(rps) {
   c.limiter.SetLimit(rate.Limit(rps))
  }
  if c.limiter.Burst() != burst {
   c.limiter.SetBurst(burst)
  }
  res := c.limiter.Reserve()
  remaining := int(c.limiter.Tokens())
  mu.Unlock()

  w.Header().Set("X-RateLimit-Limit-Requests", strconv.Itoa(burst))
  w.Header().Set("X-RateLimit-Remaining-Requests", strconv.Itoa(max(remaining, 0)))

  if d := res.Delay(); d > 0 {
   res.Cancel() // rejected, not waiting: give the token back
   w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
   app.recordRejected(r, http.StatusTooManyRequests, start)
   app.rateLimitExceededResponse(w, r)
   return
  }
  next.ServeHTTP(w, r)
 })
}
```

- DO: add `limitsFor` right below (token override, else flag defaults — `main.go:69-70` defaults 2/4):

```go
// limitsFor resolves the effective rate limit for a token: the token row's
// override when set, else the -limiter-rps / -limiter-burst flag defaults.
func (app *application) limitsFor(token *data.Token) (rps float64, burst int) {
 rps, burst = app.config.limiter.rps, app.config.limiter.burst
 if token.RateLimitRPS != nil {
  rps = *token.RateLimitRPS
 }
 if token.RateLimitBurst != nil {
  burst = *token.RateLimitBurst
 }
 return rps, burst
}
```

- DO: add `recordRejected` below that. The chat handler never runs, so the whole row is built here; `app.recordUsage(rec)` (`chat.go:315`) already does the background Insert (`context.Background()` + 3s timeout, `chat.go:317-322`) and the `"request completed"` log line (`chat.go:323-335`) — reuse it, do not duplicate:

```go
// recordRejected writes a requests row for a request the middleware rejected
// before any handler ran (429). Zero tokens, zero cost, no model. The row is
// attributed to the authenticated user and token from the context.
func (app *application) recordRejected(r *http.Request, status int, start time.Time) {
 code := "rate_limit_exceeded"
 rec := &data.Request{
  RequestID:      "chatcmpl-" + ulid.Make().String(),
  UserID:         app.contextGetUser(r).ID,
  StatusCode:     status,
  ErrorCode:      &code,
  GatewayVersion: app.config.version,
  LatencyMs:      int(time.Since(start).Milliseconds()),
 }
 if t := app.contextGetToken(r); t != nil {
  rec.TokenID = &t.ID
 }
 app.recordUsage(rec)
}
```

- DO: fix middleware.go imports (lines 3-16): drop `"net"`; add `"math"`, `"strconv"`, `"github.com/oklog/ulid/v2"`. Keep `"sync"`, `"time"`, `"golang.org/x/time/rate"`, `internal/data`.
- PATTERN: `cmd/api/chat.go:25-33` (rec build incl. `"chatcmpl-" + ulid.Make().String()`), `cmd/api/chat.go:315` (`recordUsage`), `cmd/api/middleware.go:104-127` (cleanup goroutine).
- GOTCHA (header order): all three headers must be `w.Header().Set` BEFORE `app.rateLimitExceededResponse` — `writeJSON` (`helpers.go:47-49`) copies `w.Header()` at write time; set-after-write = header lost. Same reason the `X-RateLimit-*` pair is set before `next.ServeHTTP(w, r)` (success responses carry it too — matches OpenAI).
- GOTCHA (user never nil): `recordRejected` calls `contextGetUser`, which panics on a missing user (`context.go:38-44`). Safe: `authenticate` (middleware.go:33, 50) always sets a user, and `recordRejected` only runs when `token != nil` ⇒ real user. `recoverPanic` is outermost as backstop.
- GOTCHA (`Reserve` vs `Allow`): `Reserve()` consumes a token; on rejection `res.Cancel()` returns it (spec 289), so a 429 storm does not deepen the bucket debt. `Delay() == 0` means allowed.
- GOTCHA (override freshness): `GetForPlaintext` re-reads `rate_limit_rps`/`rate_limit_burst` on every request (tokens.go:157-162), so an `UPDATE tokens` takes effect on the next request; `SetLimit`/`SetBurst` above only re-tunes the cached limiter — no restart, spec 291.
- GOTCHA (negative remaining): after `Reserve()` on an exhausted bucket, `Tokens()` can be negative — hence `max(remaining, 0)` in the header, `math.Ceil` on `Retry-After` seconds (both spec 275, 279).
- VALIDATE: `go build ./... && go vet ./... && gofmt -l .` → no output.

### 3. UPDATE `cmd/api/llm_models.go` — `listModelsHandler` (file exists, empty `package main` stub)

- DO: fill it with the OpenAI list shape (`spec.md:81`; OpenAI-compatible so SDKs work, matching the gateway's whole-body contract):

```go
package main

import (
 "net/http"

 "gateway.jackwillis517.net/internal/data"
)

// listModelsHandler serves GET /v1/models: the enabled rows of the models
// table in OpenAI's list shape. "id" is the gateway model name
// ("ollama/qwen3.5:4b") — the name clients send back as "model" — and
// "owned_by" is the provider.
func (app *application) listModelsHandler(w http.ResponseWriter, r *http.Request) {
 models, err := app.models.LLMModels.GetAllEnabled()
 if err != nil {
  app.serverErrorResponse(w, r, err)
  return
 }

 type modelObject struct {
  ID      string `json:"id"`
  Object  string `json:"object"`
  Created int64  `json:"created"`
  OwnedBy string `json:"owned_by"`
 }

 out := make([]modelObject, len(models))
 for i, m := range models {
  out[i] = modelObject{
   ID:      m.Name,
   Object:  "model",
   Created: m.CreatedAt.Unix(),
   OwnedBy: m.Provider,
  }
 }

 err = app.writeJSON(w, http.StatusOK, envelope{"object": "list", "data": out}, nil)
 if err != nil {
  app.serverErrorResponse(w, r, err)
 }
}
```

- PATTERN: `internal/data/llm_models.go:95-122` (`GetAllEnabled`, `ORDER BY name` — output order is stable); `cmd/api/helpers.go:28` (`envelope map[string]any`).
- GOTCHA: `data` import is needed only if nothing else uses it — it is not used by the handler body (`GetAllEnabled` is reached through `app.models`); if `goimports`/vet flags it, drop the `internal/data` import. Verify with build.
- GOTCHA (mapping is minimal on purpose): OpenAI also has `"permission"` and `"created"≠0` nuances — out of scope; `owned_by: provider` is the closest honest mapping (seed row: provider `ollama`). No `alias` in the response (OpenAI has no such field); alias stays a chat-request convenience only.
- VALIDATE: `go build ./... && go vet ./...`

### 4. UPDATE `cmd/api/routes.go:18` — register GET /v1/models

- DO: replace the commented line `// router.HandlerFunc(http.MethodGet, "/v1/models", app.)` with:

```go
 router.Handler(http.MethodGet, "/v1/models",
  app.requireAuthenticatedUser(http.HandlerFunc(app.listModelsHandler)))
```

- PATTERN: `cmd/api/routes.go:15-16` (chat route: `router.Handler` + `requireAuthenticatedUser(http.HandlerFunc(...))`) — required because the route is middleware-wrapped; `router.HandlerFunc` takes a bare func.
- GOTCHA: wrap with `requireAuthenticatedUser` (spec 222 — "wraps the chat, models and usage routes"); without it an anonymous GET hits the handler and lists the catalog. Anonymous requests still pass `authenticate`+`rateLimit` (token==nil → pass-through, Task 2) and are stopped here with 401 `missing_token` (`errors.go:171-178`).
- GOTCHA: `/v1/models` sits inside the `rateLimit` chain (routes.go:25) — authed model lists are rate-limited per token and carry `X-RateLimit-*` headers; that is desired (they are authed API calls).
- VALIDATE: `go build ./... && go vet ./... && gofmt -l .` → no output. `grep -n 'v1/models' cmd/api/routes.go` shows the live route, no comment.

### 5. UPDATE `curl.md` — fix stale port/path + two new testing sections

- DO (fix stale): whole-file replacements, then verify:

```sh
sed -i 's|localhost:4005|localhost:4006|g; s|/v1/tokens/authorization|/v1/tokens/authentication|g' curl.md
grep -c '4005\|tokens/authorization' curl.md   # expect 0
```

- DO (fix stale seed name): curl.md:7 says the seeded model is `ollama/qwen3.5:2b-32k`; the actual seed (migrations/000007_seed_models.up.sql) is `ollama/qwen3.5:4b` (alias `qwen-local`, ceiling 32768). Correct line 7.
- DO (append `## GET /v1/models` section after the token-mint intro block, before `## Non-streaming`):

````markdown
## GET /v1/models

OpenAI-compatible model list. `id` is the name clients send as `"model"`.

### List models with a token

```bash
curl -s http://localhost:4006/v1/models \
  -H "Authorization: Bearer $GW_TOKEN" | jq
# expect: {"object": "list", "data": [
#   {"id": "ollama/qwen3.5:4b", "object": "model", "created": <unix ts>, "owned_by": "ollama"}]}
```
````

### Without a token — 401

```bash
curl -si http://localhost:4006/v1/models | grep -E 'HTTP|code'
# expect: HTTP/1.1 401 Unauthorized, "code": "missing_token"
```

````

- DO (append `## Rate limiting` section at the end of the file):

```markdown
## Rate limiting

Per-token, keyed by `tokens.id`. Defaults come from `-limiter-rps` (2) /
`-limiter-burst` (4); a token row's `rate_limit_rps`/`rate_limit_burst`
override them live (no restart). Anonymous requests are never limited.
All commands need the `Authorization: Bearer $GW_TOKEN` header.

### Burst exhaustion → 429 + headers

Send more than the burst (4) faster than the refill (2/s):

```bash
for i in $(seq 1 6); do
  curl -s -o /dev/null -w '%{http_code} ' \
    -H "Authorization: Bearer $GW_TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}' \
    http://localhost:4006/v1/chat/completions
done; echo
# expect: mostly 200 then 429 (burst 4 + refill during the loop)
````

### The 429 response headers

Immediately after exhausting the burst (run the loop above twice in a row):

```bash
curl -si -H "Authorization: Bearer $GW_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}' \
  http://localhost:4006/v1/chat/completions \
  | grep -iE '^HTTP|retry-after|x-ratelimit'
# expect:
#   HTTP/1.1 429 Too Many Requests
#   Retry-After: 1
#   X-RateLimit-Limit-Requests: 4
#   X-RateLimit-Remaining-Requests: 0
```

### Refill — wait 1/rps, then it passes again

```bash
sleep 3    # rps 2 → ~6 tokens refilled
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $GW_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}' \
  http://localhost:4006/v1/chat/completions
# expect: 200
```

### Per-token isolation — one blocked, the other fine

```bash
GW_TOKEN2=$(curl -s -X POST localhost:4006/v1/tokens/authentication \
  -H 'Content-Type: application/json' \
  -d '{"email":"jackwillis517@gmail.com","password":"<seeded password>"}' \
  | jq -r '.authentication_token.token')
# exhaust GW_TOKEN (loop above), then immediately:
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $GW_TOKEN2" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}' \
  http://localhost:4006/v1/chat/completions
# expect: 200 — each token has its own bucket
```

### Anonymous requests are never limited

```bash
for i in $(seq 1 10); do
  curl -s -o /dev/null -w '%{http_code} ' http://localhost:4006/v1/healthcheck
done; echo
# expect: 200 ×10, no 429, no requests rows
```

### 429 rows land in `requests`

```sh
PGPASSWORD=pa55word psql -h 127.0.0.1 -U gateway -d gateway \
  -c "SELECT request_id, status_code, error_code, model, input_tokens, output_tokens, cost_usd
      FROM requests WHERE status_code = 429 ORDER BY id DESC LIMIT 3;"
# expect: request_id 'chatcmpl-<ulid>', status_code 429, error_code rate_limit_exceeded,
#         model NULL, 0 tokens, cost 0
```

### Per-token override without a restart

```sh
PGPASSWORD=pa55word psql -h 127.0.0.1 -U gateway -d gateway \
  -c "UPDATE tokens SET rate_limit_rps = 10, rate_limit_burst = 20
      WHERE id = (SELECT min(id) FROM tokens WHERE revoked_at IS NULL);"
# next request picks up the override (token row is re-read on every request):
for i in $(seq 1 8); do
  curl -s -o /dev/null -w '%{http_code} ' \
    -H "Authorization: Bearer $GW_TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}' \
    http://localhost:4006/v1/chat/completions
done; echo
# expect: all 200 (burst 20 now) and X-RateLimit-Limit-Requests: 20 in the headers
# reset afterwards:
PGPASSWORD=pa55word psql -h 127.0.0.1 -U gateway -d gateway \
  -c "UPDATE tokens SET rate_limit_rps = NULL, rate_limit_burst = NULL
      WHERE id = (SELECT min(id) FROM tokens WHERE revoked_at IS NULL);"
```

````

- GOTCHA: the `<seeded password>` placeholder matches the existing curl.md:12 convention — do not bake the real password in. The token in `min(id)`-override is whichever was minted first; if `$GW_TOKEN` and `$GW_TOKEN2` differ in id order the isolation test may interact — run the override test last, then reset to NULL.
- GOTCHA: chat calls in these loops hit the real Ollama upstream; a slow model can make bursts hard to trigger (each call takes longer than 1/rps refill). Use `"max_tokens": 1` in the loop bodies if refill keeps up — the loop pattern above is fine either way because 6 rapid calls > burst 4.
- VALIDATE: `grep -c 4006 curl.md` ≥ 5; `grep -c 'tokens/authentication' curl.md` ≥ 1; `grep -c '4005\|tokens/authorization' curl.md` = 0.

## Tests

No test files exist in the repo (none found; matches prior plan's stance). New unit tests NOT required — manual curl validation below is the acceptance gate. Seams if tests ever come: `rateLimit(next)` as an `http.Handler` with a fake context-token (httptest), `limitsFor` as a pure function, `GetAllEnabled` over real Postgres. NOT planned for W1.

## Validation

```sh
# L1-L3 after each chunk (Tasks 1-4)
go build ./... && go vet ./... && gofmt -l .
# L4 none (no test files)
# L5 manual (server on 4006 after Task 1)
make run/api
````

L5 manual order (full curl.md sections; prerequisites: Postgres up, Ollama up with `qwen3.5:4b` pulled, `.envrc` DSN):

```sh
GW_TOKEN=$(curl -s -X POST localhost:4006/v1/tokens/authentication \
  -H 'Content-Type: application/json' \
  -d '{"email":"jackwillis517@gmail.com","password":"<seeded password>"}' \
  | jq -r '.authentication_token.token')

# models: 200 with token; 401 missing_token without
curl -s -H "Authorization: Bearer $GW_TOKEN" localhost:4006/v1/models | jq '.data[0].id'
# expect "ollama/qwen3.5:4b"
curl -si localhost:4006/v1/models | grep -E 'HTTP|missing_token'

# rate limit: burst loop → 429s; headers; refill; isolation; anonymous; psql 429 row; override
# (exact commands in curl.md sections added by Task 5)

# DB check for the 429 row + override liveness:
PGPASSWORD=pa55word psql -h 127.0.0.1 -U gateway -d gateway \
  -c "SELECT status_code, error_code, count(*) FROM requests WHERE status_code = 429 GROUP BY 1,2;"
# expect: 429 | rate_limit_exceeded | n>0
```

## Acceptance

- [ ] Burst exhaustion returns `429` with `Retry-After`, `X-RateLimit-Limit-Requests: 4`, `X-RateLimit-Remaining-Requests: 0`; after ~1/rps wait the same call returns 200.
- [ ] Two tokens have independent buckets: exhausting one does not limit the other.
- [ ] Anonymous requests (`/v1/healthcheck`, `POST /v1/tokens/authentication`) are never limited and write no requests rows.
- [ ] Every 429 writes a `requests` row: `status_code=429`, `error_code='rate_limit_exceeded'`, `model NULL`, 0 tokens, `cost_usd 0`, correct `user_id` + `token_id`, and a `request completed` log line at 429.
- [ ] `UPDATE tokens SET rate_limit_rps=10, rate_limit_burst=20 ...` takes effect on the very next request, no restart; `X-RateLimit-Limit-Requests` reflects it; NULL reset falls back to flag defaults (2/4).
- [ ] `GET /v1/models` with a Bearer token returns `{"object":"list","data":[{"id":"ollama/qwen3.5:4b","object":"model","created":<unix>,"owned_by":"ollama"}]}`; without a token → 401 `missing_token`.
- [ ] Old per-IP limiter fully gone: no `net.SplitHostPort` / `r.RemoteAddr` in `middleware.go`; no `map[string]*client`.
- [ ] `go build ./... && go vet ./... && gofmt -l .` green; curl.md has zero `4005`/`tokens/authorization` refs; server runs on 4006.

## Open / assumed

- ASSUMED — **Port 4006**: brief says the `-port` default was already changed to 4006; working tree still shows `4005` (`cmd/api/main.go:60`, verified). Task 1 changes it. If the intent was the opposite (keep 4005), drop Task 1 and the sed port replacement in Task 5.
- ASSUMED — **Flag defaults stay 2/4**: spec.md:65 table says 1/5 ("spec-ideal"), live code defaults are `-limiter-rps 2` / `-limiter-burst 4` (`main.go:69-70`). Not changing flags (user decision); spec table stays as aspirational doc.
- ASSUMED — **Brute-force exposure accepted (W1)**: `POST /v1/tokens/authentication` is anonymous (email+password mint) and therefore passes the limiter unlimited, as spec 257 requires for all anonymous requests. Repeated password guessing is unconstrained in W1. Accepted risk; future fix (a second, tiny per-IP limiter on the mint route only, or lockout) is spec debt.
- ASSUMED — **`/v1/models` and `/v1/healthcheck` write no usage rows**: spec 431 ("every request that passes authentication writes exactly one row") read narrowly applies to the chat pipeline; only the chat handler + 429 middleware record today. Not extended here — flag if usage-per-endpoint is wanted.
- ASSUMED — **`max(remaining, 0)` header, `math.Ceil` Retry-After**: Go 1.27 builtin `max` + `math` (already stdlib) per spec 275/279.
- ASSUMED — **`recordRejected` reuses `recordUsage` verbatim** (chat.go:315) rather than a new insert+log path — same row, same log line, one code path; `CreatedAt` on the rec is not needed (Insert does not write the column, it defaults to `now()`, requests.go:50-52).
- Multi-instance caveat (spec 293): in-memory map means limit × instances; Cloud Run must deploy `--max-instances=1`. No code change; noted for the deploy day.

## Notes

- Rejected: recording the 429 row from inside `rateLimitExceededResponse` (errors.go) — the row needs `start` time and context, and errors.go is a body-shape module; middleware is the right seam (spec 280 puts `recordRejected` before the response too).
- Rejected: a `RequestTracker`/service struct for recording — one extra helper (`recordRejected`) + existing `recordUsage` is enough at this size; LGF keeps middleware helpers on `application`.
- Rejected: keeping the per-IP limiter as a second layer (defense for the anonymous mint route) — would solve the brute-force exposure, but the user explicitly deferred it to W1-accepted-risk; reintroduce per-IP-only-on-mint later if desired.
- Rejected: OpenAI `"permission"` array on model objects — no consumer; minimal mapping per brief.
- Sequencing: Tasks 2 and 3/4 are independent of each other (both touch only `cmd/api`); Task 1 first (curl commands in Task 5 assume 4006); Task 5 last (documents the finished behavior).
- Risk: header name drift — spec says `X-RateLimit-Limit-Requests`/`X-RateLimit-Remaining-Requests` (spec 274-275), OpenAI's live API uses `x-ratelimit-limit-requests`; Go's `Header.Set` canonicalizes, curl greps are case-insensitive (`grep -i`). No action.
- Risk: the burst-exhaustion curl loop can be masked by slow Ollama responses (each call > 500ms refills a token at rps 2). Mitigation noted in Task 5 GOTCHA (`max_tokens: 1`); psql override test (rps 10/burst 20) is deterministic regardless.

## Amendments

(append-only)
