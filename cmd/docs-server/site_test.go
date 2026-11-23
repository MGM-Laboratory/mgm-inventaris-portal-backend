package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const (
	testPage   = "<!doctype html><title>Example page</title>"
	testSpec   = "openapi: 3.1.0\ninfo:\n  title: Example\n"
	testPolicy = "default-src 'none'; script-src 'sha256-AAAA'"
)

func gzipped(t *testing.T, text string) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func testFiles(t *testing.T) fstest.MapFS {
	t.Helper()

	return fstest.MapFS{
		pageFile:     {Data: []byte(testPage)},
		pageGzipFile: {Data: gzipped(t, testPage)},
		specFile:     {Data: []byte(testSpec)},
		policyFile:   {Data: []byte(testPolicy + "\n")},
	}
}

func testSite(t *testing.T) *site {
	t.Helper()

	s, err := loadSite(testFiles(t))
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func do(s *site, method, path string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, http.NoBody)
	for k, v := range header {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	return rec
}

func checkSecurityHeaders(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()

	for name, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Fatalf("%s has %s %q, want %q", label, name, got, want)
		}
	}

	if got := rec.Header().Get("Permissions-Policy"); !strings.Contains(got, "camera=()") || strings.Contains(got, "interest-cohort") {
		t.Fatalf("%s has Permissions-Policy %q", label, got)
	}

	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("%s has no Content-Security-Policy", label)
	}
}

func TestRoutes(t *testing.T) {
	s := testSite(t)

	tests := []struct {
		path        string
		status      int
		contentType string
		policy      string
		body        string
	}{
		{"/", http.StatusOK, "text/html; charset=utf-8", testPolicy, testPage},
		{"/index.html", http.StatusOK, "text/html; charset=utf-8", testPolicy, testPage},
		{"/openapi.yaml", http.StatusOK, "text/plain; charset=utf-8", fallbackPolicy, testSpec},
		{"/healthz", http.StatusOK, "", fallbackPolicy, ""},
		{"/nope", http.StatusNotFound, "", fallbackPolicy, ""},
		{"/index.html.gz", http.StatusNotFound, "", fallbackPolicy, ""},
		{"/csp.txt", http.StatusNotFound, "", fallbackPolicy, ""},
		{"/openapi.yaml/", http.StatusNotFound, "", fallbackPolicy, ""},
		{"/../index.html", http.StatusNotFound, "", fallbackPolicy, ""},
		{"/%2e%2e/etc/passwd", http.StatusNotFound, "", fallbackPolicy, ""},
		{"//", http.StatusNotFound, "", fallbackPolicy, ""},
		{"/healthz/", http.StatusNotFound, "", fallbackPolicy, ""},
		{"/favicon.ico", http.StatusNotFound, "", fallbackPolicy, ""},
	}

	for _, tc := range tests {
		rec := do(s, http.MethodGet, tc.path, nil)

		if rec.Code != tc.status {
			t.Fatalf("GET %s gave %d, want %d", tc.path, rec.Code, tc.status)
		}

		checkSecurityHeaders(t, "GET "+tc.path, rec)

		if got := rec.Header().Get("Content-Security-Policy"); got != tc.policy {
			t.Fatalf("GET %s has the policy %q, want %q", tc.path, got, tc.policy)
		}

		if tc.contentType != "" && rec.Header().Get("Content-Type") != tc.contentType {
			t.Fatalf("GET %s has the type %q", tc.path, rec.Header().Get("Content-Type"))
		}

		if tc.status == http.StatusOK && tc.body != rec.Body.String() {
			t.Fatalf("GET %s gave the body %q", tc.path, rec.Body.String())
		}

		if tc.path == "/healthz" && rec.Body.Len() != 0 {
			t.Fatalf("the health route sent a body %q", rec.Body.String())
		}
	}
}

func TestMethods(t *testing.T) {
	s := testSite(t)

	for _, path := range []string{"/", "/openapi.yaml", "/healthz", "/nope"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace} {
			rec := do(s, method, path, nil)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s gave %d", method, path, rec.Code)
			}

			if rec.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("%s %s has Allow %q", method, path, rec.Header().Get("Allow"))
			}

			checkSecurityHeaders(t, method+" "+path, rec)
		}
	}
}

func TestHeadHasNoBody(t *testing.T) {
	s := testSite(t)

	get := do(s, http.MethodGet, "/", nil)
	head := do(s, http.MethodHead, "/", nil)

	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD gave %d with %d bytes", head.Code, head.Body.Len())
	}

	if head.Header().Get("Content-Length") != get.Header().Get("Content-Length") || head.Header().Get("ETag") != get.Header().Get("ETag") {
		t.Fatal("HEAD and GET differ in length or tag")
	}

	if rec := do(s, http.MethodHead, "/healthz", nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD of the health route gave %d with %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestGzipNegotiation(t *testing.T) {
	s := testSite(t)

	tests := []struct {
		name   string
		accept string
		gzip   bool
	}{
		{"no header", "", false},
		{"gzip", "gzip", true},
		{"a list", "br, gzip", true},
		{"upper case", "GZIP", true},
		{"with a quality", "gzip;q=0.5", true},
		{"quality zero", "gzip;q=0", false},
		{"bad quality", "gzip;q=high", false},
		{"identity", "identity", false},
		{"another coding", "br, deflate", false},
		{"wildcard", "*", true},
		{"wildcard with identity refused", "*;q=1, identity;q=0", true},
		{"wildcard refused", "*;q=0", false},
		{"explicit gzip refusal beats the wildcard", "gzip;q=0, *", false},
		{"explicit gzip beats a refused wildcard", "*;q=0, gzip", true},
	}

	for _, tc := range tests {
		rec := do(s, http.MethodGet, "/", map[string]string{"Accept-Encoding": tc.accept})

		if rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("%s has Vary %q", tc.name, rec.Header().Get("Vary"))
		}

		if (rec.Header().Get("Content-Encoding") == "gzip") != tc.gzip {
			t.Fatalf("%s has Content-Encoding %q", tc.name, rec.Header().Get("Content-Encoding"))
		}

		body := rec.Body.Bytes()

		if tc.gzip {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("%s sent a body that is not gzip, %v", tc.name, err)
			}

			body, err = io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
		}

		if string(body) != testPage {
			t.Fatalf("%s sent the body %q", tc.name, body)
		}
	}

	// the spec has no gzip copy, so it never gets the header or the Vary
	rec := do(s, http.MethodGet, "/openapi.yaml", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Vary") != "" {
		t.Fatalf("the spec has Content-Encoding %q and Vary %q", rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	}
}

func TestConditionalRequests(t *testing.T) {
	s := testSite(t)

	plain := do(s, http.MethodGet, "/", nil)
	zipped := do(s, http.MethodGet, "/", map[string]string{"Accept-Encoding": "gzip"})

	plainTag, zipTag := plain.Header().Get("ETag"), zipped.Header().Get("ETag")
	if plainTag == "" || zipTag == "" || plainTag == zipTag {
		t.Fatalf("the tags are %q and %q", plainTag, zipTag)
	}

	if plain.Header().Get("Cache-Control") != assetCacheControl {
		t.Fatalf("the page has Cache-Control %q", plain.Header().Get("Cache-Control"))
	}

	for _, match := range []string{plainTag, "W/" + plainTag, `"other", ` + plainTag, "*"} {
		rec := do(s, http.MethodGet, "/", map[string]string{"If-None-Match": match})

		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Fatalf("If-None-Match %q gave %d with %d bytes", match, rec.Code, rec.Body.Len())
		}

		checkSecurityHeaders(t, "304", rec)
	}

	// the tag of the plain form does not match the gzip form
	rec := do(s, http.MethodGet, "/", map[string]string{"If-None-Match": plainTag, "Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("a tag of the other form gave %d", rec.Code)
	}

	rec = do(s, http.MethodGet, "/", map[string]string{"If-None-Match": zipTag, "Accept-Encoding": "gzip"})
	if rec.Code != http.StatusNotModified || rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("the gzip tag gave %d with Content-Encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}

	if rec := do(s, http.MethodGet, "/", map[string]string{"If-None-Match": `"stale"`}); rec.Code != http.StatusOK {
		t.Fatalf("a stale tag gave %d", rec.Code)
	}
}

func TestLoadSiteRefusesBrokenFiles(t *testing.T) {
	tests := []struct {
		name   string
		change func(fstest.MapFS)
	}{
		{"no page", func(f fstest.MapFS) { delete(f, pageFile) }},
		{"no gzip page", func(f fstest.MapFS) { delete(f, pageGzipFile) }},
		{"no spec", func(f fstest.MapFS) { delete(f, specFile) }},
		{"no policy", func(f fstest.MapFS) { delete(f, policyFile) }},
		{"empty page", func(f fstest.MapFS) { f[pageFile] = &fstest.MapFile{} }},
		{"blank policy", func(f fstest.MapFS) { f[policyFile] = &fstest.MapFile{Data: []byte(" \n")} }},
		{"two line policy", func(f fstest.MapFS) { f[policyFile] = &fstest.MapFile{Data: []byte("a\nb")} }},
	}

	for _, tc := range tests {
		files := testFiles(t)
		tc.change(files)

		if _, err := loadSite(files); err == nil {
			t.Fatalf("%s was accepted", tc.name)
		}
	}
}

func TestDrainingHealthRoute(t *testing.T) {
	s := testSite(t)
	s.draining.Store(true)

	if rec := do(s, http.MethodGet, "/healthz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("the health route gave %d while draining", rec.Code)
	}

	if rec := do(s, http.MethodGet, "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("the page gave %d while draining", rec.Code)
	}
}

// logLines runs the handler behind the access log and returns the JSON lines that it wrote.
func logLines(t *testing.T, s *site, requests ...[2]string) []map[string]any {
	t.Helper()

	var buf bytes.Buffer

	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler := accessLog(s, s.has)

	for _, request := range requests {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(request[0], request[1], http.NoBody))
	}

	var lines []map[string]any

	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}

		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("the log line %q is not JSON, %v", raw, err)
		}

		lines = append(lines, line)
	}

	return lines
}

func TestAccessLog(t *testing.T) {
	lines := logLines(t, testSite(t),
		[2]string{http.MethodGet, "/healthz"},
		[2]string{http.MethodGet, "/?token=secret"},
		[2]string{http.MethodGet, "/attacker-chosen-path"},
		[2]string{"BREW", "/openapi.yaml"},
	)

	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 and none for the health route", len(lines))
	}

	want := []struct {
		method, path string
		status       float64
	}{
		{"GET", "/", 200},
		{"GET", "other", 404},
		{"other", "/openapi.yaml", 405},
	}

	for i, w := range want {
		line := lines[i]
		if line["msg"] != "request" || line["method"] != w.method || line["path"] != w.path || line["status"] != w.status {
			t.Fatalf("line %d is %v, want %+v", i, line, w)
		}

		if _, ok := line["duration_ms"]; !ok {
			t.Fatalf("line %d has no duration", i)
		}
	}

	for _, line := range lines {
		for _, value := range line {
			if text, ok := value.(string); ok && (strings.Contains(text, "secret") || strings.Contains(text, "attacker")) {
				t.Fatalf("the log line holds text of the request, %v", line)
			}
		}
	}
}

func TestServerLimitsAndShutdown(t *testing.T) {
	s := testSite(t)
	srv := newServer(s)

	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes != maxHeaderBytes {
		t.Fatalf("the limits are not all set, %+v", srv)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- srv.Serve(lis) }()

	resp, err := http.Get("http://" + lis.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the health route gave %d", resp.StatusCode)
	}

	if err := shutdown(s, srv, 10*time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}

	if !s.draining.Load() {
		t.Fatal("the drain flag was not set")
	}

	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned %v", err)
	}
}

func TestSettingOr(t *testing.T) {
	t.Setenv("DOCS_TEST_SETTING", "")

	if got := settingOr("DOCS_TEST_SETTING", "fallback"); got != "fallback" {
		t.Fatalf("an empty setting gave %q", got)
	}

	t.Setenv("DOCS_TEST_SETTING", "value")

	if got := settingOr("DOCS_TEST_SETTING", "fallback"); got != "value" {
		t.Fatalf("a set setting gave %q", got)
	}
}
