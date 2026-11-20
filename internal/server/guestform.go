package server

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

// The detail codes of the contract that the guest form uses.
const (
	detailRequired      = "required"
	detailInvalidFormat = "invalid_format"
	detailTooShort      = "too_short"
	detailTooLong       = "too_long"
	detailNotUBEmail    = "not_ub_email"
)

const (
	nameMin   = 3
	nameMax   = 100
	nimMin    = 8
	nimMax    = 20
	emailMax  = 254
	ubDomain  = "ub.ac.id"
	ubSubZone = "." + ubDomain
)

// textField is a JSON value that the form reads as text. A value of another JSON type does not fail the whole body, it only marks the
// field, so the answer can name the field with the code invalid_format.
type textField struct {
	value   string
	present bool
	badType bool
}

func (f *textField) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))

	switch {
	case trimmed == "null":
		return nil
	case strings.HasPrefix(trimmed, `"`):
		if err := json.Unmarshal(data, &f.value); err != nil {
			return err //nolint:wrapcheck // the error of the JSON package is the answer to a body that is not valid JSON
		}

		f.present = true
	default:
		f.present = true
		f.badType = true
	}

	return nil
}

// filled says whether the hidden field of the form holds anything. A value of the wrong type counts as filled.
func (f textField) filled() bool {
	return f.badType || f.value != ""
}

// guestForm is the body of POST /api/session/guest. A field that the contract does not name is ignored.
type guestForm struct {
	Name    textField `json:"name"`
	NimNip  textField `json:"nimNip"`
	Email   textField `json:"email"`
	Website textField `json:"website"`
}

// parseGuestForm reads the body. The error means that the body is not valid JSON of an object.
func parseGuestForm(body []byte) (guestForm, error) {
	var form guestForm
	if err := json.Unmarshal(body, &form); err != nil {
		return guestForm{}, err //nolint:wrapcheck // the caller answers bad_request and never shows the text
	}

	return form, nil
}

// identity checks every field and returns the identity with the email in lower case, or one detail for each failing field.
func (f guestForm) identity() (session.Identity, []Detail) {
	var details []Detail

	name, nameDetail := checkName(f.Name)
	if nameDetail != nil {
		details = append(details, *nameDetail)
	}

	nim, nimDetail := checkNimNip(f.NimNip)
	if nimDetail != nil {
		details = append(details, *nimDetail)
	}

	email, emailDetail := checkEmail(f.Email)
	if emailDetail != nil {
		details = append(details, *emailDetail)
	}

	return session.Identity{Name: name, NimNip: nim, Email: email}, details
}

func detail(field, code, message string) *Detail {
	return &Detail{Field: field, Code: code, Message: message}
}

// missing returns the detail of a field that is absent, empty or of the wrong type. The second value is false for a usable text.
func missing(field string, f textField, text string) (*Detail, bool) {
	switch {
	case f.badType:
		return detail(field, detailInvalidFormat, "Send this field as text."), true
	case text == "":
		return detail(field, detailRequired, "This field is required."), true
	default:
		return nil, false
	}
}

func checkName(f textField) (string, *Detail) {
	name := strings.TrimSpace(f.value)
	if d, bad := missing("name", f, name); bad {
		return "", d
	}

	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", detail("name", detailInvalidFormat, "Use letters and spaces without control characters.")
	}

	switch length := utf8.RuneCountInString(name); {
	case length < nameMin:
		return "", detail("name", detailTooShort, "Use at least 3 characters.")
	case length > nameMax:
		return "", detail("name", detailTooLong, "Use at most 100 characters.")
	}

	return name, nil
}

func checkNimNip(f textField) (string, *Detail) {
	nim := strings.TrimSpace(f.value)
	if d, bad := missing("nimNip", f, nim); bad {
		return "", d
	}

	if len(nim) < nimMin || len(nim) > nimMax || strings.IndexFunc(nim, notLetterOrDigit) >= 0 {
		return "", detail("nimNip", detailInvalidFormat, "Use 8 to 20 letters or digits.")
	}

	return nim, nil
}

// notLetterOrDigit is true for everything except ASCII letters and digits, as the pattern of the contract says.
func notLetterOrDigit(r rune) bool {
	return (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z')
}

func checkEmail(f textField) (string, *Detail) {
	email := strings.ToLower(strings.TrimSpace(f.value))
	if d, bad := missing("email", f, email); bad {
		return "", d
	}

	if len(email) > emailMax {
		return "", detail("email", detailTooLong, "Use at most 254 characters.")
	}

	local, domain, found := strings.Cut(email, "@")
	if !found || local == "" || strings.ContainsFunc(local, isSpaceOrControl) || strings.Contains(domain, "@") || !validDomain(domain) {
		return "", detail("email", detailInvalidFormat, "Use an email address such as name@student.ub.ac.id.")
	}

	if domain != ubDomain && !strings.HasSuffix(domain, ubSubZone) {
		return "", detail("email", detailNotUBEmail, "Use an email of Universitas Brawijaya.")
	}

	return email, nil
}

func isSpaceOrControl(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r)
}

// validDomain accepts dot separated labels of letters, digits and hyphens, with at least two labels and no empty label.
func validDomain(domain string) bool {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}

	for _, label := range labels {
		if label == "" || strings.IndexFunc(label, notLabelRune) >= 0 {
			return false
		}
	}

	return true
}

func notLabelRune(r rune) bool {
	return r != '-' && notLetterOrDigit(r)
}
