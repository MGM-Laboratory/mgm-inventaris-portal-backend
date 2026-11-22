package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/secretbox"
)

var testNow = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

type env struct {
	t       *testing.T
	pool    *pgxpool.Pool
	store   *catalog.Store
	adminID string
	now     time.Time
	tickets int
}

func ptr[T any](v T) *T {
	return &v
}

// newEnv opens a database for the test and adds one admin. The tests skip without DATABASE_URL.
func newEnv(t *testing.T) *env {
	t.Helper()

	pool := dbtest.New(t)

	box, err := secretbox.New([]byte(strings.Repeat("k", secretbox.KeyLength)))
	if err != nil {
		t.Fatalf("make the box %v", err)
	}

	e := &env{t: t, pool: pool, now: testNow}
	e.store = catalog.NewStore(pool, box, func() time.Time { return e.now })

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

// model adds a model in the category Example category.
func (e *env) model(name, prefix string) catalog.Model {
	e.t.Helper()

	m, err := e.store.CreateModel(context.Background(), catalog.NewModel{
		Name: name, NewCategory: &catalog.CategoryInput{Name: "Example category", Tone: "blue"}, Prefix: prefix,
	}, nil)
	if err != nil {
		e.t.Fatalf("add the model %v", err)
	}

	return m
}

func (e *env) units(modelID string, count int) []catalog.Unit {
	e.t.Helper()

	list, err := e.store.CreateUnits(context.Background(), modelID, e.adminID, catalog.UnitInput{Count: ptr(count), Condition: "Good", Place: "Example shelf"}, nil)
	if err != nil {
		e.t.Fatalf("add the units %v", err)
	}

	return list.Items
}

type heldItem struct {
	modelID  string
	qty      int
	approved *int
}

// ticket inserts a ticket with its items by hand, because the routes of the tickets do not exist yet. It returns the id.
func (e *env) ticket(status string, items ...heldItem) string {
	e.t.Helper()

	e.tickets++

	var id string

	err := e.pool.QueryRow(context.Background(), `
		insert into tickets (number, status, requester_name, requester_nim_nip, requester_email, purpose, pickup_date, duration_days,
		                     approved_duration_days)
		values ($1, $2, 'Example Borrower', '000000000000000', 'example@student.ub.ac.id', 'Example purpose', '2026-11-01', 5, 3)
		returning id::text`, fmt.Sprintf("MGM-2611-%04d-K7QX", e.tickets), status).Scan(&id)
	if err != nil {
		e.t.Fatalf("insert the ticket %v", err)
	}

	for _, item := range items {
		e.exec(`insert into ticket_items (ticket_id, model_id, qty, approved_qty) values ($1::uuid, $2::uuid, $3, $4)`,
			id, item.modelID, item.qty, item.approved)
	}

	return id
}

func (e *env) hold(ticketID, tag string) {
	e.t.Helper()

	e.exec(`insert into ticket_units (ticket_id, unit_id) select $1::uuid, id from units where tag = $2`, ticketID, tag)
}

func (e *env) release(ticketID, tag string) {
	e.t.Helper()

	e.exec(`update ticket_units set returned_at = now(), return_outcome = 'ok'
		where ticket_id = $1::uuid and unit_id = (select id from units where tag = $2)`, ticketID, tag)
}

func isRule(err error, kind catalog.RuleKind) bool {
	var rule *catalog.RuleError

	return errors.As(err, &rule) && rule.Kind == kind
}

func isDuplicate(err error, field string) *catalog.DuplicateError {
	var dup *catalog.DuplicateError
	if errors.As(err, &dup) && dup.Field == field {
		return dup
	}

	return nil
}

func tags(units []catalog.Unit) []string {
	out := make([]string, 0, len(units))
	for _, u := range units {
		out = append(out, u.Tag)
	}

	return out
}

func TestParallelAddsOfTwentyUnitsNeverGiveADuplicateTag(t *testing.T) {
	e := newEnv(t)
	m := e.model("Example headset", "EXM")

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		all    []string
		errs   []error
		counts []int
	)

	gate := make(chan struct{})

	for range 2 {
		wg.Go(func() {
			<-gate

			list, err := e.store.CreateUnits(context.Background(), m.ID, e.adminID,
				catalog.UnitInput{Count: ptr(20), Condition: "Good", Place: "Example shelf"}, nil)

			mu.Lock()
			defer mu.Unlock()

			errs = append(errs, err)
			counts = append(counts, len(list.Items))
			all = append(all, tags(list.Items)...)
		})
	}

	close(gate)
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("a request failed %v", err)
		}
	}

	seen := map[string]bool{}
	for _, tag := range all {
		seen[tag] = true
	}

	if len(all) != 40 || len(seen) != 40 || counts[0] != 20 || counts[1] != 20 {
		t.Fatalf("%d tags, %d distinct, counts %v", len(all), len(seen), counts)
	}

	for i := 1; i <= 40; i++ {
		if !seen[fmt.Sprintf("MGM-EXM-%03d", i)] {
			t.Fatalf("the tag %d is missing", i)
		}
	}

	if next := e.count(`select next_unit_number from models where id = $1::uuid`, m.ID); next != 41 {
		t.Fatalf("the counter is %d", next)
	}

	if events := e.count(`select count(*)::int from unit_events where action = 'added' and admin_id = $1::uuid`, e.adminID); events != 40 {
		t.Fatalf("%d added events", events)
	}
}

func TestTagsKeepGrowingPastNineHundredNinetyNine(t *testing.T) {
	e := newEnv(t)
	m := e.model("Example headset", "EXM")

	e.exec(`update models set next_unit_number = 999 where id = $1::uuid`, m.ID)

	got := tags(e.units(m.ID, 3))
	if strings.Join(got, ",") != "MGM-EXM-999,MGM-EXM-1000,MGM-EXM-1001" {
		t.Fatalf("the tags are %v", got)
	}
}

func TestAddingUnitsIsAllOrNothing(t *testing.T) {
	e := newEnv(t)
	m := e.model("Example headset", "EXM")

	// the serial number is refused, so the counter and the rows of the request must stay as they were
	if _, err := e.store.CreateUnits(context.Background(), m.ID, e.adminID,
		catalog.UnitInput{Count: ptr(1), Serial: ptr("SN-0001"), Condition: "Good", Place: "Example shelf"}, nil); err != nil {
		t.Fatalf("the first unit failed %v", err)
	}

	_, err := e.store.CreateUnits(context.Background(), m.ID, e.adminID,
		catalog.UnitInput{Count: ptr(1), Serial: ptr("sn-0001"), Condition: "Good", Place: "Example shelf"}, nil)

	dup := isDuplicate(err, "serial")
	if dup == nil || dup.ExistingTag != "MGM-EXM-001" {
		t.Fatalf("a serial in another letter case gives %v", err)
	}

	if next := e.count(`select next_unit_number from models where id = $1::uuid`, m.ID); next != 2 {
		t.Fatalf("the counter is %d, so the failed request kept a number", next)
	}

	if n := e.count(`select count(*)::int from units`); n != 1 {
		t.Fatalf("%d units", n)
	}
}

func TestDuplicatesAreRefusedInAnyLetterCase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := e.model("Example headset", "EXM")

	_, err := e.store.CreateCategory(ctx, catalog.CategoryInput{Name: "EXAMPLE CATEGORY", Tone: "red"}, nil)
	if isDuplicate(err, "name") == nil {
		t.Fatalf("a category in capitals gives %v", err)
	}

	_, err = e.store.CreateModel(ctx, catalog.NewModel{Name: "EXAMPLE HEADSET", CategoryID: &first.CategoryID, Prefix: "OTH"}, nil)
	if isDuplicate(err, "name") == nil {
		t.Fatalf("a model name in capitals gives %v", err)
	}

	_, err = e.store.CreateModel(ctx, catalog.NewModel{Name: "Another headset", CategoryID: &first.CategoryID, Prefix: "EXM"}, nil)
	if isDuplicate(err, "prefix") == nil {
		t.Fatalf("the same prefix gives %v", err)
	}

	second, err := e.store.CreateModel(ctx, catalog.NewModel{Name: "Another headset", CategoryID: &first.CategoryID, Prefix: "OTH"}, nil)
	if err != nil {
		t.Fatalf("a model with its own name and prefix failed %v", err)
	}

	_, err = e.store.UpdateModel(ctx, second.ID, catalog.ModelPatch{Name: ptr("example HEADSET")})
	if isDuplicate(err, "name") == nil {
		t.Fatalf("a rename to a used name gives %v", err)
	}

	// the serial number of another unit, in another letter case, on a patch
	unitA := e.units(first.ID, 1)[0]
	unitB := e.units(second.ID, 1)[0]

	if _, err := e.store.UpdateUnit(ctx, unitA.Tag, e.adminID, catalog.UnitPatch{SerialSet: true, Serial: ptr("Serial-One")}); err != nil {
		t.Fatalf("set the serial failed %v", err)
	}

	_, err = e.store.UpdateUnit(ctx, unitB.Tag, e.adminID, catalog.UnitPatch{SerialSet: true, Serial: ptr("SERIAL-one")})

	dup := isDuplicate(err, "serial")
	if dup == nil || dup.ExistingTag != unitA.Tag {
		t.Fatalf("a patch to a used serial gives %v", err)
	}

	// units without a serial never clash
	if _, err := e.store.UpdateUnit(ctx, unitB.Tag, e.adminID, catalog.UnitPatch{SerialSet: true}); err != nil {
		t.Fatalf("clear the serial failed %v", err)
	}

	if _, err := e.store.UpdateUnit(ctx, unitA.Tag, e.adminID, catalog.UnitPatch{SerialSet: true}); err != nil {
		t.Fatalf("clear the serial failed %v", err)
	}
}

func TestANewCategoryWithAnExistingNameReusesIt(t *testing.T) {
	e := newEnv(t)
	first := e.model("Example headset", "EXM")

	second, err := e.store.CreateModel(context.Background(), catalog.NewModel{
		Name: "Another headset", NewCategory: &catalog.CategoryInput{Name: "example CATEGORY", Tone: "red"}, Prefix: "OTH",
	}, nil)
	if err != nil || second.CategoryID != first.CategoryID {
		t.Fatalf("the second model has the category %q, %v", second.CategoryID, err)
	}

	page, err := e.store.ListCategories(context.Background(), catalog.PageRequest{Limit: 10})
	if err != nil || page.Total != 1 || page.Items[0].Tone != "blue" || page.Items[0].Name != "Example category" {
		t.Fatalf("the categories are %+v, %v", page, err)
	}
}

func TestAModelInAnUnknownCategoryFailsAndKeepsNoNewCategory(t *testing.T) {
	e := newEnv(t)

	_, err := e.store.CreateModel(context.Background(), catalog.NewModel{
		Name: "Example headset", CategoryID: ptr("00000000-0000-4000-8000-000000000001"), Prefix: "EXM",
	}, nil)

	var invalid *catalog.ValidationError
	if !errors.As(err, &invalid) || invalid.Details[0].Field != "categoryId" || invalid.Details[0].Code != catalog.DetailUnknownReference {
		t.Fatalf("an unknown category gives %v", err)
	}

	// a prefix clash after a new category must roll the new category back too
	e.model("Example headset", "EXM")

	_, err = e.store.CreateModel(context.Background(), catalog.NewModel{
		Name: "Another headset", NewCategory: &catalog.CategoryInput{Name: "Brand new category", Tone: "green"}, Prefix: "EXM",
	}, nil)
	if isDuplicate(err, "prefix") == nil {
		t.Fatalf("the prefix clash gives %v", err)
	}

	if n := e.count(`select count(*)::int from categories`); n != 1 {
		t.Fatalf("%d categories, so the new category was not rolled back", n)
	}
}

func TestAvailabilityCountsTheHeldQuantitiesOfTickets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := e.model("Example headset", "EXM")
	other := e.model("Example camera", "CAM")

	units := e.units(m.ID, 5)
	e.units(other.ID, 2)

	// only active units count, so two units out of use leave a stock of 3
	if _, err := e.store.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "maintenance"}); err != nil {
		t.Fatalf("maintenance failed %v", err)
	}

	if _, err := e.store.ChangeLifecycle(ctx, units[1].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "lost", Note: ptr("Gone.")}); err != nil {
		t.Fatalf("lost failed %v", err)
	}

	// a ticket in diajukan holds nothing, and a closed ticket holds nothing
	e.ticket("diajukan", heldItem{m.ID, 3, nil})
	e.ticket("dikembalikan", heldItem{m.ID, 3, ptr(3)})
	e.ticket("dibatalkan", heldItem{m.ID, 3, ptr(3)})
	e.ticket("ditolak", heldItem{m.ID, 3, ptr(0)})

	page, err := e.store.ListCatalog(ctx, catalog.ModelFilter{}, catalog.PageRequest{Limit: 10})
	if err != nil {
		t.Fatalf("list failed %v", err)
	}

	byName := map[string]catalog.CatalogModel{}
	for _, item := range page.Items {
		byName[item.Name] = item
	}

	if got := byName["Example headset"]; got.Total != 3 || got.Held != 0 || got.Available != 3 {
		t.Fatalf("with no hold the quantities are %+v", got)
	}

	// the three holding statuses add up, and the approved quantity counts and not the requested one
	e.ticket("disetujui", heldItem{m.ID, 3, ptr(1)}, heldItem{other.ID, 1, ptr(1)})
	e.ticket("siap_diambil", heldItem{m.ID, 2, ptr(1)})

	page, err = e.store.ListCatalog(ctx, catalog.ModelFilter{}, catalog.PageRequest{Limit: 10})
	if err != nil {
		t.Fatalf("list failed %v", err)
	}

	byName = map[string]catalog.CatalogModel{}
	for _, item := range page.Items {
		byName[item.Name] = item
	}

	if got := byName["Example headset"]; got.Total != 3 || got.Held != 2 || got.Available != 1 {
		t.Fatalf("the headset has %+v", got)
	}

	if got := byName["Example camera"]; got.Total != 2 || got.Held != 1 || got.Available != 1 {
		t.Fatalf("the camera has %+v", got)
	}

	// a ticket in dipinjam still holds, and a hold above the stock never shows a negative quantity
	e.ticket("dipinjam", heldItem{m.ID, 4, ptr(4)})

	one, err := e.store.ListModels(ctx, catalog.ModelFilter{Query: "headset"}, catalog.PageRequest{Limit: 10})
	if err != nil || len(one.Items) != 1 {
		t.Fatalf("the models are %+v, %v", one, err)
	}

	if got := one.Items[0]; got.Total != 3 || got.Held != 6 || got.Available != 0 {
		t.Fatalf("the headset has %+v", got)
	}

	// the filter available leaves out the model with nothing available
	avail, err := e.store.ListCatalog(ctx, catalog.ModelFilter{Available: true}, catalog.PageRequest{Limit: 10})
	if err != nil || avail.Total != 1 || avail.Items[0].Name != "Example camera" {
		t.Fatalf("the available models are %+v, %v", avail, err)
	}
}

func TestALifecycleChangeUnderAHeldUnitIsRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := e.model("Example headset", "EXM")
	units := e.units(m.ID, 2)

	id := e.ticket("dipinjam", heldItem{m.ID, 1, ptr(1)})
	e.hold(id, units[0].Tag)

	for _, to := range []string{"maintenance", "lost", "retired"} {
		_, err := e.store.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: to, Note: ptr("Example note.")})
		if !isRule(err, catalog.RuleUnitHeld) {
			t.Fatalf("a step to %s gives %v", to, err)
		}
	}

	// the permanent delete of a held unit is refused too, as a unit that a ticket used
	if err := e.store.DeleteUnit(ctx, units[0].Tag); !isRule(err, catalog.RuleStatusConflict) {
		t.Fatalf("the delete gives %v", err)
	}

	got, err := e.store.GetUnit(ctx, units[0].Tag)
	if err != nil || got.Status != "borrowed" || got.OpenTicketNumber != "MGM-2611-0001-K7QX" || got.Lifecycle != "active" {
		t.Fatalf("the unit is %+v, %v", got, err)
	}

	if n := e.count(`select count(*)::int from unit_events where unit_id = (select id from units where tag = $1) and action <> 'added'`, units[0].Tag); n != 0 {
		t.Fatalf("a refused step wrote %d events", n)
	}

	// after the return the unit may leave
	e.release(id, units[0].Tag)

	if _, err := e.store.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "maintenance"}); err != nil {
		t.Fatalf("a step after the return failed %v", err)
	}
}

func TestAChangeThatLeavesLessStockThanHeldIsRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := e.model("Example headset", "EXM")
	units := e.units(m.ID, 2)

	e.ticket("disetujui", heldItem{m.ID, 1, ptr(1)})

	if _, err := e.store.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "maintenance"}); err != nil {
		t.Fatalf("a step that keeps the stock at the hold failed %v", err)
	}

	_, err := e.store.ChangeLifecycle(ctx, units[1].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "lost", Note: ptr("Gone.")})
	if !isRule(err, catalog.RuleStockChanged) {
		t.Fatalf("a step below the hold gives %v", err)
	}

	// the same rule holds for the permanent delete of an active unit
	if err := e.store.DeleteUnit(ctx, units[1].Tag); !isRule(err, catalog.RuleStockChanged) {
		t.Fatalf("the delete gives %v", err)
	}

	// a step to active adds stock, and the unit in maintenance may be lost without a change of the stock
	if _, err := e.store.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "lost", Note: ptr("Gone.")}); err != nil {
		t.Fatalf("a step out of maintenance failed %v", err)
	}
}

func TestLifecycleStepsWriteEventsAndFollowTheTable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := e.model("Example headset", "EXM")

	type step struct {
		to   string
		note *string
		ok   bool
	}

	tests := []struct {
		name  string
		steps []step
	}{
		{"active to maintenance and back", []step{{"maintenance", ptr("Cracked."), true}, {"active", nil, true}}},
		{"active to lost and active again", []step{{"lost", ptr("Gone."), true}, {"active", nil, true}}},
		{"active to retired and active again", []step{{"retired", ptr("Old."), true}, {"active", nil, true}}},
		{"active to active", []step{{"active", nil, false}}},
		{"maintenance to maintenance", []step{{"maintenance", nil, true}, {"maintenance", nil, false}}},
		{"lost to maintenance", []step{{"lost", ptr("Gone."), true}, {"maintenance", nil, false}}},
		{"retired to maintenance", []step{{"retired", ptr("Old."), true}, {"maintenance", nil, false}}},
		{"retired to lost", []step{{"retired", ptr("Old."), true}, {"lost", ptr("Gone."), false}}},
		{"maintenance to retired", []step{{"maintenance", nil, true}, {"retired", ptr("Old."), true}}},
		{"lost to retired", []step{{"lost", ptr("Gone."), true}, {"retired", ptr("Old."), true}}},
	}

	for _, tc := range tests {
		unit := e.units(m.ID, 1)[0]
		wantEvents := 1

		for _, s := range tc.steps {
			got, err := e.store.ChangeLifecycle(ctx, unit.Tag, e.adminID, catalog.LifecycleChange{Lifecycle: s.to, Note: s.note})

			switch {
			case s.ok && err != nil:
				t.Fatalf("%s, the step to %s failed %v", tc.name, s.to, err)
			case s.ok:
				wantEvents++

				if got.Lifecycle != s.to {
					t.Fatalf("%s, the lifecycle is %s", tc.name, got.Lifecycle)
				}
			case !isRule(err, catalog.RuleStatusConflict):
				t.Fatalf("%s, the step to %s gives %v", tc.name, s.to, err)
			}
		}

		if n := e.count(`select count(*)::int from unit_events where unit_id = (select id from units where tag = $1) and admin_id = $2::uuid`,
			unit.Tag, e.adminID); n != wantEvents {
			t.Fatalf("%s wrote %d events, want %d", tc.name, n, wantEvents)
		}
	}

	// a step to lost without a note fails the rule of the field before anything changes
	unit := e.units(m.ID, 1)[0]

	_, err := e.store.ChangeLifecycle(ctx, unit.Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "lost"})

	var invalid *catalog.ValidationError
	if !errors.As(err, &invalid) || invalid.Details[0].Field != "note" {
		t.Fatalf("lost without a note gives %v", err)
	}

	// the event keeps the action, the note and the admin, and a step to active clears the note of the unit
	if _, err := e.store.ChangeLifecycle(ctx, unit.Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "lost", Note: ptr("Left on a bus.")}); err != nil {
		t.Fatalf("lost failed %v", err)
	}

	back, err := e.store.ChangeLifecycle(ctx, unit.Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "active", Note: ptr("Found again.")})
	if err != nil || back.LifecycleNote != nil || back.Status != "available" {
		t.Fatalf("the unit is %+v, %v", back, err)
	}

	events, err := e.store.ListUnitEvents(ctx, unit.Tag, catalog.PageRequest{Limit: 10})
	if err != nil || events.Total != 3 {
		t.Fatalf("the events are %+v, %v", events, err)
	}

	newest := events.Items[0]
	if newest.Action != "reactivated" || newest.Note == nil || *newest.Note != "Found again." || newest.AdminName == nil || *newest.AdminName != "Example Admin" ||
		newest.TicketNumber != nil || events.Items[1].Action != "lost" || events.Items[2].Action != "added" {
		t.Fatalf("the events are %+v", events.Items)
	}
}

func TestARetiredModelRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := e.model("Example headset", "EXM")
	units := e.units(m.ID, 2)

	// an open ticket asks for the model in each of the four open statuses
	for _, status := range []string{"diajukan", "disetujui", "siap_diambil", "dipinjam"} {
		id := e.ticket(status, heldItem{m.ID, 1, nil})

		if _, err := e.store.UpdateModel(ctx, m.ID, catalog.ModelPatch{Retired: ptr(true)}); !isRule(err, catalog.RuleStatusConflict) {
			t.Fatalf("a ticket in %s gives %v", status, err)
		}

		e.exec(`update tickets set status = 'dikembalikan' where id = $1::uuid`, id)
	}

	retired, err := e.store.UpdateModel(ctx, m.ID, catalog.ModelPatch{Retired: ptr(true)})
	if err != nil || retired.RetiredAt == nil {
		t.Fatalf("the retire failed with %v", err)
	}

	// a repeat keeps the first moment
	again, err := e.store.UpdateModel(ctx, m.ID, catalog.ModelPatch{Retired: ptr(true)})
	if err != nil || again.RetiredAt == nil || *again.RetiredAt != *retired.RetiredAt {
		t.Fatalf("a repeat gives %+v, %v", again, err)
	}

	if _, err := e.store.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "maintenance"}); err != nil {
		t.Fatalf("a unit may still leave active, %v", err)
	}

	// every step to active is refused while the model is retired
	if _, err := e.store.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "active"}); !isRule(err, catalog.RuleStatusConflict) {
		t.Fatalf("a step to active gives %v", err)
	}

	if _, err := e.store.CreateUnits(ctx, m.ID, e.adminID, catalog.UnitInput{Count: ptr(1), Condition: "Good", Place: "Example shelf"}, nil); !isRule(err, catalog.RuleStatusConflict) {
		t.Fatalf("new units for a retired model give %v", err)
	}

	// the catalog hides a retired model, and the admin list shows it only on request
	page, err := e.store.ListCatalog(ctx, catalog.ModelFilter{IncludeRetired: true}, catalog.PageRequest{Limit: 10})
	if err != nil || page.Total != 0 {
		t.Fatalf("the catalog is %+v, %v", page, err)
	}

	hidden, err := e.store.ListModels(ctx, catalog.ModelFilter{}, catalog.PageRequest{Limit: 10})
	if err != nil || hidden.Total != 0 {
		t.Fatalf("the admin list is %+v, %v", hidden, err)
	}

	shown, err := e.store.ListModels(ctx, catalog.ModelFilter{IncludeRetired: true}, catalog.PageRequest{Limit: 10})
	if err != nil || shown.Total != 1 || shown.Items[0].RetiredAt == nil {
		t.Fatalf("the admin list with retired models is %+v, %v", shown, err)
	}

	// the model comes back, and so may its units
	if _, err := e.store.UpdateModel(ctx, m.ID, catalog.ModelPatch{Retired: ptr(false)}); err != nil {
		t.Fatalf("the model comes back with %v", err)
	}

	if _, err := e.store.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "active"}); err != nil {
		t.Fatalf("a step to active failed %v", err)
	}
}

func TestPermanentDeleteOnlyForWhatNoTicketUsed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	unused := e.model("Example headset", "EXM")
	e.units(unused.ID, 2)

	usedItems := e.model("Example camera", "CAM")
	e.ticket("dikembalikan", heldItem{usedItems.ID, 1, ptr(1)})

	usedUnits := e.model("Example tripod", "TRI")
	tripods := e.units(usedUnits.ID, 2)
	id := e.ticket("dikembalikan")
	e.hold(id, tripods[0].Tag)
	e.release(id, tripods[0].Tag)

	if err := e.store.DeleteModel(ctx, usedItems.ID); !isRule(err, catalog.RuleStatusConflict) {
		t.Fatalf("a model in the items of a ticket gives %v", err)
	}

	if err := e.store.DeleteModel(ctx, usedUnits.ID); !isRule(err, catalog.RuleStatusConflict) {
		t.Fatalf("a model with a unit that a ticket used gives %v", err)
	}

	if err := e.store.DeleteUnit(ctx, tripods[0].Tag); !isRule(err, catalog.RuleStatusConflict) {
		t.Fatalf("a unit that a ticket used gives %v", err)
	}

	if err := e.store.DeleteUnit(ctx, tripods[1].Tag); err != nil {
		t.Fatalf("a unit that no ticket used failed with %v", err)
	}

	if err := e.store.DeleteUnit(ctx, tripods[1].Tag); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("a second delete gives %v", err)
	}

	if err := e.store.DeleteModel(ctx, unused.ID); err != nil {
		t.Fatalf("a model that no ticket used failed with %v", err)
	}

	// the units and their events went with the model, and the tag is not reused
	if n := e.count(`select count(*)::int from units where model_id = $1::uuid`, unused.ID); n != 0 {
		t.Fatalf("%d units are left", n)
	}

	if n := e.count(`select count(*)::int from unit_events where unit_id not in (select id from units)`); n != 0 {
		t.Fatalf("%d events are left", n)
	}

	if err := e.store.DeleteModel(ctx, unused.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("a second delete of the model gives %v", err)
	}

	for _, id := range []string{"not-a-uuid", "00000000-0000-4000-8000-000000000001"} {
		if err := e.store.DeleteModel(ctx, id); !errors.Is(err, catalog.ErrNotFound) {
			t.Fatalf("the id %q gives %v", id, err)
		}
	}
}

func TestTwoTransactionsOnTheSameModelsInOppositeOrderNeverDeadlock(t *testing.T) {
	e := newEnv(t)
	a := e.model("Example headset", "EXM")
	b := e.model("Example camera", "CAM")

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	lockBoth := func(first, second string) {
		for range 30 {
			tx, err := e.pool.Begin(context.Background())
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()

				return
			}

			locked, err := catalog.LockModels(context.Background(), tx, first, second)
			if err == nil && locked != 2 {
				err = fmt.Errorf("locked %d models", locked)
			}

			if err == nil {
				err = tx.Commit(context.Background())
			} else {
				_ = tx.Rollback(context.Background())
			}

			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()

				return
			}
		}
	}

	wg.Go(func() { lockBoth(a.ID, b.ID) })
	wg.Go(func() { lockBoth(b.ID, a.ID) })
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("a transaction failed %v", errs)
	}

	// a model that does not exist is not locked and is not an error
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin failed %v", err)
	}

	defer func() { _ = tx.Rollback(context.Background()) }()

	if locked, err := catalog.LockModels(context.Background(), tx, a.ID, a.ID, "00000000-0000-4000-8000-000000000001"); err != nil || locked != 1 {
		t.Fatalf("locked %d, %v", locked, err)
	}
}

func TestEventCursorAcrossPagesWithEqualTimes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := e.model("Example headset", "EXM")
	unit := e.units(m.ID, 1)[0]
	other := e.units(m.ID, 1)[0]

	// nine events of two units that all happen at the same moment, so the time alone cannot order them
	moment := testNow.Add(-time.Hour)

	for i := range 8 {
		target := unit.Tag
		if i%2 == 1 {
			target = other.Tag
		}

		e.exec(`insert into unit_events (unit_id, at, action, admin_id) select id, $2, 'updated', $3::uuid from units where tag = $1`,
			target, moment, e.adminID)
	}

	e.exec(`update unit_events set at = $1`, moment)

	seen := map[string]bool{}
	cursor := ""
	pages := 0

	var order []string

	for {
		page, err := e.store.ListAllUnitEvents(ctx, catalog.PageRequest{Limit: 4, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d failed %v", pages, err)
		}

		if page.Total != 10 {
			t.Fatalf("the total is %d", page.Total)
		}

		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatalf("the event %s came twice", item.ID)
			}

			seen[item.ID] = true
			order = append(order, item.ID)
		}

		pages++

		if page.NextCursor == "" {
			break
		}

		cursor = page.NextCursor
	}

	if len(seen) != 10 || pages != 3 {
		t.Fatalf("%d events on %d pages", len(seen), pages)
	}

	// the order is the id, descending, for rows with the same time
	for i := 1; i < len(order); i++ {
		if order[i-1] <= order[i] {
			t.Fatalf("the events are not in descending order of id %v", order)
		}
	}

	// the history of one unit is a page of its own
	own, err := e.store.ListUnitEvents(ctx, unit.Tag, catalog.PageRequest{Limit: 100})
	if err != nil || own.Total != 5 || len(own.Items) != 5 {
		t.Fatalf("the events of the unit are %+v, %v", own, err)
	}

	if _, err := e.store.ListUnitEvents(ctx, "MGM-EXM-099", catalog.PageRequest{Limit: 10}); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("the events of an unknown unit give %v", err)
	}
}

func TestCursorsAreSignedBoundToTheirListAndPageEveryList(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	for i := range 5 {
		m := e.model(fmt.Sprintf("Example model %d", i), fmt.Sprintf("EX%d", i))
		e.units(m.ID, 1)
	}

	for _, name := range []string{"Beta category", "alpha category", "Gamma category"} {
		if _, err := e.store.CreateCategory(ctx, catalog.CategoryInput{Name: name, Tone: "red"}, nil); err != nil {
			t.Fatalf("add the category failed %v", err)
		}
	}

	collect := func(name string, next func(cursor string) (items []string, after string, total int, err error)) []string {
		var all []string

		cursor := ""

		for range 10 {
			items, after, total, err := next(cursor)
			if err != nil {
				t.Fatalf("%s failed %v", name, err)
			}

			all = append(all, items...)

			if after == "" {
				if total != len(all) {
					t.Fatalf("%s has the total %d for %d items", name, total, len(all))
				}

				return all
			}

			cursor = after
		}

		t.Fatalf("%s never ends", name)

		return nil
	}

	units := collect("units", func(cursor string) ([]string, string, int, error) {
		page, err := e.store.ListUnits(ctx, catalog.UnitFilter{}, catalog.PageRequest{Limit: 2, Cursor: cursor})

		return tags(page.Items), page.NextCursor, page.Total, err
	})

	if strings.Join(units, ",") != "MGM-EX0-001,MGM-EX1-001,MGM-EX2-001,MGM-EX3-001,MGM-EX4-001" {
		t.Fatalf("the units are %v", units)
	}

	models := collect("models", func(cursor string) ([]string, string, int, error) {
		page, err := e.store.ListModels(ctx, catalog.ModelFilter{}, catalog.PageRequest{Limit: 2, Cursor: cursor})

		var names []string
		for _, m := range page.Items {
			names = append(names, m.Name)
		}

		return names, page.NextCursor, page.Total, err
	})

	if strings.Join(models, ",") != "Example model 0,Example model 1,Example model 2,Example model 3,Example model 4" {
		t.Fatalf("the models are %v", models)
	}

	categories := collect("categories", func(cursor string) ([]string, string, int, error) {
		page, err := e.store.ListCategories(ctx, catalog.PageRequest{Limit: 3, Cursor: cursor})

		var names []string
		for _, c := range page.Items {
			names = append(names, c.Name)
		}

		return names, page.NextCursor, page.Total, err
	})

	// the order ignores the letter case
	if strings.Join(categories, ",") != "alpha category,Beta category,Example category,Gamma category" {
		t.Fatalf("the categories are %v", categories)
	}

	// a cursor of the units does not open the list of models, and a made up cursor opens nothing
	first, err := e.store.ListUnits(ctx, catalog.UnitFilter{}, catalog.PageRequest{Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("the first page is %+v, %v", first, err)
	}

	if _, err := e.store.ListModels(ctx, catalog.ModelFilter{}, catalog.PageRequest{Limit: 1, Cursor: first.NextCursor}); !errors.Is(err, pagination.ErrCursor) {
		t.Fatalf("a cursor of another list gives %v", err)
	}

	if _, err := e.store.ListCategories(ctx, catalog.PageRequest{Limit: 1, Cursor: "bWFkZSB1cA.bWFkZSB1cA"}); !errors.Is(err, pagination.ErrCursor) {
		t.Fatalf("a made up cursor gives %v", err)
	}
}

func TestListFiltersAndTheStatusOfAUnit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	headset := e.model("Example headset", "EXM")
	camera, err := e.store.CreateModel(ctx, catalog.NewModel{
		Name: "Example camera", NewCategory: &catalog.CategoryInput{Name: "Optics", Tone: "green"}, Description: ptr("Takes still pictures"), Prefix: "CAM",
	}, nil)
	if err != nil {
		t.Fatalf("add the camera failed %v", err)
	}

	hs := e.units(headset.ID, 3)
	cm := e.units(camera.ID, 1)

	if _, err := e.store.UpdateUnit(ctx, cm[0].Tag, e.adminID, catalog.UnitPatch{SerialSet: true, Serial: ptr("CAMSERIAL9")}); err != nil {
		t.Fatalf("set the serial failed %v", err)
	}

	id := e.ticket("dipinjam", heldItem{headset.ID, 1, ptr(1)})
	e.hold(id, hs[0].Tag)

	if _, err := e.store.ChangeLifecycle(ctx, hs[1].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: "maintenance"}); err != nil {
		t.Fatalf("maintenance failed %v", err)
	}

	count := func(f catalog.UnitFilter) int {
		page, err := e.store.ListUnits(ctx, f, catalog.PageRequest{Limit: 100})
		if err != nil {
			t.Fatalf("list with %+v failed %v", f, err)
		}

		return page.Total
	}

	tests := []struct {
		name string
		f    catalog.UnitFilter
		want int
	}{
		{"every unit", catalog.UnitFilter{}, 4},
		{"borrowed", catalog.UnitFilter{Status: "borrowed"}, 1},
		{"maintenance", catalog.UnitFilter{Status: "maintenance"}, 1},
		{"available", catalog.UnitFilter{Status: "available"}, 2},
		{"retired", catalog.UnitFilter{Status: "retired"}, 0},
		{"a model", catalog.UnitFilter{ModelID: camera.ID}, 1},
		{"a category", catalog.UnitFilter{CategoryID: headset.CategoryID}, 3},
		{"a category and a status", catalog.UnitFilter{CategoryID: headset.CategoryID, Status: "available"}, 1},
		{"a model of another category", catalog.UnitFilter{ModelID: camera.ID, CategoryID: headset.CategoryID}, 0},
		{"a tag in lower case", catalog.UnitFilter{Query: "mgm-exm-00"}, 3},
		{"a model name", catalog.UnitFilter{Query: "CAMERA"}, 1},
		{"a serial", catalog.UnitFilter{Query: "camserial"}, 1},
		{"a percent sign is no wildcard", catalog.UnitFilter{Query: "%%"}, 0},
	}

	for _, tc := range tests {
		if got := count(tc.f); got != tc.want {
			t.Fatalf("%s gives %d, want %d", tc.name, got, tc.want)
		}
	}

	// the guest catalog also searches the description, and the admin list does not
	byText, err := e.store.ListCatalog(ctx, catalog.ModelFilter{Query: "still pictures"}, catalog.PageRequest{Limit: 10})
	if err != nil || byText.Total != 1 || byText.Items[0].Name != "Example camera" {
		t.Fatalf("the catalog search gives %+v, %v", byText, err)
	}

	byText2, err := e.store.ListModels(ctx, catalog.ModelFilter{Query: "still pictures"}, catalog.PageRequest{Limit: 10})
	if err != nil || byText2.Total != 0 {
		t.Fatalf("the admin search gives %+v, %v", byText2, err)
	}

	byCategory, err := e.store.ListCatalog(ctx, catalog.ModelFilter{CategoryID: camera.CategoryID}, catalog.PageRequest{Limit: 10})
	if err != nil || byCategory.Total != 1 {
		t.Fatalf("the catalog by category gives %+v, %v", byCategory, err)
	}
}

func TestTheAssetPageShowsMoreToAnAdmin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := e.model("Example headset", "EXM")
	unit := e.units(m.ID, 2)[0]

	if _, err := e.store.UpdateUnit(ctx, unit.Tag, e.adminID, catalog.UnitPatch{SerialSet: true, Serial: ptr("SN-0001")}); err != nil {
		t.Fatalf("set the serial failed %v", err)
	}

	public, err := e.store.GetPublicAsset(ctx, unit.Tag)
	if err != nil || public.Status != "available" || public.Model.Name != "Example headset" || public.Model.Category.Name != "Example category" ||
		public.Model.Available != 2 || public.Model.CategoryID != m.CategoryID {
		t.Fatalf("the public page is %+v, %v", public, err)
	}

	// the ticket picked up on 1 November for 3 days is due on 4 November, the time zone is Asia/Jakarta
	id := e.ticket("dipinjam", heldItem{m.ID, 1, ptr(1)})
	e.hold(id, unit.Tag)

	tests := []struct {
		name string
		now  time.Time
		want string
	}{
		{"the day before the due date", time.Date(2026, 11, 3, 16, 59, 0, 0, time.UTC), "dipinjam"},
		{"the due date at the end of the day in Jakarta", time.Date(2026, 11, 4, 16, 59, 0, 0, time.UTC), "dipinjam"},
		{"the day after in Jakarta, which is the same day in UTC", time.Date(2026, 11, 4, 17, 0, 0, 0, time.UTC), "terlambat"},
		{"a week later", time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC), "terlambat"},
	}

	for _, tc := range tests {
		e.now = tc.now

		admin, err := e.store.GetAdminAsset(ctx, unit.Tag)
		if err != nil || admin.RelatedTicket == nil || admin.RelatedTicket.EffectiveStatus != tc.want || admin.RelatedTicket.Number != "MGM-2611-0001-K7QX" ||
			admin.RelatedTicket.RequesterName == nil || *admin.RelatedTicket.RequesterName != "Example Borrower" {
			t.Fatalf("%s gives %+v, %v", tc.name, admin.RelatedTicket, err)
		}

		if admin.Serial == nil || *admin.Serial != "SN-0001" || admin.Condition != "Good" || admin.Place != "Example shelf" || admin.Status != "borrowed" {
			t.Fatalf("the admin page is %+v", admin)
		}
	}

	e.release(id, unit.Tag)

	admin, err := e.store.GetAdminAsset(ctx, unit.Tag)
	if err != nil || admin.RelatedTicket != nil || admin.Status != "available" {
		t.Fatalf("after the return the page is %+v, %v", admin, err)
	}

	for _, tag := range []string{"MGM-EXM-099", "bad tag", ""} {
		if _, err := e.store.GetPublicAsset(ctx, tag); !errors.Is(err, catalog.ErrNotFound) {
			t.Fatalf("the tag %q gives %v", tag, err)
		}
	}
}

func TestUpdateModelChangesTheFieldsThatTheBodyNames(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := e.model("Example headset", "EXM")

	other, err := e.store.CreateCategory(ctx, catalog.CategoryInput{Name: "Another category", Tone: "yellow"}, nil)
	if err != nil {
		t.Fatalf("add the category failed %v", err)
	}

	got, err := e.store.UpdateModel(ctx, m.ID, catalog.ModelPatch{Description: ptr("A new text."), CategoryID: &other.ID})
	if err != nil || got.Description != "A new text." || got.CategoryID != other.ID || got.Name != "Example headset" || got.Prefix != "EXM" {
		t.Fatalf("the model is %+v, %v", got, err)
	}

	_, err = e.store.UpdateModel(ctx, m.ID, catalog.ModelPatch{CategoryID: ptr("00000000-0000-4000-8000-000000000001")})

	var invalid *catalog.ValidationError
	if !errors.As(err, &invalid) || invalid.Details[0].Code != catalog.DetailUnknownReference {
		t.Fatalf("an unknown category gives %v", err)
	}

	if _, err := e.store.UpdateModel(ctx, "00000000-0000-4000-8000-000000000001", catalog.ModelPatch{Name: ptr("Anything")}); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("an unknown model gives %v", err)
	}
}
