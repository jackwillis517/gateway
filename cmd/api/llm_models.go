package main

import (
	"net/http"
)

// listModelsHandler serves GET /v1/models: the enabled rows of the models
// table in OpenAI's list shape. "id" is the gateway model name
// ("ollama/qwen3.5:4b") — the name clients send back as "model" — and
// "owned_by" is the provider.
func (app *application) listModelsHandler(w http.ResponseWriter, r *http.Request) {
	models, err := app.models.LLMModels.GetAllEnabled()
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	type modelObject struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	out := make([]modelObject, len(models))
	for i, m := range models {
		out[i] = modelObject{
			ID:      m.Name,
			Object:  "model",
			Created: m.CreatedAt.Unix(),
			OwnedBy: m.Provider,
		}
	}

	err = app.writeJSON(w, http.StatusOK, envelope{"object": "list", "data": out}, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}
