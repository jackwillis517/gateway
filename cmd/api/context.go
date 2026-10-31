// cmd/api/context.go
package main

import (
	"context"
	"net/http"

	"gateway.jackwillis517.net/internal/data"
)

type contextKey string

const (
	userContextKey   = contextKey("user")
	apiKeyContextKey = contextKey("api_key")
)

func (app *application) contextSetUser(r *http.Request, user *data.User) *http.Request {
	ctx := context.WithValue(r.Context(), userContextKey, user)
	return r.WithContext(ctx)
}

// contextGetUser returns the user set by the authenticate middleware. That
// middleware is still stubbed, so for now requests carry no user: fall
// back to the anonymous user instead of panicking, and the usage row
// records its (zero) id. The real middleware will always set a user
// (data.AnonymousUser when no key is sent), making this the LGF behavior
// again.
func (app *application) contextGetUser(r *http.Request) *data.User {
	user, ok := r.Context().Value(userContextKey).(*data.User)
	if !ok {
		return data.AnonymousUser
	}
	return user
}

func (app *application) contextSetAPIKey(r *http.Request, key *data.APIKey) *http.Request {
	ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
	return r.WithContext(ctx)
}

// contextGetAPIKey returns nil when the request is anonymous (for example
// /v1/healthcheck). It does not panic, because callers such as rateLimit
// legitimately see requests without a key.
func (app *application) contextGetAPIKey(r *http.Request) *data.APIKey {
	key, _ := r.Context().Value(apiKeyContextKey).(*data.APIKey)
	return key
}
