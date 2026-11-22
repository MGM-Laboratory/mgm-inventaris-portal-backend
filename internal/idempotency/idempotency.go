// Package idempotency lets a client repeat a request that creates something without creating it twice. A key counts for one session
// and one route. The first request claims the key and the answer is stored in the same transaction as the work, so a stored answer
// always means that the work was done. A repeat gets the stored answer and its new body is ignored.
package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/errchain"
)

const (
	// Retention is how long a key and its answer are kept.
	Retention = 24 * time.Hour
	// staleAfter is how long a claim may run. The work of one request ends well within it, so an older claim belongs to a copy that
	// died, and a new request takes the key over.
	staleAfter = 2 * time.Minute

	releaseTimeout = 5 * time.Second
	maxClaimTries  = 3
)

var (
	// ErrInProgress means that another request holds the key and has not finished.
	ErrInProgress = errors.New("a request with this key still runs")
	// ErrLost means that the claim was taken over before the answer was stored, so the work must not commit.
	ErrLost = errors.New("the claim of the key was lost")
)

var keyShape = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// ValidKey says whether the text has the shape that the contract allows for an Idempotency-Key.
func ValidKey(value string) bool {
	return keyShape.MatchString(value)
}

// DB is the part of a pool that the store needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Key names one request. Session is the hash of the session token, and Operation names the route.
type Key struct {
	Session   []byte
	Operation string
	Value     string
}

// Answer is a stored answer.
type Answer struct {
	Status int
	Body   []byte
}

// Claim is the right to do the work of one key. The work calls Record inside its transaction. A request that ends with an error calls
// Release, so a repeat can run the work again.
type Claim interface {
	Record(ctx context.Context, tx pgx.Tx, status int, body any) error
	Release(ctx context.Context)
}

// Store claims keys. It takes the time from a function, so a test passes its own clock.
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

func (s *Store) moment() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// Begin claims the key. It returns a claim when the caller does the work, the stored answer when an earlier request finished, and
// ErrInProgress when an earlier request still runs. A key older than Retention, and a claim older than staleAfter, are taken over.
func (s *Store) Begin(ctx context.Context, key Key) (Claim, *Answer, error) {
	for range maxClaimTries {
		now := s.moment()

		tag, err := s.db.Exec(ctx, `
			insert into idempotency_keys (session_hash, operation, key, state, created_at)
			values ($1, $2, $3, 'running', $4)
			on conflict (session_hash, operation, key) do update
			set state = 'running', status_code = null, response = null, created_at = $4
			where idempotency_keys.created_at <= $5
			   or (idempotency_keys.state = 'running' and idempotency_keys.created_at <= $6)`,
			key.Session, key.Operation, key.Value, now, now.Add(-Retention), now.Add(-staleAfter))
		if err != nil {
			return nil, nil, fmt.Errorf("claim the idempotency key %w", err)
		}

		if tag.RowsAffected() == 1 {
			return &claim{store: s, key: key, claimedAt: now}, nil, nil
		}

		var (
			state  string
			status *int
			body   []byte
		)

		err = s.db.QueryRow(ctx, `
			select state, status_code, response from idempotency_keys where session_hash = $1 and operation = $2 and key = $3`,
			key.Session, key.Operation, key.Value).Scan(&state, &status, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			// the key was released or swept between the two statements
			continue
		}

		if err != nil {
			return nil, nil, fmt.Errorf("read the idempotency key %w", err)
		}

		if state == "done" && status != nil {
			return nil, &Answer{Status: *status, Body: body}, nil
		}

		return nil, nil, ErrInProgress
	}

	return nil, nil, ErrInProgress
}

type claim struct {
	store     *Store
	key       Key
	claimedAt time.Time
}

// Record stores the answer. The caller runs it in the transaction of the work, before the commit.
func (c *claim) Record(ctx context.Context, tx pgx.Tx, status int, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode the stored answer %w", err)
	}

	tag, err := tx.Exec(ctx, `
		update idempotency_keys set state = 'done', status_code = $4, response = $5
		where session_hash = $1 and operation = $2 and key = $3 and state = 'running' and created_at = $6`,
		c.key.Session, c.key.Operation, c.key.Value, status, raw, c.claimedAt)
	if err != nil {
		return fmt.Errorf("store the answer %w", err)
	}

	if tag.RowsAffected() != 1 {
		return ErrLost
	}

	return nil
}

// Release gives the key back after a request that stored no answer. It runs on a context that the caller cannot cancel. A failure is
// logged, and the claim then ends by itself after staleAfter.
func (c *claim) Release(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	_, err := c.store.db.Exec(ctx, `
		delete from idempotency_keys
		where session_hash = $1 and operation = $2 and key = $3 and state = 'running' and created_at = $4`,
		c.key.Session, c.key.Operation, c.key.Value, c.claimedAt)
	if err != nil {
		slog.Warn("an idempotency key could not be released", "error_chain", errchain.Chain(err))
	}
}

// DeleteExpired removes the keys that are older than Retention at the moment now. The janitor calls it.
func DeleteExpired(ctx context.Context, db DB, now time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `delete from idempotency_keys where created_at <= $1`, now.UTC().Add(-Retention))
	if err != nil {
		return 0, fmt.Errorf("delete the expired idempotency keys %w", err)
	}

	return tag.RowsAffected(), nil
}
