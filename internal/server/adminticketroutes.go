package server

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/dashboard"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ticket"
)

// AdminTicketService is the part of the ticket package that the routes of an admin use. The id of the admin goes into the history row of
// each step.
type AdminTicketService interface {
	ListAdmin(ctx context.Context, f ticket.AdminFilter, req catalog.PageRequest) (pagination.Page[ticket.AdminSummary], error)
	GetAdmin(ctx context.Context, number string) (ticket.AdminTicket, error)
	Decide(ctx context.Context, adminID, number string, req ticket.DecisionRequest) (ticket.AdminTicket, error)
	MarkReady(ctx context.Context, adminID, number string, req ticket.StepRequest) (ticket.AdminTicket, error)
	HandOver(ctx context.Context, adminID, number string, req ticket.HandoverRequest) (ticket.AdminTicket, error)
	Return(ctx context.Context, adminID, number string, req ticket.ReturnRequest) (ticket.AdminTicket, error)
	CancelByAdmin(ctx context.Context, adminID, number string, req ticket.AdminCancelRequest) (ticket.AdminTicket, error)
	ListEvents(ctx context.Context, number string, req catalog.PageRequest) (pagination.Page[ticket.Event], error)
}

// DashboardService is the part of the dashboard package that the route uses.
type DashboardService interface {
	Counts(ctx context.Context) (dashboard.Counts, error)
}

func registerAdminTicketRoutes(app *fiber.App, deps Deps) {
	admin := RequireAdmin()

	app.Get("/api/admin/tickets", admin, listAdminTickets(deps))
	app.Get("/api/admin/tickets/:number", admin, getAdminTicket(deps))
	app.Post("/api/admin/tickets/:number/decision", admin, decideTicket(deps))
	app.Post("/api/admin/tickets/:number/ready", admin, markTicketReady(deps))
	app.Post("/api/admin/tickets/:number/handover", admin, handOverTicket(deps))
	app.Post("/api/admin/tickets/:number/return", admin, returnTicket(deps))
	app.Post("/api/admin/tickets/:number/cancel", admin, cancelAdminTicket(deps))
	app.Get("/api/admin/ticket-events", admin, listTicketEvents(deps))
	app.Get("/api/admin/dashboard", admin, getDashboard(deps))
}

// statusFilter reads the list of effective statuses of the query. A request without the parameter has no filter, and a parameter with
// an empty value is one empty status, which the service refuses.
func statusFilter(c fiber.Ctx) []string {
	if _, sent := c.Queries()["status"]; !sent {
		return nil
	}

	return strings.Split(c.Query("status"), ",")
}

// listAdminTickets is the route listAdminTickets.
func listAdminTickets(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		req, fails := readPage(c)
		if err := failIfAny(fails...); err != nil {
			return err
		}

		filter := ticket.AdminFilter{
			Query: c.Query("q"), Statuses: statusFilter(c),
			PickupFrom: c.Query("pickupFrom"), PickupTo: c.Query("pickupTo"), DueFrom: c.Query("dueFrom"), DueTo: c.Query("dueTo"),
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		page, err := deps.AdminTickets.ListAdmin(ctx, filter, req)
		if err != nil {
			return ticketError("list the tickets of the queue", err)
		}

		return c.JSON(page)
	}
}

// getAdminTicket is the route getAdminTicket.
func getAdminTicket(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		found, err := deps.AdminTickets.GetAdmin(ctx, c.Params("number"))
		if err != nil {
			return ticketError("read the ticket", err)
		}

		return c.JSON(found)
	}
}

type approvedForm struct {
	ModelID     string `json:"modelId"`
	ApprovedQty *int   `json:"approvedQty"`
}

// decisionForm is the body of POST /api/admin/tickets/{number}/decision. A field that the contract does not name is ignored.
type decisionForm struct {
	Version      *int           `json:"version"`
	Decision     string         `json:"decision"`
	Items        []approvedForm `json:"items"`
	DurationDays *int           `json:"durationDays"`
	Note         *string        `json:"note"`
}

func (f decisionForm) request() ticket.DecisionRequest {
	items := make([]ticket.ApprovedInput, 0, len(f.Items))
	for _, it := range f.Items {
		items = append(items, ticket.ApprovedInput{ModelID: it.ModelID, ApprovedQty: it.ApprovedQty})
	}

	return ticket.DecisionRequest{Version: f.Version, Decision: f.Decision, Items: items, DurationDays: f.DurationDays, Note: f.Note}
}

// decideTicket is the route decideTicket.
func decideTicket(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form decisionForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		decided, err := deps.AdminTickets.Decide(ctx, adminID(c), c.Params("number"), form.request())
		if err != nil {
			return ticketError("decide on the ticket", err)
		}

		return c.JSON(decided)
	}
}

// versionForm is the body of a step that carries only the version, and the start of the other bodies.
type versionForm struct {
	Version *int `json:"version"`
}

// markTicketReady is the route markTicketReady.
func markTicketReady(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form versionForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		ready, err := deps.AdminTickets.MarkReady(ctx, adminID(c), c.Params("number"), ticket.StepRequest{Version: form.Version})
		if err != nil {
			return ticketError("mark the ticket ready", err)
		}

		return c.JSON(ready)
	}
}

type handoverForm struct {
	Version *int     `json:"version"`
	Tags    []string `json:"tags"`
}

// handOverTicket is the route handOverTicket.
func handOverTicket(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form handoverForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		handed, err := deps.AdminTickets.HandOver(ctx, adminID(c), c.Params("number"), ticket.HandoverRequest{Version: form.Version, Tags: form.Tags})
		if err != nil {
			return ticketError("hand the units over", err)
		}

		return c.JSON(handed)
	}
}

type returnLineForm struct {
	Tag     string  `json:"tag"`
	Outcome string  `json:"outcome"`
	Note    *string `json:"note"`
}

type returnForm struct {
	Version *int             `json:"version"`
	Units   []returnLineForm `json:"units"`
}

func (f returnForm) request() ticket.ReturnRequest {
	lines := make([]ticket.ReturnLine, 0, len(f.Units))
	for _, u := range f.Units {
		lines = append(lines, ticket.ReturnLine{Tag: u.Tag, Outcome: u.Outcome, Note: u.Note})
	}

	return ticket.ReturnRequest{Version: f.Version, Units: lines}
}

// returnTicket is the route returnTicket.
func returnTicket(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form returnForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		back, err := deps.AdminTickets.Return(ctx, adminID(c), c.Params("number"), form.request())
		if err != nil {
			return ticketError("take the units back", err)
		}

		return c.JSON(back)
	}
}

type adminCancelForm struct {
	Version *int    `json:"version"`
	Note    *string `json:"note"`
}

// cancelAdminTicket is the route cancelAdminTicket.
func cancelAdminTicket(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form adminCancelForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		cancelled, err := deps.AdminTickets.CancelByAdmin(ctx, adminID(c), c.Params("number"), ticket.AdminCancelRequest{Version: form.Version, Note: form.Note})
		if err != nil {
			return ticketError("cancel the ticket", err)
		}

		return c.JSON(cancelled)
	}
}

// listTicketEvents is the route listTicketEvents.
func listTicketEvents(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		req, fails := readPage(c)
		if err := failIfAny(fails...); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		page, err := deps.AdminTickets.ListEvents(ctx, c.Query("ticketNumber"), req)
		if err != nil {
			return ticketError("list the events of the tickets", err)
		}

		return c.JSON(page)
	}
}

// getDashboard is the route getDashboard.
func getDashboard(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		counts, err := deps.Dashboard.Counts(ctx)
		if err != nil {
			return catalogError("read the dashboard", err)
		}

		return c.JSON(counts)
	}
}
