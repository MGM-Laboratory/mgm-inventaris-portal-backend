package catalog

import (
	"context"
	"fmt"
	"time"
)

// StockLevel is the stock of a model and the quantity that its tickets hold, counted as docs/data-model.md says.
type StockLevel struct {
	Total int
	Held  int
}

// Stock returns the level of each model that exists. Unlike Available it never cuts a shortage to 0, so a caller can tell a stock that
// the tickets hold exactly from one that fell below the hold. The caller holds a lock on the models when the answer has to stay true.
func Stock(ctx context.Context, q Querier, ids ...string) (map[string]StockLevel, error) {
	rows, err := q.Query(ctx, `
		select m.id::text,
		       (select count(*) from units u where u.model_id = m.id and u.lifecycle = 'active')::int,
		       coalesce((select sum(ti.approved_qty) from ticket_items ti join tickets t on t.id = ti.ticket_id
		                 where ti.model_id = m.id and t.status in (`+holdingStatuses+`)), 0)::int
		from models m where m.id = any($1::text[]::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the stock %w", err)
	}

	defer rows.Close()

	levels := map[string]StockLevel{}

	for rows.Next() {
		var (
			id    string
			level StockLevel
		)

		if err := rows.Scan(&id, &level.Total, &level.Held); err != nil {
			return nil, fmt.Errorf("read a stock %w", err)
		}

		levels[id] = level
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the stock %w", err)
	}

	return levels, nil
}

// UnitRef is a unit that a handover names by its tag. Held is true while an open row of a ticket holds the unit.
type UnitRef struct {
	ID        string
	Tag       string
	ModelID   string
	Lifecycle string
	Held      bool
}

// LockUnits locks the units with the tags, in the order of the id, and returns the ones that exist. A tag that names no unit is not
// in the result. The caller has locked the models first, which is the order of every change of a unit, so this lock never waits for a
// transaction that waits for the models.
func LockUnits(ctx context.Context, q Querier, tags ...string) ([]UnitRef, error) {
	rows, err := q.Query(ctx, `
		select u.id::text as unit_id, u.tag, u.model_id::text, u.lifecycle,
		       exists (select 1 from ticket_units tu where tu.unit_id = u.id and tu.returned_at is null)
		from units u where u.tag = any($1::text[]) order by u.id for update of u`, tags)
	if err != nil {
		return nil, fmt.Errorf("lock the units %w", err)
	}

	defer rows.Close()

	var refs []UnitRef

	for rows.Next() {
		var ref UnitRef

		if err := rows.Scan(&ref.ID, &ref.Tag, &ref.ModelID, &ref.Lifecycle, &ref.Held); err != nil {
			return nil, fmt.Errorf("read a locked unit %w", err)
		}

		refs = append(refs, ref)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock the units %w", err)
	}

	return refs, nil
}

// RecordHandover writes the event handed_over for each unit of a handover, with the admin and the ticket.
func RecordHandover(ctx context.Context, q Querier, at time.Time, adminID, ticketID string, unitIDs []string) error {
	_, err := q.Exec(ctx, `
		insert into unit_events (unit_id, at, action, admin_id, ticket_id)
		select t.unit_id, $1::timestamptz, 'handed_over', $2::uuid, $3::uuid from unnest($4::text[]::uuid[]) as t(unit_id)`,
		at, adminID, ticketID, unitIDs)
	if err != nil {
		return fmt.Errorf("write the events of the handover %w", err)
	}

	return nil
}

// ReturnedUnit is a unit that came back with the outcome that the admin found. The outcome is one of ReturnOK, ReturnMaintenance and
// ReturnLost, and a lost unit has a note.
type ReturnedUnit struct {
	UnitID  string
	Outcome string
	Note    *string
}

// RecordReturn writes the event returned for each unit of a return. A unit that comes back in maintenance or lost also changes its
// lifecycle and gets the event of that change, one microsecond later, so the history of the unit reads in one fixed order. The caller
// has closed the open rows of the ticket first, so the unit is no longer held when its lifecycle changes.
func RecordReturn(ctx context.Context, q Querier, at time.Time, adminID, ticketID string, units []ReturnedUnit) error {
	ids := make([]string, 0, len(units))
	notes := make([]*string, 0, len(units))

	var (
		changedIDs, outcomes []string
		changedNotes         []*string
	)

	for _, u := range units {
		ids = append(ids, u.UnitID)
		notes = append(notes, u.Note)

		if u.Outcome != ReturnOK {
			changedIDs = append(changedIDs, u.UnitID)
			outcomes = append(outcomes, u.Outcome)
			changedNotes = append(changedNotes, u.Note)
		}
	}

	_, err := q.Exec(ctx, `
		insert into unit_events (unit_id, at, action, admin_id, ticket_id, note)
		select t.unit_id, $1::timestamptz, 'returned', $2::uuid, $3::uuid, t.note
		from unnest($4::text[]::uuid[], $5::text[]) as t(unit_id, note)`, at, adminID, ticketID, ids, notes)
	if err != nil {
		return fmt.Errorf("write the events of the return %w", err)
	}

	if len(changedIDs) == 0 {
		return nil
	}

	_, err = q.Exec(ctx, `
		update units u set lifecycle = t.outcome, lifecycle_note = t.note
		from unnest($1::text[]::uuid[], $2::text[], $3::text[]) as t(unit_id, outcome, note)
		where u.id = t.unit_id`, changedIDs, outcomes, changedNotes)
	if err != nil {
		return fmt.Errorf("change the lifecycle of the returned units %w", err)
	}

	_, err = q.Exec(ctx, `
		insert into unit_events (unit_id, at, action, admin_id, ticket_id, note)
		select t.unit_id, $1::timestamptz, t.outcome, $2::uuid, $3::uuid, t.note
		from unnest($4::text[]::uuid[], $5::text[], $6::text[]) as t(unit_id, outcome, note)`,
		at.Add(time.Microsecond), adminID, ticketID, changedIDs, outcomes, changedNotes)
	if err != nil {
		return fmt.Errorf("write the events of the lifecycle %w", err)
	}

	return nil
}
