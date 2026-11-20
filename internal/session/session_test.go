package session

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewTokenIsRandomAndHasTheRightShape(t *testing.T) {
	seen := map[string]bool{}

	for range 50 {
		token, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken failed %v", err)
		}

		if !validToken(token) || len(token) != 43 {
			t.Fatalf("the token %q has the wrong shape", token)
		}

		if seen[token] {
			t.Fatal("two tokens are equal")
		}

		seen[token] = true
	}
}

func TestHashToken(t *testing.T) {
	token, _ := NewToken()

	hash := HashToken(token)
	if len(hash) != 32 || !bytes.Equal(hash, HashToken(token)) {
		t.Fatalf("the hash has %d bytes or is not stable", len(hash))
	}

	other, _ := NewToken()
	if bytes.Equal(hash, HashToken(other)) {
		t.Fatal("two tokens have one hash")
	}

	// the hash of the known value "abc", so the function is SHA-256 and not another digest
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := hex.EncodeToString(HashToken("abc")); got != abc {
		t.Fatalf("the hash of abc is %s", got)
	}
}

func TestValidToken(t *testing.T) {
	good, _ := NewToken()

	tests := map[string]bool{
		good:                          true,
		"":                            false,
		good[:42]:                     false,
		good + "A":                    false,
		strings.Repeat("!", 43):       false,
		strings.Repeat("A", 42) + "=": false,
		" " + good[1:]:                false,
	}

	for token, want := range tests {
		if got := validToken(token); got != want {
			t.Fatalf("validToken(%q) is %v, want %v", token, got, want)
		}
	}
}

func TestCookieName(t *testing.T) {
	if CookieName(true) != "__Host-mgm_session" || CookieName(false) != "mgm_session" {
		t.Fatalf("the names are %q and %q", CookieName(true), CookieName(false))
	}
}

func TestNewCookie(t *testing.T) {
	token, _ := NewToken()

	secure := NewCookie(true, token, 7*24*time.Hour)
	if secure.Name != "__Host-mgm_session" || secure.Value != token || !secure.Secure || !secure.HttpOnly ||
		secure.SameSite != http.SameSiteLaxMode || secure.Path != "/" || secure.Domain != "" || secure.MaxAge != 604800 {
		t.Fatalf("the cookie is %+v", secure)
	}

	header := secure.String()
	for _, want := range []string{"__Host-mgm_session=" + token, "Path=/", "Max-Age=604800", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(header, want) {
			t.Fatalf("the header %q has no %q", header, want)
		}
	}

	if strings.Contains(header, "Domain") {
		t.Fatalf("the header %q has a Domain", header)
	}

	// the flag comes from the argument alone, and the plain name goes with it
	plain := NewCookie(false, token, 8*time.Hour)
	if plain.Name != "mgm_session" || plain.Secure || strings.Contains(plain.String(), "Secure") || plain.MaxAge != 8*3600 {
		t.Fatalf("the plain cookie is %+v", plain)
	}
}

func TestClearCookie(t *testing.T) {
	for _, secure := range []bool{true, false} {
		cleared := ClearCookie(secure)
		header := cleared.String()

		if cleared.Name != CookieName(secure) || cleared.Value != "" || cleared.Secure != secure || !cleared.HttpOnly || cleared.Path != "/" {
			t.Fatalf("the clearing cookie is %+v", cleared)
		}

		if !strings.Contains(header, "Max-Age=0") || strings.Contains(header, "Domain") {
			t.Fatalf("the header %q does not clear the cookie", header)
		}
	}
}

func TestLimits(t *testing.T) {
	tests := []struct {
		kind     Kind
		idle     time.Duration
		absolute time.Duration
	}{
		{KindGuest, 12 * time.Hour, 7 * 24 * time.Hour},
		{KindAdmin, 30 * time.Minute, 8 * time.Hour},
		{KindAdminSetup, 15 * time.Minute, 15 * time.Minute},
	}

	for _, tc := range tests {
		got, ok := LimitsFor(tc.kind)
		if !ok || got.Idle != tc.idle || got.Absolute != tc.absolute {
			t.Fatalf("%s has the limits %+v", tc.kind, got)
		}
	}

	if _, ok := LimitsFor("other"); ok {
		t.Fatal("an unknown kind has limits")
	}
}

func TestIdleExpiresAtNeverPassesTheAbsoluteLimit(t *testing.T) {
	start := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

	fresh := Session{Kind: KindGuest, LastSeenAt: start, ExpiresAt: start.Add(7 * 24 * time.Hour)}
	if got := fresh.IdleExpiresAt(); !got.Equal(start.Add(12 * time.Hour)) {
		t.Fatalf("the idle limit is %v", got)
	}

	late := Session{Kind: KindGuest, LastSeenAt: start.Add(6*24*time.Hour + 20*time.Hour), ExpiresAt: start.Add(7 * 24 * time.Hour)}
	if got := late.IdleExpiresAt(); !got.Equal(late.ExpiresAt) {
		t.Fatalf("the idle limit %v passes the absolute limit", got)
	}
}
