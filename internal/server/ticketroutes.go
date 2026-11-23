package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ticket"
)

const (
	// opCreateTicket is the operation of the route that accepts an Idempotency-Key. A key counts for this route only.
	opCreateTicket = "POST /api/tickets"

	ticketCreateWindow = time.Hour
	lookupWindow       = time.Hour

	numberPrefixLength = len("MGM-")
	numberSuffixLength = len("-XXXX")
)

// TicketService is the part of the ticket package that the routes use. A list of ids is the tickets that the session may read.
type TicketService interface {
	Create(ctx context.Context, req ticket.CreateRequest, rec catalog.Recorder) (ticket.Ticket, error)
	Preview(ctx context.Context, req ticket.CreateRequest) (ticket.Ticket, error)
	List(ctx context.Context, viewable []string, req catalog.PageRequest) (pagination.Page[ticket.Ticket], error)
	Get(ctx context.Context, viewable []string, number string) (ticket.Ticket, error)
	Lookup(ctx context.Context, number, email string) (ticket.Ticket, error)
	CancelByGuest(ctx context.Context, viewable []string, number string) (ticket.Ticket, error)
}

func registerTicketRoutes(app *fiber.App, cfg config.Config, deps Deps) {
	app.Post("/api/tickets", RequireIdentity(), createTicket(cfg, deps))
	app.Get("/api/tickets", RequireGuest(), listTickets(deps))
	app.Post("/api/tickets/lookup", lookupTicket(cfg, deps))
	app.Get("/api/tickets/:number", RequireGuest(), getTicket(deps))
	app.Post("/api/tickets/:number/cancel", RequireGuest(), cancelTicket(deps))
}

// ticketError maps the error of the ticket package to the answer of the contract. The errors that the catalog package shares, which
// are the failed fields, the rules of the status and the idempotency errors, go through catalogError.
func ticketError(what string, err error) error {
	switch {
	case errors.Is(err, ticket.ErrNotFound):
		return NewError(CodeNotFound)
	case errors.Is(err, ticket.ErrNoMatch):
		return NewError(CodeTicketNotFound)
	default:
		return catalogError(what, err)
	}
}

type ticketItemForm struct {
	ModelID string `json:"modelId"`
	Qty     *int   `json:"qty"`
}

// ticketForm is the body of POST /api/tickets. A field that the contract does not name is ignored.
type ticketForm struct {
	Items        []ticketItemForm `json:"items"`
	Purpose      string           `json:"purpose"`
	PickupDate   string           `json:"pickupDate"`
	DurationDays *int             `json:"durationDays"`
	Website      textField        `json:"website"`
}

func (f ticketForm) request(sess *session.Session) ticket.CreateRequest {
	items := make([]ticket.ItemInput, 0, len(f.Items))
	for _, it := range f.Items {
		items = append(items, ticket.ItemInput{ModelID: it.ModelID, Qty: it.Qty})
	}

	return ticket.CreateRequest{
		Identity: *sess.Identity, SessionHash: sess.TokenHash, Items: items, Purpose: f.Purpose, PickupDate: f.PickupDate, DurationDays: f.DurationDays,
	}
}

// createTicket is the route createTicket. The guard has found a guest session with an identity. The rate limit counts every request,
// also one with a bad body, and it runs before the body is read. The fields are checked before the trap, so a bot that fills the
// hidden field sees the same refusals as everybody else. A request that fills it gets the answer of a real creation, which the store
// rolls back, and it claims no Idempotency-Key.
func createTicket(cfg config.Config, deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		hit, err := deps.Limiter.Hit(ctx, "ticket-create:"+clientAddr(c, cfg.TrustedProxies), cfg.TicketCreatePerHour, ticketCreateWindow)
		if err != nil {
			return dbError("count the ticket request", err)
		}

		if !hit.Allowed {
			e := NewError(CodeRateLimited)
			e.RetryAfter = hit.RetryAfterSeconds()

			return e
		}

		var form ticketForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		req := form.request(currentSession(c))

		if form.Website.filled() {
			preview, err := deps.Tickets.Preview(ctx, req)
			if err != nil {
				return ticketError("check the ticket request", err)
			}

			return c.Status(http.StatusCreated).JSON(preview)
		}

		return createWithKey(c, deps, opCreateTicket, "create the ticket", func(ctx context.Context, rec catalog.Recorder) (any, error) {
			return deps.Tickets.Create(ctx, req, rec)
		})
	}
}

// listTickets is the route listTickets.
func listTickets(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		req, fails := readPage(c)
		if err := failIfAny(fails...); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		page, err := deps.Tickets.List(ctx, currentSession(c).ViewableTickets, req)
		if err != nil {
			return ticketError("list the tickets", err)
		}

		return c.JSON(page)
	}
}

// getTicket is the route getTicket.
func getTicket(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		found, err := deps.Tickets.Get(ctx, currentSession(c).ViewableTickets, c.Params("number"))
		if err != nil {
			return ticketError("read the ticket", err)
		}

		return c.JSON(found)
	}
}

// cancelTicket is the route cancelTicket.
func cancelTicket(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		cancelled, err := deps.Tickets.CancelByGuest(ctx, currentSession(c).ViewableTickets, c.Params("number"))
		if err != nil {
			return ticketError("cancel the ticket", err)
		}

		return c.JSON(cancelled)
	}
}

// lookupForm is the body of POST /api/tickets/lookup.
type lookupForm struct {
	Number textField `json:"number"`
	Email  textField `json:"email"`
}

// lookupBody is the JSON of the schema LookupResult.
type lookupBody struct {
	CSRFToken string        `json:"csrfToken"`
	Ticket    ticket.Ticket `json:"ticket"`
}

// checkTicketNumber reads a ticket number. The letters may come in any case, because a person types them.
func checkTicketNumber(f textField) (string, *Detail) {
	number := strings.ToUpper(strings.TrimSpace(f.value))
	if d, bad := missing("number", f, number); bad {
		return "", d
	}

	if !ticket.ValidNumber(number) {
		return "", detail("number", detailInvalidFormat, "Use a number such as MGM-2611-0001-K7QX.")
	}

	return number, nil
}

// lookupLimits are the two counters of a lookup. The key of the second one holds the number without its last four characters, which
// act as the secret, together with a hash of the email. The counters hold failures only.
func lookupLimits(cfg config.Config, c fiber.Ctx, number, email string) []limitSpec {
	sum := sha256.Sum256([]byte(email))
	known := number[numberPrefixLength : len(number)-numberSuffixLength]

	return []limitSpec{
		{key: "ticket-lookup-address:" + clientAddr(c, cfg.TrustedProxies), limit: cfg.TicketLookupAddressFailures, window: lookupWindow},
		{key: "ticket-lookup-number:" + known + ":" + hex.EncodeToString(sum[:]), limit: cfg.TicketLookupNumberFailures, window: lookupWindow},
	}
}

// lookupTicket is the route lookupTicket. Anyone may call it, and it carries no CSRF token. The request hits its counters first and
// gives the hits back when the ticket is found or the database fails, so parallel guesses cannot all pass before the first failure is
// counted. A wrong number and a wrong email are one answer, and a failed lookup makes no session.
func lookupTicket(cfg config.Config, deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		if state := stateOf(c); state.loadErr != nil {
			return dbError("load the session", state.loadErr)
		}

		var form lookupForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		number, numberDetail := checkTicketNumber(form.Number)
		email, emailDetail := checkEmail(form.Email)

		if err := failIfAny(numberDetail, emailDetail); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		held, err := reserve(ctx, deps.Limiter, lookupLimits(cfg, c, number, email)...)
		if err != nil {
			return err
		}

		found, err := deps.Tickets.Lookup(ctx, number, email)
		if err != nil {
			if !errors.Is(err, ticket.ErrNoMatch) {
				held.refund(ctx)
			}

			return ticketError("look the ticket up", err)
		}

		held.refund(ctx)

		csrf, err := showTicketToSession(ctx, c, cfg, deps, found.ID)
		if err != nil {
			return err
		}

		return c.JSON(lookupBody{CSRFToken: csrf, Ticket: found})
	}
}

// showTicketToSession lets the session of the request read the ticket and returns the CSRF token of the session that the request now
// has. A request without a session starts a guest session with no identity and sets its cookie. A guest session keeps its identity
// and gains the ticket. An admin session, also a setup session, stays as it is.
func showTicketToSession(ctx context.Context, c fiber.Ctx, cfg config.Config, deps Deps, ticketID string) (string, error) {
	current := currentSession(c)

	switch {
	case current == nil:
		token, created, err := deps.Sessions.CreateGuest(ctx, nil, nil)
		if err != nil {
			return "", dbError("create the guest session", err)
		}

		if err := deps.Sessions.AddViewableTicket(ctx, created.TokenHash, ticketID); err != nil {
			return "", dbError("add the ticket to the session", err)
		}

		limits, _ := session.LimitsFor(session.KindGuest)
		setSessionCookie(c, cfg.SecureCookie, token, limits.Absolute)

		return created.CSRFToken, nil
	case current.Kind == session.KindGuest:
		if err := deps.Sessions.AddViewableTicket(ctx, current.TokenHash, ticketID); err != nil {
			return "", dbError("add the ticket to the session", err)
		}

		return current.CSRFToken, nil
	default:
		return current.CSRFToken, nil
	}
}
