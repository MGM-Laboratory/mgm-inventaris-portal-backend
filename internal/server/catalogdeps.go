package server

import (
	"context"
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

const (
	detailOutOfRange       = "out_of_range"
	detailUnknownReference = "unknown_reference"

	idempotencyHeader = "Idempotency-Key"
)

// CatalogService is the part of the catalog package that the routes use.
type CatalogService interface {
	ListCategories(ctx context.Context, req catalog.PageRequest) (pagination.Page[catalog.Category], error)
	ListCatalog(ctx context.Context, f catalog.ModelFilter, req catalog.PageRequest) (pagination.Page[catalog.CatalogModel], error)
	GetPublicAsset(ctx context.Context, tag string) (catalog.PublicAsset, error)
	GetAdminAsset(ctx context.Context, tag string) (catalog.AdminAsset, error)

	CreateCategory(ctx context.Context, in catalog.CategoryInput, rec catalog.Recorder) (catalog.Category, error)
	ListModels(ctx context.Context, f catalog.ModelFilter, req catalog.PageRequest) (pagination.Page[catalog.Model], error)
	CreateModel(ctx context.Context, in catalog.NewModel, rec catalog.Recorder) (catalog.Model, error)
	UpdateModel(ctx context.Context, id string, in catalog.ModelPatch) (catalog.Model, error)
	DeleteModel(ctx context.Context, id string) error

	CreateUnits(ctx context.Context, modelID, adminID string, in catalog.UnitInput, rec catalog.Recorder) (catalog.UnitList, error)
	ListUnits(ctx context.Context, f catalog.UnitFilter, req catalog.PageRequest) (pagination.Page[catalog.Unit], error)
	GetUnit(ctx context.Context, tag string) (catalog.Unit, error)
	UpdateUnit(ctx context.Context, tag, adminID string, in catalog.UnitPatch) (catalog.Unit, error)
	DeleteUnit(ctx context.Context, tag string) error
	ChangeLifecycle(ctx context.Context, tag, adminID string, in catalog.LifecycleChange) (catalog.Unit, error)
	ListUnitEvents(ctx context.Context, tag string, req catalog.PageRequest) (pagination.Page[catalog.UnitEvent], error)
	ListAllUnitEvents(ctx context.Context, req catalog.PageRequest) (pagination.Page[catalog.UnitEvent], error)
}

// Idempotency is the part of the idempotency package that the routes use.
type Idempotency interface {
	Begin(ctx context.Context, key idempotency.Key) (idempotency.Claim, *idempotency.Answer, error)
}

// RequireReader lets a guest session or an admin session through. A setup session of an admin may call only the setup routes, so it
// answers 403 forbidden like any other kind.
func RequireReader() fiber.Handler {
	return func(c fiber.Ctx) error {
		sess, err := requireSession(c)
		if err != nil {
			return err
		}

		if sess.Kind != session.KindGuest && sess.Kind != session.KindAdmin {
			return NewError(CodeForbidden)
		}

		return c.Next()
	}
}

// readPage reads the limit and the cursor of a list. A value that fails is returned as a detail, and the route answers 422 with all
// the details together.
func readPage(c fiber.Ctx) (catalog.PageRequest, []*Detail) {
	var fails []*Detail

	rawLimit := c.Query("limit")

	limit, err := pagination.ParseLimit(rawLimit)
	if err != nil {
		if _, convErr := strconv.Atoi(rawLimit); convErr != nil {
			fails = append(fails, detail("limit", detailInvalidFormat, "Use a whole number from 1 to 100."))
		} else {
			fails = append(fails, detail("limit", detailOutOfRange, "Use a number from 1 to 100."))
		}
	}

	cursor := c.Query("cursor")
	if len(cursor) > pagination.MaxCursorLength {
		fails = append(fails, detail("cursor", detailInvalidFormat, "Send the cursor of the last page unchanged."))
	}

	return catalog.PageRequest{Limit: limit, Cursor: cursor}, fails
}

// queryBool reads a flag of the query. No value is false.
func queryBool(c fiber.Ctx, name string) (bool, *Detail) {
	switch c.Query(name) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, detail(name, detailInvalidFormat, "Use true or false.")
	}
}

// fieldDetails turns the failed fields of the catalog package into the details of an answer.
func fieldDetails(fields []catalog.FieldError) []Detail {
	details := make([]Detail, 0, len(fields))
	for _, f := range fields {
		details = append(details, Detail{Field: f.Field, Code: f.Code, Message: f.Message})
	}

	return details
}

var ruleCodes = map[catalog.RuleKind]Code{
	catalog.RuleStatusConflict: CodeStatusConflict,
	catalog.RuleUnitHeld:       CodeUnitHeld,
	catalog.RuleStockChanged:   CodeStockChanged,
}

// catalogError maps the error of the catalog or of the idempotency store to the answer of the contract. An error that no rule names is
// a database failure, and dbError decides between 503 and 500. what names the call for the log.
func catalogError(what string, err error) error {
	var (
		invalid *catalog.ValidationError
		dup     *catalog.DuplicateError
		rule    *catalog.RuleError
	)

	switch {
	case errors.As(err, &invalid):
		return NewError(CodeValidationFailed, fieldDetails(invalid.Details)...)
	case errors.Is(err, catalog.ErrNotFound):
		return NewError(CodeNotFound)
	case errors.Is(err, pagination.ErrCursor):
		return NewError(CodeValidationFailed, *detail("cursor", detailInvalidFormat, "Send the cursor of the last page unchanged."))
	case errors.As(err, &dup):
		return duplicateError(dup)
	case errors.As(err, &rule):
		e := NewError(ruleCodes[rule.Kind])
		e.Message = rule.Message

		return e
	case errors.Is(err, idempotency.ErrInProgress), errors.Is(err, idempotency.ErrLost):
		return NewError(CodeRequestInProgress)
	default:
		return dbError(what, err)
	}
}

func duplicateError(dup *catalog.DuplicateError) *Error {
	message := "Choose another " + dup.Field + "."

	switch {
	case dup.Field == "serial" && dup.ExistingTag != "":
		message = "The serial number belongs to " + dup.ExistingTag + "."
	case dup.Field == "serial":
		message = "Choose another serial number."
	}

	return NewError(CodeDuplicate, Detail{Field: dup.Field, Code: detailDuplicate, Message: message, ExistingTag: dup.ExistingTag})
}
