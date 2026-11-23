package ticket

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
)

const rollbackTimeout = 5 * time.Second

// Store reads and writes the tables of the tickets. It takes the time from a function, so a test passes its own clock.
type Store struct {
	db     catalog.DB
	codec  *pagination.Codec
	now    func() time.Time
	limits Limits
}

// NewStore returns a store. The signer makes and checks the cursors. A nil clock means time.Now.
func NewStore(db catalog.DB, signer pagination.Signer, now func() time.Time, limits Limits) *Store {
	if now == nil {
		now = time.Now
	}

	return &Store{db: db, codec: pagination.NewCodec(signer), now: now, limits: limits}
}

func (s *Store) moment() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// inTx runs fn in a transaction. A failed or cancelled run rolls back on a context of its own, so the rollback is not cut short.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the transaction %w", err)
	}

	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()

		_ = tx.Rollback(rollbackCtx)
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the transaction %w", err)
	}

	return nil
}

// record stores the answer of an idempotent request in the transaction of the write.
func record(ctx context.Context, tx pgx.Tx, rec catalog.Recorder, status int, body any) error {
	if rec == nil {
		return nil
	}

	if err := rec.Record(ctx, tx, status, body); err != nil {
		return fmt.Errorf("record the answer %w", err)
	}

	return nil
}

// move is a guarded change of status. The update names the expected old statuses and, when Version is set, the expected version, and
// it changes no row when either differs. The same statement raises the version and keeps the note of a decision.
type move struct {
	ID      string
	From    []string
	To      string
	Actor   string
	AdminID *string
	// Version is the version that the client read, and nil skips the check, as the cancel of a guest does.
	Version *int
	// Note is stored as the decision note and in the history entry. A nil note leaves the decision note as it is.
	Note *string
}

// moveStatus applies the guarded change and writes the history entry in the same transaction. It returns false when no row matched,
// and the caller reads the ticket again to say why.
func (s *Store) moveStatus(ctx context.Context, tx pgx.Tx, m move) (bool, error) {
	at := s.moment()

	tag, err := tx.Exec(ctx, `
		update tickets set status = $3, version = version + 1, decision_note = coalesce($4::text, decision_note)
		where id = $1::uuid and status = any($2::text[]) and ($5::int is null or version = $5::int)`,
		m.ID, m.From, m.To, m.Note, m.Version)
	if err != nil {
		return false, fmt.Errorf("change the status %w", err)
	}

	if tag.RowsAffected() != 1 {
		return false, nil
	}

	_, err = tx.Exec(ctx, `
		insert into ticket_history (ticket_id, at, status, actor_kind, admin_id, note) values ($1::uuid, $2, $3, $4, $5::uuid, $6)`,
		m.ID, at, m.To, m.Actor, m.AdminID, m.Note)
	if err != nil {
		return false, fmt.Errorf("write the history %w", err)
	}

	return true, nil
}
