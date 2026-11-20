package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/errchain"
)

// touchInterval is how often a session may write last_seen_at, so a busy client does not write on every request.
const touchInterval = time.Minute

// DB is the part of a pool or a transaction that the store needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store reads and writes the sessions table. It takes the time from a function, so a test passes its own clock.
type Store struct {
	db  DB
	now func() time.Time
}

// NewStore returns a store. A nil clock means time.Now.
func NewStore(db DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}

	return &Store{db: db, now: now}
}

// moment is the clock of the store at the precision of timestamptz, so a value that the store returns equals the value it wrote.
func (s *Store) moment() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// CreateGuest makes a guest session, with or without an identity, and returns the raw token for the cookie. The database holds only
// its hash. When previous is a token hash, the same statement deletes that session, so a sign in never leaves the old one behind.
func (s *Store) CreateGuest(ctx context.Context, identity *Identity, previous []byte) (string, Session, error) {
	token, err := NewToken()
	if err != nil {
		return "", Session{}, err
	}

	csrf, err := NewToken()
	if err != nil {
		return "", Session{}, err
	}

	limits, _ := LimitsFor(KindGuest)
	now := s.moment()
	sess := Session{
		TokenHash:       HashToken(token),
		Kind:            KindGuest,
		Identity:        identity,
		CSRFToken:       csrf,
		ViewableTickets: []string{},
		CreatedAt:       now,
		LastSeenAt:      now,
		ExpiresAt:       now.Add(limits.Absolute),
	}

	var name, nimNip, email *string
	if identity != nil {
		name, nimNip, email = &identity.Name, &identity.NimNip, &identity.Email
	}

	// the data modifying CTE runs once even though nothing reads it, and a null previous matches no row
	_, err = s.db.Exec(ctx, `
		with ended as (delete from sessions where token_hash = $1)
		insert into sessions (token_hash, kind, guest_name, guest_nim_nip, guest_email, csrf_token, created_at, last_seen_at, expires_at)
		values ($2, 'guest', $3, $4, $5, $6, $7, $7, $8)`,
		previous, sess.TokenHash, name, nimNip, email, csrf, now, sess.ExpiresAt)
	if err != nil {
		return "", Session{}, fmt.Errorf("insert the session %w", err)
	}

	return token, sess, nil
}

// CreateAdmin makes a session of the kind admin or admin_setup and returns the raw token for the cookie. One statement deletes the
// session that came with the request, and also every other session of the admin when endOthers is true, so a sign in never leaves
// the old one behind.
func (s *Store) CreateAdmin(ctx context.Context, admin Admin, kind Kind, previous []byte, endOthers bool) (string, Session, error) {
	if kind != KindAdmin && kind != KindAdminSetup {
		return "", Session{}, errors.New("the kind is not an admin kind")
	}

	token, err := NewToken()
	if err != nil {
		return "", Session{}, err
	}

	csrf, err := NewToken()
	if err != nil {
		return "", Session{}, err
	}

	limits, _ := LimitsFor(kind)
	now := s.moment()
	sess := Session{
		TokenHash:       HashToken(token),
		Kind:            kind,
		Admin:           &admin,
		CSRFToken:       csrf,
		ViewableTickets: []string{},
		CreatedAt:       now,
		LastSeenAt:      now,
		ExpiresAt:       now.Add(limits.Absolute),
	}

	// the delete cannot see the row of the insert, because both run on the snapshot of the statement
	_, err = s.db.Exec(ctx, `
		with ended as (delete from sessions where token_hash = $1 or ($8::boolean and admin_id = $3::uuid))
		insert into sessions (token_hash, kind, admin_id, csrf_token, created_at, last_seen_at, expires_at)
		values ($2, $4, $3::uuid, $5, $6, $6, $7)`,
		previous, sess.TokenHash, admin.ID, string(kind), csrf, now, sess.ExpiresAt, endOthers)
	if err != nil {
		return "", Session{}, fmt.Errorf("insert the admin session %w", err)
	}

	return token, sess, nil
}

// DeleteOthers removes every session of an admin except the one with the hash keep, and it takes a pool or a transaction, so the
// owner of the admin table can end the sessions in the transaction that changes a password. A nil keep removes all of them.
func DeleteOthers(ctx context.Context, db DB, adminID string, keep []byte) (int64, error) {
	tag, err := db.Exec(ctx, `delete from sessions where admin_id = $1::uuid and token_hash is distinct from $2::bytea`, adminID, keep)
	if err != nil {
		return 0, fmt.Errorf("delete the other sessions %w", err)
	}

	return tag.RowsAffected(), nil
}

// Load returns the live session of a token, or ErrNoSession. It writes last_seen_at when the last write is a minute old.
func (s *Store) Load(ctx context.Context, token string) (Session, error) {
	if !validToken(token) {
		return Session{}, ErrNoSession
	}

	now := s.moment()
	sess := Session{TokenHash: HashToken(token)}

	var (
		kind                          string
		adminID, adminName, adminMail *string
		name, nimNip, email           *string
	)

	err := s.db.QueryRow(ctx, `
		select s.kind, s.admin_id::text, a.name, a.email, s.guest_name, s.guest_nim_nip, s.guest_email, s.csrf_token,
			s.viewable_tickets::text[], s.created_at, s.last_seen_at, s.expires_at
		from sessions s
		left join admins a on a.id = s.admin_id
		where s.token_hash = $1 and s.expires_at > $2`, sess.TokenHash, now).
		Scan(&kind, &adminID, &adminName, &adminMail, &name, &nimNip, &email, &sess.CSRFToken,
			&sess.ViewableTickets, &sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}

	if err != nil {
		return Session{}, fmt.Errorf("read the session %w", err)
	}

	// an empty array scans as nil, and a later route would then serialize null
	if sess.ViewableTickets == nil {
		sess.ViewableTickets = []string{}
	}

	sess.Kind = Kind(kind)

	limits, ok := LimitsFor(sess.Kind)
	if !ok || !now.Before(sess.LastSeenAt.Add(limits.Idle)) {
		return Session{}, ErrNoSession
	}

	if name != nil && nimNip != nil && email != nil {
		sess.Identity = &Identity{Name: *name, NimNip: *nimNip, Email: *email}
	}

	if adminID != nil && adminName != nil && adminMail != nil {
		sess.Admin = &Admin{ID: *adminID, Name: *adminName, Email: *adminMail}
	}

	if now.Sub(sess.LastSeenAt) >= touchInterval {
		touched, err := s.touch(ctx, sess.TokenHash, now)
		if err != nil {
			// the session is valid, and a missed write only shortens its idle time by a minute
			slog.Warn("the session could not be touched", "error_chain", errchain.Chain(err))
		} else if touched {
			sess.LastSeenAt = now
		}
	}

	return sess, nil
}

// touch writes last_seen_at. The condition makes the once a minute rule hold when two requests race, and it never moves the time back.
func (s *Store) touch(ctx context.Context, hash []byte, now time.Time) (bool, error) {
	tag, err := s.db.Exec(ctx, `update sessions set last_seen_at = $2 where token_hash = $1 and last_seen_at <= $3`,
		hash, now, now.Add(-touchInterval))
	if err != nil {
		return false, fmt.Errorf("update last_seen_at %w", err)
	}

	return tag.RowsAffected() == 1, nil
}

// Delete removes a session by the hash of its token. A session that is already gone is not an error.
func (s *Store) Delete(ctx context.Context, tokenHash []byte) error {
	if _, err := s.db.Exec(ctx, `delete from sessions where token_hash = $1`, tokenHash); err != nil {
		return fmt.Errorf("delete the session %w", err)
	}

	return nil
}

// DeleteExpired removes every session that is past its absolute limit or its idle limit, with the same rules as Load.
func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	now := s.moment()
	guest, _ := LimitsFor(KindGuest)
	admin, _ := LimitsFor(KindAdmin)
	setup, _ := LimitsFor(KindAdminSetup)

	tag, err := s.db.Exec(ctx, `
		delete from sessions
		where expires_at <= $1
			or (kind = 'guest' and last_seen_at <= $2)
			or (kind = 'admin' and last_seen_at <= $3)
			or (kind = 'admin_setup' and last_seen_at <= $4)`,
		now, now.Add(-guest.Idle), now.Add(-admin.Idle), now.Add(-setup.Idle))
	if err != nil {
		return 0, fmt.Errorf("delete the expired sessions %w", err)
	}

	return tag.RowsAffected(), nil
}
