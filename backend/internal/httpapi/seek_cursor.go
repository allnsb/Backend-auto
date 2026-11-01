package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Composite cursors are bound to a collection and caller. Authorization is
// still enforced by the query; base64 is an opaque encoding, not a signature.
type activityCursor struct {
	Version int       `json:"v"`
	Kind    string    `json:"kind"`
	Scope   string    `json:"scope"`
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
}

func readActivityPage(r *http.Request, kind, scope string) (time.Time, string, int, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			return time.Time{}, "", 0, errBadRequest
		}
		limit = parsed
	}
	raw := r.URL.Query().Get("after")
	if raw == "" {
		return time.Time{}, "", limit, nil
	}
	if len(raw) > 512 {
		return time.Time{}, "", 0, errBadRequest
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", 0, errBadRequest
	}
	var cursor activityCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return time.Time{}, "", 0, errBadRequest
	}
	if cursor.Version != 1 || cursor.Kind != kind || cursor.Scope != scope || cursor.At.IsZero() {
		return time.Time{}, "", 0, errBadRequest
	}
	id, err := canonicalID(cursor.ID)
	if err != nil {
		return time.Time{}, "", 0, errBadRequest
	}
	return cursor.At, id, limit, nil
}
