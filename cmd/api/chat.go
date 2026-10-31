package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"

	"gateway.jackwillis517.net/internal/data"
	"gateway.jackwillis517.net/internal/provider"
	"gateway.jackwillis517.net/internal/validator"
)

const (
	maxMessages = 500
	maxStop     = 4

	// nginx's non-standard "client closed request" status, used in the usage row.
	statusClientClosedRequest = 499
)

func (app *application) createChatCompletionHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. Make Request object for database
	rec := &data.Request{
		RequestID:      "chatcmpl-" + ulid.Make().String(),
		UserID:         app.contextGetUser(r).ID,
		CreatedAt:      start,
		GatewayVersion: app.config.version,
		StatusCode:     http.StatusOK, // overwritten on every failure path
	}
	if key := app.contextGetAPIKey(r); key != nil {
		rec.APIKeyID = &key.ID
	}
	defer func() {
		rec.LatencyMs = int(time.Since(start).Milliseconds())
		app.recordUsage(rec)
	}()

	w.Header().Set("X-Request-Id", rec.RequestID)

	// 2. Decode the clients request body
	var input provider.ChatCompletionRequest
	err := app.readJSON(w, r, &input)
	if err != nil {
		failRecord(rec, http.StatusBadRequest, errInvalidRequest)
		app.badRequestResponse(w, r, err)
		return
	}

	req := &provider.ChatCompletionRequest{
		Model:         input.Model,
		Messages:      input.Messages,
		MaxTokens:     input.MaxTokens,
		Temperature:   input.Temperature,
		TopP:          input.TopP,
		Stop:          input.Stop,
		Stream:        input.Stream,
		StreamOptions: input.StreamOptions,
		RequestID:     input.RequestID,
	}

	if req.Model == "" {
		failRecord(rec, http.StatusBadRequest, errInvalidRequest)
		app.failedValidationResponse(w, r, map[string]string{"model": "must be provided"})
		return
	}

	// 3. Resolve the models name by alias
	m, err := app.models.LLMModels.GetByNameOrAlias(req.Model)
	if err != nil {
		switch {
		case errors.Is(err, data.ErrRecordNotFound):
			failRecord(rec, http.StatusNotFound, "model_not_found")
			app.modelNotFoundResponse(w, r, req.Model)
		default:
			failRecord(rec, http.StatusInternalServerError, "internal_error")
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	rec.Model, rec.Provider, rec.UpstreamModel = &m.Name, &m.Provider, &m.UpstreamModel
	rec.Streamed = req.Stream

	// 4. Field-level validation.
	v := validator.New()
	if validateChatCompletionRequet(v, req, m); !v.Valid() {
		failRecord(rec, http.StatusBadRequest, "invalid_request_error")
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	// 5. Pick the provider and prepare upstream request
	prov, ok := app.providers[m.Provider]
	if !ok {
		failRecord(rec, http.StatusInternalServerError, "internal_error")
		app.serverErrorResponse(w, r, fmt.Errorf("no provider registered for %q", m.Provider))
		return
	}

	wantUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage

	req.Model = m.UpstreamModel
	req.RequestID = rec.RequestID
	if req.Stream {
		// Always ask upstream for usage so the row is complete; the client only
		// sees the usage chunk if it asked for it (wantUsage).
		req.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
	} else {
		req.StreamOptions = nil
	}

	// A client disconnect cancels r.Context(), which cancels the upstream call.
	// upstreamTimeout is seconds, so it must be scaled before use.
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(app.config.upstreamTimeout)*time.Second)
	defer cancel()

	if req.Stream {
		app.streamChat(ctx, w, r, prov, req, m, rec, wantUsage)
		return
	}
	app.completeChat(ctx, w, r, prov, req, m, rec)
}

// ---------------------------------------------------------------------------
// Non-streaming
// ---------------------------------------------------------------------------

func (app *application) completeChat(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	prov provider.Provider,
	req *provider.ChatCompletionRequest,
	m *data.LLMModel,
	rec *data.Request,
) {
	start := time.Now()
	resp, err := prov.Complete(ctx, req)
	setUpstreamLatency(rec, start)

	if err != nil {
		status, typ, code, msg := classifyUpstreamError(r, err)
		failRecord(rec, status, code)

		// The client is gone; there is nobody left to answer.
		if status == statusClientClosedRequest {
			return
		}
		app.logError(r, err)
		app.errorResponse(w, r, status, apiError{
			Message: msg,
			Type:    typ,
			Code:    ptr(code),
		})
		return
	}

	// The client sees the gateway's request id and model name, never the
	// provider's.
	resp.ID = rec.RequestID
	resp.Model = m.Name

	if len(resp.Choices) > 0 {
		rec.FinishReason = &resp.Choices[0].FinishReason
	}
	recordTokens(rec, resp.Usage)

	if err := app.writeJSON(w, http.StatusOK, resp, nil); err != nil {
		app.logError(r, err)
	}
}

// ---------------------------------------------------------------------------
// Streaming (SSE)
// ---------------------------------------------------------------------------

func (app *application) streamChat(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	prov provider.Provider,
	req *provider.ChatCompletionRequest,
	m *data.LLMModel,
	rec *data.Request,
	wantUsage bool,
) {
	// LGF's server-wide WriteTimeout would cut long streams short; extend
	// this response's write deadline to the upstream timeout instead.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(time.Duration(app.config.upstreamTimeout) * time.Second)); err != nil {
		app.logger.PrintError(err, map[string]string{"message": "could not extend write deadline"})
	}

	// SSE headers are written lazily, with the first chunk: an upstream
	// failure before that can still return a normal JSON error envelope.
	started := false
	begin := func() {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}

	start := time.Now()
	var ttft *int

	emit := func(chunk *provider.ChatCompletionChunk) error {
		// The client sees the gateway's id and model name, never the
		// provider's.
		chunk.ID = rec.RequestID
		chunk.Model = m.Name

		// Usage is always recorded in the row; the usage chunk itself is
		// forwarded only when the client asked for it (wantUsage).
		if chunk.Usage != nil {
			recordTokens(rec, chunk.Usage)
			if !wantUsage {
				return nil
			}
		}

		for _, c := range chunk.Choices {
			if c.Delta.Role != "" || c.Delta.Content != "" {
				if ttft == nil {
					ms := int(time.Since(start).Milliseconds())
					ttft = &ms
				}
			}
			if c.FinishReason != nil {
				rec.FinishReason = c.FinishReason
			}
		}

		if !started {
			begin()
			started = true
		}
		return app.writeSSE(w, chunk)
	}

	err := prov.Stream(ctx, req, emit)
	setUpstreamLatency(rec, start)
	if ttft != nil {
		rec.TTFTMs = ttft
	}

	if err != nil {
		status, typ, code, msg := classifyUpstreamError(r, err)
		failRecord(rec, status, code)

		// The client is gone; the usage row is still written by the
		// handler's deferred recordUsage.
		if status == statusClientClosedRequest {
			return
		}
		app.logError(r, err)

		if !started {
			// Nothing committed yet: a normal JSON error envelope.
			app.errorResponse(w, r, status, apiError{
				Message: msg,
				Type:    typ,
				Code:    ptr(code),
			})
			return
		}

		// The stream is committed: report the failure in-stream and still
		// end the stream, so SSE clients do not hang waiting for [DONE].
		_ = app.writeSSE(w, map[string]any{
			"error": apiError{Message: msg, Type: typ, Code: ptr(code)},
		})
	}

	// An empty stream is still a valid stream: commit the headers and end.
	if !started {
		begin()
	}
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeSSE marshals one payload into a single SSE "data:" frame and flushes
// it, so each chunk reaches the client as it happens.
func (app *application) writeSSE(w http.ResponseWriter, payload any) error {
	js, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", js); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Usage recording helpers
// ---------------------------------------------------------------------------

// recordUsage writes the usage into the request table in the background. It deliberately does not
// use r.Context(): that is cancelled when a streaming client disconnects.
func (app *application) recordUsage(rec *data.Request) {
	app.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		if err := app.models.Request.Insert(ctx, rec); err != nil {
			app.logger.PrintError(err, map[string]string{"message": "could not record usage", "request_id": rec.RequestID})
		}
		app.logger.PrintInfo("request completed", map[string]string{
			"request_id":          rec.RequestID,
			"status":              strconv.Itoa(rec.StatusCode),
			"model":               strOrNull(rec.Model),
			"provider":            strOrNull(rec.Provider),
			"error_code":           strOrNull(rec.ErrorCode),
			"streamed":            strconv.FormatBool(rec.Streamed),
			"finish_reason":       strOrNull(rec.FinishReason),
			"input_tokens":        strconv.Itoa(rec.InputTokens),
			"output_tokens":       strconv.Itoa(rec.OutputTokens),
			"latency_ms":          strconv.Itoa(rec.LatencyMs),
			"upstream_latency_ms": intOrNull(rec.UpstreamLatencyMs),
			"ttft_ms":             intOrNull(rec.TTFTMs),
			"cost_usd":            strconv.FormatFloat(rec.CostUSD, 'f', -1, 64),
		})
	})
}

// recordTokens copies usage counts into the usage row. The cache
// read/write counts stay 0 until a provider reports them separately
// (response-cache day).
func recordTokens(rec *data.Request, u *provider.Usage) {
	if u == nil {
		return
	}
	rec.InputTokens = u.PromptTokens
	rec.OutputTokens = u.CompletionTokens
}

func failRecord(rec *data.Request, status int, code string) {
	rec.StatusCode = status
	rec.ErrorCode = &code
}

// strOrNull and intOrNull render nullable row fields for the log line: a
// NULL prints as an empty string rather than "<nil>".
func strOrNull(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func intOrNull(i *int) string {
	if i == nil {
		return ""
	}
	return strconv.Itoa(*i)
}

func setUpstreamLatency(rec *data.Request, since time.Time) {
	ms := int(time.Since(since).Milliseconds())
	rec.UpstreamLatencyMs = &ms
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func validateChatCompletionRequet(v *validator.Validator, req *provider.ChatCompletionRequest, m *data.LLMModel) {
	v.Check(len(req.Messages) >= 1, "messages", "must contain at least 1 message")
	v.Check(len(req.Messages) <= maxMessages, "messages",
		fmt.Sprintf("must not contain more than %d messages", maxMessages))

	for i, msg := range req.Messages {
		v.Check(
			validator.PermittedValue(msg.Role, "system", "user", "assistant"),
			fmt.Sprintf("messages[%d].role", i),
			"must be one of: system, user, assistant",
		)
		v.Check(
			msg.Content != "",
			fmt.Sprintf("messages[%d].content", i),
			"must be a non-empty string",
		)
	}

	// max_tokens: 1 up to the model's ceiling
	if req.MaxTokens != nil {
		v.Check(*req.MaxTokens >= 1, "max_tokens", "must be at least 1")
		v.Check(*req.MaxTokens <= m.MaxOutputTokens, "max_tokens",
			fmt.Sprintf("must not exceed %d for this model", m.MaxOutputTokens))
	}

	// sampling
	if req.Temperature != nil {
		v.Check(*req.Temperature >= 0 && *req.Temperature <= 2, "temperature",
			"must be between 0 and 2")
	}
	if req.TopP != nil {
		v.Check(*req.TopP >= 0 && *req.TopP <= 1, "top_p", "must be between 0 and 1")
	}

	// stop
	v.Check(len(req.Stop) <= maxStop, "stop",
		fmt.Sprintf("must contain at most %d sequences", maxStop))
	for i, s := range req.Stop {
		v.Check(s != "", fmt.Sprintf("stop[%d]", i), "must not be empty")
	}
}
