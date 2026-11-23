// Package catalog owns the tables of the assets, which are categories, models, units and the events of units. It also reads the
// availability of a model from the tables of the tickets. A route handler calls its functions and never builds SQL.
package catalog

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The lifecycle values of a unit, as in the check constraint of the table.
const (
	LifecycleActive      = "active"
	LifecycleMaintenance = "maintenance"
	LifecycleLost        = "lost"
	LifecycleRetired     = "retired"
)

// What an admin finds when a unit comes back from a loan. The outcomes maintenance and lost are also lifecycle values.
const (
	ReturnOK          = "ok"
	ReturnMaintenance = LifecycleMaintenance
	ReturnLost        = LifecycleLost
)

// The statuses that a unit shows.
const (
	StatusAvailable = "available"
	StatusBorrowed  = "borrowed"
)

// The tones of a category tile.
var tones = []string{"blue", "yellow", "red", "green"}

// The detail codes of the contract that this package uses.
const (
	DetailRequired         = "required"
	DetailInvalidFormat    = "invalid_format"
	DetailOutOfRange       = "out_of_range"
	DetailTooLong          = "too_long"
	DetailTooShort         = "too_short"
	DetailUnknownReference = "unknown_reference"
	DetailDuplicate        = "duplicate"
)

// Querier is what a function needs from a pool or from a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB is a pool. Every write begins its own transaction on it.
type DB interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Recorder stores the answer of a request in the transaction of the write. The Claim of the idempotency package is one. A nil
// Recorder means that the request carried no key.
type Recorder interface {
	Record(ctx context.Context, tx pgx.Tx, status int, body any) error
}

// ErrNotFound means that the model, the unit or the category does not exist.
var ErrNotFound = errors.New("not found")

// FieldError is one field that failed a rule. The route turns it into an entry of details.
type FieldError struct {
	Field   string
	Code    string
	Message string
}

// ValidationError is a request that fails the rules of its fields.
type ValidationError struct {
	Details []FieldError
}

func (e *ValidationError) Error() string {
	return "the request fails a rule"
}

// DuplicateError is a name, a prefix or a serial number that another record has. ExistingTag is set for a serial number.
type DuplicateError struct {
	Field       string
	ExistingTag string
}

func (e *DuplicateError) Error() string {
	return "duplicate " + e.Field
}

// RuleKind says which rule of the state a request broke.
type RuleKind int

const (
	// RuleStatusConflict is a change that the status does not allow, or a permanent delete of something that a ticket used.
	RuleStatusConflict RuleKind = iota + 1
	// RuleUnitHeld is a unit in an open ticket that cannot leave the lifecycle active.
	RuleUnitHeld
	// RuleStockChanged is a change that would leave less stock than the tickets hold.
	RuleStockChanged
	// RuleVersionConflict is a request that carries an older version of a ticket than the stored one.
	RuleVersionConflict
	// RuleUnitUnavailable is a unit at a handover that is not active, not of a model of the ticket, or in an open ticket.
	RuleUnitUnavailable
)

// RuleError is a request that the state of the data refuses. Message is safe to show to a client. Details names the fields that the
// rule is about, and most rules have none.
type RuleError struct {
	Kind    RuleKind
	Message string
	Details []FieldError
}

func (e *RuleError) Error() string {
	return e.Message
}

// Category is a group of models.
type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Tone string `json:"tone"`
}

// CatalogModel is a kind of item with its quantities. Available is never negative.
type CatalogModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CategoryID  string `json:"categoryId"`
	Description string `json:"description"`
	Total       int    `json:"total"`
	Held        int    `json:"held"`
	Available   int    `json:"available"`
}

// Model is a model as an admin sees it.
type Model struct {
	CatalogModel
	Prefix    string  `json:"prefix"`
	RetiredAt *string `json:"retiredAt"`
}

// Unit is a unit as an admin sees it. OpenTicketNumber is empty when no open ticket holds the unit.
type Unit struct {
	Tag              string  `json:"tag"`
	ModelID          string  `json:"modelId"`
	ModelName        string  `json:"modelName"`
	CategoryID       string  `json:"categoryId"`
	CategoryName     string  `json:"categoryName"`
	Serial           *string `json:"serial"`
	Condition        string  `json:"condition"`
	Place            string  `json:"place"`
	Lifecycle        string  `json:"lifecycle"`
	LifecycleNote    *string `json:"lifecycleNote"`
	Status           string  `json:"status"`
	OpenTicketNumber string  `json:"openTicketNumber,omitempty"`
	CreatedAt        string  `json:"createdAt"`
}

// UnitList is the answer of the route that adds units.
type UnitList struct {
	Items []Unit `json:"items"`
}

// UnitEvent is one row of the history of a unit.
type UnitEvent struct {
	ID           string  `json:"id"`
	Tag          string  `json:"tag"`
	At           string  `json:"at"`
	Action       string  `json:"action"`
	AdminName    *string `json:"adminName"`
	Note         *string `json:"note"`
	TicketNumber *string `json:"ticketNumber"`
}

// AssetModel is the model of a unit with its category and its available quantity.
type AssetModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	CategoryID  string   `json:"categoryId"`
	Category    Category `json:"category"`
	Available   int      `json:"available"`
}

// PublicAsset is the page of a unit for anyone. It holds only the tag, the status and the model.
type PublicAsset struct {
	Tag    string     `json:"tag"`
	Status string     `json:"status"`
	Model  AssetModel `json:"model"`
}

// RelatedTicket is the open ticket that holds a unit.
type RelatedTicket struct {
	Number          string  `json:"number"`
	EffectiveStatus string  `json:"effectiveStatus"`
	RequesterName   *string `json:"requesterName"`
}

// AdminAsset is the page of a unit for an admin.
type AdminAsset struct {
	Tag           string         `json:"tag"`
	Status        string         `json:"status"`
	Model         AssetModel     `json:"model"`
	Serial        *string        `json:"serial"`
	Condition     string         `json:"condition"`
	Place         string         `json:"place"`
	RelatedTicket *RelatedTicket `json:"relatedTicket"`
}

// PageRequest is the limit and the cursor of a list. The route has checked the limit.
type PageRequest struct {
	Limit  int
	Cursor string
}

// CategoryInput is the body that adds a category.
type CategoryInput struct {
	Name string
	Tone string
}

// NewModel is the body that adds a model. Exactly one of CategoryID and NewCategory is set.
type NewModel struct {
	Name        string
	CategoryID  *string
	NewCategory *CategoryInput
	Description *string
	Prefix      string
}

// ModelPatch holds the fields that a change names. A nil field stays as it is.
type ModelPatch struct {
	Name        *string
	CategoryID  *string
	Description *string
	Retired     *bool
}

// UnitInput is the body that adds units. A nil Count is a missing field.
type UnitInput struct {
	Count     *int
	Serial    *string
	Condition string
	Place     string
}

// UnitPatch holds the fields that a change names. SerialSet is true when the body has the field, and a nil Serial then clears it.
type UnitPatch struct {
	SerialSet bool
	Serial    *string
	Condition *string
	Place     *string
}

// LifecycleChange is the body of a lifecycle step.
type LifecycleChange struct {
	Lifecycle string
	Note      *string
}

// ModelFilter narrows the list of models.
type ModelFilter struct {
	Query          string
	CategoryID     string
	Available      bool
	IncludeRetired bool
}

// UnitFilter narrows the list of units.
type UnitFilter struct {
	Query      string
	ModelID    string
	CategoryID string
	Status     string
}
