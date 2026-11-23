package ticket

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
)

const (
	adminTicketsKind = "admin-tickets"
	eventsKind       = "ticket-events"
)

// head is what a step needs to know about a ticket before it locks anything. The row is read and not locked.
type head struct {
	ID      string
	Status  string
	Version int
	Days    int
}

// readHead reads the head of the ticket that a condition selects. A missing ticket gives ErrNotFound. The condition is part of the
// code, and the request values are arguments.
func readHead(ctx context.Context, q catalog.Querier, condition string, args ...any) (head, error) {
	var h head

	err := q.QueryRow(ctx, `select id::text, status, version, duration_days from tickets where `+condition, args...).Scan(&h.ID, &h.Status, &h.Version, &h.Days)
	if errors.Is(err, pgx.ErrNoRows) {
		return head{}, ErrNotFound
	}

	if err != nil {
		return head{}, fmt.Errorf("read the ticket %w", err)
	}

	return h, nil
}

// headByNumber reads the head of the ticket with the number. A text that is not a number names no ticket.
func headByNumber(ctx context.Context, q catalog.Querier, number string) (head, error) {
	if !ValidNumber(number) {
		return head{}, ErrNotFound
	}

	return readHead(ctx, q, `number = $1`, number)
}

// refuse says why a ticket cannot take a step, or returns nil when it can. The status is checked before the version, so the retry of a
// request whose answer was lost gets a status conflict that names what really happened, and a stale version only counts while the
// status is still right.
func refuse(h head, from []string, version int) error {
	if !slices.Contains(from, h.Status) {
		return &catalog.RuleError{Kind: catalog.RuleStatusConflict, Message: "A ticket in the status " + h.Status + " does not allow this step."}
	}

	if h.Version != version {
		return &catalog.RuleError{Kind: catalog.RuleVersionConflict, Message: "The ticket changed after you read it. Reload it and try again."}
	}

	return nil
}

// explain says why a guarded update changed no row. It reads the ticket again, so the answer names the change that won.
func explain(ctx context.Context, q catalog.Querier, id string, from []string, version int) error {
	h, err := readHead(ctx, q, `id = $1::uuid`, id)
	if err != nil {
		return err
	}

	if err := refuse(h, from, version); err != nil {
		return err
	}

	return &catalog.RuleError{Kind: catalog.RuleStatusConflict, Message: "The ticket changed. Reload it and try again."}
}

// readAdmin reads the ticket that a condition selects, with the history that names the admins and the units that the ticket holds.
func (s *Store) readAdmin(ctx context.Context, q catalog.Querier, condition string, args ...any) (AdminTicket, error) {
	t, _, err := scanTicket(q.QueryRow(ctx, `select `+ticketColumns+` from tickets t where `+condition, args...), s.now())
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminTicket{}, ErrNotFound
	}

	if err != nil {
		return AdminTicket{}, fmt.Errorf("read the ticket %w", err)
	}

	summaries, err := fillAdmin(ctx, q, []Ticket{t})
	if err != nil {
		return AdminTicket{}, err
	}

	units, err := readUnits(ctx, q, []string{t.ID})
	if err != nil {
		return AdminTicket{}, err
	}

	// a ticket with no unit has an empty list and not null, so a client reads the same shape for every ticket
	held := units[t.ID]
	if held == nil {
		held = []TicketUnit{}
	}

	return AdminTicket{AdminSummary: summaries[0], Units: held}, nil
}

// fillAdmin adds the items and the history with the names of the admins to the tickets, with two queries whatever the number of tickets.
func fillAdmin(ctx context.Context, q catalog.Querier, tickets []Ticket) ([]AdminSummary, error) {
	if err := fillItems(ctx, q, tickets); err != nil {
		return nil, err
	}

	ids, at := indexOf(tickets)

	out := make([]AdminSummary, len(tickets))

	for i, t := range tickets {
		out[i] = AdminSummary{Ticket: t, History: []AdminHistoryEntry{}}
	}

	rows, err := q.Query(ctx, `
		select h.ticket_id::text, h.at, h.status, h.actor_kind, a.name, h.note
		from ticket_history h left join admins a on a.id = h.admin_id
		where h.ticket_id = any($1::text[]::uuid[])
		order by h.ticket_id, h.at, h.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the history %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			ticketID string
			moment   time.Time
			h        AdminHistoryEntry
		)

		if err := rows.Scan(&ticketID, &moment, &h.Status, &h.ActorKind, &h.ActorName, &h.Note); err != nil {
			return nil, fmt.Errorf("read a history entry %w", err)
		}

		h.At = timeText(moment)
		out[at[ticketID]].History = append(out[at[ticketID]].History, h)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the history %w", err)
	}

	return out, nil
}

// readUnits reads the units of the tickets, by ticket id, in the order of the handover and then of the tag. A ticket with no unit has no
// entry.
func readUnits(ctx context.Context, q catalog.Querier, ids []string) (map[string][]TicketUnit, error) {
	rows, err := q.Query(ctx, `
		select tu.ticket_id::text, u.tag, tu.handed_over_at, tu.returned_at, tu.return_outcome, tu.return_note
		from ticket_units tu join units u on u.id = tu.unit_id
		where tu.ticket_id = any($1::text[]::uuid[])
		order by tu.ticket_id, tu.handed_over_at, u.tag`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the units of the tickets %w", err)
	}

	defer rows.Close()

	out := map[string][]TicketUnit{}

	for rows.Next() {
		var (
			ticketID string
			unit     TicketUnit
			handed   time.Time
			returned *time.Time
		)

		if err := rows.Scan(&ticketID, &unit.Tag, &handed, &returned, &unit.ReturnOutcome, &unit.ReturnNote); err != nil {
			return nil, fmt.Errorf("read a unit of a ticket %w", err)
		}

		unit.HandedOverAt = timeText(handed)
		unit.ReturnedAt = timeTextPtr(returned)
		out[ticketID] = append(out[ticketID], unit)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the units of the tickets %w", err)
	}

	return out, nil
}

// GetAdmin returns any ticket with the units that it holds. A number that names no ticket gives ErrNotFound.
func (s *Store) GetAdmin(ctx context.Context, number string) (AdminTicket, error) {
	if !ValidNumber(number) {
		return AdminTicket{}, ErrNotFound
	}

	return s.readAdmin(ctx, s.db, `t.number = $1`, number)
}

// effectiveSQL is the status that a client shows, as an expression over the table tickets. The first argument names today in Asia/Jakarta.
// A ticket in the status dipinjam is late after its due date, which is the pickup date plus the approved days or the requested days.
const (
	dueSQL       = `t.pickup_date + coalesce(t.approved_duration_days, t.duration_days)`
	effectiveSQL = `case when t.status = 'dipinjam' and $3::date > ` + dueSQL + ` then 'terlambat' else t.status end`

	// adminFilterSQL is the part of the queue query that the list and its count share. The arguments are the search text, the effective
	// statuses or null, today, and the four bounds of the dates. The search text matches with position and not with LIKE, so a percent
	// sign or an underscore in it is plain text.
	adminFilterSQL = `
	where ($1::text = '' or position(lower($1::text) in lower(t.number)) > 0
	       or position(lower($1::text) in lower(coalesce(t.requester_name, ''))) > 0
	       or position(lower($1::text) in lower(coalesce(t.requester_nim_nip, ''))) > 0)
	  and ($2::text[] is null or (` + effectiveSQL + `) = any($2::text[]))
	  and ($4::date is null or t.pickup_date >= $4::date)
	  and ($5::date is null or t.pickup_date <= $5::date)
	  and ($6::date is null or ` + dueSQL + ` >= $6::date)
	  and ($7::date is null or ` + dueSQL + ` <= $7::date)`
)

// dateArg turns an optional date into an argument, where nil is the SQL null.
func dateArg(t *time.Time) any {
	if t == nil {
		return nil
	}

	return *t
}

// ListAdmin returns a page of the queue of the tickets of every borrower, newest first, narrowed by the filter. The cursor holds the
// creation time with its full precision and the id, so rows with the same time keep one fixed order. Total counts every ticket that the
// filter lets through.
func (s *Store) ListAdmin(ctx context.Context, f AdminFilter, req catalog.PageRequest) (pagination.Page[AdminSummary], error) {
	q, err := f.check()
	if err != nil {
		return pagination.Page[AdminSummary]{}, err
	}

	keys, err := s.codec.Decode(adminTicketsKind, req.Cursor, 2)
	if err != nil {
		return pagination.Page[AdminSummary]{}, fmt.Errorf("read the cursor %w", err)
	}

	var afterAt, afterID any

	if keys != nil {
		afterAt, afterID = keys[0], keys[1]
	}

	var statuses any
	if len(q.statuses) > 0 {
		statuses = q.statuses
	}

	now := s.now()
	filterArgs := []any{q.text, statuses, today(now), dateArg(q.pickupFrom), dateArg(q.pickupTo), dateArg(q.dueFrom), dateArg(q.dueTo)}

	rows, err := s.db.Query(ctx, `select `+ticketColumns+` from tickets t`+adminFilterSQL+`
		  and ($8::timestamptz is null or (t.created_at, t.id) < ($8::timestamptz, $9::uuid))
		order by t.created_at desc, t.id desc limit $10`, append(slices.Clone(filterArgs), afterAt, afterID, req.Limit+1)...)
	if err != nil {
		return pagination.Page[AdminSummary]{}, fmt.Errorf("list the tickets %w", err)
	}

	defer rows.Close()

	tickets := []Ticket{}
	created := []time.Time{}

	for rows.Next() {
		t, at, err := scanTicket(rows, now)
		if err != nil {
			return pagination.Page[AdminSummary]{}, fmt.Errorf("read a ticket %w", err)
		}

		tickets = append(tickets, t)
		created = append(created, at)
	}

	if err := rows.Err(); err != nil {
		return pagination.Page[AdminSummary]{}, fmt.Errorf("list the tickets %w", err)
	}

	rows.Close()

	var total int

	err = s.db.QueryRow(ctx, `select count(*)::int from tickets t`+adminFilterSQL, filterArgs...).Scan(&total)
	if err != nil {
		return pagination.Page[AdminSummary]{}, fmt.Errorf("count the tickets %w", err)
	}

	tickets, more := pagination.Trim(tickets, req.Limit)

	items := []AdminSummary{}

	if len(tickets) > 0 {
		if items, err = fillAdmin(ctx, s.db, tickets); err != nil {
			return pagination.Page[AdminSummary]{}, err
		}
	}

	page := pagination.Page[AdminSummary]{Items: items, Total: total}

	if more {
		last := len(items) - 1
		page.NextCursor = s.codec.Encode(adminTicketsKind, created[last].UTC().Format(time.RFC3339Nano), items[last].ID)
	}

	return page, nil
}

// ListEvents returns the status changes of every ticket, newest first, or of the ticket with the number when a number is given. A
// number that has the right shape and that no ticket has gives an empty page. Rows with the same time keep one fixed order by the id,
// and the cursor holds the time with its full precision.
func (s *Store) ListEvents(ctx context.Context, number string, req catalog.PageRequest) (pagination.Page[Event], error) {
	if number != "" && !ValidNumber(number) {
		return pagination.Page[Event]{}, &catalog.ValidationError{Details: []catalog.FieldError{
			{Field: "ticketNumber", Code: detailInvalidFormat, Message: "Use a number such as MGM-2611-0001-K7QX."},
		}}
	}

	keys, err := s.codec.Decode(eventsKind, req.Cursor, 2)
	if err != nil {
		return pagination.Page[Event]{}, fmt.Errorf("read the cursor %w", err)
	}

	var afterAt, afterID any

	if keys != nil {
		afterAt, afterID = keys[0], keys[1]
	}

	rows, err := s.db.Query(ctx, `
		select h.id::text, h.at, t.number, t.requester_name, h.status, h.actor_kind,
		       case when h.actor_kind = 'admin' then a.name else t.requester_name end, h.note
		from ticket_history h
		join tickets t on t.id = h.ticket_id
		left join admins a on a.id = h.admin_id
		where ($1::text = '' or t.number = $1::text)
		  and ($2::timestamptz is null or (h.at, h.id) < ($2::timestamptz, $3::uuid))
		order by h.at desc, h.id desc limit $4`, number, afterAt, afterID, req.Limit+1)
	if err != nil {
		return pagination.Page[Event]{}, fmt.Errorf("list the events %w", err)
	}

	defer rows.Close()

	items := []Event{}
	moments := []time.Time{}

	for rows.Next() {
		var (
			e  Event
			at time.Time
		)

		if err := rows.Scan(&e.ID, &at, &e.TicketNumber, &e.RequesterName, &e.Status, &e.ActorKind, &e.ActorName, &e.Note); err != nil {
			return pagination.Page[Event]{}, fmt.Errorf("read an event %w", err)
		}

		e.At = timeText(at)
		items = append(items, e)
		moments = append(moments, at.UTC())
	}

	if err := rows.Err(); err != nil {
		return pagination.Page[Event]{}, fmt.Errorf("list the events %w", err)
	}

	rows.Close()

	var total int

	err = s.db.QueryRow(ctx, `
		select count(*)::int from ticket_history h join tickets t on t.id = h.ticket_id where ($1::text = '' or t.number = $1::text)`,
		number).Scan(&total)
	if err != nil {
		return pagination.Page[Event]{}, fmt.Errorf("count the events %w", err)
	}

	items, more := pagination.Trim(items, req.Limit)
	page := pagination.Page[Event]{Items: items, Total: total}

	if more {
		last := len(items) - 1
		page.NextCursor = s.codec.Encode(eventsKind, moments[last].Format(time.RFC3339Nano), items[last].ID)
	}

	return page, nil
}
