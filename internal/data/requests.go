package data

import (
	"context"
	"database/sql"
	"time"
)

type Request struct {
	ID                int64
	RequestID         string // "chatcmpl-<ulid>", also the X-Request-Id header
	CreatedAt         time.Time
	UserID            int64
	APIKeyID          *int64
	Model             *string // as requested; nil when rejected before parsing
	Provider          *string
	UpstreamModel     *string
	StatusCode        int
	ErrorCode         *string
	Streamed          bool
	FinishReason      *string
	InputTokens       int
	OutputTokens      int
	CacheReadTokens   int
	CacheWriteTokens  int
	CostUSD           float64 // set by Insert from the models price columns
	LatencyMs         int
	UpstreamLatencyMs *int
	TTFTMs            *int
	RetryCount        int
	FallbackUsed      bool
	CacheHit          bool
	GatewayVersion    string
	PromptVersion     *string
}

type RequestModel struct {
	DB *sql.DB
}

// Insert writes a usage row and computes cost_usd in the same statement from
// the matching models row (numeric math in Postgres, no float rounding in Go).
// A cache hit, or a model with no row, costs 0.
func (m RequestModel) Insert(ctx context.Context, r *Request) error {
	query := `
		INSERT INTO requests (
			request_id, user_id, api_key_id, model, provider, upstream_model,
			status_code, error_code, streamed, finish_reason,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			latency_ms, upstream_latency_ms, ttft_ms,
			retry_count, fallback_used, cache_hit,
			gateway_version, prompt_version,
			cost_usd)
		VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14,
			$15, $16, $17,
			$18, $19, $20,
			$21, $22,
			CASE WHEN $20::boolean THEN 0 ELSE COALESCE((
				SELECT ($11::integer * input_price_per_mtok
				      + $12::integer * output_price_per_mtok
				      + $13::integer * cache_read_price_per_mtok
				      + $14::integer * cache_write_price_per_mtok) / 1000000
				FROM models
				WHERE name = $4::text
			), 0) END)
		RETURNING id, created_at, cost_usd::float8`

	args := []any{
		r.RequestID,
		r.UserID,
		r.APIKeyID,
		r.Model,
		r.Provider,
		r.UpstreamModel,
		r.StatusCode,
		r.ErrorCode,
		r.Streamed,
		r.FinishReason,
		r.InputTokens,
		r.OutputTokens,
		r.CacheReadTokens,
		r.CacheWriteTokens,
		r.LatencyMs,
		r.UpstreamLatencyMs,
		r.TTFTMs,
		r.RetryCount,
		r.FallbackUsed,
		r.CacheHit,
		r.GatewayVersion,
		r.PromptVersion,
	}

	return m.DB.QueryRowContext(ctx, query, args...).Scan(&r.ID, &r.CreatedAt, &r.CostUSD)
}
