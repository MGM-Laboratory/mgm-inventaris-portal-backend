package ticket

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
)

const (
	noteMax  = 500
	queryMin = 2
	queryMax = 100
)

// version checks the version of a request. A missing version is required, and a version below 1 cannot belong to a ticket.
func (c *checker) version(v *int) int {
	switch {
	case v == nil:
		c.fail("version", detailRequired, "This field is required.")

		return 0
	case *v < 1:
		c.fail("version", detailOutOfRange, "Use the version that you read, which is 1 or more.")

		return 0
	default:
		return *v
	}
}

// note checks a note. A required note must hold a character after the spaces at both ends are removed. An optional note that is
// missing or blank is no note, because a form that sends an empty text means that nobody wrote one.
func (c *checker) note(field string, value *string, required bool) *string {
	if value == nil {
		if required {
			c.fail(field, detailRequired, "This field is required.")
		}

		return nil
	}

	trimmed := strings.TrimSpace(*value)

	switch n := utf8.RuneCountInString(trimmed); {
	case !plain(trimmed):
		c.fail(field, detailInvalidFormat, "Use plain text.")
	case n == 0 && required:
		c.fail(field, detailRequired, "This field is required.")
	case n == 0:
		return nil
	case n > noteMax:
		c.fail(field, detailTooLong, "Use at most "+strconv.Itoa(noteMax)+" characters.")
	}

	return &trimmed
}

// modelID reads the id of a model of a list. It returns the id in lower case, or the empty text after it named the failing field.
func (c *checker) modelID(field, value string, seen map[string]bool) string {
	model := strings.ToLower(strings.TrimSpace(value))

	switch parsed, err := uuid.Parse(model); {
	case model == "":
		c.fail(field, detailRequired, "This field is required.")
	case err != nil || parsed.String() != model:
		c.fail(field, detailInvalidFormat, "Use the identifier of a model.")
	case seen[model]:
		c.fail(field, detailDuplicate, "Name each model once.")
	default:
		seen[model] = true

		return model
	}

	return ""
}

// approved is one quantity of an approval after the checks. The index is the place in the request, which names the field of a later
// failure.
type approved struct {
	index   int
	modelID string
	qty     int
}

// decision is a decision after the checks.
type decision struct {
	version int
	approve bool
	items   []approved
	days    *int
	note    *string
}

// check validates the body of a decision against the limits of the lab. An approval needs the version, the quantity of every item and
// at least one quantity above 0. A rejection needs the version and a note. The checks that need the ticket run in the transaction.
func (r DecisionRequest) check(limits Limits) (decision, error) {
	var c checker

	out := decision{version: c.version(r.Version)}

	switch r.Decision {
	case DecisionApprove:
		out.approve = true
		out.items = c.approvedItems(r.Items)
		out.days = c.approvedDays(r.DurationDays, limits)
		out.note = c.note("note", r.Note, false)
	case DecisionReject:
		out.note = c.note("note", r.Note, true)
	case "":
		c.fail("decision", detailRequired, "This field is required.")
	default:
		c.fail("decision", detailInvalidFormat, "Use approve or reject.")
	}

	return out, c.err()
}

// approvedItems checks the quantities of an approval. The sum is checked only when every quantity is a number, so one bad quantity does
// not hide behind a second message.
func (c *checker) approvedItems(in []ApprovedInput) []approved {
	if len(in) == 0 {
		c.fail("items", detailRequired, "Name the approved quantity of every item.")

		return nil
	}

	var (
		out    = make([]approved, 0, len(in))
		seen   = map[string]bool{}
		sum    int64
		sumSet = true
	)

	for i, entry := range in {
		field := "items[" + strconv.Itoa(i) + "]"
		model := c.modelID(field+".modelId", entry.ModelID, seen)

		switch {
		case entry.ApprovedQty == nil:
			c.fail(field+".approvedQty", detailRequired, "This field is required.")

			sumSet = false
		case *entry.ApprovedQty < 0:
			c.fail(field+".approvedQty", detailOutOfRange, "Use 0 or more.")

			sumSet = false
		default:
			sum += int64(*entry.ApprovedQty)

			if model != "" {
				out = append(out, approved{index: i, modelID: model, qty: *entry.ApprovedQty})
			}
		}
	}

	if sumSet && sum == 0 {
		c.fail("items", detailOutOfRange, "Approve at least one item, or reject the ticket.")
	}

	return out
}

func (c *checker) approvedDays(value *int, limits Limits) *int {
	if value == nil {
		return nil
	}

	if *value < limits.MinDays || *value > limits.MaxDays {
		c.fail("durationDays", detailOutOfRange, fmt.Sprintf("Use from %d to %d days.", limits.MinDays, limits.MaxDays))
	}

	return value
}

// ticketModel is a model of a ticket with the quantity that it asked for.
type ticketModel struct {
	id       string
	name     string
	qty      int
	approved int
}

// matchItems checks the approval against the models of the ticket. Every model of the ticket has one entry, the model of an entry is
// on the ticket, and no approved quantity is above the requested one. A model with a quantity that was refused by the stock is not
// found here, because that check needs the locks.
func (d decision) matchItems(models []ticketModel) error {
	var c checker

	asked := make(map[string]int, len(models))
	for _, m := range models {
		asked[m.id] = m.qty
	}

	listed := make(map[string]bool, len(d.items))

	for _, it := range d.items {
		field := "items[" + strconv.Itoa(it.index) + "]"
		qty, ok := asked[it.modelID]

		switch {
		case !ok:
			c.fail(field+".modelId", detailUnknownReference, "The ticket does not ask for this model.")
		case it.qty > qty:
			c.fail(field+".approvedQty", detailOutOfRange, "At most "+strconv.Itoa(qty)+" were asked for.")
		}

		listed[it.modelID] = true
	}

	for _, m := range models {
		if !listed[m.id] {
			c.fail("items", detailRequired, "Name the approved quantity of every item of the ticket.")

			break
		}
	}

	return c.err()
}

// handover is a handover after the checks.
type handover struct {
	version int
	tags    []string
}

// check validates the body of a handover. Every tag has the shape of a tag and appears once. The tags are matched with the units and
// with the approved quantities in the transaction.
func (r HandoverRequest) check() (handover, error) {
	var c checker

	out := handover{version: c.version(r.Version)}

	if len(r.Tags) == 0 {
		c.fail("tags", detailRequired, "Name the tag of every unit that you hand over.")

		return out, c.err()
	}

	seen := map[string]bool{}

	for i, raw := range r.Tags {
		field := "tags[" + strconv.Itoa(i) + "]"
		tag := strings.TrimSpace(raw)

		switch {
		case tag == "":
			c.fail(field, detailRequired, "This field is required.")
		case !catalog.ValidTag(tag):
			c.fail(field, detailInvalidFormat, "Use a tag such as MGM-VRQ-001.")
		case seen[tag]:
			c.fail(field, detailDuplicate, "Name each unit once.")
		default:
			seen[tag] = true

			out.tags = append(out.tags, tag)
		}
	}

	return out, c.err()
}

// returnLine is a line of a return after the checks.
type returnLine struct {
	index   int
	tag     string
	outcome string
	note    *string
}

// returned is a return after the checks.
type returned struct {
	version int
	units   []returnLine
}

// check validates the body of a return. Every line has a tag, an outcome and, for a lost unit, a note. The lines are matched with the
// units of the ticket in the transaction.
func (r ReturnRequest) check() (returned, error) {
	var c checker

	out := returned{version: c.version(r.Version)}

	if len(r.Units) == 0 {
		c.fail("units", detailRequired, "Name every unit that the ticket holds.")

		return out, c.err()
	}

	seen := map[string]bool{}

	for i, line := range r.Units {
		field := "units[" + strconv.Itoa(i) + "]"
		tag := strings.TrimSpace(line.Tag)

		switch {
		case tag == "":
			c.fail(field+".tag", detailRequired, "This field is required.")
		case !catalog.ValidTag(tag):
			c.fail(field+".tag", detailInvalidFormat, "Use a tag such as MGM-VRQ-001.")
		case seen[tag]:
			c.fail(field+".tag", detailDuplicate, "Name each unit once.")
		default:
			seen[tag] = true
		}

		switch line.Outcome {
		case catalog.ReturnOK, catalog.ReturnMaintenance, catalog.ReturnLost:
		case "":
			c.fail(field+".outcome", detailRequired, "This field is required.")
		default:
			c.fail(field+".outcome", detailInvalidFormat, "Use ok, maintenance or lost.")
		}

		note := c.note(field+".note", line.Note, line.Outcome == catalog.ReturnLost)
		out.units = append(out.units, returnLine{index: i, tag: tag, outcome: line.Outcome, note: note})
	}

	return out, c.err()
}

// cancellation is the cancel of an admin after the checks.
type cancellation struct {
	version int
	note    *string
}

// check validates the body of the cancel of an admin, which needs the version and a note.
func (r AdminCancelRequest) check() (cancellation, error) {
	var c checker

	out := cancellation{version: c.version(r.Version)}
	out.note = c.note("note", r.Note, true)

	return out, c.err()
}

// version checks the body of a step that carries only the version.
func (r StepRequest) check() (int, error) {
	var c checker

	v := c.version(r.Version)

	return v, c.err()
}

// adminQuery is a filter of the queue after the checks. A nil date is no bound, and no statuses is no filter.
type adminQuery struct {
	text       string
	statuses   []string
	pickupFrom *time.Time
	pickupTo   *time.Time
	dueFrom    *time.Time
	dueTo      *time.Time
}

// search checks the text of a search. An empty text means no search.
func (c *checker) search(value string) string {
	value = strings.TrimSpace(value)

	switch n := utf8.RuneCountInString(value); {
	case !plain(value):
		c.fail("q", detailInvalidFormat, "Use plain text.")
	case n == 0:
	case n < queryMin:
		c.fail("q", detailTooShort, "Use at least "+strconv.Itoa(queryMin)+" characters.")
	case n > queryMax:
		c.fail("q", detailTooLong, "Use at most "+strconv.Itoa(queryMax)+" characters.")
	}

	return value
}

// date reads an optional date of a filter.
func (c *checker) date(field, value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	parsed, err := time.Parse(dateLayout, value)
	if err != nil {
		c.fail(field, detailInvalidFormat, "Use a date such as 2026-11-20.")

		return nil
	}

	return &parsed
}

// check validates a filter of the queue. A status that is not an effective status fails, and a status that appears twice counts once.
func (f AdminFilter) check() (adminQuery, error) {
	var c checker

	out := adminQuery{text: c.search(f.Query)}

	valid := append(Statuses(), StatusLate)

	for _, status := range f.Statuses {
		switch {
		case !slices.Contains(valid, status):
			c.fail("status", detailInvalidFormat, "Use a status of a ticket, such as disetujui, siap_diambil or terlambat.")
		case !slices.Contains(out.statuses, status):
			out.statuses = append(out.statuses, status)
		}
	}

	out.pickupFrom = c.date("pickupFrom", f.PickupFrom)
	out.pickupTo = c.date("pickupTo", f.PickupTo)
	out.dueFrom = c.date("dueFrom", f.DueFrom)
	out.dueTo = c.date("dueTo", f.DueTo)

	return out, c.err()
}
