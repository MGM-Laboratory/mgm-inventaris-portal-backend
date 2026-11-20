package session_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

const queryTimeout = 20 * time.Second

// clock is a clock that a test moves by hand, so no test waits for time to pass.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)}
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

var identity = session.Identity{Name: "Example Borrower", NimNip: "000000000000000", Email: "example@student.ub.ac.id"}

func setup(t *testing.T) (*session.Store, *pgxpool.Pool, *clock) {
	t.Helper()

	pool := dbtest.New(t)
	clk := newClock()

	return session.NewStore(pool, clk.Now), pool, clk
}

func lastSeen(t *testing.T, pool *pgxpool.Pool, hash []byte) time.Time {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	var seen time.Time
	if err := pool.QueryRow(ctx, `select last_seen_at from sessions where token_hash = $1`, hash).Scan(&seen); err != nil {
		t.Fatalf("read last_seen_at %v", err)
	}

	return seen.UTC()
}

func countRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	var n int
	if err := pool.QueryRow(ctx, `select count(*) from sessions`).Scan(&n); err != nil {
		t.Fatalf("count the sessions %v", err)
	}

	return n
}

func TestCreateAndLoad(t *testing.T) {
	store, pool, clk := setup(t)
	ctx := context.Background()

	token, created, err := store.CreateGuest(ctx, &identity, nil)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	loaded, err := store.Load(ctx, token)
	if err != nil {
		t.Fatalf("load failed %v", err)
	}

	if loaded.Kind != session.KindGuest || loaded.Identity == nil || *loaded.Identity != identity || loaded.Admin != nil {
		t.Fatalf("the loaded session is %+v", loaded)
	}

	if loaded.CSRFToken == "" || loaded.CSRFToken != created.CSRFToken || loaded.CSRFToken == token {
		t.Fatal("the CSRF token is empty, differs from the created one, or equals the session token")
	}

	if len(loaded.ViewableTickets) != 0 {
		t.Fatalf("a new session lists %v tickets", loaded.ViewableTickets)
	}

	start := clk.Now()
	if !loaded.CreatedAt.Equal(start) || !loaded.ExpiresAt.Equal(start.Add(7*24*time.Hour)) || !loaded.IdleExpiresAt().Equal(start.Add(12*time.Hour)) {
		t.Fatalf("the times are %v %v %v", loaded.CreatedAt, loaded.ExpiresAt, loaded.IdleExpiresAt())
	}

	// the table holds the hash and not the token
	ctxQ, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var stored []byte
	if err := pool.QueryRow(ctxQ, `select token_hash from sessions`).Scan(&stored); err != nil {
		t.Fatalf("read the hash %v", err)
	}

	if len(stored) != 32 || !bytes.Equal(stored, session.HashToken(token)) || !bytes.Equal(stored, created.TokenHash) {
		t.Fatal("the stored value is not the SHA-256 of the token")
	}

	// a wrong token, a token of the wrong shape and an empty one are no session
	other, _ := session.NewToken()
	for _, bad := range []string{other, "junk", "", token + "A", token[:42]} {
		if _, err := store.Load(ctx, bad); !errors.Is(err, session.ErrNoSession) {
			t.Fatalf("Load(%q) gave %v, want ErrNoSession", bad, err)
		}
	}
}

func TestCreateWithoutAnIdentity(t *testing.T) {
	store, _, _ := setup(t)
	ctx := context.Background()

	token, _, err := store.CreateGuest(ctx, nil, nil)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	loaded, err := store.Load(ctx, token)
	if err != nil || loaded.Identity != nil || loaded.Kind != session.KindGuest {
		t.Fatalf("the session is %+v, error %v", loaded, err)
	}
}

func TestExpiredSessionsAreNotLoaded(t *testing.T) {
	store, _, clk := setup(t)
	ctx := context.Background()

	token, _, err := store.CreateGuest(ctx, &identity, nil)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	// idle limit, 12 hours without a request
	clk.advance(12*time.Hour - time.Second)

	if _, err := store.Load(ctx, token); err != nil {
		t.Fatalf("a session one second before the idle limit gave %v", err)
	}

	// that load touched the session, so the idle time starts again, and 12 hours later it is over
	clk.advance(12 * time.Hour)

	if _, err := store.Load(ctx, token); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("an idle session gave %v", err)
	}
}

func TestAbsoluteLimitHoldsAgainstConstantUse(t *testing.T) {
	store, _, clk := setup(t)
	ctx := context.Background()

	token, _, err := store.CreateGuest(ctx, &identity, nil)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	// a request every 6 hours keeps the session from going idle, and the absolute limit of 7 days still ends it
	for hours := 6; hours < 7*24; hours += 6 {
		clk.advance(6 * time.Hour)

		if _, err := store.Load(ctx, token); err != nil {
			t.Fatalf("the session failed after %d hours with %v", hours, err)
		}
	}

	clk.advance(6 * time.Hour)

	if _, err := store.Load(ctx, token); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("a session at the absolute limit gave %v", err)
	}
}

func TestSignInEndsThePreviousSession(t *testing.T) {
	store, pool, _ := setup(t)
	ctx := context.Background()

	first, firstSession, err := store.CreateGuest(ctx, &identity, nil)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	second, _, err := store.CreateGuest(ctx, &identity, firstSession.TokenHash)
	if err != nil {
		t.Fatalf("replace failed %v", err)
	}

	if _, err := store.Load(ctx, first); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("the old session gave %v", err)
	}

	if _, err := store.Load(ctx, second); err != nil {
		t.Fatalf("the new session gave %v", err)
	}

	if n := countRows(t, pool); n != 1 {
		t.Fatalf("the table holds %d rows", n)
	}

	// a previous hash that matches nothing is not an error and deletes nothing else
	unknown := session.HashToken("nothing")
	if _, _, err := store.CreateGuest(ctx, &identity, unknown); err != nil {
		t.Fatalf("a replace of an unknown session failed %v", err)
	}

	if n := countRows(t, pool); n != 2 {
		t.Fatalf("the table holds %d rows", n)
	}
}

func TestLastSeenIsWrittenAtMostOnceAMinute(t *testing.T) {
	store, pool, clk := setup(t)
	ctx := context.Background()

	start := clk.Now()

	token, created, err := store.CreateGuest(ctx, &identity, nil)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	steps := []struct {
		advance time.Duration
		want    time.Duration
	}{
		{30 * time.Second, 0},
		{29 * time.Second, 0},
		// 59 seconds in total, still inside the minute
		{time.Second, 60 * time.Second},
		{20 * time.Second, 60 * time.Second},
		{39 * time.Second, 60 * time.Second},
		{time.Second, 120 * time.Second},
		{10 * time.Minute, 720 * time.Second},
	}

	for i, step := range steps {
		clk.advance(step.advance)

		loaded, err := store.Load(ctx, token)
		if err != nil {
			t.Fatalf("step %d, load failed %v", i, err)
		}

		want := start.Add(step.want)
		if got := lastSeen(t, pool, created.TokenHash); !got.Equal(want) {
			t.Fatalf("step %d, last_seen_at is %v, want %v", i, got, want)
		}

		if !loaded.LastSeenAt.Equal(want) {
			t.Fatalf("step %d, the loaded session shows %v, want %v", i, loaded.LastSeenAt, want)
		}
	}
}

func TestLastSeenNeverMovesBack(t *testing.T) {
	store, pool, clk := setup(t)
	ctx := context.Background()

	token, created, err := store.CreateGuest(ctx, &identity, nil)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	clk.advance(10 * time.Minute)

	if _, err := store.Load(ctx, token); err != nil {
		t.Fatalf("load failed %v", err)
	}

	written := lastSeen(t, pool, created.TokenHash)

	// a copy whose clock is behind loads the same session, and it writes nothing
	clk.advance(-5 * time.Minute)

	if _, err := store.Load(ctx, token); err != nil {
		t.Fatalf("load with a clock that is behind failed %v", err)
	}

	if got := lastSeen(t, pool, created.TokenHash); !got.Equal(written) {
		t.Fatalf("last_seen_at moved from %v to %v", written, got)
	}
}

func TestAdminSessionsLoadTheirAccountAndTheirOwnLimits(t *testing.T) {
	store, pool, clk := setup(t)
	ctx := context.Background()

	var adminID string

	err := pool.QueryRow(ctx, `insert into admins (email, name, password_hash) values ('admin@example.org', 'Example Admin', 'hash')
		returning id::text`).Scan(&adminID)
	if err != nil {
		t.Fatalf("insert the admin %v", err)
	}

	start := clk.Now()

	insert := func(kind string, expires time.Duration) string {
		token, _ := session.NewToken()

		_, err := pool.Exec(ctx, `insert into sessions (token_hash, kind, admin_id, csrf_token, created_at, last_seen_at, expires_at)
			values ($1, $2, $3::text::uuid, 'csrf', $4, $4, $5)`, session.HashToken(token), kind, adminID, start, start.Add(expires))
		if err != nil {
			t.Fatalf("insert the %s session %v", kind, err)
		}

		return token
	}

	adminToken := insert("admin", 8*time.Hour)
	setupToken := insert("admin_setup", 15*time.Minute)

	loaded, err := store.Load(ctx, adminToken)
	if err != nil {
		t.Fatalf("load the admin session %v", err)
	}

	if loaded.Kind != session.KindAdmin || loaded.Identity != nil ||
		loaded.Admin == nil || *loaded.Admin != (session.Admin{ID: adminID, Name: "Example Admin", Email: "admin@example.org"}) {
		t.Fatalf("the admin session is %+v", loaded)
	}

	// 15 minutes idle ends a setup session and 30 minutes ends an admin session
	clk.advance(15 * time.Minute)

	if _, err := store.Load(ctx, setupToken); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("the setup session gave %v", err)
	}

	// the load at the start of the test touched nothing, because it was at the same second, so 30 minutes from the start ends it
	clk.advance(15 * time.Minute)

	if _, err := store.Load(ctx, adminToken); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("the admin session gave %v", err)
	}
}

func TestDelete(t *testing.T) {
	store, pool, _ := setup(t)
	ctx := context.Background()

	token, created, err := store.CreateGuest(ctx, &identity, nil)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	if err := store.Delete(ctx, created.TokenHash); err != nil {
		t.Fatalf("delete failed %v", err)
	}

	if _, err := store.Load(ctx, token); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("a deleted session gave %v", err)
	}

	// a second delete is not an error
	if err := store.Delete(ctx, created.TokenHash); err != nil {
		t.Fatalf("a second delete failed %v", err)
	}

	if n := countRows(t, pool); n != 0 {
		t.Fatalf("the table holds %d rows", n)
	}
}

func TestDeleteExpiredKeepsLiveSessions(t *testing.T) {
	store, pool, clk := setup(t)
	ctx := context.Background()

	var adminID string
	if err := pool.QueryRow(ctx, `insert into admins (email, name, password_hash) values ('admin@example.org', 'Example Admin', 'hash')
		returning id::text`).Scan(&adminID); err != nil {
		t.Fatalf("insert the admin %v", err)
	}

	start := clk.Now()
	insert := func(kind string, seen time.Time, expires time.Time) []byte {
		token, _ := session.NewToken()
		hash := session.HashToken(token)

		var admin any
		if kind != "guest" {
			admin = adminID
		}

		if _, err := pool.Exec(ctx, `insert into sessions (token_hash, kind, admin_id, csrf_token, created_at, last_seen_at, expires_at)
			values ($1, $2, $3::text::uuid, 'csrf', $4, $5, $6)`, hash, kind, admin, start.Add(-30*24*time.Hour), seen, expires); err != nil {
			t.Fatalf("insert the %s session %v", kind, err)
		}

		return hash
	}

	future := start.Add(24 * time.Hour)

	live := [][]byte{
		insert("guest", start.Add(-11*time.Hour), future),
		insert("admin", start.Add(-29*time.Minute), future),
		insert("admin_setup", start.Add(-14*time.Minute), future),
		insert("guest", start, start.Add(time.Second)),
	}
	gone := [][]byte{
		insert("guest", start.Add(-12*time.Hour), future),
		insert("admin", start.Add(-30*time.Minute), future),
		insert("admin_setup", start.Add(-15*time.Minute), future),
		insert("guest", start, start),
		insert("guest", start, start.Add(-time.Hour)),
	}

	deleted, err := store.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired failed %v", err)
	}

	if deleted != int64(len(gone)) {
		t.Fatalf("deleted %d rows, want %d", deleted, len(gone))
	}

	exists := func(hash []byte) bool {
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from sessions where token_hash = $1`, hash).Scan(&n); err != nil {
			t.Fatalf("count %v", err)
		}

		return n == 1
	}

	for i, hash := range live {
		if !exists(hash) {
			t.Fatalf("live session %d was deleted", i)
		}
	}

	for i, hash := range gone {
		if exists(hash) {
			t.Fatalf("expired session %d is still there", i)
		}
	}
}
