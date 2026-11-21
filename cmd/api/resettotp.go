package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database"
)

const resetTOTPUsage = "usage: api reset-totp --email <email>\n"

// runResetTOTP removes the second factor of one admin, with the recovery codes and the sessions. The command reads the full settings
// like every other command, but it does not use the TOTP key, so any valid value of the setting is enough after a lost key.
func runResetTOTP(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("reset-totp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	email := flags.String("email", "", "the email address of the admin")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, resetTOTPUsage)
	}

	if flags.NArg() > 0 || *email == "" {
		return fmt.Errorf("--email is required and nothing else is accepted\n%s", resetTOTPUsage)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load the settings %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	pool, err := database.Open(ctx, cfg.DatabaseURL.Reveal(), cfg.DBMaxConns)
	if err != nil {
		return fmt.Errorf("open the database %w", err)
	}
	defer pool.Close()

	service, err := newAdminService(cfg, pool)
	if err != nil {
		return err
	}

	id, err := service.ResetTOTP(ctx, *email)
	if errors.Is(err, admin.ErrNotFound) {
		return errors.New("no admin has this email")
	}

	if err != nil {
		return fmt.Errorf("reset the admin %w", err)
	}

	_, err = fmt.Fprintf(stdout, "removed the second factor of admin %s, who signs in with the password and enrolls a new factor\n", id)

	return err //nolint:wrapcheck // a failed write to the terminal has nothing to add
}
