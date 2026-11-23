package ticket_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ticket"
)

func ap(modelID string, qty int) ticket.ApprovedInput {
	return ticket.ApprovedInput{ModelID: modelID, ApprovedQty: ptr(qty)}
}

func tagsOf(units ...catalog.Unit) []string {
	out := make([]string, 0, len(units))
	for _, u := range units {
		out = append(out, u.Tag)
	}

	return out
}

// ruleKindOf returns the kind of the rule that an error broke, or 0 when the error is not a rule.
func ruleKindOf(err error) catalog.RuleKind {
	var rule *catalog.RuleError
	if errors.As(err, &rule) {
		return rule.Kind
	}

	return 0
}

// requireRule stops the test unless the error is the rule of the kind.
func requireRule(t *testing.T, err error, kind catalog.RuleKind) {
	t.Helper()

	if got := ruleKindOf(err); got != kind {
		t.Fatalf("the error is %v, which is the rule %d and not %d", err, got, kind)
	}
}

// addAdmin adds another admin and returns the id.
func (e *env) addAdmin(name string) string {
	e.t.Helper()

	var id string

	err := e.pool.QueryRow(context.Background(), `
		insert into admins (email, name, password_hash) values (lower($1) || '@example.org', $1, 'x') returning id::text`, name).Scan(&id)
	if err != nil {
		e.t.Fatalf("insert the admin %v", err)
	}

	return id
}

func (e *env) unitID(tag string) string {
	e.t.Helper()

	var id string
	if err := e.pool.QueryRow(context.Background(), `select id::text from units where tag = $1`, tag).Scan(&id); err != nil {
		e.t.Fatalf("read the unit %s %v", tag, err)
	}

	return id
}

func (e *env) approvalOf(version int, items ...ticket.ApprovedInput) ticket.DecisionRequest {
	return ticket.DecisionRequest{Version: ptr(version), Decision: ticket.DecisionApprove, Items: items}
}

// approve approves a ticket that was just created with the quantities, and fails the test on an error.
func (e *env) approve(made ticket.Ticket, items ...ticket.ApprovedInput) ticket.AdminTicket {
	e.t.Helper()

	got, err := e.tickets.Decide(context.Background(), e.adminID, made.Number, e.approvalOf(made.Version, items...))
	if err != nil {
		e.t.Fatalf("approve %s %v", made.Number, err)
	}

	return got
}

func (e *env) markReady(approved ticket.AdminTicket) ticket.AdminTicket {
	e.t.Helper()

	got, err := e.tickets.MarkReady(context.Background(), e.adminID, approved.Number, ticket.StepRequest{Version: ptr(approved.Version)})
	if err != nil {
		e.t.Fatalf("mark %s ready %v", approved.Number, err)
	}

	return got
}

// readyTicket creates a ticket for the model, approves every unit that it asks for and marks it ready.
func (e *env) readyTicket(b *borrower, m catalog.Model, qty int) ticket.AdminTicket {
	e.t.Helper()

	made := e.mustCreate(b, item(m.ID, qty))

	return e.markReady(e.approve(made, ap(m.ID, qty)))
}

func (e *env) handOver(ready ticket.AdminTicket, tags ...string) (ticket.AdminTicket, error) {
	return e.tickets.HandOver(context.Background(), e.adminID, ready.Number, ticket.HandoverRequest{Version: ptr(ready.Version), Tags: tags})
}

// loan creates a ticket for the model and hands the first qty units over.
func (e *env) loan(b *borrower, m catalog.Model, units []catalog.Unit, qty int) ticket.AdminTicket {
	e.t.Helper()

	got, err := e.handOver(e.readyTicket(b, m, qty), tagsOf(units[:qty]...)...)
	if err != nil {
		e.t.Fatalf("hand the units over %v", err)
	}

	return got
}

func returnAll(loan ticket.AdminTicket, outcome string) ticket.ReturnRequest {
	lines := make([]ticket.ReturnLine, 0, len(loan.Units))
	for _, u := range loan.Units {
		lines = append(lines, ticket.ReturnLine{Tag: u.Tag, Outcome: outcome})
	}

	return ticket.ReturnRequest{Version: ptr(loan.Version), Units: lines}
}

func (e *env) unitEvents(tag string) []string {
	e.t.Helper()

	page, err := e.catalog.ListUnitEvents(context.Background(), tag, catalog.PageRequest{Limit: 100})
	if err != nil {
		e.t.Fatalf("list the events of %s %v", tag, err)
	}

	out := make([]string, 0, len(page.Items))
	for _, ev := range page.Items {
		out = append(out, ev.Action)
	}

	return out
}

func TestAnApprovalHoldsTheApprovedQuantityAndTheApprovedDays(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 3)
	made := e.mustCreate(e.borrower(), item(m.ID, 2))

	if held, _ := e.available(m.ID); held != 0 {
		t.Fatalf("a submitted ticket holds %d", held)
	}

	got, err := e.tickets.Decide(ctx, e.adminID, made.Number, ticket.DecisionRequest{
		Version: ptr(1), Decision: ticket.DecisionApprove, Items: []ticket.ApprovedInput{ap(m.ID, 2)}, DurationDays: ptr(5), Note: ptr("Take them on Monday."),
	})
	if err != nil {
		t.Fatalf("approve %v", err)
	}

	if got.Status != "disetujui" || got.EffectiveStatus != "disetujui" || got.Version != 2 || got.ApprovedDurationDays == nil || *got.ApprovedDurationDays != 5 ||
		got.DueDate != "2026-11-25" || got.DurationDays != 7 {
		t.Fatalf("the ticket is %+v", got.Ticket)
	}

	if len(got.Items) != 1 || got.Items[0].ApprovedQty == nil || *got.Items[0].ApprovedQty != 2 || got.Items[0].Qty != 2 {
		t.Fatalf("the items are %+v", got.Items)
	}

	if got.DecisionNote == nil || *got.DecisionNote != "Take them on Monday." || got.Units == nil || len(got.Units) != 0 {
		t.Fatalf("the note is %v and the units are %v", got.DecisionNote, got.Units)
	}

	if len(got.History) != 2 || got.History[0].ActorName != nil || got.History[1].Status != "disetujui" || got.History[1].ActorKind != "admin" ||
		got.History[1].ActorName == nil || *got.History[1].ActorName != "Example Admin" || got.History[1].Note == nil || *got.History[1].Note != "Take them on Monday." {
		t.Fatalf("the history is %+v", got.History)
	}

	if held, available := e.available(m.ID); held != 2 || available != 1 {
		t.Fatalf("held %d, available %d", held, available)
	}

	if e.count(`select count(*)::int from ticket_history where ticket_id = $1::uuid and admin_id = $2::uuid`, made.ID, e.adminID) != 1 {
		t.Fatal("the history row does not name the admin")
	}

	// the borrower reads the same ticket without the name of the admin
	for _, h := range mustGet(t, e, made.Number).History {
		if h.ActorKind == "admin" && h.Status != "disetujui" {
			t.Fatalf("the history of the borrower is %+v", h)
		}
	}
}

// mustGet reads a ticket as an admin.
func mustGet(t *testing.T, e *env, number string) ticket.AdminTicket {
	t.Helper()

	got, err := e.tickets.GetAdmin(context.Background(), number)
	if err != nil {
		t.Fatalf("read %s %v", number, err)
	}

	return got
}

func TestAPartialApprovalAndAZeroForOneModel(t *testing.T) {
	e := newEnv(t)
	a, _ := e.model("Example A headset", "EXA", 3)
	b, _ := e.model("Example B cable", "EXB", 3)
	made := e.mustCreate(e.borrower(), item(a.ID, 2), item(b.ID, 1))

	got := e.approve(made, ap(a.ID, 1), ap(b.ID, 0))

	if len(got.Items) != 2 || *got.Items[0].ApprovedQty != 1 || *got.Items[1].ApprovedQty != 0 || got.ApprovedDurationDays == nil || *got.ApprovedDurationDays != 7 {
		t.Fatalf("the items are %+v and the days %v", got.Items, got.ApprovedDurationDays)
	}

	if held, _ := e.available(a.ID); held != 1 {
		t.Fatalf("A holds %d", held)
	}

	if held, available := e.available(b.ID); held != 0 || available != 3 {
		t.Fatalf("B holds %d and has %d", held, available)
	}
}

func TestARejectionHoldsNothingAndKeepsTheQuantitiesEmpty(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 3)
	made := e.mustCreate(e.borrower(), item(m.ID, 2))

	got, err := e.tickets.Decide(ctx, e.adminID, made.Number, ticket.DecisionRequest{Version: ptr(1), Decision: ticket.DecisionReject, Note: ptr("Not free.")})
	if err != nil {
		t.Fatalf("reject %v", err)
	}

	if got.Status != "ditolak" || got.Version != 2 || got.DecisionNote == nil || *got.DecisionNote != "Not free." || got.Items[0].ApprovedQty != nil ||
		got.ApprovedDurationDays != nil || got.History[1].Note == nil || *got.History[1].Note != "Not free." {
		t.Fatalf("the ticket is %+v", got)
	}

	if held, _ := e.available(m.ID); held != 0 {
		t.Fatalf("a rejected ticket holds %d", held)
	}

	_, err = e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(2, ap(m.ID, 1)))
	requireRule(t, err, catalog.RuleStatusConflict)
}

func TestTwoTicketsForTheLastUnitHaveOneApproval(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 1)

	// both tickets pass the creation, because a submitted ticket holds nothing
	first := e.mustCreate(e.borrower(), item(m.ID, 1))
	second := e.mustCreate(e.borrower(), item(m.ID, 1))

	errs := make([]error, 2)

	together(
		func() { _, errs[0] = e.tickets.Decide(ctx, e.adminID, first.Number, e.approvalOf(1, ap(m.ID, 1))) },
		func() { _, errs[1] = e.tickets.Decide(ctx, e.adminID, second.Number, e.approvalOf(1, ap(m.ID, 1))) },
	)

	won := 0

	for _, err := range errs {
		if err == nil {
			won++

			continue
		}

		requireRule(t, err, catalog.RuleStockChanged)

		var rule *catalog.RuleError

		_ = errors.As(err, &rule)
		if len(rule.Details) != 1 || rule.Details[0].Field != "items[0].approvedQty" || rule.Details[0].Code != "too_many" ||
			rule.Details[0].Message != "At most 0 are available." {
			t.Fatalf("the details are %+v", rule.Details)
		}
	}

	if won != 1 {
		t.Fatalf("%d approvals won, the errors are %v", won, errs)
	}

	if held, available := e.available(m.ID); held != 1 || available != 0 {
		t.Fatalf("held %d, available %d", held, available)
	}

	// the ticket that lost is still submitted and can be rejected
	if e.count(`select count(*)::int from tickets where status = 'diajukan'`) != 1 || e.count(`select count(*)::int from ticket_history where status = 'disetujui'`) != 1 {
		t.Fatal("the loser changed")
	}
}

func TestAnApprovalReadsTheStockAfterTheLockAndNotBefore(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, units := e.model("Example headset", "EXM", 2)
	made := e.mustCreate(e.borrower(), item(m.ID, 2))

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %v", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// a lifecycle change that holds the lock of the model and takes a unit out of the stock
	if _, err := catalog.LockModels(ctx, tx, m.ID); err != nil {
		t.Fatalf("lock %v", err)
	}

	if _, err := tx.Exec(ctx, `update units set lifecycle = 'maintenance' where tag = $1`, units[0].Tag); err != nil {
		t.Fatalf("change the unit %v", err)
	}

	done := make(chan error, 1)

	go func() {
		_, err := e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(1, ap(m.ID, 2)))
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("the approval did not wait for the lock and gave %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %v", err)
	}

	select {
	case err := <-done:
		requireRule(t, err, catalog.RuleStockChanged)
	case <-time.After(10 * time.Second):
		t.Fatal("the approval never ended")
	}

	// one unit is left, so an approval of one passes
	if _, err := e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(1, ap(m.ID, 1))); err != nil {
		t.Fatalf("approve one %v", err)
	}
}

func TestAnApprovalOfMoreThanTheTicketAskedForOrOfAForeignModelIsRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, _ := e.model("Example A headset", "EXA", 5)
	b, _ := e.model("Example B cable", "EXB", 5)
	made := e.mustCreate(e.borrower(), item(a.ID, 2))

	for name, tc := range map[string]struct {
		items []ticket.ApprovedInput
		want  []string
	}{
		"more than asked": {[]ticket.ApprovedInput{ap(a.ID, 3)}, []string{"items[0].approvedQty out_of_range"}},
		"foreign model":   {[]ticket.ApprovedInput{ap(a.ID, 1), ap(b.ID, 1)}, []string{"items[1].modelId unknown_reference"}},
		"model left out":  {[]ticket.ApprovedInput{ap(b.ID, 1)}, []string{"items[0].modelId unknown_reference", "items required"}},
		"all zero":        {[]ticket.ApprovedInput{ap(a.ID, 0)}, []string{"items out_of_range"}},
	} {
		_, err := e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(1, tc.items...))
		if got := fieldList(err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}

	if e.count(`select count(*)::int from tickets where id = $1::uuid and status = 'diajukan' and version = 1`, made.ID) != 1 {
		t.Fatal("a refused approval changed the ticket")
	}
}

func TestTwoAdminsOnOneTicketHaveOneWinnerAndOneConflict(t *testing.T) {
	ctx := context.Background()

	t.Run("two approvals", func(t *testing.T) {
		e := newEnv(t)
		other := e.addAdmin("Other Admin")
		m, _ := e.model("Example headset", "EXM", 3)
		made := e.mustCreate(e.borrower(), item(m.ID, 2))
		errs := make([]error, 2)

		together(
			func() { _, errs[0] = e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(1, ap(m.ID, 2))) },
			func() { _, errs[1] = e.tickets.Decide(ctx, other, made.Number, e.approvalOf(1, ap(m.ID, 2))) },
		)

		requireOneWinner(t, errs, catalog.RuleStatusConflict)

		if e.count(`select count(*)::int from ticket_history where ticket_id = $1::uuid and status = 'disetujui'`, made.ID) != 1 ||
			e.count(`select version from tickets where id = $1::uuid`, made.ID) != 2 {
			t.Fatal("both approvals wrote")
		}

		if held, _ := e.available(m.ID); held != 2 {
			t.Fatalf("the hold is %d", held)
		}
	})

	t.Run("an approval and a rejection", func(t *testing.T) {
		e := newEnv(t)
		other := e.addAdmin("Other Admin")
		m, _ := e.model("Example headset", "EXM", 3)
		made := e.mustCreate(e.borrower(), item(m.ID, 2))
		errs := make([]error, 2)

		together(
			func() { _, errs[0] = e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(1, ap(m.ID, 2))) },
			func() {
				_, errs[1] = e.tickets.Decide(ctx, other, made.Number, ticket.DecisionRequest{Version: ptr(1), Decision: ticket.DecisionReject, Note: ptr("No.")})
			},
		)

		requireOneWinner(t, errs, catalog.RuleStatusConflict)

		held, _ := e.available(m.ID)
		if (errs[0] == nil) != (held == 2) {
			t.Fatalf("the hold is %d and the approval gave %v", held, errs[0])
		}
	})

	t.Run("two handovers", func(t *testing.T) {
		e := newEnv(t)
		other := e.addAdmin("Other Admin")
		m, units := e.model("Example headset", "EXM", 3)
		ready := e.readyTicket(e.borrower(), m, 2)
		tags := tagsOf(units[:2]...)
		errs := make([]error, 2)

		together(
			func() { _, errs[0] = e.handOver(ready, tags...) },
			func() {
				_, errs[1] = e.tickets.HandOver(ctx, other, ready.Number, ticket.HandoverRequest{Version: ptr(ready.Version), Tags: tags})
			},
		)

		requireOneWinner(t, errs, catalog.RuleStatusConflict)

		if e.count(`select count(*)::int from ticket_units where ticket_id = $1::uuid`, ready.ID) != 2 ||
			e.count(`select count(*)::int from unit_events where action = 'handed_over'`) != 2 {
			t.Fatal("both handovers wrote")
		}
	})

	t.Run("two returns", func(t *testing.T) {
		e := newEnv(t)
		other := e.addAdmin("Other Admin")
		m, units := e.model("Example headset", "EXM", 3)
		loan := e.loan(e.borrower(), m, units, 2)
		req := returnAll(loan, "ok")
		errs := make([]error, 2)

		together(
			func() { _, errs[0] = e.tickets.Return(ctx, e.adminID, loan.Number, req) },
			func() { _, errs[1] = e.tickets.Return(ctx, other, loan.Number, req) },
		)

		requireOneWinner(t, errs, catalog.RuleStatusConflict)

		if e.count(`select count(*)::int from unit_events where action = 'returned'`) != 2 {
			t.Fatal("both returns wrote events")
		}
	})

	t.Run("two cancels leave one entry and no error", func(t *testing.T) {
		e := newEnv(t)
		other := e.addAdmin("Other Admin")
		m, _ := e.model("Example headset", "EXM", 3)
		ready := e.readyTicket(e.borrower(), m, 2)
		errs := make([]error, 2)

		together(
			func() {
				_, errs[0] = e.tickets.CancelByAdmin(ctx, e.adminID, ready.Number, ticket.AdminCancelRequest{Version: ptr(ready.Version), Note: ptr("First note.")})
			},
			func() {
				_, errs[1] = e.tickets.CancelByAdmin(ctx, other, ready.Number, ticket.AdminCancelRequest{Version: ptr(ready.Version), Note: ptr("Second note.")})
			},
		)

		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("the cancels gave %v and %v", errs[0], errs[1])
		}

		if e.count(`select count(*)::int from ticket_history where ticket_id = $1::uuid and status = 'dibatalkan'`, ready.ID) != 1 ||
			e.count(`select version from tickets where id = $1::uuid`, ready.ID) != ready.Version+1 {
			t.Fatal("the cancels wrote more than one change")
		}

		got := mustGet(t, e, ready.Number)
		if got.DecisionNote == nil || got.History[len(got.History)-1].Note == nil || *got.DecisionNote != *got.History[len(got.History)-1].Note {
			t.Fatalf("the note of the ticket is %v and the note of the history is %+v", got.DecisionNote, got.History[len(got.History)-1])
		}
	})
}

// requireOneWinner stops the test unless exactly one of the errors is nil and the other is the rule of the kind.
func requireOneWinner(t *testing.T, errs []error, loser catalog.RuleKind) {
	t.Helper()

	won := 0

	for _, err := range errs {
		if err == nil {
			won++

			continue
		}

		requireRule(t, err, loser)
	}

	if won != 1 {
		t.Fatalf("%d steps won, the errors are %v", won, errs)
	}
}

func TestAStepChecksTheStatusBeforeTheVersionAndTheNumberBeforeAll(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 3)
	made := e.mustCreate(e.borrower(), item(m.ID, 2))

	_, err := e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(5, ap(m.ID, 2)))
	requireRule(t, err, catalog.RuleVersionConflict)

	approved := e.approve(made, ap(m.ID, 2))

	// the retry of a lost answer carries the old version and finds the ticket decided
	_, err = e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(1, ap(m.ID, 2)))
	requireRule(t, err, catalog.RuleStatusConflict)

	_, err = e.tickets.MarkReady(ctx, e.adminID, made.Number, ticket.StepRequest{Version: ptr(1)})
	requireRule(t, err, catalog.RuleVersionConflict)

	ready := e.markReady(approved)

	_, err = e.tickets.CancelByAdmin(ctx, e.adminID, made.Number, ticket.AdminCancelRequest{Version: ptr(1), Note: ptr("Nobody came.")})
	requireRule(t, err, catalog.RuleVersionConflict)

	// a ticket that is ready cannot be rejected or approved again
	_, err = e.tickets.Decide(ctx, e.adminID, made.Number, ticket.DecisionRequest{Version: ptr(ready.Version), Decision: ticket.DecisionReject, Note: ptr("No.")})
	requireRule(t, err, catalog.RuleStatusConflict)

	for _, number := range []string{"MGM-2611-0001-ZZZZ", "not a number", ""} {
		if _, err := e.tickets.Decide(ctx, e.adminID, number, e.approvalOf(1, ap(m.ID, 1))); !errors.Is(err, ticket.ErrNotFound) {
			t.Errorf("Decide on %q gave %v", number, err)
		}

		if _, err := e.tickets.GetAdmin(ctx, number); !errors.Is(err, ticket.ErrNotFound) {
			t.Errorf("GetAdmin on %q gave %v", number, err)
		}
	}

	if _, err := e.tickets.HandOver(ctx, e.adminID, "MGM-2611-0001-ZZZZ", ticket.HandoverRequest{Version: ptr(1), Tags: []string{"MGM-EXM-001"}}); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("HandOver on an unknown number gave %v", err)
	}
}

func TestTheReadyNoticeKeepsTheHoldAndTheAdminCancelGivesItBack(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 3)
	ready := e.readyTicket(e.borrower(), m, 2)

	if ready.Status != "siap_diambil" || ready.Version != 3 || len(ready.History) != 3 || ready.History[2].ActorName == nil {
		t.Fatalf("the ticket is %+v", ready)
	}

	if held, _ := e.available(m.ID); held != 2 {
		t.Fatalf("the hold is %d", held)
	}

	cancelled, err := e.tickets.CancelByAdmin(ctx, e.adminID, ready.Number, ticket.AdminCancelRequest{Version: ptr(3), Note: ptr("  Nobody collected the items.  ")})
	if err != nil {
		t.Fatalf("cancel %v", err)
	}

	if cancelled.Status != "dibatalkan" || cancelled.Version != 4 || cancelled.DecisionNote == nil || *cancelled.DecisionNote != "Nobody collected the items." ||
		cancelled.History[3].Note == nil || *cancelled.History[3].Note != "Nobody collected the items." || cancelled.History[3].ActorKind != "admin" {
		t.Fatalf("the cancelled ticket is %+v", cancelled)
	}

	if held, available := e.available(m.ID); held != 0 || available != 3 {
		t.Fatalf("held %d, available %d", held, available)
	}

	// the retry of a lost answer carries the old version and another note, and changes nothing
	again, err := e.tickets.CancelByAdmin(ctx, e.adminID, ready.Number, ticket.AdminCancelRequest{Version: ptr(3), Note: ptr("Another note.")})
	if err != nil || again.Version != 4 || *again.DecisionNote != "Nobody collected the items." || len(again.History) != 4 {
		t.Fatalf("the repeat gives %+v, %v", again, err)
	}

	// a ticket that nobody decided on cannot be cancelled by an admin
	asking := e.borrower()
	made := e.mustCreate(asking, item(m.ID, 1))

	_, err = e.tickets.CancelByAdmin(ctx, e.adminID, made.Number, ticket.AdminCancelRequest{Version: ptr(1), Note: ptr("No.")})
	requireRule(t, err, catalog.RuleStatusConflict)

	// a ticket that a guest cancelled is already cancelled, so the admin cancel answers it as it is
	if _, err := e.tickets.CancelByGuest(ctx, e.viewable(asking), made.Number); err != nil {
		t.Fatalf("the guest cancel %v", err)
	}

	same, err := e.tickets.CancelByAdmin(ctx, e.adminID, made.Number, ticket.AdminCancelRequest{Version: ptr(1), Note: ptr("No.")})
	if err != nil || same.Status != "dibatalkan" || same.Version != 2 || same.DecisionNote != nil || len(same.History) != 2 {
		t.Fatalf("the admin cancel of a cancelled ticket gives %+v, %v", same, err)
	}
}

func TestAnAdminCancelOfALoanOrAClosedTicketIsRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, units := e.model("Example headset", "EXM", 3)
	loan := e.loan(e.borrower(), m, units, 2)

	_, err := e.tickets.CancelByAdmin(ctx, e.adminID, loan.Number, ticket.AdminCancelRequest{Version: ptr(loan.Version), Note: ptr("No.")})
	requireRule(t, err, catalog.RuleStatusConflict)

	if _, err := e.tickets.Return(ctx, e.adminID, loan.Number, returnAll(loan, "ok")); err != nil {
		t.Fatalf("return %v", err)
	}

	_, err = e.tickets.CancelByAdmin(ctx, e.adminID, loan.Number, ticket.AdminCancelRequest{Version: ptr(loan.Version + 1), Note: ptr("No.")})
	requireRule(t, err, catalog.RuleStatusConflict)
}

func TestAHandoverWritesTheUnitsTheHolderAndTheEvents(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, units := e.model("Example headset", "EXM", 5)
	b, other := e.model("Example cable", "EXB", 1)

	first := e.readyTicket(e.borrower(), a, 2)
	second := e.readyTicket(e.borrower(), a, 2)

	// the fifth unit is a spare, so it can leave the stock while the two tickets hold four
	if _, err := e.catalog.ChangeLifecycle(ctx, units[4].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: catalog.LifecycleMaintenance}); err != nil {
		t.Fatalf("change the lifecycle %v", err)
	}

	for name, tc := range map[string]struct {
		tags []string
		want []string
		rule catalog.RuleKind
	}{
		"too few":          {tagsOf(units[0]), []string{"tags required"}, 0},
		"too many":         {tagsOf(units[:3]...), []string{"tags too_many"}, 0},
		"no such unit":     {[]string{units[0].Tag, "MGM-EXM-099"}, []string{"tags[1] unknown_reference"}, 0},
		"another model":    {[]string{units[0].Tag, other[0].Tag}, nil, catalog.RuleUnitUnavailable},
		"not active":       {[]string{units[0].Tag, units[4].Tag}, nil, catalog.RuleUnitUnavailable},
		"model not on it":  {tagsOf(other[0]), nil, catalog.RuleUnitUnavailable},
		"bad tag in input": {[]string{units[0].Tag, "x"}, []string{"tags[1] invalid_format"}, 0},
	} {
		_, err := e.handOver(first, tc.tags...)
		if tc.rule != 0 {
			if ruleKindOf(err) != tc.rule {
				t.Errorf("%s gives %v", name, err)
			}

			continue
		}

		if got := fieldList(err); !slices.Equal(got, tc.want) {
			t.Errorf("%s fails with %v, want %v", name, got, tc.want)
		}
	}

	if e.count(`select count(*)::int from ticket_units`) != 0 || e.count(`select count(*)::int from unit_events where action = 'handed_over'`) != 0 ||
		e.count(`select count(*)::int from tickets where id = $1::uuid and status = 'siap_diambil' and version = $2`, first.ID, first.Version) != 1 {
		t.Fatal("a refused handover changed something")
	}

	got, err := e.handOver(first, units[1].Tag, units[0].Tag)
	if err != nil {
		t.Fatalf("hand over %v", err)
	}

	if got.Status != "dipinjam" || got.Version != first.Version+1 || len(got.Units) != 2 || got.Units[0].ReturnedAt != nil || got.Units[0].ReturnOutcome != nil {
		t.Fatalf("the ticket is %+v and the units %+v", got.Ticket, got.Units)
	}

	if !slices.Equal([]string{got.Units[0].Tag, got.Units[1].Tag}, []string{units[0].Tag, units[1].Tag}) {
		t.Fatalf("the units are %+v", got.Units)
	}

	if e.count(`select count(*)::int from ticket_units where ticket_id = $1::uuid and returned_at is null`, first.ID) != 2 ||
		e.count(`select count(*)::int from unit_events where action = 'handed_over' and ticket_id = $1::uuid and admin_id = $2::uuid`, first.ID, e.adminID) != 2 {
		t.Fatal("the handover wrote no rows or no events")
	}

	unit, err := e.catalog.GetUnit(ctx, units[0].Tag)
	if err != nil || unit.Status != "borrowed" || unit.OpenTicketNumber != first.Number {
		t.Fatalf("the unit is %+v, %v", unit, err)
	}

	if held, _ := e.available(a.ID); held != 4 {
		t.Fatalf("the hold is %d", held)
	}

	// a unit that another open ticket holds cannot go to the second ticket
	_, err = e.handOver(second, units[0].Tag, units[2].Tag)
	requireRule(t, err, catalog.RuleUnitUnavailable)

	if _, err := e.handOver(second, units[2].Tag, units[3].Tag); err != nil {
		t.Fatalf("hand over the second ticket %v", err)
	}

	// the other model is still free
	if _, available := e.available(b.ID); available != 1 {
		t.Fatalf("the other model has %d", available)
	}

	// a handover only works for a ticket that is ready
	_, err = e.handOver(first, units[0].Tag, units[1].Tag)
	requireRule(t, err, catalog.RuleStatusConflict)
}

func TestAHandoverRefusesAStockThatFellBelowTheHold(t *testing.T) {
	e := newEnv(t)
	m, units := e.model("Example headset", "EXM", 2)
	ready := e.readyTicket(e.borrower(), m, 2)

	// the guards of the catalog stop this change, so a direct update stands for a unit that left the stock by another way
	e.exec(`update units set lifecycle = 'maintenance' where tag = $1`, units[1].Tag)

	_, err := e.handOver(ready, units[0].Tag, units[1].Tag)
	requireRule(t, err, catalog.RuleStockChanged)

	if e.count(`select count(*)::int from ticket_units`) != 0 {
		t.Fatal("the refused handover wrote rows")
	}
}

func TestAReturnClosesTheRowsAppliesTheOutcomesAndReleasesTheHold(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, units := e.model("Example headset", "EXM", 3)
	loan := e.loan(e.borrower(), m, units, 2)

	t.Run("a return that misses or adds a unit changes nothing", func(t *testing.T) {
		_, err := e.tickets.Return(ctx, e.adminID, loan.Number, ticket.ReturnRequest{Version: ptr(loan.Version), Units: []ticket.ReturnLine{{Tag: units[0].Tag, Outcome: "ok"}}})
		if got := fieldList(err); !slices.Equal(got, []string{"units required"}) {
			t.Fatalf("a missing unit fails with %v", got)
		}

		_, err = e.tickets.Return(ctx, e.adminID, loan.Number, ticket.ReturnRequest{Version: ptr(loan.Version), Units: []ticket.ReturnLine{
			{Tag: units[0].Tag, Outcome: "ok"}, {Tag: units[2].Tag, Outcome: "ok"},
		}})
		if got := fieldList(err); !slices.Equal(got, []string{"units[1].tag unknown_reference", "units required"}) {
			t.Fatalf("a stranger fails with %v", got)
		}

		_, err = e.tickets.Return(ctx, e.adminID, loan.Number, ticket.ReturnRequest{Version: ptr(loan.Version), Units: []ticket.ReturnLine{
			{Tag: units[0].Tag, Outcome: "ok"}, {Tag: units[1].Tag, Outcome: "lost"},
		}})
		if got := fieldList(err); !slices.Equal(got, []string{"units[1].note required"}) {
			t.Fatalf("a lost unit with no note fails with %v", got)
		}

		if e.count(`select count(*)::int from ticket_units where returned_at is null`) != 2 ||
			e.count(`select count(*)::int from tickets where id = $1::uuid and status = 'dipinjam' and version = $2`, loan.ID, loan.Version) != 1 {
			t.Fatal("a refused return changed something")
		}
	})

	got, err := e.tickets.Return(ctx, e.adminID, loan.Number, ticket.ReturnRequest{Version: ptr(loan.Version), Units: []ticket.ReturnLine{
		{Tag: units[1].Tag, Outcome: "lost", Note: ptr("The unit did not come back.")}, {Tag: units[0].Tag, Outcome: "ok"},
	}})
	if err != nil {
		t.Fatalf("return %v", err)
	}

	// the notes of a return are in the history and not in the decision note
	if got.Status != "dikembalikan" || got.Version != loan.Version+1 || got.DecisionNote != nil {
		t.Fatalf("the ticket is %+v", got.Ticket)
	}

	last := got.History[len(got.History)-1]
	if last.Status != "dikembalikan" || last.Note == nil || *last.Note != units[1].Tag+" (lost) - The unit did not come back." {
		t.Fatalf("the history is %+v", last)
	}

	if len(got.Units) != 2 || got.Units[0].ReturnedAt == nil || *got.Units[0].ReturnOutcome != "ok" || got.Units[0].ReturnNote != nil ||
		*got.Units[1].ReturnOutcome != "lost" || *got.Units[1].ReturnNote != "The unit did not come back." {
		t.Fatalf("the units are %+v", got.Units)
	}

	// the ticket no longer holds, and the lost unit is out of the stock
	if held, available := e.available(m.ID); held != 0 || available != 2 {
		t.Fatalf("held %d, available %d", held, available)
	}

	lost, err := e.catalog.GetUnit(ctx, units[1].Tag)
	if err != nil || lost.Lifecycle != "lost" || lost.LifecycleNote == nil || *lost.LifecycleNote != "The unit did not come back." || lost.Status != "lost" {
		t.Fatalf("the lost unit is %+v, %v", lost, err)
	}

	back, err := e.catalog.GetUnit(ctx, units[0].Tag)
	if err != nil || back.Status != "available" || back.OpenTicketNumber != "" {
		t.Fatalf("the returned unit is %+v, %v", back, err)
	}

	if events := e.unitEvents(units[1].Tag); !slices.Equal(events, []string{"lost", "returned", "handed_over", "added"}) {
		t.Fatalf("the events of the lost unit are %v", events)
	}

	if events := e.unitEvents(units[0].Tag); !slices.Equal(events, []string{"returned", "handed_over", "added"}) {
		t.Fatalf("the events of the returned unit are %v", events)
	}

	if e.count(`select count(*)::int from unit_events where ticket_id = $1::uuid and admin_id = $2::uuid`, loan.ID, e.adminID) != 5 {
		t.Fatal("the events do not name the ticket and the admin")
	}

	// the same unit can go out again
	again := e.loan(e.borrower(), m, []catalog.Unit{units[0]}, 1)
	if len(again.Units) != 1 || again.Units[0].Tag != units[0].Tag {
		t.Fatalf("the second loan is %+v", again.Units)
	}
}

func TestAReturnWithAMaintenanceOutcomeTakesTheUnitOutOfTheStock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, units := e.model("Example headset", "EXM", 3)
	loan := e.loan(e.borrower(), m, units, 2)

	got, err := e.tickets.Return(ctx, e.adminID, loan.Number, ticket.ReturnRequest{Version: ptr(loan.Version), Units: []ticket.ReturnLine{
		{Tag: units[0].Tag, Outcome: "maintenance", Note: ptr("Loose cable.")}, {Tag: units[1].Tag, Outcome: "ok"},
	}})
	if err != nil {
		t.Fatalf("return %v", err)
	}

	if got.History[len(got.History)-1].Note == nil || *got.History[len(got.History)-1].Note != units[0].Tag+" (maintenance) - Loose cable." {
		t.Fatalf("the history is %+v", got.History)
	}

	unit, err := e.catalog.GetUnit(ctx, units[0].Tag)
	if err != nil || unit.Lifecycle != "maintenance" || unit.LifecycleNote == nil || *unit.LifecycleNote != "Loose cable." {
		t.Fatalf("the unit is %+v, %v", unit, err)
	}

	if events := e.unitEvents(units[0].Tag); !slices.Equal(events, []string{"maintenance", "returned", "handed_over", "added"}) {
		t.Fatalf("the events are %v", events)
	}

	// a return with no note anywhere has no history note
	loan2 := e.loan(e.borrower(), m, []catalog.Unit{units[1]}, 1)

	got2, err := e.tickets.Return(ctx, e.adminID, loan2.Number, returnAll(loan2, "ok"))
	if err != nil || got2.History[len(got2.History)-1].Note != nil || got2.DecisionNote != nil {
		t.Fatalf("the second return gives %+v, %v", got2.History, err)
	}
}

func TestALateLoanCanBeReturned(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, units := e.model("Example headset", "EXM", 2)
	b := e.borrower()

	// today is 17 November in Jakarta, so a loan from 1 November for 3 days was due on 4 November
	id, number := e.insert(rawTicket{
		status: "dipinjam", pickup: "2026-11-01", days: 5, approved: ptr(3), who: b, items: []rawItem{{modelID: m.ID, qty: 1, approved: ptr(1)}},
	})
	e.exec(`insert into ticket_units (ticket_id, unit_id) values ($1::uuid, $2::uuid)`, id, e.unitID(units[0].Tag))

	before := mustGet(t, e, number)
	if before.EffectiveStatus != "terlambat" || before.Status != "dipinjam" || len(before.Units) != 1 {
		t.Fatalf("the loan is %+v", before.Ticket)
	}

	got, err := e.tickets.Return(ctx, e.adminID, number, ticket.ReturnRequest{Version: ptr(before.Version), Units: []ticket.ReturnLine{{Tag: units[0].Tag, Outcome: "ok"}}})
	if err != nil || got.Status != "dikembalikan" || got.EffectiveStatus != "dikembalikan" || got.Units[0].ReturnedAt == nil {
		t.Fatalf("the return gives %+v, %v", got.Ticket, err)
	}

	if held, _ := e.available(m.ID); held != 0 {
		t.Fatalf("the hold is %d", held)
	}
}

func TestTheGuardsOfTheCatalogHoldAgainstOpenTickets(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, units := e.model("Example headset", "EXM", 3)
	b := e.borrower()

	// an open ticket that only asks keeps the model from retiring
	made := e.mustCreate(b, item(m.ID, 2))

	_, err := e.catalog.UpdateModel(ctx, m.ID, catalog.ModelPatch{Retired: ptr(true)})
	requireRule(t, err, catalog.RuleStatusConflict)

	approved := e.approve(made, ap(m.ID, 2))

	_, err = e.catalog.UpdateModel(ctx, m.ID, catalog.ModelPatch{Retired: ptr(true)})
	requireRule(t, err, catalog.RuleStatusConflict)

	// two units are held by the approval, so only the third may leave the stock
	_, err = e.catalog.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: catalog.LifecycleMaintenance})
	if err != nil {
		t.Fatalf("a spare unit cannot leave %v", err)
	}

	_, err = e.catalog.ChangeLifecycle(ctx, units[1].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: catalog.LifecycleMaintenance})
	requireRule(t, err, catalog.RuleStockChanged)

	if _, err := e.catalog.ChangeLifecycle(ctx, units[0].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: catalog.LifecycleActive}); err != nil {
		t.Fatalf("reactivate %v", err)
	}

	loan, err := e.handOver(e.markReady(approved), units[1].Tag, units[2].Tag)
	if err != nil {
		t.Fatalf("hand over %v", err)
	}

	// a unit in an open row cannot change its lifecycle, and a unit that a ticket used cannot be deleted
	_, err = e.catalog.ChangeLifecycle(ctx, units[1].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: catalog.LifecycleLost, Note: ptr("Gone.")})
	requireRule(t, err, catalog.RuleUnitHeld)

	requireRule(t, e.catalog.DeleteUnit(ctx, units[1].Tag), catalog.RuleStatusConflict)
	requireRule(t, e.catalog.DeleteModel(ctx, m.ID), catalog.RuleStatusConflict)

	if _, err := e.tickets.Return(ctx, e.adminID, loan.Number, returnAll(loan, "ok")); err != nil {
		t.Fatalf("return %v", err)
	}

	// the loan is closed, so the model retires and the unit changes
	if _, err := e.catalog.ChangeLifecycle(ctx, units[1].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: catalog.LifecycleMaintenance}); err != nil {
		t.Fatalf("a returned unit cannot change %v", err)
	}

	requireRule(t, e.catalog.DeleteUnit(ctx, units[1].Tag), catalog.RuleStatusConflict)

	if _, err := e.catalog.UpdateModel(ctx, m.ID, catalog.ModelPatch{Retired: ptr(true)}); err != nil {
		t.Fatalf("a model with no open ticket cannot retire %v", err)
	}
}

func TestTheStepsOfAnAdminTakeTheLocksInOneOrderAndNeverDeadlock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a, aUnits := e.model("Example A headset", "EXA", 40)
	b, bUnits := e.model("Example B camera", "EXB", 40)

	deadlock := func(err error) bool {
		var pgErr *pgconn.PgError

		return errors.As(err, &pgErr) && pgErr.Code == "40P01"
	}

	for round := range 8 {
		made := e.mustCreate(e.borrower(), item(a.ID, 1), item(b.ID, 1))
		ready := e.markReady(e.approve(e.mustCreate(e.borrower(), item(b.ID, 1), item(a.ID, 1)), ap(a.ID, 1), ap(b.ID, 1)))
		loan := e.loan(e.borrower(), a, aUnits[20+round:], 1)
		newcomer := e.borrower()
		errs := make([]error, 7)

		together(
			func() {
				_, errs[0] = e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(1, ap(b.ID, 1), ap(a.ID, 1)))
			},
			func() { _, errs[1] = e.handOver(ready, aUnits[round].Tag, bUnits[round].Tag) },
			func() { _, errs[2] = e.create(newcomer, item(b.ID, 1), item(a.ID, 1)) },
			func() {
				_, errs[3] = e.catalog.ChangeLifecycle(ctx, bUnits[30+round].Tag, e.adminID, catalog.LifecycleChange{Lifecycle: catalog.LifecycleMaintenance})
			},
			func() {
				_, errs[4] = e.catalog.UpdateModel(ctx, a.ID, catalog.ModelPatch{Description: ptr(fmt.Sprintf("Round %d.", round))})
			},
			func() { _, errs[5] = e.tickets.Return(ctx, e.adminID, loan.Number, returnAll(loan, "ok")) },
			// open tickets ask for the model in every round, so the retirement is refused and never both waits and wins
			func() { _, errs[6] = e.catalog.UpdateModel(ctx, b.ID, catalog.ModelPatch{Retired: ptr(true)}) },
		)

		if ruleKindOf(errs[6]) != catalog.RuleStatusConflict {
			t.Fatalf("round %d, the retirement gave %v and not a status conflict, deadlock %v", round, errs[6], deadlock(errs[6]))
		}

		for i, err := range errs[:6] {
			if err != nil {
				t.Fatalf("round %d, step %d gave %v, deadlock %v", round, i, err, deadlock(err))
			}
		}
	}
}

func TestTheQueueFiltersByStatusTextAndDatesAndPagesWithACursor(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	alice := e.borrowerWith(session.Identity{Name: "Alice Example", NimNip: "245150200000001", Email: "alice@student.ub.ac.id"})
	bob := e.borrowerWith(session.Identity{Name: "Bob Builder", NimNip: "245150200000002", Email: "bob@student.ub.ac.id"})
	carol := e.borrowerWith(session.Identity{Name: "Carol Danvers", NimNip: "245150200000003", Email: "carol@student.ub.ac.id"})

	// today is 17 November in Jakarta. The numbers follow the order of the insert, and a later ticket is newer.
	numbers := map[string]string{}

	for _, step := range []struct {
		name string
		rt   rawTicket
	}{
		{"t1", rawTicket{status: "diajukan", pickup: "2026-11-20", days: 3, who: alice}},
		{"t2", rawTicket{status: "disetujui", pickup: "2026-11-18", days: 5, approved: ptr(2), who: bob}},
		{"t3", rawTicket{status: "siap_diambil", pickup: "2026-11-17", days: 1, who: bob}},
		{"t4", rawTicket{status: "dipinjam", pickup: "2026-11-10", days: 10, who: carol}},
		{"t5", rawTicket{status: "dipinjam", pickup: "2026-11-01", days: 5, approved: ptr(3), who: carol}},
		{"t6", rawTicket{status: "dikembalikan", pickup: "2026-10-01", days: 2}},
		{"t7", rawTicket{status: "dibatalkan", pickup: "2026-10-05", days: 2}},
		{"t8", rawTicket{status: "ditolak", pickup: "2026-10-06", days: 2}},
	} {
		_, numbers[step.name] = e.insert(step.rt)
	}

	names := func(f ticket.AdminFilter) (got []string, total int) {
		t.Helper()

		page, err := e.tickets.ListAdmin(ctx, f, catalog.PageRequest{Limit: 100})
		if err != nil {
			t.Fatalf("list %+v %v", f, err)
		}

		for _, it := range page.Items {
			for name, number := range numbers {
				if number == it.Number {
					got = append(got, name)
				}
			}
		}

		return got, page.Total
	}

	for name, tc := range map[string]struct {
		f    ticket.AdminFilter
		want []string
	}{
		"everything, newest first":    {ticket.AdminFilter{}, []string{"t8", "t7", "t6", "t5", "t4", "t3", "t2", "t1"}},
		"a loan that is not late":     {ticket.AdminFilter{Statuses: []string{"dipinjam"}}, []string{"t4"}},
		"a late loan":                 {ticket.AdminFilter{Statuses: []string{"terlambat"}}, []string{"t5"}},
		"both loans":                  {ticket.AdminFilter{Statuses: []string{"dipinjam", "terlambat"}}, []string{"t5", "t4"}},
		"two statuses":                {ticket.AdminFilter{Statuses: []string{"disetujui", "siap_diambil"}}, []string{"t3", "t2"}},
		"a closed status":             {ticket.AdminFilter{Statuses: []string{"dikembalikan"}}, []string{"t6"}},
		"a name in capitals":          {ticket.AdminFilter{Query: "ALICE"}, []string{"t1"}},
		"a nim part":                  {ticket.AdminFilter{Query: "000002"}, []string{"t3", "t2"}},
		"a number part in lower case": {ticket.AdminFilter{Query: "9001-aaaa"}, []string{"t1"}},
		"a wildcard is plain text":    {ticket.AdminFilter{Query: "%%"}, nil},
		"an underscore is plain text": {ticket.AdminFilter{Query: "a_"}, nil},
		"pickup from":                 {ticket.AdminFilter{PickupFrom: "2026-11-17"}, []string{"t3", "t2", "t1"}},
		"pickup to":                   {ticket.AdminFilter{PickupTo: "2026-11-01"}, []string{"t8", "t7", "t6", "t5"}},
		"pickup between":              {ticket.AdminFilter{PickupFrom: "2026-11-10", PickupTo: "2026-11-18"}, []string{"t4", "t3", "t2"}},
		"due between":                 {ticket.AdminFilter{DueFrom: "2026-11-20", DueTo: "2026-11-23"}, []string{"t4", "t2", "t1"}},
		"a status with a due date":    {ticket.AdminFilter{Statuses: []string{"dipinjam", "terlambat"}, DueTo: "2026-11-04"}, []string{"t5"}},
		"a text with a status":        {ticket.AdminFilter{Query: "carol", Statuses: []string{"terlambat"}}, []string{"t5"}},
		"nothing matches":             {ticket.AdminFilter{Query: "carol", Statuses: []string{"diajukan"}}, nil},
	} {
		got, total := names(tc.f)
		if !slices.Equal(got, tc.want) || total != len(tc.want) {
			t.Errorf("%s gives %v with the total %d, want %v", name, got, total, tc.want)
		}
	}

	// three pages of 3, 3 and 2 hold every ticket once, and the total counts them all on each page
	var seen []string

	cursor := ""

	for range 3 {
		page, err := e.tickets.ListAdmin(ctx, ticket.AdminFilter{}, catalog.PageRequest{Limit: 3, Cursor: cursor})
		if err != nil || page.Total != 8 {
			t.Fatalf("the page is %+v, %v", page, err)
		}

		for _, it := range page.Items {
			seen = append(seen, it.Number)

			if it.History == nil || it.Items == nil {
				t.Fatalf("the ticket %s has no history or items list", it.Number)
			}
		}

		cursor = page.NextCursor
	}

	all, _ := names(ticket.AdminFilter{})
	if len(seen) != 8 || cursor != "" || len(all) != 8 {
		t.Fatalf("the pages hold %v and the last cursor is %q", seen, cursor)
	}

	for i, number := range seen {
		if numbers[all[i]] != number {
			t.Fatalf("the page order differs at %d", i)
		}
	}

	// the cursor of another list is refused, and so are bad fields. The raw tickets have no history, so every one gets a row.
	e.exec(`insert into ticket_history (ticket_id, status, actor_kind) select id, status, 'guest' from tickets`)

	events, err := e.tickets.ListEvents(ctx, "", catalog.PageRequest{Limit: 1})
	if err != nil {
		t.Fatalf("list the events %v", err)
	}

	if _, err := e.tickets.ListAdmin(ctx, ticket.AdminFilter{}, catalog.PageRequest{Limit: 1, Cursor: events.NextCursor}); !errors.Is(err, pagination.ErrCursor) {
		t.Fatalf("a cursor of the feed gave %v", err)
	}

	if _, err := e.tickets.ListAdmin(ctx, ticket.AdminFilter{Query: "a", Statuses: []string{"x"}}, catalog.PageRequest{Limit: 1}); err == nil ||
		!slices.Equal(fieldList(err), []string{"q too_short", "status invalid_format"}) {
		t.Fatalf("bad fields gave %v", err)
	}
}

func TestTheQueueNamesTheAdminOfEachChange(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 3)
	approved := e.approve(e.mustCreate(e.borrower(), item(m.ID, 1)), ap(m.ID, 1))

	page, err := e.tickets.ListAdmin(ctx, ticket.AdminFilter{}, catalog.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Number != approved.Number {
		t.Fatalf("the page is %+v, %v", page, err)
	}

	h := page.Items[0].History
	if len(h) != 2 || h[0].ActorName != nil || h[1].ActorName == nil || *h[1].ActorName != "Example Admin" {
		t.Fatalf("the history is %+v", h)
	}
}

func TestTheFeedPagesByTimeAndIdAndNamesTheActors(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m, _ := e.model("Example headset", "EXM", 10)

	var made []ticket.Ticket

	for range 5 {
		made = append(made, e.mustCreate(e.borrower(), item(m.ID, 1)))
	}

	e.approve(made[0], ap(m.ID, 1))
	e.approve(made[1], ap(m.ID, 1))

	// every row gets the same time, so only the id keeps the order
	e.exec(`update ticket_history set at = '2026-11-18T00:00:00Z'`)

	var want []string

	rows, err := e.pool.Query(ctx, `select id::text from ticket_history order by at desc, id desc`)
	if err != nil {
		t.Fatalf("read the ids %v", err)
	}

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan %v", err)
		}

		want = append(want, id)
	}

	rows.Close()

	var got []string

	cursor := ""

	for pages := 0; pages < 10; pages++ {
		page, err := e.tickets.ListEvents(ctx, "", catalog.PageRequest{Limit: 3, Cursor: cursor})
		if err != nil || page.Total != 7 {
			t.Fatalf("the page is %+v, %v", page, err)
		}

		for _, ev := range page.Items {
			got = append(got, ev.ID)
		}

		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}

	if len(want) != 7 || !slices.Equal(got, want) {
		t.Fatalf("the feed gives %v, want %v", got, want)
	}

	one, err := e.tickets.ListEvents(ctx, made[0].Number, catalog.PageRequest{Limit: 10})
	if err != nil || one.Total != 2 || len(one.Items) != 2 {
		t.Fatalf("the feed of one ticket is %+v, %v", one, err)
	}

	for _, ev := range one.Items {
		switch ev.ActorKind {
		case "admin":
			if ev.Status != "disetujui" || ev.ActorName == nil || *ev.ActorName != "Example Admin" || ev.RequesterName == nil || ev.TicketNumber != made[0].Number {
				t.Fatalf("the admin event is %+v", ev)
			}
		default:
			if ev.Status != "diajukan" || ev.ActorName == nil || *ev.ActorName != *ev.RequesterName {
				t.Fatalf("the guest event is %+v", ev)
			}
		}
	}

	if none, err := e.tickets.ListEvents(ctx, "MGM-2611-0001-ZZZZ", catalog.PageRequest{Limit: 10}); err != nil || none.Total != 0 || len(none.Items) != 0 || none.Items == nil {
		t.Fatalf("an unknown number gives %+v, %v", none, err)
	}

	if _, err := e.tickets.ListEvents(ctx, "nope", catalog.PageRequest{Limit: 10}); !slices.Equal(fieldList(err), []string{"ticketNumber invalid_format"}) {
		t.Fatalf("a bad number gave %v", err)
	}

	if _, err := e.tickets.ListEvents(ctx, "", catalog.PageRequest{Limit: 1, Cursor: "forged"}); !errors.Is(err, pagination.ErrCursor) {
		t.Fatalf("a forged cursor gave %v", err)
	}

	// the retention job clears the name of the requester, and the name of the admin stays
	e.exec(`update tickets set requester_name = null, requester_nim_nip = null, requester_email = null, purpose = null, anonymized_at = now() where id = $1::uuid`, made[0].ID)

	cleared, err := e.tickets.ListEvents(ctx, made[0].Number, catalog.PageRequest{Limit: 10})
	if err != nil {
		t.Fatalf("list %v", err)
	}

	for _, ev := range cleared.Items {
		if ev.RequesterName != nil || (ev.ActorKind == "guest") != (ev.ActorName == nil) {
			t.Fatalf("the event after the retention is %+v", ev)
		}
	}
}

type recordingNotifier struct {
	mu      sync.Mutex
	notices []ticket.Notice
	err     error
}

func (r *recordingNotifier) Notify(_ context.Context, n ticket.Notice) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.notices = append(r.notices, n)

	return r.err
}

func (r *recordingNotifier) kinds() []ticket.NoticeKind {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]ticket.NoticeKind, 0, len(r.notices))
	for _, n := range r.notices {
		out = append(out, n.Kind)
	}

	return out
}

func TestTheBorrowerHearsOnlyOfChangesThatCommitted(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	notifier := &recordingNotifier{err: errors.New("the mail did not leave")}
	e.tickets.WithNotifier(notifier)

	m, _ := e.model("Example headset", "EXM", 5)
	b := e.borrower()

	// a refused decision sends nothing
	made := e.mustCreate(b, item(m.ID, 1))
	if _, err := e.tickets.Decide(ctx, e.adminID, made.Number, e.approvalOf(9, ap(m.ID, 1))); err == nil {
		t.Fatal("a stale version passed")
	}

	if len(notifier.kinds()) != 0 {
		t.Fatalf("a refused decision sent %v", notifier.kinds())
	}

	// a mail that fails never undoes the decision
	approved := e.approve(made, ap(m.ID, 1))
	ready := e.markReady(approved)

	if _, err := e.tickets.CancelByAdmin(ctx, e.adminID, ready.Number, ticket.AdminCancelRequest{Version: ptr(ready.Version), Note: ptr("Nobody came.")}); err != nil {
		t.Fatalf("cancel %v", err)
	}

	// the repeat changes nothing and sends nothing
	if _, err := e.tickets.CancelByAdmin(ctx, e.adminID, ready.Number, ticket.AdminCancelRequest{Version: ptr(ready.Version), Note: ptr("Nobody came.")}); err != nil {
		t.Fatalf("cancel again %v", err)
	}

	rejected := e.mustCreate(e.borrower(), item(m.ID, 1))
	if _, err := e.tickets.Decide(ctx, e.adminID, rejected.Number, ticket.DecisionRequest{Version: ptr(1), Decision: ticket.DecisionReject, Note: ptr("No.")}); err != nil {
		t.Fatalf("reject %v", err)
	}

	want := []ticket.NoticeKind{ticket.NoticeApproved, ticket.NoticeReady, ticket.NoticeCancelled, ticket.NoticeRejected}
	if got := notifier.kinds(); !slices.Equal(got, want) {
		t.Fatalf("the notices are %v, want %v", got, want)
	}

	first := notifier.notices[0]
	if first.Number != made.Number || first.Email == nil || *first.Email != b.identity.Email || first.Name == nil || *first.Name != b.identity.Name {
		t.Fatalf("the first notice is %+v", first)
	}
}
