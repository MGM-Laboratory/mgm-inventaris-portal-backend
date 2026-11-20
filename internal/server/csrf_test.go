package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

func TestOriginAllowed(t *testing.T) {
	tests := []struct {
		name, origin, fetchSite string
		want                    bool
	}{
		{"the public origin", testOrigin, "", true},
		{"the public origin in capitals", "HTTPS://PORTAL.EXAMPLE.ORG", "", true},
		{"the public origin and a cross site hint", testOrigin, "cross-site", true},
		{"another host", "https://evil.example.org", "", false},
		{"another host and a same origin hint", "https://evil.example.org", "same-origin", false},
		{"the same host on http", "http://portal.example.org", "", false},
		{"the same host on another port", "https://portal.example.org:8443", "", false},
		{"a subdomain", "https://a.portal.example.org", "", false},
		{"a longer host", "https://portal.example.org.evil.example.org", "", false},
		{"a trailing slash", testOrigin + "/", "", false},
		{"the null origin", "null", "", false},
		{"the null origin and a same origin hint", "null", "same-origin", false},
		{"no origin and same origin", "", "same-origin", true},
		{"no origin and no hint", "", "", false},
		{"no origin and same site", "", "same-site", false},
		{"no origin and cross site", "", "cross-site", false},
		{"no origin and none", "", "none", false},
	}

	for _, tc := range tests {
		if got := originAllowed(tc.origin, tc.fetchSite, testOrigin); got != tc.want {
			t.Fatalf("%s gave %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCSRFExemptList(t *testing.T) {
	tests := []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/session/guest", true},
		{http.MethodPost, "/api/admin/session", true},
		{http.MethodPost, "/api/tickets/lookup", true},
		{http.MethodPost, "/API/Session/Guest/", true},
		{http.MethodDelete, "/api/session/guest", false},
		{http.MethodPost, "/api/session", false},
		{http.MethodDelete, "/api/session", false},
		{http.MethodPost, "/api/tickets", false},
		{http.MethodPost, "/api/tickets/lookup/x", false},
		{http.MethodPost, "/api/admin/session/x", false},
		{http.MethodPost, "/api/admin/me/password", false},
		{http.MethodPatch, "/api/admin/session", false},
	}

	for _, tc := range tests {
		if got := isCSRFExempt(tc.method, tc.path); got != tc.want {
			t.Fatalf("%s %s gave %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestOriginAndCSRFOnTheRoutes(t *testing.T) {
	store := newFakeStore()
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{}})

	// a write route that needs a session, and a read route
	app.Post("/test/write", RequireSession(), func(c fiber.Ctx) error { return c.SendStatus(http.StatusNoContent) })
	app.Patch("/test/write", RequireSession(), func(c fiber.Ctx) error { return c.SendStatus(http.StatusNoContent) })

	token, sess := store.add(session.KindGuest, nil)
	withCookieOf := func(token string, extra map[string]string) map[string]string {
		h := map[string]string{"Cookie": session.CookieName(true) + "=" + token}
		for k, v := range extra {
			h[k] = v
		}

		return h
	}
	withCookie := func(extra map[string]string) map[string]string { return withCookieOf(token, extra) }

	// the two routes that end or replace a session get sessions of their own, so the other cases keep theirs
	endToken, endSess := store.add(session.KindGuest, nil)
	replaceToken, _ := store.add(session.KindGuest, nil)

	tests := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		status  int
		code    string
	}{
		{"write with the token", http.MethodPost, "/test/write", withCookie(map[string]string{"Origin": testOrigin, "X-CSRF-Token": sess.CSRFToken}), 204, ""},
		{"patch with the token", http.MethodPatch, "/test/write", withCookie(map[string]string{"Origin": testOrigin, "X-CSRF-Token": sess.CSRFToken}), 204, ""},
		{"delete with the token", http.MethodDelete, "/api/session", withCookieOf(endToken, map[string]string{"Origin": testOrigin, "X-CSRF-Token": endSess.CSRFToken}), 204, ""},
		{"write without the token", http.MethodPost, "/test/write", withCookie(map[string]string{"Origin": testOrigin}), 403, "csrf_failed"},
		{"write with a wrong token", http.MethodPost, "/test/write", withCookie(map[string]string{"Origin": testOrigin, "X-CSRF-Token": sess.CSRFToken + "x"}), 403, "csrf_failed"},
		{"write with a short token", http.MethodPost, "/test/write", withCookie(map[string]string{"Origin": testOrigin, "X-CSRF-Token": sess.CSRFToken[:10]}), 403, "csrf_failed"},
		{"write with the session cookie as token", http.MethodPost, "/test/write", withCookie(map[string]string{"Origin": testOrigin, "X-CSRF-Token": token}), 403, "csrf_failed"},
		{"write from another origin", http.MethodPost, "/test/write", withCookie(map[string]string{"Origin": "https://evil.example.org", "X-CSRF-Token": sess.CSRFToken}), 403, "origin_not_allowed"},
		{"write with no origin and no hint", http.MethodPost, "/test/write", withCookie(map[string]string{"X-CSRF-Token": sess.CSRFToken}), 403, "origin_not_allowed"},
		{"write with no origin and a cross site hint", http.MethodPost, "/test/write", withCookie(map[string]string{"Sec-Fetch-Site": "cross-site", "X-CSRF-Token": sess.CSRFToken}), 403, "origin_not_allowed"},
		{"write with no origin and a same origin hint", http.MethodPost, "/test/write", withCookie(map[string]string{"Sec-Fetch-Site": "same-origin", "X-CSRF-Token": sess.CSRFToken}), 204, ""},
		{"origin is checked before the token", http.MethodPost, "/test/write", withCookie(map[string]string{"Origin": "https://evil.example.org"}), 403, "origin_not_allowed"},
		{"write with no session", http.MethodPost, "/test/write", map[string]string{"Origin": testOrigin}, 401, "unauthenticated"},
		{"read needs nothing but the session", http.MethodGet, "/api/session", withCookie(map[string]string{"Origin": "https://evil.example.org"}), 200, ""},
		{"read of the config needs nothing", http.MethodGet, "/api/config", nil, 200, ""},
		{"exempt route with another origin", http.MethodPost, "/api/session/guest", withCookie(map[string]string{"Origin": "https://evil.example.org", "Content-Type": "application/json"}), 403, "origin_not_allowed"},
		{"exempt route with no origin", http.MethodPost, "/api/session/guest", map[string]string{"Content-Type": "application/json"}, 403, "origin_not_allowed"},
		{"exempt route with a session and no token", http.MethodPost, "/api/session/guest", withCookieOf(replaceToken, map[string]string{"Origin": testOrigin, "Content-Type": "application/json"}), 201, ""},
	}

	for _, tc := range tests {
		body := ""
		if tc.path == "/api/session/guest" {
			body = validGuestBody
		}

		res, data := mustSend(t, app, tc.method, tc.path, body, tc.headers)
		if res.StatusCode != tc.status {
			t.Fatalf("%s answers %d, %s", tc.name, res.StatusCode, data)
		}

		if tc.code != "" && decodeError(t, data).Error.Code != tc.code {
			t.Fatalf("%s has the body %s", tc.name, data)
		}
	}
}

func TestGuards(t *testing.T) {
	store := newFakeStore()
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{}})

	ok := func(c fiber.Ctx) error { return c.SendString("ok") }

	app.Get("/test/any", RequireSession(), ok)
	app.Get("/test/guest", RequireGuest(), ok)
	app.Get("/test/identity", RequireIdentity(), ok)
	app.Get("/test/admin", RequireAdmin(), ok)

	identity := &session.Identity{Name: "Example Borrower", NimNip: "000000000000000", Email: "example@student.ub.ac.id"}
	guestToken, _ := store.add(session.KindGuest, nil)
	identifiedToken, _ := store.add(session.KindGuest, identity)
	adminToken, _ := store.add(session.KindAdmin, nil)
	setupToken, _ := store.add(session.KindAdminSetup, nil)
	unknown, _ := session.NewToken()

	tests := []struct {
		path, token string
		status      int
		code        string
	}{
		{"/test/any", "", 401, "unauthenticated"},
		{"/test/any", unknown, 401, "session_expired"},
		{"/test/any", guestToken, 200, ""},
		{"/test/any", setupToken, 200, ""},
		{"/test/guest", "", 401, "unauthenticated"},
		{"/test/guest", unknown, 401, "session_expired"},
		{"/test/guest", guestToken, 200, ""},
		{"/test/guest", adminToken, 403, "forbidden"},
		{"/test/guest", setupToken, 403, "forbidden"},
		{"/test/identity", "", 401, "unauthenticated"},
		{"/test/identity", guestToken, 403, "identity_required"},
		{"/test/identity", identifiedToken, 200, ""},
		{"/test/identity", adminToken, 403, "forbidden"},
		{"/test/admin", "", 401, "unauthenticated"},
		{"/test/admin", unknown, 401, "session_expired"},
		{"/test/admin", guestToken, 403, "forbidden"},
		{"/test/admin", setupToken, 403, "forbidden"},
		{"/test/admin", adminToken, 200, ""},
	}

	for _, tc := range tests {
		headers := map[string]string{}
		if tc.token != "" {
			headers["Cookie"] = session.CookieName(true) + "=" + tc.token
		}

		res, data := mustSend(t, app, http.MethodGet, tc.path, "", headers)
		if res.StatusCode != tc.status {
			t.Fatalf("%s with %q answers %d, %s", tc.path, tc.token, res.StatusCode, data)
		}

		if tc.code != "" && decodeError(t, data).Error.Code != tc.code {
			t.Fatalf("%s has the body %s", tc.path, data)
		}
	}
}

func TestGetSessionShowsTheAdmin(t *testing.T) {
	store := newFakeStore()
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{}})

	token, _ := store.add(session.KindAdmin, nil)

	res, data := mustSend(t, app, http.MethodGet, "/api/session", "", cookieHeader(token))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status is %d, %s", res.StatusCode, data)
	}

	for _, want := range []string{`"kind":"admin"`, `"admin":{"id":"00000000-0000-4000-8000-000000000009"`, `"name":"Example Admin"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("the body has no %s, %s", want, data)
		}
	}

	if strings.Contains(string(data), `"identity"`) {
		t.Fatalf("an admin session shows an identity, %s", data)
	}
}

func TestDatabaseUnreachable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"a connection error of PostgreSQL", &pgconn.PgError{Code: "08006"}, true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"a statement timeout", &pgconn.PgError{Code: "57014"}, true},
		{"a unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"a syntax error", &pgconn.PgError{Code: "42601"}, false},
		{"a network error", &outageError{}, true},
		{"a deadline", context.DeadlineExceeded, true},
		{"a closed connection", io.EOF, true},
		{"a wrapped network error", errors.Join(errors.New("insert"), &outageError{}), true},
		{"a plain error", errors.New("something else"), false},
	}

	for _, tc := range tests {
		if got := databaseUnreachable(tc.err); got != tc.want {
			t.Fatalf("%s gave %v, want %v", tc.name, got, tc.want)
		}
	}

	var apiErr *Error
	if !errors.As(dbError("insert", &outageError{}), &apiErr) || apiErr.Code != CodeUnavailable {
		t.Fatal("an outage is not the code unavailable")
	}

	if errors.As(dbError("insert", errors.New("bug")), &apiErr) {
		t.Fatal("a bug became an API error")
	}
}
