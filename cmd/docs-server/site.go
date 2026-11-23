package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// The files of the site. The routes map to these four names and to nothing else, and no part of a request ever becomes a path.
	pageFile     = "index.html"
	pageGzipFile = "index.html.gz"
	specFile     = "openapi.yaml"
	policyFile   = "csp.txt"

	healthPath = "/healthz"

	// fallbackPolicy is the policy of every answer that is not the page. None of them loads or runs anything.
	fallbackPolicy = "default-src 'none'; frame-ancestors 'none'"

	// assetCacheControl keeps a page for five minutes. A deploy replaces the image, and the ETag lets a client check cheaply after that.
	assetCacheControl = "public, max-age=300, must-revalidate"

	allowedMethods = "GET, HEAD"
)

// asset is one file that a route serves. The bodies and the tags are made once at start.
type asset struct {
	contentType string
	policy      string
	plain       []byte
	plainTag    string
	// gzip is the pre-compressed copy of plain, or nil when the file has none
	gzip    []byte
	gzipTag string
}

// site serves the files that were read at start. It holds no other state than the flag of a drain.
type site struct {
	routes map[string]*asset
	// draining makes /healthz answer 503, so the proxy and the health check see a stop before the server stops listening
	draining atomic.Bool
}

// loadSite reads the four files of the site once. A missing or empty file stops the start, so a broken image never reports healthy.
func loadSite(files fs.FS) (*site, error) {
	page, err := readNonEmpty(files, pageFile)
	if err != nil {
		return nil, err
	}

	pageGzip, err := readNonEmpty(files, pageGzipFile)
	if err != nil {
		return nil, err
	}

	spec, err := readNonEmpty(files, specFile)
	if err != nil {
		return nil, err
	}

	policy, err := readNonEmpty(files, policyFile)
	if err != nil {
		return nil, err
	}

	pagePolicy := strings.TrimSpace(string(policy))
	if pagePolicy == "" || strings.ContainsAny(pagePolicy, "\r\n") {
		return nil, fmt.Errorf("%s must hold one line with the policy", policyFile)
	}

	pageAsset := &asset{
		contentType: "text/html; charset=utf-8",
		policy:      pagePolicy,
		plain:       page,
		plainTag:    tagOf(page, ""),
		gzip:        pageGzip,
		gzipTag:     tagOf(pageGzip, "-gzip"),
	}
	specAsset := &asset{
		contentType: "text/plain; charset=utf-8",
		policy:      fallbackPolicy,
		plain:       spec,
		plainTag:    tagOf(spec, ""),
	}

	return &site{routes: map[string]*asset{"/": pageAsset, "/index.html": pageAsset, "/openapi.yaml": specAsset}}, nil
}

func readNonEmpty(files fs.FS, name string) ([]byte, error) {
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return nil, fmt.Errorf("read %s %w", name, err)
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", name)
	}

	return data, nil
}

// tagOf makes a strong ETag from the content. The gzip copy has its own tag, because it is another representation.
func tagOf(data []byte, suffix string) string {
	sum := sha256.Sum256(data)

	return `"` + hex.EncodeToString(sum[:16]) + suffix + `"`
}

// ServeHTTP answers the fixed routes and nothing else. Every answer carries the security headers, also a 404 and a 405.
func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	setSecurityHeaders(header)
	header.Set("Content-Security-Policy", fallbackPolicy)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		header.Set("Allow", allowedMethods)
		fail(w, http.StatusMethodNotAllowed)

		return
	}

	if r.URL.Path == healthPath {
		header.Set("Cache-Control", "no-store")

		if s.draining.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		w.WriteHeader(http.StatusOK)

		return
	}

	file, ok := s.routes[r.URL.Path]
	if !ok {
		fail(w, http.StatusNotFound)

		return
	}

	serveAsset(w, r, file)
}

func fail(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, http.StatusText(status), status)
}

// setSecurityHeaders sets the headers that every answer carries except the Content-Security-Policy, which differs by route.
// Strict-Transport-Security is left to the proxy that ends the TLS.
func setSecurityHeaders(header http.Header) {
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// serveAsset answers a conditional request with 304 and otherwise sends the file, in the gzip form when the client accepts it. It does
// not serve ranges, because the files are small.
func serveAsset(w http.ResponseWriter, r *http.Request, file *asset) {
	header := w.Header()
	body, tag := file.plain, file.plainTag

	if file.gzip != nil {
		header.Add("Vary", "Accept-Encoding")

		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			body, tag = file.gzip, file.gzipTag
			header.Set("Content-Encoding", "gzip")
		}
	}

	header.Set("Content-Security-Policy", file.policy)
	header.Set("Cache-Control", assetCacheControl)
	header.Set("ETag", tag)

	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, tag) {
		header.Del("Content-Encoding")
		w.WriteHeader(http.StatusNotModified)

		return
	}

	header.Set("Content-Type", file.contentType)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodHead {
		return
	}

	// a client that went away is not an error of the server
	_, _ = w.Write(body)
}

// acceptsGzip reports whether the client accepts gzip. An entry for gzip decides by its quality. Without one, the wildcard decides, as
// RFC 9110 asks, so "*" accepts gzip too. A quality of zero or a malformed quality counts as no.
func acceptsGzip(header string) bool {
	wildcard := false

	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		coding = strings.TrimSpace(coding)

		switch {
		case strings.EqualFold(coding, "gzip"):
			return qualityAboveZero(params)
		case coding == "*":
			wildcard = qualityAboveZero(params)
		}
	}

	return wildcard
}

// qualityAboveZero reads the parameters after a coding. No quality means 1, and a quality that does not parse counts as zero.
func qualityAboveZero(params string) bool {
	quality, found := strings.CutPrefix(strings.ToLower(strings.TrimSpace(params)), "q=")
	if !found {
		return true
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(quality), 64)

	return err == nil && value > 0
}

// etagMatches compares an If-None-Match header with a tag the weak way, as RFC 9110 asks for this header.
func etagMatches(header, tag string) bool {
	for part := range strings.SplitSeq(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == tag {
			return true
		}
	}

	return false
}

// statusWriter remembers the status that the handler wrote, for the access log.
type statusWriter struct {
	http.ResponseWriter

	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the writer below.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// accessLog writes one JSON line for each request except the health route. It never writes the query, a header or a body. The path
// is logged only for a route that exists, and the method only when it is GET or HEAD, so a client cannot fill the log with text of its own.
func accessLog(next http.Handler, known func(path string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == healthPath {
			next.ServeHTTP(w, r)

			return
		}

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, r)

		path := "other"
		if known(r.URL.Path) {
			path = r.URL.Path
		}

		method := "other"
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			method = r.Method
		}

		slog.Info("request", "method", method, "path", path, "status", sw.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

// has reports whether the path is one of the routes.
func (s *site) has(path string) bool {
	_, ok := s.routes[path]

	return ok
}
