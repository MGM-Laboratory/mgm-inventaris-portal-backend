package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/dashboard"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
)

// 03:00 UTC on 17 November is 10:00 on the same day in Jakarta.
var testNow = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

type fixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	number int
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()

	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec failed %v", err)
	}
}

func (f *fixture) id(sql string, args ...any) string {
	f.t.Helper()

	var id string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		f.t.Fatalf("query failed %v", err)
	}

	return id
}

func (f *fixture) category(name, tone string) string {
	return f.id(`insert into categories (name, tone) values ($1, $2) returning id::text`, name, tone)
}

func (f *fixture) model(category, name, prefix string) string {
	return f.id(`insert into models (name, category_id, prefix) values ($1, $2::uuid, $3) returning id::text`, name, category, prefix)
}

// unit adds a unit with the lifecycle. A lost or retired unit gets a note, as the table demands.
func (f *fixture) unit(model, prefix string, n int, lifecycle string) string {
	var note *string

	if lifecycle == "lost" || lifecycle == "retired" {
		text := "Example note."
		note = &text
	}

	return f.id(`
		insert into units (tag, model_id, condition, place, lifecycle, lifecycle_note)
		values ($1, $2::uuid, 'Good', 'Example shelf', $3, $4) returning id::text`, fmt.Sprintf("MGM-%s-%03d", prefix, n), model, lifecycle, note)
}

func (f *fixture) ticket(status, pickup string, days int, approved *int) string {
	f.t.Helper()

	f.number++

	date, err := time.Parse("2006-01-02", pickup)
	if err != nil {
		f.t.Fatalf("the date %q is not a date", pickup)
	}

	return f.id(`
		insert into tickets (number, status, requester_name, requester_nim_nip, requester_email, purpose, pickup_date, duration_days, approved_duration_days)
		values ($1, $2, 'Example Borrower', '245150200000001', 'example@student.ub.ac.id', 'Example purpose', $3::date, $4, $5) returning id::text`,
		fmt.Sprintf("MGM-2611-%04d-AAAA", f.number), status, date, days, approved)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	return &fixture{t: t, pool: dbtest.New(t)}
}

func TestTheCountsOfAnEmptyDatabaseAreZerosAndAnEmptyList(t *testing.T) {
	f := newFixture(t)

	got, err := dashboard.NewStore(f.pool, func() time.Time { return testNow }).Counts(context.Background())
	if err != nil {
		t.Fatalf("count %v", err)
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal %v", err)
	}

	const want = `{"tickets":{"diajukan":0,"disetujui":0,"siapDiambil":0,"dipinjam":0,"terlambat":0},` +
		`"units":{"available":0,"borrowed":0,"maintenance":0,"lost":0,"retired":0},` +
		`"byStatus":{"diajukan":0,"disetujui":0,"siapDiambil":0,"dipinjam":0,"terlambat":0,"dikembalikan":0,"dibatalkan":0,"ditolak":0},"byCategory":[]}`
	if string(body) != want {
		t.Fatalf("the counts are %s", body)
	}
}

func TestTheCountsFollowTheTicketsTheUnitsAndTheCategories(t *testing.T) {
	f := newFixture(t)

	audio := f.category("Audio", "blue")
	video := f.category("Video", "red")
	f.category("Empty", "green")

	headset := f.model(audio, "Example headset", "AUD")
	camera := f.model(video, "Example camera", "VID")

	free := f.unit(headset, "AUD", 1, "active")
	borrowed := f.unit(headset, "AUD", 2, "active")
	f.unit(headset, "AUD", 3, "maintenance")
	f.unit(headset, "AUD", 4, "lost")
	f.unit(camera, "VID", 1, "active")
	f.unit(camera, "VID", 2, "retired")
	lateUnit := f.unit(camera, "VID", 3, "active")

	two, three := 2, 3

	// the open tickets. A loan that is due today is not late, because a loan is late after its due date.
	f.ticket("diajukan", "2026-11-20", 3, nil)
	f.ticket("diajukan", "2026-11-21", 3, nil)
	f.ticket("disetujui", "2026-11-18", 5, &two)
	f.ticket("siap_diambil", "2026-11-17", 1, nil)
	loan := f.ticket("dipinjam", "2026-11-15", 5, nil)
	f.ticket("dipinjam", "2026-11-12", 5, nil)
	late := f.ticket("dipinjam", "2026-11-01", 5, &three)

	// the closed tickets, one of which returned the first unit, so that unit is available again
	returned := f.ticket("dikembalikan", "2026-10-01", 2, nil)
	f.ticket("dikembalikan", "2026-10-03", 2, nil)
	f.ticket("dibatalkan", "2026-10-05", 2, nil)
	f.ticket("ditolak", "2026-10-06", 2, nil)

	f.exec(`insert into ticket_units (ticket_id, unit_id) values ($1::uuid, $2::uuid), ($3::uuid, $4::uuid)`, loan, borrowed, late, lateUnit)
	f.exec(`insert into ticket_units (ticket_id, unit_id, returned_at, return_outcome) values ($1::uuid, $2::uuid, now(), 'ok')`, returned, free)

	got, err := dashboard.NewStore(f.pool, func() time.Time { return testNow }).Counts(context.Background())
	if err != nil {
		t.Fatalf("count %v", err)
	}

	wantOpen := dashboard.OpenTickets{Submitted: 2, Approved: 1, Ready: 1, Borrowed: 2, Late: 1}
	if got.Tickets != wantOpen {
		t.Fatalf("the open tickets are %+v, want %+v", got.Tickets, wantOpen)
	}

	if want := (dashboard.AllTickets{OpenTickets: wantOpen, Returned: 2, Cancelled: 1, Rejected: 1}); got.ByStatus != want {
		t.Fatalf("the tickets by status are %+v, want %+v", got.ByStatus, want)
	}

	if want := (dashboard.Units{Available: 2, Borrowed: 2, Maintenance: 1, Lost: 1, Retired: 1}); got.Units != want {
		t.Fatalf("the units are %+v, want %+v", got.Units, want)
	}

	want := []dashboard.CategoryCounts{
		{CategoryID: audio, Name: "Audio", Tone: "blue", Total: 4, Units: dashboard.Units{Available: 1, Borrowed: 1, Maintenance: 1, Lost: 1}},
		{Name: "Empty", Tone: "green"},
		{CategoryID: video, Name: "Video", Tone: "red", Total: 3, Units: dashboard.Units{Available: 1, Borrowed: 1, Retired: 1}},
	}

	if len(got.ByCategory) != len(want) {
		t.Fatalf("the categories are %+v", got.ByCategory)
	}

	for i, w := range want {
		if w.CategoryID == "" {
			w.CategoryID = got.ByCategory[i].CategoryID
		}

		if got.ByCategory[i] != w {
			t.Errorf("the category %d is %+v, want %+v", i, got.ByCategory[i], w)
		}
	}

	// the totals of the categories add up to the units
	sum := 0
	for _, c := range got.ByCategory {
		sum += c.Total
	}

	if sum != got.Units.Available+got.Units.Borrowed+got.Units.Maintenance+got.Units.Lost+got.Units.Retired {
		t.Fatalf("the categories hold %d units and the units hold %+v", sum, got.Units)
	}
}

func TestALoanIsLateOnlyAfterItsDueDateInJakarta(t *testing.T) {
	f := newFixture(t)

	// due on 17 November in Jakarta
	f.ticket("dipinjam", "2026-11-12", 5, nil)

	for name, tc := range map[string]struct {
		now  time.Time
		late int
	}{
		// 16:59 UTC on 17 November is 23:59 in Jakarta, still the due day
		"the due day, late evening in Jakarta": {time.Date(2026, 11, 17, 16, 59, 0, 0, time.UTC), 0},
		// 17:00 UTC on 17 November is midnight on 18 November in Jakarta
		"the day after in Jakarta": {time.Date(2026, 11, 17, 17, 0, 0, 0, time.UTC), 1},
		"early on the due day":     {time.Date(2026, 11, 16, 17, 0, 0, 0, time.UTC), 0},
	} {
		now := tc.now

		got, err := dashboard.NewStore(f.pool, func() time.Time { return now }).Counts(context.Background())
		if err != nil || got.Tickets.Late != tc.late || got.Tickets.Borrowed != 1-tc.late || got.ByStatus.Late != tc.late {
			t.Errorf("%s gives %+v, %v, want %d late", name, got.Tickets, err, tc.late)
		}
	}
}
