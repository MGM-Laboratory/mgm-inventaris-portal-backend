package server

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

const validGuestBody = `{"name":"Example Borrower","nimNip":"000000000000000","email":"Example@Student.UB.ac.id"}`

func jsonHeaders() map[string]string {
	return map[string]string{"Origin": testOrigin, "Content-Type": "application/json"}
}

type sessionAnswer struct {
	Kind          string `json:"kind"`
	CSRFToken     string `json:"csrfToken"`
	ExpiresAt     string `json:"expiresAt"`
	IdleExpiresAt string `json:"idleExpiresAt"`
	Identity      *struct {
		Name   string `json:"name"`
		NimNip string `json:"nimNip"`
		Email  string `json:"email"`
	} `json:"identity"`
}

func mustSend(t *testing.T, app *fiber.App, method, path, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()

	res, data, err := send(app, method, path, body, headers)
	if err != nil {
		t.Fatalf("%s %s failed %v", method, path, err)
	}

	return res, data
}

func sessionCookieOf(t *testing.T, res *http.Response) *http.Cookie {
	t.Helper()

	for _, cookie := range res.Cookies() {
		if cookie.Name == session.CookieName(true) {
			return cookie
		}
	}

	t.Fatal("the answer sets no session cookie")

	return nil
}

func cookieHeader(token string) map[string]string {
	return map[string]string{"Cookie": session.CookieName(true) + "=" + token, "Origin": testOrigin}
}

func TestGuestSignInAnswersAndSetsTheCookie(t *testing.T) {
	store := newFakeStore()
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{}})

	res, body := mustSend(t, app, http.MethodPost, "/api/session/guest", validGuestBody, jsonHeaders())
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status is %d, %s", res.StatusCode, body)
	}

	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control is %q", res.Header.Get("Cache-Control"))
	}

	var got sessionAnswer
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the body is not JSON %v", err)
	}

	if got.Kind != "guest" || got.CSRFToken == "" || got.Identity == nil {
		t.Fatalf("the body is %s", body)
	}

	// the email is kept in lower case
	if got.Identity.Email != "example@student.ub.ac.id" || got.Identity.Name != "Example Borrower" || got.Identity.NimNip != "000000000000000" {
		t.Fatalf("the identity is %+v", got.Identity)
	}

	expires, err := time.Parse(time.RFC3339, got.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt is %q", got.ExpiresAt)
	}

	idle, err := time.Parse(time.RFC3339, got.IdleExpiresAt)
	if err != nil {
		t.Fatalf("idleExpiresAt is %q", got.IdleExpiresAt)
	}

	if gap := expires.Sub(idle); gap != 7*24*time.Hour-12*time.Hour {
		t.Fatalf("the gap between the limits is %v", gap)
	}

	cookie := sessionCookieOf(t, res)
	if cookie.Name != "__Host-mgm_session" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode ||
		cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge != 7*24*60*60 {
		t.Fatalf("the cookie is %+v", cookie)
	}

	if !store.has(cookie.Value) || cookie.Value == got.CSRFToken {
		t.Fatal("the cookie does not match the new session, or it equals the CSRF token")
	}

	// the same cookie reads the same session and the same CSRF token
	res, body = mustSend(t, app, http.MethodGet, "/api/session", "", cookieHeader(cookie.Value))
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), got.CSRFToken) {
		t.Fatalf("GET /api/session answers %d, %s", res.StatusCode, body)
	}

	// ending the session needs the CSRF token and clears the cookie
	headers := cookieHeader(cookie.Value)
	headers["X-CSRF-Token"] = got.CSRFToken

	res, _ = mustSend(t, app, http.MethodDelete, "/api/session", "", headers)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /api/session answers %d", res.StatusCode)
	}

	if cleared := sessionCookieOf(t, res); cleared.MaxAge >= 0 || cleared.Value != "" || !cleared.Secure || !cleared.HttpOnly {
		t.Fatalf("the clearing cookie is %+v", cleared)
	}

	if store.has(cookie.Value) {
		t.Fatal("the session still exists")
	}

	res, body = mustSend(t, app, http.MethodGet, "/api/session", "", cookieHeader(cookie.Value))
	if res.StatusCode != http.StatusUnauthorized || decodeError(t, body).Error.Code != "session_expired" {
		t.Fatalf("the old cookie answers %d, %s", res.StatusCode, body)
	}
}

func TestGuestSignInEndsThePreviousSession(t *testing.T) {
	store := newFakeStore()
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{}})

	oldToken, _ := store.add(session.KindGuest, nil)

	headers := jsonHeaders()
	headers["Cookie"] = session.CookieName(true) + "=" + oldToken

	res, body := mustSend(t, app, http.MethodPost, "/api/session/guest", validGuestBody, headers)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status is %d, %s", res.StatusCode, body)
	}

	if store.has(oldToken) || store.count() != 1 || !store.has(sessionCookieOf(t, res).Value) {
		t.Fatal("the old session is not replaced by the new one")
	}
}

func TestHiddenFieldGetsANormalAnswerAndCreatesNothing(t *testing.T) {
	store := newFakeStore()
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{}})

	oldToken, _ := store.add(session.KindGuest, nil)

	body := `{"name":"Example Borrower","nimNip":"000000000000000","email":"example@student.ub.ac.id","website":"https://spam.example.org"}`
	headers := jsonHeaders()
	headers["Cookie"] = session.CookieName(true) + "=" + oldToken

	res, data := mustSend(t, app, http.MethodPost, "/api/session/guest", body, headers)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status is %d, %s", res.StatusCode, data)
	}

	var got sessionAnswer
	if err := json.Unmarshal(data, &got); err != nil || got.Kind != "guest" || got.CSRFToken == "" || got.Identity == nil {
		t.Fatalf("the answer does not look normal, %s", data)
	}

	cookie := sessionCookieOf(t, res)
	if cookie.Value == "" || !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("the cookie does not look normal, %+v", cookie)
	}

	// nothing was created, and the session that came with the request was not ended
	if store.created != 0 || store.count() != 1 || !store.has(oldToken) || store.has(cookie.Value) {
		t.Fatalf("the store holds %d sessions and created %d", store.count(), store.created)
	}

	// the cookie opens nothing
	res, _ = mustSend(t, app, http.MethodGet, "/api/session", "", cookieHeader(cookie.Value))
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the fake cookie answers %d", res.StatusCode)
	}

	// a bad form with the hidden field filled gets the same refusal as without it
	bad := `{"name":"x","nimNip":"1","email":"a@example.org","website":"y"}`

	res, _ = mustSend(t, app, http.MethodPost, "/api/session/guest", bad, jsonHeaders())
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a bad form with the trap answers %d", res.StatusCode)
	}
}

func TestEmptyHiddenFieldIsNotATrap(t *testing.T) {
	store := newFakeStore()
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{}})

	body := `{"name":"Example Borrower","nimNip":"000000000000000","email":"example@student.ub.ac.id","website":"","extra":1}`

	res, data := mustSend(t, app, http.MethodPost, "/api/session/guest", body, jsonHeaders())
	if res.StatusCode != http.StatusCreated || store.created != 1 {
		t.Fatalf("status is %d with %d sessions created, %s", res.StatusCode, store.created, data)
	}
}

func TestGuestSignInBadBodies(t *testing.T) {
	app := newTestAPI()

	for name, body := range map[string]string{
		"empty": "", "cut off": `{"name":`, "an array": `[]`, "a string": `"text"`, "text": "name=Example",
		"trailing text": validGuestBody + "x",
	} {
		res, data := mustSend(t, app, http.MethodPost, "/api/session/guest", body, jsonHeaders())
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s answers %d, %s", name, res.StatusCode, data)
		}

		if e := decodeError(t, data); e.Error.Code != "bad_request" || e.Error.RequestID == "" {
			t.Fatalf("%s has the body %s", name, data)
		}
	}
}

func TestGuestSignInBodyLimitIs64KiB(t *testing.T) {
	app := newTestAPI()
	pad := func(size int) string {
		return `{"name":"Example Borrower","nimNip":"000000000000000","email":"example@student.ub.ac.id","pad":"` + strings.Repeat("a", size) + `"}`
	}

	// an unknown field is ignored, so a body just under the limit is a normal sign in
	res, data := mustSend(t, app, http.MethodPost, "/api/session/guest", pad(60<<10), jsonHeaders())
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("a body of 60 KiB answers %d, %s", res.StatusCode, data)
	}

	// the server refuses a body over the limit before a route reads it, and the test client reports that as an error
	_, _, err := send(app, http.MethodPost, "/api/session/guest", pad(65<<10), jsonHeaders())
	if err == nil || !strings.Contains(err.Error(), "body size exceeds") {
		t.Fatalf("a body of 65 KiB gave the error %v", err)
	}
}

func TestGuestSignInValidationBody(t *testing.T) {
	app := newTestAPI()

	res, data := mustSend(t, app, http.MethodPost, "/api/session/guest", `{"name":"Ed","nimNip":"12 34","email":"a@gmail.com"}`, jsonHeaders())
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status is %d, %s", res.StatusCode, data)
	}

	e := decodeError(t, data)
	if e.Error.Code != "validation_failed" || len(e.Error.Details) != 3 {
		t.Fatalf("the body is %s", data)
	}

	want := [][2]string{{"name", "too_short"}, {"nimNip", "invalid_format"}, {"email", "not_ub_email"}}
	for i, d := range e.Error.Details {
		if d.Field != want[i][0] || d.Code != want[i][1] || d.Message == "" {
			t.Fatalf("detail %d is %+v", i, d)
		}
	}
}

func TestGuestFormRules(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		field string
		code  string
	}{
		{"valid", `{"name":"Abc","nimNip":"12345678","email":"a@ub.ac.id"}`, "", ""},
		{"name is trimmed before it is counted", `{"name":"  ab  ","nimNip":"12345678","email":"a@ub.ac.id"}`, "name", "too_short"},
		{"name of 100 characters", `{"name":"` + strings.Repeat("é", 100) + `","nimNip":"12345678","email":"a@ub.ac.id"}`, "", ""},
		{"name of 101 characters", `{"name":"` + strings.Repeat("é", 101) + `","nimNip":"12345678","email":"a@ub.ac.id"}`, "name", "too_long"},
		{"name is missing", `{"nimNip":"12345678","email":"a@ub.ac.id"}`, "name", "required"},
		{"name is spaces", `{"name":"   ","nimNip":"12345678","email":"a@ub.ac.id"}`, "name", "required"},
		{"name is null", `{"name":null,"nimNip":"12345678","email":"a@ub.ac.id"}`, "name", "required"},
		{"name is a number", `{"name":5,"nimNip":"12345678","email":"a@ub.ac.id"}`, "name", "invalid_format"},
		{"name has a control character", `{"name":"Abc\u0007d","nimNip":"12345678","email":"a@ub.ac.id"}`, "name", "invalid_format"},
		{"nim of 7", `{"name":"Abc","nimNip":"1234567","email":"a@ub.ac.id"}`, "nimNip", "invalid_format"},
		{"nim of 20", `{"name":"Abc","nimNip":"12345678901234567890","email":"a@ub.ac.id"}`, "", ""},
		{"nim of 21", `{"name":"Abc","nimNip":"123456789012345678901","email":"a@ub.ac.id"}`, "nimNip", "invalid_format"},
		{"nim with a letter", `{"name":"Abc","nimNip":"19800101A1","email":"a@ub.ac.id"}`, "", ""},
		{"nim with a hyphen", `{"name":"Abc","nimNip":"1234-5678","email":"a@ub.ac.id"}`, "nimNip", "invalid_format"},
		{"nim with a non ASCII digit", `{"name":"Abc","nimNip":"١٢٣٤٥٦٧٨","email":"a@ub.ac.id"}`, "nimNip", "invalid_format"},
		{"nim is missing", `{"name":"Abc","email":"a@ub.ac.id"}`, "nimNip", "required"},
		{"email of a subdomain", `{"name":"Abc","nimNip":"12345678","email":"a@mail.student.ub.ac.id"}`, "", ""},
		{"email in capitals", `{"name":"Abc","nimNip":"12345678","email":"A@STUDENT.UB.AC.ID"}`, "", ""},
		{"email of another domain", `{"name":"Abc","nimNip":"12345678","email":"a@gmail.com"}`, "email", "not_ub_email"},
		{"email of a longer domain", `{"name":"Abc","nimNip":"12345678","email":"a@ub.ac.id.example.org"}`, "email", "not_ub_email"},
		{"email of a joined domain", `{"name":"Abc","nimNip":"12345678","email":"a@evilub.ac.id"}`, "email", "not_ub_email"},
		{"email without an at sign", `{"name":"Abc","nimNip":"12345678","email":"student.ub.ac.id"}`, "email", "invalid_format"},
		{"email without a local part", `{"name":"Abc","nimNip":"12345678","email":"@ub.ac.id"}`, "email", "invalid_format"},
		{"email with two at signs", `{"name":"Abc","nimNip":"12345678","email":"a@b@ub.ac.id"}`, "email", "invalid_format"},
		{"email with a space inside", `{"name":"Abc","nimNip":"12345678","email":"a b@ub.ac.id"}`, "email", "invalid_format"},
		{"email with an empty label", `{"name":"Abc","nimNip":"12345678","email":"a@..ub.ac.id"}`, "email", "invalid_format"},
		{"email with no dot in the domain", `{"name":"Abc","nimNip":"12345678","email":"a@localhost"}`, "email", "invalid_format"},
		{"email too long", `{"name":"Abc","nimNip":"12345678","email":"` + strings.Repeat("a", 250) + `@ub.ac.id"}`, "email", "too_long"},
		{"email is missing", `{"name":"Abc","nimNip":"12345678"}`, "email", "required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			form, err := parseGuestForm([]byte(tc.body))
			if err != nil {
				t.Fatalf("the body does not parse %v", err)
			}

			_, details := form.identity()

			if tc.field == "" {
				if len(details) != 0 {
					t.Fatalf("a valid form gave %+v", details)
				}

				return
			}

			if len(details) != 1 || details[0].Field != tc.field || details[0].Code != tc.code {
				t.Fatalf("the details are %+v, want %s %s", details, tc.field, tc.code)
			}
		})
	}
}

func TestGuestSignInRateLimit(t *testing.T) {
	store := newFakeStore()
	limiter := &fakeLimiter{over: true, retry: 25*time.Minute + 200*time.Millisecond}
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: limiter})

	// the limiter counts a request with a bad body too, and it runs before the body is read
	for _, body := range []string{validGuestBody, `{`} {
		res, data := mustSend(t, app, http.MethodPost, "/api/session/guest", body, jsonHeaders())
		if res.StatusCode != http.StatusTooManyRequests || decodeError(t, data).Error.Code != "rate_limited" {
			t.Fatalf("status is %d, %s", res.StatusCode, data)
		}

		if res.Header.Get("Retry-After") != "1501" {
			t.Fatalf("Retry-After is %q", res.Header.Get("Retry-After"))
		}
	}

	if store.created != 0 || len(limiter.keys) != 2 {
		t.Fatalf("created %d sessions and counted %d requests", store.created, len(limiter.keys))
	}

	if limiter.limits[0] != 60 || limiter.windows[0] != time.Hour || limiter.keys[0] != "guest-signin:0.0.0.0" {
		t.Fatalf("the limiter got %v %v %v", limiter.keys[0], limiter.limits[0], limiter.windows[0])
	}
}

func TestGuestSignInKeyUsesTheTrustedHeaderOnly(t *testing.T) {
	// the test connection has the peer 0.0.0.0, so trusting that address makes X-Real-IP count
	trusted := testConfig()
	trusted.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/32")}

	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"trusted peer", trusted, "guest-signin:2001:db8:1:2::/64"},
		{"untrusted peer", testConfig(), "guest-signin:0.0.0.0"},
	}

	for _, tc := range tests {
		limiter := &fakeLimiter{}
		app := NewAPI(tc.cfg, Deps{Sessions: newFakeStore(), Limiter: limiter})
		headers := jsonHeaders()
		headers["X-Real-IP"] = "2001:db8:1:2:3:4:5:6"

		mustSend(t, app, http.MethodPost, "/api/session/guest", validGuestBody, headers)

		if len(limiter.keys) != 1 || limiter.keys[0] != tc.want {
			t.Fatalf("%s got the keys %q, want %q", tc.name, limiter.keys, tc.want)
		}
	}
}

func TestResolveClient(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("::1/128")}

	tests := []struct {
		name, peer, header, want string
	}{
		{"trusted peer and an IPv4 header", "172.18.0.1:5000", "203.0.113.9", "203.0.113.9"},
		{"trusted peer and a header with spaces", "172.18.0.1:5000", " 203.0.113.9 ", "203.0.113.9"},
		{"trusted peer and an IPv6 header", "172.18.0.1:5000", "2001:db8:aaaa:bbbb:1:2:3:4", "2001:db8:aaaa:bbbb::/64"},
		{"trusted peer and a mapped header", "172.18.0.1:5000", "::ffff:203.0.113.9", "203.0.113.9"},
		{"trusted peer and no header", "172.18.0.1:5000", "", "172.18.0.1"},
		{"trusted peer and a bad header", "172.18.0.1:5000", "203.0.113.9, 10.0.0.1", "172.18.0.1"},
		{"trusted IPv6 peer", "[::1]:5000", "203.0.113.9", "203.0.113.9"},
		{"untrusted peer ignores the header", "198.51.100.7:5000", "203.0.113.9", "198.51.100.7"},
		{"untrusted IPv6 peer is a /64", "[2001:db8:1:2:3:4:5:6]:5000", "203.0.113.9", "2001:db8:1:2::/64"},
		{"two addresses of one /64 share a key", "[2001:db8:1:2:ffff:ffff:ffff:ffff]:5000", "", "2001:db8:1:2::/64"},
		{"mapped peer is an IPv4 address", "[::ffff:198.51.100.7]:5000", "", "198.51.100.7"},
		{"peer with a zone", "[fe80::1%eth0]:5000", "", "fe80::/64"},
		{"peer that does not parse", "not-an-address", "203.0.113.9", "unknown"},
	}

	for _, tc := range tests {
		if got := resolveClient(tc.peer, tc.header, proxies); got != tc.want {
			t.Fatalf("%s gave %q, want %q", tc.name, got, tc.want)
		}
	}

	// an empty list trusts nobody
	if got := resolveClient("172.18.0.1:5000", "203.0.113.9", nil); got != "172.18.0.1" {
		t.Fatalf("with no proxies the key is %q", got)
	}
}

func TestSessionRouteErrors(t *testing.T) {
	store := newFakeStore()
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{}})

	unknown, _ := session.NewToken()

	tests := []struct {
		name    string
		method  string
		headers map[string]string
		status  int
		code    string
	}{
		{"read with no cookie", http.MethodGet, nil, 401, "unauthenticated"},
		{"read with an unknown cookie", http.MethodGet, cookieHeader(unknown), 401, "session_expired"},
		{"read with a junk cookie", http.MethodGet, cookieHeader("junk"), 401, "session_expired"},
		{"end with no cookie", http.MethodDelete, sameOriginHeaders(), 401, "unauthenticated"},
		{"end with an unknown cookie", http.MethodDelete, cookieHeader(unknown), 401, "session_expired"},
	}

	for _, tc := range tests {
		res, data := mustSend(t, app, tc.method, "/api/session", "", tc.headers)
		if res.StatusCode != tc.status || decodeError(t, data).Error.Code != tc.code {
			t.Fatalf("%s answers %d, %s", tc.name, res.StatusCode, data)
		}
	}
}

func TestSessionLookupFailureAnswers503(t *testing.T) {
	store := newFakeStore()
	store.loadErr = &outageError{}
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{}})

	token, _ := session.NewToken()

	res, data := mustSend(t, app, http.MethodGet, "/api/session", "", cookieHeader(token))
	if res.StatusCode != http.StatusServiceUnavailable || decodeError(t, data).Error.Code != "unavailable" {
		t.Fatalf("status is %d, %s", res.StatusCode, data)
	}

	// the sign in route needs no session, so a failed lookup does not block it
	headers := jsonHeaders()
	headers["Cookie"] = session.CookieName(true) + "=" + token

	res, _ = mustSend(t, app, http.MethodPost, "/api/session/guest", validGuestBody, headers)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("the sign in answers %d", res.StatusCode)
	}
}

func TestSessionRoutesLogNoSecret(t *testing.T) {
	logs := captureLog(t)
	store := newFakeStore()
	store.loadErr = &outageError{}
	app := NewAPI(testConfig(), Deps{Sessions: store, Limiter: &fakeLimiter{err: &outageError{}}})

	token, _ := session.NewToken()
	headers := jsonHeaders()
	headers["Cookie"] = session.CookieName(true) + "=" + token
	headers["X-CSRF-Token"] = "sample-csrf-value"

	mustSend(t, app, http.MethodGet, "/api/session", "", headers)
	mustSend(t, app, http.MethodPost, "/api/session/guest", validGuestBody, headers)

	logged := logs.String()
	for _, leak := range []string{token, "sample-csrf-value", "example@student.ub.ac.id", "Example Borrower", "000000000000000"} {
		if strings.Contains(logged, leak) {
			t.Fatalf("the log holds %q, %s", leak, logged)
		}
	}
}

func TestDatabaseOutageIsAnswered503(t *testing.T) {
	limiter := &fakeLimiter{err: &outageError{}}
	app := NewAPI(testConfig(), Deps{Sessions: newFakeStore(), Limiter: limiter})

	res, data := mustSend(t, app, http.MethodPost, "/api/session/guest", validGuestBody, jsonHeaders())
	if res.StatusCode != http.StatusServiceUnavailable || decodeError(t, data).Error.Code != "unavailable" {
		t.Fatalf("status is %d, %s", res.StatusCode, data)
	}
}
