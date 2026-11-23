package ticket_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/secretbox"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ticket"
)

var (
	testNow    = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	testLimits = ticket.Limits{MaxItems: 10, MinDays: 1, MaxDays: 30, MaxOpen: 3}
)

// clock moves by one second at every reading, so rows made one after the other have different times and no test waits for time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(time.Second)

	return c.now
}

func ptr[T any](v T) *T {
	return &v
}

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	tickets  *ticket.Store
	catalog  *catalog.Store
	sessions *session.Store
	adminID  string
	raw      int
	people   int
}

// newEnv opens a database for the test and adds one admin. The tests skip without DATABASE_URL.
func newEnv(t *testing.T) *env {
	t.Helper()

	return newEnvLimits(t, testLimits)
}

func newEnvLimits(t *testing.T, limits ticket.Limits) *env {
	t.Helper()

	pool := dbtest.New(t)

	box, err := secretbox.New([]byte(strings.Repeat("k", secretbox.KeyLength)))
	if err != nil {
		t.Fatalf("make the box %v", err)
	}

	clk := &clock{now: testNow}
	e := &env{t: t, pool: pool}
	e.tickets = ticket.NewStore(pool, box, clk.Now, limits)
	e.catalog = catalog.NewStore(pool, box, clk.Now)
	e.sessions = session.NewStore(pool, clk.Now)

	err = pool.QueryRow(context.Background(), `
		insert into admins (email, name, password_hash) values ('admin@example.org', 'Example Admin', 'x') returning id::text`).Scan(&e.adminID)
	if err != nil {
		t.Fatalf("insert the admin %v", err)
	}

	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()

	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("exec failed %v", err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()

	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count failed %v", err)
	}

	return n
}

// model adds a model with the units and returns both.
func (e *env) model(name, prefix string, units int) (catalog.Model, []catalog.Unit) {
	e.t.Helper()

	m, err := e.catalog.CreateModel(context.Background(), catalog.NewModel{
		Name: name, NewCategory: &catalog.CategoryInput{Name: "Example category", Tone: "blue"}, Prefix: prefix,
	}, nil)
	if err != nil {
		e.t.Fatalf("add the model %v", err)
	}

	list, err := e.catalog.CreateUnits(context.Background(), m.ID, e.adminID, catalog.UnitInput{Count: ptr(units), Condition: "Good", Place: "Example shelf"}, nil)
	if err != nil {
		e.t.Fatalf("add the units %v", err)
	}

	return m, list.Items
}

func (e *env) available(modelID string) (held, available int) {
	e.t.Helper()

	page, err := e.catalog.ListCatalog(context.Background(), catalog.ModelFilter{}, catalog.PageRequest{Limit: 100})
	if err != nil {
		e.t.Fatalf("list the catalog %v", err)
	}

	for _, m := range page.Items {
		if m.ID == modelID {
			return m.Held, m.Available
		}
	}

	e.t.Fatalf("the model %s is not in the catalog", modelID)

	return 0, 0
}

// borrower is a guest session with an identity.
type borrower struct {
	identity session.Identity
	token    string
	hash     []byte
}

// borrower adds a guest session for a person of its own.
func (e *env) borrower() *borrower {
	e.t.Helper()

	e.people++

	return e.borrowerWith(session.Identity{
		Name:   fmt.Sprintf("Example Borrower %d", e.people),
		NimNip: fmt.Sprintf("%015d", e.people),
		Email:  fmt.Sprintf("borrower%d@student.ub.ac.id", e.people),
	})
}

func (e *env) borrowerWith(identity session.Identity) *borrower {
	e.t.Helper()

	token, sess, err := e.sessions.CreateGuest(context.Background(), &identity, nil)
	if err != nil {
		e.t.Fatalf("make the session %v", err)
	}

	return &borrower{identity: identity, token: token, hash: sess.TokenHash}
}

// viewable returns the ids of the tickets that the session of the borrower may read, oldest first.
func (e *env) viewable(b *borrower) []string {
	e.t.Helper()

	sess, err := e.sessions.Load(context.Background(), b.token)
	if err != nil {
		e.t.Fatalf("load the session %v", err)
	}

	return sess.ViewableTickets
}

func item(modelID string, qty int) ticket.ItemInput {
	return ticket.ItemInput{ModelID: modelID, Qty: ptr(qty)}
}

func request(b *borrower, items ...ticket.ItemInput) ticket.CreateRequest {
	return ticket.CreateRequest{
		Identity: b.identity, SessionHash: b.hash, Items: items, Purpose: "Example loan for a lab exercise.", PickupDate: "2026-11-20", DurationDays: ptr(7),
	}
}

func (e *env) create(b *borrower, items ...ticket.ItemInput) (ticket.Ticket, error) {
	return e.tickets.Create(context.Background(), request(b, items...), nil)
}

func (e *env) mustCreate(b *borrower, items ...ticket.ItemInput) ticket.Ticket {
	e.t.Helper()

	got, err := e.create(b, items...)
	if err != nil {
		e.t.Fatalf("create the ticket %v", err)
	}

	return got
}

// rawTicket is a ticket that a test inserts by hand, as the routes of an admin would leave it.
type rawTicket struct {
	status   string
	pickup   string
	days     int
	approved *int
	who      *borrower
	items    []rawItem
}

type rawItem struct {
	modelID  string
	qty      int
	approved *int
}

// insert writes a ticket and its items without the checks of Create and returns its id and its number.
func (e *env) insert(rt rawTicket) (id, number string) {
	e.t.Helper()

	e.raw++
	number = fmt.Sprintf("MGM-2611-9%03d-AAAA", e.raw)

	identity := session.Identity{Name: "Example Raw", NimNip: "999999999999999", Email: "raw@student.ub.ac.id"}
	if rt.who != nil {
		identity = rt.who.identity
	}

	pickup, days := rt.pickup, rt.days

	if pickup == "" {
		pickup = "2026-11-01"
	}

	if days == 0 {
		days = 5
	}

	pickupDate, err := time.Parse("2006-01-02", pickup)
	if err != nil {
		e.t.Fatalf("the pickup date %q is not a date", pickup)
	}

	err = e.pool.QueryRow(context.Background(), `
		insert into tickets (number, status, requester_name, requester_nim_nip, requester_email, purpose, pickup_date, duration_days,
		                     approved_duration_days)
		values ($1, $2, $3, $4, lower($5), 'Example purpose', $6::date, $7, $8) returning id::text`,
		number, rt.status, identity.Name, identity.NimNip, identity.Email, pickupDate, days, rt.approved).Scan(&id)
	if err != nil {
		e.t.Fatalf("insert the ticket %v", err)
	}

	for _, it := range rt.items {
		e.exec(`insert into ticket_items (ticket_id, model_id, qty, approved_qty) values ($1::uuid, $2::uuid, $3, $4)`, id, it.modelID, it.qty, it.approved)
	}

	return id, number
}

// fieldList turns the error of a refused request into "field code" for each failing field, in order. Another error gives nil.
func fieldList(err error) []string {
	var invalid *catalog.ValidationError
	if !errors.As(err, &invalid) {
		return nil
	}

	out := make([]string, 0, len(invalid.Details))
	for _, d := range invalid.Details {
		out = append(out, d.Field+" "+d.Code)
	}

	return out
}

// fields is fieldList for the goroutine of a test, and it stops the test when the error is not a refusal of fields.
func fields(t *testing.T, err error) []string {
	t.Helper()

	out := fieldList(err)
	if out == nil {
		t.Fatalf("the error is %v, not a refusal of fields", err)
	}

	return out
}

func isStatusConflict(err error) bool {
	var rule *catalog.RuleError

	return errors.As(err, &rule) && rule.Kind == catalog.RuleStatusConflict
}

// together starts every function at once and returns when all of them ended.
func together(fns ...func()) {
	var wg sync.WaitGroup

	gate := make(chan struct{})

	for _, fn := range fns {
		wg.Go(func() {
			<-gate
			fn()
		})
	}

	close(gate)
	wg.Wait()
}

var numberShape = regexp.MustCompile(`^MGM-2611-[0-9]{4}-[A-HJ-NP-Z2-9]{4}$`)

func runningNumber(t *testing.T, number string) int {
	t.Helper()

	n, err := strconv.Atoi(strings.Split(number, "-")[2])
	if err != nil {
		t.Fatalf("the number %q has no running part", number)
	}

	return n
}

func TestCreateWritesTheTicketItsItemsAndItsFirstEvent(t *testing.T) {
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 3)
	b := e.borrower()

	got := e.mustCreate(b, item(m.ID, 2))

	if !numberShape.MatchString(got.Number) || runningNumber(t, got.Number) != 1 {
		t.Fatalf("the number is %q", got.Number)
	}

	if got.Status != "diajukan" || got.EffectiveStatus != "diajukan" || got.Version != 1 || got.ID == "" {
		t.Fatalf("the status is %s, %s, version %d", got.Status, got.EffectiveStatus, got.Version)
	}

	r := got.Requester
	if r.Kind != "guest" || r.Name == nil || *r.Name != b.identity.Name || r.NimNip == nil || *r.NimNip != b.identity.NimNip ||
		r.Email == nil || *r.Email != b.identity.Email {
		t.Fatalf("the requester is %+v", r)
	}

	if got.Purpose == nil || *got.Purpose != "Example loan for a lab exercise." || got.PickupDate != "2026-11-20" || got.DurationDays != 7 ||
		got.ApprovedDurationDays != nil || got.DueDate != "2026-11-27" || got.DecisionNote != nil || got.AnonymizedAt != nil {
		t.Fatalf("the ticket is %+v", got)
	}

	if len(got.Items) != 1 || got.Items[0].ModelID != m.ID || got.Items[0].ModelName != "Example headset" || got.Items[0].Qty != 2 || got.Items[0].ApprovedQty != nil {
		t.Fatalf("the items are %+v", got.Items)
	}

	if len(got.History) != 1 || got.History[0].Status != "diajukan" || got.History[0].ActorKind != "guest" || got.History[0].Note != nil {
		t.Fatalf("the history is %+v", got.History)
	}

	if e.count(`select count(*)::int from tickets`) != 1 || e.count(`select count(*)::int from ticket_items where approved_qty is null`) != 1 ||
		e.count(`select count(*)::int from ticket_history where admin_id is null`) != 1 ||
		e.count(`select count(*)::int from ticket_counters where period = '2611' and last = 1`) != 1 {
		t.Fatal("the rows of the ticket are not as expected")
	}

	// a ticket that is only asked for holds nothing
	if held, available := e.available(m.ID); held != 0 || available != 3 {
		t.Fatalf("held %d, available %d", held, available)
	}

	if !slices.Equal(e.viewable(b), []string{got.ID}) {
		t.Fatalf("the session may read %v", e.viewable(b))
	}
}

func TestTheNumberIsUniqueAndHasNoGapUnderParallelCreates(t *testing.T) {
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 30)

	const n = 20

	var (
		mu      sync.Mutex
		numbers []string
		errs    []error
		fns     []func()
	)

	for range n {
		b := e.borrower()

		fns = append(fns, func() {
			got, err := e.create(b, item(m.ID, 1))

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)

				return
			}

			numbers = append(numbers, got.Number)
		})
	}

	together(fns...)

	if len(errs) != 0 {
		t.Fatalf("%d creates failed, the first with %v", len(errs), errs[0])
	}

	running := make([]int, 0, n)
	for _, number := range numbers {
		running = append(running, runningNumber(t, number))
	}

	slices.Sort(running)

	for i, r := range running {
		if r != i+1 {
			t.Fatalf("the running numbers are %v", running)
		}
	}

	if e.count(`select last from ticket_counters where period = '2611'`) != n || e.count(`select count(distinct number)::int from tickets`) != n {
		t.Fatal("the counter or the numbers are not as expected")
	}
}

func TestACreateThatFailsGivesItsNumberBack(t *testing.T) {
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 3)
	b := e.borrower()

	// the answer cannot be stored, so the transaction fails after the number was taken
	_, err := e.tickets.Create(context.Background(), request(b, item(m.ID, 1)), failingRecorder{})
	if err == nil {
		t.Fatal("the create passed a recorder that fails")
	}

	if e.count(`select count(*)::int from tickets`) != 0 || e.count(`select count(*)::int from ticket_items`) != 0 ||
		e.count(`select count(*)::int from ticket_history`) != 0 || e.count(`select count(*)::int from ticket_counters`) != 0 {
		t.Fatal("a failed create left rows behind")
	}

	if len(e.viewable(b)) != 0 {
		t.Fatalf("the session may read %v", e.viewable(b))
	}

	if got := e.mustCreate(b, item(m.ID, 1)); runningNumber(t, got.Number) != 1 {
		t.Fatalf("the next number is %s, so the failed create kept its number", got.Number)
	}
}

type failingRecorder struct{}

func (failingRecorder) Record(context.Context, pgx.Tx, int, any) error {
	return errors.New("the answer cannot be stored")
}

func TestAPreviewStoresNothingAndAnswersLikeACreate(t *testing.T) {
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 3)
	b := e.borrower()

	got, err := e.tickets.Preview(context.Background(), request(b, item(m.ID, 2)))
	if err != nil {
		t.Fatalf("the preview failed %v", err)
	}

	if !numberShape.MatchString(got.Number) || got.Status != "diajukan" || len(got.Items) != 1 || len(got.History) != 1 {
		t.Fatalf("the preview is %+v", got)
	}

	if e.count(`select count(*)::int from tickets`) != 0 || e.count(`select count(*)::int from ticket_counters`) != 0 || len(e.viewable(b)) != 0 {
		t.Fatal("the preview stored something")
	}

	// the preview refuses what a create refuses
	_, err = e.tickets.Preview(context.Background(), request(b, item(m.ID, 9)))
	if got := fields(t, err); !slices.Equal(got, []string{"items[0].qty too_many"}) {
		t.Fatalf("the refusal is %v", got)
	}

	if real := e.mustCreate(b, item(m.ID, 1)); runningNumber(t, real.Number) != 1 {
		t.Fatalf("the first real number is %s", real.Number)
	}
}

func TestACreateRefusesWhatTheStockAndTheModelsForbid(t *testing.T) {
	e := newEnv(t)
	m, units := e.model("Example headset", "EXM", 3)
	retired, _ := e.model("Example retired", "RET", 1)
	b := e.borrower()

	if _, err := e.catalog.UpdateModel(context.Background(), retired.ID, catalog.ModelPatch{Retired: ptr(true)}); err != nil {
		t.Fatalf("retire the model %v", err)
	}

	if _, err := e.catalog.ChangeLifecycle(context.Background(), units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: catalog.LifecycleMaintenance}); err != nil {
		t.Fatalf("change the lifecycle %v", err)
	}

	unknown := "00000000-0000-4000-8000-0000000000ff"

	_, err := e.create(b, item(retired.ID, 1), item(unknown, 1), item(m.ID, 3))

	want := []string{"items[0].modelId model_retired", "items[1].modelId unknown_reference", "items[2].qty too_many"}
	if got := fields(t, err); !slices.Equal(got, want) {
		t.Fatalf("the refusals are %v, want %v", got, want)
	}

	// the unit in maintenance is not stock, so 2 are available
	var invalid *catalog.ValidationError

	if !errors.As(err, &invalid) || invalid.Details[2].Message != "At most 2 are available." {
		t.Fatalf("the message is %+v", invalid)
	}

	if e.count(`select count(*)::int from tickets`) != 0 || e.count(`select count(*)::int from ticket_counters`) != 0 {
		t.Fatal("a refused request left rows behind")
	}

	if got := e.mustCreate(b, item(m.ID, 2)); got.Items[0].Qty != 2 {
		t.Fatalf("the ticket for the available quantity is %+v", got)
	}
}

func TestTheHoldOfAnApprovedTicketCountsAndItsCancelGivesItBack(t *testing.T) {
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 2)

	first, second := e.borrower(), e.borrower()

	one := e.mustCreate(first, item(m.ID, 2))

	// an approval, as the routes of an admin will do it, holds the approved quantity
	e.exec(`update ticket_items set approved_qty = 2 where ticket_id = $1::uuid`, one.ID)
	e.exec(`update tickets set status = 'disetujui', version = version + 1 where id = $1::uuid`, one.ID)

	if held, available := e.available(m.ID); held != 2 || available != 0 {
		t.Fatalf("held %d, available %d", held, available)
	}

	_, err := e.create(second, item(m.ID, 1))
	if got := fields(t, err); !slices.Equal(got, []string{"items[0].qty too_many"}) {
		t.Fatalf("the refusal is %v", got)
	}

	// the cancel of an approved ticket, as the routes of an admin will do it, releases the hold
	e.exec(`update tickets set status = 'dibatalkan', version = version + 1 where id = $1::uuid`, one.ID)

	if held, available := e.available(m.ID); held != 0 || available != 2 {
		t.Fatalf("held %d, available %d", held, available)
	}

	if got := e.mustCreate(second, item(m.ID, 2)); got.Items[0].Qty != 2 {
		t.Fatalf("the ticket after the cancel is %+v", got)
	}
}

func TestAGuestCancelFreesASlotOfTheLimitAndChangesNoStock(t *testing.T) {
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 5)
	b := e.borrower()

	var made []ticket.Ticket
	for range 3 {
		made = append(made, e.mustCreate(b, item(m.ID, 1)))
	}

	_, err := e.create(b, item(m.ID, 1))
	if got := fields(t, err); !slices.Equal(got, []string{"requester too_many_open_tickets"}) {
		t.Fatalf("the refusal is %v", got)
	}

	cancelled, err := e.tickets.CancelByGuest(context.Background(), e.viewable(b), made[1].Number)
	if err != nil || cancelled.Status != "dibatalkan" {
		t.Fatalf("the cancel gave %+v, %v", cancelled, err)
	}

	if held, available := e.available(m.ID); held != 0 || available != 5 {
		t.Fatalf("held %d, available %d", held, available)
	}

	if _, err := e.create(b, item(m.ID, 1)); err != nil {
		t.Fatalf("the create after the cancel failed %v", err)
	}
}

func TestParallelCreatesForTheLastUnitBothPassBecauseARequestHoldsNothing(t *testing.T) {
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 1)

	var (
		mu   sync.Mutex
		errs []error
	)

	var fns []func()

	for range 2 {
		b := e.borrower()

		fns = append(fns, func() {
			_, err := e.create(b, item(m.ID, 1))

			mu.Lock()
			defer mu.Unlock()

			errs = append(errs, err)
		})
	}

	together(fns...)

	// docs/data-model.md says that the hold starts at the approval, so both requests are accepted and the approval of the second
	// answers stock_changed. The check at creation is for a quantity that is too high at that moment.
	if errs[0] != nil || errs[1] != nil || e.count(`select count(*)::int from tickets where status = 'diajukan'`) != 2 {
		t.Fatalf("the creates gave %v and %v", errs[0], errs[1])
	}

	if held, available := e.available(m.ID); held != 0 || available != 1 {
		t.Fatalf("held %d, available %d", held, available)
	}
}

func TestACreateWaitsForALockOfTheStockAndReadsTheStockAfterIt(t *testing.T) {
	ctx := context.Background()

	t.Run("a hold that commits first refuses the request", func(t *testing.T) {
		e := newEnv(t)
		m, _ := e.model("Example headset", "EXM", 2)
		b := e.borrower()

		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %v", err)
		}

		defer func() { _ = tx.Rollback(ctx) }()

		// the lock and the hold of an approval, which the routes of an admin will take in this order
		if _, err := catalog.LockModels(ctx, tx, m.ID); err != nil {
			t.Fatalf("lock %v", err)
		}

		var held string

		err = tx.QueryRow(ctx, `
			insert into tickets (number, status, requester_name, requester_nim_nip, requester_email, purpose, pickup_date, duration_days, approved_duration_days)
			values ('MGM-2611-9001-AAAA', 'disetujui', 'Example Raw', '999999999999999', 'raw@student.ub.ac.id', 'Example purpose', '2026-11-20', 5, 5)
			returning id::text`).Scan(&held)
		if err != nil {
			t.Fatalf("insert the ticket %v", err)
		}

		if _, err := tx.Exec(ctx, `insert into ticket_items (ticket_id, model_id, qty, approved_qty) values ($1::uuid, $2::uuid, 2, 2)`, held, m.ID); err != nil {
			t.Fatalf("insert the item %v", err)
		}

		done := make(chan error, 1)

		go func() {
			_, err := e.create(b, item(m.ID, 1))
			done <- err
		}()

		// the create has to wait for the lock, and the wait proves that it checks the stock after the lock and not before it
		select {
		case err := <-done:
			t.Fatalf("the create did not wait for the lock and gave %v", err)
		case <-time.After(300 * time.Millisecond):
		}

		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %v", err)
		}

		select {
		case err := <-done:
			if got := fields(t, err); !slices.Equal(got, []string{"items[0].qty too_many"}) {
				t.Fatalf("the refusal is %v", got)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the create never ended")
		}
	})

	t.Run("a change that frees nothing lets it through after the wait", func(t *testing.T) {
		e := newEnv(t)
		m, _ := e.model("Example headset", "EXM", 2)
		b := e.borrower()

		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %v", err)
		}

		defer func() { _ = tx.Rollback(ctx) }()

		if _, err := catalog.LockModels(ctx, tx, m.ID); err != nil {
			t.Fatalf("lock %v", err)
		}

		done := make(chan error, 1)

		go func() {
			_, err := e.create(b, item(m.ID, 2))
			done <- err
		}()

		select {
		case err := <-done:
			t.Fatalf("the create did not wait for the lock and gave %v", err)
		case <-time.After(300 * time.Millisecond):
		}

		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback %v", err)
		}

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the create failed after the wait %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the create never ended")
		}
	})
}

func TestTwoRequestsWithTheSameModelsInOppositeOrderNeverDeadlock(t *testing.T) {
	e := newEnv(t)
	a, _ := e.model("Example headset", "EXA", 40)
	b, _ := e.model("Example camera", "EXB", 40)

	for round := range 15 {
		first, second := e.borrower(), e.borrower()
		errs := make([]error, 3)

		together(
			func() { _, errs[0] = e.create(first, item(a.ID, 1), item(b.ID, 1)) },
			func() { _, errs[1] = e.create(second, item(b.ID, 1), item(a.ID, 1)) },
			// a change of stock locks the same models for update in the order of the id, as an approval does
			func() {
				tx, err := e.pool.Begin(context.Background())
				if err != nil {
					errs[2] = err

					return
				}

				defer func() { _ = tx.Rollback(context.Background()) }()

				if _, errs[2] = catalog.LockModels(context.Background(), tx, b.ID, a.ID); errs[2] != nil {
					return
				}

				errs[2] = tx.Commit(context.Background())
			},
		)

		if errs[0] != nil || errs[1] != nil || errs[2] != nil {
			t.Fatalf("round %d gave %v, %v and %v", round, errs[0], errs[1], errs[2])
		}
	}

	if e.count(`select count(*)::int from tickets`) != 30 {
		t.Fatalf("%d tickets", e.count(`select count(*)::int from tickets`))
	}
}

func TestRetiringOrDeletingAModelAndCreatingATicketForItNeverBothWin(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	for round := range 12 {
		m, _ := e.model(fmt.Sprintf("Example headset %d", round), fmt.Sprintf("R%02d", round), 2)
		b := e.borrower()

		var createErr, changeErr error

		together(
			func() { _, createErr = e.create(b, item(m.ID, 1)) },
			func() {
				// the even rounds retire the model and the odd rounds delete it
				if round%2 == 0 {
					_, changeErr = e.catalog.UpdateModel(ctx, m.ID, catalog.ModelPatch{Retired: ptr(true)})
				} else {
					changeErr = e.catalog.DeleteModel(ctx, m.ID)
				}
			},
		)

		// the ticket that exists means that the guard of the model saw it, and the guard that won means that the ticket was refused
		created, changed := createErr == nil, changeErr == nil
		if created == changed {
			t.Fatalf("round %d, the create gave %v and the change gave %v", round, createErr, changeErr)
		}

		if created && !isStatusConflict(changeErr) {
			t.Fatalf("round %d, the change gave %v and not a status conflict", round, changeErr)
		}

		if changed {
			got := fields(t, createErr)[0]
			if got != "items[0].modelId model_retired" && got != "items[0].modelId unknown_reference" {
				t.Fatalf("round %d, the create gave %v", round, createErr)
			}
		}
	}
}

func TestOpenTicketsCountForTheEmailAndForTheNimOrNipAndOnlyWhileOpen(t *testing.T) {
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 20)

	person := session.Identity{Name: "Example Borrower", NimNip: "ab123456", Email: "person@student.ub.ac.id"}
	b := e.borrowerWith(person)

	var made []ticket.Ticket
	for range 3 {
		made = append(made, e.mustCreate(b, item(m.ID, 1)))
	}

	refused := []string{"requester too_many_open_tickets"}

	for name, identity := range map[string]session.Identity{
		"the same email in capitals":        {Name: "Another Name", NimNip: "zz999999", Email: "PERSON@Student.UB.ac.id"},
		"the same NIM in other letter case": {Name: "Another Name", NimNip: "AB123456", Email: "other@student.ub.ac.id"},
	} {
		_, err := e.create(e.borrowerWith(identity), item(m.ID, 1))
		if got := fields(t, err); !slices.Equal(got, refused) {
			t.Fatalf("%s gives %v", name, got)
		}
	}

	// a person with another email and another NIM is not counted
	if _, err := e.create(e.borrower(), item(m.ID, 1)); err != nil {
		t.Fatalf("another person was refused %v", err)
	}

	// a closed ticket does not count
	e.exec(`update tickets set status = 'dikembalikan' where id = $1::uuid`, made[0].ID)

	if _, err := e.create(b, item(m.ID, 1)); err != nil {
		t.Fatalf("the create after a return failed %v", err)
	}
}

func TestEveryStatusCountsAsOpenOrNotAsTheDesignSays(t *testing.T) {
	e := newEnvLimits(t, ticket.Limits{MaxItems: 10, MinDays: 1, MaxDays: 30, MaxOpen: 1})
	m, _ := e.model("Example headset", "EXM", 20)

	for _, status := range ticket.Statuses() {
		b := e.borrower()
		e.insert(rawTicket{status: status, who: b})

		_, err := e.create(b, item(m.ID, 1))

		if ticket.IsOpen(status) {
			if got := fields(t, err); !slices.Equal(got, []string{"requester too_many_open_tickets"}) {
				t.Fatalf("a ticket in the status %s did not count, %v", status, err)
			}
		} else if err != nil {
			t.Fatalf("a ticket in the status %s counted, %v", status, err)
		}
	}
}

func TestParallelCreatesOfOnePersonNeverPassTheLimitOfOpenTickets(t *testing.T) {
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 20)

	person := session.Identity{Name: "Example Borrower", NimNip: "000000000000001", Email: "person@student.ub.ac.id"}

	var (
		mu     sync.Mutex
		failed []string
		passed int
		fns    []func()
	)

	for range 6 {
		// each request comes from its own session, as a person with two browsers would send them
		b := e.borrowerWith(person)

		fns = append(fns, func() {
			_, err := e.create(b, item(m.ID, 1))

			mu.Lock()
			defer mu.Unlock()

			if err == nil {
				passed++

				return
			}

			failed = append(failed, strings.Join(fieldList(err), ",")+" "+fmt.Sprint(err))
		})
	}

	together(fns...)

	if passed != 3 || len(failed) != 3 || e.count(`select count(*)::int from tickets`) != 3 {
		t.Fatalf("%d passed, %d failed with %v", passed, len(failed), failed)
	}

	for _, f := range failed {
		if !strings.HasPrefix(f, "requester too_many_open_tickets ") {
			t.Fatalf("a request failed with %s", f)
		}
	}
}

func TestEveryStatusHoldsStockAsTheCatalogSays(t *testing.T) {
	e := newEnv(t)

	for i, status := range ticket.Statuses() {
		m, _ := e.model(fmt.Sprintf("Example headset %d", i), fmt.Sprintf("S%d", i), 5)
		e.insert(rawTicket{status: status, items: []rawItem{{modelID: m.ID, qty: 1, approved: ptr(1)}}})

		wantHeld := 0
		if ticket.IsHolding(status) {
			wantHeld = 1
		}

		if held, available := e.available(m.ID); held != wantHeld || available != 5-wantHeld {
			t.Fatalf("a ticket in the status %s gives held %d and available %d in the catalog, and the ticket package says %v",
				status, held, available, ticket.IsHolding(status))
		}
	}
}

func TestAGuestReadsOnlyTheTicketsOfItsOwnSession(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 5)

	one, two := e.borrower(), e.borrower()
	mine := e.mustCreate(one, item(m.ID, 1))
	theirs := e.mustCreate(two, item(m.ID, 1))

	if got, err := e.tickets.Get(ctx, e.viewable(one), mine.Number); err != nil || got.ID != mine.ID {
		t.Fatalf("a guest read its own ticket, %+v, %v", got, err)
	}

	// the ticket of another guest, a number that does not exist and an empty list are the same error
	_, other := e.tickets.Get(ctx, e.viewable(one), theirs.Number)
	_, missing := e.tickets.Get(ctx, e.viewable(one), "MGM-2611-9999-AAAA")
	_, empty := e.tickets.Get(ctx, nil, mine.Number)
	_, badShape := e.tickets.Get(ctx, e.viewable(one), "not a number")

	for name, err := range map[string]error{"another guest": other, "a missing number": missing, "an empty list": empty, "a bad shape": badShape} {
		if !errors.Is(err, ticket.ErrNotFound) {
			t.Fatalf("%s gives %v", name, err)
		}
	}

	// the cancel of the ticket of another guest finds nothing and changes nothing
	if _, err := e.tickets.CancelByGuest(ctx, e.viewable(one), theirs.Number); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("the cancel gave %v", err)
	}

	if e.count(`select count(*)::int from tickets where status = 'diajukan'`) != 2 {
		t.Fatal("a cancel of another guest changed a ticket")
	}

	page, err := e.tickets.List(ctx, e.viewable(one), catalog.PageRequest{Limit: 50})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != mine.ID {
		t.Fatalf("the list is %+v, %v", page, err)
	}

	page, err = e.tickets.List(ctx, nil, catalog.PageRequest{Limit: 50})
	if err != nil || page.Total != 0 || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("the list of a session with no ticket is %+v, %v", page, err)
	}
}

func TestLookupFindsATicketByItsNumberAndEmailOnly(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 5)
	b := e.borrower()
	mine := e.mustCreate(b, item(m.ID, 1))

	got, err := e.tickets.Lookup(ctx, mine.Number, strings.ToUpper(b.identity.Email))
	if err != nil || got.ID != mine.ID || got.Number != mine.Number {
		t.Fatalf("the lookup gave %+v, %v", got, err)
	}

	last := mine.Number[len(mine.Number)-1]
	other := byte('A')

	if last == other {
		other = 'B'
	}

	wrongSuffix := mine.Number[:len(mine.Number)-1] + string(other)

	for name, args := range map[string][2]string{
		"a wrong email":    {mine.Number, "other@student.ub.ac.id"},
		"a wrong suffix":   {wrongSuffix, b.identity.Email},
		"a missing number": {"MGM-2611-9999-AAAA", b.identity.Email},
		"a bad shape":      {"nothing", b.identity.Email},
		"an empty email":   {mine.Number, ""},
	} {
		if _, err := e.tickets.Lookup(ctx, args[0], args[1]); !errors.Is(err, ticket.ErrNoMatch) {
			t.Fatalf("%s gives %v", name, err)
		}
	}

	// the retention job clears the email, so the number alone finds nothing
	e.exec(`update tickets set requester_name = null, requester_nim_nip = null, requester_email = null, purpose = null, anonymized_at = now()
		where id = $1::uuid`, mine.ID)

	if _, err := e.tickets.Lookup(ctx, mine.Number, b.identity.Email); !errors.Is(err, ticket.ErrNoMatch) {
		t.Fatalf("the lookup of an anonymized ticket gave %v", err)
	}

	read, err := e.tickets.Get(ctx, e.viewable(b), mine.Number)
	if err != nil || read.Requester.Name != nil || read.Requester.Email != nil || read.Purpose != nil || read.AnonymizedAt == nil {
		t.Fatalf("the anonymized ticket is %+v, %v", read, err)
	}
}

func TestCancelByGuestChangesTheStatusOnceAndAnswersARepeat(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 5)
	b := e.borrower()
	mine := e.mustCreate(b, item(m.ID, 1))

	first, err := e.tickets.CancelByGuest(ctx, e.viewable(b), mine.Number)
	if err != nil || first.Status != "dibatalkan" || first.EffectiveStatus != "dibatalkan" || first.Version != 2 {
		t.Fatalf("the cancel gave %+v, %v", first, err)
	}

	if len(first.History) != 2 || first.History[1].Status != "dibatalkan" || first.History[1].ActorKind != "guest" || first.History[1].Note != nil ||
		first.History[0].Status != "diajukan" || first.DecisionNote != nil {
		t.Fatalf("the history is %+v", first.History)
	}

	again, err := e.tickets.CancelByGuest(ctx, e.viewable(b), mine.Number)
	if err != nil || again.Status != "dibatalkan" || again.Version != 2 || len(again.History) != 2 {
		t.Fatalf("the repeat gave %+v, %v", again, err)
	}

	if e.count(`select count(*)::int from ticket_history where ticket_id = $1::uuid`, mine.ID) != 2 {
		t.Fatal("the repeat wrote a history entry")
	}
}

func TestCancelByGuestRefusesEveryOtherStatus(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	for _, status := range []string{"disetujui", "siap_diambil", "dipinjam", "dikembalikan", "ditolak"} {
		b := e.borrower()
		id, number := e.insert(rawTicket{status: status, who: b})

		if _, err := session.AddViewableTicket(ctx, e.pool, b.hash, id); err != nil {
			t.Fatalf("add the ticket %v", err)
		}

		_, err := e.tickets.CancelByGuest(ctx, e.viewable(b), number)
		if !isStatusConflict(err) {
			t.Fatalf("a ticket in the status %s gave %v", status, err)
		}

		if e.count(`select count(*)::int from tickets where id = $1::uuid and status = $2 and version = 1`, id, status) != 1 ||
			e.count(`select count(*)::int from ticket_history where ticket_id = $1::uuid`, id) != 0 {
			t.Fatalf("the refused cancel of a ticket in the status %s changed it", status)
		}
	}
}

func TestParallelCancelsAndADecisionLeaveOneAnswerEach(t *testing.T) {
	ctx := context.Background()

	t.Run("five cancels of one ticket write one entry", func(t *testing.T) {
		e := newEnv(t)
		m, _ := e.model("Example headset", "EXM", 5)
		b := e.borrower()
		mine := e.mustCreate(b, item(m.ID, 1))
		viewable := e.viewable(b)

		errs := make([]error, 5)
		fns := make([]func(), 0, len(errs))

		for i := range errs {
			fns = append(fns, func() { _, errs[i] = e.tickets.CancelByGuest(ctx, viewable, mine.Number) })
		}

		together(fns...)

		for _, err := range errs {
			if err != nil {
				t.Fatalf("a cancel failed %v", err)
			}
		}

		if e.count(`select count(*)::int from ticket_history where ticket_id = $1::uuid and status = 'dibatalkan'`, mine.ID) != 1 ||
			e.count(`select version from tickets where id = $1::uuid`, mine.ID) != 2 {
			t.Fatal("the parallel cancels wrote more than one change")
		}
	})

	t.Run("a decision that commits first wins over the cancel", func(t *testing.T) {
		e := newEnv(t)
		m, _ := e.model("Example headset", "EXM", 5)
		b := e.borrower()
		mine := e.mustCreate(b, item(m.ID, 1))
		viewable := e.viewable(b)

		// the guarded update of an approval, which the routes of an admin will make
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %v", err)
		}

		defer func() { _ = tx.Rollback(ctx) }()

		if _, err := tx.Exec(ctx, `update tickets set status = 'disetujui', version = version + 1 where id = $1::uuid and status = 'diajukan'`, mine.ID); err != nil {
			t.Fatalf("approve %v", err)
		}

		done := make(chan error, 1)

		go func() {
			_, err := e.tickets.CancelByGuest(ctx, viewable, mine.Number)
			done <- err
		}()

		// the cancel reads the old status and waits at its own update, and the read after the wait names the status that won
		select {
		case err := <-done:
			t.Fatalf("the cancel did not wait and gave %v", err)
		case <-time.After(300 * time.Millisecond):
		}

		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %v", err)
		}

		select {
		case err := <-done:
			if !isStatusConflict(err) {
				t.Fatalf("the cancel gave %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the cancel never ended")
		}

		if e.count(`select count(*)::int from tickets where id = $1::uuid and status = 'disetujui' and version = 2`, mine.ID) != 1 {
			t.Fatal("the cancel overwrote the decision")
		}
	})
}

func TestListPagesNewestFirstWithASignedCursor(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 5)
	b := e.borrower()

	var made []ticket.Ticket
	for range 3 {
		made = append(made, e.mustCreate(b, item(m.ID, 1)))
	}

	viewable := e.viewable(b)
	if !slices.Equal(viewable, []string{made[0].ID, made[1].ID, made[2].ID}) {
		t.Fatalf("the session may read %v", viewable)
	}

	first, err := e.tickets.List(ctx, viewable, catalog.PageRequest{Limit: 2})
	if err != nil || len(first.Items) != 2 || first.Total != 3 || first.NextCursor == "" {
		t.Fatalf("the first page is %+v, %v", first, err)
	}

	if first.Items[0].ID != made[2].ID || first.Items[1].ID != made[1].ID {
		t.Fatalf("the first page is not newest first, %s and %s", first.Items[0].Number, first.Items[1].Number)
	}

	for _, got := range first.Items {
		if len(got.Items) != 1 || len(got.History) != 1 {
			t.Fatalf("a ticket of the page has %d items and %d history entries", len(got.Items), len(got.History))
		}
	}

	second, err := e.tickets.List(ctx, viewable, catalog.PageRequest{Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != made[0].ID || second.NextCursor != "" || second.Total != 3 {
		t.Fatalf("the second page is %+v, %v", second, err)
	}

	if _, err := e.tickets.List(ctx, viewable, catalog.PageRequest{Limit: 2, Cursor: "forged"}); !errors.Is(err, pagination.ErrCursor) {
		t.Fatalf("a forged cursor gave %v", err)
	}
}

func TestAReadShowsALoanAsLateOnlyAfterItsDueDate(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	// today is 17 November in Jakarta. The due date is the pickup date plus the approved days.
	for name, tc := range map[string]struct {
		rt   rawTicket
		want string
		due  string
	}{
		"due in the past":      {rawTicket{status: "dipinjam", pickup: "2026-11-01", days: 5, approved: ptr(3)}, "terlambat", "2026-11-04"},
		"due on a later day":   {rawTicket{status: "dipinjam", pickup: "2026-11-16", days: 5, approved: ptr(3)}, "dipinjam", "2026-11-19"},
		"requested days count": {rawTicket{status: "dipinjam", pickup: "2026-11-10", days: 2}, "terlambat", "2026-11-12"},
		"not a loan":           {rawTicket{status: "siap_diambil", pickup: "2026-11-01", days: 1}, "siap_diambil", "2026-11-02"},
		"returned in time":     {rawTicket{status: "dikembalikan", pickup: "2026-11-01", days: 1}, "dikembalikan", "2026-11-02"},
	} {
		b := e.borrower()
		tc.rt.who = b
		id, number := e.insert(tc.rt)

		if _, err := session.AddViewableTicket(ctx, e.pool, b.hash, id); err != nil {
			t.Fatalf("add the ticket %v", err)
		}

		got, err := e.tickets.Get(ctx, e.viewable(b), number)
		if err != nil || got.EffectiveStatus != tc.want || got.DueDate != tc.due || got.Status != tc.rt.status {
			t.Fatalf("%s gives %+v, %v", name, got, err)
		}
	}
}

func TestTheStoredAnswerOfAKeyIsTheTicketThatWasMade(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 5)
	b := e.borrower()

	keys := idempotency.NewStore(e.pool, func() time.Time { return testNow })
	key := idempotency.Key{Session: b.hash, Operation: "POST /api/tickets", Value: "c0a80101-example-key"}

	claim, answer, err := keys.Begin(ctx, key)
	if err != nil || claim == nil || answer != nil {
		t.Fatalf("the first claim gave %v, %v, %v", claim, answer, err)
	}

	made, err := e.tickets.Create(ctx, request(b, item(m.ID, 1)), claim)
	if err != nil {
		t.Fatalf("create %v", err)
	}

	_, stored, err := keys.Begin(ctx, key)
	if err != nil || stored == nil || stored.Status != 201 {
		t.Fatalf("the repeat gave %v, %v", stored, err)
	}

	var body struct {
		Number string `json:"number"`
		Status string `json:"status"`
	}

	if err := json.Unmarshal(stored.Body, &body); err != nil || body.Number != made.Number || body.Status != "diajukan" {
		t.Fatalf("the stored answer is %s, %v", stored.Body, err)
	}

	if e.count(`select count(*)::int from tickets`) != 1 {
		t.Fatal("the repeat made a second ticket")
	}
}

func TestTheSessionKeepsTheTwentyNewestTickets(t *testing.T) {
	ctx := context.Background()
	e := newEnvLimits(t, ticket.Limits{MaxItems: 10, MinDays: 1, MaxDays: 30, MaxOpen: 50})
	m, _ := e.model("Example headset", "EXM", 30)
	b := e.borrower()

	var ids []string

	for range 22 {
		made := e.mustCreate(b, item(m.ID, 1))
		ids = append(ids, made.ID)
	}

	if got := e.viewable(b); !slices.Equal(got, ids[2:]) {
		t.Fatalf("the session may read %v", got)
	}

	page, err := e.tickets.List(ctx, e.viewable(b), catalog.PageRequest{Limit: 100})
	if err != nil || page.Total != 20 {
		t.Fatalf("the list is %+v, %v", page, err)
	}
}
