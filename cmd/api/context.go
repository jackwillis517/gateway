// cmd/api/context.go
package main

import (
	"context"
	"net/http"

	"gateway.jackwillis517.net/internal/data"
)

type contextKey string

const (
	userContextKey  = contextKey("user")
	tokenContextKey = contextKey("token")
)

func (app *application) contextSetUser(r *http.Request, user *data.User) *http.Request {
	ctx := context.WithValue(r.Context(), userContextKey, user)
	return r.WithContext(ctx)
}

// contextGetUser returns the user set by the authenticate middleware, which
// runs on every request and always sets one (data.AnonymousUser when no
// Authorization header is sent). A missing user is therefore a programming
// error — recoverPanic converts the panic to a 500.
func (app *application) contextGetUser(r *http.Request) *data.User {
	user, ok := r.Context().Value(userContextKey).(*data.User)
	if !ok {
		panic("missing user in request context")
	}
	return user
}

func (app *application) contextSetToken(r *http.Request, token *data.Token) *http.Request {
	ctx := context.WithValue(r.Context(), tokenContextKey, token)
	return r.WithContext(ctx)
}

// contextGetToken returns nil when the request is anonymous (for example
// /v1/healthcheck). It does not panic, because callers such as rateLimit
// legitimately see requests without a token.
func (app *application) contextGetToken(r *http.Request) *data.Token {
	token, _ := r.Context().Value(tokenContextKey).(*data.Token)
	return token
}
