// Command api starts the REST API of the LabMGM inventory portal.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/server"
)

// version is set at build time with -ldflags.
var version = "dev"

const (
	// drainGrace is how long /readyz answers 503 before the servers stop, so the proxy and the rolling update see it. The two servers
	// then stop together under shutdownTimeout, so a stop signal takes drainGrace plus shutdownTimeout at most.
	drainGrace      = 3 * time.Second
	shutdownTimeout = 15 * time.Second
)

func main() {
	level := new(slog.LevelVar)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	// slog and not log.Fatal, because an alert on the error level would miss a line at the info level
	if err := run(level); err != nil {
		slog.Error("stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run(level *slog.LevelVar) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load the settings %w", err)
	}

	logLevel, err := cfg.SlogLevel()
	if err != nil {
		return fmt.Errorf("read the log level %w", err)
	}

	level.Set(logLevel)

	slog.Info("starting", "version", version, "environment", cfg.Environment)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// the pool opens first because it waits for a database that is still starting. The migrations run before the service listens, so a
	// copy that cannot migrate never reports healthy and the rolling update stops.
	pool, err := database.Open(ctx, cfg.DatabaseURL.Reveal(), cfg.DBMaxConns)
	if err != nil {
		return fmt.Errorf("open the database %w", err)
	}
	defer pool.Close()

	if cfg.MigrateOnStart {
		if err := database.Migrate(cfg.DatabaseURL.Reveal()); err != nil {
			return fmt.Errorf("migrate the database %w", err)
		}
	}

	api := server.NewAPI(cfg)
	health := server.NewHealth(pool, func() server.PoolStats {
		stat := pool.Stat()

		return server.PoolStats{Acquired: stat.AcquiredConns(), Max: stat.MaxConns()}
	})

	serverErr := make(chan error, 2)

	go func() { serverErr <- listen(api, cfg.HTTPAddr) }()
	go func() { serverErr <- listen(health.App(), cfg.HealthAddr) }()

	select {
	case err := <-serverErr:
		return fmt.Errorf("a server stopped %w", err)
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	// from here a second signal gets its default action, so an operator can stop the process without waiting for the timeout
	stop()

	health.Drain()
	time.Sleep(drainGrace)

	return shutdown(api, health.App())
}

// shutdown stops both servers together. A server that never ran, because the signal came first, is not an error.
func shutdown(api, health *fiber.App) error {
	var (
		wg   sync.WaitGroup
		errs [2]error
	)

	for i, target := range []struct {
		name string
		app  *fiber.App
	}{{"api", api}, {"health", health}} {
		wg.Go(func() {
			err := target.app.ShutdownWithTimeout(shutdownTimeout)
			if err != nil && !errors.Is(err, fiber.ErrNotRunning) {
				errs[i] = fmt.Errorf("stop the %s server %w", target.name, err)
			}
		})
	}

	wg.Wait()

	return errors.Join(errs[:]...)
}

func listen(app *fiber.App, addr string) error {
	if err := app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
		return err
	}

	return errors.New("the listener returned with no error")
}
