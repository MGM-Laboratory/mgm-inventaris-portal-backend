package ratelimit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ratelimit"
)

func TestRefundTakesBackOneHit(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	clk := &clock{now: time.Date(2026, 11, 17, 10, 20, 0, 0, time.UTC)}
	limiter := ratelimit.New(pool, clk.Now)

	first, err := limiter.Hit(ctx, "admin-signin-pair:198.51.100.7:x", 5, 15*time.Minute)
	if err != nil {
		t.Fatalf("hit failed %v", err)
	}

	// the window of the hit is the one that holds 10:20, which starts at 10:15
	if want := time.Date(2026, 11, 17, 10, 15, 0, 0, time.UTC); !first.WindowStart.Equal(want) {
		t.Fatalf("the window starts at %v, want %v", first.WindowStart, want)
	}

	if _, err := limiter.Hit(ctx, "admin-signin-pair:198.51.100.7:x", 5, 15*time.Minute); err != nil {
		t.Fatalf("hit failed %v", err)
	}

	if err := limiter.Refund(ctx, "admin-signin-pair:198.51.100.7:x", first.WindowStart); err != nil {
		t.Fatalf("refund failed %v", err)
	}

	next, err := limiter.Hit(ctx, "admin-signin-pair:198.51.100.7:x", 5, 15*time.Minute)
	if err != nil || next.Count != 2 {
		t.Fatalf("after two hits and one refund the next hit counts %d, %v", next.Count, err)
	}

	// a refund never takes the count below zero, and a window or a key that does not exist is not an error
	for range 4 {
		if err := limiter.Refund(ctx, "admin-signin-pair:198.51.100.7:x", first.WindowStart); err != nil {
			t.Fatalf("refund failed %v", err)
		}
	}

	if err := limiter.Refund(ctx, "no-such-key", first.WindowStart); err != nil {
		t.Fatalf("a refund of an unknown key failed %v", err)
	}

	zero, _ := limiter.Hit(ctx, "admin-signin-pair:198.51.100.7:x", 5, 15*time.Minute)
	if zero.Count != 1 {
		t.Fatalf("after too many refunds the next hit counts %d, want 1", zero.Count)
	}

	// a refund of an earlier window leaves the present window alone
	earlier := first.WindowStart.Add(-15 * time.Minute)
	if err := limiter.Refund(ctx, "admin-signin-pair:198.51.100.7:x", earlier); err != nil {
		t.Fatalf("refund failed %v", err)
	}

	again, _ := limiter.Hit(ctx, "admin-signin-pair:198.51.100.7:x", 5, 15*time.Minute)
	if again.Count != 2 {
		t.Fatalf("a refund of another window changed the count to %d", again.Count)
	}
}

// TestReserveAndRefundCountFailuresOnly follows the use of the admin sign in. Every request hits first and refunds when it does not
// fail, so parallel guesses are counted before they finish, and only the failures stay.
func TestReserveAndRefundCountFailuresOnly(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	clk := &clock{now: time.Date(2026, 11, 17, 10, 20, 0, 0, time.UTC)}
	limiter := ratelimit.New(pool, clk.Now)

	const key = "admin-account:abc"

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)

	// twenty parallel guesses against a limit of 5. Every one of them fails, so none refunds. The limit lets exactly 5 through.
	for range 20 {
		wg.Go(func() {
			res, err := limiter.Hit(ctx, key, 5, 15*time.Minute)
			if err != nil {
				t.Errorf("hit failed %v", err)

				return
			}

			if res.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	if allowed != 5 {
		t.Fatalf("%d of 20 parallel requests were allowed, want 5", allowed)
	}

	// a blocked request counts like any other hit until its caller refunds it, and a later window starts at 0
	res, err := limiter.Hit(ctx, key, 5, 15*time.Minute)
	if err != nil || res.Allowed || res.Count != 21 {
		t.Fatalf("the blocked request gave %+v and %v", res, err)
	}

	if err := limiter.Refund(ctx, key, res.WindowStart); err != nil {
		t.Fatalf("refund failed %v", err)
	}

	clk.set(clk.Now().Add(15 * time.Minute))

	fresh, _ := limiter.Hit(ctx, key, 5, 15*time.Minute)
	if !fresh.Allowed || fresh.Count != 1 {
		t.Fatalf("the next window gave %+v", fresh)
	}
}
