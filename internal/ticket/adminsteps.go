package ticket

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
)

// The steps of an admin share one order. They read the head of the ticket and refuse a wrong status or version at once, so a stale
// request takes no lock. A step that touches stock then locks the models of the ticket in the order of the id, reads the head again,
// because the ticket may have moved while the lock was awaited, and checks the stock. The guarded update of the status comes last
// among the checks, and its history row is written in the same transaction. Two admins on one ticket meet at the lock or at the
// guarded update, so one of them wins and the other gets a conflict.

// ticketModels reads the models of a ticket with the quantities that it asked for and that were approved.
func ticketModels(ctx context.Context, q catalog.Querier, ticketID string) ([]ticketModel, error) {
	rows, err := q.Query(ctx, `
		select ti.model_id::text, m.name, ti.qty, coalesce(ti.approved_qty, 0)
		from ticket_items ti join models m on m.id = ti.model_id
		where ti.ticket_id = $1::uuid order by ti.model_id`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("read the models of the ticket %w", err)
	}

	defer rows.Close()

	var models []ticketModel

	for rows.Next() {
		var m ticketModel

		if err := rows.Scan(&m.id, &m.name, &m.qty, &m.approved); err != nil {
			return nil, fmt.Errorf("read a model of the ticket %w", err)
		}

		models = append(models, m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the models of the ticket %w", err)
	}

	return models, nil
}

func modelIDs(models []ticketModel) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.id)
	}

	return ids
}

// lockAndRecheck locks the models of the ticket and reads the head again, so a change that committed while the lock was awaited is
// seen before the stock is read.
func lockAndRecheck(ctx context.Context, tx pgx.Tx, h head, ids []string, from []string, version int) error {
	if _, err := catalog.LockModels(ctx, tx, ids...); err != nil {
		return fmt.Errorf("lock the models of the ticket %w", err)
	}

	latest, err := readHead(ctx, tx, `id = $1::uuid`, h.ID)
	if err != nil {
		return err
	}

	return refuse(latest, from, version)
}

// shortStock refuses an approval that asks for more than is available. It names each model that is short, with the quantity that is
// left. The ticket is in the status diajukan, so it holds nothing yet and its own quantities are not part of the held quantity.
func shortStock(ctx context.Context, q catalog.Querier, in decision) error {
	ids := make([]string, 0, len(in.items))
	for _, it := range in.items {
		ids = append(ids, it.modelID)
	}

	levels, err := catalog.Stock(ctx, q, ids...)
	if err != nil {
		return fmt.Errorf("read the stock of the approval %w", err)
	}

	var fails []catalog.FieldError

	for _, it := range in.items {
		level := levels[it.modelID]

		if free := max(level.Total-level.Held, 0); it.qty > free {
			fails = append(fails, catalog.FieldError{
				Field: "items[" + strconv.Itoa(it.index) + "].approvedQty", Code: DetailTooMany, Message: "At most " + strconv.Itoa(free) + " are available.",
			})
		}
	}

	if len(fails) == 0 {
		return nil
	}

	return &catalog.RuleError{Kind: catalog.RuleStockChanged, Message: "The stock of an item changed. Reload the ticket and decide again.", Details: fails}
}

// Decide approves or rejects a ticket in the status diajukan. An approval locks the models of the ticket, checks that every approved
// quantity is available, stores the approved quantities and the approved days, and from then on the ticket holds that stock. A
// rejection holds nothing and needs no lock. Both write the note as the decision note and in the history.
func (s *Store) Decide(ctx context.Context, adminID, number string, req DecisionRequest) (AdminTicket, error) {
	in, err := req.check(s.limits)
	if err != nil {
		return AdminTicket{}, err
	}

	to := StatusRejected
	if in.approve {
		to = StatusApproved
	}

	from := Sources(to, ActorAdmin)

	var out AdminTicket

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		h, err := headByNumber(ctx, tx, number)
		if err != nil {
			return err
		}

		if err := refuse(h, from, in.version); err != nil {
			return err
		}

		m := move{ID: h.ID, From: from, To: to, Actor: ActorAdmin, AdminID: &adminID, Version: &in.version, Note: in.note}

		var models []ticketModel

		if in.approve {
			models, err = ticketModels(ctx, tx, h.ID)
			if err != nil {
				return err
			}

			if err := in.matchItems(models); err != nil {
				return err
			}

			if err := lockAndRecheck(ctx, tx, h, modelIDs(models), from, in.version); err != nil {
				return err
			}

			if err := shortStock(ctx, tx, in); err != nil {
				return err
			}

			days := h.Days
			if in.days != nil {
				days = *in.days
			}

			m.ApprovedDays = &days
		}

		changed, err := s.moveStatus(ctx, tx, m)
		if err != nil {
			return err
		}

		if !changed {
			return explain(ctx, tx, h.ID, from, in.version)
		}

		if in.approve {
			if err := writeApproved(ctx, tx, h.ID, in.items); err != nil {
				return err
			}
		}

		out, err = s.readAdmin(ctx, tx, `t.id = $1::uuid`, h.ID)

		return err
	})
	if err != nil {
		return AdminTicket{}, err
	}

	if in.approve {
		s.notify(ctx, NoticeApproved, out)
	} else {
		s.notify(ctx, NoticeRejected, out)
	}

	return out, nil
}

// writeApproved stores the approved quantity of every item of the ticket. The caller has checked that the list names each model once.
func writeApproved(ctx context.Context, tx pgx.Tx, ticketID string, items []approved) error {
	ids := make([]string, 0, len(items))
	quantities := make([]int32, 0, len(items))

	for _, it := range items {
		ids = append(ids, it.modelID)
		quantities = append(quantities, int32(it.qty)) //nolint:gosec // a quantity is at most the quantity of the request, which the limit of the items bounds
	}

	tag, err := tx.Exec(ctx, `
		update ticket_items ti set approved_qty = t.qty
		from unnest($2::text[]::uuid[], $3::int[]) as t(model_id, qty)
		where ti.ticket_id = $1::uuid and ti.model_id = t.model_id`, ticketID, ids, quantities)
	if err != nil {
		return fmt.Errorf("write the approved quantities %w", err)
	}

	if tag.RowsAffected() != int64(len(items)) {
		return errors.New("the approved quantities did not match the items of the ticket")
	}

	return nil
}

// MarkReady moves a ticket in the status disetujui to siap_diambil. It holds the same stock as before and takes no lock on the models.
func (s *Store) MarkReady(ctx context.Context, adminID, number string, req StepRequest) (AdminTicket, error) {
	version, err := req.check()
	if err != nil {
		return AdminTicket{}, err
	}

	from := Sources(StatusReady, ActorAdmin)

	out, err := s.simpleStep(ctx, number, from, version, func(h head) move {
		return move{ID: h.ID, From: from, To: StatusReady, Actor: ActorAdmin, AdminID: &adminID, Version: &version}
	})
	if err != nil {
		return AdminTicket{}, err
	}

	s.notify(ctx, NoticeReady, out)

	return out, nil
}

// CancelByAdmin cancels a ticket in the status disetujui or siap_diambil with a note, which releases its hold. A ticket that is already
// cancelled is returned as it is, whatever version the request carries and without a new note, so a caller that lost the answer can
// send the request again. The status is checked before the version for the same reason.
func (s *Store) CancelByAdmin(ctx context.Context, adminID, number string, req AdminCancelRequest) (AdminTicket, error) {
	in, err := req.check()
	if err != nil {
		return AdminTicket{}, err
	}

	from := Sources(StatusCancelled, ActorAdmin)

	var (
		out     AdminTicket
		changed bool
	)

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		h, err := headByNumber(ctx, tx, number)
		if err != nil {
			return err
		}

		if h.Status == StatusCancelled {
			out, err = s.readAdmin(ctx, tx, `t.id = $1::uuid`, h.ID)

			return err
		}

		if err := refuse(h, from, in.version); err != nil {
			return err
		}

		moved, err := s.moveStatus(ctx, tx, move{ID: h.ID, From: from, To: StatusCancelled, Actor: ActorAdmin, AdminID: &adminID, Version: &in.version, Note: in.note})
		if err != nil {
			return err
		}

		if !moved {
			latest, err := readHead(ctx, tx, `id = $1::uuid`, h.ID)
			if err != nil {
				return err
			}

			// a cancel that committed first leaves the answer of a repeat
			if latest.Status == StatusCancelled {
				out, err = s.readAdmin(ctx, tx, `t.id = $1::uuid`, h.ID)

				return err
			}

			return explain(ctx, tx, h.ID, from, in.version)
		}

		changed = true
		out, err = s.readAdmin(ctx, tx, `t.id = $1::uuid`, h.ID)

		return err
	})
	if err != nil {
		return AdminTicket{}, err
	}

	if changed {
		s.notify(ctx, NoticeCancelled, out)
	}

	return out, nil
}

// simpleStep runs a step that changes only the status and takes no lock on the models.
func (s *Store) simpleStep(ctx context.Context, number string, from []string, version int, build func(head) move) (AdminTicket, error) {
	var out AdminTicket

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		h, err := headByNumber(ctx, tx, number)
		if err != nil {
			return err
		}

		if err := refuse(h, from, version); err != nil {
			return err
		}

		changed, err := s.moveStatus(ctx, tx, build(h))
		if err != nil {
			return err
		}

		if !changed {
			return explain(ctx, tx, h.ID, from, version)
		}

		out, err = s.readAdmin(ctx, tx, `t.id = $1::uuid`, h.ID)

		return err
	})

	return out, err
}

// HandOver moves a ticket in the status siap_diambil to dipinjam and records the units that the admin took from the shelf. It locks the
// models of the ticket, checks that the stock still covers every hold, locks the named units, and checks each one. A tag that no unit
// has is a failed field. A unit that is not active, is of a model that the ticket does not hold, or sits in an open ticket makes the
// request unavailable. Each model then needs exactly its approved quantity of units.
func (s *Store) HandOver(ctx context.Context, adminID, number string, req HandoverRequest) (AdminTicket, error) {
	in, err := req.check()
	if err != nil {
		return AdminTicket{}, err
	}

	from := Sources(StatusBorrowed, ActorAdmin)

	var out AdminTicket

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		h, err := headByNumber(ctx, tx, number)
		if err != nil {
			return err
		}

		if err := refuse(h, from, in.version); err != nil {
			return err
		}

		models, err := ticketModels(ctx, tx, h.ID)
		if err != nil {
			return err
		}

		if err := lockAndRecheck(ctx, tx, h, modelIDs(models), from, in.version); err != nil {
			return err
		}

		if err := stillCovered(ctx, tx, models); err != nil {
			return err
		}

		refs, err := catalog.LockUnits(ctx, tx, in.tags...)
		if err != nil {
			return fmt.Errorf("lock the units of the handover %w", err)
		}

		unitIDs, err := matchUnits(in.tags, refs, models)
		if err != nil {
			return err
		}

		changed, err := s.moveStatus(ctx, tx, move{ID: h.ID, From: from, To: StatusBorrowed, Actor: ActorAdmin, AdminID: &adminID, Version: &in.version})
		if err != nil {
			return err
		}

		if !changed {
			return explain(ctx, tx, h.ID, from, in.version)
		}

		at := s.moment()

		if _, err := tx.Exec(ctx, `
			insert into ticket_units (ticket_id, unit_id, handed_over_at)
			select $1::uuid, t.unit_id, $3::timestamptz from unnest($2::text[]::uuid[]) as t(unit_id)`, h.ID, unitIDs, at); err != nil {
			if code, constraint := violationOf(err); code == uniqueViolation && constraint == openUnitKey {
				return &catalog.RuleError{Kind: catalog.RuleUnitUnavailable, Message: "A unit is already in an open ticket."}
			}

			return fmt.Errorf("write the units of the ticket %w", err)
		}

		if err := catalog.RecordHandover(ctx, tx, at, adminID, h.ID, unitIDs); err != nil {
			return err
		}

		out, err = s.readAdmin(ctx, tx, `t.id = $1::uuid`, h.ID)

		return err
	})
	if err != nil {
		return AdminTicket{}, err
	}

	return out, nil
}

// stillCovered refuses a handover when the stock of a model of the ticket fell below the quantity that the tickets hold. The hold of
// this ticket is part of the held quantity, so a shortfall means that a unit left the stock after the approval.
func stillCovered(ctx context.Context, q catalog.Querier, models []ticketModel) error {
	levels, err := catalog.Stock(ctx, q, modelIDs(models)...)
	if err != nil {
		return fmt.Errorf("read the stock of the handover %w", err)
	}

	for _, m := range models {
		if m.approved == 0 {
			continue
		}

		if level := levels[m.id]; level.Total < level.Held {
			return &catalog.RuleError{Kind: catalog.RuleStockChanged, Message: "The stock of " + m.name + " is below the approved quantity. Fix the stock or cancel the ticket."}
		}
	}

	return nil
}

// matchUnits checks the units of a handover against the models of the ticket and returns the ids of the units in the order of the
// tags. A tag that no unit has is a failed field. A unit that cannot be handed over makes the request unavailable. A model with the
// wrong number of units is a failed field.
func matchUnits(tags []string, refs []catalog.UnitRef, models []ticketModel) ([]string, error) {
	byTag := make(map[string]catalog.UnitRef, len(refs))
	for _, r := range refs {
		byTag[r.Tag] = r
	}

	var c checker

	for i, tag := range tags {
		if _, ok := byTag[tag]; !ok {
			c.fail("tags["+strconv.Itoa(i)+"]", detailUnknownReference, "No unit has this tag.")
		}
	}

	if err := c.err(); err != nil {
		return nil, err
	}

	approvedOf := make(map[string]ticketModel, len(models))
	for _, m := range models {
		approvedOf[m.id] = m
	}

	counts := map[string]int{}
	ids := make([]string, 0, len(tags))

	for _, tag := range tags {
		ref := byTag[tag]

		if m := approvedOf[ref.ModelID]; m.approved == 0 || ref.Lifecycle != catalog.LifecycleActive || ref.Held {
			return nil, &catalog.RuleError{Kind: catalog.RuleUnitUnavailable, Message: "The unit " + tag + " cannot be handed over for this ticket."}
		}

		counts[ref.ModelID]++

		ids = append(ids, ref.ID)
	}

	for _, m := range models {
		switch {
		case m.approved == 0 || counts[m.id] == m.approved:
		case counts[m.id] < m.approved:
			c.fail("tags", detailRequired, "Hand over exactly "+strconv.Itoa(m.approved)+" units of "+m.name+".")
		default:
			c.fail("tags", DetailTooMany, "Hand over exactly "+strconv.Itoa(m.approved)+" units of "+m.name+".")
		}
	}

	return ids, c.err()
}

// heldUnit is an open row of a ticket with the unit that it holds.
type heldUnit struct {
	id      string
	tag     string
	modelID string
}

// Return takes the units of a ticket in the status dipinjam back, also when the ticket is late. The body names every unit that the
// ticket holds with an outcome. The return is all or nothing in one transaction. It moves the ticket to dikembalikan first, which ends
// its hold, then closes the open rows, and then applies the outcome of each unit, so a unit that comes back lost or in maintenance
// lowers the stock only after the ticket no longer holds.
func (s *Store) Return(ctx context.Context, adminID, number string, req ReturnRequest) (AdminTicket, error) {
	in, err := req.check()
	if err != nil {
		return AdminTicket{}, err
	}

	from := Sources(StatusReturned, ActorAdmin)

	var out AdminTicket

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		h, err := headByNumber(ctx, tx, number)
		if err != nil {
			return err
		}

		if err := refuse(h, from, in.version); err != nil {
			return err
		}

		models, err := ticketModels(ctx, tx, h.ID)
		if err != nil {
			return err
		}

		if err := lockAndRecheck(ctx, tx, h, modelIDs(models), from, in.version); err != nil {
			return err
		}

		held, err := openUnits(ctx, tx, h.ID)
		if err != nil {
			return err
		}

		lines, err := in.matchHeld(held)
		if err != nil {
			return err
		}

		changed, err := s.moveStatus(ctx, tx, move{
			ID: h.ID, From: from, To: StatusReturned, Actor: ActorAdmin, AdminID: &adminID, Version: &in.version, HistoryNote: in.historyNote(),
		})
		if err != nil {
			return err
		}

		if !changed {
			return explain(ctx, tx, h.ID, from, in.version)
		}

		at := s.moment()

		if err := closeRows(ctx, tx, h.ID, at, lines); err != nil {
			return err
		}

		if err := catalog.RecordReturn(ctx, tx, at, adminID, h.ID, lines); err != nil {
			return err
		}

		out, err = s.readAdmin(ctx, tx, `t.id = $1::uuid`, h.ID)

		return err
	})
	if err != nil {
		return AdminTicket{}, err
	}

	return out, nil
}

// openUnits reads the units that the open rows of the ticket hold.
func openUnits(ctx context.Context, q catalog.Querier, ticketID string) ([]heldUnit, error) {
	rows, err := q.Query(ctx, `
		select u.id::text, u.tag, u.model_id::text
		from ticket_units tu join units u on u.id = tu.unit_id
		where tu.ticket_id = $1::uuid and tu.returned_at is null order by u.tag`, ticketID)
	if err != nil {
		return nil, fmt.Errorf("read the units of the ticket %w", err)
	}

	defer rows.Close()

	var held []heldUnit

	for rows.Next() {
		var u heldUnit

		if err := rows.Scan(&u.id, &u.tag, &u.modelID); err != nil {
			return nil, fmt.Errorf("read a unit of the ticket %w", err)
		}

		held = append(held, u)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the units of the ticket %w", err)
	}

	return held, nil
}

// matchHeld checks the lines of a return against the units that the ticket holds. A tag that the ticket does not hold is a failed
// field, and so is a held unit that no line names, because a return that leaves a unit out would leave it in an open row of a closed
// ticket. It returns the lines as the units of the catalog package.
func (r returned) matchHeld(held []heldUnit) ([]catalog.ReturnedUnit, error) {
	byTag := make(map[string]heldUnit, len(held))
	for _, u := range held {
		byTag[u.tag] = u
	}

	var c checker

	named := map[string]bool{}
	out := make([]catalog.ReturnedUnit, 0, len(r.units))

	for _, line := range r.units {
		u, ok := byTag[line.tag]
		if !ok {
			c.fail("units["+strconv.Itoa(line.index)+"].tag", detailUnknownReference, "The ticket does not hold this unit.")

			continue
		}

		named[line.tag] = true
		out = append(out, catalog.ReturnedUnit{UnitID: u.id, Outcome: line.outcome, Note: line.note})
	}

	var left []string

	for _, u := range held {
		if !named[u.tag] {
			left = append(left, u.tag)
		}
	}

	if len(left) > 0 {
		c.fail("units", detailRequired, "Name every unit that the ticket holds. Missing "+strings.Join(left, ", ")+".")
	}

	return out, c.err()
}

// historyNote joins the notes of the lines into the note of the history entry, one line for each note and in the order of the request.
// A return with no note gives nil.
func (r returned) historyNote() *string {
	var lines []string

	for _, line := range r.units {
		if line.note != nil {
			lines = append(lines, line.tag+" ("+line.outcome+") - "+*line.note)
		}
	}

	if len(lines) == 0 {
		return nil
	}

	joined := strings.Join(lines, "\n")

	return &joined
}

// closeRows ends the open rows of the units that came back, with the outcome and the note of each. Every line must close one row.
func closeRows(ctx context.Context, tx pgx.Tx, ticketID string, at time.Time, lines []catalog.ReturnedUnit) error {
	ids := make([]string, 0, len(lines))
	outcomes := make([]string, 0, len(lines))
	notes := make([]*string, 0, len(lines))

	for _, l := range lines {
		ids = append(ids, l.UnitID)
		outcomes = append(outcomes, l.Outcome)
		notes = append(notes, l.Note)
	}

	tag, err := tx.Exec(ctx, `
		update ticket_units tu set returned_at = $2::timestamptz, return_outcome = t.outcome, return_note = t.note
		from unnest($3::text[]::uuid[], $4::text[], $5::text[]) as t(unit_id, outcome, note)
		where tu.ticket_id = $1::uuid and tu.unit_id = t.unit_id and tu.returned_at is null`, ticketID, at, ids, outcomes, notes)
	if err != nil {
		return fmt.Errorf("close the units of the ticket %w", err)
	}

	if tag.RowsAffected() != int64(len(lines)) {
		return errors.New("the return did not close the open rows of the ticket")
	}

	return nil
}
