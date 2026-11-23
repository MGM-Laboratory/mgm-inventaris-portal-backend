package ticket

import "context"

// AdminHistoryEntry is one change of status as an admin sees it. ActorName is the name of the admin, and a guest entry has none.
type AdminHistoryEntry struct {
	At        string  `json:"at"`
	Status    string  `json:"status"`
	ActorKind string  `json:"actorKind"`
	ActorName *string `json:"actorName,omitempty"`
	Note      *string `json:"note"`
}

// TicketUnit is a unit that a ticket really holds, or held until the return.
type TicketUnit struct {
	Tag           string  `json:"tag"`
	HandedOverAt  string  `json:"handedOverAt"`
	ReturnedAt    *string `json:"returnedAt"`
	ReturnOutcome *string `json:"returnOutcome"`
	ReturnNote    *string `json:"returnNote"`
}

// AdminSummary is a ticket as the queue of an admin shows it. Its history names the admin of each change. The embedded history of the
// borrower view is hidden by the field of the same JSON name, so the answer holds one history.
type AdminSummary struct {
	Ticket
	History []AdminHistoryEntry `json:"history"`
}

// AdminTicket is a ticket as an admin reads it. It adds the units that the ticket holds.
type AdminTicket struct {
	AdminSummary
	Units []TicketUnit `json:"units"`
}

// Event is one status change of a ticket in the activity feed of the admins. ActorName is the name of the admin for an admin action
// and the name of the requester for a guest action, and it is nil after the retention job anonymized the ticket.
type Event struct {
	ID            string  `json:"id"`
	At            string  `json:"at"`
	TicketNumber  string  `json:"ticketNumber"`
	RequesterName *string `json:"requesterName"`
	Status        string  `json:"status"`
	ActorKind     string  `json:"actorKind"`
	ActorName     *string `json:"actorName"`
	Note          *string `json:"note"`
}

// AdminFilter narrows the queue of an admin. Statuses holds effective statuses. The dates are YYYY-MM-DD in Asia/Jakarta, and an empty
// date is no bound.
type AdminFilter struct {
	Query      string
	Statuses   []string
	PickupFrom string
	PickupTo   string
	DueFrom    string
	DueTo      string
}

// ApprovedInput is the approved quantity of one model of the ticket. A nil ApprovedQty is a missing field.
type ApprovedInput struct {
	ModelID     string
	ApprovedQty *int
}

// The two decisions of the field decision.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// DecisionRequest is the body of a decision. A nil Version is a missing field. Items, DurationDays and Note belong to an approval, and
// Note alone belongs to a rejection, where it is required.
type DecisionRequest struct {
	Version      *int
	Decision     string
	Items        []ApprovedInput
	DurationDays *int
	Note         *string
}

// StepRequest is the body of a step that carries only the version, which is the notice that a ticket is ready.
type StepRequest struct {
	Version *int
}

// HandoverRequest is the body of a handover, with the tags of the units that the admin took from the shelf.
type HandoverRequest struct {
	Version *int
	Tags    []string
}

// ReturnLine is one unit of a return with the outcome that the admin found.
type ReturnLine struct {
	Tag     string
	Outcome string
	Note    *string
}

// ReturnRequest is the body of a return. It names every unit that the ticket holds.
type ReturnRequest struct {
	Version *int
	Units   []ReturnLine
}

// AdminCancelRequest is the body of the cancel of an approved ticket by an admin. The note is required.
type AdminCancelRequest struct {
	Version *int
	Note    *string
}

// NoticeKind says what happened to a ticket.
type NoticeKind string

// The changes that a borrower may want to hear about.
const (
	NoticeApproved  NoticeKind = "approved"
	NoticeRejected  NoticeKind = "rejected"
	NoticeReady     NoticeKind = "ready"
	NoticeCancelled NoticeKind = "cancelled"
)

// Notice is what a mail about a change needs. It holds the address of the borrower, so an implementation must never write it to a log.
type Notice struct {
	Kind   NoticeKind
	Number string
	Name   *string
	Email  *string
	Note   *string
}

// Notifier tells the borrower of a ticket about a change. The store calls it after the change is committed, and it ignores the error,
// so a mail that fails never undoes a decision. No provider exists yet, so the service wires an implementation that only logs.
type Notifier interface {
	Notify(ctx context.Context, notice Notice) error
}
