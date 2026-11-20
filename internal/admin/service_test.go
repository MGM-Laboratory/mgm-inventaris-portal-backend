package admin_test

import (
	"context"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin/admintest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/password"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/secretbox"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/totp"
)

const (
	initialPassword = "example initial password"
	ownPassword     = "example own password"
	sessionOne      = "session one"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

type fixture struct {
	repo  *admintest.Memory
	svc   *admin.Service
	box   *secretbox.Box
	clock *clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	box, err := secretbox.New([]byte(strings.Repeat("0123456789abcdef", 2)))
	if err != nil {
		t.Fatalf("make the box %v", err)
	}

	clk := &clock{now: time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)}
	repo := admintest.NewMemory()

	return &fixture{
		repo: repo, box: box, clock: clk,
		svc: admin.NewService(repo, password.New(2, 5*time.Second), box, clk.Now),
	}
}

// create makes an admin with the initial password and returns its id.
func (f *fixture) create(t *testing.T, email string) string {
	t.Helper()

	id, _, err := f.svc.Create(context.Background(), admin.CreateInput{Email: email, Name: "Example Admin", Password: initialPassword})
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	return id
}

// enrolled is an admin with TOTP, made through the real two step enrollment. The returned secret makes the codes.
type enrolled struct {
	id     string
	secret []byte
	codes  []string
}

// code returns a valid code that is in a later step than any code so far, so the replay guard accepts it.
func (f *fixture) code(e enrolled) string {
	f.clock.advance(totp.StepSeconds * time.Second)

	return totp.Code(e.secret, totp.Step(f.clock.Now()))
}

func (f *fixture) enroll(t *testing.T, email string) enrolled {
	t.Helper()

	ctx := context.Background()
	id := f.create(t, email)
	in := admin.EnrollInput{Password: initialPassword}

	started, err := f.svc.StartTOTP(ctx, id, []byte(sessionOne), in)
	if err != nil {
		t.Fatalf("start failed %v", err)
	}

	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(started.Secret)
	if err != nil {
		t.Fatalf("the secret is not Base32 %v", err)
	}

	in.ConfirmCode = totp.Code(secret, totp.Step(f.clock.Now()))

	codes, err := f.svc.FinishTOTP(ctx, id, []byte(sessionOne), in)
	if err != nil {
		t.Fatalf("finish failed %v", err)
	}

	return enrolled{id: id, secret: secret, codes: codes}
}

func TestCreate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	id, email, err := f.svc.Create(ctx, admin.CreateInput{Email: "  Admin@Example.ORG ", Name: " Example Admin ", Password: initialPassword})
	if err != nil || id == "" || email != "admin@example.org" {
		t.Fatalf("create gave %q, %q and %v", id, email, err)
	}

	stored, err := f.repo.ByID(ctx, id)
	if err != nil {
		t.Fatalf("read failed %v", err)
	}

	if stored.Name != "Example Admin" || stored.TOTPEnabled() || strings.Contains(stored.PasswordHash, initialPassword) ||
		!strings.HasPrefix(stored.PasswordHash, "$argon2id$") {
		t.Fatalf("the stored admin is %+v", stored)
	}

	// the initial password lives for 24 hours, as docs/data-model.md says
	if stored.PasswordExpiresAt == nil || !stored.PasswordExpiresAt.Equal(f.clock.Now().Add(24*time.Hour)) {
		t.Fatalf("the password expires at %v", stored.PasswordExpiresAt)
	}

	_, _, err = f.svc.Create(ctx, admin.CreateInput{Email: "ADMIN@example.org", Name: "Another Admin", Password: initialPassword})
	if !errors.Is(err, admin.ErrDuplicate) {
		t.Fatalf("a taken email gave %v", err)
	}
}

func TestCreateRefusesBadValues(t *testing.T) {
	f := newFixture(t)

	tests := []struct {
		name  string
		in    admin.CreateInput
		field string
	}{
		{"no email", admin.CreateInput{Name: "Example Admin", Password: initialPassword}, "email"},
		{"no at sign", admin.CreateInput{Email: "admin.example.org", Name: "Example Admin", Password: initialPassword}, "email"},
		{"no domain dot", admin.CreateInput{Email: "admin@example", Name: "Example Admin", Password: initialPassword}, "email"},
		{"space in email", admin.CreateInput{Email: "ad min@example.org", Name: "Example Admin", Password: initialPassword}, "email"},
		{"long email", admin.CreateInput{Email: strings.Repeat("a", 250) + "@example.org", Name: "Example Admin", Password: initialPassword}, "email"},
		{"short name", admin.CreateInput{Email: "admin@example.org", Name: "A", Password: initialPassword}, "name"},
		{"control in name", admin.CreateInput{Email: "admin@example.org", Name: "Example\nAdmin", Password: initialPassword}, "name"},
		{"short password", admin.CreateInput{Email: "admin@example.org", Name: "Example Admin", Password: "too short"}, "password"},
		{"long password", admin.CreateInput{Email: "admin@example.org", Name: "Example Admin", Password: strings.Repeat("a", 129)}, "password"},
	}

	for _, tc := range tests {
		_, _, err := f.svc.Create(context.Background(), tc.in)

		var invalid *admin.InvalidInputError
		if !errors.As(err, &invalid) || invalid.Field != tc.field {
			t.Fatalf("%s gave %v", tc.name, err)
		}

		if strings.Contains(err.Error(), initialPassword) || strings.Contains(err.Error(), "admin@") {
			t.Fatalf("%s shows a value in %q", tc.name, err)
		}
	}
}

func TestSignInWithoutTOTPGivesASetupSession(t *testing.T) {
	f := newFixture(t)
	f.create(t, "admin@example.org")

	got, err := f.svc.SignIn(context.Background(), admin.SignInInput{Email: "Admin@example.org", Password: initialPassword})
	if err != nil || !got.Setup || got.UsedRecoveryCode || got.Account.Email != "admin@example.org" {
		t.Fatalf("sign in gave %+v and %v", got, err)
	}
}

func TestSignInWithTOTP(t *testing.T) {
	f := newFixture(t)
	e := f.enroll(t, "admin@example.org")
	ctx := context.Background()

	// the password is the initial one, which expires after a day. Setting an own password clears the expiry.
	if err := f.svc.ChangePassword(ctx, e.id, admin.ChangePasswordInput{Current: initialPassword, New: ownPassword, TOTPCode: f.code(e), RequireTOTP: true}, nil); err != nil {
		t.Fatalf("change failed %v", err)
	}

	got, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: ownPassword, TOTPCode: f.code(e)})
	if err != nil || got.Setup || got.UsedRecoveryCode {
		t.Fatalf("sign in gave %+v and %v", got, err)
	}
}

func TestSignInRefusesEveryWrongValueTheSameWay(t *testing.T) {
	f := newFixture(t)
	e := f.enroll(t, "admin@example.org")
	ctx := context.Background()

	if err := f.svc.ChangePassword(ctx, e.id, admin.ChangePasswordInput{Current: initialPassword, New: ownPassword, TOTPCode: f.code(e), RequireTOTP: true}, nil); err != nil {
		t.Fatalf("change failed %v", err)
	}

	good := f.code(e)
	other := "000000"
	if good == other {
		other = "000001"
	}

	tests := []struct {
		name string
		in   admin.SignInInput
	}{
		{"unknown email", admin.SignInInput{Email: "nobody@example.org", Password: ownPassword, TOTPCode: good}},
		{"wrong password", admin.SignInInput{Email: "admin@example.org", Password: "example other password", TOTPCode: good}},
		{"wrong code", admin.SignInInput{Email: "admin@example.org", Password: ownPassword, TOTPCode: other}},
		{"no code", admin.SignInInput{Email: "admin@example.org", Password: ownPassword}},
		{"unknown recovery code", admin.SignInInput{Email: "admin@example.org", Password: ownPassword, RecoveryCode: "AAAAA-AAAAA"}},
		{"junk recovery code", admin.SignInInput{Email: "admin@example.org", Password: ownPassword, RecoveryCode: "x"}},
		{"empty everything", admin.SignInInput{}},
	}

	for _, tc := range tests {
		if _, err := f.svc.SignIn(ctx, tc.in); !errors.Is(err, admin.ErrInvalidCredentials) {
			t.Fatalf("%s gave %v", tc.name, err)
		}
	}

	// none of the failures used the good code, so it still works
	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: ownPassword, TOTPCode: good}); err != nil {
		t.Fatalf("the good code failed %v", err)
	}
}

func TestSignInWithTheInitialPasswordAlwaysGivesASetupSession(t *testing.T) {
	f := newFixture(t)
	e := f.enroll(t, "admin@example.org")

	f.clock.advance(admin.InitialPasswordLifetime - time.Second)

	got, err := f.svc.SignIn(context.Background(), admin.SignInInput{Email: "admin@example.org", Password: initialPassword, TOTPCode: f.codeNoAdvance(e)})
	if err != nil || !got.Setup {
		t.Fatalf("an initial password that has a second left gave %+v and %v", got, err)
	}

	f.clock.advance(time.Second)

	got, err = f.svc.SignIn(context.Background(), admin.SignInInput{Email: "admin@example.org", Password: initialPassword, TOTPCode: f.code(e)})
	if err != nil || !got.Setup {
		t.Fatalf("an expired initial password gave %+v and %v", got, err)
	}
}

// codeNoAdvance returns the code of the present step. It is for a test that moved the clock itself.
func (f *fixture) codeNoAdvance(e enrolled) string {
	return totp.Code(e.secret, totp.Step(f.clock.Now()))
}

func TestATOTPCodeWorksOnce(t *testing.T) {
	f := newFixture(t)
	e := f.enroll(t, "admin@example.org")
	ctx := context.Background()

	in := admin.SignInInput{Email: "admin@example.org", Password: initialPassword, TOTPCode: f.code(e)}

	if _, err := f.svc.SignIn(ctx, in); err != nil {
		t.Fatalf("the first use failed %v", err)
	}

	if _, err := f.svc.SignIn(ctx, in); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("the second use gave %v", err)
	}

	// the code of the step before is still inside the window, and it is refused because it is not above the stored step
	earlier := totp.Code(e.secret, totp.Step(f.clock.Now())-1)
	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, TOTPCode: earlier}); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("a code of an earlier step gave %v", err)
	}

	// the next step is above the stored one and inside the window
	later := totp.Code(e.secret, totp.Step(f.clock.Now())+1)
	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, TOTPCode: later}); err != nil {
		t.Fatalf("a code of the next step failed %v", err)
	}
}

func TestARecoveryCodeWorksOnce(t *testing.T) {
	f := newFixture(t)
	e := f.enroll(t, "admin@example.org")
	ctx := context.Background()

	if len(e.codes) != admin.RecoveryCodeCount {
		t.Fatalf("the enrollment gave %d codes", len(e.codes))
	}

	in := admin.SignInInput{Email: "admin@example.org", Password: initialPassword, RecoveryCode: strings.ToLower(e.codes[3])}

	got, err := f.svc.SignIn(ctx, in)
	if err != nil || !got.UsedRecoveryCode {
		t.Fatalf("the first use gave %+v and %v", got, err)
	}

	if _, err := f.svc.SignIn(ctx, in); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("the second use gave %v", err)
	}

	profile, err := f.svc.Profile(ctx, e.id)
	if err != nil || profile.RecoveryCodesLeft != admin.RecoveryCodeCount-1 || !profile.TOTPEnabled {
		t.Fatalf("the profile is %+v and the error %v", profile, err)
	}
}

func TestChangePassword(t *testing.T) {
	f := newFixture(t)
	e := f.enroll(t, "admin@example.org")
	ctx := context.Background()

	var kept []byte

	f.repo.OnSetPassword = func(_ string, keep []byte) { kept = keep }

	bad := []struct {
		name string
		in   admin.ChangePasswordInput
	}{
		{"wrong current", admin.ChangePasswordInput{Current: "example other password", New: ownPassword, TOTPCode: f.code(e), RequireTOTP: true}},
		{"no code", admin.ChangePasswordInput{Current: initialPassword, New: ownPassword, RequireTOTP: true}},
		{"wrong code", admin.ChangePasswordInput{Current: initialPassword, New: ownPassword, TOTPCode: "000000", RequireTOTP: true}},
	}

	for _, tc := range bad {
		if err := f.svc.ChangePassword(ctx, e.id, tc.in, []byte("keep")); !errors.Is(err, admin.ErrInvalidCredentials) {
			t.Fatalf("%s gave %v", tc.name, err)
		}
	}

	if kept != nil {
		t.Fatal("a failed change reached the repository")
	}

	in := admin.ChangePasswordInput{Current: initialPassword, New: ownPassword, TOTPCode: f.code(e), RequireTOTP: true}
	if err := f.svc.ChangePassword(ctx, e.id, in, []byte("keep")); err != nil {
		t.Fatalf("change failed %v", err)
	}

	if string(kept) != "keep" {
		t.Fatalf("the repository was told to keep %q", kept)
	}

	stored, _ := f.repo.ByID(ctx, e.id)
	if stored.PasswordExpiresAt != nil || strings.Contains(stored.PasswordHash, ownPassword) {
		t.Fatalf("the stored admin is %+v", stored)
	}

	// the old password is gone and the new one works
	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, TOTPCode: f.code(e)}); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("the old password gave %v", err)
	}

	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: ownPassword, TOTPCode: f.code(e)}); err != nil {
		t.Fatalf("the new password failed %v", err)
	}
}

func TestASetupSessionChangesThePasswordWithoutACode(t *testing.T) {
	f := newFixture(t)
	id := f.create(t, "admin@example.org")

	err := f.svc.ChangePassword(context.Background(), id, admin.ChangePasswordInput{Current: initialPassword, New: ownPassword}, nil)
	if err != nil {
		t.Fatalf("change failed %v", err)
	}

	// an admin session needs a code, and an admin with no TOTP cannot give one
	err = f.svc.ChangePassword(context.Background(), id, admin.ChangePasswordInput{Current: ownPassword, New: initialPassword, RequireTOTP: true}, nil)
	if !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("a full session without TOTP gave %v", err)
	}
}

func TestEnrollment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.create(t, "admin@example.org")

	if _, err := f.svc.StartTOTP(ctx, id, []byte(sessionOne), admin.EnrollInput{Password: "example other password"}); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("a wrong password gave %v", err)
	}

	in := admin.EnrollInput{Password: initialPassword}

	first, err := f.svc.StartTOTP(ctx, id, []byte(sessionOne), in)
	if err != nil {
		t.Fatalf("start failed %v", err)
	}

	again, _ := f.svc.StartTOTP(ctx, id, []byte(sessionOne), in)
	elsewhere, _ := f.svc.StartTOTP(ctx, id, []byte("session two"), in)

	if first.Secret != again.Secret || first.Secret == elsewhere.Secret || len(first.Secret) != 32 {
		t.Fatalf("the secrets are %q, %q and %q", first.Secret, again.Secret, elsewhere.Secret)
	}

	if !strings.HasPrefix(first.URI, "otpauth://totp/LabMGM:admin@example.org?") || !strings.Contains(first.URI, "secret="+first.Secret) {
		t.Fatalf("the address is %q", first.URI)
	}

	// nothing is stored until the second call
	if stored, _ := f.repo.ByID(ctx, id); stored.TOTPEnabled() || stored.TOTPSecret != "" {
		t.Fatalf("the start stored %+v", stored)
	}

	secret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(first.Secret)
	in.ConfirmCode = "000000"

	if _, err := f.svc.FinishTOTP(ctx, id, []byte(sessionOne), in); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("a wrong confirm code gave %v", err)
	}

	// a code of the secret that another session got is no good here
	otherSecret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(elsewhere.Secret)
	in.ConfirmCode = totp.Code(otherSecret, totp.Step(f.clock.Now()))
	if _, err := f.svc.FinishTOTP(ctx, id, []byte(sessionOne), in); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("a code of another secret gave %v", err)
	}

	in.ConfirmCode = totp.Code(secret, totp.Step(f.clock.Now()))

	codes, err := f.svc.FinishTOTP(ctx, id, []byte(sessionOne), in)
	if err != nil || len(codes) != admin.RecoveryCodeCount {
		t.Fatalf("finish gave %d codes and %v", len(codes), err)
	}

	stored, _ := f.repo.ByID(ctx, id)
	if !stored.TOTPEnabled() || stored.TOTPLastStep == nil || strings.Contains(stored.TOTPSecret, first.Secret) {
		t.Fatalf("the stored admin is %+v", stored)
	}

	opened, err := f.box.Open(stored.TOTPSecret, []byte(id))
	if err != nil || string(opened) != string(secret) {
		t.Fatalf("the stored secret opens to %q and %v", opened, err)
	}

	// the secret is sealed for this admin only
	if _, err := f.box.Open(stored.TOTPSecret, []byte("another admin")); err == nil {
		t.Fatal("the sealed secret opened for another admin")
	}
}

func TestEnrollingAgainRotatesTheSecret(t *testing.T) {
	f := newFixture(t)
	e := f.enroll(t, "admin@example.org")
	ctx := context.Background()

	// an admin who has TOTP sends a code of the current secret in both calls
	if _, err := f.svc.StartTOTP(ctx, e.id, []byte(sessionOne), admin.EnrollInput{Password: initialPassword}); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("no code gave %v", err)
	}

	started, err := f.svc.StartTOTP(ctx, e.id, []byte(sessionOne), admin.EnrollInput{Password: initialPassword, TOTPCode: f.code(e)})
	if err != nil {
		t.Fatalf("start failed %v", err)
	}

	newSecret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(started.Secret)
	if string(newSecret) == string(e.secret) {
		t.Fatal("the new secret equals the current one")
	}

	// the first call changed nothing, so the current secret still signs in
	if stored, _ := f.repo.ByID(ctx, e.id); string(mustOpen(t, f, stored)) != string(e.secret) {
		t.Fatal("the start replaced the secret")
	}

	f.clock.advance(totp.StepSeconds * time.Second)

	codes, err := f.svc.FinishTOTP(ctx, e.id, []byte(sessionOne), admin.EnrollInput{
		Password: initialPassword, TOTPCode: totp.Code(e.secret, totp.Step(f.clock.Now())), ConfirmCode: totp.Code(newSecret, totp.Step(f.clock.Now())),
	})
	if err != nil || len(codes) != admin.RecoveryCodeCount {
		t.Fatalf("finish gave %d codes and %v", len(codes), err)
	}

	rotated := enrolled{id: e.id, secret: newSecret, codes: codes}

	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, TOTPCode: f.code(e)}); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("a code of the old secret gave %v", err)
	}

	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, TOTPCode: f.code(rotated)}); err != nil {
		t.Fatalf("a code of the new secret failed %v", err)
	}

	// the old recovery codes are gone
	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, RecoveryCode: e.codes[0]}); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("an old recovery code gave %v", err)
	}

	// a new enrollment from the same session gets a secret of its own, because the enrollment time changed
	again, err := f.svc.StartTOTP(ctx, e.id, []byte(sessionOne), admin.EnrollInput{Password: initialPassword, TOTPCode: f.code(rotated)})
	if err != nil || again.Secret == started.Secret {
		t.Fatalf("the next secret is %q and %v", again.Secret, err)
	}
}

func mustOpen(t *testing.T, f *fixture, a admin.Account) []byte {
	t.Helper()

	secret, err := f.box.Open(a.TOTPSecret, []byte(a.ID))
	if err != nil {
		t.Fatalf("open failed %v", err)
	}

	return secret
}

func TestRenewRecoveryCodes(t *testing.T) {
	f := newFixture(t)
	e := f.enroll(t, "admin@example.org")
	ctx := context.Background()

	for name, in := range map[string]admin.RenewInput{
		"wrong password": {Password: "example other password", TOTPCode: f.code(e)},
		"no code":        {Password: initialPassword},
		"wrong code":     {Password: initialPassword, TOTPCode: "000000"},
	} {
		if _, err := f.svc.RenewRecoveryCodes(ctx, e.id, in); !errors.Is(err, admin.ErrInvalidCredentials) {
			t.Fatalf("%s gave %v", name, err)
		}
	}

	fresh, err := f.svc.RenewRecoveryCodes(ctx, e.id, admin.RenewInput{Password: initialPassword, TOTPCode: f.code(e)})
	if err != nil || len(fresh) != admin.RecoveryCodeCount {
		t.Fatalf("renew gave %d codes and %v", len(fresh), err)
	}

	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, RecoveryCode: e.codes[0]}); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("an old code gave %v", err)
	}

	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, RecoveryCode: fresh[0]}); err != nil {
		t.Fatalf("a new code failed %v", err)
	}

	// an admin with no TOTP has nothing to renew
	other := f.create(t, "other@example.org")
	if _, err := f.svc.RenewRecoveryCodes(ctx, other, admin.RenewInput{Password: initialPassword, TOTPCode: "000000"}); !errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("an admin without TOTP gave %v", err)
	}
}

func TestAnAdminThatIsGone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	gone := "00000000-0000-4000-8000-000000000404"

	if _, err := f.svc.Profile(ctx, gone); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("profile gave %v", err)
	}

	if err := f.svc.ChangePassword(ctx, gone, admin.ChangePasswordInput{Current: initialPassword, New: ownPassword}, nil); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("change gave %v", err)
	}
}

func TestAStorageFailureIsNotAFailedSignIn(t *testing.T) {
	f := newFixture(t)
	f.create(t, "admin@example.org")

	boom := errors.New("the storage is down")
	f.repo.Err = boom

	_, err := f.svc.SignIn(context.Background(), admin.SignInInput{Email: "admin@example.org", Password: initialPassword})
	if !errors.Is(err, boom) || errors.Is(err, admin.ErrInvalidCredentials) {
		t.Fatalf("sign in gave %v", err)
	}
}
