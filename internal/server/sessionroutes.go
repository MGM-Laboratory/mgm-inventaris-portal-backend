package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

const guestSigninWindow = time.Hour

// The JSON of the schemas Session and GuestIdentity in openapi.yaml.
type identityBody struct {
	Name   string `json:"name"`
	NimNip string `json:"nimNip"`
	Email  string `json:"email"`
}

type adminBody struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type sessionBody struct {
	Kind          session.Kind  `json:"kind"`
	CSRFToken     string        `json:"csrfToken"`
	ExpiresAt     string        `json:"expiresAt"`
	IdleExpiresAt string        `json:"idleExpiresAt"`
	Identity      *identityBody `json:"identity,omitempty"`
	Admin         *adminBody    `json:"admin,omitempty"`
}

func timeBody(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func newSessionBody(s session.Session) sessionBody {
	body := sessionBody{
		Kind:          s.Kind,
		CSRFToken:     s.CSRFToken,
		ExpiresAt:     timeBody(s.ExpiresAt),
		IdleExpiresAt: timeBody(s.IdleExpiresAt()),
	}

	if s.Identity != nil {
		body.Identity = &identityBody{Name: s.Identity.Name, NimNip: s.Identity.NimNip, Email: s.Identity.Email}
	}

	if s.Admin != nil {
		body.Admin = &adminBody{ID: s.Admin.ID, Name: s.Admin.Name, Email: s.Admin.Email}
	}

	return body
}

func setSessionCookie(c fiber.Ctx, secure bool, token string, maxAge time.Duration) {
	c.Append(fiber.HeaderSetCookie, session.NewCookie(secure, token, maxAge).String())
}

func badBody() *Error {
	e := NewError(CodeBadRequest)
	e.Message = "The body is not valid JSON."

	return e
}

// countGuestSession counts one new guest session for the address of the request against the guest sign in limit. Every route that
// starts a guest session without a form calls it, so none of them is a way around the limit.
func countGuestSession(ctx context.Context, c fiber.Ctx, cfg config.Config, deps Deps) error {
	key := "guest-signin:" + clientAddr(c, cfg.TrustedProxies)

	hit, err := deps.Limiter.Hit(ctx, key, cfg.GuestSigninPerHour, guestSigninWindow)
	if err != nil {
		return dbError("count the guest sign in", err)
	}

	if !hit.Allowed {
		e := NewError(CodeRateLimited)
		e.RetryAfter = hit.RetryAfterSeconds()

		return e
	}

	return nil
}

// The order of the steps of the guest sign in matters. The rate limit counts every request, also one with a bad body, and it runs
// before the body is read. The validation runs before the trap, so a bot that fills the hidden field
// sees the same answers to a bad form as everybody else.
func startGuestSession(cfg config.Config, deps Deps) fiber.Handler {
	limits, _ := session.LimitsFor(session.KindGuest)

	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		if err := countGuestSession(ctx, c, cfg, deps); err != nil {
			return err
		}

		form, err := parseGuestForm(c.Body())
		if err != nil {
			return badBody()
		}

		identity, details := form.identity()
		if len(details) > 0 {
			return NewError(CodeValidationFailed, details...)
		}

		if form.Website.filled() {
			return fakeGuestSession(c, cfg, identity)
		}

		var previous []byte
		if current := currentSession(c); current != nil {
			previous = current.TokenHash
		}

		token, created, err := deps.Sessions.CreateGuest(ctx, &identity, previous)
		if err != nil {
			return dbError("create the guest session", err)
		}

		setSessionCookie(c, cfg.SecureCookie, token, limits.Absolute)

		return c.Status(http.StatusCreated).JSON(newSessionBody(created))
	}
}

// fakeGuestSession answers a request that filled the hidden field. The answer has the shape of a real one and the cookie holds a
// random value, but nothing is stored, so the cookie matches no session. It deletes no session that came with the request.
func fakeGuestSession(c fiber.Ctx, cfg config.Config, identity session.Identity) error {
	token, err := session.NewToken()
	if err != nil {
		return fmt.Errorf("make the token %w", err)
	}

	csrf, err := session.NewToken()
	if err != nil {
		return fmt.Errorf("make the CSRF token %w", err)
	}

	limits, _ := session.LimitsFor(session.KindGuest)
	now := time.Now()

	setSessionCookie(c, cfg.SecureCookie, token, limits.Absolute)

	return c.Status(http.StatusCreated).JSON(newSessionBody(session.Session{
		Kind:       session.KindGuest,
		Identity:   &identity,
		CSRFToken:  csrf,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(limits.Absolute),
	}))
}

// getSession is the route getSession. The guard has found the session already.
func getSession() fiber.Handler {
	return func(c fiber.Ctx) error {
		return c.JSON(newSessionBody(*currentSession(c)))
	}
}

// endSession is the operation of openapi.yaml.
func deleteSession(cfg config.Config, deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		if err := deps.Sessions.Delete(ctx, currentSession(c).TokenHash); err != nil {
			return dbError("delete the session", err)
		}

		c.Append(fiber.HeaderSetCookie, session.ClearCookie(cfg.SecureCookie).String())

		return c.SendStatus(http.StatusNoContent)
	}
}
