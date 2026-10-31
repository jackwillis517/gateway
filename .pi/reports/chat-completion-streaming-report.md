# Report: chat completion + streaming
Plan: plans/chat-completion-streaming.md | Branch: master (uncommitted) | Status: COMPLETE (unit level)

## Summary
Filled completeChat/streamChat stubs, added classifyUpstreamError (user-provided verbatim), recordTokens, writeSSE; fixed 2 bugs (ns-duration, string(int) logs); widened writeJSON to any; anonymous-user shim. 12 unit tests green; manual curl matrix left to user (tests.md).

## Tasks
1. `cmd/api/chat.go` (UPDATE): both stub bodies real; `SetWriteDeadline` per-response; lazy SSE `begin()`; in-stream error + `[DONE]`; ttft/upstream latency recorded; duration bug `* time.Second`; strconv log fix
2. `cmd/api/errors.go` (UPDATE): `classifyUpstreamError` pasted verbatim (user message); imports context/errors/provider
3. `cmd/api/chat.go` (UPDATE): `recordTokens`, `writeSSE`
4. `cmd/api/helpers.go` (UPDATE): writeJSON `envelope` → `any`
5. `cmd/api/context.go` (UPDATE): contextGetUser panic → `data.AnonymousUser` fallback (middleware stubbed; user implements)
6. `cmd/api/chat_test.go` (CREATE): 12 tests
7. `tests.md` (CREATE): verification guide

## Tests
`go test ./cmd/api/ -v`: 12/12 PASS — classify table (6), recordTokens, complete success/502/499, stream success/usage-hidden/empty-stream/pre-chunk-error/in-stream-error/499, writeJSON-any, context shim.

## Validation
gofmt clean · `go build ./...` green · `go vet ./...` green · `go test` 12/12 · staticcheck: only pre-existing U1000s (auth/keygen helpers for future days).

## Deviations
- Task 4 decision: kept `apiError{Code *string}` (15 call sites), `ptr(code)` — as recommended.
- Task 5 decision: adapted to jsonlog `PrintError(map)`; SetWriteDeadline failure logs PrintError, stream continues.
- 499 error code is `client_disconnected` (user's function), plan draft said `client_closed_request` — user function wins.
- TestCompleteChatSuccess/StreamChatSuccess: test recs initialized `StatusCode: 200` as the real handler does (handler sets it at creation).
- Follow-up: user-reported "writeJSON errors" were actually `classifyUpstreamError` redeclared (user WIP copy in chat.go vs plan copy in errors.go) — removed chat.go copy.

## Issues
- Manual curl matrix (tests.md §2) needs Ollama + running Postgres — not run this session.
- Anthropic cleanup still pending: `ProviderAnthropic` const + `models.provider` CHECK migration (anthropic.go deleted from tree).
- `.git` damaged externally mid-session; rebuilt (see docker report). `go mod tidy` run after user WIP added godotenv + ulid/v2 imports (go.sum entries were missing).