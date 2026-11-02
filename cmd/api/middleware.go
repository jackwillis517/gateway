package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/time/rate"

	"gateway.jackwillis517.net/internal/data"
	"gateway.jackwillis517.net/internal/validator"
)

func (app *application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				w.Header().Set("Connection", "close")
				app.serverErrorResponse(w, r, fmt.Errorf("%s", err))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves the Authorization: Bearer header into a user and
// token in the request context (LGF 9.2, adapted: GetForPlaintext returns
// both the user and the token).
func (app *application) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Authorization")

		// No header: anonymous. /v1/healthcheck stays open.
		authorizationHeader := r.Header.Get("Authorization")
		if authorizationHeader == "" {
			r = app.contextSetUser(r, data.AnonymousUser)
			next.ServeHTTP(w, r)
			return
		}

		headerParts := strings.Split(authorizationHeader, " ")
		if len(headerParts) != 2 || headerParts[0] != "Bearer" {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		// The token must be the right shape before we hit the database.
		token := headerParts[1]
		v := validator.New()
		if data.ValidateTokenPlaintext(v, token); !v.Valid() {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		user, t, err := app.models.Tokens.GetForPlaintext(token)
		if err != nil {
			switch {
			case errors.Is(err, data.ErrRecordNotFound):
				app.invalidAuthenticationTokenResponse(w, r)
			default:
				app.serverErrorResponse(w, r, err)
			}
			return
		}

		r = app.contextSetUser(r, user)
		r = app.contextSetToken(r, t)

		// Update last_used_at at most once a minute, in the background, so a
		// busy token does not write on every request.
		if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > time.Minute {
			app.background(func() { _ = app.models.Tokens.Touch(t.ID) })
		}

		next.ServeHTTP(w, r)
	})
}

// requireAuthenticatedUser guards routes that an anonymous request must not
// reach. Without it an anonymous call would run the chat handler and write a
// usage row attributed to AnonymousUser.ID — which is the same row as the
// seeded user.
func (app *application) requireAuthenticatedUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.contextGetUser(r).IsAnonymous() {
			app.authenticationRequiredResponse(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (app *application) rateLimit(next http.Handler) http.Handler {
	type client struct {
		limiter  *rate.Limiter
		lastSeen time.Time
	}

	// A mutex and a map of limiters keyed by tokens.id — not IP, so every
	// token gets its own bucket regardless of network topology.
	var (
		mu      sync.Mutex
		clients = make(map[int64]*client)
	)

	// Launch a background goroutine which removes old entries from the clients
	// map once every minute: limiters idle for 3 minutes are dropped.
	go func() {
		for {
			time.Sleep(time.Minute)
			// Lock the mutex to prevent any rate limiter checks from happening
			// while the cleanup is taking place.
			mu.Lock()
			for id, client := range clients {
				if time.Since(client.lastSeen) > 3*time.Minute {
					delete(clients, id)
				}
			}
			// Importantly, unlock the mutex when the cleanup is complete.
			mu.Unlock()
		}
	}()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !app.config.limiter.enabled {
			next.ServeHTTP(w, r)
			return
		}

		// Anonymous (no Authorization header): healthcheck and token mint
		// pass through unlimited.
		token := app.contextGetToken(r)
		if token == nil {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rps, burst := app.limitsFor(token)

		// Lock the mutex to prevent this code from being executed concurrently.
		mu.Lock()

		// Add the token to the map if it is not there already.
		c, ok := clients[token.ID]
		if !ok {
			c = &client{limiter: rate.NewLimiter(rate.Limit(rps), burst)}
			clients[token.ID] = c
		}
		c.lastSeen = time.Now()

		// A DB override changed since this limiter was created: apply it
		// without a restart.
		if c.limiter.Limit() != rate.Limit(rps) {
			c.limiter.SetLimit(rate.Limit(rps))
		}
		if c.limiter.Burst() != burst {
			c.limiter.SetBurst(burst)
		}

		res := c.limiter.Reserve()
		remaining := int(c.limiter.Tokens())

		// Very importantly, unlock the mutex before calling the next handler
		// (or writing the response).
		mu.Unlock()

		// Set headers before any write: writeJSON copies the header map at
		// write time, and success responses carry the pair too.
		w.Header().Set("X-RateLimit-Limit-Requests", strconv.Itoa(burst))
		w.Header().Set("X-RateLimit-Remaining-Requests", strconv.Itoa(max(remaining, 0)))

		if d := res.Delay(); d > 0 {
			// Rejected, not waiting: give the token back so a 429 storm does
			// not deepen the bucket debt.
			res.Cancel()
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
			app.recordRejected(r, http.StatusTooManyRequests, start)
			app.rateLimitExceededResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// limitsFor resolves the effective rate limit for a token: the token row's
// override when set, else the -limiter-rps / -limiter-burst flag defaults.
func (app *application) limitsFor(token *data.Token) (rps float64, burst int) {
	rps, burst = app.config.limiter.rps, app.config.limiter.burst
	if token.RateLimitRPS != nil {
		rps = *token.RateLimitRPS
	}
	if token.RateLimitBurst != nil {
		burst = *token.RateLimitBurst
	}
	return rps, burst
}

// recordRejected writes a requests row for a request the middleware rejected
// before any handler ran (429). Zero tokens, zero cost, no model. The row is
// attributed to the authenticated user and token from the context. It reuses
// the chat handler's recordUsage, so the row and log line are identical.
func (app *application) recordRejected(r *http.Request, status int, start time.Time) {
	code := "rate_limit_exceeded"
	rec := &data.Request{
		RequestID:      "chatcmpl-" + ulid.Make().String(),
		UserID:         app.contextGetUser(r).ID,
		StatusCode:     status,
		ErrorCode:      &code,
		GatewayVersion: app.config.version,
		LatencyMs:      int(time.Since(start).Milliseconds()),
	}
	if t := app.contextGetToken(r); t != nil {
		rec.TokenID = &t.ID
	}
	app.recordUsage(rec)
}
