package admin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/password"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/secretbox"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/totp"
)

// totpIssuer is the name that an authenticator app shows for the account.
const totpIssuer = "LabMGM"

const (
	emailMax = 254
	nameMin  = 2
	nameMax  = 100
)

// Service holds the rules of the admin accounts. It takes the time from a function, so a test passes its own clock.
type Service struct {
	repo   Repository
	hasher *password.Hasher
	box    *secretbox.Box
	now    func() time.Time
}

// NewService returns a service. A nil clock means time.Now.
func NewService(repo Repository, hasher *password.Hasher, box *secretbox.Box, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}

	return &Service{repo: repo, hasher: hasher, box: box, now: now}
}

// SignInInput is what an admin sends to sign in. A code that is absent is an empty text.
type SignInInput struct {
	Email        string
	Password     string
	TOTPCode     string
	RecoveryCode string
}

// SignInResult says who signed in. Setup is true when the admin gets only a setup session, because TOTP is not enrolled or the
// initial password expired. UsedRecoveryCode is true when a recovery code was the second factor, which ends the other sessions.
type SignInResult struct {
	Account          Account
	Setup            bool
	UsedRecoveryCode bool
}

// SignIn checks the email, the password and the second factor. Every wrong or missing value gives ErrInvalidCredentials. An email
// that is not an account costs a password check against a dummy hash, so the time does not show which emails are accounts. A busy
// hasher gives password.ErrBusy, which is not a failed sign in.
func (s *Service) SignIn(ctx context.Context, in SignInInput) (SignInResult, error) {
	acct, err := s.repo.ByEmail(ctx, strings.ToLower(strings.TrimSpace(in.Email)))

	switch {
	case errors.Is(err, ErrNotFound):
		if err := s.hasher.VerifyDummy(ctx); err != nil {
			return SignInResult{}, err //nolint:wrapcheck // the caller tells a busy hasher from a failure
		}

		return SignInResult{}, ErrInvalidCredentials
	case err != nil:
		return SignInResult{}, fmt.Errorf("read the admin %w", err)
	}

	if err := s.checkPassword(ctx, acct, in.Password); err != nil {
		return SignInResult{}, err
	}

	var usedRecovery bool

	if acct.TOTPEnabled() {
		switch {
		case in.TOTPCode != "":
			err = s.checkTOTP(ctx, acct, in.TOTPCode)
		case in.RecoveryCode != "":
			err = s.useRecoveryCode(ctx, acct, in.RecoveryCode)
			usedRecovery = err == nil
		default:
			err = ErrInvalidCredentials
		}

		if err != nil {
			return SignInResult{}, err
		}
	}

	// the initial password stays in force until the admin sets a new one, so it never gives a full session, with a TOTP code or without
	initial := acct.PasswordExpiresAt != nil

	return SignInResult{Account: acct, Setup: !acct.TOTPEnabled() || initial, UsedRecoveryCode: usedRecovery}, nil
}

func (s *Service) load(ctx context.Context, id string) (Account, error) {
	acct, err := s.repo.ByID(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Account{}, ErrNotFound
	}

	if err != nil {
		return Account{}, fmt.Errorf("read the admin %w", err)
	}

	return acct, nil
}

func (s *Service) checkPassword(ctx context.Context, acct Account, typed string) error {
	ok, err := s.hasher.Verify(ctx, typed, acct.PasswordHash)
	if err != nil {
		return err //nolint:wrapcheck // the caller tells a busy hasher from a failure
	}

	if !ok {
		return ErrInvalidCredentials
	}

	return nil
}

func (s *Service) openSecret(acct Account) ([]byte, error) {
	secret, err := s.box.Open(acct.TOTPSecret, []byte(acct.ID))
	if err != nil {
		return nil, fmt.Errorf("open the TOTP secret %w", err)
	}

	return secret, nil
}

// checkTOTP accepts a code of the enrolled secret once. A code of a step that is not above the stored step is refused, also when
// two requests send it together, because the write that records the step is guarded.
func (s *Service) checkTOTP(ctx context.Context, acct Account, code string) error {
	secret, err := s.openSecret(acct)
	if err != nil {
		return err
	}

	step, ok := totp.Match(secret, code, s.now())
	if !ok {
		return ErrInvalidCredentials
	}

	accepted, err := s.repo.AcceptTOTPStep(ctx, acct.ID, step)
	if err != nil {
		return fmt.Errorf("record the TOTP step %w", err)
	}

	if !accepted {
		return ErrInvalidCredentials
	}

	return nil
}

func (s *Service) useRecoveryCode(ctx context.Context, acct Account, typed string) error {
	stored, err := s.repo.UnusedRecoveryHashes(ctx, acct.ID)
	if err != nil {
		return fmt.Errorf("read the recovery codes %w", err)
	}

	hash, ok := findHash(stored, HashRecoveryCode(typed))
	if !ok {
		return ErrInvalidCredentials
	}

	used, err := s.repo.UseRecoveryCode(ctx, acct.ID, hash, s.now())
	if err != nil {
		return fmt.Errorf("use the recovery code %w", err)
	}

	if !used {
		return ErrInvalidCredentials
	}

	return nil
}

// Profile is the own account of an admin as the route shows it.
type Profile struct {
	ID                string
	Name              string
	Email             string
	TOTPEnabled       bool
	PasswordExpiresAt *time.Time
	RecoveryCodesLeft int
}

// Profile reads the account. It returns ErrNotFound for an admin that is gone.
func (s *Service) Profile(ctx context.Context, id string) (Profile, error) {
	acct, err := s.load(ctx, id)
	if err != nil {
		return Profile{}, err
	}

	left, err := s.repo.UnusedRecoveryHashes(ctx, id)
	if err != nil {
		return Profile{}, fmt.Errorf("read the recovery codes %w", err)
	}

	return Profile{
		ID: acct.ID, Name: acct.Name, Email: acct.Email, TOTPEnabled: acct.TOTPEnabled(),
		PasswordExpiresAt: acct.PasswordExpiresAt, RecoveryCodesLeft: len(left),
	}, nil
}

// ChangePasswordInput is a change of the password. RequireTOTP is true for a full admin session, which also sends a TOTP code. A
// setup session has none yet.
type ChangePasswordInput struct {
	Current     string
	New         string
	TOTPCode    string
	RequireTOTP bool
}

// ChangePassword checks the current password and the code, stores the new hash, and ends every other session of the admin. keep is
// the hash of the token of the session that asked, and it stays.
func (s *Service) ChangePassword(ctx context.Context, id string, in ChangePasswordInput, keep []byte) error {
	acct, err := s.load(ctx, id)
	if err != nil {
		return err
	}

	if err := s.checkPassword(ctx, acct, in.Current); err != nil {
		return err
	}

	if in.RequireTOTP {
		if !acct.TOTPEnabled() {
			return ErrInvalidCredentials
		}

		if err := s.checkTOTP(ctx, acct, in.TOTPCode); err != nil {
			return err
		}
	}

	hash, err := s.hasher.Hash(ctx, in.New)
	if err != nil {
		return err //nolint:wrapcheck // the caller tells a busy hasher from a failure
	}

	if err := s.repo.SetPassword(ctx, id, hash, keep); err != nil {
		return fmt.Errorf("store the password %w", err)
	}

	return nil
}

// EnrollInput is a call of the two step enrollment of TOTP. TOTPCode is the code of the current secret, which an admin who has TOTP
// already must send. ConfirmCode is the code of the new secret and is sent in the second call only.
type EnrollInput struct {
	Password    string
	TOTPCode    string
	ConfirmCode string
}

// Enrollment is the new secret that the first call shows.
type Enrollment struct {
	Secret string
	URI    string
}

// pendingSecret is the secret of an enrollment that is not finished. The service stores nothing until the second call, so the first
// call derives the secret from the key of the service, the admin, the session and the time of the last enrollment. The second call
// derives the same value and checks the confirm code against it. A finished enrollment changes the time, so the next one gets another
// secret, and a session that never finishes leaves nothing behind.
func (s *Service) pendingSecret(acct Account, session []byte) []byte {
	enrolled := "none"
	if acct.TOTPEnabledAt != nil {
		enrolled = strconv.FormatInt(acct.TOTPEnabledAt.UnixMicro(), 10)
	}

	return s.box.Derive(totp.SecretLength, []byte("totp enrollment"), []byte(acct.ID), session, []byte(enrolled))
}

func (s *Service) checkEnrollment(ctx context.Context, id string, in EnrollInput) (Account, error) {
	acct, err := s.load(ctx, id)
	if err != nil {
		return Account{}, err
	}

	if err := s.checkPassword(ctx, acct, in.Password); err != nil {
		return Account{}, err
	}

	if acct.TOTPEnabled() {
		if err := s.checkTOTP(ctx, acct, in.TOTPCode); err != nil {
			return Account{}, err
		}
	}

	return acct, nil
}

// StartTOTP checks the password, and the code when TOTP is enrolled already, and returns the secret of the new enrollment. session is
// the hash of the token of the session that asks.
func (s *Service) StartTOTP(ctx context.Context, id string, session []byte, in EnrollInput) (Enrollment, error) {
	acct, err := s.checkEnrollment(ctx, id, in)
	if err != nil {
		return Enrollment{}, err
	}

	secret := s.pendingSecret(acct, session)

	return Enrollment{Secret: totp.EncodeSecret(secret), URI: totp.URI(totpIssuer, acct.Email, secret)}, nil
}

// FinishTOTP checks the same values as StartTOTP and the confirm code of the new secret. It stores the secret sealed, replaces the
// recovery codes and returns the new codes in clear. They are shown once.
func (s *Service) FinishTOTP(ctx context.Context, id string, session []byte, in EnrollInput) ([]string, error) {
	acct, err := s.checkEnrollment(ctx, id, in)
	if err != nil {
		return nil, err
	}

	secret := s.pendingSecret(acct, session)

	step, ok := totp.Match(secret, in.ConfirmCode, s.now())
	if !ok {
		return nil, ErrInvalidCredentials
	}

	codes, hashes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, err
	}

	sealed, err := s.box.Seal(secret, []byte(acct.ID))
	if err != nil {
		return nil, fmt.Errorf("seal the TOTP secret %w", err)
	}

	stored, err := s.repo.EnableTOTP(ctx, id, acct.TOTPEnabledAt, sealed, s.now(), step, hashes)
	if err != nil {
		return nil, fmt.Errorf("store the TOTP secret %w", err)
	}

	// another call finished the enrollment first, so the secret that this call checked is not the pending one any more
	if !stored {
		return nil, ErrInvalidCredentials
	}

	return codes, nil
}

// RenewInput is a request for new recovery codes.
type RenewInput struct {
	Password string
	TOTPCode string
}

// RenewRecoveryCodes checks the password and a TOTP code, and replaces every recovery code. The new codes are shown once.
func (s *Service) RenewRecoveryCodes(ctx context.Context, id string, in RenewInput) ([]string, error) {
	acct, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := s.checkPassword(ctx, acct, in.Password); err != nil {
		return nil, err
	}

	if !acct.TOTPEnabled() {
		return nil, ErrInvalidCredentials
	}

	if err := s.checkTOTP(ctx, acct, in.TOTPCode); err != nil {
		return nil, err
	}

	codes, hashes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, err
	}

	if err := s.repo.ReplaceRecoveryCodes(ctx, id, hashes); err != nil {
		return nil, fmt.Errorf("replace the recovery codes %w", err)
	}

	return codes, nil
}

// CreateInput is the data of a new admin. Password is the one use initial password.
type CreateInput struct {
	Email    string
	Name     string
	Password string
}

// Create makes an admin whose initial password expires after InitialPasswordLifetime. The first sign in therefore gets a setup
// session, and the admin sets a password and TOTP of their own. It returns the id and the email as stored. A taken email gives
// ErrDuplicate, and a value that breaks a rule gives an InvalidInputError.
func (s *Service) Create(ctx context.Context, in CreateInput) (string, string, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !plausibleEmail(email) {
		return "", "", &InvalidInputError{Field: "email", Problem: "is not an email address of at most 254 characters"}
	}

	name := strings.TrimSpace(in.Name)
	if length := utf8.RuneCountInString(name); length < nameMin || length > nameMax || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", "", &InvalidInputError{Field: "name", Problem: "needs 2 to 100 characters and no control character"}
	}

	if problem := password.CheckNew(in.Password, ""); problem != "" {
		return "", "", &InvalidInputError{Field: "password", Problem: "needs " + strconv.Itoa(password.MinLength) + " to " + strconv.Itoa(password.MaxLength) + " characters"}
	}

	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return "", "", fmt.Errorf("hash the password %w", err)
	}

	expires := s.now().Add(InitialPasswordLifetime)

	id, err := s.repo.Insert(ctx, Account{Email: email, Name: name, PasswordHash: hash, PasswordExpiresAt: &expires})
	if err != nil {
		return "", "", err //nolint:wrapcheck // the caller tells a taken email from a failure
	}

	return id, email, nil
}

// ResetTOTP removes the second factor of the admin with the email, together with the recovery codes and every session. It is for
// a lost phone or a changed key. The next sign in with the password gets a setup session that enrolls a new factor. It returns the id.
func (s *Service) ResetTOTP(ctx context.Context, email string) (string, error) {
	acct, err := s.repo.ByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if errors.Is(err, ErrNotFound) {
		return "", ErrNotFound
	}

	if err != nil {
		return "", fmt.Errorf("read the admin %w", err)
	}

	if err := s.repo.ResetTOTP(ctx, acct.ID); err != nil {
		return "", fmt.Errorf("reset the second factor %w", err)
	}

	return acct.ID, nil
}

// plausibleEmail checks the shape only. The admin gets no mail from the service, so nothing proves that the address exists.
func plausibleEmail(email string) bool {
	local, domain, found := strings.Cut(email, "@")

	return found && utf8.RuneCountInString(email) <= emailMax && local != "" && strings.Contains(domain, ".") &&
		!strings.Contains(domain, "@") && !strings.ContainsFunc(email, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
