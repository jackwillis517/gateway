package data

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Providers the gateway can route to; must match the CHECK constraint on
// models.provider.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderOllama    = "ollama"
)

// LLMModel mirrors a row in the models table: the catalog of models the
// gateway accepts, where each one routes, and what it costs.
// (Named LLMModel so it is not confused with the Models wrapper struct.)
type LLMModel struct {
	ID                     int64     `json:"-"`
	Name                   string    `json:"name"`  // "ollama/qwen3.5:2b-32k"
	Alias                  *string   `json:"alias"` // "qwen-local"
	Provider               string    `json:"provider"`
	UpstreamModel          string    `json:"upstream_model"` // id sent to the provider
	InputPricePerMTok      float64   `json:"input_price_per_mtok"`
	OutputPricePerMTok     float64   `json:"output_price_per_mtok"`
	CacheReadPricePerMTok  float64   `json:"cache_read_price_per_mtok"`
	CacheWritePricePerMTok float64   `json:"cache_write_price_per_mtok"`
	MaxOutputTokens        int       `json:"max_output_tokens"`
	Enabled                bool      `json:"enabled"`
	CreatedAt              time.Time `json:"created_at"`
	Version                int       `json:"-"`
}

type LLMModelModel struct {
	DB *sql.DB
}

const llmModelColumns = `
	id, name, alias, provider, upstream_model,
	input_price_per_mtok::float8, output_price_per_mtok::float8,
	cache_read_price_per_mtok::float8, cache_write_price_per_mtok::float8,
	max_output_tokens, enabled, created_at, version`

func scanLLMModel(row interface{ Scan(...any) error }, m *LLMModel) error {
	return row.Scan(
		&m.ID,
		&m.Name,
		&m.Alias,
		&m.Provider,
		&m.UpstreamModel,
		&m.InputPricePerMTok,
		&m.OutputPricePerMTok,
		&m.CacheReadPricePerMTok,
		&m.CacheWritePricePerMTok,
		&m.MaxOutputTokens,
		&m.Enabled,
		&m.CreatedAt,
		&m.Version,
	)
}

// GetByNameOrAlias looks up an enabled model by its full name
// ("ollama/qwen3.5:2b-32k") or its alias ("qwen-local"). Returns
// ErrRecordNotFound for unknown or disabled models, which the handler turns
// into 404 model_not_found.
func (m LLMModelModel) GetByNameOrAlias(nameOrAlias string) (*LLMModel, error) {
	if nameOrAlias == "" {
		return nil, ErrRecordNotFound
	}

	query := `SELECT ` + llmModelColumns + `
		FROM models
		WHERE (name = $1 OR alias = $1) AND enabled`

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var model LLMModel
	err := scanLLMModel(m.DB.QueryRowContext(ctx, query, nameOrAlias), &model)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, ErrRecordNotFound
		default:
			return nil, err
		}
	}

	return &model, nil
}

// GetAllEnabled lists enabled models by name, for GET /v1/models.
func (m LLMModelModel) GetAllEnabled() ([]*LLMModel, error) {
	query := `SELECT ` + llmModelColumns + `
		FROM models
		WHERE enabled
		ORDER BY name`

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := m.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	models := []*LLMModel{}
	for rows.Next() {
		var model LLMModel
		if err := scanLLMModel(rows, &model); err != nil {
			return nil, err
		}
		models = append(models, &model)
	}

	return models, rows.Err()
}
