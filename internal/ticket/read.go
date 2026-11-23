package ticket

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
)

const ticketsKind = "tickets"

// ticketColumns lists the columns of a ticket row in the order that scanTicket reads them.
const ticketColumns = `
	t.id::text, t.number, t.status, t.version, t.created_at, t.requester_kind, t.requester_name, t.requester_nim_nip, t.requester_email,
	t.purpose, t.pickup_date, t.duration_days, t.approved_duration_days, t.decision_note, t.anonymized_at`

// scanTicket reads one row of ticketColumns. The items and the history come from fillDetails. It also returns the creation time at the
// full precision of the column, which the cursor of a list needs.
func scanTicket(row pgx.Row, now time.Time) (Ticket, time.Time, error) {
	var (
		t          Ticket
		created    time.Time
		pickup     time.Time
		anonymized *time.Time
	)

	err := row.Scan(&t.ID, &t.Number, &t.Status, &t.Version, &created, &t.Requester.Kind, &t.Requester.Name, &t.Requester.NimNip,
		&t.Requester.Email, &t.Purpose, &pickup, &t.DurationDays, &t.ApprovedDurationDays, &t.DecisionNote, &anonymized)
	if err != nil {
		return Ticket{}, time.Time{}, err //nolint:wrapcheck // the callers wrap it with what they read
	}

	due := dueDate(pickup, t.DurationDays, t.ApprovedDurationDays)

	t.CreatedAt = timeText(created)
	t.PickupDate = pickup.Format(dateLayout)
	t.DueDate = due.Format(dateLayout)
	t.EffectiveStatus = effectiveStatus(t.Status, due, today(now))
	t.AnonymizedAt = timeTextPtr(anonymized)
	t.Items = []Item{}
	t.History = []HistoryEntry{}

	return t, created, nil
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

// fillDetails adds the items and the history to the tickets with two queries, whatever the number of tickets. The items come in the
// order of the model name and the history comes oldest first.
func fillDetails(ctx context.Context, q catalog.Querier, tickets []Ticket) error {
	if len(tickets) == 0 {
		return nil
	}

	ids := make([]string, 0, len(tickets))
	at := make(map[string]int, len(tickets))

	for i, t := range tickets {
		ids = append(ids, t.ID)
		at[t.ID] = i
	}

	itemRows, err := q.Query(ctx, `
		select ti.ticket_id::text, ti.model_id::text, m.name, ti.qty, ti.approved_qty
		from ticket_items ti join models m on m.id = ti.model_id
		where ti.ticket_id = any($1::text[]::uuid[])
		order by ti.ticket_id, lower(m.name), ti.model_id`, ids)
	if err != nil {
		return fmt.Errorf("read the items %w", err)
	}

	defer itemRows.Close()

	for itemRows.Next() {
		var (
			ticketID string
			it       Item
		)

		if err := itemRows.Scan(&ticketID, &it.ModelID, &it.ModelName, &it.Qty, &it.ApprovedQty); err != nil {
			return fmt.Errorf("read an item %w", err)
		}

		tickets[at[ticketID]].Items = append(tickets[at[ticketID]].Items, it)
	}

	if err := itemRows.Err(); err != nil {
		return fmt.Errorf("read the items %w", err)
	}

	itemRows.Close()

	historyRows, err := q.Query(ctx, `
		select ticket_id::text, at, status, actor_kind, note from ticket_history
		where ticket_id = any($1::text[]::uuid[])
		order by ticket_id, at, id`, ids)
	if err != nil {
		return fmt.Errorf("read the history %w", err)
	}

	defer historyRows.Close()

	for historyRows.Next() {
		var (
			ticketID string
			moment   time.Time
			h        HistoryEntry
		)

		if err := historyRows.Scan(&ticketID, &moment, &h.Status, &h.ActorKind, &h.Note); err != nil {
			return fmt.Errorf("read a history entry %w", err)
		}

		h.At = timeText(moment)
		tickets[at[ticketID]].History = append(tickets[at[ticketID]].History, h)
	}

	if err := historyRows.Err(); err != nil {
		return fmt.Errorf("read the history %w", err)
	}

	return nil
}

// readOne reads the ticket that a query selects by one condition. The condition is part of the code and never of a request, and the
// request values are arguments. A missing ticket gives ErrNotFound.
func (s *Store) readOne(ctx context.Context, q catalog.Querier, condition string, args ...any) (Ticket, error) {
	t, _, err := scanTicket(q.QueryRow(ctx, `select `+ticketColumns+` from tickets t where `+condition, args...), s.now())
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}

	if err != nil {
		return Ticket{}, fmt.Errorf("read the ticket %w", err)
	}

	list := []Ticket{t}
	if err := fillDetails(ctx, q, list); err != nil {
		return Ticket{}, err
	}

	return list[0], nil
}

// Get returns the ticket with the number when the session may see it, which means that its id is in the list of the session. Any
// other case is ErrNotFound, so a session cannot tell a ticket of another person from a number that does not exist.
func (s *Store) Get(ctx context.Context, viewable []string, number string) (Ticket, error) {
	if !ValidNumber(number) || len(viewable) == 0 {
		return Ticket{}, ErrNotFound
	}

	return s.readOne(ctx, s.db, `t.number = $1 and t.id = any($2::text[]::uuid[])`, number, viewable)
}

// Lookup returns the ticket that has the number and the email. The email is compared in lower case. A wrong number and a wrong email
// give the same ErrNoMatch, and so does a ticket that the retention job anonymized, because its email is gone.
func (s *Store) Lookup(ctx context.Context, number, email string) (Ticket, error) {
	if !ValidNumber(number) {
		return Ticket{}, ErrNoMatch
	}

	t, err := s.readOne(ctx, s.db, `t.number = $1 and lower(t.requester_email) = $2`, number, strings.ToLower(email))
	if errors.Is(err, ErrNotFound) {
		return Ticket{}, ErrNoMatch
	}

	return t, err
}

// List returns a page of the tickets of the session, newest first. The cursor holds the creation time with its full precision and the
// id, so rows with the same time keep one fixed order. Total counts every ticket of the session.
func (s *Store) List(ctx context.Context, viewable []string, req catalog.PageRequest) (pagination.Page[Ticket], error) {
	keys, err := s.codec.Decode(ticketsKind, req.Cursor, 2)
	if err != nil {
		return pagination.Page[Ticket]{}, fmt.Errorf("read the cursor %w", err)
	}

	var afterAt, afterID any

	if keys != nil {
		afterAt, afterID = keys[0], keys[1]
	}

	if viewable == nil {
		viewable = []string{}
	}

	rows, err := s.db.Query(ctx, `
		select `+ticketColumns+` from tickets t
		where t.id = any($1::text[]::uuid[]) and ($2::timestamptz is null or (t.created_at, t.id) < ($2::timestamptz, $3::uuid))
		order by t.created_at desc, t.id desc limit $4`, viewable, afterAt, afterID, req.Limit+1)
	if err != nil {
		return pagination.Page[Ticket]{}, fmt.Errorf("list the tickets %w", err)
	}

	defer rows.Close()

	now := s.now()
	items := []Ticket{}
	created := []time.Time{}

	for rows.Next() {
		t, at, err := scanTicket(rows, now)
		if err != nil {
			return pagination.Page[Ticket]{}, fmt.Errorf("read a ticket %w", err)
		}

		items = append(items, t)
		created = append(created, at)
	}

	if err := rows.Err(); err != nil {
		return pagination.Page[Ticket]{}, fmt.Errorf("list the tickets %w", err)
	}

	rows.Close()

	var total int

	err = s.db.QueryRow(ctx, `select count(*)::int from tickets where id = any($1::text[]::uuid[])`, viewable).Scan(&total)
	if err != nil {
		return pagination.Page[Ticket]{}, fmt.Errorf("count the tickets %w", err)
	}

	items, more := pagination.Trim(items, req.Limit)
	if err := fillDetails(ctx, s.db, items); err != nil {
		return pagination.Page[Ticket]{}, err
	}

	page := pagination.Page[Ticket]{Items: items, Total: total}

	if more {
		last := len(items) - 1
		page.NextCursor = s.codec.Encode(ticketsKind, created[last].UTC().Format(time.RFC3339Nano), items[last].ID)
	}

	return page, nil
}
