package catalog

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	categoryNameMin = 2
	categoryNameMax = 60
	modelNameMin    = 2
	modelNameMax    = 100
	descriptionMax  = 500
	serialMin       = 3
	serialMax       = 100
	textMin         = 2
	textMax         = 200
	noteMax         = 500
	queryMin        = 2
	queryMax        = 100

	// MaxUnitsPerRequest is the most units that one request adds.
	MaxUnitsPerRequest = 20
)

var (
	prefixShape = regexp.MustCompile(`^[A-Z0-9]{2,6}$`)
	tagShape    = regexp.MustCompile(`^MGM-[A-Z0-9]{2,6}-[0-9]{3,}$`)

	lifecycles = []string{LifecycleActive, LifecycleMaintenance, LifecycleLost, LifecycleRetired}
	statuses   = []string{StatusAvailable, StatusBorrowed, LifecycleMaintenance, LifecycleLost, LifecycleRetired}
)

// checker collects the failed fields of one request.
type checker struct {
	fails []FieldError
}

func (c *checker) fail(field, code, message string) {
	c.fails = append(c.fails, FieldError{Field: field, Code: code, Message: message})
}

func (c *checker) err() error {
	if len(c.fails) == 0 {
		return nil
	}

	return &ValidationError{Details: c.fails}
}

// text trims the value and checks its length in characters. An empty value is required, and an optional field is skipped before the
// call. It returns the trimmed value.
func (c *checker) text(field, value string, minLen, maxLen int) string {
	value = strings.TrimSpace(value)

	switch n := utf8.RuneCountInString(value); {
	case n == 0:
		c.fail(field, DetailRequired, "This field is required.")
	case n < minLen:
		c.fail(field, DetailTooShort, "Use at least "+strconv.Itoa(minLen)+" characters.")
	case n > maxLen:
		c.fail(field, DetailTooLong, "Use at most "+strconv.Itoa(maxLen)+" characters.")
	}

	return value
}

// optionalText is a text that may be empty. It returns the trimmed value.
func (c *checker) optionalText(field, value string, maxLen int) string {
	value = strings.TrimSpace(value)

	if utf8.RuneCountInString(value) > maxLen {
		c.fail(field, DetailTooLong, "Use at most "+strconv.Itoa(maxLen)+" characters.")
	}

	return value
}

func (c *checker) uuid(field, value string) {
	if value == "" {
		c.fail(field, DetailRequired, "This field is required.")
	} else if !isUUID(value) {
		c.fail(field, DetailInvalidFormat, "Use the identifier of an existing record.")
	}
}

func (c *checker) tone(field, value string) {
	switch {
	case value == "":
		c.fail(field, DetailRequired, "This field is required.")
	case !slices.Contains(tones, value):
		c.fail(field, DetailInvalidFormat, "Use blue, yellow, red or green.")
	}
}

// serial trims a serial number and checks its length. An empty serial number means none, and the result is nil.
func (c *checker) serial(field string, value *string) *string {
	if value == nil {
		return nil
	}

	trimmed := strings.TrimSpace(*value)

	switch n := utf8.RuneCountInString(trimmed); {
	case n == 0:
		return nil
	case n < serialMin:
		c.fail(field, DetailTooShort, "Use at least "+strconv.Itoa(serialMin)+" characters.")
	case n > serialMax:
		c.fail(field, DetailTooLong, "Use at most "+strconv.Itoa(serialMax)+" characters.")
	}

	return &trimmed
}

// searchText checks the text of a search. An empty text means no search.
func (c *checker) searchText(value string) string {
	value = strings.TrimSpace(value)

	switch n := utf8.RuneCountInString(value); {
	case n == 0:
	case n < queryMin:
		c.fail("q", DetailTooShort, "Use at least "+strconv.Itoa(queryMin)+" characters.")
	case n > queryMax:
		c.fail("q", DetailTooLong, "Use at most "+strconv.Itoa(queryMax)+" characters.")
	}

	return value
}

func (c *checker) optionalUUID(field, value string) {
	if value != "" && !isUUID(value) {
		c.fail(field, DetailInvalidFormat, "Use the identifier of an existing record.")
	}
}

// check validates the body of a new category and returns the cleaned input.
func (in CategoryInput) check(field string, c *checker) CategoryInput {
	prefix := ""
	if field != "" {
		prefix = field + "."
	}

	out := CategoryInput{Name: c.text(prefix+"name", in.Name, categoryNameMin, categoryNameMax), Tone: in.Tone}
	c.tone(prefix+"tone", in.Tone)

	return out
}

// check validates the body of a new model and returns the cleaned input.
func (in NewModel) check() (NewModel, error) {
	var c checker

	out := NewModel{Name: c.text("name", in.Name, modelNameMin, modelNameMax)}

	switch {
	case in.CategoryID != nil && in.NewCategory != nil, in.CategoryID == nil && in.NewCategory == nil:
		c.fail("categoryId", DetailRequired, "Send either categoryId or newCategory.")
	case in.CategoryID != nil:
		c.uuid("categoryId", *in.CategoryID)

		out.CategoryID = in.CategoryID
	default:
		cleaned := in.NewCategory.check("newCategory", &c)
		out.NewCategory = &cleaned
	}

	if in.Description != nil {
		text := c.optionalText("description", *in.Description, descriptionMax)
		out.Description = &text
	}

	switch {
	case in.Prefix == "":
		c.fail("prefix", DetailRequired, "This field is required.")
	case !prefixShape.MatchString(in.Prefix):
		c.fail("prefix", DetailInvalidFormat, "Use 2 to 6 capital letters or digits.")
	default:
		out.Prefix = in.Prefix
	}

	return out, c.err()
}

func (in ModelPatch) check() (ModelPatch, error) {
	var c checker

	out := ModelPatch{Retired: in.Retired}

	if in.Name == nil && in.CategoryID == nil && in.Description == nil && in.Retired == nil {
		c.fail("body", DetailRequired, "Send at least one field to change.")
	}

	if in.Name != nil {
		name := c.text("name", *in.Name, modelNameMin, modelNameMax)
		out.Name = &name
	}

	if in.CategoryID != nil {
		c.uuid("categoryId", *in.CategoryID)

		out.CategoryID = in.CategoryID
	}

	if in.Description != nil {
		text := c.optionalText("description", *in.Description, descriptionMax)
		out.Description = &text
	}

	return out, c.err()
}

func (in UnitInput) check() (UnitInput, error) {
	var c checker

	out := UnitInput{Count: in.Count}

	switch {
	case in.Count == nil:
		c.fail("count", DetailRequired, "This field is required.")
	case *in.Count < 1 || *in.Count > MaxUnitsPerRequest:
		c.fail("count", DetailOutOfRange, "Add 1 to "+strconv.Itoa(MaxUnitsPerRequest)+" units.")
	}

	out.Serial = c.serial("serial", in.Serial)
	if out.Serial != nil && in.Count != nil && *in.Count > 1 {
		c.fail("serial", DetailInvalidFormat, "Leave the serial number out when you add more than one unit.")
	}

	out.Condition = c.text("condition", in.Condition, textMin, textMax)
	out.Place = c.text("place", in.Place, textMin, textMax)

	return out, c.err()
}

func (in UnitPatch) check() (UnitPatch, error) {
	var c checker

	out := UnitPatch{SerialSet: in.SerialSet}

	if !in.SerialSet && in.Condition == nil && in.Place == nil {
		c.fail("body", DetailRequired, "Send at least one field to change.")
	}

	if in.SerialSet {
		out.Serial = c.serial("serial", in.Serial)
	}

	if in.Condition != nil {
		text := c.text("condition", *in.Condition, textMin, textMax)
		out.Condition = &text
	}

	if in.Place != nil {
		text := c.text("place", *in.Place, textMin, textMax)
		out.Place = &text
	}

	return out, c.err()
}

func (in LifecycleChange) check() (LifecycleChange, error) {
	var c checker

	out := LifecycleChange{Lifecycle: in.Lifecycle}

	switch {
	case in.Lifecycle == "":
		c.fail("lifecycle", DetailRequired, "This field is required.")
	case !slices.Contains(lifecycles, in.Lifecycle):
		c.fail("lifecycle", DetailInvalidFormat, "Use active, maintenance, lost or retired.")
	}

	needsNote := in.Lifecycle == LifecycleLost || in.Lifecycle == LifecycleRetired

	note := ""
	if in.Note != nil {
		note = c.optionalText("note", *in.Note, noteMax)
	}

	switch {
	case note != "":
		out.Note = &note
	case needsNote:
		c.fail("note", DetailRequired, "A note is required for this step.")
	}

	return out, c.err()
}

// check validates the filter of the list of models.
func (f ModelFilter) check() (ModelFilter, error) {
	var c checker

	f.Query = c.searchText(f.Query)
	c.optionalUUID("categoryId", f.CategoryID)

	return f, c.err()
}

// check validates the filter of the list of units.
func (f UnitFilter) check() (UnitFilter, error) {
	var c checker

	f.Query = c.searchText(f.Query)
	c.optionalUUID("modelId", f.ModelID)
	c.optionalUUID("categoryId", f.CategoryID)

	if f.Status != "" && !slices.Contains(statuses, f.Status) {
		c.fail("status", DetailInvalidFormat, "Use available, borrowed, maintenance, lost or retired.")
	}

	return f, c.err()
}

// ValidTag says whether the text has the shape of a unit tag. A text without it names no unit.
func ValidTag(tag string) bool {
	return tagShape.MatchString(tag)
}
