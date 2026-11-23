package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ratelimit"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ticket"
)

const (
	sampleTicketID     = "00000000-0000-4000-8000-0000000000f1"
	sampleNumber       = "MGM-2611-0001-K7QX"
	otherTicketID      = "00000000-0000-4000-8000-0000000000f2"
	sampleBorrowerMail = "example@student.ub.ac.id"
)

var sampleTicket = ticket.Ticket{
	ID: sampleTicketID, Number: sampleNumber, Status: "diajukan", EffectiveStatus: "diajukan", Version: 1, CreatedAt: "2026-11-17T03:00:00Z",
	Requester:  ticket.Requester{Kind: "guest", Name: ptrTo("Example Borrower"), NimNip: ptrTo("000000000000000"), Email: ptrTo(sampleBorrowerMail)},
	Purpose:    ptrTo("Example loan for a lab exercise."),
	PickupDate: "2026-11-20", DurationDays: 7, DueDate: "2026-11-27",
	Items:   []ticket.Item{{ModelID: sampleModelID, ModelName: "Example headset", Qty: 2}},
	History: []ticket.HistoryEntry{{At: "2026-11-17T03:00:00Z", Status: "diajukan", ActorKind: "guest"}},
}

func ptrTo[T any](v T) *T {
	return &v
}

// fakeTickets answers every call with a canned result, or with err when it is set. It records the arguments of the last call.
type fakeTickets struct {
	mu  sync.Mutex
	err error

	calls    []string
	create   ticket.CreateRequest
	rec      catalog.Recorder
	viewable []string
	page     catalog.PageRequest
	number   string
	email    string
	found    ticket.Ticket
}

func (f *fakeTickets) enter(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, name)
}

func (f *fakeTickets) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func (f *fakeTickets) Create(ctx context.Context, req ticket.CreateRequest, rec catalog.Recorder) (ticket.Ticket, error) {
	f.enter("Create")
	f.create, f.rec = req, rec

	if f.err != nil {
		return ticket.Ticket{}, f.err
	}

	if rec != nil {
		if err := rec.Record(ctx, nil, http.StatusCreated, sampleTicket); err != nil {
			return ticket.Ticket{}, err
		}
	}

	return sampleTicket, nil
}

func (f *fakeTickets) Preview(_ context.Context, req ticket.CreateRequest) (ticket.Ticket, error) {
	f.enter("Preview")
	f.create = req

	return sampleTicket, f.err
}

func (f *fakeTickets) List(_ context.Context, viewable []string, req catalog.PageRequest) (pagination.Page[ticket.Ticket], error) {
	f.enter("List")
	f.viewable, f.page = viewable, req

	return pagination.Page[ticket.Ticket]{Items: []ticket.Ticket{sampleTicket}, Total: 1, NextCursor: "next"}, f.err
}

func (f *fakeTickets) Get(_ context.Context, viewable []string, number string) (ticket.Ticket, error) {
	f.enter("Get")
	f.viewable, f.number = viewable, number

	return sampleTicket, f.err
}

func (f *fakeTickets) Lookup(_ context.Context, number, email string) (ticket.Ticket, error) {
	f.enter("Lookup")
	f.number, f.email = number, email

	return sampleTicket, f.err
}

func (f *fakeTickets) CancelByGuest(_ context.Context, viewable []string, number string) (ticket.Ticket, error) {
	f.enter("CancelByGuest")
	f.viewable, f.number = viewable, number

	cancelled := sampleTicket
	cancelled.Status, cancelled.EffectiveStatus = "dibatalkan", "dibatalkan"

	return cancelled, f.err
}

// ticketLimiter records the hits and the refunds. A key in over answers that its limit is over.
type ticketLimiter struct {
	mu      sync.Mutex
	hits    []string
	limits  map[string]int
	windows map[string]time.Duration
	refunds []string
	over    map[string]bool
	err     error
}

func newTicketLimiter() *ticketLimiter {
	return &ticketLimiter{limits: map[string]int{}, windows: map[string]time.Duration{}, over: map[string]bool{}}
}

func (l *ticketLimiter) Hit(_ context.Context, key string, limit int, window time.Duration) (ratelimit.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.hits = append(l.hits, key)
	l.limits[key], l.windows[key] = limit, window

	if l.err != nil {
		return ratelimit.Result{}, l.err
	}

	allowed := true

	for prefix := range l.over {
		if strings.HasPrefix(key, prefix) {
			allowed = false
		}
	}

	return ratelimit.Result{Count: 1, Allowed: allowed, RetryAfter: 90 * time.Second}, nil
}

func (l *ticketLimiter) Refund(_ context.Context, key string, _ time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refunds = append(l.refunds, key)

	return nil
}

// overWhere makes every key that has the prefix answer that its limit is over.
func (l *ticketLimiter) overWhere(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.over[prefix] = true
}

func (l *ticketLimiter) hitsWith(prefix string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []string

	for _, key := range l.hits {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}

	return out
}

// setViewable gives a session the tickets that it may read.
func (f *fakeStore) setViewable(token string, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	hash := string(session.HashToken(token))
	sess := f.sessions[hash]
	sess.ViewableTickets = ids
	f.sessions[hash] = sess
}

func (f *fakeStore) viewableOf(token string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.sessions[string(session.HashToken(token))].ViewableTickets
}

// ticketEnv is an application with the fake tickets, the fake keys and one session of each kind.
type ticketEnv struct {
	t      *testing.T
	app    *fiber.App
	store  *fakeStore
	tix    *fakeTickets
	keys   *fakeIdempotency
	lim    *ticketLimiter
	guest  client
	noID   client
	admin  client
	setup  client
	guestS session.Session
}

func newTicketEnv(t *testing.T) *ticketEnv {
	t.Helper()

	e := &ticketEnv{t: t, store: newFakeStore(), tix: &fakeTickets{}, keys: &fakeIdempotency{claim: &fakeClaim{}}, lim: newTicketLimiter()}
	e.app = NewAPI(testConfig(), Deps{Sessions: e.store, Limiter: e.lim, Tickets: e.tix, Idempotency: e.keys})

	token, sess := e.store.add(session.KindGuest, &session.Identity{Name: "Example Borrower", NimNip: "000000000000000", Email: sampleBorrowerMail})
	e.store.setViewable(token, sampleTicketID)
	e.guest, e.guestS = client{token: token, csrf: sess.CSRFToken}, sess

	token, sess = e.store.add(session.KindGuest, nil)
	e.noID = client{token: token, csrf: sess.CSRFToken}

	token, sess = e.store.add(session.KindAdmin, nil)
	e.admin = client{token: token, csrf: sess.CSRFToken}

	token, sess = e.store.add(session.KindAdminSetup, nil)
	e.setup = client{token: token, csrf: sess.CSRFToken}

	return e
}

func (e *ticketEnv) clientFor(as string) client {
	switch as {
	case "guest":
		return e.guest
	case "noid":
		return e.noID
	case "admin":
		return e.admin
	case "setup":
		return e.setup
	default:
		return client{}
	}
}

func (e *ticketEnv) call(c client, method, path, body string, extra map[string]string) (*http.Response, []byte) {
	e.t.Helper()

	return mustSend(e.t, e.app, method, path, body, c.headers(extra))
}
