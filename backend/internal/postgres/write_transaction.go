package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"odysseia.local/backend/internal/telemetry"
)

// Keep pgx BeginFunc's commit-on-success semantics while measuring pool checkout
// separately from BEGIN and COMMIT. The caller already owns its write permit.
func writeTransaction(ctx context.Context, pool *pgxpool.Pool, operation func(pgx.Tx) error) error {
	finish := telemetry.Start(ctx, telemetry.PoolAcquire)
	connection, err := pool.Acquire(ctx)
	finish()
	if err != nil {
		return err
	}
	defer connection.Release()
	finish = telemetry.Start(ctx, telemetry.TransactionBegin)
	tx, err := connection.Begin(ctx)
	finish()
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := operation(tx); err != nil {
		return err
	}
	finish = telemetry.Start(ctx, telemetry.TransactionCommit)
	err = tx.Commit(ctx)
	finish()
	return err
}
