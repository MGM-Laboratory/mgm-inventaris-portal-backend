package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/dashboard"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ticket"
)

const (
	sampleAdminID = "00000000-0000-4000-8000-000000000009"
	sampleTag1    = "MGM-EXM-001"
	sampleTag2    = "MGM-EXM-002"

	adminTicketsPath = "/api/admin/tickets"
	adminTicketPath  = adminTicketsPath + "/" + sampleNumber
	decisionPath     = adminTicketPath + "/decision"
	readyPath        = adminTicketPath + "/ready"
	handoverPath     = adminTicketPath + "/handover"
	returnPath       = adminTicketPath + "/return"
	adminCancelPath  = adminTicketPath + "/cancel"
	eventsPath       = "/api/admin/ticket-events"
	dashboardPath    = "/api/admin/dashboard"

	approveJSON  = `{"version":1,"decision":"approve","items":[{"modelId":"` + sampleModelID + `","approvedQty":2}],"durationDays":7,"note":"Take them on Monday."}`
	rejectJSON   = `{"version":1,"decision":"reject","note":"The items are not free on those days."}`
	versionJSON  = `{"version":2}`
	handoverJSON = `{"version":3,"tags":["` + sampleTag1 + `","` + sampleTag2 + `"]}`
	returnJSON   = `{"version":4,"units":[{"tag":"` + sampleTag1 + `","outcome":"ok"},{"tag":"` + sampleTag2 + `","outcome":"lost","note":"The unit did not come back."}]}`
	cancelJSON   = `{"version":3,"note":"Nobody collected the items."}`
)

var sampleAdminTicket = ticket.AdminTicket{
	AdminSummary: ticket.AdminSummary{
		Ticket: ticket.Ticket{
			ID: sampleTicketID, Number: sampleNumber, Status: "dipinjam", EffectiveStatus: "terlambat", Version: 4, CreatedAt: "2026-11-17T03:00:00Z",
			Requester:  sampleTicket.Requester,
			Purpose:    ptrTo("Example loan for a lab exercise."),
			PickupDate: "2026-11-20", DurationDays: 7, ApprovedDurationDays: ptrTo(5), DueDate: "2026-11-25",
			Items:        []ticket.Item{{ModelID: sampleModelID, ModelName: "Example headset", Qty: 2, ApprovedQty: ptrTo(2)}},
			DecisionNote: ptrTo("Take them on Monday."),
			History:      []ticket.HistoryEntry{},
		},
		History: []ticket.AdminHistoryEntry{
			{At: "2026-11-17T03:00:00Z", Status: "diajukan", ActorKind: "guest"},
			{At: "2026-11-17T04:00:00Z", Status: "disetujui", ActorKind: "admin", ActorName: ptrTo("Example Admin"), Note: ptrTo("Take them on Monday.")},
		},
	},
	Units: []ticket.TicketUnit{{Tag: sampleTag1, HandedOverAt: "2026-11-20T03:00:00Z"}},
}

// fakeAdminTickets answers every call with a canned result, or with err when it is set. It records the arguments of the last call.
type fakeAdminTickets struct {
	mu  sync.Mutex
	err error

	calls    []string
	adminID  string
	number   string
	filter   ticket.AdminFilter
	page     catalog.PageRequest
	decision ticket.DecisionRequest
	step     ticket.StepRequest
	handover ticket.HandoverRequest
	ret      ticket.ReturnRequest
	cancel   ticket.AdminCancelRequest
}

func (f *fakeAdminTickets) enter(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, name)
}

func (f *fakeAdminTickets) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func (f *fakeAdminTickets) ListAdmin(_ context.Context, filter ticket.AdminFilter, req catalog.PageRequest) (pagination.Page[ticket.AdminSummary], error) {
	f.enter("ListAdmin")
	f.filter, f.page = filter, req

	return pagination.Page[ticket.AdminSummary]{Items: []ticket.AdminSummary{sampleAdminTicket.AdminSummary}, Total: 1, NextCursor: "next"}, f.err
}

func (f *fakeAdminTickets) GetAdmin(_ context.Context, number string) (ticket.AdminTicket, error) {
	f.enter("GetAdmin")
	f.number = number

	return sampleAdminTicket, f.err
}

func (f *fakeAdminTickets) Decide(_ context.Context, adminID, number string, req ticket.DecisionRequest) (ticket.AdminTicket, error) {
	f.enter("Decide")
	f.adminID, f.number, f.decision = adminID, number, req

	return sampleAdminTicket, f.err
}

func (f *fakeAdminTickets) MarkReady(_ context.Context, adminID, number string, req ticket.StepRequest) (ticket.AdminTicket, error) {
	f.enter("MarkReady")
	f.adminID, f.number, f.step = adminID, number, req

	return sampleAdminTicket, f.err
}

func (f *fakeAdminTickets) HandOver(_ context.Context, adminID, number string, req ticket.HandoverRequest) (ticket.AdminTicket, error) {
	f.enter("HandOver")
	f.adminID, f.number, f.handover = adminID, number, req

	return sampleAdminTicket, f.err
}

func (f *fakeAdminTickets) Return(_ context.Context, adminID, number string, req ticket.ReturnRequest) (ticket.AdminTicket, error) {
	f.enter("Return")
	f.adminID, f.number, f.ret = adminID, number, req

	return sampleAdminTicket, f.err
}

func (f *fakeAdminTickets) CancelByAdmin(_ context.Context, adminID, number string, req ticket.AdminCancelRequest) (ticket.AdminTicket, error) {
	f.enter("CancelByAdmin")
	f.adminID, f.number, f.cancel = adminID, number, req

	return sampleAdminTicket, f.err
}

func (f *fakeAdminTickets) ListEvents(_ context.Context, number string, req catalog.PageRequest) (pagination.Page[ticket.Event], error) {
	f.enter("ListEvents")
	f.number, f.page = number, req

	event := ticket.Event{
		ID: "00000000-0000-4000-8000-0000000000e1", At: "2026-11-17T04:00:00Z", TicketNumber: sampleNumber, RequesterName: ptrTo("Example Borrower"),
		Status: "disetujui", ActorKind: "admin", ActorName: ptrTo("Example Admin"),
	}

	return pagination.Page[ticket.Event]{Items: []ticket.Event{event}, Total: 1}, f.err
}

type fakeDashboard struct {
	err   error
	calls int
}

func (f *fakeDashboard) Counts(context.Context) (dashboard.Counts, error) {
	f.calls++

	counts := dashboard.Counts{
		Tickets:    dashboard.OpenTickets{Submitted: 2, Approved: 1, Ready: 0, Borrowed: 3, Late: 1},
		Units:      dashboard.Units{Available: 20, Borrowed: 4, Maintenance: 1},
		ByCategory: []dashboard.CategoryCounts{{CategoryID: sampleCategory.ID, Name: "Example category", Tone: "blue", Total: 25, Units: dashboard.Units{Available: 20, Borrowed: 4, Maintenance: 1}}},
	}
	counts.ByStatus = dashboard.AllTickets{OpenTickets: counts.Tickets, Returned: 12, Cancelled: 2, Rejected: 1}

	return counts, f.err
}

// adminTicketEnv is the application of the ticket env with the fake services of the admin side.
type adminTicketEnv struct {
	*ticketEnv
	adm  *fakeAdminTickets
	dash *fakeDashboard
}

func newAdminTicketEnv(t *testing.T) *adminTicketEnv {
	t.Helper()

	e := &adminTicketEnv{ticketEnv: newTicketEnv(t), adm: &fakeAdminTickets{}, dash: &fakeDashboard{}}
	e.app = NewAPI(testConfig(), Deps{
		Sessions: e.store, Limiter: e.lim, Tickets: e.tix, AdminTickets: e.adm, Dashboard: e.dash, Idempotency: e.keys,
	})

	return e
}

// adminTicketCase is one request to a route of the admin and the answer that the contract names for it. The field op is the key of
// the operation in openapi.yaml, so the test of the contract can tell which status codes have a case.
type adminTicketCase struct {
	op      string
	name    string
	as      string
	method  string
	path    string
	body    string
	extra   map[string]string
	arrange func(*adminTicketEnv)
	status  int
	code    string
	check   func(*testing.T, *adminTicketEnv, *http.Response, []byte)
}

func adminFail(err error) func(*adminTicketEnv) {
	return func(e *adminTicketEnv) { e.adm.err, e.dash.err = err, err }
}

func adminCalls(t *testing.T, e *adminTicketEnv, want ...string) {
	t.Helper()

	if got := e.adm.called(); !slices.Equal(got, want) {
		t.Fatalf("the ticket service was called with %v, want %v", got, want)
	}
}

// adminTicketShape checks that an answer has every field of the schema AdminTicket, with the name of the admin in the history and the
// units, and no id.
func adminTicketShape(t *testing.T, body []byte) {
	t.Helper()

	got := decode[map[string]json.RawMessage](t, body)

	for _, key := range []string{"number", "status", "effectiveStatus", "version", "createdAt", "requester", "purpose", "pickupDate", "durationDays",
		"approvedDurationDays", "dueDate", "items", "decisionNote", "history", "anonymizedAt", "units"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("the ticket has no %s, %s", key, body)
		}
	}

	if _, ok := got["id"]; ok {
		t.Fatalf("the ticket has the field id, %s", body)
	}

	history := decode[[]map[string]json.RawMessage](t, got["history"])
	if len(history) != 2 {
		t.Fatalf("the history is %s", got["history"])
	}

	if _, ok := history[0]["actorName"]; ok {
		t.Fatalf("a guest entry names an admin, %s", got["history"])
	}

	if string(history[1]["actorName"]) != `"Example Admin"` {
		t.Fatalf("an admin entry does not name the admin, %s", got["history"])
	}

	for _, key := range []string{"at", "status", "actorKind", "note"} {
		if _, ok := history[0][key]; !ok {
			t.Fatalf("a history entry has no %s, %s", key, got["history"])
		}
	}

	units := decode[[]map[string]json.RawMessage](t, got["units"])
	if len(units) != 1 {
		t.Fatalf("the units are %s", got["units"])
	}

	for _, key := range []string{"tag", "handedOverAt", "returnedAt", "returnOutcome", "returnNote"} {
		if _, ok := units[0][key]; !ok {
			t.Fatalf("a unit has no %s, %s", key, got["units"])
		}
	}
}

// adminStep describes one of the five routes that change a ticket. The cases of the five are alike, so one table makes them.
type adminStep struct {
	op       string
	path     string
	body     string
	call     string
	arranged func(*testing.T, *adminTicketEnv)
}

func adminSteps() []adminStep {
	return []adminStep{
		{"POST /api/admin/tickets/{number}/decision", decisionPath, approveJSON, "Decide", func(t *testing.T, e *adminTicketEnv) {
			t.Helper()

			req := e.adm.decision
			if req.Version == nil || *req.Version != 1 || req.Decision != "approve" || len(req.Items) != 1 || req.Items[0].ModelID != sampleModelID ||
				req.Items[0].ApprovedQty == nil || *req.Items[0].ApprovedQty != 2 || req.DurationDays == nil || *req.DurationDays != 7 ||
				req.Note == nil || *req.Note != "Take them on Monday." {
				t.Fatalf("the decision is %+v", req)
			}
		}},
		{"POST /api/admin/tickets/{number}/ready", readyPath, versionJSON, "MarkReady", func(t *testing.T, e *adminTicketEnv) {
			t.Helper()

			if e.adm.step.Version == nil || *e.adm.step.Version != 2 {
				t.Fatalf("the step is %+v", e.adm.step)
			}
		}},
		{"POST /api/admin/tickets/{number}/handover", handoverPath, handoverJSON, "HandOver", func(t *testing.T, e *adminTicketEnv) {
			t.Helper()

			req := e.adm.handover
			if req.Version == nil || *req.Version != 3 || !slices.Equal(req.Tags, []string{sampleTag1, sampleTag2}) {
				t.Fatalf("the handover is %+v", req)
			}
		}},
		{"POST /api/admin/tickets/{number}/return", returnPath, returnJSON, "Return", func(t *testing.T, e *adminTicketEnv) {
			t.Helper()

			req := e.adm.ret
			if req.Version == nil || *req.Version != 4 || len(req.Units) != 2 || req.Units[0].Tag != sampleTag1 || req.Units[0].Outcome != "ok" ||
				req.Units[0].Note != nil || req.Units[1].Outcome != "lost" || req.Units[1].Note == nil || *req.Units[1].Note != "The unit did not come back." {
				t.Fatalf("the return is %+v", req)
			}
		}},
		{"POST /api/admin/tickets/{number}/cancel", adminCancelPath, cancelJSON, "CancelByAdmin", func(t *testing.T, e *adminTicketEnv) {
			t.Helper()

			req := e.adm.cancel
			if req.Version == nil || *req.Version != 3 || req.Note == nil || *req.Note != "Nobody collected the items." {
				t.Fatalf("the cancel is %+v", req)
			}
		}},
	}
}

func adminTicketCases() []adminTicketCase {
	var cases []adminTicketCase

	add := func(more ...adminTicketCase) { cases = append(cases, more...) }

	// the cases of every route of the admin, which are no session, the wrong roles, a bug and an outage
	common := func(op, method, path, body string) []adminTicketCase {
		return []adminTicketCase{
			{op: op, name: "no session", as: "none", method: method, path: path, body: body, status: 401, code: "unauthenticated"},
			{op: op, name: "a guest", as: "guest", method: method, path: path, body: body, status: 403, code: "forbidden",
				check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) { adminCalls(t, e) }},
			{op: op, name: "a guest with no identity", as: "noid", method: method, path: path, body: body, status: 403, code: "forbidden"},
			{op: op, name: "a setup session", as: "setup", method: method, path: path, body: body, status: 403, code: "forbidden",
				check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) { adminCalls(t, e) }},
			{op: op, name: "a bug", as: "admin", method: method, path: path, body: body, arrange: adminFail(errors.New("example failure")), status: 500, code: "internal"},
			{op: op, name: "an outage", as: "admin", method: method, path: path, body: body, arrange: adminFail(&outageError{}), status: 503, code: "unavailable"},
		}
	}

	const list = "GET /api/admin/tickets"

	add(common(list, "GET", adminTicketsPath, "")...)
	add(
		adminTicketCase{op: list, name: "an admin reads the first page", as: "admin", method: "GET", path: adminTicketsPath, status: 200,
			check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, body []byte) {
				adminCalls(t, e, "ListAdmin")

				page := decode[struct {
					Items      []json.RawMessage `json:"items"`
					Total      int               `json:"total"`
					NextCursor string            `json:"nextCursor"`
				}](t, body)
				if len(page.Items) != 1 || page.Total != 1 || page.NextCursor != "next" {
					t.Fatalf("the page is %s", body)
				}

				item := decode[map[string]json.RawMessage](t, page.Items[0])
				if _, ok := item["units"]; ok {
					t.Fatalf("an item of the queue has the units, %s", page.Items[0])
				}

				if !strings.Contains(string(item["history"]), `"actorName":"Example Admin"`) {
					t.Fatalf("the history of an item does not name the admin, %s", item["history"])
				}

				if e.adm.page.Limit != 50 || e.adm.page.Cursor != "" || e.adm.filter.Query != "" || e.adm.filter.Statuses != nil {
					t.Fatalf("the service got %+v and %+v", e.adm.page, e.adm.filter)
				}
			}},
		adminTicketCase{op: list, name: "the filters reach the service", as: "admin", method: "GET",
			path:   adminTicketsPath + "?q=head&status=disetujui,siap_diambil&pickupFrom=2026-11-01&pickupTo=2026-11-30&dueFrom=2026-11-02&dueTo=2026-12-01&limit=5&cursor=abc",
			status: 200,
			check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) {
				f := e.adm.filter

				if f.Query != "head" || !slices.Equal(f.Statuses, []string{"disetujui", "siap_diambil"}) || f.PickupFrom != "2026-11-01" ||
					f.PickupTo != "2026-11-30" || f.DueFrom != "2026-11-02" || f.DueTo != "2026-12-01" || e.adm.page.Limit != 5 || e.adm.page.Cursor != "abc" {
					t.Fatalf("the service got %+v and %+v", e.adm.page, f)
				}
			}},
		adminTicketCase{op: list, name: "an empty status is one empty status", as: "admin", method: "GET", path: adminTicketsPath + "?status=", status: 200,
			check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) {
				if !slices.Equal(e.adm.filter.Statuses, []string{""}) {
					t.Fatalf("the statuses are %q", e.adm.filter.Statuses)
				}
			}},
		adminTicketCase{op: list, name: "a limit of 0", as: "admin", method: "GET", path: adminTicketsPath + "?limit=0", status: 422, code: "validation_failed",
			check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) { adminCalls(t, e) }},
		adminTicketCase{op: list, name: "a filter that the service refuses", as: "admin", method: "GET", path: adminTicketsPath + "?q=a",
			arrange: adminFail(validation("q", "too_short")), status: 422, code: "validation_failed"},
		adminTicketCase{op: list, name: "a cursor that does not verify", as: "admin", method: "GET", path: adminTicketsPath + "?cursor=forged",
			arrange: adminFail(pagination.ErrCursor), status: 422, code: "validation_failed"},
	)

	const get = "GET /api/admin/tickets/{number}"

	add(common(get, "GET", adminTicketPath, "")...)
	add(
		adminTicketCase{op: get, name: "an admin reads any ticket", as: "admin", method: "GET", path: adminTicketPath, status: 200,
			check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, body []byte) {
				adminCalls(t, e, "GetAdmin")
				adminTicketShape(t, body)

				if e.adm.number != sampleNumber {
					t.Fatalf("the service got %q", e.adm.number)
				}
			}},
		adminTicketCase{op: get, name: "a number that no ticket has", as: "admin", method: "GET", path: adminTicketPath, arrange: adminFail(ticket.ErrNotFound),
			status: 404, code: "not_found"},
	)

	for _, step := range adminSteps() {
		add(common(step.op, "POST", step.path, step.body)...)
		add(
			adminTicketCase{op: step.op, name: "an admin takes the step", as: "admin", method: "POST", path: step.path, body: step.body, status: 200,
				check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, body []byte) {
					adminCalls(t, e, step.call)
					adminTicketShape(t, body)
					step.arranged(t, e)

					if e.adm.number != sampleNumber || e.adm.adminID != sampleAdminID {
						t.Fatalf("the service got %q and the admin %q", e.adm.number, e.adm.adminID)
					}
				}},
			adminTicketCase{op: step.op, name: "a body that is not JSON", as: "admin", method: "POST", path: step.path, body: "{", status: 400, code: "bad_request",
				check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) { adminCalls(t, e) }},
			adminTicketCase{op: step.op, name: "a version of the wrong type", as: "admin", method: "POST", path: step.path, body: `{"version":"1"}`, status: 400, code: "bad_request",
				check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) { adminCalls(t, e) }},
			adminTicketCase{op: step.op, name: "a wrong CSRF token", as: "admin", method: "POST", path: step.path, body: step.body, extra: map[string]string{"X-CSRF-Token": "wrong"},
				status: 403, code: "csrf_failed", check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) { adminCalls(t, e) }},
			adminTicketCase{op: step.op, name: "another origin", as: "admin", method: "POST", path: step.path, body: step.body, extra: map[string]string{"Origin": "https://evil.example.org"},
				status: 403, code: "origin_not_allowed", check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) { adminCalls(t, e) }},
			adminTicketCase{op: step.op, name: "a number that no ticket has", as: "admin", method: "POST", path: step.path, body: step.body, arrange: adminFail(ticket.ErrNotFound),
				status: 404, code: "not_found"},
			adminTicketCase{op: step.op, name: "a status that does not allow the step", as: "admin", method: "POST", path: step.path, body: step.body,
				arrange: adminFail(&catalog.RuleError{Kind: catalog.RuleStatusConflict, Message: "A ticket in the status dibatalkan does not allow this step."}),
				status:  409, code: "status_conflict"},
			adminTicketCase{op: step.op, name: "a stale version", as: "admin", method: "POST", path: step.path, body: step.body,
				arrange: adminFail(&catalog.RuleError{Kind: catalog.RuleVersionConflict, Message: "The ticket changed after you read it."}),
				status:  409, code: "version_conflict"},
			adminTicketCase{op: step.op, name: "a field that fails a rule", as: "admin", method: "POST", path: step.path, body: step.body, arrange: adminFail(validation("version", "required")),
				status: 422, code: "validation_failed",
				check: func(t *testing.T, _ *adminTicketEnv, _ *http.Response, body []byte) {
					if got := decodeError(t, body).Error.Details; len(got) != 1 || got[0].Field != "version" || got[0].Code != "required" {
						t.Fatalf("the details are %+v", got)
					}
				}},
		)
	}

	const decide = "POST /api/admin/tickets/{number}/decision"

	add(
		adminTicketCase{op: decide, name: "a rejection", as: "admin", method: "POST", path: decisionPath, body: rejectJSON, status: 200,
			check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) {
				req := e.adm.decision
				if req.Decision != "reject" || req.Note == nil || *req.Note != "The items are not free on those days." || len(req.Items) != 0 || req.DurationDays != nil {
					t.Fatalf("the decision is %+v", req)
				}
			}},
		adminTicketCase{op: decide, name: "the stock fell", as: "admin", method: "POST", path: decisionPath, body: approveJSON,
			arrange: adminFail(&catalog.RuleError{Kind: catalog.RuleStockChanged, Message: "The stock of an item changed.", Details: []catalog.FieldError{
				{Field: "items[0].approvedQty", Code: "too_many", Message: "At most 1 are available."},
			}}), status: 409, code: "stock_changed",
			check: func(t *testing.T, _ *adminTicketEnv, _ *http.Response, body []byte) {
				if got := decodeError(t, body).Error.Details; len(got) != 1 || got[0].Field != "items[0].approvedQty" || got[0].Code != "too_many" {
					t.Fatalf("the details are %+v", got)
				}
			}},
	)

	const handover = "POST /api/admin/tickets/{number}/handover"

	add(adminTicketCase{op: handover, name: "a unit that cannot be handed over", as: "admin", method: "POST", path: handoverPath, body: handoverJSON,
		arrange: adminFail(&catalog.RuleError{Kind: catalog.RuleUnitUnavailable, Message: "The unit cannot be handed over."}), status: 409, code: "unit_unavailable"},
		adminTicketCase{op: handover, name: "the stock fell below the approved quantity", as: "admin", method: "POST", path: handoverPath, body: handoverJSON,
			arrange: adminFail(&catalog.RuleError{Kind: catalog.RuleStockChanged, Message: "The stock is below the approved quantity."}), status: 409, code: "stock_changed",
			check: func(t *testing.T, _ *adminTicketEnv, _ *http.Response, body []byte) {
				if got := decodeError(t, body).Error.Details; len(got) != 0 {
					t.Fatalf("the details are %+v", got)
				}
			}},
	)

	const events = "GET /api/admin/ticket-events"

	add(common(events, "GET", eventsPath, "")...)
	add(
		adminTicketCase{op: events, name: "an admin reads the feed", as: "admin", method: "GET", path: eventsPath + "?ticketNumber=" + sampleNumber + "&limit=7&cursor=abc", status: 200,
			check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, body []byte) {
				adminCalls(t, e, "ListEvents")

				page := decode[struct {
					Items []map[string]json.RawMessage `json:"items"`
					Total int                          `json:"total"`
				}](t, body)
				if len(page.Items) != 1 || page.Total != 1 {
					t.Fatalf("the page is %s", body)
				}

				for _, key := range []string{"id", "at", "ticketNumber", "requesterName", "status", "actorKind", "actorName", "note"} {
					if _, ok := page.Items[0][key]; !ok {
						t.Fatalf("an event has no %s, %s", key, body)
					}
				}

				if e.adm.number != sampleNumber || e.adm.page.Limit != 7 || e.adm.page.Cursor != "abc" {
					t.Fatalf("the service got %q and %+v", e.adm.number, e.adm.page)
				}
			}},
		adminTicketCase{op: events, name: "no filter", as: "admin", method: "GET", path: eventsPath, status: 200,
			check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, _ []byte) {
				if e.adm.number != "" || e.adm.page.Limit != 50 {
					t.Fatalf("the service got %q and %+v", e.adm.number, e.adm.page)
				}
			}},
		adminTicketCase{op: events, name: "a limit over 100", as: "admin", method: "GET", path: eventsPath + "?limit=101", status: 422, code: "validation_failed"},
		adminTicketCase{op: events, name: "a number with the wrong shape", as: "admin", method: "GET", path: eventsPath + "?ticketNumber=x",
			arrange: adminFail(validation("ticketNumber", "invalid_format")), status: 422, code: "validation_failed"},
	)

	const board = "GET /api/admin/dashboard"

	add(common(board, "GET", dashboardPath, "")...)
	add(adminTicketCase{op: board, name: "an admin reads the counts", as: "admin", method: "GET", path: dashboardPath, status: 200,
		check: func(t *testing.T, e *adminTicketEnv, _ *http.Response, body []byte) {
			if e.dash.calls != 1 {
				t.Fatalf("the dashboard was read %d times", e.dash.calls)
			}

			parts := decode[map[string]json.RawMessage](t, body)
			got := map[string]map[string]any{}

			for _, key := range []string{"tickets", "byStatus", "units"} {
				got[key] = decode[map[string]any](t, parts[key])
			}

			if len(got["tickets"]) != 5 || got["tickets"]["siapDiambil"] != 0.0 || got["tickets"]["terlambat"] != 1.0 || got["tickets"]["dipinjam"] != 3.0 {
				t.Fatalf("the open tickets are %v", got["tickets"])
			}

			if len(got["byStatus"]) != 8 || got["byStatus"]["dikembalikan"] != 12.0 || got["byStatus"]["diajukan"] != 2.0 {
				t.Fatalf("the tickets by status are %v", got["byStatus"])
			}

			if len(got["units"]) != 5 || got["units"]["available"] != 20.0 {
				t.Fatalf("the units are %v", got["units"])
			}

			var raw struct {
				ByCategory []map[string]any `json:"byCategory"`
			}

			if err := json.Unmarshal(body, &raw); err != nil || len(raw.ByCategory) != 1 || len(raw.ByCategory[0]) != 9 || raw.ByCategory[0]["total"] != 25.0 ||
				raw.ByCategory[0]["tone"] != "blue" || raw.ByCategory[0]["maintenance"] != 1.0 {
				t.Fatalf("the categories are %s, %v", body, err)
			}
		}})

	return cases
}

func TestEveryAdminTicketRouteAnswersEveryStatusOfTheContract(t *testing.T) {
	for _, tc := range adminTicketCases() {
		t.Run(tc.op+" "+tc.name, func(t *testing.T) {
			e := newAdminTicketEnv(t)

			if tc.arrange != nil {
				tc.arrange(e)
			}

			res, body := e.call(e.clientFor(tc.as), tc.method, tc.path, tc.body, tc.extra)
			if res.StatusCode != tc.status {
				t.Fatalf("the status is %d, want %d, %s", res.StatusCode, tc.status, body)
			}

			if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Request-Id") == "" {
				t.Fatalf("the headers are %v", res.Header)
			}

			if tc.code != "" {
				got := decodeError(t, body)
				if got.Error.Code != tc.code || got.Error.Message == "" || got.Error.RequestID != res.Header.Get("X-Request-Id") {
					t.Fatalf("the error is %s", body)
				}
			} else if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
				t.Fatalf("the content type is %q", res.Header.Get("Content-Type"))
			}

			if tc.check != nil {
				tc.check(t, e, res, body)
			}
		})
	}
}

// adminTicketContractCodes lists the status codes that openapi.yaml names for each admin ticket operation. A script made the list from
// the paths and the response codes of the operations listAdminTickets, getAdminTicket, decideTicket, markTicketReady, handOverTicket,
// returnTicket, cancelAdminTicket, listTicketEvents and getDashboard, and it must be made again when the contract changes.
var adminTicketContractCodes = map[string][]int{
	"GET /api/admin/tickets":                    {200, 401, 403, 422, 500, 503},
	"GET /api/admin/tickets/{number}":           {200, 401, 403, 404, 500, 503},
	"POST /api/admin/tickets/{number}/decision": {200, 400, 401, 403, 404, 409, 422, 500, 503},
	"POST /api/admin/tickets/{number}/ready":    {200, 400, 401, 403, 404, 409, 422, 500, 503},
	"POST /api/admin/tickets/{number}/handover": {200, 400, 401, 403, 404, 409, 422, 500, 503},
	"POST /api/admin/tickets/{number}/return":   {200, 400, 401, 403, 404, 409, 422, 500, 503},
	"POST /api/admin/tickets/{number}/cancel":   {200, 400, 401, 403, 404, 409, 422, 500, 503},
	"GET /api/admin/ticket-events":              {200, 401, 403, 422, 500, 503},
	"GET /api/admin/dashboard":                  {200, 401, 403, 500, 503},
}

func TestEveryStatusOfTheAdminTicketContractHasACase(t *testing.T) {
	have := map[string]map[int]bool{}

	for _, tc := range adminTicketCases() {
		if have[tc.op] == nil {
			have[tc.op] = map[int]bool{}
		}

		have[tc.op][tc.status] = true
	}

	for op, codes := range adminTicketContractCodes {
		for _, status := range codes {
			if !have[op][status] {
				t.Errorf("%s has no case for the status %d", op, status)
			}
		}

		for status := range have[op] {
			if !slices.Contains(codes, status) {
				t.Errorf("%s answers %d, which the contract does not list", op, status)
			}
		}
	}

	if len(have) != len(adminTicketContractCodes) {
		t.Fatalf("the cases cover %d operations and the contract list has %d", len(have), len(adminTicketContractCodes))
	}
}

func TestTheAdminTicketRoutesAreTheOnesOfTheContract(t *testing.T) {
	app := NewAPI(testConfig(), Deps{Sessions: newFakeStore(), Limiter: newTicketLimiter()})

	have := map[string]bool{}

	for _, route := range app.GetRoutes() {
		have[route.Method+" "+route.Path] = true
	}

	for op := range adminTicketContractCodes {
		method, path, _ := strings.Cut(op, " ")
		path = strings.NewReplacer("{number}", ":number").Replace(path)

		if !have[method+" "+path] {
			t.Errorf("the route %s %s is not served", method, path)
		}
	}
}

func TestTheAdminTicketRoutesTakeNoIdempotencyKey(t *testing.T) {
	// the contract names four routes that accept the header, and none of them is a step of a ticket
	e := newAdminTicketEnv(t)

	res, body := e.call(e.admin, "POST", readyPath, versionJSON, map[string]string{"Idempotency-Key": "example-key-0001"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the step answers %d, %s", res.StatusCode, body)
	}

	if len(e.keys.keys) != 0 {
		t.Fatal("a step of a ticket claimed a key")
	}
}

func TestTheAnswersOfTheBorrowerNeverNameAnAdmin(t *testing.T) {
	e := newTicketEnv(t)

	for _, tc := range []struct{ method, path, body string }{{"GET", ticketPath, ""}, {"GET", createPath, ""}, {"POST", cancelPath, ""}} {
		res, body := e.call(e.guest, tc.method, tc.path, tc.body, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s %s answers %d, %s", tc.method, tc.path, res.StatusCode, body)
		}

		if strings.Contains(string(body), "actorName") || strings.Contains(string(body), "Example Admin") {
			t.Fatalf("%s %s names an admin, %s", tc.method, tc.path, body)
		}
	}
}
