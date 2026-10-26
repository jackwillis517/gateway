package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// apiError is the OpenAI error object: {"error": {message, type, param, code}}.
type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// OpenAI error types.
const (
	errInvalidRequest = "invalid_request_error"
	errAuthentication = "authentication_error"
	errPermission     = "permission_error"
	errRateLimit      = "rate_limit_error"
	errAPI            = "api_error"
)

func ptr(s string) *string { return &s }

func (app *application) logError(r *http.Request, err error) {
	app.logger.PrintError(err, map[string]string{
		"method": r.Method,
		"uri":    r.URL.RequestURI(),
	})
}

// errorResponse is the one place the body shape is decided.
func (app *application) errorResponse(w http.ResponseWriter, r *http.Request, status int, e apiError) {
	env := envelope{"error": e}

	err := app.writeJSON(w, status, env, nil)
	if err != nil {
		app.logError(r, err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (app *application) serverErrorResponse(w http.ResponseWriter, r *http.Request, err error) {
	app.logError(r, err)
	app.errorResponse(w, r, http.StatusInternalServerError, apiError{
		Message: "the server encountered a problem and could not process your request",
		Type:    errAPI,
	})
}

func (app *application) notFoundResponse(w http.ResponseWriter, r *http.Request) {
	app.errorResponse(w, r, http.StatusNotFound, apiError{
		Message: "the requested resource could not be found",
		Type:    errInvalidRequest,
		Code:    ptr("not_found"),
	})
}

func (app *application) methodNotAllowedResponse(w http.ResponseWriter, r *http.Request) {
	app.errorResponse(w, r, http.StatusMethodNotAllowed, apiError{
		Message: fmt.Sprintf("the %s method is not supported for this resource", r.Method),
		Type:    errInvalidRequest,
		Code:    ptr("method_not_allowed"),
	})
}

func (app *application) badRequestResponse(w http.ResponseWriter, r *http.Request, err error) {
	app.errorResponse(w, r, http.StatusBadRequest, apiError{
		Message: err.Error(),
		Type:    errInvalidRequest,
	})
}

// LGF returns a map of field -> message. OpenAI has one message and one param,
// so report the first field (sorted, so it's stable) and include all in the text.
func (app *application) failedValidationResponse(w http.ResponseWriter, r *http.Request, errors map[string]string) {
	fields := make([]string, 0, len(errors))
	for f := range errors {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = fmt.Sprintf("%s: %s", f, errors[f])
	}

	app.errorResponse(w, r, http.StatusBadRequest, apiError{
		Message: strings.Join(parts, "; "),
		Type:    errInvalidRequest,
		Param:   ptr(fields[0]),
		Code:    ptr("invalid_value"),
	})
}

func (app *application) unsupportedParameterResponse(w http.ResponseWriter, r *http.Request, param string) {
	app.errorResponse(w, r, http.StatusBadRequest, apiError{
		Message: fmt.Sprintf("parameter %q is not supported by this gateway", param),
		Type:    errInvalidRequest,
		Param:   ptr(param),
		Code:    ptr("unsupported_parameter"),
	})
}

func (app *application) modelNotFoundResponse(w http.ResponseWriter, r *http.Request, model string) {
	app.errorResponse(w, r, http.StatusNotFound, apiError{
		Message: fmt.Sprintf("the model %q does not exist or is disabled", model),
		Type:    errInvalidRequest,
		Param:   ptr("model"),
		Code:    ptr("model_not_found"),
	})
}

func (app *application) invalidAuthenticationTokenResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	app.errorResponse(w, r, http.StatusUnauthorized, apiError{
		Message: "invalid or missing API key",
		Type:    errAuthentication,
		Code:    ptr("invalid_api_key"),
	})
}

func (app *application) authenticationRequiredResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	app.errorResponse(w, r, http.StatusUnauthorized, apiError{
		Message: "you must provide an API key in the Authorization header",
		Type:    errAuthentication,
		Code:    ptr("missing_api_key"),
	})
}

func (app *application) notPermittedResponse(w http.ResponseWriter, r *http.Request) {
	app.errorResponse(w, r, http.StatusForbidden, apiError{
		Message: "this API key does not have the required scope",
		Type:    errPermission,
		Code:    ptr("insufficient_scope"),
	})
}

func (app *application) rateLimitExceededResponse(w http.ResponseWriter, r *http.Request) {
	app.errorResponse(w, r, http.StatusTooManyRequests, apiError{
		Message: "rate limit exceeded, retry after the time in the Retry-After header",
		Type:    errRateLimit,
		Code:    ptr("rate_limit_exceeded"),
	})
}

func (app *application) upstreamErrorResponse(w http.ResponseWriter, r *http.Request, status int, code string, err error) {
	app.logError(r, err)
	app.errorResponse(w, r, status, apiError{ // 502, 504, or 429 for upstream_rate_limited
		Message: "the upstream provider returned an error",
		Type:    errAPI,
		Code:    ptr(code),
	})
}
