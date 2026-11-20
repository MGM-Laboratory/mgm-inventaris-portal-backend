// Package server builds the HTTP applications of the service. One serves the API and a second one serves the health routes on an
// internal port that the proxy does not route.
package server

import (
	"net/netip"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
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

func newApp(trusted []netip.Prefix) *fiber.App {
	cfg := fiber.Config{
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
		BodyLimit:    bodyLimit,
		ErrorHandler: ErrorHandler,
	}

	// X-Real-IP counts only when the peer is in the list of the settings, as docs/api.md says
	if len(trusted) > 0 {
		proxies := make([]string, 0, len(trusted))
		for _, prefix := range trusted {
			proxies = append(proxies, prefix.String())
		}

		cfg.TrustProxy = true
		cfg.TrustProxyConfig = fiber.TrustProxyConfig{Proxies: proxies}
		cfg.ProxyHeader = "X-Real-IP"
		cfg.EnableIPValidation = true
	}

	return fiber.New(cfg)
}

// NewAPI returns the application of the API with its middleware and its routes.
func NewAPI(cfg config.Config, deps Deps) *fiber.App {
	app := newApp(cfg.TrustedProxies)

	app.Use(RequestID(), NoStore(), AccessLog(), Recover())
	app.Use(LoadSession(deps.Sessions, session.CookieName(cfg.SecureCookie)), OriginAndCSRF(cfg.PublicBaseURL))

	app.Get("/api/config", configHandler(cfg))

	app.Post("/api/session/guest", startGuestSession(cfg, deps))
	app.Get("/api/session", RequireSession(), getSession())
	app.Delete("/api/session", RequireSession(), deleteSession(cfg, deps))

	app.Post("/api/admin/session", signInAdmin(cfg, deps))
	app.Get("/api/admin/me", RequireAdmin(), getAdminMe(deps))
	app.Post("/api/admin/me/password", RequireAdminAccount(), changeAdminPassword(cfg, deps))
	app.Post("/api/admin/me/totp", RequireAdminAccount(), enrollAdminTotp(cfg, deps))
	app.Post("/api/admin/me/recovery-codes", RequireAdmin(), renewAdminRecoveryCodes(cfg, deps))

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
