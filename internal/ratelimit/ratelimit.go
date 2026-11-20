// Package ratelimit counts requests in fixed windows in the rate_counters table, so two copies of the service share one count.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the part of a pool or a transaction that the limiter needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Limiter counts hits for a key. It takes the time from a function, so a test passes its own clock.
type Limiter struct {
	db  DB
	now func() time.Time
}

// New returns a limiter. A nil clock means time.Now.
func New(db DB, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}

	return &Limiter{db: db, now: now}
}

// Result is the answer of one hit.
type Result struct {
	// Count is the number of hits in the window, this one included.
	Count int
	// Allowed is false when Count is above the limit.
	Allowed bool
	// RetryAfter is the time that is left in the window.
	RetryAfter time.Duration
}

// RetryAfterSeconds is RetryAfter in whole seconds for the header Retry-After, and never less than 1.
func (r Result) RetryAfterSeconds() int {
	return max(int((r.RetryAfter+time.Second-1)/time.Second), 1)
}

// Hit counts one request for the key in the fixed window that holds the present moment. Every call counts, also one that is over the
// limit. The count changes in one atomic statement, so concurrent calls get different counts.
func (l *Limiter) Hit(ctx context.Context, key string, limit int, window time.Duration) (Result, error) {
	if window <= 0 || limit < 1 {
		return Result{}, errors.New("the window and the limit must be positive")
	}

	now := l.now().UTC()
	start := now.Truncate(window)

	var count int

	err := l.db.QueryRow(ctx, `
		insert into rate_counters (key, window_start, count) values ($1, $2, 1)
		on conflict (key, window_start) do update set count = rate_counters.count + 1
		returning count`, key, start).Scan(&count)
	if err != nil {
		return Result{}, fmt.Errorf("count the request %w", err)
	}

	return Result{Count: count, Allowed: count <= limit, RetryAfter: start.Add(window).Sub(now)}, nil
}

// DeleteBefore removes the counters of windows that started before the cutoff. A window that has ended counts for nothing.
func DeleteBefore(ctx context.Context, db DB, cutoff time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `delete from rate_counters where window_start < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete the old counters %w", err)
	}

	return tag.RowsAffected(), nil
}
