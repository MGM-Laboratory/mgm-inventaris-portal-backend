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
		{"the default", "", "http://127.0.0.1:8080/healthz", false},
		{"a port only", ":9090", "http://127.0.0.1:9090/healthz", false},
		{"a host and a port", "127.0.0.1:9000", "http://127.0.0.1:9000/healthz", false},
		{"every interface", "0.0.0.0:8080", "http://127.0.0.1:8080/healthz", false},
		{"every interface in IPv6", "[::]:8080", "http://127.0.0.1:8080/healthz", false},
		{"an IPv6 host", "[::1]:8080", "http://[::1]:8080/healthz", false},
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
		if r.URL.Path != healthPath || r.Method != http.MethodGet {
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
		{"draining", http.StatusServiceUnavailable, 1},
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

	if code := runHealthcheck(nil, addr, time.Second, &stderr); code != 1 || stderr.Len() == 0 {
		t.Fatalf("got the exit code %d and the output %q", code, stderr.String())
	}
}

func TestRunHealthcheckAgainstTheSite(t *testing.T) {
	s := testSite(t)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "http://")

	if code := runHealthcheck(nil, addr, time.Second, &bytes.Buffer{}); code != 0 {
		t.Fatalf("the check of a running site gave %d", code)
	}

	s.draining.Store(true)

	if code := runHealthcheck(nil, addr, time.Second, &bytes.Buffer{}); code != 1 {
		t.Fatalf("the check of a draining site gave %d", code)
	}
}

func TestRunCommand(t *testing.T) {
	var stderr bytes.Buffer

	if code := runCommand([]string{"serve"}, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage") {
		t.Fatalf("an unknown command gave %d and %q", code, stderr.String())
	}

	stderr.Reset()

	if code := runHealthcheck([]string{"extra"}, "", time.Second, &stderr); code != 2 {
		t.Fatalf("an argument gave %d", code)
	}

	// the address of the environment is used, and nothing listens on this port, so the check fails with 1
	t.Setenv(addrEnv, "127.0.0.1:1")

	if code := runCommand([]string{"healthcheck"}, &stderr); code != 1 {
		t.Fatalf("the check with the address of the environment gave %d", code)
	}
}
