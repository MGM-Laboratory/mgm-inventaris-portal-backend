package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

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
	res, body := do(t, NewAPI(testConfig()), http.MethodGet, "/api/config", nil)

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
	app := NewAPI(testConfig())

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
	app := NewAPI(testConfig())

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

	app := NewAPI(testConfig())

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
	app := NewAPI(testConfig())

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
	app := NewAPI(testConfig())

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
	app := NewAPI(testConfig())

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
		res, body := do(t, app, tc.method, tc.path, nil)
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
			res, body := do(t, NewHealth(tc.pinger), http.MethodGet, tc.path, nil)

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
	res, body := do(t, NewHealth(fakePinger{}), http.MethodGet, "/api/config", nil)
	if res.StatusCode != http.StatusNotFound || decodeError(t, body).Error.Code != "not_found" {
		t.Fatalf("the health app answered %d %s", res.StatusCode, body)
	}
}
