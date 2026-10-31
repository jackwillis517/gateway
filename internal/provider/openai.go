package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAICompatible talks to any server that implements OpenAI's
// /chat/completions endpoint. OpenAI and Ollama's /v1 endpoint both do, so
// they share this one implementation and differ only in base URL and API key.
type OpenAICompatible struct {
	name    string
	baseURL string // includes the /v1 prefix, no trailing slash
	apiKey  string // empty means no Authorization header (Ollama)
	client  *http.Client
}

var _ Provider = (*OpenAICompatible)(nil)

func newOpenAICompatible(name, baseURL, apiKey string) *OpenAICompatible {
	return &OpenAICompatible{
		name:    name,
		baseURL: baseURL,
		apiKey:  apiKey,
		// No Client.Timeout: it would cut off long streams. Deadlines come
		// from the context the handler passes in.
		client: &http.Client{},
	}
}

// NewOpenAI returns a provider for api.openai.com.
func NewOpenAI(apiKey string) *OpenAICompatible {
	return newOpenAICompatible("openai", "https://api.openai.com/v1", apiKey)
}

// Complete implements Provider.
func (p *OpenAICompatible) Complete(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	r := *req // shallow copy so we do not change the caller's request
	r.Stream = false
	r.StreamOptions = nil

	resp, err := p.do(ctx, &r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: decoding response: %w", p.name, err)
	}
	return &out, nil
}

// Stream implements Provider. It decodes each upstream SSE "data:" line into
// a ChatCompletionChunk and hands it to emit. The handler writes the final
// "data: [DONE]" itself, so Stream just returns nil when it sees one.
func (p *OpenAICompatible) Stream(ctx context.Context, req *ChatCompletionRequest, emit func(*ChatCompletionChunk) error) error {
	r := *req
	r.Stream = true

	resp, err := p.do(ctx, &r)
	if err != nil {
		return err // any non-200 is returned here, before the first emit
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024) // allow long lines

	for sc.Scan() {
		payload, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // blank separator lines, SSE comments
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			return nil
		}

		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("%s: decoding chunk: %w", p.name, err)
		}
		if err := emit(&chunk); err != nil {
			return err // client gone: stop reading, which closes the upstream body
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// The body ended without [DONE]: the upstream stream was cut short.
	return io.ErrUnexpectedEOF
}

// do sends the request and returns the response only if the status is 200.
// Non-200 responses become an *UpstreamError.
func (p *OpenAICompatible) do(ctx context.Context, req *ChatCompletionRequest) (*http.Response, error) {
	body, err := json.Marshal(req) // RequestID is json:"-", so it is not sent
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		// Context errors come back wrapped, so errors.Is(err,
		// context.DeadlineExceeded) still works in the handler.
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &UpstreamError{Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	return resp, nil
}
