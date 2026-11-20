package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
)

const (
	csrfHeader      = "X-CSRF-Token"
	fetchSiteHeader = "Sec-Fetch-Site"
	sameOrigin      = "same-origin"
)

// csrfExempt is the one list of the writes that carry no CSRF token, because they can be called without a session. They keep the
// Origin check. Every key is the method, a space and the path in lower case with no trailing slash.
var csrfExempt = map[string]struct{}{
	"POST /api/session/guest":  {},
	"POST /api/admin/session":  {},
	"POST /api/tickets/lookup": {},
}

func isCSRFExempt(method, path string) bool {
	// the router ignores case and a trailing slash, so the list does too
	_, ok := csrfExempt[method+" "+asciiLower(strings.TrimRight(path, "/"))]

	return ok
}

// isWrite says whether a method may change state, which is what the Origin and CSRF checks guard. Only the safe methods are free, so
// a method that a route adds later is guarded without a change here.
func isWrite(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// asciiLower folds only the letters A to Z, as the router does, so a path cannot match the list here and miss the route.
func asciiLower(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if ch >= 'A' && ch <= 'Z' {
			b[i] = ch + ('a' - 'A')
		}
	}

	return string(b)
}

// originAllowed applies the rule of docs/api.md. The Origin header has to equal the public origin. A request with no Origin header
// passes only when the browser says that it is same-origin.
func originAllowed(origin, fetchSite, publicOrigin string) bool {
	if origin == "" {
		return fetchSite == sameOrigin
	}

	return strings.EqualFold(origin, publicOrigin)
}

// OriginAndCSRF guards every POST, PATCH and DELETE. The origin check comes first and covers every write. The CSRF token check
// follows for a write that has a session, except on the routes of csrfExempt. A write without a live session passes this check, and
// the guard of its route answers 401. A write whose session lookup failed is refused here with 503 or 500.
func OriginAndCSRF(publicOrigin string) fiber.Handler {
	return func(c fiber.Ctx) error {
		method := c.Method()
		if !isWrite(method) {
			return c.Next()
		}

		if !originAllowed(c.Get(fiber.HeaderOrigin), c.Get(fetchSiteHeader), publicOrigin) {
			return NewError(CodeOriginNotAllowed)
		}

		if isCSRFExempt(method, c.Path()) {
			return c.Next()
		}

		// a lookup that failed must not let a write skip the token check, so the write is refused here
		if state := stateOf(c); state.loadErr != nil {
			return dbError("load the session", state.loadErr)
		}

		sess := currentSession(c)
		if sess == nil {
			return c.Next()
		}

		// an empty header never matches, even against an empty token
		sent := c.Get(csrfHeader)
		if sent == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(sess.CSRFToken)) != 1 {
			return NewError(CodeCSRFFailed)
		}

		return c.Next()
	}
}
