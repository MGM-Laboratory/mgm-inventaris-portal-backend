package main

import (
	"context"
	"log/slog"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ticket"
)

// logNotifier stands in for a mail provider, which the lab has not chosen yet. It writes what kind of notice would go out and for which
// ticket, and it never writes the address, the name or the note. The last four characters of a ticket number act as its secret for a
// lookup, so the log holds the number without them.
type logNotifier struct{}

func (logNotifier) Notify(ctx context.Context, notice ticket.Notice) error {
	slog.InfoContext(ctx, "a ticket notice would be sent", "kind", notice.Kind, "ticket", ticket.WithoutSecret(notice.Number))

	return nil
}
