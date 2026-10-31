package httpapi

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"odysseia.local/backend/internal/forum"
	"odysseia.local/backend/internal/identity"
	"odysseia.local/backend/internal/telemetry"
)

//go:embed openapi.json
var openAPI []byte

type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
}

type API struct {
	mux          *http.ServeMux
	verify       func(context.Context, string) (identity.Authentication, error)
	resolve      func(context.Context, identity.ExternalID) (identity.User, error)
	measurements telemetry.Measurements
}

func NewAPI(
	verify func(context.Context, string) (identity.Authentication, error),
	resolve func(context.Context, identity.ExternalID) (identity.User, error),
) *API {
	mux := http.NewServeMux()
	api := &API{mux: mux, verify: verify, resolve: resolve}
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAPI)
	})
	mux.HandleFunc("GET /v1/me", api.authenticated(func(w http.ResponseWriter, _ *http.Request, actor identity.Actor) error {
		return writeJSON(w, http.StatusOK, actor.User)
	}))
	return api
}

func (api *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := telemetry.WithMeasurements(r.Context(), &api.measurements)
	api.mux.ServeHTTP(w, r.WithContext(ctx))
}

// Measurements is an in-process aggregate, not a public diagnostic route.
// Test control pipes can read it without exposing private queue activity.
func (api *API) Measurements() telemetry.Snapshot { return api.measurements.Snapshot() }

type userHandler func(http.ResponseWriter, *http.Request, identity.Actor) error

func (api *API) authenticated(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("WWW-Authenticate", `Bearer realm="odysseia"`)
		values := r.Header.Values("Authorization")
		if len(values) != 1 {
			writeProblem(w, http.StatusUnauthorized, "authentication_required")
			return
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeProblem(w, http.StatusUnauthorized, "invalid_token")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		finish := telemetry.Start(ctx, telemetry.TokenVerification)
		auth, err := api.verify(ctx, parts[1])
		finish()
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="odysseia", error="invalid_token"`)
			writeProblem(w, http.StatusUnauthorized, "invalid_token")
			return
		}
		w.Header().Del("WWW-Authenticate")
		finish = telemetry.Start(ctx, telemetry.IdentityLookup)
		user, err := api.resolve(ctx, auth.ExternalID)
		finish()
		if errors.Is(err, identity.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "account_not_provisioned")
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "current user lookup failed", "error", err)
			writeProblem(w, http.StatusServiceUnavailable, "dependency_unavailable")
			return
		}
		if err := next(w, r.WithContext(ctx), identity.Actor{User: user, ExpiresAt: auth.ExpiresAt}); err != nil {
			writeAPIError(ctx, w, err)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
	return nil
}

func writeAPIError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadRequest):
		writeProblem(w, 400, "invalid_request")
	case errors.Is(err, errBodyTooLarge):
		writeProblem(w, 413, "body_too_large")
	case errors.Is(err, errRequestTimeout):
		writeProblem(w, 408, "request_timeout")
	case errors.Is(err, forum.ErrNotFound):
		writeProblem(w, 404, "resource_not_found")
	case errors.Is(err, forum.ErrForbidden):
		writeProblem(w, 403, "permission_denied")
	case errors.Is(err, forum.ErrExists):
		writeProblem(w, 409, "member_exists")
	case errors.Is(err, forum.ErrConflict):
		writeProblem(w, 409, "idempotency_conflict")
	case errors.Is(err, forum.ErrReference):
		writeProblem(w, 422, "invalid_reference")
	case errors.Is(err, forum.ErrInvalid):
		writeProblem(w, 422, "invalid_input")
	case errors.Is(err, forum.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeProblem(w, http.StatusServiceUnavailable, "write_capacity_exceeded")
	default:
		slog.ErrorContext(ctx, "API operation failed", "error", err)
		writeProblem(w, 503, "dependency_unavailable")
	}
}

func writeProblem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type: "about:blank", Title: http.StatusText(status), Status: status, Code: code,
	})
}
