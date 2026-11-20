// Package admin owns the tables admins and admin_recovery_codes. It checks the password, the TOTP code and the recovery codes of an
// admin, and it creates the admin accounts. It never writes a session, a cookie or an answer.
package admin

import (
	"context"
	"errors"
	"time"
)

// InitialPasswordLifetime is how long the one use initial password of a new admin works, as docs/data-model.md says.
const InitialPasswordLifetime = 24 * time.Hour

var (
	// ErrInvalidCredentials means that an email, a password or a code is wrong. It never says which one.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrNotFound means that no admin has the email or the id.
	ErrNotFound = errors.New("no such admin")
	// ErrDuplicate means that an admin with the email exists already.
	ErrDuplicate = errors.New("the email is in use")
)

// InvalidInputError says that a value of a new admin breaks a rule. It names the field and the rule and holds no value.
type InvalidInputError struct {
	Field   string
	Problem string
}

func (e *InvalidInputError) Error() string {
	return "the " + e.Field + " " + e.Problem
}

// Account is a row of the table admins.
type Account struct {
	ID                string
	Email             string
	Name              string
	PasswordHash      string
	PasswordExpiresAt *time.Time
	// TOTPSecret is the secret in its sealed form, and empty until TOTP is enrolled.
	TOTPSecret    string
	TOTPEnabledAt *time.Time
	TOTPLastStep  *int64
}

// TOTPEnabled says whether the admin finished the enrollment of TOTP.
func (a Account) TOTPEnabled() bool {
	return a.TOTPEnabledAt != nil
}

// Repository is the storage of the admin accounts. The PostgreSQL version is Postgres, and admintest has one in memory.
type Repository interface {
	// ByEmail and ByID return ErrNotFound for a missing admin. The email has to be in lower case.
	ByEmail(ctx context.Context, email string) (Account, error)
	ByID(ctx context.Context, id string) (Account, error)
	// Insert stores a new admin and returns its id, or ErrDuplicate.
	Insert(ctx context.Context, account Account) (string, error)

	// AcceptTOTPStep records a time step as the last used one. It returns false, and changes nothing, when the stored step is the
	// same or later, so a code is never accepted twice. The check and the write are one statement.
	AcceptTOTPStep(ctx context.Context, id string, step int64) (bool, error)
	// EnableTOTP stores a new secret, marks TOTP as enrolled at the given time, sets the last used step and replaces the recovery
	// codes with the given hashes, all in one transaction. It runs only while the stored enrollment time still equals expected,
	// which is nil for a first enrollment. It returns false and changes nothing otherwise.
	EnableTOTP(ctx context.Context, id string, expected *time.Time, sealedSecret string, at time.Time, step int64, codeHashes []string) (bool, error)

	// SetPassword stores a new hash, clears the expiry of the initial password and ends every session of the admin except the one
	// with the hash keep, in one transaction.
	SetPassword(ctx context.Context, id, hash string, keep []byte) error

	// UnusedRecoveryHashes lists the hashes of the codes that are not used.
	UnusedRecoveryHashes(ctx context.Context, id string) ([]string, error)
	// UseRecoveryCode marks one code as used. It returns false when the code is used already, so a code works once even when two
	// requests send it together. The check and the write are one statement.
	UseRecoveryCode(ctx context.Context, id, hash string, at time.Time) (bool, error)
	// ReplaceRecoveryCodes deletes every code of the admin and stores the new hashes in one transaction.
	ReplaceRecoveryCodes(ctx context.Context, id string, hashes []string) error
}
