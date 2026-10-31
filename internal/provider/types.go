package provider

import (
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// Request
// ---------------------------------------------------------------------------

// ChatCompletionRequest is the OpenAI-shaped request body. The handler decodes
// and validates it, then hands it to a Provider. Optional numeric fields are
// pointers so "unset" can be told apart from an explicit zero.
type ChatCompletionRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          StopSequences  `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	// RequestID ("chatcmpl-<ulid>") is set by the handler. It is never read
	// from or sent to the wire; providers use it to stamp responses and chunks.
	RequestID string `json:"-"`
}

// Message is a single chat message. Role is "system", "user" or "assistant".
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// StreamOptions mirrors OpenAI's stream_options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// StopSequences accepts either a single string or an array of strings, as
// OpenAI's "stop" parameter does. After decoding it is always a slice.
type StopSequences []string

func (s *StopSequences) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = StopSequences{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("stop must be a string or an array of strings")
	}
	*s = StopSequences(many)
	return nil
}

// ---------------------------------------------------------------------------
// Response (non-streaming)
// ---------------------------------------------------------------------------

// ChatCompletionResponse is the OpenAI-shaped non-streaming response.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice is one completion. This gateway always returns exactly one, Index 0.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"` // "stop" or "length"
}

// ---------------------------------------------------------------------------
// Streaming chunks
// ---------------------------------------------------------------------------

// ChatCompletionChunk is one SSE "data:" payload in a streaming response.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"` // "chat.completion.chunk"
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`

	// Usage is only set on the final usage chunk (empty Choices), and only
	// when stream_options.include_usage was requested.
	Usage *Usage `json:"usage,omitempty"`
}

// ChunkChoice is the streaming counterpart of Choice.
type ChunkChoice struct {
	Index int        `json:"index"`
	Delta ChunkDelta `json:"delta"`

	// FinishReason is a pointer so it serializes as null on every chunk
	// except the last content chunk.
	FinishReason *string `json:"finish_reason"`
}

// ChunkDelta carries the incremental piece of the message.
type ChunkDelta struct {
	Role    string `json:"role,omitempty"`    // "assistant" on the first chunk only
	Content string `json:"content,omitempty"` // text fragment
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// Usage is OpenAI-shaped token accounting. Ollama's OpenAI-compatible endpoint
// already reports it in this shape, and has no cache read/write counts, so
// there is no prompt_tokens_details here.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
