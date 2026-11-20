package admin_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database/dbtest"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/password"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/secretbox"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/totp"
)

// The tests below run on a real PostgreSQL and skip without DATABASE_URL. They prove what the in-memory repository cannot, which is
// that a guarded update lets one of two parallel calls through.

type dbFixture struct {
	pool     *pgxpool.Pool
	repo     *admin.Postgres
	svc      *admin.Service
	sessions *session.Store
	box      *secretbox.Box
	clock    *clock
}

func newDBFixture(t *testing.T) *dbFixture {
	t.Helper()

	pool := dbtest.New(t)
	clk := &clock{now: time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)}

	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("make the box %v", err)
	}

	repo := admin.NewPostgres(pool)

	return &dbFixture{
		pool: pool, repo: repo, box: box, clock: clk, sessions: session.NewStore(pool, clk.Now),
		svc: admin.NewService(repo, password.New(2, 10*time.Second), box, clk.Now),
	}
}

func (f *dbFixture) create(t *testing.T, email string) string {
	t.Helper()

	id, _, err := f.svc.Create(context.Background(), admin.CreateInput{Email: email, Name: "Example Admin", Password: initialPassword})
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	return id
}

// parallel runs fn n times at once and returns how many calls answered true.
func parallel(n int, fn func() bool) int {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)

	start := make(chan struct{})

	for range n {
		wg.Go(func() {
			<-start

			if fn() {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}

	close(start)
	wg.Wait()

	return wins
}

func (f *dbFixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()

	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count failed %v", err)
	}

	return n
}

func TestPostgresCreateAndRead(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()

	id := f.create(t, "Admin@Example.org")

	byEmail, err := f.repo.ByEmail(ctx, "admin@example.org")
	if err != nil {
		t.Fatalf("by email failed %v", err)
	}

	byID, err := f.repo.ByID(ctx, id)
	if err != nil || byID.ID != id || byEmail.ID != id {
		t.Fatalf("by id gave %+v and %v", byID, err)
	}

	if byID.Name != "Example Admin" || byID.TOTPSecret != "" || byID.TOTPEnabled() || byID.TOTPLastStep != nil || byID.PasswordExpiresAt == nil {
		t.Fatalf("the admin is %+v", byID)
	}

	if want := f.clock.Now().Add(admin.InitialPasswordLifetime); !byID.PasswordExpiresAt.Equal(want) {
		t.Fatalf("the password expires at %v, want %v", byID.PasswordExpiresAt, want)
	}

	if _, err := f.repo.ByEmail(ctx, "nobody@example.org"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("an unknown email gave %v", err)
	}

	if _, err := f.repo.ByID(ctx, "00000000-0000-4000-8000-000000000404"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("an unknown id gave %v", err)
	}
}

func TestCreateAdminRefusesADuplicateEmail(t *testing.T) {
	f := newDBFixture(t)
	f.create(t, "admin@example.org")

	for _, email := range []string{"admin@example.org", "ADMIN@Example.org", " admin@example.org "} {
		_, _, err := f.svc.Create(context.Background(), admin.CreateInput{Email: email, Name: "Another Admin", Password: initialPassword})
		if !errors.Is(err, admin.ErrDuplicate) {
			t.Fatalf("%q gave %v", email, err)
		}
	}

	if n := f.count(t, `select count(*) from admins`); n != 1 {
		t.Fatalf("%d admins exist", n)
	}

	// two parallel creations of one email leave one row, and the loser gets ErrDuplicate and not a raw error
	wins := parallel(6, func() bool {
		_, _, err := f.svc.Create(context.Background(), admin.CreateInput{Email: "race@example.org", Name: "Race Admin", Password: initialPassword})
		if err != nil && !errors.Is(err, admin.ErrDuplicate) {
			t.Errorf("a parallel create gave %v", err)
		}

		return err == nil
	})

	if wins != 1 || f.count(t, `select count(*) from admins where email = 'race@example.org'`) != 1 {
		t.Fatalf("%d parallel creations won", wins)
	}
}

func TestARecoveryCodeWorksOnceEvenWhenSentTwiceAtOnce(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()
	id := f.create(t, "admin@example.org")

	codes, hashes, err := admin.NewRecoveryCodes(3)
	if err != nil {
		t.Fatalf("make failed %v", err)
	}

	if err := f.repo.ReplaceRecoveryCodes(ctx, id, hashes); err != nil {
		t.Fatalf("store failed %v", err)
	}

	// the guarded update lets exactly one of the parallel calls mark the code
	wins := parallel(8, func() bool {
		used, err := f.repo.UseRecoveryCode(ctx, id, hashes[0], f.clock.Now())
		if err != nil {
			t.Errorf("use failed %v", err)
		}

		return used
	})
	if wins != 1 {
		t.Fatalf("%d of 8 parallel calls used the code", wins)
	}

	left, err := f.repo.UnusedRecoveryHashes(ctx, id)
	if err != nil || len(left) != 2 || slices.Contains(left, hashes[0]) {
		t.Fatalf("the unused hashes are %v and %v", left, err)
	}

	if used := f.count(t, `select count(*) from admin_recovery_codes where used_at is not null`); used != 1 {
		t.Fatalf("%d codes are marked", used)
	}

	// through the service, two sign ins with one code let one in
	account, _ := f.repo.ByID(ctx, id)
	enableTOTP(t, f, account)

	signIns := parallel(6, func() bool {
		_, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, RecoveryCode: codes[1]})
		if err != nil && !errors.Is(err, admin.ErrInvalidCredentials) {
			t.Errorf("a parallel sign in gave %v", err)
		}

		return err == nil
	})
	if signIns != 1 {
		t.Fatalf("%d of 6 parallel sign ins used one recovery code", signIns)
	}
}

// enableTOTP stores a TOTP secret for the account through the repository and returns the secret.
func enableTOTP(t *testing.T, f *dbFixture, account admin.Account) []byte {
	t.Helper()

	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatalf("make the secret %v", err)
	}

	sealed, err := f.box.Seal(secret, []byte(account.ID))
	if err != nil {
		t.Fatalf("seal failed %v", err)
	}

	current, err := f.repo.UnusedRecoveryHashes(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("read failed %v", err)
	}

	stored, err := f.repo.EnableTOTP(context.Background(), account.ID, nil, sealed, f.clock.Now(), 1, current)
	if err != nil || !stored {
		t.Fatalf("enable gave %v and %v", stored, err)
	}

	return secret
}

func TestTheTOTPReplayGuardHoldsUnderParallelCalls(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()
	id := f.create(t, "admin@example.org")

	// the first step is accepted by exactly one of the parallel calls
	wins := parallel(8, func() bool {
		ok, err := f.repo.AcceptTOTPStep(ctx, id, 100)
		if err != nil {
			t.Errorf("accept failed %v", err)
		}

		return ok
	})
	if wins != 1 {
		t.Fatalf("%d of 8 parallel calls accepted one step", wins)
	}

	for _, tc := range []struct {
		step int64
		want bool
	}{{100, false}, {99, false}, {1, false}, {101, true}, {101, false}, {200, true}} {
		if ok, err := f.repo.AcceptTOTPStep(ctx, id, tc.step); err != nil || ok != tc.want {
			t.Fatalf("step %d gave %v and %v, want %v", tc.step, ok, err, tc.want)
		}
	}

	account, _ := f.repo.ByID(ctx, id)
	if account.TOTPLastStep == nil || *account.TOTPLastStep != 200 {
		t.Fatalf("the stored step is %v", account.TOTPLastStep)
	}

	// through the service, parallel sign ins with one code let one in
	secret := enableTOTP(t, f, account)

	step := totp.Step(f.clock.Now()) + 1000
	f.clock.advance(1000 * totp.StepSeconds * time.Second)

	code := totp.Code(secret, step)

	signIns := parallel(6, func() bool {
		_, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: initialPassword, TOTPCode: code})
		if err != nil && !errors.Is(err, admin.ErrInvalidCredentials) {
			t.Errorf("a parallel sign in gave %v", err)
		}

		return err == nil
	})
	if signIns != 1 {
		t.Fatalf("%d of 6 parallel sign ins used one TOTP code", signIns)
	}
}

func TestEnableTOTPIsGuardedByTheEnrollmentTime(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()
	id := f.create(t, "admin@example.org")
	_, first, _ := admin.NewRecoveryCodes(10)

	at := f.clock.Now()

	// two parallel finishes of the first enrollment let one through, and the table holds the codes of that one only
	var (
		mu     sync.Mutex
		winner []string
	)

	wins := parallel(6, func() bool {
		_, hashes, err := admin.NewRecoveryCodes(10)
		if err != nil {
			t.Errorf("make failed %v", err)
		}

		stored, err := f.repo.EnableTOTP(ctx, id, nil, "sealed one", at, 5, hashes)
		if err != nil {
			t.Errorf("enable failed %v", err)
		}

		if stored {
			mu.Lock()
			winner = hashes
			mu.Unlock()
		}

		return stored
	})
	if wins != 1 {
		t.Fatalf("%d of 6 parallel finishes stored a secret", wins)
	}

	left, err := f.repo.UnusedRecoveryHashes(ctx, id)
	slices.Sort(left)
	slices.Sort(winner)

	if err != nil || !slices.Equal(left, winner) {
		t.Fatalf("the table holds %d codes and the winner made %d", len(left), len(winner))
	}

	account, _ := f.repo.ByID(ctx, id)
	if account.TOTPSecret != "sealed one" || account.TOTPEnabledAt == nil || !account.TOTPEnabledAt.Equal(at) || account.TOTPLastStep == nil || *account.TOTPLastStep != 5 {
		t.Fatalf("the stored admin is %+v", account)
	}

	// a second enrollment must name the time of the first, and a stale time stores nothing
	later := at.Add(time.Hour)

	if stored, err := f.repo.EnableTOTP(ctx, id, nil, "sealed two", later, 6, first); err != nil || stored {
		t.Fatalf("a stale expected time gave %v and %v", stored, err)
	}

	stale := at.Add(-time.Minute)
	if stored, err := f.repo.EnableTOTP(ctx, id, &stale, "sealed two", later, 6, first); err != nil || stored {
		t.Fatalf("a wrong expected time gave %v and %v", stored, err)
	}

	if stored, err := f.repo.EnableTOTP(ctx, id, account.TOTPEnabledAt, "sealed two", later, 6, first); err != nil || !stored {
		t.Fatalf("the right expected time gave %v and %v", stored, err)
	}

	left, _ = f.repo.UnusedRecoveryHashes(ctx, id)
	slices.Sort(first)
	slices.Sort(left)

	if !slices.Equal(left, first) {
		t.Fatal("the codes were not replaced by the second enrollment")
	}

	// the constraint of the table holds, an enrolled admin always has a secret
	if n := f.count(t, `select count(*) from admins where totp_enabled_at is not null and totp_secret is null`); n != 0 {
		t.Fatalf("%d admins are enrolled without a secret", n)
	}
}

func TestParallelRenewalsLeaveOneSetOfCodes(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()
	id := f.create(t, "admin@example.org")

	var (
		mu   sync.Mutex
		sets [][]string
	)

	parallel(6, func() bool {
		_, hashes, err := admin.NewRecoveryCodes(10)
		if err != nil {
			t.Errorf("make failed %v", err)
		}

		if err := f.repo.ReplaceRecoveryCodes(ctx, id, hashes); err != nil {
			t.Errorf("replace failed %v", err)
		}

		mu.Lock()
		sets = append(sets, hashes)
		mu.Unlock()

		return true
	})

	left, err := f.repo.UnusedRecoveryHashes(ctx, id)
	if err != nil || len(left) != 10 {
		t.Fatalf("the table holds %d codes after six parallel renewals, %v", len(left), err)
	}

	slices.Sort(left)

	matches := 0

	for _, set := range sets {
		sorted := slices.Clone(set)
		slices.Sort(sorted)

		if slices.Equal(sorted, left) {
			matches++
		}
	}

	if matches != 1 {
		t.Fatalf("the codes in the table are the set of %d renewals, want exactly one", matches)
	}

	if err := f.repo.ReplaceRecoveryCodes(ctx, "00000000-0000-4000-8000-000000000404", []string{"aa"}); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("the codes of an admin that is gone gave %v", err)
	}
}

func TestAPasswordChangeEndsTheOtherSessions(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()

	id := f.create(t, "admin@example.org")
	otherID := f.create(t, "other@example.org")
	who := session.Admin{ID: id, Name: "Example Admin", Email: "admin@example.org"}

	// the service uses the same pool, so the sessions are in the same database
	_, first, err := f.sessions.CreateAdmin(ctx, who, session.KindAdmin, nil, false)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	secondToken, second, _ := f.sessions.CreateAdmin(ctx, who, session.KindAdmin, nil, false)
	_, third, _ := f.sessions.CreateAdmin(ctx, who, session.KindAdminSetup, nil, false)
	_, elsewhere, _ := f.sessions.CreateAdmin(ctx, session.Admin{ID: otherID, Name: "Other", Email: "other@example.org"}, session.KindAdmin, nil, false)
	guestToken, _, _ := f.sessions.CreateGuest(ctx, nil, nil)

	if n := f.count(t, `select count(*) from sessions`); n != 5 {
		t.Fatalf("%d sessions were made", n)
	}

	err = f.svc.ChangePassword(ctx, id, admin.ChangePasswordInput{Current: initialPassword, New: ownPassword}, second.TokenHash)
	if err != nil {
		t.Fatalf("change failed %v", err)
	}

	var left [][]byte

	rows, err := f.pool.Query(ctx, `select token_hash from sessions`)
	if err != nil {
		t.Fatalf("list failed %v", err)
	}

	for rows.Next() {
		var hash []byte
		if err := rows.Scan(&hash); err != nil {
			t.Fatalf("scan failed %v", err)
		}

		left = append(left, hash)
	}

	rows.Close()

	has := func(hash []byte) bool {
		return slices.ContainsFunc(left, func(other []byte) bool { return string(other) == string(hash) })
	}

	if len(left) != 3 || has(first.TokenHash) || has(third.TokenHash) || !has(second.TokenHash) || !has(elsewhere.TokenHash) || !has(session.HashToken(guestToken)) {
		t.Fatalf("%d sessions are left and the kept one is %v", len(left), has(second.TokenHash))
	}

	if _, err := f.sessions.Load(ctx, secondToken); err != nil {
		t.Fatalf("the session that asked was ended %v", err)
	}

	account, _ := f.repo.ByID(ctx, id)
	if account.PasswordExpiresAt != nil {
		t.Fatal("the initial password still expires")
	}

	// the hash is the new one, and a change without a kept session ends all the sessions of the admin
	if _, err := f.svc.SignIn(ctx, admin.SignInInput{Email: "admin@example.org", Password: ownPassword}); err != nil {
		t.Fatalf("the new password failed %v", err)
	}

	if err := f.svc.ChangePassword(ctx, id, admin.ChangePasswordInput{Current: ownPassword, New: initialPassword}, nil); err != nil {
		t.Fatalf("the second change failed %v", err)
	}

	if n := f.count(t, `select count(*) from sessions where admin_id = $1::uuid`, id); n != 0 {
		t.Fatalf("%d sessions of the admin are left", n)
	}

	if n := f.count(t, `select count(*) from sessions`); n != 2 {
		t.Fatalf("%d sessions are left in all, want the guest and the other admin", n)
	}

	// a change for an admin that is gone fails and ends nothing
	if err := f.repo.SetPassword(ctx, "00000000-0000-4000-8000-000000000404", "hash", nil); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("a missing admin gave %v", err)
	}
}

func TestACanceledPasswordChangeChangesNothing(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()
	id := f.create(t, "admin@example.org")

	// a canceled context stops the transaction before it commits, so neither the password nor the sessions change
	canceled, cancel := context.WithCancel(ctx)
	cancel()

	if err := f.repo.SetPassword(canceled, id, "$argon2id$changed", nil); err == nil {
		t.Fatal("a canceled call succeeded")
	}

	account, _ := f.repo.ByID(ctx, id)
	if account.PasswordHash == "$argon2id$changed" || account.PasswordExpiresAt == nil {
		t.Fatalf("the canceled call changed the admin %+v", account)
	}
}
