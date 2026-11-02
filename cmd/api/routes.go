package main

import (
	"net/http"

	"github.com/julienschmidt/httprouter"
)

func (app *application) routes() http.Handler {
	router := httprouter.New()

	router.HandlerFunc(http.MethodGet, "/v1/healthcheck", app.healthcheckHandler)
	router.HandlerFunc(http.MethodPost, "/v1/tokens/authentication", app.createAuthenticationTokenHandler)

	// httprouter takes a http.Handler (not HandlerFunc) for wrapped routes.
	router.Handler(http.MethodPost, "/v1/chat/completions",
		app.requireAuthenticatedUser(http.HandlerFunc(app.createChatCompletionHandler)))
	router.Handler(http.MethodGet, "/v1/models",
		app.requireAuthenticatedUser(http.HandlerFunc(app.listModelsHandler)))

	return app.recoverPanic(app.authenticate(app.rateLimit(router)))
}
