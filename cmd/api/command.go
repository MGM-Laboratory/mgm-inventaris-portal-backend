package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/database"
)

// adminInitialEnv is the variable that holds the initial password of a new admin. A flag would show it in the process list.
const adminInitialEnv = "ADMIN_INITIAL_PASSWORD"

// commandTimeout bounds one command, which makes one hash and a few statements.
const commandTimeout = time.Minute

const usage = "usage: api create-admin --email <email> --name <name>\n       api reset-totp --email <email>\n"

// runCommand runs a subcommand and returns the exit code. A command prints to the writers it is given and never to the log, because
// its output is for the person who runs it.
func runCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var err error

	switch args[0] {
	case "create-admin":
		err = runCreateAdmin(args[1:], stdin, stdout)
	case "reset-totp":
		err = runResetTOTP(args[1:], stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], usage)

		return 2
	}

	if err != nil {
		_, _ = fmt.Fprintln(stderr, args[0]+" failed,", err)

		return 1
	}

	return 0
}

func runCreateAdmin(args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("create-admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	email := flags.String("email", "", "the email address of the admin")
	name := flags.String("name", "", "the name of the admin")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, usage)
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument\n%s", usage)
	}

	if *email == "" || *name == "" {
		return fmt.Errorf("both --email and --name are required\n%s", usage)
	}

	initial, err := readInitialPassword(os.Getenv(adminInitialEnv), stdin)
	if err != nil {
		return err
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

	if cfg.MigrateOnStart {
		if err := database.Migrate(cfg.DatabaseURL.Reveal()); err != nil {
			return fmt.Errorf("migrate the database %w", err)
		}
	}

	service, err := newAdminService(cfg, pool)
	if err != nil {
		return err
	}

	id, stored, err := service.Create(ctx, admin.CreateInput{Email: *email, Name: *name, Password: initial})

	var invalid *admin.InvalidInputError

	switch {
	case errors.Is(err, admin.ErrDuplicate):
		return errors.New("an admin with this email exists already")
	case errors.As(err, &invalid):
		return invalid
	case err != nil:
		return fmt.Errorf("create the admin %w", err)
	}

	_, err = fmt.Fprintf(stdout, "created admin %s with id %s\n", stored, id)

	return err //nolint:wrapcheck // a failed write to the terminal has nothing to add
}

// readInitialPassword takes the password from the environment value, or else from one line of the standard input. It refuses a
// terminal, because the password would show on the screen as it is typed.
func readInitialPassword(fromEnv string, stdin io.Reader) (string, error) {
	if fromEnv != "" {
		return fromEnv, nil
	}

	if file, ok := stdin.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("set %s or pipe the password on the standard input", adminInitialEnv)
		}
	}

	line, err := bufio.NewReader(io.LimitReader(stdin, 1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the password %w", err)
	}

	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", fmt.Errorf("no password on the standard input, set %s or pipe one", adminInitialEnv)
	}

	return password, nil
}
