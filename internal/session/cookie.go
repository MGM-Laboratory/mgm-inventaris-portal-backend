package session

import (
	"net/http"
	"time"
)

const (
	cookieName     = "mgm_session"
	hostCookieName = "__Host-" + cookieName
)

// CookieName is the name of the session cookie. The prefix __Host- needs the Secure flag, and a browser drops a cookie that has the
// prefix without it, so a setup without Secure uses the plain name.
func CookieName(secure bool) string {
	if secure {
		return hostCookieName
	}

	return cookieName
}

// NewCookie builds the session cookie. The Secure flag comes from the setting and never from the request. The cookie has no Domain,
// so a browser sends it to the exact host only. Set maxAge to the absolute limit of the session.
func NewCookie(secure bool, token string, maxAge time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName(secure),
		Value:    token,
		Path:     "/",
		MaxAge:   max(int(maxAge/time.Second), 1),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearCookie builds the cookie that makes a browser drop the session cookie. A __Host- cookie can only be replaced by a cookie with
// the same attributes, so it has the same name, path and flags.
func ClearCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName(secure),
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0).UTC(),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}
