package database_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
)

const queryTimeout = 20 * time.Second

var schemaTables = []string{
	"admin_recovery_codes", "admins", "audit_log", "categories", "idempotency_keys", "models", "rate_counters", "sessions",
	"ticket_counters", "ticket_history", "ticket_items", "ticket_units", "tickets", "unit_events", "units",
}

func tableNames(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()

	rows, err := pool.Query(ctx, `select table_name from information_schema.tables
		where table_schema = 'public' and table_name <> 'schema_migrations' order by table_name`)
	if err != nil {
		t.Fatalf("list the tables %v", err)
	}

	defer rows.Close()

	var names []string

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("read a table name %v", err)
		}

		names = append(names, name)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("read the tables %v", err)
	}

	return names
}

func TestMigrateUpAndDown(t *testing.T) {
	dbURL := dbtest.Empty(t)

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	if err := database.Migrate(dbURL); err != nil {
		t.Fatalf("migrate up %v", err)
	}

	// a second run finds nothing to do
	if err := database.Migrate(dbURL); err != nil {
		t.Fatalf("migrate up again %v", err)
	}

	pool, err := database.Open(ctx, dbURL, 2)
	if err != nil {
		t.Fatalf("open %v", err)
	}

	defer pool.Close()

	if got := tableNames(t, ctx, pool); !slices.Equal(got, schemaTables) {
		t.Fatalf("tables are %v, want %v", got, schemaTables)
	}

	if err := database.MigrateDown(dbURL); err != nil {
		t.Fatalf("migrate down %v", err)
	}

	if got := tableNames(t, ctx, pool); len(got) != 0 {
		t.Fatalf("tables left after the down migration %v", got)
	}

	if err := database.Migrate(dbURL); err != nil {
		t.Fatalf("migrate up after down %v", err)
	}
}

func TestMigrateFromTwoCopies(t *testing.T) {
	dbURL := dbtest.Empty(t)

	var wg sync.WaitGroup

	errs := make([]error, 2)

	for i := range errs {
		wg.Add(1)

		go func() {
			defer wg.Done()

			errs[i] = database.Migrate(dbURL)
		}()
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("a copy failed to migrate %v", err)
		}
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	_, err := pool.Exec(ctx, sql, args...)

	return err
}

func scanString(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	var out string
	if err := pool.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
		t.Fatalf("query failed %v", err)
	}

	return out
}

// violation says whether err is the PostgreSQL error with the given SQLSTATE and constraint
func violation(err error, code, constraint string) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
}

const (
	uniqueViolation = "23505"
	checkViolation  = "23514"
)

func newModel(t *testing.T, pool *pgxpool.Pool, name, prefix string) string {
	t.Helper()

	category := scanString(t, pool, `insert into categories (name, tone) values ($1, 'blue')
		on conflict (lower(name)) do update set name = excluded.name returning id::text`, "Cat "+prefix)

	return scanString(t, pool, `insert into models (name, category_id, prefix) values ($1, $2, $3) returning id::text`,
		name, category, prefix)
}

func newUnit(t *testing.T, pool *pgxpool.Pool, modelID, tag string) string {
	t.Helper()

	return scanString(t, pool, `insert into units (tag, model_id, condition, place) values ($1, $2, 'Good', 'Shelf A') returning id::text`,
		tag, modelID)
}

func newTicket(t *testing.T, pool *pgxpool.Pool, number string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	var id string

	err := pool.QueryRow(ctx, `insert into tickets
		(number, requester_name, requester_nim_nip, requester_email, purpose, pickup_date, duration_days)
		values ($1, 'Example Borrower', '000000000000', 'example@student.ub.ac.id', 'Example loan', current_date, 3)
		returning id::text`, number).Scan(&id)

	return id, err
}

func TestOpenUnitIsHeldOnce(t *testing.T) {
	pool := dbtest.New(t)

	model := newModel(t, pool, "Headset", "HDS")
	unit := newUnit(t, pool, model, "MGM-HDS-001")

	first, err := newTicket(t, pool, "MGM-2611-0001-AAAA")
	if err != nil {
		t.Fatalf("first ticket %v", err)
	}

	second, err := newTicket(t, pool, "MGM-2611-0002-BBBB")
	if err != nil {
		t.Fatalf("second ticket %v", err)
	}

	if err := exec(t, pool, `insert into ticket_units (ticket_id, unit_id) values ($1, $2)`, first, unit); err != nil {
		t.Fatalf("first hold %v", err)
	}

	err = exec(t, pool, `insert into ticket_units (ticket_id, unit_id) values ($1, $2)`, second, unit)
	if !violation(err, uniqueViolation, "ticket_units_open_unit_key") {
		t.Fatalf("a second open hold of one unit gave %v", err)
	}

	// once the first row is closed, another ticket can hold the unit
	if err := exec(t, pool, `update ticket_units set returned_at = now(), return_outcome = 'ok' where ticket_id = $1`, first); err != nil {
		t.Fatalf("close the first hold %v", err)
	}

	if err := exec(t, pool, `insert into ticket_units (ticket_id, unit_id) values ($1, $2)`, second, unit); err != nil {
		t.Fatalf("hold after a return %v", err)
	}
}

func TestReturnNeedsAnOutcome(t *testing.T) {
	pool := dbtest.New(t)

	model := newModel(t, pool, "Headset", "HDS")
	unit := newUnit(t, pool, model, "MGM-HDS-001")

	ticket, err := newTicket(t, pool, "MGM-2611-0001-AAAA")
	if err != nil {
		t.Fatalf("ticket %v", err)
	}

	err = exec(t, pool, `insert into ticket_units (ticket_id, unit_id, returned_at) values ($1, $2, now())`, ticket, unit)
	if !violation(err, checkViolation, "ticket_units_return_is_complete") {
		t.Fatalf("a return with no outcome gave %v", err)
	}
}

func TestNamesAndSerialsAreUniqueIgnoringCase(t *testing.T) {
	pool := dbtest.New(t)

	if err := exec(t, pool, `insert into categories (name, tone) values ('Audio', 'blue')`); err != nil {
		t.Fatalf("category %v", err)
	}

	err := exec(t, pool, `insert into categories (name, tone) values ('AUDIO', 'red')`)
	if !violation(err, uniqueViolation, "categories_name_lower_key") {
		t.Fatalf("a category name in another case gave %v", err)
	}

	model := newModel(t, pool, "Headset", "HDS")

	err = exec(t, pool, `insert into models (name, category_id, prefix)
		select 'headset', category_id, 'HDT' from models where id = $1`, model)
	if !violation(err, uniqueViolation, "models_name_lower_key") {
		t.Fatalf("a model name in another case gave %v", err)
	}

	err = exec(t, pool, `insert into models (name, category_id, prefix)
		select 'Other', category_id, 'HDS' from models where id = $1`, model)
	if !violation(err, uniqueViolation, "models_prefix_key") {
		t.Fatalf("a repeated prefix gave %v", err)
	}

	err = exec(t, pool, `insert into models (name, category_id, prefix)
		select 'Lower', category_id, 'hds1' from models where id = $1`, model)
	if !violation(err, checkViolation, "models_prefix_shape") {
		t.Fatalf("a lower case prefix gave %v", err)
	}

	if err := exec(t, pool, `insert into units (tag, model_id, serial, condition, place) values ('MGM-HDS-001', $1, 'SN-abc-1', 'Good', 'Shelf')`, model); err != nil {
		t.Fatalf("unit with a serial %v", err)
	}

	err = exec(t, pool, `insert into units (tag, model_id, serial, condition, place) values ('MGM-HDS-002', $1, 'sn-ABC-1', 'Good', 'Shelf')`, model)
	if !violation(err, uniqueViolation, "units_serial_lower_key") {
		t.Fatalf("a serial in another case gave %v", err)
	}

	// units without a serial never clash
	for _, tag := range []string{"MGM-HDS-003", "MGM-HDS-004"} {
		if err := exec(t, pool, `insert into units (tag, model_id, condition, place) values ($1, $2, 'Good', 'Shelf')`, tag, model); err != nil {
			t.Fatalf("unit without a serial %v", err)
		}
	}
}

func TestTicketItemRules(t *testing.T) {
	pool := dbtest.New(t)

	model := newModel(t, pool, "Headset", "HDS")

	ticket, err := newTicket(t, pool, "MGM-2611-0001-AAAA")
	if err != nil {
		t.Fatalf("ticket %v", err)
	}

	err = exec(t, pool, `insert into ticket_items (ticket_id, model_id, qty, approved_qty) values ($1, $2, 2, 3)`, ticket, model)
	if !violation(err, checkViolation, "ticket_items_approved_qty_range") {
		t.Fatalf("an approved quantity above the request gave %v", err)
	}

	if err := exec(t, pool, `insert into ticket_items (ticket_id, model_id, qty, approved_qty) values ($1, $2, 2, 2)`, ticket, model); err != nil {
		t.Fatalf("an approved quantity equal to the request %v", err)
	}

	err = exec(t, pool, `insert into ticket_items (ticket_id, model_id, qty) values ($1, $2, 1)`, ticket, model)
	if !violation(err, uniqueViolation, "ticket_items_pkey") {
		t.Fatalf("a model twice in one ticket gave %v", err)
	}
}

func TestConcurrentTicketNumbersAreRefused(t *testing.T) {
	pool := dbtest.New(t)

	const writers = 6

	var wg sync.WaitGroup

	errs := make([]error, writers)

	for i := range errs {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, errs[i] = newTicket(t, pool, "MGM-2611-0007-CCCC")
		}()
	}

	wg.Wait()

	var created int

	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case !violation(err, uniqueViolation, "tickets_number_key"):
			t.Fatalf("unexpected error %v", err)
		}
	}

	if created != 1 {
		t.Fatalf("%d inserts of one number succeeded, want 1", created)
	}
}

func TestAnonymizedTicketKeepsItsShape(t *testing.T) {
	pool := dbtest.New(t)

	ticket, err := newTicket(t, pool, "MGM-2611-0001-AAAA")
	if err != nil {
		t.Fatalf("ticket %v", err)
	}

	// the retention job sets anonymized_at and clears the requester in one statement
	err = exec(t, pool, `update tickets set requester_name = null where id = $1`, ticket)
	if !violation(err, checkViolation, "tickets_requester_kept_until_anonymized") {
		t.Fatalf("a cleared name without anonymized_at gave %v", err)
	}

	err = exec(t, pool, `update tickets set requester_name = null, requester_nim_nip = null, requester_email = null,
		purpose = null, decision_note = null, anonymized_at = now() where id = $1`, ticket)
	if err != nil {
		t.Fatalf("anonymize %v", err)
	}
}
