package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/password"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

// AdminService is the part of the admin package that the routes use.
type AdminService interface {
	SignIn(ctx context.Context, in admin.SignInInput) (admin.SignInResult, error)
	Profile(ctx context.Context, id string) (admin.Profile, error)
	ChangePassword(ctx context.Context, id string, in admin.ChangePasswordInput, keep []byte) error
	StartTOTP(ctx context.Context, id string, session []byte, in admin.EnrollInput) (admin.Enrollment, error)
	FinishTOTP(ctx context.Context, id string, session []byte, in admin.EnrollInput) ([]string, error)
	RenewRecoveryCodes(ctx context.Context, id string, in admin.RenewInput) ([]string, error)
}

const (
	detailDuplicate = "duplicate"

	passwordMax     = 128
	recoveryCodeMax = 64
	totpCodeLength  = 6

	signInMessage  = "The email, the password or the code is wrong."
	accountMessage = "The password or the code is wrong."
)

// RequireAdminAccount needs an admin session or a setup session. The routes of the setup use it, and every other admin route uses
// RequireAdmin.
func RequireAdminAccount() fiber.Handler {
	return func(c fiber.Ctx) error {
		sess, err := requireSession(c)
		if err != nil {
			return err
		}

		if sess.Kind != session.KindAdmin && sess.Kind != session.KindAdminSetup {
			return NewError(CodeForbidden)
		}

		return c.Next()
	}
}

// decodeBody reads a JSON object into v. Any failure is the answer bad_request, and its text never reaches the client.
func decodeBody(c fiber.Ctx, v any) error {
	if err := json.Unmarshal(c.Body(), v); err != nil {
		return badBody()
	}

	return nil
}

func isCodeShape(code string) bool {
	if len(code) != totpCodeLength {
		return false
	}

	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// checkPasswordField validates a password that is only compared, so its length has an upper bound and no lower one.
func checkPasswordField(field, value string) *Detail {
	switch {
	case value == "":
		return detail(field, detailRequired, "This field is required.")
	case utf8.RuneCountInString(value) > passwordMax:
		return detail(field, detailTooLong, "Use at most 128 characters.")
	default:
		return nil
	}
}

// checkCodeField validates a TOTP code. An empty code is absent, and the field is required only when required is true.
func checkCodeField(field, value string, required bool) *Detail {
	switch {
	case value == "" && required:
		return detail(field, detailRequired, "This field is required.")
	case value != "" && !isCodeShape(value):
		return detail(field, detailInvalidFormat, "Use the 6 digits of the code.")
	default:
		return nil
	}
}

func failIfAny(candidates ...*Detail) error {
	var details []Detail

	for _, d := range candidates {
		if d != nil {
			details = append(details, *d)
		}
	}

	if len(details) == 0 {
		return nil
	}

	return NewError(CodeValidationFailed, details...)
}

type adminSignInForm struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	TOTPCode     string `json:"totpCode"`
	RecoveryCode string `json:"recoveryCode"`
}

func (f adminSignInForm) check() error {
	var emailDetail *Detail

	switch email := strings.TrimSpace(f.Email); {
	case email == "":
		emailDetail = detail("email", detailRequired, "This field is required.")
	case utf8.RuneCountInString(email) > emailMax:
		emailDetail = detail("email", detailTooLong, "Use at most 254 characters.")
	case !strings.Contains(email, "@"):
		emailDetail = detail("email", detailInvalidFormat, "Use an email address.")
	}

	var recoveryDetail *Detail
	if len(f.RecoveryCode) > recoveryCodeMax {
		recoveryDetail = detail("recoveryCode", detailTooLong, "Use at most 64 characters.")
	}

	return failIfAny(emailDetail, checkPasswordField("password", f.Password), checkCodeField("totpCode", f.TOTPCode, false), recoveryDetail)
}

// signInAdmin is the route signInAdmin. The counters of the limits hold failures only. The request hits them first and refunds the
// hits when the sign in does not fail, and it never counts a request that the body check refused.
func signInAdmin(cfg config.Config, deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form adminSignInForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		if err := form.check(); err != nil {
			return err
		}

		email := strings.ToLower(strings.TrimSpace(form.Email))
		addr := clientAddr(c, cfg.TrustedProxies)
		emailHash := sha256.Sum256([]byte(email))

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		held, err := reserve(ctx, deps.Limiter,
			limitSpec{"admin-signin-pair:" + addr + ":" + hex.EncodeToString(emailHash[:]), cfg.AdminSigninPairFailures, adminFailureWindow},
			limitSpec{"admin-signin-address:" + addr, cfg.AdminSigninAddressFailures, adminAddressWindow})
		if err != nil {
			return err
		}

		result, err := deps.Admins.SignIn(ctx, admin.SignInInput{
			Email: email, Password: form.Password, TOTPCode: form.TOTPCode, RecoveryCode: form.RecoveryCode,
		})

		held.settle(ctx, err)

		if err != nil {
			return adminError("sign in the admin", signInMessage, err)
		}

		kind := session.KindAdmin
		if result.Setup {
			kind = session.KindAdminSetup
		}

		var previous []byte
		if current := currentSession(c); current != nil {
			previous = current.TokenHash
		}

		who := session.Admin{ID: result.Account.ID, Name: result.Account.Name, Email: result.Account.Email}

		token, created, err := deps.Sessions.CreateAdmin(ctx, who, kind, previous, result.UsedRecoveryCode)
		if err != nil {
			return dbError("create the admin session", err)
		}

		limits, _ := session.LimitsFor(kind)
		setSessionCookie(c, cfg.SecureCookie, token, limits.Absolute)

		return c.Status(http.StatusCreated).JSON(newSessionBody(created))
	}
}

type adminProfileBody struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Email             string  `json:"email"`
	TOTPEnabled       bool    `json:"totpEnabled"`
	PasswordExpiresAt *string `json:"passwordExpiresAt"`
	RecoveryCodesLeft int     `json:"recoveryCodesLeft"`
}

// getAdminMe is the route getAdminMe.
func getAdminMe(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		profile, err := deps.Admins.Profile(ctx, currentSession(c).Admin.ID)
		if err != nil {
			return adminError("read the admin profile", accountMessage, err)
		}

		body := adminProfileBody{
			ID: profile.ID, Name: profile.Name, Email: profile.Email,
			TOTPEnabled: profile.TOTPEnabled, RecoveryCodesLeft: profile.RecoveryCodesLeft,
		}

		if profile.PasswordExpiresAt != nil {
			expires := timeBody(*profile.PasswordExpiresAt)
			body.PasswordExpiresAt = &expires
		}

		return c.JSON(body)
	}
}

// accountKey is the key of the counter of the failed checks of one session. It holds the hash of the token, as the table does.
func accountKey(sess *session.Session) string {
	return "admin-account:" + hex.EncodeToString(sess.TokenHash)
}

// accountRoute is what the three account routes that check a password share. The body check comes first and counts nothing. Then the
// request hits the counter of its session, runs, and keeps the hit only when a credential was wrong. run returns the error of the
// admin service as it is, and the body of a 200 answer, or nil for a 204 answer. what names the call for the log.
func accountRoute(cfg config.Config, deps Deps, what string, run func(ctx context.Context, sess *session.Session) (any, error)) fiber.Handler {
	return func(c fiber.Ctx) error {
		sess := currentSession(c)

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		held, err := reserve(ctx, deps.Limiter, limitSpec{accountKey(sess), cfg.AdminAccountFailures, adminFailureWindow})
		if err != nil {
			return err
		}

		body, err := run(ctx, sess)

		held.settle(ctx, err)

		if err != nil {
			return adminError(what, accountMessage, err)
		}

		if body == nil {
			return c.SendStatus(http.StatusNoContent)
		}

		return c.JSON(body)
	}
}

type passwordChangeForm struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
	TOTPCode        string `json:"totpCode"`
}

func (f passwordChangeForm) check() error {
	var newDetail *Detail

	switch password.CheckNew(f.NewPassword, f.CurrentPassword) {
	case password.ProblemTooShort:
		if f.NewPassword == "" {
			newDetail = detail("newPassword", detailRequired, "This field is required.")
		} else {
			newDetail = detail("newPassword", detailTooShort, "Use at least 12 characters.")
		}
	case password.ProblemTooLong:
		newDetail = detail("newPassword", detailTooLong, "Use at most 128 characters.")
	case password.ProblemUnchanged:
		newDetail = detail("newPassword", detailDuplicate, "Choose a password that differs from the current one.")
	}

	return failIfAny(checkPasswordField("currentPassword", f.CurrentPassword), newDetail, checkCodeField("totpCode", f.TOTPCode, false))
}

// changeAdminPassword is the route changeAdminPassword.
func changeAdminPassword(cfg config.Config, deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form passwordChangeForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		if err := form.check(); err != nil {
			return err
		}

		return accountRoute(cfg, deps, "change the admin password", func(ctx context.Context, sess *session.Session) (any, error) {
			return nil, deps.Admins.ChangePassword(ctx, sess.Admin.ID, admin.ChangePasswordInput{
				Current: form.CurrentPassword, New: form.NewPassword, TOTPCode: form.TOTPCode, RequireTOTP: sess.Kind == session.KindAdmin,
			}, sess.TokenHash)
		})(c)
	}
}

type totpEnrollForm struct {
	CurrentPassword string `json:"currentPassword"`
	TOTPCode        string `json:"totpCode"`
	ConfirmCode     string `json:"confirmCode"`
}

func (f totpEnrollForm) check() error {
	return failIfAny(checkPasswordField("currentPassword", f.CurrentPassword), checkCodeField("totpCode", f.TOTPCode, false),
		checkCodeField("confirmCode", f.ConfirmCode, false))
}

type totpEnrollmentBody struct {
	Secret     string `json:"secret"`
	OtpauthURI string `json:"otpauthUri"`
}

type recoveryCodesBody struct {
	RecoveryCodes []string `json:"recoveryCodes"`
}

// enrollAdminTotp is the route enrollAdminTotp. A call without confirmCode starts the enrollment, and a call with it finishes it.
func enrollAdminTotp(cfg config.Config, deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form totpEnrollForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		if err := form.check(); err != nil {
			return err
		}

		return accountRoute(cfg, deps, "enroll TOTP", func(ctx context.Context, sess *session.Session) (any, error) {
			in := admin.EnrollInput{Password: form.CurrentPassword, TOTPCode: form.TOTPCode, ConfirmCode: form.ConfirmCode}

			if form.ConfirmCode == "" {
				started, err := deps.Admins.StartTOTP(ctx, sess.Admin.ID, sess.TokenHash, in)
				if err != nil {
					return nil, err //nolint:wrapcheck // accountRoute maps the error of the service
				}

				return totpEnrollmentBody{Secret: started.Secret, OtpauthURI: started.URI}, nil
			}

			codes, err := deps.Admins.FinishTOTP(ctx, sess.Admin.ID, sess.TokenHash, in)
			if err != nil {
				return nil, err //nolint:wrapcheck // accountRoute maps the error of the service
			}

			return recoveryCodesBody{RecoveryCodes: codes}, nil
		})(c)
	}
}

type recoveryRenewForm struct {
	CurrentPassword string `json:"currentPassword"`
	TOTPCode        string `json:"totpCode"`
}

func (f recoveryRenewForm) check() error {
	return failIfAny(checkPasswordField("currentPassword", f.CurrentPassword), checkCodeField("totpCode", f.TOTPCode, true))
}

// renewAdminRecoveryCodes is the route renewAdminRecoveryCodes.
func renewAdminRecoveryCodes(cfg config.Config, deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form recoveryRenewForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		if err := form.check(); err != nil {
			return err
		}

		return accountRoute(cfg, deps, "renew the recovery codes", func(ctx context.Context, sess *session.Session) (any, error) {
			codes, err := deps.Admins.RenewRecoveryCodes(ctx, sess.Admin.ID, admin.RenewInput{Password: form.CurrentPassword, TOTPCode: form.TOTPCode})
			if err != nil {
				return nil, err //nolint:wrapcheck // accountRoute maps the error of the service
			}

			return recoveryCodesBody{RecoveryCodes: codes}, nil
		})(c)
	}
}
