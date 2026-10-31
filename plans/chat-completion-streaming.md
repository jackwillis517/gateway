# Plan: Wire completeChat + streamChat stubs into cmd/api/chat.go

Implements: spec.md "chat completions (non-streaming and SSE streaming)" (Oct 28) | Type: enhance | Complexity: M | Systems: cmd/api, internal/provider | Deps: provider impls exist (openai/ollama), data.Request model exists

## Goal
As API user, I want working POST /v1/chat/completions (JSON + SSE), so that OpenAI SDKs work against gateway. Stubs at `cmd/api/chat.go` print placeholder text; replace with real logic + create 2 missing helpers.

## Out of scope
- Retries/fallback (Nov 4), response cache (Nov 11), usage endpoint (Nov 13), key mgmt (Nov 18) — see Day list.
- Changing provider package (Complete/Stream signatures already match).
- Auth + rate-limit middleware — **stubbed, user implements separately** (see task 7).

## Read first (required)
- `cmd/api/chat.go` — handler pipeline already built; stubs to fill
- `cmd/api/errors.go` — `apiError` shape, `errorResponse(w,r,status,apiError)` signature
- `cmd/api/helpers.go` — `writeJSON` (widened to `any`), `background()`
- `internal/provider/types.go` — `ChatCompletionResponse` (FinishReason string), `ChatCompletionChunk` (FinishReason *string), `ChunkDelta.Content` string
- `internal/provider/provider.go` — `UpstreamError{Status,Message}`
- `internal/data/requests.go` — `Request` fields: FinishReason *string, InputTokens/OutputTokens int, TTFTMs/UpstreamLatencyMs *int, ErrorCode *string
- `internal/jsonlog/jsonlog.go` — only PrintInfo/PrintError/PrintFatal exist, map-based; no Warn level

## Create
- `classifyUpstreamError` — errors.go
- `recordTokens` — chat.go usage-helpers section
- `writeSSE` — chat.go next to streamChat

## Update
- `cmd/api/chat.go` — fill both stubs, add writeSSE, fix duration bug, strconv log fix
- `cmd/api/helpers.go` — writeJSON param `envelope` → `any`
- `cmd/api/context.go` — anonymous-user fallback (middleware stubbed)
- `cmd/api/routes.go` — chat + healthcheck handlers as-is; middleware left stubbed (user implements; see task 7)

---

## Tasks

### 1. UPDATE `cmd/api/chat.go` — fill both stubs

- completeChat: `prov.Complete` → relabel id/model to gateway identity → record finish_reason + tokens → `writeJSON(200, resp)` (bare body).
- streamChat: `http.NewResponseController(w).SetWriteDeadline(now + upstreamTimeout)` (server WriteTimeout 30s would cut long streams); **lazy** SSE `begin()` (upstream error before first chunk → normal JSON envelope with real status); emit relabels id/model per chunk; usage chunk forwarded only when `wantUsage`, tokens always recorded; `ttft_ms` on first content delta; finish_reason captured; error after `started` → in-stream `{"error":...}` chunk + `[DONE]` (SSE clients hang without it); 499 → silent return (deferred recordUsage still writes row).
- FIX existing bug: `time.Duration(app.config.upstreamTimeout)` = 120 **nanoseconds** → `time.Duration(...) * time.Second`.
- FIX existing bug: `string(rec.StatusCode)` (rune garbage) → `strconv.Itoa`.
- VALIDATE: `go build ./... && go vet ./...`
- MEETS: both stubs gone; compiles.

### 2. ADD `classifyUpstreamError` (`cmd/api/errors.go`)

**Use this exact function (user-provided):**

```go
// ---------------------------------------------------------------------------
// Upstream error classification
// ---------------------------------------------------------------------------

// classifyUpstreamError maps a provider error to the gateway's HTTP status,
// OpenAI error type and code (see the spec's error table).
func classifyUpstreamError(r *http.Request, err error) (status int, typ, code, msg string) {
	var ue *provider.UpstreamError
	switch {
	case r.Context().Err() != nil:
		// The client went away; the upstream call was cancelled with it.
		return statusClientClosedRequest, "", "client_disconnected", ""
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "api_error", "upstream_timeout",
			"the upstream provider did not respond in time"
	case errors.As(err, &ue) && ue.Status == http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "rate_limit_error", "upstream_rate_limited",
			"the upstream provider rate limited this request"
	default:
		return http.StatusBadGateway, "api_error", "upstream_error",
			"the upstream provider returned an error"
	}
}
```

- Order matters: `r.Context().Err()` check **first** — a disconnect cancels the upstream call too, so it would otherwise fall through to 504/502; checking the request context first always wins.
- `errors.As(err, &ue)` then `ue.Status == 429` — 429 passes through as 429 `upstream_rate_limited` (distinct from the gateway's own limiter). All other upstream errors (5xx, 4xx, bad data) → 502 `upstream_error`, per spec's table.
- `statusClientClosedRequest` const lives in chat.go.
- VALIDATE: `go build ./... && go vet ./...`
- MEETS: spec error table implemented; 429 passthrough distinct; 499 silent (deferred recordUsage still writes the row).

### 3. ADD `recordTokens` (`cmd/api/chat.go`, usage-helpers section)

- `func recordTokens(rec *data.Request, u *provider.Usage)`: nil-safe no-op; else `rec.InputTokens = u.PromptTokens`, `rec.OutputTokens = u.CompletionTokens`.
- GOTCHA: `data.Request` has CacheReadTokens/CacheWriteTokens but `provider.Usage` has no cache fields — leave 0 until cache day.
- VALIDATE: `go build ./...`
- MEETS: usage rows complete for both paths.

### 4. DECISION — error type name → RESOLVED: keep `apiError`

- 15 call sites use `apiError{Message, Type, Param *string, Code *string}` — kept; classifier callers use `ptr(code)`.

### 5. DECISION — logger methods → RESOLVED: adapt to jsonlog

- `app.logger.PrintError(err, map[string]string{...})`; SetWriteDeadline failure logs PrintError, stream continues.

### 6. UPDATE `cmd/api/helpers.go` — writeJSON accepts any

- `func (app *application) writeJSON(w http.ResponseWriter, status int, data any, headers http.Header) error`
- WHY: completeChat writes the bare OpenAI response struct (no envelope).
- GOTCHA: `errorResponse` passes `envelope` — still compiles (envelope satisfies any).
- VALIDATE: `go build ./...`
- MEETS: bare + envelope bodies both work.

### 7. UPDATE `cmd/api/context.go` — middleware left STUBBED (user implements)

- **Scope change (user):** `authenticate` + `rateLimit` stay stubbed — user implements those. This plan only needs the chat handler to work without them.
- `middleware.go` is a `package main` stub; `createChatCompletionHandler` reads `app.contextGetUser(r).ID` / `contextGetAPIKey(r)` — nil user → nil-pointer panic.
- So: **minimal safety shim**, not real auth:
  - `contextGetUser(r)` returns `data.AnonymousUser` when no user in context — no panic; usage row records anonymous (zero) id.
  - `contextGetAPIKey(r)` returns nil (handler already nil-safe).
  - `recoverPanic` NOT required — user wires it with their middleware.
- routes.go: chat + healthcheck handlers stay as-is. No middleware chain.
- VALIDATE: `curl POST localhost:4000/v1/chat/completions` (no key) → reaches handler (400/404/502), never panics.
- MEETS: handler works standalone; user can layer `recoverPanic(authenticate(rateLimit(...)))` later without touching this plan's code.
- Open 3 RESOLVED: auth/rate-limit middleware out of scope — user implements.

### 8. Manual verification

- Non-streaming (OLLAMA_BASE_URL set): curl POST with `"model": "ollama/<model>"`, `"stream": false` → OpenAI body, `id` = `chatcmpl-<ulid>`, `model` = gateway name; `X-Request-Id` header matches.
- Streaming: `"stream": true` → SSE frames `data: {...}\n\n`, last two lines usage chunk (if include_usage) + `data: [DONE]`.
- `wantUsage=false` → no usage chunk, but row still has tokens (`SELECT input_tokens, output_tokens FROM requests ORDER BY id DESC LIMIT 1`).
- Kill upstream (bad OLLAMA_BASE_URL) → 502 `upstream_error`; kill client mid-stream → row status 499; tiny `-upstream-timeout 1` + slow model → 504 `upstream_timeout`.

## Tests

- Unit: `cmd/api/chat_test.go` — classifyUpstreamError table test (disconnected/deadline/429/5xx/default), recordTokens, complete success/502/499, stream success/usage-hidden/empty-stream/pre-chunk-error/in-stream-error/499, writeJSON-any, context shim. fakeProvider in memory, no DB/network.
- Full matrix documented in `tests.md`.

## Validation

L1 `go build ./...` · L2 `go vet ./...` · L3 `go test ./cmd/api/` (12/12 green) · L4 manual curl matrix (tests.md §2, needs Ollama + Postgres) · L5 `psql` row checks.

## Acceptance

- [x] Both stubs replaced; no `fmt.Println` placeholders
- [x] `time.Duration` bug fixed
- [x] recordUsage logs use strconv.Itoa
- [ ] Non-streaming + streaming via curl — covered by `cmd/api/chat_test.go` + tests.md manual matrix (needs Ollama: not run in this session)
- [x] Row written on every path incl. 499/502/504 — unit-tested via rec assertions
- [x] `go build/vet/staticcheck` green (U1000s pre-exist: auth/keygen helpers)

## Open / assumed

- Open 1 RESOLVED: keep `apiError`.
- Open 2 RESOLVED: adapt to jsonlog Print*.
- Open 3 RESOLVED: auth + rate-limit middleware stubbed — user implements separately.
- ASSUMED — `writeSSE` lives in chat.go next to streamChat.
- ASSUMED — Usage type stays 3-field; cache token columns stay 0 until provider reports them.

## Notes

- Rejected: adapting writeSSE to use writeJSON — wrong content-type, no per-chunk flush.
- Rejected: buffering whole stream then writing — kills streaming UX, TTFT meaningless.
- Risk: http.ResponseController SetWriteDeadline returns error when handler runs behind middleware wrapping ResponseWriter without Unwrap — httprouter does not wrap; fine here.

---

# Updated task list — all days (spec.md)

| Day    | Work                                                                                       | Status                             |
| ------ | ------------------------------------------------------------------------------------------ | ---------------------------------- |
| Oct 24 | GET /v1/healthcheck (version in body)                                                      | ✅ done (`cmd/api/healthcheck.go`) |
| Oct 28 | POST /v1/chat/completions — **this plan** (middleware stubbed; user implements separately)  | 🔨 this plan                       |
| ~      | Docker containerization (`plans/docker-containerization.md`): implemented, acceptance green | ✅ done                            |
| Oct 30 | GET /v1/models — `listModelsHandler` (cmd/api/llm_models.go exists, route commented out)   | ⏳                                 |
| Nov 4  | Retries + fallback — `rec.RetryCount`, `rec.FallbackUsed` columns already exist            | ⏳                                 |
| Nov 6  | Cloud Run Secret Manager for provider keys (spec: never flags, env today)                   | ⏳                                 |
| Nov 11 | Response cache — `rec.CacheHit`, `models` price columns + `requests.cost_usd` already exist | ⏳                                 |
| Nov 13 | GET /v1/usage — `showUsageHandler` (cmd/api/usage.go exists, route commented)               | ⏳                                 |
| Nov 18 | Key management — POST/DELETE /v1/admin/api-keys (cmd/api/api_keys.go exists, routes commented) | ⏳                           |
| ~      | Grafana dashboard over requests table                                                      | ⏳                                 |
| ~      | Cloud Run deploy (`PORT` env default still TODO — see docker plan Open 4)                  | ⏳                                 |

## Amendments
(append-only: date — change — why)
- 10-30 — IMPLEMENTED: tasks 1-3, 6, 7 done + 12 unit tests (`cmd/api/chat_test.go`) + `tests.md` verification guide. Task 4 resolved = keep `apiError`; task 5 resolved = adapt to jsonlog Print*. 499 uses code `client_disconnected`; write-deadline failure logs PrintError, streams continue.
- 10-30 — user reported "writeJSON errors" — actual cause was `classifyUpstreamError` redeclared (user's WIP copy in chat.go + plan's copy in errors.go). Removed chat.go copy; writeJSON needed no change.
- 10-30 — `.git` damaged externally (see docker plan amendments); go.mod/go.sum regenerated via `go mod tidy` after user reported missing go.sum entries (godotenv + ulid/v2 imports from user WIP).