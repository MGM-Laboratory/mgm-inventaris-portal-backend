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
	// WindowStart is the start of the window that counted the hit. Refund needs it, so a refund never reaches into a later window.
	WindowStart time.Time
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

	return Result{Count: count, Allowed: count <= limit, RetryAfter: start.Add(window).Sub(now), WindowStart: start}, nil
}

// Refund takes back one hit in the window that started at start. A limit that counts only failures hits first, so that parallel
// requests cannot all pass before the first failure is counted, and refunds the hit of a request that did not fail. The count never
// goes below 0, and a window that is gone is not an error.
func (l *Limiter) Refund(ctx context.Context, key string, start time.Time) error {
	if _, err := l.db.Exec(ctx, `update rate_counters set count = greatest(count - 1, 0) where key = $1 and window_start = $2`, key, start); err != nil {
		return fmt.Errorf("refund the request %w", err)
	}

	return nil
}

// DeleteBefore removes the counters of windows that started before the cutoff. A window that has ended counts for nothing.
func DeleteBefore(ctx context.Context, db DB, cutoff time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `delete from rate_counters where window_start < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete the old counters %w", err)
	}

	return tag.RowsAffected(), nil
}
