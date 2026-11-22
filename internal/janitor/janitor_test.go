package janitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/janitor"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

var now = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

func insertSession(t *testing.T, pool *pgxpool.Pool, seen, expires time.Time) []byte {
	t.Helper()

	token, _ := session.NewToken()
	hash := session.HashToken(token)

	_, err := pool.Exec(context.Background(), `insert into sessions (token_hash, kind, csrf_token, created_at, last_seen_at, expires_at)
		values ($1, 'guest', 'csrf', $2, $2, $3)`, hash, now.Add(-30*24*time.Hour), expires)
	if err != nil {
		t.Fatalf("insert a session %v", err)
	}

	if _, err := pool.Exec(context.Background(), `update sessions set last_seen_at = $2 where token_hash = $1`, hash, seen); err != nil {
		t.Fatalf("set last_seen_at %v", err)
	}

	return hash
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count failed %v", err)
	}

	return n
}

func TestSweepDeletesOnlyExpiredRows(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()

	liveHash := insertSession(t, pool, now.Add(-time.Hour), now.Add(time.Hour))
	insertSession(t, pool, now.Add(-time.Hour), now)
	insertSession(t, pool, now.Add(-13*time.Hour), now.Add(time.Hour))

	for i, start := range []time.Time{now.Add(-25 * time.Hour), now.Add(-24*time.Hour - time.Second), now.Add(-24 * time.Hour), now.Add(-time.Hour)} {
		if _, err := pool.Exec(ctx, `insert into rate_counters (key, window_start, count) values ($1, $2, 1)`, "key", start); err != nil {
			t.Fatalf("insert counter %d failed %v", i, err)
		}
	}

	res, err := janitor.Sweep(ctx, pool, now)
	if err != nil {
		t.Fatalf("the sweep failed %v", err)
	}

	if !res.Ran || res.Sessions != 2 || res.Counters != 2 {
		t.Fatalf("the result is %+v", res)
	}

	if n := count(t, pool, `select count(*) from sessions`); n != 1 {
		t.Fatalf("%d sessions are left", n)
	}

	if n := count(t, pool, `select count(*) from sessions where token_hash = $1`, liveHash); n != 1 {
		t.Fatal("the live session was deleted")
	}

	// the counter that started exactly 24 hours ago and the recent one stay
	if n := count(t, pool, `select count(*) from rate_counters`); n != 2 {
		t.Fatalf("%d counters are left", n)
	}

	// a second sweep finds nothing and still runs
	res, err = janitor.Sweep(ctx, pool, now)
	if err != nil || !res.Ran || res.Sessions != 0 || res.Counters != 0 {
		t.Fatalf("the second sweep gave %+v, error %v", res, err)
	}
}

func TestSweepDeletesOnlyIdempotencyKeysOlderThanTheRetention(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()

	for i, created := range []time.Time{now.Add(-25 * time.Hour), now.Add(-24 * time.Hour), now.Add(-23 * time.Hour)} {
		_, err := pool.Exec(ctx, `insert into idempotency_keys (session_hash, operation, key, state, created_at) values ($1, 'op', $2, 'running', $3)`,
			[]byte{1}, "example-key-"+string(rune('a'+i)), created)
		if err != nil {
			t.Fatalf("insert key %d failed %v", i, err)
		}
	}

	res, err := janitor.Sweep(ctx, pool, now)
	if err != nil || !res.Ran || res.Keys != 2 {
		t.Fatalf("the sweep gave %+v, error %v", res, err)
	}

	if n := count(t, pool, `select count(*) from idempotency_keys`); n != 1 {
		t.Fatalf("%d keys are left", n)
	}
}

func TestSweepSkipsWhileAnotherCopyHoldsTheLock(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()

	insertSession(t, pool, now.Add(-13*time.Hour), now.Add(time.Hour))

	// the other copy is in the middle of its sweep
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %v", err)
	}

	if _, err := other.Exec(ctx, `select pg_advisory_xact_lock($1)`, janitor.LockKey); err != nil {
		t.Fatalf("take the lock %v", err)
	}

	res, err := janitor.Sweep(ctx, pool, now)
	if err != nil || res.Ran {
		t.Fatalf("a sweep with the lock held by another copy gave %+v, error %v", res, err)
	}

	if n := count(t, pool, `select count(*) from sessions`); n != 1 {
		t.Fatalf("the skipped sweep changed the table, %d rows", n)
	}

	// the lock ends with the transaction of the other copy, so the next sweep runs
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit %v", err)
	}

	res, err = janitor.Sweep(ctx, pool, now)
	if err != nil || !res.Ran || res.Sessions != 1 {
		t.Fatalf("the sweep after the release gave %+v, error %v", res, err)
	}
}

// failingDB is a database that cannot begin a transaction.
type failingDB struct{}

func (failingDB) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("no database") }

func TestRunStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)

		janitor.Run(ctx, failingDB{}, time.Hour, func() time.Time { return now })
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop after the context ended")
	}
}
