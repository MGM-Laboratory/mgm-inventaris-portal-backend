package ticket

import (
	"context"
	"log/slog"
)

// WithNotifier sets the notifier that hears about the decisions, the ready notices and the cancels of an admin. A store with none tells
// nobody. It returns the store, so the call can follow NewStore.
func (s *Store) WithNotifier(n Notifier) *Store {
	s.notifier = n

	return s
}

// notify tells the borrower about a change that is already committed. A failed notice is logged without the address and never
// changes the answer, because the change cannot be undone by a mail that did not leave.
func (s *Store) notify(ctx context.Context, kind NoticeKind, t AdminTicket) {
	if s.notifier == nil {
		return
	}

	notice := Notice{Kind: kind, Number: t.Number, Name: t.Requester.Name, Email: t.Requester.Email, Note: t.DecisionNote}

	if err := s.notifier.Notify(context.WithoutCancel(ctx), notice); err != nil {
		slog.Error("a notice of a ticket failed", "kind", kind, "number", t.Number)
	}
}
