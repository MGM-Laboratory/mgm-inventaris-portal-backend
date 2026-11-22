package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
)

const eventsKind = "unit-events"

// ListUnitEvents returns the history of one unit, newest first.
func (s *Store) ListUnitEvents(ctx context.Context, tag string, req PageRequest) (pagination.Page[UnitEvent], error) {
	if !ValidTag(tag) {
		return pagination.Page[UnitEvent]{}, ErrNotFound
	}

	var exists bool
	if err := s.db.QueryRow(ctx, `select exists (select 1 from units where tag = $1)`, tag).Scan(&exists); err != nil {
		return pagination.Page[UnitEvent]{}, fmt.Errorf("read the unit %w", err)
	}

	if !exists {
		return pagination.Page[UnitEvent]{}, ErrNotFound
	}

	return s.listEvents(ctx, tag, req)
}

// ListAllUnitEvents returns the history of every unit, newest first.
func (s *Store) ListAllUnitEvents(ctx context.Context, req PageRequest) (pagination.Page[UnitEvent], error) {
	return s.listEvents(ctx, "", req)
}

// listEvents sorts by time and then by id, so rows with the same time keep one fixed order and a cursor never skips or repeats one.
// The cursor holds the time with its full precision.
func (s *Store) listEvents(ctx context.Context, tag string, req PageRequest) (pagination.Page[UnitEvent], error) {
	keys, err := s.codec.Decode(eventsKind, req.Cursor, 2)
	if err != nil {
		return pagination.Page[UnitEvent]{}, fmt.Errorf("read the cursor %w", err)
	}

	var afterAt, afterID any

	if keys != nil {
		afterAt, afterID = keys[0], keys[1]
	}

	rows, err := s.db.Query(ctx, `
		select e.id::text, u.tag, e.at, e.action, a.name, e.note, t.number
		from unit_events e
		join units u on u.id = e.unit_id
		left join admins a on a.id = e.admin_id
		left join tickets t on t.id = e.ticket_id
		where ($1::text = '' or u.tag = $1::text)
		  and ($2::timestamptz is null or (e.at, e.id) < ($2::timestamptz, $3::uuid))
		order by e.at desc, e.id desc limit $4`, tag, afterAt, afterID, req.Limit+1)
	if err != nil {
		return pagination.Page[UnitEvent]{}, fmt.Errorf("list the events %w", err)
	}

	defer rows.Close()

	items := []UnitEvent{}
	moments := []time.Time{}

	for rows.Next() {
		var (
			e  UnitEvent
			at time.Time
		)

		if err := rows.Scan(&e.ID, &e.Tag, &at, &e.Action, &e.AdminName, &e.Note, &e.TicketNumber); err != nil {
			return pagination.Page[UnitEvent]{}, fmt.Errorf("read an event %w", err)
		}

		e.At = timeText(at)
		items = append(items, e)
		moments = append(moments, at.UTC())
	}

	if err := rows.Err(); err != nil {
		return pagination.Page[UnitEvent]{}, fmt.Errorf("list the events %w", err)
	}

	var total int

	err = s.db.QueryRow(ctx, `
		select count(*)::int from unit_events e join units u on u.id = e.unit_id where ($1::text = '' or u.tag = $1::text)`, tag).Scan(&total)
	if err != nil {
		return pagination.Page[UnitEvent]{}, fmt.Errorf("count the events %w", err)
	}

	items, more := pagination.Trim(items, req.Limit)
	page := pagination.Page[UnitEvent]{Items: items, Total: total}

	if more {
		last := len(items) - 1
		page.NextCursor = s.codec.Encode(eventsKind, moments[last].Format(time.RFC3339Nano), items[last].ID)
	}

	return page, nil
}
