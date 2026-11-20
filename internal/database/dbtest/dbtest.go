// Package dbtest gives a test its own empty PostgreSQL database with every migration applied. A test skips itself when DATABASE_URL
// is not set, so a run on a machine without a database still passes. CI sets it for a throwaway server.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database"
)

const setupTimeout = 30 * time.Second

// Empty creates a database with no tables for the test and returns its URL. The database is dropped when the test ends. Point
// DATABASE_URL at a throwaway server only, because a run that is killed leaves its database behind.
func Empty(t *testing.T) string {
	t.Helper()

	base := os.Getenv("DATABASE_URL")
	if base == "" {
		// a CI run without a server would pass with every database test skipped, so it fails instead
		if os.Getenv("CI") != "" {
			t.Fatal("DATABASE_URL is not set in CI, so the database tests would not run")
		}

		t.Skip("DATABASE_URL is not set, so the test has no database")
	}

	adminURL, err := url.Parse(base)
	if err != nil {
		t.Fatal("DATABASE_URL is not a valid URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to the test server %v", err)
	}

	defer func() { _ = admin.Close(ctx) }()

	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("make a name %v", err)
	}

	name := "t_" + hex.EncodeToString(raw)
	quoted := pgx.Identifier{name}.Sanitize()

	// the cleanup is registered before the database exists, so a failure further on still drops it
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), setupTimeout)
		defer dropCancel()

		conn, err := pgx.Connect(dropCtx, base)
		if err != nil {
			t.Errorf("connect to drop the test database %v", err)

			return
		}

		defer func() { _ = conn.Close(dropCtx) }()

		if _, err := conn.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the test database %v", err)
		}
	})

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("create the test database %v", err)
	}

	adminURL.Path = "/" + name

	// a database or dbname parameter overrides the path in pgx, so the test would use the shared database
	query := adminURL.Query()
	query.Del("database")
	query.Del("dbname")
	adminURL.RawQuery = query.Encode()

	return adminURL.String()
}

// New creates a database for the test, migrates it and returns a pool on it.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dbURL := Empty(t)

	if err := database.Migrate(dbURL); err != nil {
		t.Fatalf("migrate the test database %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	pool, err := database.Open(ctx, dbURL, 8)
	if err != nil {
		t.Fatalf("open the test database %v", err)
	}

	t.Cleanup(pool.Close)

	return pool
}
