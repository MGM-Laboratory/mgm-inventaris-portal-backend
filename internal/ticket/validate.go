package ticket

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
)

const (
	purposeMin = 5
	purposeMax = 500
)

var numberShape = regexp.MustCompile(`^MGM-[0-9]{4}-[0-9]{4}-[A-HJ-NP-Z2-9]{4}$`)

// ValidNumber says whether the text has the shape of a ticket number. A text without it names no ticket.
func ValidNumber(number string) bool {
	return numberShape.MatchString(number)
}

// Limits are the limits of the lab that a new ticket obeys. They come from the settings.
type Limits struct {
	// MaxItems is the most items in one request, all quantities added.
	MaxItems int
	// MinDays and MaxDays bound the days of a loan.
	MinDays int
	MaxDays int
	// MaxOpen is the most open tickets for one email or one NIM or NIP.
	MaxOpen int
}

// checker collects the failed fields of one request.
type checker struct {
	fails []catalog.FieldError
}

func (c *checker) fail(field, code, message string) {
	c.fails = append(c.fails, catalog.FieldError{Field: field, Code: code, Message: message})
}

func (c *checker) err() error {
	if len(c.fails) == 0 {
		return nil
	}

	return &catalog.ValidationError{Details: c.fails}
}

// plain says whether a text can be stored. PostgreSQL refuses a NUL byte and a sequence that is not UTF-8, which would otherwise be a 500.
func plain(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// item is a requested model after the checks.
type item struct {
	modelID string
	qty     int
}

// input is a request after the checks, in the form that the transaction uses.
type input struct {
	items    []item
	purpose  string
	pickup   time.Time
	duration int
}

// check validates the body of a new ticket against the limits at the moment now, and returns the cleaned input. A failing field is
// named once, and every failing field comes back together. The checks of the stock and of the open tickets need the database and
// run later.
func (r CreateRequest) check(limits Limits, now time.Time) (input, error) {
	var c checker

	out := input{items: c.items(r.Items, limits.MaxItems)}

	purpose := strings.TrimSpace(r.Purpose)

	switch n := utf8.RuneCountInString(purpose); {
	case !plain(purpose):
		c.fail("purpose", detailInvalidFormat, "Use plain text.")
	case n == 0:
		c.fail("purpose", detailRequired, "This field is required.")
	case n < purposeMin:
		c.fail("purpose", detailTooShort, "Use at least "+strconv.Itoa(purposeMin)+" characters.")
	case n > purposeMax:
		c.fail("purpose", detailTooLong, "Use at most "+strconv.Itoa(purposeMax)+" characters.")
	}

	out.purpose = purpose
	out.pickup = c.pickupDate(r.PickupDate, today(now))
	out.duration = c.durationDays(r.DurationDays, limits)

	return out, c.err()
}

// items checks the item list. The sum of the quantities is checked against the limit only when every quantity is a number, so one
// bad quantity does not hide behind a second message about the sum.
func (c *checker) items(in []ItemInput, maxItems int) []item {
	if len(in) == 0 {
		c.fail("items", detailRequired, "Ask for at least one item.")

		return nil
	}

	var (
		out    = make([]item, 0, len(in))
		seen   = map[string]bool{}
		sum    int64
		sumSet = true
	)

	for i, entry := range in {
		field := "items[" + strconv.Itoa(i) + "]"
		model := strings.ToLower(strings.TrimSpace(entry.ModelID))

		switch parsed, err := uuid.Parse(model); {
		case model == "":
			c.fail(field+".modelId", detailRequired, "This field is required.")
		case err != nil || parsed.String() != model:
			c.fail(field+".modelId", detailInvalidFormat, "Use the identifier of a model.")
		case seen[model]:
			c.fail(field+".modelId", detailDuplicate, "Ask for each model once and raise its quantity instead.")
		default:
			seen[model] = true
		}

		switch {
		case entry.Qty == nil:
			c.fail(field+".qty", detailRequired, "This field is required.")

			sumSet = false
		case *entry.Qty < 1:
			c.fail(field+".qty", detailOutOfRange, "Ask for at least 1.")

			sumSet = false
		default:
			// a quantity above the limit counts as one more than the limit, so a sum of huge quantities cannot overflow
			sum += min(int64(*entry.Qty), int64(maxItems)+1)
			out = append(out, item{modelID: model, qty: *entry.Qty})
		}
	}

	if sumSet && sum > int64(maxItems) {
		c.fail("items", DetailTooManyItems, fmt.Sprintf("Ask for at most %d items in all.", maxItems))
	}

	return out
}

// pickupDate reads a date as YYYY-MM-DD and refuses a date before today in Asia/Jakarta.
func (c *checker) pickupDate(value string, today time.Time) time.Time {
	value = strings.TrimSpace(value)

	if value == "" {
		c.fail("pickupDate", detailRequired, "This field is required.")

		return time.Time{}
	}

	date, err := time.Parse(dateLayout, value)
	if err != nil {
		c.fail("pickupDate", detailInvalidFormat, "Use a date such as 2026-11-20.")

		return time.Time{}
	}

	if date.Before(today) {
		c.fail("pickupDate", detailOutOfRange, "Use today or a later date.")
	}

	return date
}

func (c *checker) durationDays(value *int, limits Limits) int {
	if value == nil {
		c.fail("durationDays", detailRequired, "This field is required.")

		return 0
	}

	if *value < limits.MinDays || *value > limits.MaxDays {
		c.fail("durationDays", detailOutOfRange, fmt.Sprintf("Use from %d to %d days.", limits.MinDays, limits.MaxDays))
	}

	return *value
}
