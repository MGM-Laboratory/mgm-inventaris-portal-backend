package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadInitialPassword(t *testing.T) {
	tests := []struct {
		name    string
		fromEnv string
		stdin   string
		want    string
		wantErr bool
	}{
		{"from the environment", "example env password", "example stdin password\n", "example env password", false},
		{"from one line", "", "example stdin password\n", "example stdin password", false},
		{"without a newline", "", "example stdin password", "example stdin password", false},
		{"with a carriage return", "", "example stdin password\r\n", "example stdin password", false},
		{"only the first line", "", "first line password\nsecond line password\n", "first line password", false},
		{"spaces inside and at the ends stay", "", " two  spaces \n", " two  spaces ", false},
		{"nothing", "", "", "", true},
		{"an empty line", "", "\n", "", true},
	}

	for _, tc := range tests {
		got, err := readInitialPassword(tc.fromEnv, strings.NewReader(tc.stdin))
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Fatalf("%s gave %q and %v", tc.name, got, err)
		}
	}
}

func TestRunCommandRefusesWhatItDoesNotKnow(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := runCommand([]string{"drop-everything"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("the exit code is %d", code)
	}

	if !strings.Contains(stderr.String(), "usage: api create-admin") || stdout.Len() != 0 {
		t.Fatalf("stderr is %q and stdout is %q", stderr.String(), stdout.String())
	}
}

func TestCreateAdminNeedsBothFlags(t *testing.T) {
	for _, args := range [][]string{
		{"create-admin"},
		{"create-admin", "--email", "admin@example.org"},
		{"create-admin", "--name", "Example Admin"},
		{"create-admin", "--password", "example flag password"},
		{"create-admin", "--email", "admin@example.org", "--name", "Example Admin", "extra"},
	} {
		var stdout, stderr bytes.Buffer

		// the password is on the standard input, so a failure here comes from the flags
		code := runCommand(args, strings.NewReader("example stdin password\n"), &stdout, &stderr)
		if code == 0 || stdout.Len() != 0 {
			t.Fatalf("%v gave the exit code %d and the output %q", args, code, stdout.String())
		}

		if strings.Contains(stderr.String(), "example stdin password") || strings.Contains(stderr.String(), "example flag password") {
			t.Fatalf("%v printed a password, %q", args, stderr.String())
		}
	}
}

func TestCreateAdminStopsWithoutSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PUBLIC_BASE_URL", "")
	t.Setenv("TOTP_ENCRYPTION_KEY", "")
	t.Setenv("ADMIN_INITIAL_PASSWORD", "example env password")

	var stdout, stderr bytes.Buffer

	code := runCommand([]string{"create-admin", "--email", "admin@example.org", "--name", "Example Admin"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("the exit code is %d and the output %q", code, stdout.String())
	}

	if !strings.Contains(stderr.String(), "DATABASE_URL") || strings.Contains(stderr.String(), "example env password") || strings.Contains(stderr.String(), "admin@example.org") {
		t.Fatalf("stderr is %q", stderr.String())
	}
}
