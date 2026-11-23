// Package ticket owns the tables of the loan tickets, which are tickets, ticket items, ticket units, the history of a ticket and the
// counter of the numbers. A route handler calls its functions and never builds SQL. The package reads the stock of a model through the
// catalog package, and it does not touch the tables of the assets.
package ticket

import (
	"errors"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

// The detail codes of the contract that this package adds to the ones of the catalog package.
const (
	DetailTooMany          = "too_many"
	DetailModelRetired     = "model_retired"
	DetailTooManyItems     = "too_many_items"
	DetailTooManyOpen      = "too_many_open_tickets"
	detailRequired         = "required"
	detailInvalidFormat    = "invalid_format"
	detailOutOfRange       = "out_of_range"
	detailTooShort         = "too_short"
	detailTooLong          = "too_long"
	detailDuplicate        = "duplicate"
	detailUnknownReference = "unknown_reference"
)

var (
	// ErrNotFound means that the ticket does not exist, or that the session may not see it. The two cases are the same answer.
	ErrNotFound = errors.New("ticket not found")
	// ErrNoMatch means that no ticket has the number and the email of a lookup. A wrong number and a wrong email are the same error.
	ErrNoMatch = errors.New("no ticket matches the number and the email")
	// ErrNumbersExhausted means that the running number of the month is past 9999, which the shape of a number cannot hold.
	ErrNumbersExhausted = errors.New("the numbers of this month are used up")
)

// Requester is the person that a ticket belongs to. The fields are nil after the retention job anonymized the ticket.
type Requester struct {
	Kind   string  `json:"kind"`
	Name   *string `json:"name"`
	NimNip *string `json:"nimNip"`
	Email  *string `json:"email"`
}

// Item is a model that a ticket asks for. ApprovedQty is nil before the decision.
type Item struct {
	ModelID     string `json:"modelId"`
	ModelName   string `json:"modelName"`
	Qty         int    `json:"qty"`
	ApprovedQty *int   `json:"approvedQty"`
}

// HistoryEntry is one change of status.
type HistoryEntry struct {
	At        string  `json:"at"`
	Status    string  `json:"status"`
	ActorKind string  `json:"actorKind"`
	Note      *string `json:"note"`
}

// Ticket is a ticket as a borrower sees it. ID is the primary key, and it is never part of the JSON, because the number is the id
// of a ticket for a client.
type Ticket struct {
	ID                   string         `json:"-"`
	Number               string         `json:"number"`
	Status               string         `json:"status"`
	EffectiveStatus      string         `json:"effectiveStatus"`
	Version              int            `json:"version"`
	CreatedAt            string         `json:"createdAt"`
	Requester            Requester      `json:"requester"`
	Purpose              *string        `json:"purpose"`
	PickupDate           string         `json:"pickupDate"`
	DurationDays         int            `json:"durationDays"`
	ApprovedDurationDays *int           `json:"approvedDurationDays"`
	DueDate              string         `json:"dueDate"`
	Items                []Item         `json:"items"`
	DecisionNote         *string        `json:"decisionNote"`
	History              []HistoryEntry `json:"history"`
	AnonymizedAt         *string        `json:"anonymizedAt"`
}

// ItemInput is one entry of the item list of a request. A nil Qty is a missing field.
type ItemInput struct {
	ModelID string
	Qty     *int
}

// CreateRequest is the body of a new ticket with the session that sends it. The name, the NIM or NIP and the email come from the
// identity of the session. A nil DurationDays is a missing field.
type CreateRequest struct {
	Identity     session.Identity
	SessionHash  []byte
	Items        []ItemInput
	Purpose      string
	PickupDate   string
	DurationDays *int
}
