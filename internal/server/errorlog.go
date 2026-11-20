package server

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

const maxChain = 16

// errorChain lists the errors of a chain from the outside in, so a log line says what failed without a row or a value of the request.
// An entry holds the type. A wrapper made with fmt.Errorf adds its own words, a plain errors.New adds its text, and a PostgreSQL error
// adds its code and constraint. Every other error adds only its type, because its text can hold a value of the request. A message
// that is written in our code must therefore never format request data into itself.
func errorChain(err error) []string {
	var chain []string

	for range maxChain {
		if err == nil {
			return chain
		}

		chain = append(chain, describeError(err))

		switch e := err.(type) { //nolint:errorlint // the loop walks the chain itself, one level at a time
		case interface{ Unwrap() error }:
			err = e.Unwrap()
		case interface{ Unwrap() []error }:
			inner := e.Unwrap()
			if len(inner) == 0 {
				return chain
			}

			err = inner[0]
		default:
			return chain
		}
	}

	return chain
}

func describeError(err error) string {
	typ := fmt.Sprintf("%T", err)

	if pgErr, ok := err.(*pgconn.PgError); ok { //nolint:errorlint // one level, the caller walks the chain
		return fmt.Sprintf("%s sqlstate %s constraint %q", typ, pgErr.Code, pgErr.ConstraintName)
	}

	switch typ {
	case "*errors.errorString":
		return typ + ": " + err.Error()
	case "*fmt.wrapError":
		text := err.Error()
		if inner := errors.Unwrap(err); inner != nil {
			text = strings.Replace(text, inner.Error(), "", 1)
		}

		return typ + ": " + strings.TrimSpace(text)
	default:
		return typ
	}
}
