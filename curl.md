# curl commands for `POST /v1/chat/completions`

The gateway listens on port `4005` by default (`-port` flag). The seeded model
is `ollama/qwen3.5:4b` (alias `qwen-local`), served by the `ollama`
provider, with a `max_output_tokens` ceiling of `32768`.

Mint a token first, then send it on every authenticated request:

```sh
GW_TOKEN=$(curl -s -X POST localhost:4005/v1/tokens/authentication \
  -H 'Content-Type: application/json' \
  -d '{"email":"jackwillis517@gmail.com","password":"<seeded password>"}' \
  | jq -r '.authentication_token.token')
```

Add `-H "Authorization: Bearer $GW_TOKEN"` to every command below. The token expires after 24h; mint a new one when it does. Keep the mint response around — the override test needs the token's `id`:

```sh
MINT=$(curl -s -X POST localhost:4005/v1/tokens/authentication \
  -H 'Content-Type: application/json' \
  -d '{"email":"jackwillis517@gmail.com","password":"<seeded password>"}')
GW_TOKEN=$(echo "$MINT" | jq -r '.authentication_token.token')
GW_TOKEN_ID=$(echo "$MINT" | jq -r '.authentication_token.id')
```

## GET /v1/models

OpenAI-compatible model list. `id` is the name clients send as `"model"`.

### List models with a token

```bash
curl -s http://localhost:4005/v1/models \
  -H "Authorization: Bearer $GW_TOKEN" | jq
# expect: {"object": "list", "data": [
#   {"id": "ollama/qwen3.5:4b", "object": "model", "created": <unix ts>, "owned_by": "ollama"}]}
```

### Without a token — 401

```bash
curl -si http://localhost:4005/v1/models | grep -E 'HTTP|code'
# expect: HTTP/1.1 401 Unauthorized, "code": "missing_token"
```

## Non-streaming

### Basic

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "Say hello in one sentence."}]
  }'
```

### All optional parameters

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "ollama/qwen3.5:4b",
    "messages": [
      {"role": "system", "content": "You are a terse assistant."},
      {"role": "user", "content": "Name two primary colors."}
    ],
    "max_tokens": 100,
    "temperature": 0.7,
    "top_p": 0.9,
    "stop": ["\n"]
  }'
```

### Verify response rewriting

The gateway must return its own `X-Request-Id` header and rewrite `id`/`model`
in the body — never the provider's values. Include headers with `-i` and check:

```bash
curl -si http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "ping"}],
    "max_tokens": 5
  }'
```

- `X-Request-Id` present and matches `"id"` in body (starts with `chatcmpl-`)
- `"model"` in body is the gateway name/alias, not the upstream model

## Streaming (SSE)

### Basic stream

```bash
curl -sN http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "Count from 1 to 5."}],
    "stream": true
  }'
```

`-N` disables curl buffering so chunks arrive as they are sent. Expect
`Content-Type: text/event-stream` and a terminal
`data: [DONE]` line.

### Stream with usage

`stream_options.include_usage: true` must produce a final usage chunk before
`[DONE]` (after the `finish_reason` chunk, with empty `choices`):

```bash
curl -sN http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "Say hi."}],
    "stream": true,
    "stream_options": {"include_usage": true}
  }'
```

### Stream without usage requested

Same as above with `"stream_options"` omitted — no usage chunk should reach the
client, but the gateway still requests usage upstream (internal check; from curl
just verify no usage chunk appears):

```bash
curl -sN http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "Say hi."}],
    "stream": true
  }'
```

## Validation

All of these return `400` with `type: "invalid_request_error"` unless noted.
The `param` field of the error names the first failing field (sorted).

### Missing model — earliest failure path

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "hi"}]
  }'
# expect: "model": "must be provided"
```

### Unknown model — 404, not 400

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-99-turbu
    "messages": [{"role": "user", "content": "hi"}]
  }'
# expect 404, "the model \"gpt-99-turbo\" does not exist or is disabled"
```

### Empty / missing messages

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": []
  }'
# expect: "messages": "must contain at least 1 message"
```

### Invalid role

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "robot", "content": "hi"}]
  }'
# expect: "messages[0].role": "must be one of: system, user, assistant"
```

### Empty content

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": ""}]
  }'
# expect: "messages[0].content": "must be a non-empty string"
```

### max_tokens below minimum

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "hi"}],
    "max_tokens": 0
  }'
# expect: "max_tokens": "must be at least 1"
```

### max_tokens above the model ceiling

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "hi"}],
    "max_tokens": 999999
  }'
# expect: "max_tokens": "must not exceed 32768 for this model"
```

### temperature out of range [0, 2]

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "hi"}],
    "temperature": 2.5
  }'
# expect: "temperature": "must be between 0 and 2"
```

### top_p out of range [0, 1]

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "hi"}],
    "top_p": 1.5
  }'
# expect: "top_p": "must be between 0 and 1"
```

### Too many stop sequences (limit 4)

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "hi"}],
    "stop": ["a", "b", "c", "d", "e"]
  }'
# expect: "stop": "must contain at most 4 sequences"
```

### Empty stop sequence

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "hi"}],
    "stop": [""]
  }'
# expect: "stop[0]": "must not be empty"
```

### Multiple simultaneous failures

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [],
    "max_tokens": 0,
    "temperature": -1
  }'
# expect: all three fields reported, sorted; "param" = first sorted field ("max_tokens")
```

## JSON decoding errors

Handled by `readJSON` before validation runs — also `400
invalid_request_error`, but the message is free-form (no `param`/field map).

### Empty body

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d ''
# expect: "body must not be empty"
```

### Malformed JSON

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "qwen-local", '
# expect: "body contains badly-formed JSON (at character N)"
```

### Wrong type for a field

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": "not an array"
  }'
# expect: "body contains incorrect JSON type for field \"messages\""
```

### Unknown field (strict decoding — `DisallowUnknownFields`)

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen-local",
    "messages": [{"role": "user", "content": "hi"}],
    "frequency_penalty": 0.5
  }'
# expect: "body contains unknown key \"frequency_penalty\""
```

### Two JSON values in one body

```bash
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  --data-raw '{"model": "qwen-local", "messages": [{"role": "user", "content": "hi"}]} {"another": "object"}'
# expect: "body must only contain a single JSON value"
```

### Body over 1MB

```bash
python3 -c 'import json; print(json.dumps({"model":"qwen-local","messages":[{"role":"user","content":"x"*1100000}]}))' > /tmp/big.json
curl -s http://localhost:4005/v1/chat/completions \
  -H "Content-Type: application/json" \
  --data-binary @/tmp/big.json
# expect: "body must not be larger than 1048576 bytes"
```

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
    -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' \
    http://localhost:4005/v1/chat/completions
done; echo
# expect: mostly 200 then 429 (burst 4 + refill during the loop)
```

### The 429 response headers

Immediately after exhausting the burst (run the loop above twice in a row):

```bash
curl -si -H "Authorization: Bearer $GW_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' \
  http://localhost:4005/v1/chat/completions \
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
  -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' \
  http://localhost:4005/v1/chat/completions
# expect: 200
```

### Per-token isolation — one blocked, the other fine

```bash
GW_TOKEN2=$(curl -s -X POST localhost:4005/v1/tokens/authentication \
  -H 'Content-Type: application/json' \
  -d '{"email":"jackwillis517@gmail.com","password":"<seeded password>"}' \
  | jq -r '.authentication_token.token')
# exhaust GW_TOKEN (loop above), then immediately:
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $GW_TOKEN2" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' \
  http://localhost:4005/v1/chat/completions
# expect: 200 — each token has its own bucket
```

### Anonymous requests are never limited

```bash
for i in $(seq 1 10); do
  curl -s -o /dev/null -w '%{http_code} ' http://localhost:4005/v1/healthcheck
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
      WHERE id = $GW_TOKEN_ID;"
# next request picks up the override (token row is re-read on every request):
for i in $(seq 1 8); do
  curl -s -o /dev/null -w '%{http_code} ' \
    -H "Authorization: Bearer $GW_TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen-local","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' \
    http://localhost:4005/v1/chat/completions
done; echo
# expect: all 200 (burst 20 now) and X-RateLimit-Limit-Requests: 20 in the headers
# reset afterwards:
PGPASSWORD=pa55word psql -h 127.0.0.1 -U gateway -d gateway \
  -c "UPDATE tokens SET rate_limit_rps = NULL, rate_limit_burst = NULL
      WHERE id = $GW_TOKEN_ID;"
```
