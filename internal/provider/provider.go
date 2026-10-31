// Package provider provides an interface and implementations for Complete and Stream
// functions per AI provider
package provider

import (
	"context"
	"fmt"
)

type Provider interface {
	Complete(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error)
	Stream(ctx context.Context, req *ChatCompletionRequest, emit func(*ChatCompletionChunk) error) error
}

type UpstreamError struct {
	Status  int
	Message string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.Status, e.Message)
}
