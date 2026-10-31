package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RealtimeBacklog contains operational aggregates only, never queued body data.
type RealtimeBacklog struct {
	Pending          int64      `json:"pending"`
	Publications     int64      `json:"publications"`
	Disconnects      int64      `json:"disconnects"`
	OldestCreatedAt  *time.Time `json:"oldest_created_at,omitempty"`
	OldestAgeSeconds float64    `json:"oldest_age_seconds"`
}

// ReadRealtimeBacklog needs SELECT on the aggregate view, not on the outbox.
// An empty queue does not prove that a consumer or the broker is healthy.
func ReadRealtimeBacklog(ctx context.Context, pool *pgxpool.Pool) (RealtimeBacklog, error) {
	var result RealtimeBacklog
	err := pool.QueryRow(ctx, `SELECT pending,publications,disconnects,oldest_created_at,oldest_age_seconds
		FROM odysseia.realtime_backlog`).Scan(&result.Pending, &result.Publications, &result.Disconnects, &result.OldestCreatedAt, &result.OldestAgeSeconds)
	return result, err
}
