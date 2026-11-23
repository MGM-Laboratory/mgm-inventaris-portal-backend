package ticket

import (
	"slices"
	"strings"
	"time"
)

// The stored statuses of a ticket, as in the check constraint of the table.
const (
	StatusSubmitted = "diajukan"
	StatusApproved  = "disetujui"
	StatusReady     = "siap_diambil"
	StatusBorrowed  = "dipinjam"
	StatusReturned  = "dikembalikan"
	StatusCancelled = "dibatalkan"
	StatusRejected  = "ditolak"

	// StatusLate is shown for a ticket in the status dipinjam after its due date. It is never stored.
	StatusLate = "terlambat"
)

// The kinds of actor in the history of a ticket, as in the check constraint of the table.
const (
	ActorGuest = "guest"
	ActorAdmin = "admin"
)

// Statuses lists every stored status.
func Statuses() []string {
	return []string{StatusSubmitted, StatusApproved, StatusReady, StatusBorrowed, StatusReturned, StatusCancelled, StatusRejected}
}

// holding lists the statuses whose approved quantity is held, and open lists the statuses of an open ticket. docs/data-model.md names
// both lists, and the catalog package keeps the same two lists in its own queries because it cannot import this package. A test of the
// database keeps the two copies equal.
var (
	holding = []string{StatusApproved, StatusReady, StatusBorrowed}
	open    = []string{StatusSubmitted, StatusApproved, StatusReady, StatusBorrowed}
)

// IsHolding says whether a ticket in the status holds the approved quantity of its items.
func IsHolding(status string) bool {
	return slices.Contains(holding, status)
}

// IsOpen says whether a ticket in the status counts for the limit of open tickets.
func IsOpen(status string) bool {
	return slices.Contains(open, status)
}

// sqlList writes statuses as the body of an SQL list. The values are the constants above and never come from a request.
func sqlList(statuses []string) string {
	quoted := make([]string, 0, len(statuses))
	for _, s := range statuses {
		quoted = append(quoted, "'"+s+"'")
	}

	return strings.Join(quoted, ", ")
}

// Step is one change of status that a kind of actor may make. A route that changes a status checks this table, so a new route adds a
// line here and nowhere else.
type Step struct {
	From  string
	To    string
	Actor string
}

// steps is the table of the allowed changes. A guest cancels only a ticket that nobody decided on, and an admin does every other step.
var steps = []Step{
	{StatusSubmitted, StatusApproved, ActorAdmin},
	{StatusSubmitted, StatusRejected, ActorAdmin},
	{StatusSubmitted, StatusCancelled, ActorGuest},
	{StatusApproved, StatusReady, ActorAdmin},
	{StatusApproved, StatusCancelled, ActorAdmin},
	{StatusReady, StatusBorrowed, ActorAdmin},
	{StatusReady, StatusCancelled, ActorAdmin},
	{StatusBorrowed, StatusReturned, ActorAdmin},
}

// Allowed says whether the actor may move a ticket from one status to another.
func Allowed(from, to, actor string) bool {
	return slices.Contains(steps, Step{From: from, To: to, Actor: actor})
}

// Sources lists the statuses from which the actor may move a ticket to the status. A guarded update names them as the expected old
// status.
func Sources(to, actor string) []string {
	var from []string

	for _, s := range steps {
		if s.To == to && s.Actor == actor {
			from = append(from, s.From)
		}
	}

	return from
}

// jakarta is the time zone of a pickup date, a due date and the month of a number. Indonesia has no daylight saving time in this
// zone, so a fixed offset is the same as Asia/Jakarta and needs no time zone database in the image.
var jakarta = time.FixedZone("Asia/Jakarta", 7*60*60)

const dateLayout = "2006-01-02"

// today is the date in Asia/Jakarta at the moment, as a date at midnight UTC so it compares with a date column.
func today(now time.Time) time.Time {
	local := now.In(jakarta)

	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

// dueDate is the pickup date plus the approved days, or the requested days when none were approved.
func dueDate(pickup time.Time, days int, approved *int) time.Time {
	if approved != nil {
		days = *approved
	}

	return pickup.AddDate(0, 0, days)
}

// effectiveStatus is the status that a client shows. A ticket in the status dipinjam is late when today is after its due date.
func effectiveStatus(status string, due, today time.Time) string {
	if status == StatusBorrowed && today.After(due) {
		return StatusLate
	}

	return status
}
