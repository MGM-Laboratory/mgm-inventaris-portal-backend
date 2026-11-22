// Package janitor deletes what has expired, on a timer. Only one of the two copies of the service sweeps at a time, because a sweep
// holds an advisory lock of PostgreSQL.
package janitor

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/errchain"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ratelimit"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

const (
	// LockKey is the number of the advisory lock that a sweep holds. It is above the 32 bit range of the lock of the migrations, so the
	// two never clash.
	LockKey int64 = 7428104001
	// counterRetention keeps a counter a long time after its window ended. The longest window is an hour, so this is a wide margin.
	counterRetention = 24 * time.Hour
	sweepTimeout     = 30 * time.Second
	rollbackTimeout  = 5 * time.Second
)

// DB is the part of the pool that a sweep needs.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Result says what a sweep did. Ran is false when the other copy held the lock.
type Result struct {
	Ran      bool
	Sessions int64
	Counters int64
	Keys     int64
}

// Sweep deletes the expired sessions, the old counters and the expired idempotency keys in one transaction. The lock is a transaction lock, so it ends with the
// transaction, also when the connection breaks. A copy that does not get the lock does nothing.
func Sweep(ctx context.Context, db DB, now time.Time) (Result, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin the sweep %w", err)
	}

	// Rollback after Commit is a no-op, and a failed or cancelled sweep rolls back here, so the lock never outlives the sweep
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()

		_ = tx.Rollback(rollbackCtx)
	}()

	var locked bool
	if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, LockKey).Scan(&locked); err != nil {
		return Result{}, fmt.Errorf("ask for the lock %w", err)
	}

	if !locked {
		return Result{}, nil
	}

	sessions, err := session.NewStore(tx, func() time.Time { return now }).DeleteExpired(ctx)
	if err != nil {
		return Result{}, err
	}

	counters, err := ratelimit.DeleteBefore(ctx, tx, now.Add(-counterRetention))
	if err != nil {
		return Result{}, err
	}

	keys, err := idempotency.DeleteExpired(ctx, tx, now)
	if err != nil {
		return Result{}, err //nolint:wrapcheck // DeleteExpired names the call
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit the sweep %w", err)
	}

	return Result{Ran: true, Sessions: sessions, Counters: counters, Keys: keys}, nil
}

// Run sweeps now and then once for each interval, until the context ends. A failed sweep is logged and the next one starts on time.
func Run(ctx context.Context, db DB, interval time.Duration, now func() time.Time) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		sweepOnce(ctx, db, now)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepOnce contains a panic, so a bug in a sweep cannot end the process that serves the API.
func sweepOnce(ctx context.Context, db DB, now func() time.Time) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("a sweep panicked", "panic_type", fmt.Sprintf("%T", r), "stack", string(debug.Stack()))
		}
	}()

	if ctx.Err() != nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()

	res, err := Sweep(ctx, db, now())
	if err != nil {
		slog.Warn("a sweep failed", "error_chain", errchain.Chain(err))

		return
	}

	if res.Sessions > 0 || res.Counters > 0 || res.Keys > 0 {
		slog.Info("a sweep deleted rows", "sessions", res.Sessions, "counters", res.Counters, "idempotency_keys", res.Keys)
	}
}
