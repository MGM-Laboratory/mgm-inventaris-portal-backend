package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// dbError wraps the error of a database call. An outage becomes the 503 answer unavailable, and every other failure stays a plain
// error that the error handler logs as a chain and answers as internal. what says which call failed, and holds no request data.
func dbError(what string, err error) error {
	if databaseUnreachable(err) {
		return NewError(CodeUnavailable)
	}

	return fmt.Errorf("%s %w", what, err)
}

// databaseUnreachable says whether the error means that the database could not take the call at all, as opposed to a bug in a
// statement. It covers a refused or a broken connection, a timeout, and the SQLSTATE classes 08 (connection), 53 (resources) and 57
// (operator intervention, which includes a statement timeout).
func databaseUnreachable(err error) bool {
	var (
		pgErr   *pgconn.PgError
		connErr *pgconn.ConnectError
		netErr  net.Error
	)

	switch {
	case errors.As(err, &pgErr):
		class := pgErr.Code[:min(len(pgErr.Code), 2)]

		return class == "08" || class == "53" || class == "57"
	case errors.As(err, &connErr), errors.As(err, &netErr):
		return true
	default:
		return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	}
}
