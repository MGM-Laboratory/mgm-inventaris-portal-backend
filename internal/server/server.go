// Package server builds the HTTP applications of the service. One serves the API and a second one serves the health routes on an
// internal port that the proxy does not route.
package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
)

// The limits on a connection. Without them a client that sends its request one byte at a time, or that keeps an idle connection
// open, holds the connection and its buffers for as long as it likes.
const (
	readTimeout  = 15 * time.Second
	writeTimeout = 15 * time.Second
	idleTimeout  = 60 * time.Second
	// bodyLimit is the most that the server reads into memory before a route can check the body. The largest body is a ticket request.
	bodyLimit = 64 << 10
)

// minDurationDays is the shortest loan. The contract shows it in the limits, and it is not a setting.
const minDurationDays = 1

// readyTimeout is how long the readiness route waits for the database.
const readyTimeout = 2 * time.Second

// Pinger is the part of the database pool that the readiness route needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

func newApp(trustedProxy string) *fiber.App {
	cfg := fiber.Config{
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
		BodyLimit:    bodyLimit,
		ErrorHandler: ErrorHandler,
	}

	// X-Real-IP counts only when the request comes from the proxy of the settings, as docs/api.md says
	if trustedProxy != "" {
		cfg.TrustProxy = true
		cfg.TrustProxyConfig = fiber.TrustProxyConfig{Proxies: []string{trustedProxy}}
		cfg.ProxyHeader = "X-Real-IP"
		cfg.EnableIPValidation = true
	}

	return fiber.New(cfg)
}

// NewAPI returns the application of the API with its middleware and its routes.
func NewAPI(cfg config.Config) *fiber.App {
	app := newApp(cfg.TrustedProxy)

	app.Use(RequestID(), NoStore(), AccessLog(), Recover())

	app.Get("/api/config", configHandler(cfg))

	return app
}

type limits struct {
	MaxItemsPerRequest int `json:"maxItemsPerRequest"`
	MinDurationDays    int `json:"minDurationDays"`
	MaxDurationDays    int `json:"maxDurationDays"`
	MaxOpenTickets     int `json:"maxOpenTickets"`
}

type publicConfig struct {
	PublicBaseURL string `json:"publicBaseUrl"`
	Limits        limits `json:"limits"`
}

func configHandler(cfg config.Config) fiber.Handler {
	body := publicConfig{
		PublicBaseURL: cfg.PublicBaseURL,
		Limits: limits{
			MaxItemsPerRequest: cfg.MaxItemsPerRequest,
			MinDurationDays:    minDurationDays,
			MaxDurationDays:    cfg.MaxDurationDays,
			MaxOpenTickets:     cfg.MaxOpenTickets,
		},
	}

	return func(c fiber.Ctx) error {
		return c.JSON(body)
	}
}

type health struct {
	Status string `json:"status"`
}

// NewHealth returns the application of the health routes. The route /healthz reads nothing and the route /readyz pings the database.
func NewHealth(db Pinger) *fiber.App {
	app := newApp("")

	app.Use(RequestID(), NoStore(), Recover())

	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(health{Status: "ok"})
	})

	app.Get("/readyz", func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), readyTimeout)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			slog.Warn("the database check failed", "request_id", requestID(c), "error", err)

			return c.Status(fiber.StatusServiceUnavailable).JSON(health{Status: "unavailable"})
		}

		return c.JSON(health{Status: "ready"})
	})

	return app
}
