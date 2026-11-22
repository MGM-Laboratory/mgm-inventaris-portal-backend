package idempotency_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
)

var start = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

func newStore(t *testing.T) (*idempotency.Store, *pgxpool.Pool, *clock) {
	t.Helper()

	pool := dbtest.New(t)
	clk := &clock{now: start}

	return idempotency.NewStore(pool, clk.Now), pool, clk
}

func keyOf(session byte, operation, value string) idempotency.Key {
	return idempotency.Key{Session: []byte{session}, Operation: operation, Value: value}
}

// finish records an answer in a transaction and commits it, as the write of a route does.
func finish(t *testing.T, pool *pgxpool.Pool, claim idempotency.Claim, status int, body any) error {
	t.Helper()

	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed %v", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := claim.Record(ctx, tx, status, body); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit failed %v", err)
	}

	return nil
}

func TestValidKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"c0a80101-example-key", true},
		{"abcdefgh", true},
		{strings.Repeat("a", 64), true},
		{"abcdefg", false},
		{strings.Repeat("a", 65), false},
		{"has space in it", false},
		{"semi;colon-key", false},
		{"line\nbreak-key", false},
		{"", false},
	}

	for _, tc := range tests {
		if got := idempotency.ValidKey(tc.key); got != tc.want {
			t.Fatalf("ValidKey(%q) is %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestARepeatGetsTheStoredAnswerWhateverTheBodyIs(t *testing.T) {
	store, pool, _ := newStore(t)
	ctx := context.Background()
	key := keyOf(1, "POST /api/admin/models", "example-key-one")

	claim, answer, err := store.Begin(ctx, key)
	if err != nil || claim == nil || answer != nil {
		t.Fatalf("the first call gives %v, %v, %v", claim, answer, err)
	}

	if err := finish(t, pool, claim, 201, map[string]string{"id": "first"}); err != nil {
		t.Fatalf("record failed %v", err)
	}

	// the helper never sees the body of a repeat, so a repeat with another body gets the same answer
	for range 2 {
		claim, answer, err = store.Begin(ctx, key)
		if err != nil || claim != nil || answer == nil || answer.Status != 201 {
			t.Fatalf("a repeat gives %v, %v, %v", claim, answer, err)
		}

		var got map[string]string
		if err := json.Unmarshal(answer.Body, &got); err != nil || got["id"] != "first" {
			t.Fatalf("the stored body is %s, %v", answer.Body, err)
		}
	}
}

func TestAKeyCountsForOneSessionAndOneRoute(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()

	for _, key := range []idempotency.Key{
		keyOf(1, "POST /api/admin/models", "example-key-one"),
		keyOf(2, "POST /api/admin/models", "example-key-one"),
		keyOf(1, "POST /api/admin/categories", "example-key-one"),
		keyOf(1, "POST /api/admin/models", "example-key-two"),
	} {
		if claim, answer, err := store.Begin(ctx, key); err != nil || claim == nil || answer != nil {
			t.Fatalf("%+v gives %v, %v, %v", key, claim, answer, err)
		}
	}
}

func TestTwoParallelFirstCallsGiveOneClaim(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()
	key := keyOf(1, "POST /api/admin/models/:id/units", "example-key-one")

	const callers = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claims  int
		running int
		other   []error
	)

	gate := make(chan struct{})

	for range callers {
		wg.Go(func() {
			<-gate

			claim, _, err := store.Begin(ctx, key)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil && claim != nil:
				claims++
			case errors.Is(err, idempotency.ErrInProgress):
				running++
			default:
				other = append(other, err)
			}
		})
	}

	close(gate)
	wg.Wait()

	if claims != 1 || running != callers-1 || len(other) != 0 {
		t.Fatalf("%d claims, %d in progress, other errors %v", claims, running, other)
	}
}

func TestARequestThatFailsGivesTheKeyBack(t *testing.T) {
	store, pool, _ := newStore(t)
	ctx := context.Background()
	key := keyOf(1, "POST /api/admin/categories", "example-key-one")

	claim, _, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("begin failed %v", err)
	}

	// the write rolled back, so no answer was stored and the claim still runs
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed %v", err)
	}

	if err := claim.Record(ctx, tx, 201, map[string]string{"id": "never"}); err != nil {
		t.Fatalf("record failed %v", err)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback failed %v", err)
	}

	if _, _, err := store.Begin(ctx, key); !errors.Is(err, idempotency.ErrInProgress) {
		t.Fatalf("a repeat while the claim runs gives %v", err)
	}

	claim.Release(ctx)

	again, answer, err := store.Begin(ctx, key)
	if err != nil || again == nil || answer != nil {
		t.Fatalf("a repeat after the release gives %v, %v, %v", again, answer, err)
	}
}

func TestReleaseKeepsAnAnswerThatWasStored(t *testing.T) {
	store, pool, _ := newStore(t)
	ctx := context.Background()
	key := keyOf(1, "POST /api/admin/categories", "example-key-one")

	claim, _, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("begin failed %v", err)
	}

	if err := finish(t, pool, claim, 201, map[string]string{"id": "kept"}); err != nil {
		t.Fatalf("record failed %v", err)
	}

	claim.Release(ctx)

	if _, answer, err := store.Begin(ctx, key); err != nil || answer == nil {
		t.Fatalf("the answer is gone, %v, %v", answer, err)
	}
}

func TestAnAnswerExpiresAfterTheRetention(t *testing.T) {
	store, pool, clk := newStore(t)
	ctx := context.Background()
	key := keyOf(1, "POST /api/admin/models", "example-key-one")

	claim, _, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("begin failed %v", err)
	}

	if err := finish(t, pool, claim, 201, map[string]string{"id": "old"}); err != nil {
		t.Fatalf("record failed %v", err)
	}

	clk.advance(idempotency.Retention - time.Second)

	if _, answer, err := store.Begin(ctx, key); err != nil || answer == nil {
		t.Fatalf("one second before the end the answer is %v, %v", answer, err)
	}

	clk.advance(time.Second)

	fresh, answer, err := store.Begin(ctx, key)
	if err != nil || fresh == nil || answer != nil {
		t.Fatalf("at the end of the retention the call gives %v, %v, %v", fresh, answer, err)
	}
}

func TestADeadClaimIsTakenOverAndTheOldWriteCannotCommit(t *testing.T) {
	store, pool, clk := newStore(t)
	ctx := context.Background()
	key := keyOf(1, "POST /api/admin/models", "example-key-one")

	first, _, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("begin failed %v", err)
	}

	clk.advance(time.Minute)

	if _, _, err := store.Begin(ctx, key); !errors.Is(err, idempotency.ErrInProgress) {
		t.Fatalf("a young claim is not taken over, %v", err)
	}

	clk.advance(2 * time.Minute)

	second, answer, err := store.Begin(ctx, key)
	if err != nil || second == nil || answer != nil {
		t.Fatalf("an old claim is taken over, %v, %v, %v", second, answer, err)
	}

	if err := finish(t, pool, first, 201, map[string]string{"id": "late"}); !errors.Is(err, idempotency.ErrLost) {
		t.Fatalf("the old write records with %v", err)
	}

	if err := finish(t, pool, second, 201, map[string]string{"id": "new"}); err != nil {
		t.Fatalf("the new write fails to record %v", err)
	}
}

func TestDeleteExpiredKeepsTheKeysInsideTheRetention(t *testing.T) {
	store, pool, clk := newStore(t)
	ctx := context.Background()

	for _, value := range []string{"example-key-one", "example-key-two"} {
		if _, _, err := store.Begin(ctx, keyOf(1, "POST /api/admin/models", value)); err != nil {
			t.Fatalf("begin failed %v", err)
		}

		clk.advance(time.Hour)
	}

	// the first key is 2 hours old and the second 1 hour old, so a cutoff of 24 hours after the first one deletes only the first
	now := start.Add(idempotency.Retention)

	deleted, err := idempotency.DeleteExpired(ctx, pool, now)
	if err != nil || deleted != 1 {
		t.Fatalf("deleted %d, %v", deleted, err)
	}

	deleted, err = idempotency.DeleteExpired(ctx, pool, now.Add(time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("deleted %d, %v", deleted, err)
	}
}
