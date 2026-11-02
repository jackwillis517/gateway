# Plan: Stateful authentication token auth (LGF Ch. 9/15) replacing API-key scaffolding

Implements: user request (this plan) | Builds on: `spec.md` §Token authentication (lines 187-232) | Type: refactor+feature | Complexity: M | Systems: `internal/data`, `cmd/api`, `migrations`, dev Postgres `gateway`
Deps: `golang.org/x/crypto` (bcrypt) — NEW; `github.com/lib/pq`, `github.com/julienschmidt/httprouter`, `internal/validator` — existing.

Validate docs/patterns/task sanity before implementing. Import from `gateway.jackwillis517.net/...` (NOT `greenlight.alexedwards.net` from the book).

## Goal

As the single user (Jack), I want `POST /v1/tokens/authorization` to exchange my email+password for a stateful Bearer token (LGF 9.3), and every request to carry `Authorization: Bearer <token>` resolved by an `authenticate` middleware (LGF 9.2) into a user + token in request context, so that API-key scaffolding is gone and usage rows attribute calls to my real user id + token id.

Problem: current auth is scaffolded — `internal/data/api_keys.go` + `api_keys` table + `contextGetAPIKey`, with `contextGetUser` falling back to `AnonymousUser` because no middleware exists.
Solution: LGF-style `tokens` table (SHA-256 hash, 26-char base62 plaintext), `TokenModel` (New/generateToken/Insert/GetForPlaintext/Touch/Revoke), `authenticate` middleware in the chain `recoverPanic(authenticate(rateLimit(router)))`, `createAuthenticationTokenHandler`, LGF context methods.

## Out of scope

- Not: per-token rate limiting keyed by `tokens.id` (spec §Rate limiting) — current per-IP `rateLimit` stays; rework is spec debt.
- Not: `/v1/admin/tokens` endpoints, `GetAllForUser`, `requireScope("admin")` (Nov 18, later).
- Not: `cmd/keygen` CLI — dir exists but empty/untracked; leave it, spec debt.
- Not: signup/activation/password-reset flows (LGF Ch. 16 token scopes beyond `ScopeAuthentication` constant).
- Not: `GET /v1/models`, `GET /v1/usage` routes (still commented in routes.go).
- Not changing: chat completion handler logic, provider layer, usage recording pipeline (only the `api_key_id`→`token_id` rename ripples through it).

## Read first (required)

- `spec.md:187-232` — token auth spec: 26-char base62 token, `GetForPlaintext` join, middleware steps 1-6, `contextSetToken`, Touch-in-background.
- `spec.md:308-360` — schema: `000004_create_tokens_table`, `tokens` columns, `requests.token_id`, "No `password_hash` column" note (line 332 — DEVIATED, see Open/assumed).
- `internal/data/api_keys.go` — the model being replaced (SHA-256 helper, `ScopeChat`/`ScopeAdmin` consts, Insert/GetForPlaintext/Touch/Revoke SQL to mirror).
- `cmd/api/context.go` — current context methods incl. the stub fallback comments.
- `cmd/api/middleware.go:1-25` — `recoverPanic` pattern; `rateLimit` stays untouched.
- `cmd/api/helpers.go:105-120` — `app.background(fn)` helper (recovers panics, waitgroup) for Touch.
- [LGF greenlight Ch. 9.2-9.3](https://lets-go-further.thedevbook.com/) — authenticate middleware + createAuthenticationTokenHandler (user pasted these; import paths must change).

## Create

- `internal/data/validator.go` — `ValidateEmail`, `ValidatePasswordPlaintext` (LGF 9.3).
- `internal/data/tokens.go` — `Token`, `TokenModel`, scopes consts, `ValidateTokenPlaintext`, base62 encoding.
- `cmd/api/tokens.go` — `createAuthenticationTokenHandler`.

## Patterns

Current model to mirror then delete (`internal/data/api_keys.go`):

```go
func hashAPIKey(plaintext string) []byte {
 h := sha256.Sum256([]byte(plaintext))
 return h[:]
}
// Insert: RETURNING id, created_at; GetForPlaintext: INNER JOIN users, revoked_at IS NULL,
// (expires_at IS NULL OR expires_at > now()), users.activated, ErrRecordNotFound on sql.ErrNoRows
```

Error envelope (`cmd/api/errors.go:151-158`):

```go
func (app *application) invalidAuthenticationTokenResponse(w http.ResponseWriter, r *http.Request) {
 w.Header().Set("WWW-Authenticate", "Bearer")
 app.errorResponse(w, r, http.StatusUnauthorized, apiError{
  Message: "invalid or missing API key",
  Type:    errAuthentication,
  Code:    ptr("invalid_api_key"),
 })
}
```

Chain target (`spec.md:50`): `return app.recoverPanic(app.authenticate(app.rateLimit(router)))`

## Tasks

### 0. Dependency: add `golang.org/x/crypto`

- DO: `go get golang.org/x/crypto@latest` then `go mod tidy` (bcrypt needed by `users.go` Password type).
- GOTCHA: repo has no `vendor/` dir today; Makefile `audit` runs `go mod vendor` — run `go mod tidy` not `vendor` during dev, or run `make vendor` at the end.
- VALIDATE: `grep -q "golang.org/x/crypto" go.mod && go build ./...`

### 1. UPDATE `migrations/000002_create_users_table.{up,down}.sql` — add password column

- DO (up): add `password_hash bytea NOT NULL,` after `email citext UNIQUE NOT NULL,` in the `users` CREATE TABLE.
- DO (down): unchanged (`DROP TABLE IF EXISTS users;` already drops it).
- GOTCHA: NOT NULL is fine — single user, seeded with a hash by 000003.
- VALIDATE: `grep -n password_hash migrations/000002_create_users_table.up.sql`

### 2. UPDATE `migrations/000003_seed_user.up.sql` — seed the password

- DO: change INSERT to `INSERT INTO users (name, email, password_hash) VALUES ('Jack', 'jackwillis517@gmail.com', '<bcrypt literal>') ON CONFLICT (email) DO NOTHING;`
- DO: generate the literal once, on your machine, and paste it in (migration must work on a fresh DB):

  ```sh
    cat > /tmp/hash.go <<'EOF'
    package main
    import ("fmt"; "golang.org/x/crypto/bcrypt")
    func main() { h, _ := bcrypt.GenerateFromPassword([]byte("pa55word"), 12); fmt.Println(string(h)) }
    EOF
    go run /tmp/hash.go   # prints $2a$12$... — paste into 000003
  ```

- GOTCHA: cost 12 = LGF. Use a real secret, not "pa55word", if this DB is ever exposed; dev DSN password is `pa55word` — the _user_ password can be anything, stored only as hash. x/crypto bcrypt rejects `$2y$` (htpasswd output) — use the Go snippet above (`$2a$`).
- VALIDATE: `go run /tmp/hash.go` succeeds twice (same cost prefix).

### 3. RENAME+REWRITE `migrations/000004_create_api_keys_table.*` → `migrations/000004_create_tokens_table.{up,down}.sql`

- DO: `git mv migrations/000004_create_api_keys_table.up.sql migrations/000004_create_tokens_table.up.sql` (and .down), then replace up content with spec `spec.md:343-357`:

  ```sql
  CREATE TABLE IF NOT EXISTS tokens (
      id                bigserial PRIMARY KEY,
      user_id           bigint NOT NULL REFERENCES users ON DELETE CASCADE,
      name              text NOT NULL,                 -- 'laptop', 'ci'
      hashed_token      bytea NOT NULL UNIQUE,         -- sha256(plaintext)
      scopes            text[] NOT NULL DEFAULT '{chat}',
      rate_limit_rps    numeric(8,3) CHECK (rate_limit_rps > 0),   -- NULL = flag default
      rate_limit_burst  integer CHECK (rate_limit_burst > 0),
      tokens_per_minute integer CHECK (tokens_per_minute > 0),
      created_at        timestamptz NOT NULL DEFAULT now(),
      last_used_at      timestamptz,
      expires_at        timestamptz,                   -- NULL = never
      revoked_at        timestamptz                    -- NULL = active
  );
  CREATE INDEX IF NOT EXISTS tokens_user_id_idx ON tokens (user_id);
  ```

- DO (down): `DROP TABLE IF EXISTS tokens;`
- GOTCHA: no `key_prefix` column — LGF tokens have no prefix (spec 191). Keep version number `000004` (user said overwrite in place; dev DB repair in Task 12).

### 4. UPDATE `migrations/000006_create_requests_table.up.sql` — `api_key_id` → `token_id`

- DO: replace `api_key_id bigint REFERENCES api_keys ON DELETE SET NULL,` with `token_id bigint REFERENCES tokens ON DELETE SET NULL,   -- which token made the call` (matches `spec.md:378`).
- VALIDATE: `grep -rn "api_key" migrations/` → no output.

### 5. UPDATE `internal/validator/validator.go` — EmailRX (LGF 9.3)

- DO: add exported `var EmailRX = regexp.MustCompile("^[a-zA-Z0-9.!#$%&'*+\\/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$")`(LGF's regex) +`"regexp"` import.
- VALIDATE: `go build ./internal/validator/`

### 6. CREATE `internal/data/validator.go`

- DO: LGF 9.3 validators, package `data`:
  - `ValidateEmail(v *validator.Validator, email string)` — Check not empty + `validator.Matches(email, validator.EmailRX)`.
  - `ValidatePasswordPlaintext(v *validator.Validator, password string)` — Check not empty, `len >= 8`, `len <= 72` (bcrypt byte limit).
- DO: import `gateway.jackwillis517.net/internal/validator`.

### 7. UPDATE `internal/data/users.go` — LGF `password` type + `GetByEmail` scan

- DO: add LGF Ch. 9 type (unexported, same package):

  ```go
  type password struct {
      plaintext *string
      hash      []byte
  }
  func (p *password) Set(plaintextPassword string) error {
      hash, err := bcrypt.GenerateFromPassword([]byte(plaintextPassword), 12)
      if err != nil { return err }
      p.plaintext = &plaintextPassword
      p.hash = hash
      return nil
  }
  func (p *password) Matches(plaintextPassword string) (bool, error) {
      if p.hash == nil { return false, nil }
      err := bcrypt.CompareHashAndPassword(p.hash, []byte(plaintextPassword))
      if err != nil {
          if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) { return false, nil }
          return false, err
      }
      return true, nil
  }
  ```

- DO: add `Password password` field to `User` struct (json:"-" behavior via unexported type; keep existing tags).
- DO: `GetByEmail` SELECT adds `users.password_hash` (after email), Scan adds `&user.Password.hash` in the same position.
- VALIDATE: `go build ./...` (after Task 8/9 too — cross-file).

### 8. CREATE `internal/data/tokens.go`, REMOVE `internal/data/api_keys.go`

- DO: `git rm internal/data/api_keys.go`; create `tokens.go`, package `data`, with:
  - Constants: `ScopeAuthentication = "authentication"`, `ScopeActivation = "activation"` (LGF Ch. 15) AND keep `ScopeChat = "chat"`, `ScopeAdmin = "admin"` (currently in api_keys.go:17-20; future admin endpoints).
  - `type Token struct` per `spec.md:195-204`: `Plaintext string \`json:"token,omitempty"\``,`Hash []byte \`json:"-"\``,`ID int64 \`json:"id"\``,`UserID int64 \`json:"-"\``,`Name string \`json:"name"\``,`Scopes []string \`json:"scopes"\``,`RateLimitRPS *float64 \`json:"rate_limit_rps"\``,`RateLimitBurst*int \`json:"rate_limit_burst"\``,`TokensPerMinute *int \`json:"tokens_per_minute"\``,`CreatedAt time.Time \`json:"created_at"\``,`LastUsedAt*time.Time \`json:"last_used_at"\``,`ExpiresAt *time.Time \`json:"expires_at"\``,`RevokedAt*time.Time \`json:"revoked_at"\``.
  - `type TokenModel struct { DB *sql.DB }`.
  - `func hashToken(plaintext string) []byte` — `sha256.Sum256`, mirror old `hashAPIKey` (api_keys.go:52-55).
  - `func generateToken(userID int64, name string, scopes []string, ttl time.Duration) (*Token, error)` — 16 `crypto/rand` bytes → base62-encode (big.Int divmod 62, alphabet `A-Za-z0-9`, left-pad `'A'` to 26 chars) → `Token{Plaintext, Hash: hashToken(...), UserID, Name, Scopes}`; if `ttl > 0` set `ExpiresAt`.
  - `func (m TokenModel) New(userID int64, name string, ttl time.Duration, scopes ...string) (*Token, error)` — calls `generateToken` + `m.Insert`. NOTE: pasted LGF call site is `New(user.ID, 24*time.Hour, scope)` without name; adapted because spec's `name text NOT NULL` needs a value. Handler passes `name = "authorization"`.
  - `func (m TokenModel) Insert(t *Token) error` — mirror old api_keys.go:66-92 but: table `tokens`, columns `(user_id, name, hashed_token, scopes, rate_limit_rps, rate_limit_burst, tokens_per_minute, expires_at)` = `$1..$8`, `pq.Array(t.Scopes)`, `RETURNING id, created_at`.
  - `func (m TokenModel) GetForPlaintext(plaintext string) (*User, *Token, error)` — mirror old api_keys.go:95-160: `SELECT users.id, users.created_at, users.name, users.email, users.activated, users.version, tokens.id, tokens.user_id, tokens.name, tokens.scopes, tokens.rate_limit_rps, tokens.rate_limit_burst, tokens.tokens_per_minute, tokens.created_at, tokens.last_used_at, tokens.expires_at FROM tokens INNER JOIN users ON users.id = tokens.user_id WHERE tokens.hashed_token = $1 AND tokens.revoked_at IS NULL AND (tokens.expires_at IS NULL OR tokens.expires_at > now()) AND users.activated`; scan into `&user...` + `&token...` + `pq.Array(&token.Scopes)`; `sql.ErrNoRows` → `ErrRecordNotFound`. (No `ScopeAuthentication` WHERE clause — this repo's tokens ARE the API credentials, spec 209; scope value only matters for future requireScope.)
  - `func (m TokenModel) Touch(id int64) error` — old api_keys.go:191-203 SQL, table `tokens`.
  - `func (m TokenModel) Revoke(id, userID int64) error` — old api_keys.go:209-233 SQL, table `tokens`.
  - `func ValidateTokenPlaintext(v *validator.Validator, tokenPlaintext string)` — LGF 15.4: Check not empty; Check `len == 26`, "must be 26 bytes long".
- GOTCHA: `GetAllForUser` and `Prefix`/`key_prefix` do NOT carry over — spec 207-211 lists only the five funcs above.

### 9. UPDATE `internal/data/models.go`

- DO: `Models` struct: replace `APIKey APIKeyModel` with `Tokens TokenModel`; `NewModels`: `Tokens: TokenModel{DB: db}`. (Field `Tokens` matches pasted handler `models.Tokens.New`.)

### 10. UPDATE `internal/data/requests.go` — `APIKeyID` → `TokenID`

- DO: rename struct field `APIKeyID *int64` → `TokenID *int64` (requests.go:19); Insert SQL column `api_key_id` → `token_id` (requests.go:21) — placeholder `$3` position unchanged; args slice `r.APIKeyID` → `r.TokenID` (requests.go:~63).

### 11. UPDATE `cmd/api/context.go` — LGF context methods

- DO: keys: `userContextKey`, `tokenContextKey = contextKey("token")` (replace `apiKeyContextKey`).
- DO: `contextSetUser` unchanged. `contextGetUser` → LGF panic version (drop the AnonymousUser fallback + stub comment):

  ```go
  func (app *application) contextGetUser(r *http.Request) *data.User {
      user, ok := r.Context().Value(userContextKey).(*data.User)
      if !ok {
          panic("missing user in request context")
      }
      return user
  }
  ```

  Justification: `authenticate` (Task 13) runs on EVERY request in the chain and sets `data.AnonymousUser` when the header is empty (spec 217), so a missing user is a programming error (route registered outside the chain); `recoverPanic` is outermost and converts the panic to a 500.
- DO: `contextSetToken` / `contextGetToken` replacing the APIKey pair; `contextGetToken` stays nil-safe (no panic) — anonymous requests legitimately carry no token (healthcheck; future 429 recording, spec 258).
- VALIDATE (Tasks 7-11): `go build ./... && go vet ./...`

### 12. Dev DB recovery: `api_keys` → `tokens` (destructive, pre-prod)

- DO: old `api_keys` table is live (migration version 7, clean), `requests.api_key_id` references it. `api_keys` has 0 rows; `requests` has 18 dev rows, ALL `api_key_id` NULL (verified), so the transition is lossless except the 18 dev usage rows. Recommended: full reset so DB provably matches the rewritten migrations:

  ```sh
    # optional backup of dev usage rows:
    PGPASSWORD=pa55word pg_dump -h 127.0.0.1 -U gateway -d gateway -t requests > /tmp/requests-backup.sql
    # reset (drops view first, then tables, then migrate bookkeeping):
    PGPASSWORD=pa55word psql -h 127.0.0.1 -U gateway -d gateway \
      -c "DROP VIEW IF EXISTS daily_usage;" \
      -c "DROP TABLE IF EXISTS requests, api_keys, models, users CASCADE;" \
      -c "DROP TABLE IF EXISTS schema_migrations;"
    # re-run all migrations against the NEW files:
    yes | make db/migrations/up    # or: migrate -path ./migrations -database "$GATEWAY_DB_DSN" up
  ```

- GOTCHA: overwriting `000004`/`000006` in place means `migrate up` alone would be a no-op (DB at v7) — the reset + re-run is what makes the dev DB match the files. Alternative (keeps the 18 rows): hand-surgery — `ALTER TABLE requests DROP COLUMN api_key_id;`, `DROP TABLE api_keys;`, `psql -f migrations/000004_create_tokens_table.up.sql`, `ALTER TABLE requests ADD COLUMN token_id bigint REFERENCES tokens ON DELETE SET NULL;` — but then schema drift lives only in the DB; reset is cleaner for a disposable dev DB.
- VALIDATE:

  ```sh
    PGPASSWORD=pa55word psql -h 127.0.0.1 -U gateway -d gateway \
      -c "SELECT version, dirty FROM schema_migrations;" \
      -c "\dt" -c "\d tokens" -c "\d requests" | grep -E "version|dirty|tokens|token_id"
    # expect version 7 dirty f; tables list tokens (no api_keys); requests has token_id
  ```

### 13. UPDATE `cmd/api/middleware.go` — add `authenticate` (LGF 9.2, adapted)

- DO: add after `recoverPanic`:

  ```go
    func (app *application) authenticate(next http.Handler) http.Handler {
     return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
      w.Header().Add("Vary", "Authorization")
      authorizationHeader := r.Header.Get("Authorization")
      if authorizationHeader == "" {
       r = app.contextSetUser(r, data.AnonymousUser)   // spec 217: healthcheck stays open
       next.ServeHTTP(w, r)
       return
      }
      headerParts := strings.Split(authorizationHeader, " ")
      if len(headerParts) != 2 || headerParts[0] != "Bearer" {
       app.invalidAuthenticationTokenResponse(w, r)
       return
      }
      token := headerParts[1]
      v := validator.New()
      if data.ValidateTokenPlaintext(v, token); !v.Valid() {
       app.invalidAuthenticationTokenResponse(w, r)
       return
      }
      user, t, err := app.models.Tokens.GetForPlaintext(token)   // adapted: joins users, returns both
      if err != nil {
       switch {
       case errors.Is(err, data.ErrRecordNotFound):
        app.invalidAuthenticationTokenResponse(w, r)
       default:
        app.serverErrorResponse(w, r, err)
       }
       return
      }
      r = app.contextSetUser(r, user)
      r = app.contextSetToken(r, t)                               // spec 220
      if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > time.Minute {
       app.background(func() { _ = app.models.Tokens.Touch(t.ID) })  // spec 221
      }
      next.ServeHTTP(w, r)
     })
    }
  ```

- DO: add `requireAuthenticatedUser` right below (LGF Ch. 16, one guard — spec 223 says it wraps the chat route; without it an anonymous request would run the chat handler and write a usage row attributed to `AnonymousUser.ID == 1 == Jack`, polluting usage):

  ```go
    func (app *application) requireAuthenticatedUser(next http.Handler) http.Handler {
     return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
      if app.contextGetUser(r).IsAnonymous() {
       app.authenticationRequiredResponse(w, r)   // existing helper, 401 missing_api_key
       return
      }
      next.ServeHTTP(w, r)
     })
    }
  ```

- DO imports: `"errors"`, `"strings"`, `"time"`, `gateway.jackwillis517.net/internal/data`, `gateway.jackwillis517.net/internal/validator`.
- PATTERN: `cmd/api/middleware.go:8-20` (recoverPanic wrapping shape), `internal/data/users.go:30` (IsAnonymous).
- GOTCHA: LGF's version calls `GetForToken(scope, token)` and only sets the user; this repo's spec version returns `(*User, *Token, error)` and sets both in context. Touch check is Go-side + SQL-side throttled (both) — keep SQL throttle in Touch.

### 14. CREATE `cmd/api/tokens.go` — `createAuthenticationTokenHandler` (LGF 9.3, cleaned)

- DO: the user's pasted LGF handler has a syntax slip (duplicate `var input struct {`) — implement the cleaned version:

  ```go
    func (app *application) createAuthenticationTokenHandler(w http.ResponseWriter, r *http.Request) {
     var input struct {
      Email    string `json:"email"`
      Password string `json:"password"`
     }
     err := app.readJSON(w, r, &input)
     if err != nil {
      app.badRequestResponse(w, r, err)
      return
     }
     v := validator.New()
     data.ValidateEmail(v, input.Email)
     data.ValidatePasswordPlaintext(v, input.Password)
     if !v.Valid() {
      app.failedValidationResponse(w, r, v.Errors)
      return
     }
     user, err := app.models.Users.GetByEmail(input.Email)
     if err != nil {
      switch {
      case errors.Is(err, data.ErrRecordNotFound):
       app.invalidCredentialsResponse(w, r)
      default:
       app.serverErrorResponse(w, r, err)
      }
      return
     }
     match, err := user.Password.Matches(input.Password)
     if err != nil {
      app.serverErrorResponse(w, r, err)
      return
     }
     if !match {
      app.invalidCredentialsResponse(w, r)
      return
     }
     token, err := app.models.Tokens.New(user.ID, "authorization", 24*time.Hour, data.ScopeAuthentication)
     if err != nil {
      app.serverErrorResponse(w, r, err)
      return
     }
     err = app.writeJSON(w, http.StatusCreated, envelope{"authentication_token": token}, nil)
     if err != nil {
      app.serverErrorResponse(w, r, err)
     }
    }
  ```

- GOTCHA: `user.Password` is unexported type `password` — `Matches`/`Set` are defined in package `data`, user struct same package, so handler calls work through the exported method on the field. 201 + `{"authentication_token": {...token json...}}` — token serializes with `token` key for plaintext (`json:"token,omitempty"`), never the hash.
- VALIDATE: `go build ./... && go vet ./...`

### 15. UPDATE `cmd/api/errors.go` — 401 helpers

- DO: add `invalidCredentialsResponse` (LGF 9.3), OpenAI envelope:

  ```go
    func (app *application) invalidCredentialsResponse(w http.ResponseWriter, r *http.Request) {
     w.Header().Set("WWW-Authenticate", "Bearer")
     app.errorResponse(w, r, http.StatusUnauthorized, apiError{
      Message: "invalid authentication credentials",
      Type:    errAuthentication,
      Code:    ptr("invalid_credentials"),
     })
    }
  ```

- DO: update `invalidAuthenticationTokenResponse` (errors.go:151-158): `Code: ptr("invalid_api_key")` → `ptr("invalid_token")` (spec 219), `Message: "invalid or missing API key"` → `"invalid, revoked or expired token"`. `Type` stays `errAuthentication`, `WWW-Authenticate` header stays.
- DO NOT: touch `authenticationRequiredResponse` (used by `requireAuthenticatedUser`, Task 13) or `notPermittedResponse`.
- VALIDATE: `go build ./...`

### 16. UPDATE `cmd/api/routes.go`

- DO:

  ```go
    router.HandlerFunc(http.MethodGet, "/v1/healthcheck", app.healthcheckHandler)
    router.HandlerFunc(http.MethodPost, "/v1/tokens/authorization", app.createAuthenticationTokenHandler)
    router.Handler(http.MethodPost, "/v1/chat/completions",
     app.requireAuthenticatedUser(http.HandlerFunc(app.createChatCompletionHandler)))
    // leave the commented /v1/models, /v1/usage, /v1/admin/api-keys TODO comments; fix their text to /v1/admin/tokens
    return app.recoverPanic(app.authenticate(app.rateLimit(router)))
  ```

- PATTERN: current `cmd/api/routes.go:10-18`.
- GOTCHA: httprouter `HandlerFunc` takes `http.HandlerFunc`; a middleware-wrapped handler needs `router.Handler(method, path, http.Handler)` (LGF 16 uses this for wrapped routes). `authenticate` wraps EVERYTHING incl. healthcheck (empty header → anonymous → pass, spec 217); `rateLimit` stays innermost for now (per-IP); spec 50-53 wants authenticate before rateLimit — done by wrapping order.

### 17. UPDATE `cmd/api/chat.go` — context getters + field rename

- DO: line 33 `UserID: app.contextGetUser(r).ID` unchanged (works: authenticate guarantees a user).
- DO: lines 38-40: `if key := app.contextGetAPIKey(r); key != nil { rec.APIKeyID = &key.ID }` → `if t := app.contextGetToken(r); t != nil { rec.TokenID = &t.ID }`.
- VALIDATE: `go build ./... && go vet ./... && gofmt -l .` (no output)

### 18. UPDATE `spec.md` — reconcile with recommendation (a)

- DO: §Token authentication (~line 225-229): replace the `cmd/keygen` CLI paragraph with the `POST /v1/tokens/authorization` endpoint (email+password → 24h token, `scopes {"authentication"}`), note keygen dropped (or mark as future debt).
- DO: line ~332: replace "No `password_hash` column..." with the deviation note: `password_hash bytea NOT NULL` on users, bcrypt cost 12, seeded by `000003`.
- DO: routes table (~line 77-82): add `| POST | /v1/tokens/authorization | none (email+password) | Nov 18 |`.
- VALIDATE: `grep -n "tokens/authorization\|password_hash" spec.md`

## Tests

No test files exist in repo (none found). Seams: middleware chain (http.Handler), `TokenModel` over real Postgres, context getters. New unit tests NOT required by user; if added later: `TestTokenModel_*` mirroring LGF 15.x, `TestAuthenticate` with httptest. Manual end-to-end below is the acceptance gate.

## Validation

```sh
# L1-L3
go build ./... && go vet ./... && gofmt -l .
# migration apply (fresh reset already done in Task 12)
yes | make db/migrations/up
# run server
make run/api   # port 4005
```

L5 manual, in order:

```sh
# 1. healthcheck still open, no header
curl -s localhost:4005/v1/healthcheck | jq    # {"status":"available",...} 200

# 2. wrong password → 401 invalid_credentials
curl -si -X POST localhost:4005/v1/tokens/authorization \
  -d '{"email":"jackwillis517@gmail.com","password":"wrong"}' | grep -E "HTTP|code"

# 3. correct creds → 201, capture token
TOKEN=$(curl -s -X POST localhost:4005/v1/tokens/authorization \
  -d '{"email":"jackwillis517@gmail.com","password":"pa55word"}' \
  | jq -r '.authentication_token.token')
echo "$TOKEN"   # 26 chars

# 4. use token on chat → row written with real user id + token id
curl -s -H "Authorization: Bearer $TOKEN" localhost:4005/v1/chat/completions \
  -H 'Content-Type: application/json' -d @req.json | jq '.id'
PGPASSWORD=pa55word psql -h 127.0.0.1 -U gateway -d gateway \
  -c "SELECT r.user_id, r.token_id, r.status_code, u.email, t.name, t.scopes
      FROM requests r JOIN users u ON u.id=r.user_id JOIN tokens t ON t.id=r.token_id
      ORDER BY r.id DESC LIMIT 1;"
# expect user_id=1 (real row, not anonymous fallback), token_id=<tokens.id>, scopes={authentication}

# 5. garbage bearer → 401 invalid_token, NO requests row (spec: 401s not stored)
curl -si -H "Authorization: Bearer garbage" localhost:4005/v1/chat/completions \
  -d @req.json | grep -E "HTTP|invalid_token"

# 6. no header on chat → 401 missing_api_key (requireAuthenticatedUser), no row
curl -si -X POST localhost:4005/v1/chat/completions -d @req.json | grep -E "HTTP|missing"

# 7. Touch throttling
PGPASSWORD=pa55word psql -h 127.0.0.1 -U gateway -d gateway \
  -c "SELECT id, last_used_at FROM tokens ORDER BY id DESC LIMIT 1;"   # set after step 4
```

Also verify `SELECT count(*) FROM api_keys` fails (table gone) and `schema_migrations` shows version 7 dirty f.

## Acceptance

- [ ] `POST /v1/tokens/authorization` returns 201 `{"authentication_token":{"token":"<26 chars>",...}}` for seeded email+password; 401 `invalid_credentials` otherwise; 400 envelope for malformed JSON.
- [ ] `Authorization: Bearer <token>` authenticates `/v1/chat/completions`; usage row has correct `user_id` (1) + `token_id`.
- [ ] Empty header → anonymous passes to healthcheck (200); chat without header → 401 `missing_api_key`; bad/expired/revoked token → 401 `invalid_token`; 401s write NO requests rows.
- [ ] `api_keys` gone (code + DB), `tokens`/`token_id` match `spec.md` schema, migrations 1-7 apply clean on fresh DB.
- [ ] `go build ./... && go vet ./...` green; no `api_key` refs in code (only `OPENAI_API_KEY` env stays).

## Open / assumed

- ASSUMED — **Password auth (the big fork)**: recommendation (a) adopted: `password_hash bytea NOT NULL` on `users` (LGF `password` type, bcrypt cost 12) + seeded in `000003`, matching the pasted LGF handler literally. Spec `spec.md:7,225,332` still says "no password"; Task 18 amends spec. x/crypto not previously a dep (verified) — Task 0 adds it. Open to override: see Q1.
- ASSUMED — **Scope model**: pasted LGF code mints `ScopeAuthentication` tokens; spec's `scopes text[]` defaults `{chat}` and future admin endpoints need `{admin}`. Kept LGF constants (`ScopeAuthentication`, `ScopeActivation`) + existing `ScopeChat`/`ScopeAdmin`; the new endpoint stores `{"authentication"}` (middleware ignores scope — `GetForPlaintext` has no scope WHERE). No consumer checks scopes today (verified: `HasScope`/`notPermittedResponse` unused). When `requireScope("chat"/"admin")` lands (Nov 18), revisit what `/v1/tokens/authorization` mints.
- ASSUMED — **TTL 24h** on tokens from the endpoint (LGF paste) despite spec's "expires_at NULL = never" for CLI-minted credentials. Token expiry forces daily re-auth; if annoying, mint non-expiring tokens later via keygen/admin endpoint.
- ASSUMED — **requireAuthenticatedUser added** (spec 223) though the user's 5-item list didn't name it — without it anonymous requests hit the chat handler and write usage rows as `AnonymousUser.ID=1`, indistinguishable from Jack (same id). Dropped on request.
- ASSUMED — **Dev DB reset** (18 disposable dev usage rows lost, optional pg_dump backup in Task 12) instead of surgical ALTER. Pre-prod single-user; golang-migrate v7 clean.
- ASSUMED — **Endpoint path `/v1/tokens/authorization`** as user wrote it (LGF book names it `/v1/tokens/authentication`). Response key stays `authentication_token` per pasted handler.
- ASSUMED — **Per-token rate limiting out of scope** (spec 236-291 wants `Reserve()` keyed by `tokens.id` after authenticate); current per-IP `Allow()` limiter unchanged. Chain order already matches spec 50.
- ASSUMED — **keygen CLI dropped** from spec tree (Task 18); empty `cmd/keygen/` dir left on disk (untracked by git, no files).

## Notes

- Rejected: new migration `000008` renaming api_keys→tokens — dev DB is disposable, old api_keys table has 0 rows, user explicitly said "overwrite needed migrations"; a rename migration would keep dead history for a pre-prod single-user system.
- Rejected: passwordless token issuance (tension 1 option b) — insecure, unauthenticated token minting; worse deviation from LGF than adding password_hash.
- Rejected: keeping `contextGetUser` non-panicking fallback — comment says fallback exists only because middleware was stubbed; with real `authenticate` guaranteeing a user, LGF panic + `recoverPanic` outermost catches programming errors loudly instead of silently attributing anonymous usage to id 1.
- Rejected: `GetAllForUser`/`HasScope` carryover — spec 207-211 lists exactly five TokenModel methods; admin endpoints are Nov 18.
- Sequencing risk: Tasks 1-4 (migrations) + Task 12 (DB reset) are independent of Go tasks; do DB reset LAST or FIRST, not mid-refactor (code must compile against one schema at a time — old code + new DB breaks `recordUsage` until Task 17 done).
- `requests.api_key_id` had 0 non-null values (verified) — rename loses nothing even without reset.
- golang-migrate dirty-state: DB is clean (v7, dirty=f), no `force` needed after reset.

## Amendments

(append-only)

---

## Post-implementation review (gap audit)

Code + DB + spec largely landed (working tree, uncommitted). Verified green: `go build ./...`, `go vet ./...`, `gofmt -l .` empty; DB v7 `dirty=f`, `tokens` + `requests.token_id` live, `api_keys` gone, `users` row has `$2a$12$` hash; middleware/handler/context/errors/routes/requests match this plan (incl. `requireAuthenticatedUser`, `invalid_token`, `invalidCredentialsResponse`, panic `contextGetUser`, base62 + `'A'` pad to 26, `Touch` SQL throttle, `Revoke`).

Remaining gaps — none touch Go source; tasks below close them:

### G1. CREATE `.envrc` — make is currently broken
- DO: `Makefile:2` is `include .envrc`; file missing → every make target dies: `make: *** .envrc: No such file or directory` (verified). Create:
  ```
  export GATEWAY_DB_DSN='postgres://gateway:pa55word@127.0.0.1:5432/gateway?sslmode=disable'
  export OLLAMA_BASE_URL='http://localhost:11434'   # seeded model 000007 is ollama/qwen3.5:4b
  # export OPENAI_API_KEY='...'                     # optional; godotenv .env also works
  ```
- WHY 127.0.0.1:5432: gateway DB verified reachable on local postgres 5432; compose maps container DB to host 5433 (docker-compose.yml:9), `db:5432` only inside the compose network (docker-compose.yml:29).
- VALIDATE: `make help >/dev/null && echo MAKE-OK`

### G2. UPDATE `.gitignore` — keep `.envrc` out of git
- DO: append `.envrc` (holds DSN password + optional OPENAI key). `.env` already ignored (gitignore:1).
- VALIDATE: `git check-ignore .envrc`

### G3. UPDATE `curl.md:6-8` — stale stub note + wrong env var
- DO: replace lines 7-8 ("Auth middleware is still stubbed... $GATEWAY_KEY") with the real flow: mint token first:
  ```sh
  GW_TOKEN=$(curl -s -X POST localhost:4005/v1/tokens/authorization \
    -d '{"email":"jackwillis517@gmail.com","password":"<seeded password>"}' \
    | jq -r '.authentication_token.token')
  ```
  then add `-H "Authorization: Bearer $GW_TOKEN"` to every chat command (spec 228 uses `$GW_TOKEN`, not `$GATEWAY_KEY`).
- VALIDATE: `grep -c "GATEWAY_KEY" curl.md` → 0

### G4. UPDATE `spec.md` — three leftovers
- `spec.md:458`: struct example still `APIKeyID *int64` → `TokenID *int64` (matches internal/data/requests.go:14).
- `spec.md:37` tree: `context.go contextSetUser / contextGetUser / contextGetToken` → add `contextSetToken` (matches cmd/api/context.go:29).
- `spec.md:228`: `localhost:4000` → `localhost:4005` (server `-port` default 4005, main.go; curl.md agrees).
- VALIDATE: `grep -n "APIKeyID\|4000" spec.md` → empty

### G5. RUN the L5 manual validation (never executed end-to-end)
- Pre: `.envrc` exists (G1), Ollama up (`OLLAMA_BASE_URL` reachable, model `qwen3.5:4b` pulled) or expect 502/`model_not_found` upstream path instead of 200.
- `make run/api` then, in order:
  1. `curl -si localhost:4005/v1/healthcheck` → 200, no header needed.
  2. `curl -si -X POST localhost:4005/v1/tokens/authorization -d '{"email":"jackwillis517@gmail.com","password":"wrong"}'` → 401 `invalid_credentials`.
  3. Correct creds → 201, `.authentication_token.token` is 26 chars.
  4. `curl -s -H "Authorization: Bearer $GW_TOKEN" localhost:4005/v1/chat/completions -H 'Content-Type: application/json' -d @req.json` → chatcmpl id; psql: `SELECT r.user_id, r.token_id, t.scopes FROM requests r JOIN tokens t ON t.id = r.token_id ORDER BY r.id DESC LIMIT 1` → user_id=1, token_id set, scopes={authentication}.
  5. `-H "Authorization: Bearer garbage"` → 401 `invalid_token`, NO new requests row.
  6. No header → 401 `missing_api_key`, NO new requests row (`requireAuthenticatedUser`, not the handler).
  7. Two quick authed calls ≥1 min apart → single `last_used_at` bump (Touch throttle, SQL + Go both).
- VALIDATE: all seven outcomes; `SELECT version, dirty FROM schema_migrations` → 7/f.

### G6. NOTE (no action) — pre-existing `make audit` bug
- Makefile `audit` runs `go test -race -vet=off ./bin/` — `bin/` holds the built binary, not packages (`go test ./bin/` → "no Go files"). Also runs `staticcheck` + `vendor` (no `vendor/` dir; `go build` works without). Latent, out of scope here; fix when tests exist (`./...`).
- Also NOTE: repo still has zero tests; LGF 15.x `TestTokenModel_*` / `TestAuthenticate` are future debt (spec silent on tests).

## Amendments (append-only)
- <date> — Post-implementation audit appended (G1-G6): implementation of Tasks 0-18 verified against plan; DB reset+remigrate (Task 13/12) confirmed done (v7 clean, tokens live); Q1-Q4 from "Open / assumed" all settled as recommended (password auth (a), LGF scope consts + `{"authentication"}` minted, in-place 000004 overwrite + full dev reset, `requireAuthenticatedUser` included) — user directive "address the remaining gaps" taken as acceptance of recommendations.
- <date> — `cmd/keygen/` still an empty untracked dir on disk (spec tree updated to drop it); leave or `rmdir`, cosmetic.
