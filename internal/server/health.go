package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
)

const (
	readyTimeout = 2 * time.Second
	// readyCache is how long an answer is reused, so a flood of probes costs one ping and not one pool connection for each probe.
	readyCache = 2 * time.Second
)

// Pinger is the part of the database pool that the readiness route needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PoolStats is how many connections of the pool are in use.
type PoolStats struct {
	Acquired, Max int32
}

type dbState int

const (
	dbDown dbState = iota
	dbOK
	// dbBusy is a ping that timed out while every connection of the pool was in use. The database is not down, and a probe that took
	// both copies out of service under load would turn a slowdown into an outage.
	dbBusy
)

type readiness struct {
	db    Pinger
	stats func() PoolStats
	ttl   time.Duration
	now   func() time.Time

	draining atomic.Bool

	mu      sync.Mutex
	checked time.Time
	state   dbState
}

func (r *readiness) ready() bool {
	if r.draining.Load() {
		return false
	}

	return r.database() != dbDown
}

func (r *readiness) database() dbState {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.checked.IsZero() && r.now().Sub(r.checked) < r.ttl {
		return r.state
	}

	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	err := r.db.Ping(ctx)

	cancel()

	switch {
	case err == nil:
		r.state = dbOK
	case r.saturated():
		slog.Warn("the database check timed out while every connection of the pool is in use", "error", err)

		r.state = dbBusy
	default:
		slog.Warn("the database check failed", "error", err)

		r.state = dbDown
	}

	r.checked = r.now()

	return r.state
}

func (r *readiness) saturated() bool {
	if r.stats == nil {
		return false
	}

	stats := r.stats()

	return stats.Max > 0 && stats.Acquired >= stats.Max
}

type health struct {
	Status string `json:"status"`
}

// Health serves the health routes on the internal port.
type Health struct {
	app   *fiber.App
	ready *readiness
}

// NewHealth returns the health routes. The route /healthz reads nothing. The route /readyz answers from a short lived result of a ping
// and says ready while the pool is saturated. The stats function may be nil.
func NewHealth(db Pinger, stats func() PoolStats) *Health {
	return newHealth(db, stats, readyCache, time.Now)
}

func newHealth(db Pinger, stats func() PoolStats, ttl time.Duration, now func() time.Time) *Health {
	h := &Health{app: newApp(nil), ready: &readiness{db: db, stats: stats, ttl: ttl, now: now}}

	h.app.Use(RequestID(), NoStore(), Recover())

	h.app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(health{Status: "ok"})
	})

	h.app.Get("/readyz", func(c fiber.Ctx) error {
		if !h.ready.ready() {
			return c.Status(http.StatusServiceUnavailable).JSON(health{Status: "unavailable"})
		}

		return c.JSON(health{Status: "ready"})
	})

	return h
}

// App is the application to listen with.
func (h *Health) App() *fiber.App {
	return h.app
}

// Drain makes /readyz answer 503 from now on, so the proxy stops sending requests while the API finishes the ones it has.
func (h *Health) Drain() {
	h.ready.draining.Store(true)
}
