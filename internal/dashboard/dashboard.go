// Package dashboard reads the counts that the dashboard of an admin shows. It only reads. It crosses the tables of the assets and of the
// tickets on purpose, because the counts need both, and it owns no table. Every number comes from one query, and each query returns a
// bounded result, which is a handful of rows or one row for each category.
package dashboard

import (
	"context"
	"fmt"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
)

// jakarta is the time zone of a due date. Indonesia has no daylight saving time in this zone, so a fixed offset is the same as
// Asia/Jakarta and needs no time zone database in the image.
var jakarta = time.FixedZone("Asia/Jakarta", 7*60*60)

// OpenTickets counts the open tickets of each effective status. A late ticket counts as terlambat and not as dipinjam.
type OpenTickets struct {
	Submitted int `json:"diajukan"`
	Approved  int `json:"disetujui"`
	Ready     int `json:"siapDiambil"`
	Borrowed  int `json:"dipinjam"`
	Late      int `json:"terlambat"`
}

// AllTickets counts the tickets of every effective status, the closed ones included.
type AllTickets struct {
	OpenTickets
	Returned  int `json:"dikembalikan"`
	Cancelled int `json:"dibatalkan"`
	Rejected  int `json:"ditolak"`
}

// Units counts the units of each status that a unit shows.
type Units struct {
	Available   int `json:"available"`
	Borrowed    int `json:"borrowed"`
	Maintenance int `json:"maintenance"`
	Lost        int `json:"lost"`
	Retired     int `json:"retired"`
}

// CategoryCounts counts the units of one category. Total is the sum of the five status counts.
type CategoryCounts struct {
	CategoryID string `json:"categoryId"`
	Name       string `json:"name"`
	Tone       string `json:"tone"`
	Total      int    `json:"total"`
	Units
}

// Counts is the answer of the dashboard.
type Counts struct {
	Tickets    OpenTickets      `json:"tickets"`
	Units      Units            `json:"units"`
	ByStatus   AllTickets       `json:"byStatus"`
	ByCategory []CategoryCounts `json:"byCategory"`
}

// Store reads the counts. It takes the time from a function, so a test passes its own clock.
type Store struct {
	db  catalog.Querier
	now func() time.Time
}

// NewStore returns a store. A nil clock means time.Now.
func NewStore(db catalog.Querier, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}

	return &Store{db: db, now: now}
}

// ticketsByStatus counts all tickets by effective status in one pass over the table. The due date is the pickup date plus the approved
// days or the requested days, and a ticket in the status dipinjam is late when today in Asia/Jakarta is after it. Today is the first
// argument, as a date.
const ticketsByStatus = `
	select case when t.status = 'dipinjam' and $1::date > t.pickup_date + coalesce(t.approved_duration_days, t.duration_days)
	            then 'terlambat' else t.status end as effective, count(*)::int
	from tickets t group by 1`

// unitsByStatus counts the units by the status that a unit shows, which is its lifecycle unless it is active, and then borrowed while
// an open row holds it and available otherwise. The partial unique index on the open rows serves the exists test.
const unitsByStatus = `
	select case when u.lifecycle <> 'active' then u.lifecycle
	            when exists (select 1 from ticket_units tu where tu.unit_id = u.id and tu.returned_at is null) then 'borrowed'
	            else 'available' end as status, count(*)::int
	from units u group by 1`

// unitsByCategory counts the units of every category with the same statuses. A category with no unit has zeros. The open rows join on
// the unit, and a unit has at most one, so a unit counts once.
const unitsByCategory = `
	select c.id::text, c.name, c.tone,
	       count(u.id) filter (where u.lifecycle = 'active' and ot.unit_id is null)::int,
	       count(u.id) filter (where u.lifecycle = 'active' and ot.unit_id is not null)::int,
	       count(u.id) filter (where u.lifecycle = 'maintenance')::int,
	       count(u.id) filter (where u.lifecycle = 'lost')::int,
	       count(u.id) filter (where u.lifecycle = 'retired')::int,
	       count(u.id)::int
	from categories c
	left join models m on m.category_id = c.id
	left join units u on u.model_id = m.id
	left join ticket_units ot on ot.unit_id = u.id and ot.returned_at is null
	group by c.id, c.name, c.tone
	order by lower(c.name), c.id`

// Counts reads the numbers of the dashboard. The three queries run one after the other on the same pool, so the numbers come from
// moments that are a few milliseconds apart, and nothing here needs one snapshot.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var out Counts

	today := s.now().In(jakarta)
	day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)

	rows, err := s.db.Query(ctx, ticketsByStatus, day)
	if err != nil {
		return Counts{}, fmt.Errorf("count the tickets %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			status string
			n      int
		)

		if err := rows.Scan(&status, &n); err != nil {
			return Counts{}, fmt.Errorf("read a ticket count %w", err)
		}

		out.ByStatus.add(status, n)
	}

	if err := rows.Err(); err != nil {
		return Counts{}, fmt.Errorf("count the tickets %w", err)
	}

	rows.Close()

	out.Tickets = out.ByStatus.OpenTickets

	if out.Units, err = s.units(ctx); err != nil {
		return Counts{}, err
	}

	if out.ByCategory, err = s.categories(ctx); err != nil {
		return Counts{}, err
	}

	return out, nil
}

// add counts n tickets of the status. The status is one of the effective statuses, which the check constraint of the table limits.
func (a *AllTickets) add(status string, n int) {
	switch status {
	case "diajukan":
		a.Submitted += n
	case "disetujui":
		a.Approved += n
	case "siap_diambil":
		a.Ready += n
	case "dipinjam":
		a.Borrowed += n
	case "terlambat":
		a.Late += n
	case "dikembalikan":
		a.Returned += n
	case "dibatalkan":
		a.Cancelled += n
	case "ditolak":
		a.Rejected += n
	}
}

func (s *Store) units(ctx context.Context) (Units, error) {
	rows, err := s.db.Query(ctx, unitsByStatus)
	if err != nil {
		return Units{}, fmt.Errorf("count the units %w", err)
	}

	defer rows.Close()

	var out Units

	for rows.Next() {
		var (
			status string
			n      int
		)

		if err := rows.Scan(&status, &n); err != nil {
			return Units{}, fmt.Errorf("read a unit count %w", err)
		}

		switch status {
		case "available":
			out.Available = n
		case "borrowed":
			out.Borrowed = n
		case "maintenance":
			out.Maintenance = n
		case "lost":
			out.Lost = n
		case "retired":
			out.Retired = n
		}
	}

	if err := rows.Err(); err != nil {
		return Units{}, fmt.Errorf("count the units %w", err)
	}

	return out, nil
}

func (s *Store) categories(ctx context.Context) ([]CategoryCounts, error) {
	rows, err := s.db.Query(ctx, unitsByCategory)
	if err != nil {
		return nil, fmt.Errorf("count the units of the categories %w", err)
	}

	defer rows.Close()

	out := []CategoryCounts{}

	for rows.Next() {
		var c CategoryCounts

		err := rows.Scan(&c.CategoryID, &c.Name, &c.Tone, &c.Available, &c.Borrowed, &c.Maintenance, &c.Lost, &c.Retired, &c.Total)
		if err != nil {
			return nil, fmt.Errorf("read a category count %w", err)
		}

		out = append(out, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count the units of the categories %w", err)
	}

	return out, nil
}
