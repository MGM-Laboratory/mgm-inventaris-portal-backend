package password

import "unicode/utf8"

// The length bounds of a new password, in characters, as the contract says.
const (
	MinLength = 12
	MaxLength = 128
)

// The detail codes of a refused new password. They are codes of the contract.
const (
	ProblemTooShort  = "too_short"
	ProblemTooLong   = "too_long"
	ProblemUnchanged = "duplicate"
)

// CheckNew returns the detail code of a new password that breaks a rule, and an empty text when it passes. current is the password
// that it replaces, and an empty current skips the comparison.
func CheckNew(candidate, current string) string {
	switch length := utf8.RuneCountInString(candidate); {
	case length < MinLength:
		return ProblemTooShort
	case length > MaxLength:
		return ProblemTooLong
	case current != "" && candidate == current:
		return ProblemUnchanged
	default:
		return ""
	}
}
