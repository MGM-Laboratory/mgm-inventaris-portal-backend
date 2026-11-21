package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthURL(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
		fail bool
	}{
		{"the default", "", "http://127.0.0.1:8081/healthz", false},
		{"a port only", ":8081", "http://127.0.0.1:8081/healthz", false},
		{"a host and a port", "127.0.0.1:9000", "http://127.0.0.1:9000/healthz", false},
		{"every interface", "0.0.0.0:8081", "http://127.0.0.1:8081/healthz", false},
		{"every interface in IPv6", "[::]:8081", "http://127.0.0.1:8081/healthz", false},
		{"an IPv6 host", "[::1]:8081", "http://[::1]:8081/healthz", false},
		{"no port", "127.0.0.1", "", true},
		{"not an address", "not an address", "", true},
	}

	for _, tc := range tests {
		got, err := healthURL(tc.addr)
		if (err != nil) != tc.fail || got != tc.want {
			t.Fatalf("%s gave %q and %v", tc.name, got, err)
		}
	}
}

func serveStatus(t *testing.T, status int) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" || r.Method != http.MethodGet {
			http.NotFound(w, r)

			return
		}

		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "http://")
}

func TestRunHealthcheckExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   int
	}{
		{"ok", http.StatusOK, 0},
		{"unavailable", http.StatusServiceUnavailable, 1},
		{"not found", http.StatusNotFound, 1},
		{"no content", http.StatusNoContent, 1},
		{"redirect", http.StatusFound, 1},
	} {
		var stderr bytes.Buffer

		if code := runHealthcheck(nil, serveStatus(t, tc.status), time.Second, &stderr); code != tc.code {
			t.Fatalf("%s gave the exit code %d and the output %q", tc.name, code, stderr.String())
		}

		if tc.code == 0 && stderr.Len() != 0 {
			t.Fatalf("%s printed %q", tc.name, stderr.String())
		}
	}
}

func TestRunHealthcheckFailsWhenNothingListens(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := lis.Addr().String()
	_ = lis.Close()

	var stderr bytes.Buffer

	if code := runHealthcheck(nil, addr, time.Second, &stderr); code != 1 || !strings.Contains(stderr.String(), "healthcheck failed") {
		t.Fatalf("the exit code is %d and the output %q", code, stderr.String())
	}
}

func TestRunHealthcheckFailsOnATimeout(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	var stderr bytes.Buffer

	start := time.Now()

	if code := runHealthcheck(nil, strings.TrimPrefix(srv.URL, "http://"), 100*time.Millisecond, &stderr); code != 1 {
		t.Fatalf("the exit code is %d and the output %q", code, stderr.String())
	}

	if time.Since(start) > 5*time.Second {
		t.Fatalf("the check took %s, so the timeout did not bound it", time.Since(start))
	}
}

func TestRunHealthcheckRefusesAnArgumentAndABadAddress(t *testing.T) {
	var stderr bytes.Buffer

	if code := runHealthcheck([]string{"extra"}, "", time.Second, &stderr); code != 2 {
		t.Fatalf("an argument gave the exit code %d", code)
	}

	stderr.Reset()

	if code := runHealthcheck(nil, "not an address", time.Second, &stderr); code != 1 || !strings.Contains(stderr.String(), "HEALTH_ADDR") {
		t.Fatalf("a bad address gave the exit code %d and the output %q", code, stderr.String())
	}
}

func TestRunCommandRunsTheHealthcheckFromTheEnvironment(t *testing.T) {
	var stdout, stderr bytes.Buffer

	t.Setenv("HEALTH_ADDR", serveStatus(t, http.StatusOK))

	if code := runCommand([]string{"healthcheck"}, strings.NewReader(""), &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Fatalf("the exit code is %d, stdout %q and stderr %q", code, stdout.String(), stderr.String())
	}

	// no database setting is set here, so a pass shows that the command reads HEALTH_ADDR only
	t.Setenv("DATABASE_URL", "")
	t.Setenv("TOTP_ENCRYPTION_KEY", "")

	if code := runCommand([]string{"healthcheck"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("the exit code is %d and stderr %q", code, stderr.String())
	}
}
