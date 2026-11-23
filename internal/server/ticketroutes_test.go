package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ticket"
)

// ticketCase is one request to a route of the tickets and the answer that the contract names for it. The field op is the key of the
// operation in openapi.yaml, so the test of the contract can tell which status codes have a case.
type ticketCase struct {
	op      string
	name    string
	as      string
	method  string
	path    string
	body    string
	extra   map[string]string
	arrange func(*ticketEnv)
	status  int
	code    string
	check   func(*testing.T, *ticketEnv, *http.Response, []byte)
}

func ticketFail(err error) func(*ticketEnv) {
	return func(e *ticketEnv) { e.tix.err = err }
}

func ticketCalls(t *testing.T, e *ticketEnv, want ...string) {
	t.Helper()

	if got := e.tix.called(); !slices.Equal(got, want) {
		t.Fatalf("the ticket service was called with %v, want %v", got, want)
	}
}

const (
	createPath = "/api/tickets"
	lookupPath = "/api/tickets/lookup"
	cancelPath = "/api/tickets/" + sampleNumber + "/cancel"
	ticketPath = "/api/tickets/" + sampleNumber

	createJSON = `{"items":[{"modelId":"` + sampleModelID + `","qty":2}],"purpose":"Example loan for a lab exercise.","pickupDate":"2026-11-20","durationDays":7}`
	lookupJSON = `{"number":"` + sampleNumber + `","email":"` + sampleBorrowerMail + `"}`
)

func invalidFields(fields ...string) error {
	var details []catalog.FieldError
	for _, f := range fields {
		details = append(details, catalog.FieldError{Field: f, Code: "too_many", Message: "Example message."})
	}

	return &catalog.ValidationError{Details: details}
}

func ticketNumberOf(t *testing.T, body []byte) string {
	t.Helper()

	return decode[struct {
		Number string `json:"number"`
	}](t, body).Number
}

// ticketShape checks that an answer has every field of the schema Ticket and no field of the admin view.
func ticketShape(t *testing.T, body []byte) {
	t.Helper()

	got := decode[map[string]json.RawMessage](t, body)

	for _, key := range []string{"number", "status", "effectiveStatus", "version", "createdAt", "requester", "purpose", "pickupDate", "durationDays",
		"approvedDurationDays", "dueDate", "items", "decisionNote", "history", "anonymizedAt"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("the ticket has no %s, %s", key, body)
		}
	}

	for _, key := range []string{"id", "units"} {
		if _, ok := got[key]; ok {
			t.Fatalf("the ticket of a borrower has the field %s, %s", key, body)
		}
	}
}

func ticketCases() []ticketCase {
	var cases []ticketCase

	add := func(more ...ticketCase) { cases = append(cases, more...) }

	// the common cases of a route of the guest, which are no session, a bug and an outage
	common := func(op, method, path, body string, as string) []ticketCase {
		return []ticketCase{
			{op: op, name: "no session", as: "none", method: method, path: path, body: body, status: 401, code: "unauthenticated"},
			{op: op, name: "a bug", as: as, method: method, path: path, body: body, arrange: ticketFail(errors.New("example failure")), status: 500, code: "internal"},
			{op: op, name: "an outage", as: as, method: method, path: path, body: body, arrange: ticketFail(&outageError{}), status: 503, code: "unavailable"},
			{op: op, name: "an admin", as: "admin", method: method, path: path, body: body, status: 403, code: "forbidden"},
			{op: op, name: "a setup session", as: "setup", method: method, path: path, body: body, status: 403, code: "forbidden"},
		}
	}

	const create = "POST /api/tickets"

	add(common(create, "POST", createPath, createJSON, "guest")...)
	add(
		ticketCase{op: create, name: "a guest creates a ticket", as: "guest", method: "POST", path: createPath, body: createJSON, status: 201,
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, body []byte) {
				ticketCalls(t, e, "Create")
				ticketShape(t, body)

				req := e.tix.create
				if req.Identity.Email != sampleBorrowerMail || req.Identity.NimNip != "000000000000000" || req.Identity.Name != "Example Borrower" ||
					string(req.SessionHash) != string(e.guestS.TokenHash) {
					t.Fatalf("the identity or the session is %+v", req)
				}

				if len(req.Items) != 1 || req.Items[0].ModelID != sampleModelID || req.Items[0].Qty == nil || *req.Items[0].Qty != 2 ||
					req.Purpose != "Example loan for a lab exercise." || req.PickupDate != "2026-11-20" || req.DurationDays == nil || *req.DurationDays != 7 {
					t.Fatalf("the request is %+v", req)
				}

				// the counter of the address counts the request, once, before the work
				keys := e.lim.hitsWith("ticket-create:")
				if len(keys) != 1 || keys[0] != "ticket-create:0.0.0.0" || e.lim.limits[keys[0]] != 20 || e.lim.windows[keys[0]] != time.Hour {
					t.Fatalf("the counters were hit with %v, %v, %v", e.lim.hits, e.lim.limits, e.lim.windows)
				}

				if e.tix.rec != nil || len(e.keys.keys) != 0 {
					t.Fatal("a request with no key claimed a key")
				}
			}},
		ticketCase{op: create, name: "a key claims the key of its session and route", as: "guest", method: "POST", path: createPath, body: createJSON,
			extra: idemHeaders(), status: 201,
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) {
				if len(e.keys.keys) != 1 {
					t.Fatalf("%d claims", len(e.keys.keys))
				}

				key := e.keys.keys[0]
				if string(key.Session) != string(e.guestS.TokenHash) || key.Operation != "POST /api/tickets" || key.Value != idemValue {
					t.Fatalf("the claim is %+v", key)
				}

				if e.tix.rec == nil || len(e.keys.claim.recorded) != 1 || e.keys.claim.recorded[0] != 201 || e.keys.claim.released != 0 {
					t.Fatalf("recorded %v, released %d", e.keys.claim.recorded, e.keys.claim.released)
				}
			}},
		ticketCase{op: create, name: "a repeat gets the stored answer and the service does not run", as: "guest", method: "POST", path: createPath, body: createJSON,
			extra: idemHeaders(), status: 201,
			arrange: func(e *ticketEnv) {
				e.keys.answer = &idempotency.Answer{Status: http.StatusCreated, Body: []byte(`{"number":"` + sampleNumber + `"}`)}
			},
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, body []byte) {
				ticketCalls(t, e)

				if ticketNumberOf(t, body) != sampleNumber {
					t.Fatalf("the body is %s", body)
				}
			}},
		ticketCase{op: create, name: "the hidden field makes an answer and creates nothing", as: "guest", method: "POST", path: createPath,
			body:  strings.Replace(createJSON, `{"items"`, `{"website":"https://spam.example.org","items"`, 1),
			extra: idemHeaders(), status: 201,
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, body []byte) {
				ticketCalls(t, e, "Preview")
				ticketShape(t, body)

				if len(e.keys.keys) != 0 || e.tix.rec != nil {
					t.Fatal("the trap claimed a key")
				}
			}},
		ticketCase{op: create, name: "the hidden field of the wrong type counts as filled", as: "guest", method: "POST", path: createPath,
			body:   strings.Replace(createJSON, `{"items"`, `{"website":5,"items"`, 1),
			status: 201,
			check:  func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e, "Preview") }},
		ticketCase{op: create, name: "the trap shows the refusals of a real request", as: "guest", method: "POST", path: createPath,
			body:    strings.Replace(createJSON, `{"items"`, `{"website":"x","items"`, 1),
			arrange: ticketFail(invalidFields("items[0].qty")), status: 422, code: "validation_failed"},
		ticketCase{op: create, name: "a body that is not JSON", as: "guest", method: "POST", path: createPath, body: "{", status: 400, code: "bad_request",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: create, name: "a quantity of the wrong type", as: "guest", method: "POST", path: createPath,
			body: strings.Replace(createJSON, `"qty":2`, `"qty":"two"`, 1), status: 400, code: "bad_request"},
		ticketCase{op: create, name: "a guest with no identity", as: "noid", method: "POST", path: createPath, body: createJSON, status: 403, code: "identity_required",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) {
				ticketCalls(t, e)

				if len(e.lim.hits) != 0 {
					t.Fatalf("a request that the guard refused was counted %v", e.lim.hits)
				}
			}},
		ticketCase{op: create, name: "a wrong CSRF token", as: "guest", method: "POST", path: createPath, body: createJSON,
			extra: map[string]string{"X-CSRF-Token": "wrong"}, status: 403, code: "csrf_failed",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: create, name: "another origin", as: "guest", method: "POST", path: createPath, body: createJSON,
			extra: map[string]string{"Origin": "https://evil.example.org"}, status: 403, code: "origin_not_allowed",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: create, name: "a repeat while the first runs", as: "guest", method: "POST", path: createPath, body: createJSON, extra: idemHeaders(),
			arrange: func(e *ticketEnv) { e.keys.err = idempotency.ErrInProgress }, status: 409, code: "request_in_progress",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: create, name: "a claim that was taken over", as: "guest", method: "POST", path: createPath, body: createJSON, extra: idemHeaders(),
			arrange: ticketFail(idempotency.ErrLost), status: 409, code: "request_in_progress",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) {
				if e.keys.claim.released != 1 || len(e.keys.claim.recorded) != 0 {
					t.Fatalf("recorded %v, released %d", e.keys.claim.recorded, e.keys.claim.released)
				}
			}},
		ticketCase{op: create, name: "fields that fail a rule", as: "guest", method: "POST", path: createPath, body: createJSON,
			arrange: ticketFail(invalidFields("items[0].qty", "requester")), status: 422, code: "validation_failed",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, body []byte) {
				d := decodeError(t, body).Error.Details
				if len(d) != 2 || d[0].Field != "items[0].qty" || d[1].Field != "requester" || e.keys.claim.released != 0 {
					t.Fatalf("the details are %+v", d)
				}
			}},
		ticketCase{op: create, name: "a key of the wrong shape", as: "guest", method: "POST", path: createPath, body: createJSON,
			extra: map[string]string{"Idempotency-Key": "short"}, status: 422, code: "validation_failed",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: create, name: "the limit of the address is over", as: "guest", method: "POST", path: createPath, body: createJSON,
			arrange: func(e *ticketEnv) { e.lim.overWhere("ticket-create:") }, status: 429, code: "rate_limited",
			check: func(t *testing.T, e *ticketEnv, res *http.Response, _ []byte) {
				ticketCalls(t, e)

				if res.Header.Get("Retry-After") != "90" {
					t.Fatalf("Retry-After is %q", res.Header.Get("Retry-After"))
				}
			}},
		ticketCase{op: create, name: "a failure of the counter", as: "guest", method: "POST", path: createPath, body: createJSON,
			arrange: func(e *ticketEnv) { e.lim.err = &outageError{} }, status: 503, code: "unavailable"},
	)

	const list = "GET /api/tickets"

	add(common(list, "GET", "/api/tickets", "", "guest")...)
	add(
		ticketCase{op: list, name: "a guest reads its list", as: "guest", method: "GET", path: "/api/tickets?limit=5&cursor=abc", status: 200,
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, body []byte) {
				ticketCalls(t, e, "List")

				page := decode[struct {
					Items      []json.RawMessage `json:"items"`
					Total      *int              `json:"total"`
					NextCursor string            `json:"nextCursor"`
				}](t, body)
				if len(page.Items) != 1 || page.Total == nil || page.NextCursor != "next" {
					t.Fatalf("the page is %s", body)
				}

				ticketShape(t, page.Items[0])

				if !slices.Equal(e.tix.viewable, []string{sampleTicketID}) || e.tix.page != (catalog.PageRequest{Limit: 5, Cursor: "abc"}) {
					t.Fatalf("the service got %v and %+v", e.tix.viewable, e.tix.page)
				}
			}},
		ticketCase{op: list, name: "a guest with no identity reads its list too", as: "noid", method: "GET", path: "/api/tickets", status: 200,
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) {
				ticketCalls(t, e, "List")

				if e.tix.page.Limit != 50 || len(e.tix.viewable) != 0 {
					t.Fatalf("the service got %v and %+v", e.tix.viewable, e.tix.page)
				}
			}},
		ticketCase{op: list, name: "a limit of 0", as: "guest", method: "GET", path: "/api/tickets?limit=0", status: 422, code: "validation_failed",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: list, name: "a cursor that the service refuses", as: "guest", method: "GET", path: "/api/tickets?cursor=forged",
			arrange: ticketFail(pagination.ErrCursor), status: 422, code: "validation_failed",
			check: func(t *testing.T, _ *ticketEnv, _ *http.Response, body []byte) {
				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Field != "cursor" {
					t.Fatalf("the details are %+v", d)
				}
			}},
	)

	const lookup = "POST /api/tickets/lookup"

	add(
		ticketCase{op: lookup, name: "no session starts a guest session without an identity", as: "none", method: "POST", path: lookupPath, body: lookupJSON, status: 200,
			check: func(t *testing.T, e *ticketEnv, res *http.Response, body []byte) {
				ticketCalls(t, e, "Lookup")

				if e.tix.number != sampleNumber || e.tix.email != sampleBorrowerMail {
					t.Fatalf("the service got %q and %q", e.tix.number, e.tix.email)
				}

				got := decode[struct {
					CSRFToken string          `json:"csrfToken"`
					Ticket    json.RawMessage `json:"ticket"`
				}](t, body)
				ticketShape(t, got.Ticket)

				cookie := sessionCookieOf(t, res)

				started, err := e.store.Load(t.Context(), cookie.Value)
				if err != nil {
					t.Fatalf("the cookie matches no session %v", err)
				}

				if started.Kind != session.KindGuest || started.Identity != nil || started.CSRFToken != got.CSRFToken || got.CSRFToken == "" {
					t.Fatalf("the session is %+v and the token %q", started, got.CSRFToken)
				}

				if !slices.Equal(started.ViewableTickets, []string{sampleTicketID}) {
					t.Fatalf("the new session may read %v", started.ViewableTickets)
				}

				if !cookie.HttpOnly || !cookie.Secure {
					t.Fatalf("the cookie is %+v", cookie)
				}

				// a found ticket gives its hits back
				if len(e.lim.hitsWith("ticket-lookup-")) != 2 || len(e.lim.refunds) != 2 {
					t.Fatalf("hits %v and refunds %v", e.lim.hits, e.lim.refunds)
				}
			}},
		ticketCase{op: lookup, name: "a lower case number and an email in capitals are normalized", as: "none", method: "POST", path: lookupPath,
			body:   `{"number":" mgm-2611-0001-k7qx ","email":" Example@Student.UB.ac.id "}`,
			status: 200,
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) {
				if e.tix.number != sampleNumber || e.tix.email != sampleBorrowerMail {
					t.Fatalf("the service got %q and %q", e.tix.number, e.tix.email)
				}
			}},
		ticketCase{op: lookup, name: "a guest session keeps its identity and gains the ticket", as: "guest", method: "POST", path: lookupPath, body: lookupJSON, status: 200,
			arrange: func(e *ticketEnv) { e.store.setViewable(e.guest.token, otherTicketID) },
			check: func(t *testing.T, e *ticketEnv, res *http.Response, body []byte) {
				if len(res.Cookies()) != 0 || e.store.count() != 4 {
					t.Fatalf("a session was made or ended, %d sessions, cookies %v", e.store.count(), res.Cookies())
				}

				if got := e.store.viewableOf(e.guest.token); !slices.Equal(got, []string{otherTicketID, sampleTicketID}) {
					t.Fatalf("the session may read %v", got)
				}

				if decode[struct {
					CSRFToken string `json:"csrfToken"`
				}](t, body).CSRFToken != e.guestS.CSRFToken {
					t.Fatalf("the token of the answer is not the one of the session, %s", body)
				}
			}},
		ticketCase{op: lookup, name: "a lookup of a guest needs no CSRF token", as: "guest", method: "POST", path: lookupPath, body: lookupJSON,
			extra: map[string]string{"X-CSRF-Token": ""}, status: 200},
		ticketCase{op: lookup, name: "an admin session changes in no way", as: "admin", method: "POST", path: lookupPath, body: lookupJSON, status: 200,
			check: func(t *testing.T, e *ticketEnv, res *http.Response, body []byte) {
				if len(res.Cookies()) != 0 || e.store.count() != 4 || len(e.store.viewableOf(e.admin.token)) != 0 {
					t.Fatalf("the admin session changed, %d sessions", e.store.count())
				}

				if decode[struct {
					CSRFToken string `json:"csrfToken"`
				}](t, body).CSRFToken == "" {
					t.Fatalf("the answer has no token, %s", body)
				}
			}},
		ticketCase{op: lookup, name: "a setup session changes in no way", as: "setup", method: "POST", path: lookupPath, body: lookupJSON, status: 200,
			check: func(t *testing.T, e *ticketEnv, res *http.Response, _ []byte) {
				if len(res.Cookies()) != 0 || e.store.count() != 4 || len(e.store.viewableOf(e.setup.token)) != 0 {
					t.Fatalf("the setup session changed, %d sessions", e.store.count())
				}
			}},
		ticketCase{op: lookup, name: "a body that is not JSON", as: "none", method: "POST", path: lookupPath, body: "{", status: 400, code: "bad_request",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: lookup, name: "another origin", as: "none", method: "POST", path: lookupPath, body: lookupJSON,
			extra: map[string]string{"Origin": "https://evil.example.org"}, status: 403, code: "origin_not_allowed",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: lookup, name: "a wrong number or email makes no session and keeps its hits", as: "none", method: "POST", path: lookupPath, body: lookupJSON,
			arrange: ticketFail(ticket.ErrNoMatch), status: 404, code: "ticket_not_found",
			check: func(t *testing.T, e *ticketEnv, res *http.Response, _ []byte) {
				if len(res.Cookies()) != 0 || e.store.count() != 4 || len(e.lim.refunds) != 0 || len(e.lim.hitsWith("ticket-lookup-")) != 2 {
					t.Fatalf("%d sessions, cookies %v, hits %v, refunds %v", e.store.count(), res.Cookies(), e.lim.hits, e.lim.refunds)
				}
			}},
		ticketCase{op: lookup, name: "a number that is no number", as: "none", method: "POST", path: lookupPath,
			body: `{"number":"MGM-2611-0001","email":"` + sampleBorrowerMail + `"}`, status: 422, code: "validation_failed",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, body []byte) {
				ticketCalls(t, e)

				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Field != "number" || d[0].Code != "invalid_format" || len(e.lim.hits) != 0 {
					t.Fatalf("the details are %+v and the hits %v", d, e.lim.hits)
				}
			}},
		ticketCase{op: lookup, name: "an email of another school and a missing number", as: "none", method: "POST", path: lookupPath,
			body: `{"email":"someone@example.org"}`, status: 422, code: "validation_failed",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, body []byte) {
				d := decodeError(t, body).Error.Details
				if len(d) != 2 || d[0].Field != "number" || d[0].Code != "required" || d[1].Field != "email" || d[1].Code != "not_ub_email" {
					t.Fatalf("the details are %+v", d)
				}
			}},
		ticketCase{op: lookup, name: "the counter of the address is over", as: "none", method: "POST", path: lookupPath, body: lookupJSON,
			arrange: func(e *ticketEnv) { e.lim.overWhere("ticket-lookup-address:") }, status: 429, code: "rate_limited",
			check: func(t *testing.T, e *ticketEnv, res *http.Response, _ []byte) {
				ticketCalls(t, e)

				if res.Header.Get("Retry-After") != "90" || len(e.lim.refunds) != 1 {
					t.Fatalf("Retry-After is %q, refunds %v", res.Header.Get("Retry-After"), e.lim.refunds)
				}
			}},
		ticketCase{op: lookup, name: "the counter of the number and the email is over", as: "none", method: "POST", path: lookupPath, body: lookupJSON,
			arrange: func(e *ticketEnv) { e.lim.overWhere("ticket-lookup-number:") }, status: 429, code: "rate_limited",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) {
				ticketCalls(t, e)

				// the first counter had counted, and the refund took its hit back
				if len(e.lim.hits) != 2 || len(e.lim.refunds) != 2 {
					t.Fatalf("hits %v, refunds %v", e.lim.hits, e.lim.refunds)
				}
			}},
		ticketCase{op: lookup, name: "a bug gives the hits back", as: "none", method: "POST", path: lookupPath, body: lookupJSON,
			arrange: ticketFail(errors.New("example failure")), status: 500, code: "internal",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) {
				if len(e.lim.refunds) != 2 || e.store.count() != 4 {
					t.Fatalf("refunds %v, %d sessions", e.lim.refunds, e.store.count())
				}
			}},
		ticketCase{op: lookup, name: "an outage of the database", as: "none", method: "POST", path: lookupPath, body: lookupJSON,
			arrange: ticketFail(&outageError{}), status: 503, code: "unavailable"},
		ticketCase{op: lookup, name: "an outage while the session loads", as: "guest", method: "POST", path: lookupPath, body: lookupJSON,
			arrange: func(e *ticketEnv) { e.store.loadErr = &outageError{} }, status: 503, code: "unavailable",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: lookup, name: "an outage of the counter", as: "none", method: "POST", path: lookupPath, body: lookupJSON,
			arrange: func(e *ticketEnv) { e.lim.err = &outageError{} }, status: 503, code: "unavailable"},
	)

	const get = "GET /api/tickets/{number}"

	add(common(get, "GET", ticketPath, "", "guest")...)
	add(
		ticketCase{op: get, name: "a guest reads a ticket of its session", as: "guest", method: "GET", path: ticketPath, status: 200,
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, body []byte) {
				ticketCalls(t, e, "Get")
				ticketShape(t, body)

				if e.tix.number != sampleNumber || !slices.Equal(e.tix.viewable, []string{sampleTicketID}) {
					t.Fatalf("the service got %q and %v", e.tix.number, e.tix.viewable)
				}
			}},
		ticketCase{op: get, name: "a ticket that the session did not create or find", as: "guest", method: "GET", path: ticketPath, arrange: ticketFail(ticket.ErrNotFound),
			status: 404, code: "not_found"},
	)

	const cancel = "POST /api/tickets/{number}/cancel"

	add(common(cancel, "POST", cancelPath, "", "guest")...)
	add(
		ticketCase{op: cancel, name: "a guest cancels", as: "guest", method: "POST", path: cancelPath, status: 200,
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, body []byte) {
				ticketCalls(t, e, "CancelByGuest")
				ticketShape(t, body)

				got := decode[struct{ Status, EffectiveStatus string }](t, body)
				if got.Status != "dibatalkan" || got.EffectiveStatus != "dibatalkan" || e.tix.number != sampleNumber || !slices.Equal(e.tix.viewable, []string{sampleTicketID}) {
					t.Fatalf("the answer is %s and the service got %q and %v", body, e.tix.number, e.tix.viewable)
				}
			}},
		ticketCase{op: cancel, name: "a guest with no identity cancels a ticket that it found", as: "noid", method: "POST", path: cancelPath, status: 200,
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e, "CancelByGuest") }},
		ticketCase{op: cancel, name: "a wrong CSRF token", as: "guest", method: "POST", path: cancelPath, extra: map[string]string{"X-CSRF-Token": "wrong"},
			status: 403, code: "csrf_failed",
			check: func(t *testing.T, e *ticketEnv, _ *http.Response, _ []byte) { ticketCalls(t, e) }},
		ticketCase{op: cancel, name: "a ticket that the session did not create or find", as: "guest", method: "POST", path: cancelPath, arrange: ticketFail(ticket.ErrNotFound),
			status: 404, code: "not_found"},
		ticketCase{op: cancel, name: "a status that does not allow it", as: "guest", method: "POST", path: cancelPath,
			arrange: ticketFail(&catalog.RuleError{Kind: catalog.RuleStatusConflict, Message: "Only a ticket in the status diajukan can be cancelled."}),
			status:  409, code: "status_conflict"},
	)

	return cases
}

func TestEveryTicketRouteAnswersEveryStatusOfTheContract(t *testing.T) {
	for _, tc := range ticketCases() {
		t.Run(tc.op+" "+tc.name, func(t *testing.T) {
			e := newTicketEnv(t)

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

// ticketContractCodes lists the status codes that openapi.yaml names for each ticket operation of the borrower. A script made the
// list from the paths and the response codes of the operations createTicket, listTickets, lookupTicket, getTicket and cancelTicket,
// and it must be made again when the contract changes.
var ticketContractCodes = map[string][]int{
	"POST /api/tickets":                 {201, 400, 401, 403, 409, 422, 429, 500, 503},
	"GET /api/tickets":                  {200, 401, 403, 422, 500, 503},
	"POST /api/tickets/lookup":          {200, 400, 403, 404, 422, 429, 500, 503},
	"GET /api/tickets/{number}":         {200, 401, 403, 404, 500, 503},
	"POST /api/tickets/{number}/cancel": {200, 401, 403, 404, 409, 500, 503},
}

func TestEveryStatusOfTheTicketContractHasACase(t *testing.T) {
	have := map[string]map[int]bool{}

	for _, tc := range ticketCases() {
		if have[tc.op] == nil {
			have[tc.op] = map[int]bool{}
		}

		have[tc.op][tc.status] = true
	}

	for op, codes := range ticketContractCodes {
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

	if len(have) != len(ticketContractCodes) {
		t.Fatalf("the cases cover %d operations and the contract list has %d", len(have), len(ticketContractCodes))
	}
}

func TestTheTicketRoutesAreTheOnesOfTheContract(t *testing.T) {
	app := NewAPI(testConfig(), Deps{Sessions: newFakeStore(), Limiter: newTicketLimiter()})

	have := map[string]bool{}

	for _, route := range app.GetRoutes() {
		have[route.Method+" "+route.Path] = true
	}

	for op := range ticketContractCodes {
		method, path, _ := strings.Cut(op, " ")
		path = strings.NewReplacer("{number}", ":number").Replace(path)

		if !have[method+" "+path] {
			t.Errorf("the route %s %s is not served", method, path)
		}
	}
}

func TestTheLookupCountersHoldNeitherTheSecretNorTheEmail(t *testing.T) {
	e := newTicketEnv(t)

	res, body := e.call(client{}, "POST", lookupPath, lookupJSON, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the lookup answers %d, %s", res.StatusCode, body)
	}

	sum := sha256.Sum256([]byte(sampleBorrowerMail))
	want := []string{"ticket-lookup-address:0.0.0.0", "ticket-lookup-number:2611-0001:" + hex.EncodeToString(sum[:])}

	if !slices.Equal(e.lim.hits, want) {
		t.Fatalf("the counters are %v, want %v", e.lim.hits, want)
	}

	for key, limit := range e.lim.limits {
		if limit != map[string]int{want[0]: 20, want[1]: 10}[key] || e.lim.windows[key] != time.Hour {
			t.Fatalf("the counter %s has the limit %d and the window %v", key, limit, e.lim.windows[key])
		}
	}

	for _, key := range e.lim.hits {
		if strings.Contains(key, "K7QX") || strings.Contains(strings.ToLower(key), "student.ub.ac.id") {
			t.Fatalf("the counter %s holds the secret or the email", key)
		}
	}
}
