package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

// uniqueViolation is the SQLSTATE of a unique constraint that a statement broke.
const uniqueViolation = "23505"

const rollbackTimeout = 5 * time.Second

// DB is the part of a pool that the PostgreSQL repository needs.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Postgres stores the admin accounts in PostgreSQL.
type Postgres struct {
	db DB
}

// NewPostgres returns the repository on a pool.
func NewPostgres(db DB) *Postgres {
	return &Postgres{db: db}
}

const accountColumns = `id::text, email, name, password_hash, password_expires_at, coalesce(totp_secret, ''), totp_enabled_at, totp_last_step`

func (p *Postgres) one(ctx context.Context, where string, arg any) (Account, error) {
	var a Account

	err := p.db.QueryRow(ctx, `select `+accountColumns+` from admins where `+where, arg).
		Scan(&a.ID, &a.Email, &a.Name, &a.PasswordHash, &a.PasswordExpiresAt, &a.TOTPSecret, &a.TOTPEnabledAt, &a.TOTPLastStep)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}

	if err != nil {
		return Account{}, fmt.Errorf("read the admin %w", err)
	}

	return a, nil
}

// ByEmail returns the admin with the email, which is in lower case.
func (p *Postgres) ByEmail(ctx context.Context, email string) (Account, error) {
	return p.one(ctx, `email = $1`, email)
}

// ByID returns the admin with the id.
func (p *Postgres) ByID(ctx context.Context, id string) (Account, error) {
	return p.one(ctx, `id = $1::uuid`, id)
}

// Insert stores a new admin and returns its id.
func (p *Postgres) Insert(ctx context.Context, a Account) (string, error) {
	var id string

	err := p.db.QueryRow(ctx, `
		insert into admins (email, name, password_hash, password_expires_at) values ($1, $2, $3, $4) returning id::text`,
		a.Email, a.Name, a.PasswordHash, a.PasswordExpiresAt).Scan(&id)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "admins_email_key" {
		return "", ErrDuplicate
	}

	if err != nil {
		return "", fmt.Errorf("insert the admin %w", err)
	}

	return id, nil
}

// AcceptTOTPStep records a time step when it is above the stored one.
func (p *Postgres) AcceptTOTPStep(ctx context.Context, id string, step int64) (bool, error) {
	tag, err := p.db.Exec(ctx, `
		update admins set totp_last_step = $2
		where id = $1::uuid and (totp_last_step is null or totp_last_step < $2)`, id, step)
	if err != nil {
		return false, fmt.Errorf("update the TOTP step %w", err)
	}

	return tag.RowsAffected() == 1, nil
}

// inTx runs fn in a transaction. A failed or cancelled run rolls back on a context of its own, so the rollback is not cut short.
func (p *Postgres) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the transaction %w", err)
	}

	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()

		_ = tx.Rollback(rollbackCtx)
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the transaction %w", err)
	}

	return nil
}

// replaceCodes deletes the recovery codes of the admin and stores the new hashes. The lock on the admin row makes two replacements
// run one after the other. Without it the second delete would not see the rows that the first one inserted, and the admin would
// end with two sets of valid codes.
func replaceCodes(ctx context.Context, tx pgx.Tx, id string, hashes []string) error {
	var locked string
	if err := tx.QueryRow(ctx, `select id::text from admins where id = $1::uuid for update`, id).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("lock the admin %w", err)
	}

	if _, err := tx.Exec(ctx, `delete from admin_recovery_codes where admin_id = $1::uuid`, id); err != nil {
		return fmt.Errorf("delete the recovery codes %w", err)
	}

	for _, hash := range hashes {
		if _, err := tx.Exec(ctx, `insert into admin_recovery_codes (admin_id, code_hash) values ($1::uuid, $2)`, id, hash); err != nil {
			return fmt.Errorf("insert a recovery code %w", err)
		}
	}

	return nil
}

// EnableTOTP stores the secret and the recovery codes when the enrollment time is still the expected one.
func (p *Postgres) EnableTOTP(ctx context.Context, id string, expected *time.Time, sealed string, at time.Time, step int64, hashes []string) (bool, error) {
	var stored bool

	err := p.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			update admins set totp_secret = $3, totp_enabled_at = $4, totp_last_step = $5
			where id = $1::uuid and totp_enabled_at is not distinct from $2::timestamptz`, id, expected, sealed, at, step)
		if err != nil {
			return fmt.Errorf("store the TOTP secret %w", err)
		}

		if tag.RowsAffected() != 1 {
			return nil
		}

		stored = true

		return replaceCodes(ctx, tx, id, hashes)
	})

	return stored && err == nil, err
}

// SetPassword stores the hash and ends the other sessions in one transaction.
func (p *Postgres) SetPassword(ctx context.Context, id, hash string, keep []byte) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update admins set password_hash = $2, password_expires_at = null where id = $1::uuid`, id, hash)
		if err != nil {
			return fmt.Errorf("update the password %w", err)
		}

		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}

		if _, err := session.DeleteOthers(ctx, tx, id, keep); err != nil {
			return fmt.Errorf("end the other sessions %w", err)
		}

		return nil
	})
}

// UnusedRecoveryHashes lists the hashes of the codes that are not used.
func (p *Postgres) UnusedRecoveryHashes(ctx context.Context, id string) ([]string, error) {
	rows, err := p.db.Query(ctx, `select code_hash from admin_recovery_codes where admin_id = $1::uuid and used_at is null`, id)
	if err != nil {
		return nil, fmt.Errorf("list the recovery codes %w", err)
	}

	hashes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the recovery codes %w", err)
	}

	return hashes, nil
}

// UseRecoveryCode marks a code as used when it is not used yet.
func (p *Postgres) UseRecoveryCode(ctx context.Context, id, hash string, at time.Time) (bool, error) {
	tag, err := p.db.Exec(ctx, `
		update admin_recovery_codes set used_at = $3
		where admin_id = $1::uuid and code_hash = $2 and used_at is null`, id, hash, at)
	if err != nil {
		return false, fmt.Errorf("mark the recovery code %w", err)
	}

	return tag.RowsAffected() == 1, nil
}

// ReplaceRecoveryCodes deletes the codes of the admin and stores new hashes.
func (p *Postgres) ReplaceRecoveryCodes(ctx context.Context, id string, hashes []string) error {
	return p.inTx(ctx, func(tx pgx.Tx) error {
		return replaceCodes(ctx, tx, id, hashes)
	})
}
