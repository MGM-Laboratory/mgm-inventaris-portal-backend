package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/password"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ratelimit"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/secretbox"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

// These tests run the admin routes on a real PostgreSQL and skip without DATABASE_URL. They prove that the counters of the limits
// hold failures only, with the real statements of the limiter, and that parallel requests are counted before they finish.

func counter(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), `select coalesce(sum(count), 0) from rate_counters where key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("read the counter failed %v", err)
	}

	return n
}

func pairKey(addr, email string) string {
	sum := sha256.Sum256([]byte(email))

	return "admin-signin-pair:" + addr + ":" + hex.EncodeToString(sum[:])
}

func newDBAdminApp(t *testing.T, mutate func(*config.Config)) (*pgxpool.Pool, *adminEnv) {
	t.Helper()

	pool := dbtest.New(t)

	// the clock stands still at the start of a window, so no test crosses into the next one
	clk := &stepClock{now: time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)}

	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("make the box %v", err)
	}

	cfg := adminConfig()
	if mutate != nil {
		mutate(&cfg)
	}

	svc := admin.NewService(admin.NewPostgres(pool), password.New(2, 10*time.Second), box, clk.Now)
	store := session.NewStore(pool, clk.Now)

	env := &adminEnv{t: t, clock: clk, svc: svc, box: box}
	env.app = NewAPI(cfg, Deps{Sessions: store, Limiter: ratelimit.New(pool, clk.Now), Admins: svc})

	return pool, env
}

func TestAdminSignInCountersOnARealDatabase(t *testing.T) {
	pool, env := newDBAdminApp(t, func(c *config.Config) { c.AdminSigninAddressFailures = 7 })

	good := env.newAdmin("admin@example.org")
	env.newAdmin("other@example.org")

	wrong := func(email string) *http.Response {
		res, _, _ := env.signIn(client{}, env.signInBody(email, otherPassword, nil))

		return res
	}

	pairA, address := pairKey(defaultAddr, "admin@example.org"), "admin-signin-address:"+defaultAddr

	// a success is counted and refunded at once, so it leaves the counters where they were
	if res, data, _ := env.signIn(client{}, env.signInBody(good.email, adminPassword, nil)); res.StatusCode != http.StatusCreated {
		t.Fatalf("the sign in answers %d, %s", res.StatusCode, data)
	}

	if counter(t, pool, pairA) != 0 || counter(t, pool, address) != 0 {
		t.Fatalf("a success left the counters at %d and %d", counter(t, pool, pairA), counter(t, pool, address))
	}

	for i := 1; i <= 4; i++ {
		if res := wrong(good.email); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d answers %d", i, res.StatusCode)
		}
	}

	if counter(t, pool, pairA) != 4 || counter(t, pool, address) != 4 {
		t.Fatalf("four failures made the counters %d and %d", counter(t, pool, pairA), counter(t, pool, address))
	}

	// a success between the failures does not reset them
	if res, _, _ := env.signIn(client{}, env.signInBody(good.email, adminPassword, nil)); res.StatusCode != http.StatusCreated {
		t.Fatalf("the sign in answers %d", res.StatusCode)
	}

	if counter(t, pool, pairA) != 4 {
		t.Fatalf("a success changed the pair counter to %d", counter(t, pool, pairA))
	}

	if res := wrong(good.email); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the fifth failure answers %d", res.StatusCode)
	}

	res, data, _ := env.signIn(client{}, env.signInBody(good.email, adminPassword, nil))
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusTooManyRequests || code != "rate_limited" || res.Header.Get("Retry-After") != "900" {
		t.Fatalf("the sixth request answers %d, %s with Retry-After %q", res.StatusCode, data, res.Header.Get("Retry-After"))
	}

	// the blocked request was refunded, and it never reached the counter of the address
	if counter(t, pool, pairA) != 5 || counter(t, pool, address) != 5 {
		t.Fatalf("the counters are %d and %d, want 5 and 5", counter(t, pool, pairA), counter(t, pool, address))
	}

	// two failures of another email use the rest of the limit of the address
	for range 2 {
		if res := wrong("other@example.org"); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a failure for another email answers %d", res.StatusCode)
		}
	}

	res, data, _ = env.signIn(client{}, env.signInBody("other@example.org", adminPassword, nil))
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusTooManyRequests || code != "rate_limited" || res.Header.Get("Retry-After") != "3600" {
		t.Fatalf("the request over the address limit answers %d, %s with Retry-After %q", res.StatusCode, data, res.Header.Get("Retry-After"))
	}

	pairB := pairKey(defaultAddr, "other@example.org")
	if counter(t, pool, pairB) != 2 || counter(t, pool, address) != 7 {
		t.Fatalf("the counters are %d and %d, want 2 and 7, so the refund of the blocked request is missing", counter(t, pool, pairB), counter(t, pool, address))
	}

	// another address has counters of its own
	if res, _, _ := env.signIn(client{addr: "198.51.100.99"}, env.signInBody(good.email, adminPassword, nil)); res.StatusCode != http.StatusCreated {
		t.Fatalf("another address answers %d", res.StatusCode)
	}
}

func TestParallelAdminGuessesAreCountedBeforeTheyFinish(t *testing.T) {
	pool, env := newDBAdminApp(t, nil)
	p := env.newAdmin("admin@example.org")

	results := make(chan int, 12)

	for range 12 {
		go func() {
			res, _, _ := env.signIn(client{}, env.signInBody(p.email, otherPassword, nil))
			results <- res.StatusCode
		}()
	}

	byStatus := map[int]int{}
	for range 12 {
		byStatus[<-results]++
	}

	// the limit is 5 failures for the pair, so five guesses reach the password check, and the other seven are refused at the door
	if byStatus[http.StatusUnauthorized] != 5 || byStatus[http.StatusTooManyRequests] != 7 {
		t.Fatalf("the answers are %v, want five 401 and seven 429", byStatus)
	}

	if got := counter(t, pool, pairKey(defaultAddr, p.email)); got != 5 {
		t.Fatalf("the pair counter is %d, want 5", got)
	}
}

func TestAccountCountersOnARealDatabase(t *testing.T) {
	pool, env := newDBAdminApp(t, nil)
	p := env.newAdmin("admin@example.org")

	_, _, setup := env.signIn(client{}, env.signInBody(p.email, adminPassword, nil))
	if setup.token == "" {
		t.Fatal("no setup session")
	}

	key := "admin-account:" + hex.EncodeToString(session.HashToken(setup.token))
	wrong := jsonOf(t, map[string]any{"currentPassword": otherPassword, "newPassword": "example brand new password"})

	for i := 1; i <= 5; i++ {
		res, data := env.call(setup, http.MethodPost, "/api/admin/me/password", wrong)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d answers %d, %s", i, res.StatusCode, data)
		}
	}

	res, data := env.call(setup, http.MethodPost, "/api/admin/me/password", jsonOf(t, map[string]any{"currentPassword": adminPassword, "newPassword": "example brand new password"}))
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusTooManyRequests || code != "rate_limited" || res.Header.Get("Retry-After") != "900" {
		t.Fatalf("the sixth request answers %d, %s", res.StatusCode, data)
	}

	if got := counter(t, pool, key); got != 5 {
		t.Fatalf("the session counter is %d, want 5", got)
	}

	// the key holds the hash of the token and not the token
	var leaked int
	if err := pool.QueryRow(context.Background(), `select count(*) from rate_counters where strpos(key, $1) > 0`, setup.token).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("a key holds the session token, %d %v", leaked, err)
	}

	// the password did not change, so the old one still signs in
	if res, _, _ := env.signIn(client{addr: "198.51.100.50"}, env.signInBody(p.email, adminPassword, nil)); res.StatusCode != http.StatusCreated {
		t.Fatalf("the old password answers %d", res.StatusCode)
	}
}
