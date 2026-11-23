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

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/janitor"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/password"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ratelimit"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/secretbox"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/server"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ticket"
)

// version is set at build time with -ldflags.
var version = "dev"

const (
	// drainGrace is how long /readyz answers 503 before the servers stop, so the proxy and the rolling update see it. The two servers
	// then stop together under shutdownTimeout, so a stop signal takes drainGrace plus shutdownTimeout at most.
	drainGrace      = 3 * time.Second
	shutdownTimeout = 15 * time.Second
	// janitorInterval is the time between two sweeps of expired sessions, old counters and expired idempotency keys.
	janitorInterval = 10 * time.Minute
	// hashWait is how long a request waits for a free slot of the password hasher before it answers that the service is busy.
	hashWait = 3 * time.Second
)

func main() {
	if len(os.Args) > 1 {
		os.Exit(runCommand(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}

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

	if cfg.Environment == config.EnvProduction && len(cfg.TrustedProxies) == 0 {
		slog.Warn("TRUSTED_PROXIES is empty, so every client counts as the proxy address for the rate limits")
	}

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

	// the janitor stops before the pool closes, whatever ends the run
	janitorCtx, stopJanitor := context.WithCancel(ctx)
	janitorDone := make(chan struct{})

	go func() {
		defer close(janitorDone)

		janitor.Run(janitorCtx, pool, janitorInterval, time.Now)
	}()

	defer func() {
		stopJanitor()
		<-janitorDone
	}()

	admins, err := newAdminService(cfg, pool)
	if err != nil {
		return err
	}

	box, err := secretbox.New(cfg.TOTPKey())
	if err != nil {
		return fmt.Errorf("make the cursor box %w", err)
	}

	tickets := ticket.NewStore(pool, box, time.Now, ticket.Limits{
		MaxItems: cfg.MaxItemsPerRequest, MinDays: server.MinDurationDays, MaxDays: cfg.MaxDurationDays, MaxOpen: cfg.MaxOpenTickets,
	})

	api := server.NewAPI(cfg, server.Deps{
		Sessions:    session.NewStore(pool, time.Now),
		Limiter:     ratelimit.New(pool, time.Now),
		Admins:      admins,
		Catalog:     catalog.NewStore(pool, box, time.Now),
		Tickets:     tickets,
		Idempotency: idempotency.NewStore(pool, time.Now),
	})
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

// newAdminService builds the admin service of the settings on the pool. The command create-admin builds it the same way.
func newAdminService(cfg config.Config, db admin.DB) (*admin.Service, error) {
	box, err := secretbox.New(cfg.TOTPKey())
	if err != nil {
		return nil, fmt.Errorf("make the TOTP box %w", err)
	}

	return admin.NewService(admin.NewPostgres(db), password.New(cfg.PasswordHashConcurrency, hashWait), box, time.Now), nil
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
