package ticket

import (
	"context"
	"log/slog"
	"strings"
	"time"
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

	// a slow mail provider must not hold the answer of a step that is already committed
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()

	if err := s.notifier.Notify(ctx, notice); err != nil {
		slog.Error("a notice of a ticket failed", "kind", kind, "ticket", WithoutSecret(t.Number))
	}
}

// notifyTimeout is the time that a notifier has to send one notice.
const notifyTimeout = 5 * time.Second

// WithoutSecret returns a ticket number without its last part. That part is the secret that a borrower needs for a lookup, so it
// never goes into a log.
func WithoutSecret(number string) string {
	if cut := strings.LastIndex(number, "-"); cut > 0 {
		return number[:cut]
	}

	return number
}
