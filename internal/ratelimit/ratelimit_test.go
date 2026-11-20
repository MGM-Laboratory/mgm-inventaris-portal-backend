package ratelimit_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ratelimit"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = t
}

func TestRetryAfterSeconds(t *testing.T) {
	tests := []struct {
		left time.Duration
		want int
	}{
		{40 * time.Minute, 2400},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{time.Millisecond, 1},
		{0, 1},
		{-time.Second, 1},
	}

	for _, tc := range tests {
		if got := (ratelimit.Result{RetryAfter: tc.left}).RetryAfterSeconds(); got != tc.want {
			t.Fatalf("%v left gave %d, want %d", tc.left, got, tc.want)
		}
	}
}

func TestHitRejectsABadWindowOrLimit(t *testing.T) {
	limiter := ratelimit.New(nil, nil)

	for _, tc := range []struct {
		limit  int
		window time.Duration
	}{{0, time.Hour}, {-1, time.Hour}, {5, 0}, {5, -time.Minute}} {
		if _, err := limiter.Hit(context.Background(), "key", tc.limit, tc.window); err == nil {
			t.Fatalf("limit %d and window %v were accepted", tc.limit, tc.window)
		}
	}
}

func TestHitCountsInFixedWindows(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	clk := &clock{now: time.Date(2026, 11, 17, 10, 20, 0, 0, time.UTC)}
	limiter := ratelimit.New(pool, clk.Now)

	// the limit is 3, so the fourth request in the window is over, and every request counts
	for i := 1; i <= 5; i++ {
		res, err := limiter.Hit(ctx, "guest-signin:198.51.100.7", 3, time.Hour)
		if err != nil {
			t.Fatalf("hit %d failed %v", i, err)
		}

		if res.Count != i || res.Allowed != (i <= 3) {
			t.Fatalf("hit %d gave %+v", i, res)
		}

		// the window is 10:00 to 11:00 and the clock is at 10:20
		if res.RetryAfter != 40*time.Minute || res.RetryAfterSeconds() != 2400 {
			t.Fatalf("hit %d has the retry time %v", i, res.RetryAfter)
		}
	}

	// another key has its own count
	other, err := limiter.Hit(ctx, "guest-signin:198.51.100.8", 3, time.Hour)
	if err != nil || other.Count != 1 || !other.Allowed {
		t.Fatalf("another key gave %+v, error %v", other, err)
	}

	// the last second of the window is still the same window, with one second left
	clk.set(time.Date(2026, 11, 17, 10, 59, 59, 0, time.UTC))

	res, err := limiter.Hit(ctx, "guest-signin:198.51.100.7", 3, time.Hour)
	if err != nil || res.Count != 6 || res.RetryAfter != time.Second {
		t.Fatalf("the last second gave %+v, error %v", res, err)
	}

	// the next window starts again at 1
	clk.set(time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC))

	res, err = limiter.Hit(ctx, "guest-signin:198.51.100.7", 3, time.Hour)
	if err != nil || res.Count != 1 || !res.Allowed || res.RetryAfter != time.Hour {
		t.Fatalf("the next window gave %+v, error %v", res, err)
	}
}

func TestHitIsAtomicUnderConcurrentCalls(t *testing.T) {
	pool := dbtest.New(t)
	clk := &clock{now: time.Date(2026, 11, 17, 10, 20, 0, 0, time.UTC)}
	limiter := ratelimit.New(pool, clk.Now)

	const calls = 20

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		counts  []int
		allowed int
		failure error
	)

	for range calls {
		wg.Add(1)

		go func() {
			defer wg.Done()

			res, err := limiter.Hit(context.Background(), "guest-signin:198.51.100.7", 10, time.Hour)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				failure = err

				return
			}

			counts = append(counts, res.Count)

			if res.Allowed {
				allowed++
			}
		}()
	}

	wg.Wait()

	if failure != nil {
		t.Fatalf("a call failed %v", failure)
	}

	// every call got its own count, with no lost update and no repeat
	slices.Sort(counts)

	for i, got := range counts {
		if got != i+1 {
			t.Fatalf("the counts are %v", counts)
		}
	}

	if allowed != 10 {
		t.Fatalf("%d calls were allowed, want 10", allowed)
	}

	var stored int
	if err := pool.QueryRow(context.Background(), `select count from rate_counters where key = 'guest-signin:198.51.100.7'`).Scan(&stored); err != nil {
		t.Fatalf("read the counter %v", err)
	}

	if stored != calls {
		t.Fatalf("the counter is %d, want %d", stored, calls)
	}
}

func TestDeleteBefore(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	cutoff := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

	for i, start := range []time.Time{cutoff.Add(-time.Hour), cutoff.Add(-time.Second), cutoff, cutoff.Add(time.Hour)} {
		if _, err := pool.Exec(ctx, `insert into rate_counters (key, window_start, count) values ($1, $2, 1)`, "key", start); err != nil {
			t.Fatalf("insert %d failed %v", i, err)
		}
	}

	deleted, err := ratelimit.DeleteBefore(ctx, pool, cutoff)
	if err != nil || deleted != 2 {
		t.Fatalf("deleted %d rows, error %v", deleted, err)
	}

	var left int
	if err := pool.QueryRow(ctx, `select count(*) from rate_counters where window_start >= $1`, cutoff).Scan(&left); err != nil || left != 2 {
		t.Fatalf("%d rows are left, error %v", left, err)
	}
}
