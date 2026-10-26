package main

import (
	"net/http"

	"github.com/julienschmidt/httprouter"
)

func (app *application) routes() *httprouter.Router {
	router := httprouter.New()

	router.HandlerFunc(http.MethodGet, "/v1/healthcheck", app.healthcheckHandler)
	router.HandlerFunc(http.MethodPost, "/v1/chat/completions", app.createChatCompletionHandler)
	// router.HandlerFunc(http.MethodGet, "/v1/models", app.)
	// router.HandlerFunc(http.MethodGet, "/v1/usage", app.)
	// router.HandlerFunc(http.MethodPost, "/v1/admin/api-keys", app.)
	// router.HandlerFunc(http.MethodDelete, "/v1/admin/api-keys", app.)

	return router
}
