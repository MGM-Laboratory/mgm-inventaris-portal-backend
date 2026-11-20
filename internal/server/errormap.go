package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/errchain"
)

// Code is a stable error code that a client branches on. The list is the same as in docs/errors.md.
type Code string

// The codes of docs/errors.md.
const (
	CodeBadRequest         Code = "bad_request"
	CodeUnauthenticated    Code = "unauthenticated"
	CodeSessionExpired     Code = "session_expired"
	CodeInvalidCredentials Code = "invalid_credentials"
	CodeForbidden          Code = "forbidden"
	CodeIdentityRequired   Code = "identity_required"
	CodeCSRFFailed         Code = "csrf_failed"
	CodeOriginNotAllowed   Code = "origin_not_allowed"
	CodeNotFound           Code = "not_found"
	CodeTicketNotFound     Code = "ticket_not_found"
	CodeStockChanged       Code = "stock_changed"
	CodeVersionConflict    Code = "version_conflict"
	CodeStatusConflict     Code = "status_conflict"
	CodeUnitHeld           Code = "unit_held"
	CodeUnitUnavailable    Code = "unit_unavailable"
	CodeDuplicate          Code = "duplicate"
	CodeRequestInProgress  Code = "request_in_progress"
	CodeValidationFailed   Code = "validation_failed"
	CodeRateLimited        Code = "rate_limited"
	CodeInternal           Code = "internal"
	CodeUnavailable        Code = "unavailable"
)

// catalog is the one place that gives each code its status and its default message. The messages hold no internal detail.
var catalog = map[Code]struct {
	status  int
	message string
}{
	CodeBadRequest:         {http.StatusBadRequest, "The request cannot be taken."},
	CodeUnauthenticated:    {http.StatusUnauthorized, "Sign in to use this route."},
	CodeSessionExpired:     {http.StatusUnauthorized, "The session expired. Sign in again."},
	CodeInvalidCredentials: {http.StatusUnauthorized, "The email, the password or the code is wrong."},
	CodeForbidden:          {http.StatusForbidden, "This session cannot use this route."},
	CodeIdentityRequired:   {http.StatusForbidden, "Send the guest form first."},
	CodeCSRFFailed:         {http.StatusForbidden, "The CSRF token is missing or wrong."},
	CodeOriginNotAllowed:   {http.StatusForbidden, "The origin of the request is not allowed."},
	CodeNotFound:           {http.StatusNotFound, "The thing does not exist."},
	CodeTicketNotFound:     {http.StatusNotFound, "No ticket matches this number and email."},
	CodeStockChanged:       {http.StatusConflict, "The stock changed. Reload and decide again."},
	CodeVersionConflict:    {http.StatusConflict, "The ticket changed. Reload and apply the change again."},
	CodeStatusConflict:     {http.StatusConflict, "The status does not allow this change."},
	CodeUnitHeld:           {http.StatusConflict, "The unit is in an open ticket."},
	CodeUnitUnavailable:    {http.StatusConflict, "The unit cannot be handed over."},
	CodeDuplicate:          {http.StatusConflict, "The value is already used."},
	CodeRequestInProgress:  {http.StatusConflict, "The first request of this key still runs."},
	CodeValidationFailed:   {http.StatusUnprocessableEntity, "A field fails a rule."},
	CodeRateLimited:        {http.StatusTooManyRequests, "Too many requests. Wait and try again."},
	CodeInternal:           {http.StatusInternalServerError, "Something went wrong. Give the request id to a maintainer."},
	CodeUnavailable:        {http.StatusServiceUnavailable, "The service cannot reach its database. Try again later."},
}

type Detail struct {
	Field       string `json:"field"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	ExistingTag string `json:"existingTag,omitempty"`
}

// Error is the error that a handler returns. The handler never builds a status or a body of its own.
type Error struct {
	Code    Code
	Message string
	Details []Detail
	// RetryAfter is the number of seconds for the Retry-After header, and 0 leaves the header out.
	RetryAfter int
}

func NewError(code Code, details ...Detail) *Error {
	return &Error{Code: code, Message: catalog[code].message, Details: details}
}

func (e *Error) Error() string {
	return string(e.Code)
}

func (e *Error) Status() int {
	if entry, ok := catalog[e.Code]; ok {
		return entry.status
	}

	return http.StatusInternalServerError
}

// errorBody and errorEnvelope are the JSON of the schemas ErrorBody and Error in openapi.yaml.
type errorBody struct {
	Code      Code     `json:"code"`
	Message   string   `json:"message"`
	Details   []Detail `json:"details,omitempty"`
	RequestID string   `json:"requestId"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// resolve maps any error to the Error that the client sees and to the status of its answer. A status that fiber chose for itself
// keeps its number, and every error of a handler that is not an *Error becomes internal, whatever its text says.
func resolve(err error) (*Error, int) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr, apiErr.Status()
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		switch {
		case fiberErr.Code == http.StatusNotFound:
			return NewError(CodeNotFound), http.StatusNotFound
		case fiberErr.Code >= http.StatusBadRequest && fiberErr.Code < http.StatusInternalServerError:
			return NewError(CodeBadRequest), fiberErr.Code
		}
	}

	return NewError(CodeInternal), http.StatusInternalServerError
}

func logFailure(c fiber.Ctx, err error, status int) {
	attrs := []any{"request_id", requestID(c), "method", c.Method(), "path", routeLabel(c, err), "status", status}

	var apiErr *Error
	if errors.As(err, &apiErr) {
		attrs = append(attrs, "code", apiErr.Code)
	} else {
		attrs = append(attrs, "error_chain", errchain.Chain(err))
	}

	slog.Error("a route failed", attrs...)
}

// ErrorHandler writes the error body of the contract for every error of a route, a missing route and a panic.
func ErrorHandler(c fiber.Ctx, err error) error {
	apiErr, status := resolve(err)

	if status >= http.StatusInternalServerError {
		logFailure(c, err, status)
	}

	if apiErr.RetryAfter > 0 {
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(apiErr.RetryAfter))
	}

	return c.Status(status).JSON(errorEnvelope{Error: errorBody{
		Code: apiErr.Code, Message: apiErr.Message, Details: apiErr.Details, RequestID: requestID(c),
	}})
}
