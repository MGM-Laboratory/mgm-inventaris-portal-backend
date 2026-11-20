// Package session owns the sessions table. A session is a random token in a cookie, and the database keeps only the SHA-256 hash of
// the token.
package session

import (
	"errors"
	"time"
)

// Kind is the kind of a session, as in the check constraint of the table.
type Kind string

// The kinds of a session.
const (
	KindGuest      Kind = "guest"
	KindAdmin      Kind = "admin"
	KindAdminSetup Kind = "admin_setup"
)

// Limits are the idle and the absolute lifetime of a session. A session that is idle for Idle, or older than Absolute, is gone.
type Limits struct {
	Idle     time.Duration
	Absolute time.Duration
}

// LimitsFor returns the limits of docs/api.md for a kind. The second value is false for a kind that has none.
func LimitsFor(kind Kind) (Limits, bool) {
	switch kind {
	case KindGuest:
		return Limits{Idle: 12 * time.Hour, Absolute: 7 * 24 * time.Hour}, true
	case KindAdmin:
		return Limits{Idle: 30 * time.Minute, Absolute: 8 * time.Hour}, true
	case KindAdminSetup:
		return Limits{Idle: 15 * time.Minute, Absolute: 15 * time.Minute}, true
	default:
		return Limits{}, false
	}
}

// ErrNoSession means that the token matches no live session. An unknown token, an expired one and a token of the wrong shape all
// give it, so a client cannot tell them apart.
var ErrNoSession = errors.New("no session")

// Identity is what a guest typed in the guest form. It is not an account.
type Identity struct {
	Name   string
	NimNip string
	Email  string
}

// Admin is the part of an admin account that a session answer shows.
type Admin struct {
	ID    string
	Name  string
	Email string
}

// Session is a live session. The raw token is never part of it.
type Session struct {
	TokenHash       []byte
	Kind            Kind
	Identity        *Identity
	Admin           *Admin
	CSRFToken       string
	ViewableTickets []string
	CreatedAt       time.Time
	LastSeenAt      time.Time
	ExpiresAt       time.Time
}

// IdleExpiresAt is the moment at which the session ends when nothing uses it, and never later than the absolute limit.
func (s Session) IdleExpiresAt() time.Time {
	limits, ok := LimitsFor(s.Kind)
	if !ok {
		return s.ExpiresAt
	}

	idle := s.LastSeenAt.Add(limits.Idle)
	if idle.After(s.ExpiresAt) {
		return s.ExpiresAt
	}

	return idle
}
