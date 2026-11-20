// Package database opens the PostgreSQL pool of the service and applies the SQL migrations that are embedded in the binary.
package database

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// The limits of a connection. The database is shared, so a statement that runs for too long or a transaction that is left open
// must not hold its resources.
const (
	statementTimeout       = "15s"
	lockTimeout            = "5s"
	idleInTransactionLimit = "30s"
	connectTimeout         = 5 * time.Second
	pingTimeout            = 5 * time.Second
	connectAttempts        = 10
	connectWait            = 2 * time.Second
)

// Open returns a pool and waits until the database answers, so a restart of the database during a deploy does not stop the service.
func Open(ctx context.Context, url string, maxConns uint) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// the parse error can echo the URL, which holds the password
		return nil, errors.New("the database settings are not valid")
	}

	cfg.MaxConns = int32(min(maxConns, 1<<30))
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	cfg.ConnConfig.RuntimeParams["application_name"] = "mgm-inventaris-api"
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = statementTimeout
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = lockTimeout
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = idleInTransactionLimit

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create the pool %w", err)
	}

	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		err = pool.Ping(pingCtx)

		cancel()

		if err == nil {
			return pool, nil
		}

		if attempt == connectAttempts || ctx.Err() != nil {
			pool.Close()

			return nil, fmt.Errorf("the database does not answer %w", err)
		}

		slog.Warn("the database does not answer yet", "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
		case <-time.After(connectWait):
		}
	}
}

// AheadError means the database is at a migration that this binary has no file for, because a newer copy migrated it.
type AheadError struct {
	Database, Binary uint
}

func (e *AheadError) Error() string {
	return fmt.Sprintf("the database is at migration version %d and this version of the service knows only up to %d, "+
		"so start the newer version, or set MIGRATE_ON_START to false to run this one on purpose", e.Database, e.Binary)
}

// Migrate applies the pending migrations. The library holds an advisory lock of PostgreSQL while it applies them, so two copies never
// migrate together and the second finds nothing to do.
func Migrate(url string) error {
	migrator, err := newMigrator(url)
	if err != nil {
		return err
	}

	defer func() { _, _ = migrator.Close() }()

	if err := checkNotAhead(migrator); err != nil {
		return err
	}

	upErr := migrator.Up()
	if upErr == nil || errors.Is(upErr, migrate.ErrNoChange) {
		return nil
	}

	// a newer copy can migrate between the check above and the lock that Up waits for, and Up then fails on a version it has no file for
	if aheadErr := checkNotAhead(migrator); aheadErr != nil {
		return aheadErr
	}

	return fmt.Errorf("apply the migrations %w", upErr)
}

func checkNotAhead(migrator *migrate.Migrate) error {
	current, dirty, err := migrator.Version()
	if errors.Is(err, migrate.ErrNilVersion) || dirty {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read the schema version %w", err)
	}

	newest, err := newestVersion()
	if err != nil {
		return err
	}

	if current > newest {
		return &AheadError{Database: current, Binary: newest}
	}

	return nil
}

// MigrateDown reverts every migration. It exists for the tests of the down files and the service never calls it.
func MigrateDown(url string) error {
	migrator, err := newMigrator(url)
	if err != nil {
		return err
	}

	defer func() { _, _ = migrator.Close() }()

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("revert the migrations %w", err)
	}

	return nil
}

func newMigrator(url string) (*migrate.Migrate, error) {
	source, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read the embedded migrations %w", err)
	}

	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		// the parse error can echo the URL, which holds the password
		return nil, errors.New("the database settings are not valid")
	}

	cfg.ConnectTimeout = connectTimeout
	cfg.RuntimeParams["application_name"] = "mgm-inventaris-migrate"

	db := stdlib.OpenDB(*cfg)

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("open the migration driver %w", err)
	}

	migrator, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("create the migrator %w", err)
	}

	return migrator, nil
}

// newestVersion returns the highest version among the embedded migrations.
func newestVersion() (uint, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.up.sql")
	if err != nil {
		return 0, fmt.Errorf("list the embedded migrations %w", err)
	}

	var newest uint

	for _, name := range names {
		prefix, _, _ := strings.Cut(strings.TrimPrefix(name, "migrations/"), "_")

		version, err := strconv.ParseUint(prefix, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("the migration %s has no number in front of its name", name)
		}

		newest = max(newest, uint(version))
	}

	return newest, nil
}
