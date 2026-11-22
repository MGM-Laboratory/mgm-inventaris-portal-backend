package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/errchain"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ratelimit"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

// dbTimeout bounds the database work of one route, on top of the limits of the pool connection.
const dbTimeout = 10 * time.Second

// SessionStore is the part of the session package that the routes use.
type SessionStore interface {
	CreateGuest(ctx context.Context, identity *session.Identity, previous []byte) (string, session.Session, error)
	CreateAdmin(ctx context.Context, admin session.Admin, kind session.Kind, previous []byte, endOthers bool) (string, session.Session, error)
	Load(ctx context.Context, token string) (session.Session, error)
	Delete(ctx context.Context, tokenHash []byte) error
}

// RateLimiter is the part of the ratelimit package that the routes use.
type RateLimiter interface {
	Hit(ctx context.Context, key string, limit int, window time.Duration) (ratelimit.Result, error)
	Refund(ctx context.Context, key string, start time.Time) error
}

// Deps is what the routes need from outside the package.
type Deps struct {
	Sessions    SessionStore
	Limiter     RateLimiter
	Admins      AdminService
	Catalog     CatalogService
	Idempotency Idempotency
}

// sessionState is what LoadSession found for the request.
type sessionState struct {
	current *session.Session
	// cookieSent is true when the request carried a session cookie, whether or not it matched a live session.
	cookieSent bool
	// loadErr is the database error of the lookup. A guard answers 503 for an outage and 500 for a bug, and not 401.
	loadErr error
}

type sessionStateKey struct{}

func stateOf(c fiber.Ctx) *sessionState {
	state, _ := c.Locals(sessionStateKey{}).(*sessionState)
	if state == nil {
		return &sessionState{}
	}

	return state
}

// currentSession returns the session of the request, or nil when it has none.
func currentSession(c fiber.Ctx) *session.Session {
	return stateOf(c).current
}

// LoadSession finds the session of the cookie and puts it in the context of the request. It never fails a request. A request
// without a live session goes on, and the guards of the route decide what that means.
func LoadSession(store SessionStore, cookieName string) fiber.Handler {
	return func(c fiber.Ctx) error {
		state := &sessionState{}
		c.Locals(sessionStateKey{}, state)

		token := c.Cookies(cookieName)
		if token == "" {
			return c.Next()
		}

		state.cookieSent = true

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		sess, err := store.Load(ctx, token)

		cancel()

		switch {
		case err == nil:
			state.current = &sess
		case errors.Is(err, session.ErrNoSession):
		default:
			state.loadErr = err

			if databaseUnreachable(err) {
				slog.Warn("the session could not be loaded", "request_id", requestID(c), "error_chain", errchain.Chain(err))
			}
		}

		return c.Next()
	}
}

// requireSession returns the session of the request, or the error that the guards answer when there is none. A cookie that matches no
// live session means the session expired, and a request with no cookie means nobody signed in.
func requireSession(c fiber.Ctx) (*session.Session, error) {
	state := stateOf(c)

	switch {
	case state.current != nil:
		return state.current, nil
	case state.loadErr != nil:
		return nil, dbError("load the session", state.loadErr)
	case state.cookieSent:
		return nil, NewError(CodeSessionExpired)
	default:
		return nil, NewError(CodeUnauthenticated)
	}
}

// RequireSession lets a request with any live session through and answers 401 to the others.
func RequireSession() fiber.Handler {
	return func(c fiber.Ctx) error {
		if _, err := requireSession(c); err != nil {
			return err
		}

		return c.Next()
	}
}

// RequireGuest needs a guest session, with or without an identity. Another kind answers 403 forbidden.
func RequireGuest() fiber.Handler {
	return func(c fiber.Ctx) error {
		sess, err := requireSession(c)
		if err != nil {
			return err
		}

		if sess.Kind != session.KindGuest {
			return NewError(CodeForbidden)
		}

		return c.Next()
	}
}

// RequireIdentity needs a guest session that holds an identity. A guest session without one answers 403 identity_required.
func RequireIdentity() fiber.Handler {
	return func(c fiber.Ctx) error {
		sess, err := requireSession(c)
		if err != nil {
			return err
		}

		if sess.Kind != session.KindGuest {
			return NewError(CodeForbidden)
		}

		if sess.Identity == nil {
			return NewError(CodeIdentityRequired)
		}

		return c.Next()
	}
}

// RequireAdmin needs a full admin session. A setup session of an admin is not one.
func RequireAdmin() fiber.Handler {
	return func(c fiber.Ctx) error {
		sess, err := requireSession(c)
		if err != nil {
			return err
		}

		if sess.Kind != session.KindAdmin {
			return NewError(CodeForbidden)
		}

		return c.Next()
	}
}
