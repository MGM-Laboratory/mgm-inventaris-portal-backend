// Command docs-server serves the API docs page of the LabMGM inventory portal. It serves a page that was built from openapi.yaml, the
// raw spec and a health route, from files that it reads once at start, and it uses only the standard library.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is set at build time with -ldflags.
var version = "dev"

const (
	dirEnv      = "DOCS_DIR"
	dirDefault  = "/site"
	addrEnv     = "DOCS_ADDR"
	addrDefault = ":8080"

	// drainGrace is how long /healthz answers 503 before the server stops listening, so the proxy and the health check see it.
	// A stop signal takes drainGrace plus shutdownTimeout at most.
	drainGrace      = 3 * time.Second
	shutdownTimeout = 10 * time.Second

	// The limits of a connection. The routes take no body, so a client has little reason to be slow.
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = time.Minute
	idleTimeout       = 30 * time.Second
	maxHeaderBytes    = 8 << 10
)

func main() {
	if len(os.Args) > 1 {
		os.Exit(runCommand(os.Args[1:], os.Stderr))
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// slog and not log.Fatal, as the API does, so an alert on the error level sees the line
	if err := run(); err != nil {
		slog.Error("stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	dir := settingOr(dirEnv, dirDefault)
	addr := settingOr(addrEnv, addrDefault)

	s, err := loadSite(os.DirFS(dir))
	if err != nil {
		return fmt.Errorf("load the site from %s %w", dir, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s %w", addr, err)
	}

	srv := newServer(s)

	slog.Info("starting", "version", version, "addr", listener.Addr().String())

	serveErr := make(chan error, 1)

	go func() { serveErr <- srv.Serve(listener) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("the server stopped %w", err)
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	// from here a second signal gets its default action, so an operator can stop the process without waiting for the timeout
	stop()

	return shutdown(s, srv, drainGrace, shutdownTimeout)
}

// newServer builds the server with its limits. Its own errors go to the same JSON log as the rest.
func newServer(s *site) *http.Server {
	return &http.Server{
		Handler:           accessLog(s, s.has),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
}

// shutdown makes /healthz answer 503, waits for the grace, and then stops the server and lets the requests that run finish.
func shutdown(s *site, srv *http.Server, grace, timeout time.Duration) error {
	s.draining.Store(true)
	time.Sleep(grace)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("stop the server %w", err)
	}

	return nil
}

// settingOr returns the value of an environment variable, or the default when it is empty.
func settingOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
