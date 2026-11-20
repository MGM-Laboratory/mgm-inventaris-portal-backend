package server

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin/admintest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/password"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ratelimit"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/secretbox"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/totp"
)

const (
	adminPassword = "example admin password"
	otherPassword = "example other password"
	defaultAddr   = "198.51.100.7"
)

type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// countingLimiter counts like the real limiter, with one window for ever. The window it reports is the one that was asked for.
type countingLimiter struct {
	mu      sync.Mutex
	counts  map[string]int
	limits  map[string]int
	windows map[string]time.Duration
	refunds int
}

func newCountingLimiter() *countingLimiter {
	return &countingLimiter{counts: map[string]int{}, limits: map[string]int{}, windows: map[string]time.Duration{}}
}

func (l *countingLimiter) Hit(_ context.Context, key string, limit int, window time.Duration) (ratelimit.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.counts[key]++
	l.limits[key] = limit
	l.windows[key] = window

	return ratelimit.Result{Count: l.counts[key], Allowed: l.counts[key] <= limit, RetryAfter: window}, nil
}

func (l *countingLimiter) Refund(_ context.Context, key string, _ time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.counts[key] = max(l.counts[key]-1, 0)
	l.refunds++

	return nil
}

// count returns the count of the one key that starts with the prefix.
func (l *countingLimiter) count(t *testing.T, prefix string) int {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()

	var found []string

	for key := range l.counts {
		if strings.HasPrefix(key, prefix) {
			found = append(found, key)
		}
	}

	if len(found) != 1 {
		t.Fatalf("%d keys start with %q, %v", len(found), prefix, found)
	}

	return l.counts[found[0]]
}

func (l *countingLimiter) keys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]string, 0, len(l.counts))
	for key := range l.counts {
		out = append(out, key)
	}

	return out
}

type adminEnv struct {
	t       *testing.T
	app     *fiber.App
	store   *fakeStore
	limiter *countingLimiter
	repo    *admintest.Memory
	box     *secretbox.Box
	clock   *stepClock
	svc     *admin.Service
}

func adminConfig() config.Config {
	cfg := testConfig()
	cfg.AdminSigninPairFailures = 5
	cfg.AdminSigninAddressFailures = 20
	cfg.AdminAccountFailures = 5
	// the test connection has the peer 0.0.0.0, so trusting it makes X-Real-IP count
	cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/32")}

	return cfg
}

func newAdminEnv(t *testing.T, mutate ...func(*config.Config)) *adminEnv {
	t.Helper()

	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("make the box %v", err)
	}

	env := &adminEnv{
		t: t, store: newFakeStore(), limiter: newCountingLimiter(), repo: admintest.NewMemory(), box: box,
		clock: &stepClock{now: time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)},
	}

	env.svc = admin.NewService(env.repo, password.New(2, 5*time.Second), box, env.clock.Now)
	env.repo.OnSetPassword = env.store.endOthers

	cfg := adminConfig()
	for _, m := range mutate {
		m(&cfg)
	}

	env.app = NewAPI(cfg, Deps{Sessions: env.store, Limiter: env.limiter, Admins: env.svc})

	return env
}

// person is an admin of a test with the secret and the recovery codes of its TOTP.
type person struct {
	id, email string
	secret    []byte
	codes     []string
}

// newAdmin makes an admin with the initial password and no TOTP.
func (e *adminEnv) newAdmin(email string) person {
	e.t.Helper()

	id, _, err := e.svc.Create(context.Background(), admin.CreateInput{Email: email, Name: "Example Admin", Password: adminPassword})
	if err != nil {
		e.t.Fatalf("create failed %v", err)
	}

	return person{id: id, email: email}
}

// newFullAdmin makes an admin with TOTP, ten recovery codes and a password of its own that does not expire.
func (e *adminEnv) newFullAdmin(email string) person {
	e.t.Helper()

	p := e.newAdmin(email)

	secret, err := totp.NewSecret()
	if err != nil {
		e.t.Fatalf("make the secret %v", err)
	}

	sealed, err := e.box.Seal(secret, []byte(p.id))
	if err != nil {
		e.t.Fatalf("seal failed %v", err)
	}

	account, err := e.repo.ByID(context.Background(), p.id)
	if err != nil {
		e.t.Fatalf("read failed %v", err)
	}

	enabled := e.clock.Now()
	account.TOTPSecret, account.TOTPEnabledAt, account.PasswordExpiresAt = sealed, &enabled, nil
	e.repo.Put(account)

	codes, hashes, err := admin.NewRecoveryCodes(admin.RecoveryCodeCount)
	if err != nil {
		e.t.Fatalf("make the codes %v", err)
	}

	if err := e.repo.ReplaceRecoveryCodes(context.Background(), p.id, hashes); err != nil {
		e.t.Fatalf("store the codes %v", err)
	}

	p.secret, p.codes = secret, codes

	return p
}

// code returns a code of a step above every step so far.
func (e *adminEnv) code(p person) string {
	e.clock.advance(totp.StepSeconds * time.Second)

	return totp.Code(p.secret, totp.Step(e.clock.Now()))
}

// client is a session of a test.
type client struct {
	token, csrf, addr string
}

func (c client) headers(extra map[string]string) map[string]string {
	h := map[string]string{"Origin": testOrigin, "Content-Type": "application/json", "X-Real-IP": defaultAddr}

	if c.addr != "" {
		h["X-Real-IP"] = c.addr
	}

	if c.token != "" {
		h["Cookie"] = session.CookieName(true) + "=" + c.token
	}

	if c.csrf != "" {
		h["X-CSRF-Token"] = c.csrf
	}

	for k, v := range extra {
		h[k] = v
	}

	return h
}

func (e *adminEnv) call(c client, method, path, body string) (*http.Response, []byte) {
	e.t.Helper()

	return mustSend(e.t, e.app, method, path, body, c.headers(nil))
}

func jsonOf(t *testing.T, v map[string]any) string {
	t.Helper()

	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed %v", err)
	}

	return string(out)
}

func (e *adminEnv) signInBody(email, pw string, extra map[string]any) string {
	body := map[string]any{"email": email, "password": pw}
	for k, v := range extra {
		body[k] = v
	}

	return jsonOf(e.t, body)
}

// signIn calls the sign in route and returns the new session when it answers 201.
func (e *adminEnv) signIn(c client, body string) (*http.Response, []byte, client) {
	e.t.Helper()

	res, data := e.call(c, http.MethodPost, "/api/admin/session", body)
	if res.StatusCode != http.StatusCreated {
		return res, data, client{}
	}

	var answer sessionAnswer
	if err := json.Unmarshal(data, &answer); err != nil {
		e.t.Fatalf("the answer is not JSON %v", err)
	}

	return res, data, client{token: sessionCookieOf(e.t, res).Value, csrf: answer.CSRFToken, addr: c.addr}
}

// login signs a person in with the password and a fresh TOTP code and returns the admin session.
func (e *adminEnv) login(p person) client {
	e.t.Helper()

	res, data, c := e.signIn(client{}, e.signInBody(p.email, adminPassword, map[string]any{"totpCode": e.code(p)}))
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("the sign in answers %d, %s", res.StatusCode, data)
	}

	return c
}

func errorOf(t *testing.T, data []byte) (code, message string) {
	t.Helper()

	e := decodeError(t, data)

	return e.Error.Code, e.Error.Message
}

func TestAdminSignInKinds(t *testing.T) {
	env := newAdminEnv(t)

	full := env.newFullAdmin("full@example.org")
	env.newAdmin("setup@example.org")

	expired := env.newFullAdmin("expired@example.org")
	account, _ := env.repo.ByID(context.Background(), expired.id)
	past := env.clock.Now().Add(-time.Hour)
	account.PasswordExpiresAt = &past
	env.repo.Put(account)

	tests := []struct {
		name     string
		body     string
		kind     string
		maxAge   int
		idleness time.Duration
	}{
		{"password and TOTP code", env.signInBody("Full@Example.org", adminPassword, map[string]any{"totpCode": env.code(full)}), "admin", 8 * 3600, 8*time.Hour - 30*time.Minute},
		{"password and recovery code", env.signInBody("full@example.org", adminPassword, map[string]any{"recoveryCode": strings.ToLower(full.codes[0])}), "admin", 8 * 3600, 8*time.Hour - 30*time.Minute},
		{"no TOTP yet", env.signInBody("setup@example.org", adminPassword, nil), "admin_setup", 900, 0},
		{"initial password expired", env.signInBody("expired@example.org", adminPassword, map[string]any{"totpCode": env.code(expired)}), "admin_setup", 900, 0},
	}

	for _, tc := range tests {
		res, data, _ := env.signIn(client{}, tc.body)
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("%s answers %d, %s", tc.name, res.StatusCode, data)
		}

		var got struct {
			sessionAnswer
			Admin *struct {
				ID, Name, Email string
			} `json:"admin"`
		}

		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("%s gave bad JSON %v", tc.name, err)
		}

		if got.Kind != tc.kind || got.CSRFToken == "" || got.Admin == nil || got.Admin.Name != "Example Admin" || got.Identity != nil {
			t.Fatalf("%s gave %s", tc.name, data)
		}

		cookie := sessionCookieOf(t, res)
		if cookie.Name != "__Host-mgm_session" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != tc.maxAge {
			t.Fatalf("%s set the cookie %+v", tc.name, cookie)
		}

		if res.Header.Get("Cache-Control") != "no-store" || !env.store.has(cookie.Value) {
			t.Fatalf("%s did not store the session or lost a header", tc.name)
		}

		expires, _ := time.Parse(time.RFC3339, got.ExpiresAt)
		if tc.kind == "admin_setup" && time.Until(expires) > 16*time.Minute {
			t.Fatalf("%s lasts until %s", tc.name, got.ExpiresAt)
		}
	}
}

func TestAdminSignInAnswersTheSameForEveryWrongValue(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")

	good := env.code(p)
	bad := "000000"
	if good == bad {
		bad = "000001"
	}

	tests := []struct {
		name string
		body string
	}{
		{"unknown email", env.signInBody("nobody@example.org", adminPassword, map[string]any{"totpCode": good})},
		{"wrong password", env.signInBody(p.email, otherPassword, map[string]any{"totpCode": good})},
		{"wrong code", env.signInBody(p.email, adminPassword, map[string]any{"totpCode": bad})},
		{"no code", env.signInBody(p.email, adminPassword, nil)},
		{"wrong recovery code", env.signInBody(p.email, adminPassword, map[string]any{"recoveryCode": "AAAAA-AAAAA"})},
		{"junk recovery code", env.signInBody(p.email, adminPassword, map[string]any{"recoveryCode": "x"})},
	}

	var first []byte

	for i, tc := range tests {
		// a new address for each case, so the limits do not mix into this check
		res, data, _ := env.signIn(client{addr: "198.51.100." + strconv.Itoa(10+i)}, tc.body)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s answers %d, %s", tc.name, res.StatusCode, data)
		}

		code, _ := errorOf(t, data)
		if code != "invalid_credentials" {
			t.Fatalf("%s gives the code %s", tc.name, code)
		}

		// the same body for every wrong value, apart from the request id
		stripped := regexp.MustCompile(`"requestId":"[^"]*"`).ReplaceAll(data, nil)
		if i == 0 {
			first = stripped
		} else if string(stripped) != string(first) {
			t.Fatalf("%s answers %s and the first case answered %s", tc.name, stripped, first)
		}

		if len(res.Cookies()) != 0 {
			t.Fatalf("%s set a cookie", tc.name)
		}
	}
}

func TestAdminSignInRefusesAUsedRecoveryCode(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	body := env.signInBody(p.email, adminPassword, map[string]any{"recoveryCode": p.codes[4]})

	if res, data, _ := env.signIn(client{}, body); res.StatusCode != http.StatusCreated {
		t.Fatalf("the first use answers %d, %s", res.StatusCode, data)
	}

	res, data, _ := env.signIn(client{}, body)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the second use answers %d, %s", res.StatusCode, data)
	}

	// another code of the same admin still works
	if res, data, _ := env.signIn(client{}, env.signInBody(p.email, adminPassword, map[string]any{"recoveryCode": p.codes[5]})); res.StatusCode != http.StatusCreated {
		t.Fatalf("another code answers %d, %s", res.StatusCode, data)
	}
}

func TestAdminSignInRefusesAReusedTOTPCode(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	body := env.signInBody(p.email, adminPassword, map[string]any{"totpCode": env.code(p)})

	if res, data, _ := env.signIn(client{}, body); res.StatusCode != http.StatusCreated {
		t.Fatalf("the first use answers %d, %s", res.StatusCode, data)
	}

	if res, data, _ := env.signIn(client{}, body); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the second use answers %d, %s", res.StatusCode, data)
	}
}

func TestAdminSignInChecksTheBody(t *testing.T) {
	env := newAdminEnv(t)

	tests := []struct {
		name   string
		body   string
		status int
		field  string
		code   string
	}{
		{"not JSON", `{"email":`, 400, "", "bad_request"},
		{"an array", `[]`, 400, "", "bad_request"},
		{"a number for the email", `{"email":5,"password":"x"}`, 400, "", "bad_request"},
		{"no email", env.signInBody("", adminPassword, nil), 422, "email", "required"},
		{"blank email", env.signInBody("   ", adminPassword, nil), 422, "email", "required"},
		{"no at sign", env.signInBody("admin.example.org", adminPassword, nil), 422, "email", "invalid_format"},
		{"long email", env.signInBody(strings.Repeat("a", 250)+"@example.org", adminPassword, nil), 422, "email", "too_long"},
		{"no password", env.signInBody("admin@example.org", "", nil), 422, "password", "required"},
		{"long password", env.signInBody("admin@example.org", strings.Repeat("a", 129), nil), 422, "password", "too_long"},
		{"short code", env.signInBody("admin@example.org", adminPassword, map[string]any{"totpCode": "12345"}), 422, "totpCode", "invalid_format"},
		{"letters in code", env.signInBody("admin@example.org", adminPassword, map[string]any{"totpCode": "12a456"}), 422, "totpCode", "invalid_format"},
		{"long recovery code", env.signInBody("admin@example.org", adminPassword, map[string]any{"recoveryCode": strings.Repeat("A", 65)}), 422, "recoveryCode", "too_long"},
	}

	for _, tc := range tests {
		res, data := env.call(client{}, http.MethodPost, "/api/admin/session", tc.body)
		if res.StatusCode != tc.status {
			t.Fatalf("%s answers %d, %s", tc.name, res.StatusCode, data)
		}

		e := decodeError(t, data)
		if tc.status == 400 && e.Error.Code != tc.code {
			t.Fatalf("%s has the code %s", tc.name, e.Error.Code)
		}

		if tc.status == 422 && (e.Error.Code != "validation_failed" || len(e.Error.Details) != 1 || e.Error.Details[0].Field != tc.field || e.Error.Details[0].Code != tc.code) {
			t.Fatalf("%s has the body %s", tc.name, data)
		}
	}

	// a body that the check refused counted nothing
	if got := env.limiter.keys(); len(got) != 0 {
		t.Fatalf("the refused bodies made the counters %v", got)
	}
}

func TestAdminSignInOriginAndMethod(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	body := env.signInBody(p.email, adminPassword, map[string]any{"totpCode": env.code(p)})

	res, data := mustSend(t, env.app, http.MethodPost, "/api/admin/session", body, client{}.headers(map[string]string{"Origin": "https://evil.example.org"}))
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusForbidden || code != "origin_not_allowed" {
		t.Fatalf("another origin answers %d, %s", res.StatusCode, data)
	}

	// no CSRF token is needed, because the caller has no session yet
	if res, data := env.call(client{}, http.MethodPost, "/api/admin/session", body); res.StatusCode != http.StatusCreated {
		t.Fatalf("a call with no token answers %d, %s", res.StatusCode, data)
	}

	if res, _ := env.call(client{}, http.MethodGet, "/api/admin/session", ""); res.StatusCode != http.StatusMethodNotAllowed && res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET answers %d", res.StatusCode)
	}
}

func TestAdminSignInReplacesTheSessionThatCame(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")

	guestToken, _ := env.store.add(session.KindGuest, nil)
	if !env.store.has(guestToken) {
		t.Fatal("the guest session was not stored")
	}

	res, data, c := env.signIn(client{token: guestToken}, env.signInBody(p.email, adminPassword, map[string]any{"totpCode": env.code(p)}))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("the sign in answers %d, %s", res.StatusCode, data)
	}

	if env.store.has(guestToken) || !env.store.has(c.token) || env.store.count() != 1 {
		t.Fatalf("the sessions are not replaced, %d are stored", env.store.count())
	}
}

func TestOnlyARecoveryCodeSignInEndsTheOtherSessions(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")

	first := env.login(p)
	second := env.login(p)

	// a sign in with a TOTP code leaves the other sessions alone
	if !env.store.has(first.token) || !env.store.has(second.token) {
		t.Fatal("a TOTP sign in ended a session")
	}

	another := env.newFullAdmin("another@example.org")
	elsewhere := env.login(another)

	res, data, third := env.signIn(client{}, env.signInBody(p.email, adminPassword, map[string]any{"recoveryCode": p.codes[0]}))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("the sign in answers %d, %s", res.StatusCode, data)
	}

	if env.store.has(first.token) || env.store.has(second.token) {
		t.Fatal("the other sessions of the admin are still there")
	}

	if !env.store.has(third.token) || !env.store.has(elsewhere.token) {
		t.Fatal("the new session or the session of another admin is gone")
	}
}

func TestFailedAdminSignInsAreLimitedForOneEmailAndAddress(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	wrong := env.signInBody(p.email, otherPassword, nil)

	for i := 1; i <= 5; i++ {
		if res, data, _ := env.signIn(client{}, wrong); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d answers %d, %s", i, res.StatusCode, data)
		}
	}

	// the sixth request is refused, also with the right credentials, and it makes no sign in
	res, data, _ := env.signIn(client{}, env.signInBody(p.email, adminPassword, map[string]any{"totpCode": env.code(p)}))
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusTooManyRequests || code != "rate_limited" {
		t.Fatalf("the sixth request answers %d, %s", res.StatusCode, data)
	}

	if res.Header.Get("Retry-After") != "900" || env.store.count() != 0 {
		t.Fatalf("Retry-After is %q and %d sessions exist", res.Header.Get("Retry-After"), env.store.count())
	}

	// the blocked request did not count, so the pair holds the five failures
	if got := env.limiter.count(t, "admin-signin-pair:"); got != 5 {
		t.Fatalf("the pair counts %d", got)
	}

	// the same email from another address, and another email from this address, are not blocked
	if res, _, _ := env.signIn(client{addr: "198.51.100.99"}, wrong); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another address answers %d", res.StatusCode)
	}

	if res, _, _ := env.signIn(client{}, env.signInBody("nobody@example.org", otherPassword, nil)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another email answers %d", res.StatusCode)
	}
}

func TestASuccessDoesNotResetTheFailures(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	wrong := env.signInBody(p.email, otherPassword, nil)

	for range 4 {
		env.signIn(client{}, wrong)
	}

	env.login(p)

	if got := env.limiter.count(t, "admin-signin-pair:"); got != 4 {
		t.Fatalf("after four failures and a success the pair counts %d, want 4", got)
	}

	if res, _, _ := env.signIn(client{}, wrong); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the fifth failure answers %d", res.StatusCode)
	}

	if res, _, _ := env.signIn(client{}, wrong); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the next request answers %d, so the success reset the count", res.StatusCode)
	}
}

func TestFailedAdminSignInsAreLimitedForOneAddress(t *testing.T) {
	env := newAdminEnv(t, func(c *config.Config) { c.AdminSigninAddressFailures = 3 })

	for i := range 3 {
		body := env.signInBody("nobody"+strconv.Itoa(i)+"@example.org", otherPassword, nil)
		if res, data, _ := env.signIn(client{}, body); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d answers %d, %s", i, res.StatusCode, data)
		}
	}

	res, data, _ := env.signIn(client{}, env.signInBody("nobody9@example.org", otherPassword, nil))
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusTooManyRequests || code != "rate_limited" || res.Header.Get("Retry-After") != "3600" {
		t.Fatalf("the fourth email answers %d, %s", res.StatusCode, data)
	}

	if res, _, _ := env.signIn(client{addr: "198.51.100.200"}, env.signInBody("nobody9@example.org", otherPassword, nil)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another address answers %d", res.StatusCode)
	}
}

func TestTheCounterKeysHoldNoEmail(t *testing.T) {
	env := newAdminEnv(t)
	env.signIn(client{}, env.signInBody("Secret.Person@Example.org", otherPassword, nil))

	keys := env.limiter.keys()
	if len(keys) != 2 {
		t.Fatalf("the sign in made the counters %v", keys)
	}

	for _, key := range keys {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "secret") || strings.Contains(lower, "example") || strings.Contains(key, "@") {
			t.Fatalf("the key %q holds the email", key)
		}
	}
}

func TestAnUnknownEmailCostsAHashCheckToo(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")

	timeOf := func(body string, addr string) time.Duration {
		best := time.Hour

		for i := range 3 {
			start := time.Now()
			env.signIn(client{addr: addr + strconv.Itoa(i)}, body)

			best = min(best, time.Since(start))
		}

		return best
	}

	wrongPassword := timeOf(env.signInBody(p.email, otherPassword, nil), "198.51.101.")
	unknownEmail := timeOf(env.signInBody("nobody@example.org", otherPassword, nil), "198.51.102.")

	// the unknown email must not be the quick one, which would show that it is no account
	if unknownEmail*2 < wrongPassword {
		t.Fatalf("an unknown email takes %v and a wrong password takes %v", unknownEmail, wrongPassword)
	}
}

// failingAdmins is an AdminService that fails every call with one error.
type failingAdmins struct{ err error }

func (f failingAdmins) SignIn(context.Context, admin.SignInInput) (admin.SignInResult, error) {
	return admin.SignInResult{}, f.err
}

func (f failingAdmins) Profile(context.Context, string) (admin.Profile, error) {
	return admin.Profile{}, f.err
}

func (f failingAdmins) ChangePassword(context.Context, string, admin.ChangePasswordInput, []byte) error {
	return f.err
}

func (f failingAdmins) StartTOTP(context.Context, string, []byte, admin.EnrollInput) (admin.Enrollment, error) {
	return admin.Enrollment{}, f.err
}

func (f failingAdmins) FinishTOTP(context.Context, string, []byte, admin.EnrollInput) ([]string, error) {
	return nil, f.err
}

func (f failingAdmins) RenewRecoveryCodes(context.Context, string, admin.RenewInput) ([]string, error) {
	return nil, f.err
}

func TestABusyHasherAnswers503AndCountsNothing(t *testing.T) {
	limiter := newCountingLimiter()
	store := newFakeStore()
	app := NewAPI(adminConfig(), Deps{Sessions: store, Limiter: limiter, Admins: failingAdmins{err: password.ErrBusy}})

	res, data := mustSend(t, app, http.MethodPost, "/api/admin/session", `{"email":"admin@example.org","password":"example admin password"}`, client{}.headers(nil))
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusServiceUnavailable || code != "unavailable" || res.Header.Get("Retry-After") != "2" {
		t.Fatalf("the sign in answers %d, %s", res.StatusCode, data)
	}

	if strings.Contains(string(data), "database") {
		t.Fatalf("the answer blames the database, %s", data)
	}

	for _, key := range limiter.keys() {
		if limiter.counts[key] != 0 {
			t.Fatalf("the counter %q holds %d after a busy hasher", key, limiter.counts[key])
		}
	}

	// the account routes refund in the same way
	token, sess := store.add(session.KindAdmin, nil)
	headers := client{token: token, csrf: sess.CSRFToken}.headers(nil)

	res, data = mustSend(t, app, http.MethodPost, "/api/admin/me/recovery-codes", `{"currentPassword":"example admin password","totpCode":"123456"}`, headers)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the account route answers %d, %s", res.StatusCode, data)
	}

	if got := limiter.counts[accountKey(&sess)]; got != 0 {
		t.Fatalf("the session counter holds %d", got)
	}
}

func TestADatabaseFailureOfTheAdminServiceAnswers500Or503(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
	}{
		"a timeout":     {context.DeadlineExceeded, http.StatusServiceUnavailable},
		"an outage":     {&outageError{}, http.StatusServiceUnavailable},
		"a plain error": {errTest, http.StatusInternalServerError},
	} {
		app := NewAPI(adminConfig(), Deps{Sessions: newFakeStore(), Limiter: newCountingLimiter(), Admins: failingAdmins{err: tc.err}})

		res, data := mustSend(t, app, http.MethodPost, "/api/admin/session", `{"email":"admin@example.org","password":"example admin password"}`, client{}.headers(nil))
		if res.StatusCode != tc.status {
			t.Fatalf("%s answers %d, %s", name, res.StatusCode, data)
		}

		if strings.Contains(string(data), "test failure") {
			t.Fatalf("%s shows the error text, %s", name, data)
		}
	}
}

type testFailureError struct{}

func (testFailureError) Error() string { return "test failure" }

var errTest error = testFailureError{}

func TestGetAdminMe(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	c := env.login(p)

	res, data := env.call(c, http.MethodGet, "/api/admin/me", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status is %d, %s", res.StatusCode, data)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("the body is not JSON %v", err)
	}

	want := map[string]any{"id": p.id, "name": "Example Admin", "email": "admin@example.org", "totpEnabled": true, "passwordExpiresAt": nil, "recoveryCodesLeft": float64(10)}
	if len(got) != len(want) {
		t.Fatalf("the body is %s", data)
	}

	for key, value := range want {
		if actual, present := got[key]; !present || actual != value {
			t.Fatalf("%s is %v, want %v, in %s", key, actual, value, data)
		}
	}

	// an admin with an initial password shows the moment that it expires
	fresh := env.newAdmin("fresh@example.org")
	account, _ := env.repo.ByID(context.Background(), fresh.id)
	account.TOTPEnabledAt = nil
	env.repo.Put(account)

	token, _ := env.store.add(session.KindAdmin, nil)
	env.store.mu.Lock()
	stored := env.store.sessions[string(session.HashToken(token))]
	stored.Admin = &session.Admin{ID: fresh.id, Name: "Example Admin", Email: fresh.email}
	env.store.sessions[string(session.HashToken(token))] = stored
	env.store.mu.Unlock()

	res, data = env.call(client{token: token}, http.MethodGet, "/api/admin/me", "")
	if want := `"passwordExpiresAt":"` + env.clock.Now().Add(24*time.Hour).Format(time.RFC3339) + `"`; res.StatusCode != http.StatusOK || !strings.Contains(string(data), want) || !strings.Contains(string(data), `"totpEnabled":false`) {
		t.Fatalf("the fresh admin gives %d, %s", res.StatusCode, data)
	}
}

func TestAdminAccountRoutesNeedTheRightSession(t *testing.T) {
	env := newAdminEnv(t)
	setup := env.newAdmin("setup@example.org")
	_, _, setupClient := env.signIn(client{}, env.signInBody(setup.email, adminPassword, nil))

	guestToken, guest := env.store.add(session.KindGuest, nil)
	guestClient := client{token: guestToken, csrf: guest.CSRFToken}

	passwordBody := jsonOf(t, map[string]any{"currentPassword": adminPassword, "newPassword": "example brand new password"})
	enrollBody := jsonOf(t, map[string]any{"currentPassword": adminPassword})
	renewBody := jsonOf(t, map[string]any{"currentPassword": adminPassword, "totpCode": "123456"})

	tests := []struct {
		name   string
		c      client
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{"me without a session", client{}, http.MethodGet, "/api/admin/me", "", 401, "unauthenticated"},
		{"me with a guest", guestClient, http.MethodGet, "/api/admin/me", "", 403, "forbidden"},
		{"me with a setup session", setupClient, http.MethodGet, "/api/admin/me", "", 403, "forbidden"},
		{"password without a session", client{}, http.MethodPost, "/api/admin/me/password", passwordBody, 401, "unauthenticated"},
		{"password with a guest", guestClient, http.MethodPost, "/api/admin/me/password", passwordBody, 403, "forbidden"},
		{"totp with a guest", guestClient, http.MethodPost, "/api/admin/me/totp", enrollBody, 403, "forbidden"},
		{"totp without a session", client{}, http.MethodPost, "/api/admin/me/totp", enrollBody, 401, "unauthenticated"},
		{"codes with a guest", guestClient, http.MethodPost, "/api/admin/me/recovery-codes", renewBody, 403, "forbidden"},
		{"codes with a setup session", setupClient, http.MethodPost, "/api/admin/me/recovery-codes", renewBody, 403, "forbidden"},
		{"codes without a session", client{}, http.MethodPost, "/api/admin/me/recovery-codes", renewBody, 401, "unauthenticated"},
		{"a session of the other site", client{token: "junk"}, http.MethodGet, "/api/admin/me", "", 401, "session_expired"},
	}

	for _, tc := range tests {
		res, data := env.call(tc.c, tc.method, tc.path, tc.body)
		if res.StatusCode != tc.status {
			t.Fatalf("%s answers %d, %s", tc.name, res.StatusCode, data)
		}

		if code, _ := errorOf(t, data); code != tc.code {
			t.Fatalf("%s has the code %s", tc.name, code)
		}
	}

	// the setup session may call the two setup routes and the session routes
	for _, path := range []string{"/api/admin/me/password", "/api/admin/me/totp"} {
		res, data := env.call(setupClient, http.MethodPost, path, `{"currentPassword":"`+otherPassword+`"}`)
		if res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusUnauthorized && !strings.Contains(string(data), "invalid_credentials") {
			t.Fatalf("a setup session was refused at %s, %d %s", path, res.StatusCode, data)
		}
	}

	if res, _ := env.call(setupClient, http.MethodGet, "/api/session", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("a setup session cannot read its session, %d", res.StatusCode)
	}
}

func TestAdminAccountRoutesNeedTheCSRFTokenAndTheOrigin(t *testing.T) {
	env := newAdminEnv(t)
	c := env.login(env.newFullAdmin("admin@example.org"))
	body := jsonOf(t, map[string]any{"currentPassword": otherPassword, "totpCode": "123456"})

	tests := []struct {
		name    string
		headers map[string]string
		code    string
	}{
		{"no token", c.headers(map[string]string{"X-CSRF-Token": ""}), "csrf_failed"},
		{"wrong token", c.headers(map[string]string{"X-CSRF-Token": c.csrf + "x"}), "csrf_failed"},
		{"another origin", c.headers(map[string]string{"Origin": "https://evil.example.org"}), "origin_not_allowed"},
		{"no origin", c.headers(map[string]string{"Origin": ""}), "origin_not_allowed"},
	}

	for _, path := range []string{"/api/admin/me/password", "/api/admin/me/totp", "/api/admin/me/recovery-codes"} {
		for _, tc := range tests {
			headers := tc.headers
			if headers["Origin"] == "" {
				delete(headers, "Origin")
			}

			if headers["X-CSRF-Token"] == "" {
				delete(headers, "X-CSRF-Token")
			}

			res, data := mustSend(t, env.app, http.MethodPost, path, body, headers)
			if code, _ := errorOf(t, data); res.StatusCode != http.StatusForbidden || code != tc.code {
				t.Fatalf("%s at %s answers %d, %s", tc.name, path, res.StatusCode, data)
			}
		}
	}

	// the refused requests reached no check, so nothing was counted
	for _, key := range env.limiter.keys() {
		if strings.HasPrefix(key, "admin-account:") {
			t.Fatalf("a refused request made the counter %q", key)
		}
	}
}

func TestChangeAdminPassword(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	current := env.login(p)
	other := env.login(p)

	body := func(extra map[string]any) string {
		m := map[string]any{"currentPassword": adminPassword, "newPassword": "example brand new password", "totpCode": env.code(p)}
		for k, v := range extra {
			m[k] = v
		}

		return jsonOf(t, m)
	}

	bad := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"wrong current password", body(map[string]any{"currentPassword": otherPassword}), 401, "invalid_credentials"},
		{"no TOTP code", body(map[string]any{"totpCode": nil}), 401, "invalid_credentials"},
		{"wrong TOTP code", body(map[string]any{"totpCode": "000000"}), 401, "invalid_credentials"},
		{"short new password", body(map[string]any{"newPassword": "too short"}), 422, "validation_failed"},
		{"long new password", body(map[string]any{"newPassword": strings.Repeat("a", 129)}), 422, "validation_failed"},
		{"the same password", body(map[string]any{"newPassword": adminPassword}), 422, "validation_failed"},
		{"no new password", body(map[string]any{"newPassword": ""}), 422, "validation_failed"},
		{"bad code shape", body(map[string]any{"totpCode": "12"}), 422, "validation_failed"},
		{"not JSON", `{`, 400, "bad_request"},
	}

	for _, tc := range bad {
		res, data := env.call(current, http.MethodPost, "/api/admin/me/password", tc.body)
		if code, _ := errorOf(t, data); res.StatusCode != tc.status || code != tc.code {
			t.Fatalf("%s answers %d, %s", tc.name, res.StatusCode, data)
		}
	}

	// a failed change leaves the other session in place
	if !env.store.has(other.token) {
		t.Fatal("a failed change ended a session")
	}

	res, data := env.call(current, http.MethodPost, "/api/admin/me/password", body(nil))
	if res.StatusCode != http.StatusNoContent || len(data) != 0 {
		t.Fatalf("the change answers %d, %s", res.StatusCode, data)
	}

	// the change ends the other sessions and keeps the one that asked
	if env.store.has(other.token) || !env.store.has(current.token) {
		t.Fatal("the sessions after the change are wrong")
	}

	// the old password is gone, and the new one signs in
	if res, _, _ := env.signIn(client{}, env.signInBody(p.email, adminPassword, map[string]any{"totpCode": env.code(p)})); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old password answers %d", res.StatusCode)
	}

	if res, data, _ := env.signIn(client{}, env.signInBody(p.email, "example brand new password", map[string]any{"totpCode": env.code(p)})); res.StatusCode != http.StatusCreated {
		t.Fatalf("the new password answers %d, %s", res.StatusCode, data)
	}
}

func TestASetupSessionSetsPasswordAndTOTP(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newAdmin("admin@example.org")

	res, data, setup := env.signIn(client{}, env.signInBody(p.email, adminPassword, nil))
	if res.StatusCode != http.StatusCreated || !strings.Contains(string(data), `"admin_setup"`) {
		t.Fatalf("the sign in answers %d, %s", res.StatusCode, data)
	}

	// step 1, a new password, with no code because a setup session has none yet
	res, data = env.call(setup, http.MethodPost, "/api/admin/me/password", jsonOf(t, map[string]any{"currentPassword": adminPassword, "newPassword": "example brand new password"}))
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("the password change answers %d, %s", res.StatusCode, data)
	}

	if account, _ := env.repo.ByID(context.Background(), p.id); account.PasswordExpiresAt != nil {
		t.Fatal("the initial password still expires")
	}

	// step 2, the secret
	res, data = env.call(setup, http.MethodPost, "/api/admin/me/totp", jsonOf(t, map[string]any{"currentPassword": "example brand new password"}))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the start answers %d, %s", res.StatusCode, data)
	}

	var started struct{ Secret, OtpauthURI string }
	if err := json.Unmarshal(data, &started); err != nil || !strings.HasPrefix(started.OtpauthURI, "otpauth://totp/LabMGM:admin@example.org?") || len(started.Secret) != 32 {
		t.Fatalf("the start gave %s and %v", data, err)
	}

	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(started.Secret)
	if err != nil {
		t.Fatalf("the secret is not Base32 %v", err)
	}

	// a wrong confirm code fails and counts as a failure
	res, data = env.call(setup, http.MethodPost, "/api/admin/me/totp", jsonOf(t, map[string]any{"currentPassword": "example brand new password", "confirmCode": "000000"}))
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusUnauthorized || code != "invalid_credentials" {
		t.Fatalf("a wrong confirm code answers %d, %s", res.StatusCode, data)
	}

	// step 3, the confirm code gives the first recovery codes
	confirm := totp.Code(secret, totp.Step(env.clock.Now()))

	res, data = env.call(setup, http.MethodPost, "/api/admin/me/totp", jsonOf(t, map[string]any{"currentPassword": "example brand new password", "confirmCode": confirm}))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the finish answers %d, %s", res.StatusCode, data)
	}

	var finished struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}

	if err := json.Unmarshal(data, &finished); err != nil || len(finished.RecoveryCodes) != 10 {
		t.Fatalf("the finish gave %s and %v", data, err)
	}

	shape := regexp.MustCompile(`^[A-HJ-NP-Z2-9]{5}-[A-HJ-NP-Z2-9]{5}$`)
	for _, code := range finished.RecoveryCodes {
		if !shape.MatchString(code) {
			t.Fatalf("the code %q has a bad shape", code)
		}
	}

	// the next sign in is a full session, with a code or with a recovery code
	env.clock.advance(totp.StepSeconds * time.Second)

	res, data, _ = env.signIn(client{}, env.signInBody(p.email, "example brand new password", map[string]any{"totpCode": totp.Code(secret, totp.Step(env.clock.Now()))}))
	if res.StatusCode != http.StatusCreated || !strings.Contains(string(data), `"kind":"admin"`) {
		t.Fatalf("the full sign in answers %d, %s", res.StatusCode, data)
	}

	res, data, _ = env.signIn(client{}, env.signInBody(p.email, "example brand new password", map[string]any{"recoveryCode": finished.RecoveryCodes[0]}))
	if res.StatusCode != http.StatusCreated || !strings.Contains(string(data), `"kind":"admin"`) {
		t.Fatalf("the recovery code sign in answers %d, %s", res.StatusCode, data)
	}
}

func TestAnAdminSessionEnrollsTOTPAgain(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	c := env.login(p)

	// an admin who has TOTP sends a code in both calls
	res, data := env.call(c, http.MethodPost, "/api/admin/me/totp", jsonOf(t, map[string]any{"currentPassword": adminPassword}))
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusUnauthorized || code != "invalid_credentials" {
		t.Fatalf("a start with no code answers %d, %s", res.StatusCode, data)
	}

	res, data = env.call(c, http.MethodPost, "/api/admin/me/totp", jsonOf(t, map[string]any{"currentPassword": adminPassword, "totpCode": env.code(p)}))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the start answers %d, %s", res.StatusCode, data)
	}

	var started struct{ Secret string }
	if err := json.Unmarshal(data, &started); err != nil {
		t.Fatalf("bad JSON %v", err)
	}

	secret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(started.Secret)
	if string(secret) == string(p.secret) {
		t.Fatal("the new secret equals the old one")
	}

	env.clock.advance(totp.StepSeconds * time.Second)

	res, data = env.call(c, http.MethodPost, "/api/admin/me/totp", jsonOf(t, map[string]any{
		"currentPassword": adminPassword, "totpCode": totp.Code(p.secret, totp.Step(env.clock.Now())), "confirmCode": totp.Code(secret, totp.Step(env.clock.Now())),
	}))
	if res.StatusCode != http.StatusOK || !strings.Contains(string(data), `"recoveryCodes"`) {
		t.Fatalf("the finish answers %d, %s", res.StatusCode, data)
	}

	p.secret = secret

	if res, data, _ := env.signIn(client{}, env.signInBody(p.email, adminPassword, map[string]any{"totpCode": env.code(p)})); res.StatusCode != http.StatusCreated {
		t.Fatalf("a code of the new secret answers %d, %s", res.StatusCode, data)
	}
}

func TestRenewAdminRecoveryCodes(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	c := env.login(p)

	bad := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"no code", jsonOf(t, map[string]any{"currentPassword": adminPassword}), 422, "validation_failed"},
		{"no password", jsonOf(t, map[string]any{"totpCode": "123456"}), 422, "validation_failed"},
		{"wrong password", jsonOf(t, map[string]any{"currentPassword": otherPassword, "totpCode": env.code(p)}), 401, "invalid_credentials"},
		{"wrong code", jsonOf(t, map[string]any{"currentPassword": adminPassword, "totpCode": "000000"}), 401, "invalid_credentials"},
	}

	for _, tc := range bad {
		res, data := env.call(c, http.MethodPost, "/api/admin/me/recovery-codes", tc.body)
		if code, _ := errorOf(t, data); res.StatusCode != tc.status || code != tc.code {
			t.Fatalf("%s answers %d, %s", tc.name, res.StatusCode, data)
		}
	}

	res, data := env.call(c, http.MethodPost, "/api/admin/me/recovery-codes", jsonOf(t, map[string]any{"currentPassword": adminPassword, "totpCode": env.code(p)}))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the renewal answers %d, %s", res.StatusCode, data)
	}

	var fresh struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}

	if err := json.Unmarshal(data, &fresh); err != nil || len(fresh.RecoveryCodes) != 10 {
		t.Fatalf("the renewal gave %s and %v", data, err)
	}

	// the old codes are gone and the new ones work
	if res, _, _ := env.signIn(client{}, env.signInBody(p.email, adminPassword, map[string]any{"recoveryCode": p.codes[0]})); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an old code answers %d", res.StatusCode)
	}

	if res, _, _ := env.signIn(client{}, env.signInBody(p.email, adminPassword, map[string]any{"recoveryCode": fresh.RecoveryCodes[0]})); res.StatusCode != http.StatusCreated {
		t.Fatalf("a new code answers %d", res.StatusCode)
	}
}

func TestFailedAccountChecksAreLimitedForOneSession(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	c := env.login(p)
	other := env.login(p)

	wrong := jsonOf(t, map[string]any{"currentPassword": otherPassword, "totpCode": "000000"})

	// a request that the body check refused counts nothing
	for range 6 {
		env.call(c, http.MethodPost, "/api/admin/me/recovery-codes", `{"currentPassword":"x"}`)
	}

	for i := 1; i <= 5; i++ {
		res, data := env.call(c, http.MethodPost, "/api/admin/me/recovery-codes", wrong)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d answers %d, %s", i, res.StatusCode, data)
		}
	}

	good := jsonOf(t, map[string]any{"currentPassword": adminPassword, "totpCode": env.code(p)})

	res, data := env.call(c, http.MethodPost, "/api/admin/me/recovery-codes", good)
	if code, _ := errorOf(t, data); res.StatusCode != http.StatusTooManyRequests || code != "rate_limited" || res.Header.Get("Retry-After") != "900" {
		t.Fatalf("the sixth request answers %d, %s", res.StatusCode, data)
	}

	// the limit holds for the other routes of the same session, and not for another session of the same admin
	if res, _ := env.call(c, http.MethodPost, "/api/admin/me/totp", jsonOf(t, map[string]any{"currentPassword": adminPassword})); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the totp route answers %d", res.StatusCode)
	}

	if res, data := env.call(other, http.MethodPost, "/api/admin/me/recovery-codes", jsonOf(t, map[string]any{"currentPassword": adminPassword, "totpCode": env.code(p)})); res.StatusCode != http.StatusOK {
		t.Fatalf("the other session answers %d, %s", res.StatusCode, data)
	}

	// a session that is limited can still read its profile, because the counter guards the checks only
	if res, _ := env.call(c, http.MethodGet, "/api/admin/me", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("the profile answers %d", res.StatusCode)
	}
}

func TestASuccessfulAccountCheckIsNotCounted(t *testing.T) {
	env := newAdminEnv(t)
	p := env.newFullAdmin("admin@example.org")
	c := env.login(p)

	for range 7 {
		res, data := env.call(c, http.MethodPost, "/api/admin/me/recovery-codes", jsonOf(t, map[string]any{"currentPassword": adminPassword, "totpCode": env.code(p)}))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("a good request answers %d, %s", res.StatusCode, data)
		}
	}

	if got := env.limiter.count(t, "admin-account:"); got != 0 {
		t.Fatalf("seven successes left the count at %d", got)
	}

	for key := range env.limiter.counts {
		if strings.HasPrefix(key, "admin-account:") && strings.Contains(key, c.token) {
			t.Fatalf("the key %q holds the session token", key)
		}
	}
}
