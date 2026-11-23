package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
)

const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"

	rollbackTimeout = 5 * time.Second
)

// The constraints that a write can break on purpose, which the functions map to an answer of the contract.
const (
	categoryNameIndex = "categories_name_lower_key"
	modelNameIndex    = "models_name_lower_key"
	modelPrefixKey    = "models_prefix_key"
	unitSerialIndex   = "units_serial_lower_key"
)

// The statuses of a ticket that hold stock, and the statuses of an open ticket. docs/data-model.md names both lists.
const (
	holdingStatuses = `'disetujui', 'siap_diambil', 'dipinjam'`
	openStatuses    = `'diajukan', 'disetujui', 'siap_diambil', 'dipinjam'`
)

// Store reads and writes the tables of the assets. It takes the time from a function, so a test passes its own clock.
type Store struct {
	db    DB
	codec *pagination.Codec
	now   func() time.Time
}

// NewStore returns a store. The signer makes and checks the cursors. A nil clock means time.Now.
func NewStore(db DB, signer pagination.Signer, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}

	return &Store{db: db, codec: pagination.NewCodec(signer), now: now}
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
func record(ctx context.Context, tx pgx.Tx, rec Recorder, status int, body any) error {
	if rec == nil {
		return nil
	}

	if err := rec.Record(ctx, tx, status, body); err != nil {
		return fmt.Errorf("record the answer %w", err)
	}

	return nil
}

// violation returns the SQLSTATE and the constraint of a PostgreSQL error in the chain, or empty texts.
func violation(err error) (code, constraint string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName
	}

	return "", ""
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}

	_, err := uuid.Parse(s)

	return err == nil
}

// LockModels locks the rows of the models with one statement that sorts by id. Every transaction that changes stock locks the models
// this way, so two of them that touch the same models in opposite order cannot wait for each other. It returns the number of rows that
// it locked, which is lower than the number of distinct ids when a model does not exist.
func LockModels(ctx context.Context, q Querier, ids ...string) (int, error) {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	rows, err := q.Query(ctx, `select id from models where id = any($1::text[]::uuid[]) order by id for update`, sorted)
	if err != nil {
		return 0, fmt.Errorf("lock the models %w", err)
	}

	defer rows.Close()

	locked := 0
	for rows.Next() {
		locked++
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("lock the models %w", err)
	}

	return locked, nil
}

// LockModelsShared takes a share lock on the rows of the models, in the order of the id, and returns for each model that exists
// whether it is retired. A transaction that creates a ticket takes this lock, so it never runs together with a change of stock that
// takes the update lock of LockModels, and creations of tickets do not wait for each other. The output column has its own name, so
// order by id sorts by the uuid column as LockModels does and not by the text. A model that does not exist is missing from the result.
func LockModelsShared(ctx context.Context, q Querier, ids ...string) (map[string]bool, error) {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	rows, err := q.Query(ctx, `select id::text as model_id, retired_at is not null from models where id = any($1::text[]::uuid[]) order by id for share`, sorted)
	if err != nil {
		return nil, fmt.Errorf("lock the models for share %w", err)
	}

	defer rows.Close()

	retired := map[string]bool{}

	for rows.Next() {
		var (
			id   string
			gone bool
		)

		if err := rows.Scan(&id, &gone); err != nil {
			return nil, fmt.Errorf("read a locked model %w", err)
		}

		retired[id] = gone
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock the models for share %w", err)
	}

	return retired, nil
}

// Available returns the available quantity of each model that exists, which is its stock minus the quantity that its tickets hold and
// never below 0. The caller holds a lock on the models when the answer has to stay true until it commits.
func Available(ctx context.Context, q Querier, ids ...string) (map[string]int, error) {
	rows, err := q.Query(ctx, `
		select m.id::text,
		       (select count(*) from units u where u.model_id = m.id and u.lifecycle = 'active')::int,
		       coalesce((select sum(ti.approved_qty) from ticket_items ti join tickets t on t.id = ti.ticket_id
		                 where ti.model_id = m.id and t.status in (`+holdingStatuses+`)), 0)::int
		from models m where m.id = any($1::text[]::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the available quantities %w", err)
	}

	defer rows.Close()

	available := map[string]int{}

	for rows.Next() {
		var (
			id          string
			total, held int
		)

		if err := rows.Scan(&id, &total, &held); err != nil {
			return nil, fmt.Errorf("read an available quantity %w", err)
		}

		available[id] = max(total-held, 0)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the available quantities %w", err)
	}

	return available, nil
}

// lockedModel is a model row that the transaction holds.
type lockedModel struct {
	ID      string
	Prefix  string
	Next    int
	Retired bool
}

// lockModel locks one model row and reads what a stock change needs. A missing model gives ErrNotFound.
func lockModel(ctx context.Context, q Querier, id string) (lockedModel, error) {
	var m lockedModel

	err := q.QueryRow(ctx, `select id::text, prefix, next_unit_number, retired_at is not null from models where id = $1::uuid for update`, id).
		Scan(&m.ID, &m.Prefix, &m.Next, &m.Retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedModel{}, ErrNotFound
	}

	if err != nil {
		return lockedModel{}, fmt.Errorf("lock the model %w", err)
	}

	return m, nil
}

// stockOf returns the stock of a model, which is the count of its active units, and the quantity that its tickets hold.
func stockOf(ctx context.Context, q Querier, modelID string) (total, held int, err error) {
	err = q.QueryRow(ctx, `
		select (select count(*) from units u where u.model_id = $1::uuid and u.lifecycle = 'active')::int,
		       coalesce((select sum(ti.approved_qty) from ticket_items ti join tickets t on t.id = ti.ticket_id
		                 where ti.model_id = $1::uuid and t.status in (`+holdingStatuses+`)), 0)::int`, modelID).Scan(&total, &held)
	if err != nil {
		return 0, 0, fmt.Errorf("read the stock %w", err)
	}

	return total, held, nil
}

func timeText(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func timeTextPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}

	text := timeText(*t)

	return &text
}
