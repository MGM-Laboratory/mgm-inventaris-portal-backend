package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
)

func testConfig() config.Config {
	return config.Config{
		PublicBaseURL:      "https://portal.example.org",
		MaxItemsPerRequest: 10,
		MaxDurationDays:    30,
		MaxOpenTickets:     3,
		SecureCookie:       true,
		GuestSigninPerHour: 60,
	}
}

func do(t *testing.T, app *fiber.App, method, path string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()

	req := httptest.NewRequest(method, path, http.NoBody)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed %v", err)
	}

	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read the body %v", err)
	}

	return res, body
}

func TestConfigRoute(t *testing.T) {
	res, body := do(t, newTestAPI(), http.MethodGet, "/api/config", nil)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status is %d, want 200", res.StatusCode)
	}

	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type is %q", ct)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the body is not JSON %v", err)
	}

	keys := func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}

		slices.Sort(out)

		return out
	}

	if !slices.Equal(keys(got), []string{"limits", "publicBaseUrl"}) {
		t.Fatalf("the keys are %v", keys(got))
	}

	if got["publicBaseUrl"] != "https://portal.example.org" {
		t.Fatalf("publicBaseUrl is %v", got["publicBaseUrl"])
	}

	limitsMap, ok := got["limits"].(map[string]any)
	if !ok {
		t.Fatalf("limits is %v", got["limits"])
	}

	want := map[string]any{"maxItemsPerRequest": 10.0, "minDurationDays": 1.0, "maxDurationDays": 30.0, "maxOpenTickets": 3.0}
	if !slices.Equal(keys(limitsMap), keys(want)) {
		t.Fatalf("the limit keys are %v", keys(limitsMap))
	}

	for k, v := range want {
		if limitsMap[k] != v {
			t.Fatalf("%s is %v, want %v", k, limitsMap[k], v)
		}
	}
}

func TestHeadersOnEveryAnswer(t *testing.T) {
	app := newTestAPI()

	for _, path := range []string{"/api/config", "/api/nothing"} {
		res, _ := do(t, app, http.MethodGet, path, nil)

		if res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s has Cache-Control %q", path, res.Header.Get("Cache-Control"))
		}

		if _, err := uuid.Parse(res.Header.Get("X-Request-Id")); err != nil {
			t.Fatalf("%s has request id %q", path, res.Header.Get("X-Request-Id"))
		}
	}
}

func TestRequestID(t *testing.T) {
	app := newTestAPI()

	res, _ := do(t, app, http.MethodGet, "/api/config", map[string]string{"X-Request-Id": "client-id.1_ok"})
	if got := res.Header.Get("X-Request-Id"); got != "client-id.1_ok" {
		t.Fatalf("a safe client id was replaced by %q", got)
	}

	for _, unsafe := range []string{"has space", "semi;colon", "slash/id", strings.Repeat("a", 129)} {
		res, _ := do(t, app, http.MethodGet, "/api/config", map[string]string{"X-Request-Id": unsafe})
		if got := res.Header.Get("X-Request-Id"); got == unsafe {
			t.Fatalf("an unsafe id was kept, %q", got)
		}
	}
}

type envelope struct {
	Error struct {
		Code      string   `json:"code"`
		Message   string   `json:"message"`
		Details   []Detail `json:"details"`
		RequestID string   `json:"requestId"`
	} `json:"error"`
}

func decodeError(t *testing.T, body []byte) envelope {
	t.Helper()

	var e envelope
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("the error body is not JSON %v, %s", err, body)
	}

	return e
}

func TestEveryCodeHasItsDocumentedStatus(t *testing.T) {
	// the table of docs/errors.md
	want := map[Code]int{
		CodeBadRequest: 400, CodeUnauthenticated: 401, CodeSessionExpired: 401, CodeInvalidCredentials: 401,
		CodeForbidden: 403, CodeIdentityRequired: 403, CodeCSRFFailed: 403, CodeOriginNotAllowed: 403,
		CodeNotFound: 404, CodeTicketNotFound: 404,
		CodeStockChanged: 409, CodeVersionConflict: 409, CodeStatusConflict: 409, CodeUnitHeld: 409, CodeUnitUnavailable: 409,
		CodeDuplicate: 409, CodeRequestInProgress: 409,
		CodeValidationFailed: 422, CodeRateLimited: 429, CodeInternal: 500, CodeUnavailable: 503,
	}

	if len(catalog) != len(want) {
		t.Fatalf("the catalog has %d codes and the documented table has %d", len(catalog), len(want))
	}

	app := newTestAPI()

	// every route is added before the first request
	for code := range want {
		app.Get("/test/"+string(code), func(fiber.Ctx) error { return NewError(code) })
	}

	for code, status := range want {
		res, body := do(t, app, http.MethodGet, "/test/"+string(code), nil)
		if res.StatusCode != status {
			t.Fatalf("%s answers %d, want %d", code, res.StatusCode, status)
		}

		e := decodeError(t, body)
		if e.Error.Code != string(code) || e.Error.Message == "" || e.Error.RequestID != res.Header.Get("X-Request-Id") {
			t.Fatalf("%s has the body %s", code, body)
		}
	}
}

func TestErrorDetailsAndRetryAfter(t *testing.T) {
	app := newTestAPI()

	app.Get("/test/details", func(fiber.Ctx) error {
		return NewError(CodeDuplicate, Detail{Field: "serial", Code: "duplicate", Message: "Used.", ExistingTag: "MGM-HDS-001"})
	})
	app.Get("/test/limited", func(fiber.Ctx) error {
		e := NewError(CodeRateLimited)
		e.RetryAfter = 900

		return e
	})

	_, body := do(t, app, http.MethodGet, "/test/details", nil)

	e := decodeError(t, body)
	if len(e.Error.Details) != 1 || e.Error.Details[0].ExistingTag != "MGM-HDS-001" || e.Error.Details[0].Field != "serial" {
		t.Fatalf("details are %+v", e.Error.Details)
	}

	res, body := do(t, app, http.MethodGet, "/test/limited", nil)
	if res.Header.Get("Retry-After") != "900" {
		t.Fatalf("Retry-After is %q", res.Header.Get("Retry-After"))
	}

	var raw map[string]map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("not JSON %v", err)
	}

	if _, has := raw["error"]["details"]; has {
		t.Fatal("an error without details carries a details field")
	}
}

func TestUnexpectedErrorsHideTheirText(t *testing.T) {
	app := newTestAPI()

	app.Get("/test/plain", func(fiber.Ctx) error {
		return errors.New("password=hunter2 failed for row 17")
	})
	app.Get("/test/panic", func(fiber.Ctx) error {
		panic("secret panic value")
	})

	for _, path := range []string{"/test/plain", "/test/panic"} {
		res, body := do(t, app, http.MethodGet, path, nil)
		if res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("%s answers %d", path, res.StatusCode)
		}

		e := decodeError(t, body)
		if e.Error.Code != "internal" || e.Error.RequestID == "" {
			t.Fatalf("%s has the body %s", path, body)
		}

		for _, leak := range []string{"hunter2", "secret panic value", "goroutine", ".go:"} {
			if strings.Contains(string(body), leak) {
				t.Fatalf("%s leaks %q in %s", path, leak, body)
			}
		}

		if res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s lost its Cache-Control header", path)
		}
	}
}

func TestFiberErrors(t *testing.T) {
	app := newTestAPI()

	app.Get("/test/teapot", func(fiber.Ctx) error { return fiber.NewError(fiber.StatusRequestEntityTooLarge, "too big") })
	app.Get("/test/bad-gateway", func(fiber.Ctx) error { return fiber.NewError(fiber.StatusBadGateway, "upstream said no") })

	tests := []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, "/api/nothing", http.StatusNotFound, "not_found"},
		{http.MethodGet, "/", http.StatusNotFound, "not_found"},
		{http.MethodPost, "/api/config", http.StatusMethodNotAllowed, "bad_request"},
		{http.MethodGet, "/test/teapot", http.StatusRequestEntityTooLarge, "bad_request"},
		{http.MethodGet, "/test/bad-gateway", http.StatusInternalServerError, "internal"},
	}

	for _, tc := range tests {
		res, body := do(t, app, tc.method, tc.path, sameOriginHeaders())
		if res.StatusCode != tc.status {
			t.Fatalf("%s %s answers %d, want %d", tc.method, tc.path, res.StatusCode, tc.status)
		}

		if e := decodeError(t, body); e.Error.Code != tc.code || strings.Contains(e.Error.Message, "upstream") || strings.Contains(e.Error.Message, "too big") {
			t.Fatalf("%s %s has the body %s", tc.method, tc.path, body)
		}
	}
}

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestHealthRoutes(t *testing.T) {
	tests := []struct {
		name   string
		pinger fakePinger
		path   string
		status int
		body   string
	}{
		{"alive without a database", fakePinger{err: errors.New("down")}, "/healthz", 200, `{"status":"ok"}`},
		{"ready", fakePinger{}, "/readyz", 200, `{"status":"ready"}`},
		{"not ready", fakePinger{err: errors.New("connection refused")}, "/readyz", 503, `{"status":"unavailable"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, body := do(t, NewHealth(tc.pinger, nil).App(), http.MethodGet, tc.path, nil)

			if res.StatusCode != tc.status || string(body) != tc.body {
				t.Fatalf("answer is %d %s, want %d %s", res.StatusCode, body, tc.status, tc.body)
			}

			if res.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control is %q", res.Header.Get("Cache-Control"))
			}
		})
	}
}

func TestHealthAppHasNoAPIRoute(t *testing.T) {
	res, body := do(t, NewHealth(fakePinger{}, nil).App(), http.MethodGet, "/api/config", nil)
	if res.StatusCode != http.StatusNotFound || decodeError(t, body).Error.Code != "not_found" {
		t.Fatalf("the health app answered %d %s", res.StatusCode, body)
	}
}

// countingPinger counts the pings and answers with the error that the test sets.
type countingPinger struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (p *countingPinger) Ping(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++

	return p.err
}

func (p *countingPinger) set(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.err = err
}

func (p *countingPinger) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func readyz(t *testing.T, h *Health) (int, string) {
	t.Helper()

	res, body := do(t, h.App(), http.MethodGet, "/readyz", nil)

	return res.StatusCode, string(body)
}

func TestReadyzReusesTheAnswerForAShortTime(t *testing.T) {
	pinger := &countingPinger{}
	clock := &fakeClock{now: time.Unix(1000, 0)}
	h := newHealth(pinger, nil, 2*time.Second, clock.Now)

	for range 5 {
		if code, body := readyz(t, h); code != 200 || body != `{"status":"ready"}` {
			t.Fatalf("answer is %d %s", code, body)
		}
	}

	if pinger.count() != 1 {
		t.Fatalf("five probes made %d pings, want 1", pinger.count())
	}

	// the database fails, but the cached answer still holds until the time is up
	pinger.set(errors.New("connection refused"))

	if code, _ := readyz(t, h); code != 200 || pinger.count() != 1 {
		t.Fatalf("a probe inside the time made a ping, calls %d, status %d", pinger.count(), code)
	}

	clock.now = clock.now.Add(3 * time.Second)

	if code, body := readyz(t, h); code != 503 || body != `{"status":"unavailable"}` {
		t.Fatalf("answer after the time is %d %s", code, body)
	}

	if pinger.count() != 2 {
		t.Fatalf("calls are %d, want 2", pinger.count())
	}
}

func TestReadyzSaysReadyWhileThePoolIsSaturated(t *testing.T) {
	pinger := &countingPinger{err: context.DeadlineExceeded}
	clock := &fakeClock{now: time.Unix(1000, 0)}

	saturated := PoolStats{Acquired: 8, Max: 8}
	stats := func() PoolStats { return saturated }
	h := newHealth(pinger, stats, time.Second, clock.Now)

	if code, body := readyz(t, h); code != 200 || body != `{"status":"ready"}` {
		t.Fatalf("a busy pool answered %d %s", code, body)
	}

	// the same failure with free connections is a database that is down
	saturated = PoolStats{Acquired: 3, Max: 8}
	clock.now = clock.now.Add(2 * time.Second)

	if code, body := readyz(t, h); code != 503 || body != `{"status":"unavailable"}` {
		t.Fatalf("a down database answered %d %s", code, body)
	}
}

func TestReadyzKeepsTheEvidenceOfASaturatedPoolFromBeforeTheWait(t *testing.T) {
	pinger := &countingPinger{err: context.DeadlineExceeded}
	clock := &fakeClock{now: time.Unix(1000, 0)}

	reads := 0
	stats := func() PoolStats {
		reads++

		// the pool is full when the ping starts and free when it has failed
		if reads == 1 {
			return PoolStats{Acquired: 8, Max: 8}
		}

		return PoolStats{Acquired: 0, Max: 8}
	}
	h := newHealth(pinger, stats, time.Second, clock.Now)

	if code, body := readyz(t, h); code != http.StatusOK || body != `{"status":"ready"}` {
		t.Fatalf("a pool that was full before the failed ping answered %d %s", code, body)
	}
}

func TestReadyzAnswers503WhileDraining(t *testing.T) {
	h := newHealth(&countingPinger{}, nil, time.Second, time.Now)

	if code, _ := readyz(t, h); code != 200 {
		t.Fatalf("status before the drain is %d", code)
	}

	h.Drain()

	if code, body := readyz(t, h); code != 503 || body != `{"status":"unavailable"}` {
		t.Fatalf("answer while draining is %d %s", code, body)
	}

	if res, _ := do(t, h.App(), http.MethodGet, "/healthz", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("healthz while draining is %d", res.StatusCode)
	}
}

// captureLog sends the default logger to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	return captureLogAt(t, slog.LevelInfo)
}

func captureLogAt(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &buf
}

func TestPanicIsLoggedWithItsStackAndNotAnswered(t *testing.T) {
	logs := captureLog(t)
	app := newTestAPI()

	app.Get("/test/panic", func(fiber.Ctx) error {
		panic("boom value")
	})

	res, body := do(t, app, http.MethodGet, "/test/panic", map[string]string{
		"Cookie": "__Host-mgm_session=sample-session-value", "Authorization": "Bearer sample-token-value",
	})
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status is %d", res.StatusCode)
	}

	for _, stack := range []string{"goroutine", "runtime/debug", ".go:", "boom value", "panic"} {
		if strings.Contains(string(body), stack) {
			t.Fatalf("the answer holds %q, %s", stack, body)
		}
	}

	logged := logs.String()

	for _, want := range []string{`"panic_value":"boom value"`, `"panic_type":"string"`, "goroutine", "server_test.go"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the log has no %q, %s", want, logged)
		}
	}

	for _, leak := range []string{"sample-session-value", "sample-token-value"} {
		if strings.Contains(logged, leak) {
			t.Fatalf("the log holds %q", leak)
		}
	}
}

func TestInternalErrorIsLoggedAsAChain(t *testing.T) {
	logs := captureLog(t)
	app := newTestAPI()

	app.Get("/test/chain", func(fiber.Ctx) error {
		_, err := strconv.Atoi("secret-input-value")

		return fmt.Errorf("load the ticket %w", err)
	})

	res, body := do(t, app, http.MethodGet, "/test/chain", nil)
	if res.StatusCode != http.StatusInternalServerError || strings.Contains(string(body), "load the ticket") {
		t.Fatalf("answer is %d %s", res.StatusCode, body)
	}

	logged := logs.String()

	for _, want := range []string{"*fmt.wrapError: load the ticket", "*strconv.NumError", "*errors.errorString"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the chain has no %q, %s", want, logged)
		}
	}

	// the text of a library error can hold the value of the request, so only its type is logged
	if strings.Contains(logged, "secret-input-value") {
		t.Fatalf("the log holds a request value, %s", logged)
	}
}

func TestAccessLogLabelsTheRoute(t *testing.T) {
	logs := captureLog(t)
	app := newTestAPI()

	do(t, app, http.MethodGet, "/api/config", nil)
	do(t, app, http.MethodGet, "/api/tickets/MGM-2611-0001-AAAA", nil)
	do(t, app, http.MethodPost, "/api/config", sameOriginHeaders())

	logged := logs.String()

	if strings.Contains(logged, "MGM-2611-0001-AAAA") {
		t.Fatalf("the log holds the raw path, %s", logged)
	}

	if strings.Count(logged, `"path":"/api/config"`) != 1 || strings.Count(logged, `"path":"unmatched"`) != 2 {
		t.Fatalf("the labels are wrong, %s", logged)
	}
}

func TestAccessLogShowsTheClientAddressOnlyAtTheDebugLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		level slog.Level
		want  bool
	}{
		{"info", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, true},
	} {
		logs := captureLogAt(t, tc.level)
		app := newTestAPI()

		do(t, app, http.MethodGet, "/api/config", map[string]string{"X-Real-IP": "203.0.113.9"})

		logged := logs.String()
		for _, key := range []string{`"peer":`, `"client_source":`, `"client":`} {
			if strings.Contains(logged, key) != tc.want {
				t.Fatalf("%s level, the key %s is present %v, want %v in %s", tc.name, key, !tc.want, tc.want, logged)
			}
		}

		if tc.want && !strings.Contains(logged, `"client_source":"peer"`) {
			t.Fatalf("%s level, the source of an untrusted peer is not peer in %s", tc.name, logged)
		}

		// an address that came in a header of the request is never written when the peer is not trusted, at any level
		if strings.Contains(logged, "203.0.113.9") {
			t.Fatalf("%s level, the log holds an address from a header of an untrusted peer, %s", tc.name, logged)
		}
	}
}
