package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const sampleDatabaseURL = "postgres://app:s3cret-sample@db.example.test:5432/portal?sslmode=disable"

func baseEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":    sampleDatabaseURL,
		"PUBLIC_BASE_URL": "https://portal.example.org",
	}
}

func with(overrides map[string]string) map[string]string {
	environ := baseEnv()
	for k, v := range overrides {
		environ[k] = v
	}

	return environ
}

func TestDefaults(t *testing.T) {
	cfg, err := parse(baseEnv())
	if err != nil {
		t.Fatalf("parse failed %v", err)
	}

	if cfg.HTTPAddr != ":8080" || cfg.HealthAddr != "127.0.0.1:8081" {
		t.Fatalf("addresses are %q and %q", cfg.HTTPAddr, cfg.HealthAddr)
	}

	if cfg.Environment != EnvProduction || cfg.LogLevel != "info" || !cfg.MigrateOnStart || cfg.DBMaxConns != 8 {
		t.Fatalf("unexpected defaults %+v", cfg)
	}

	if cfg.MaxItemsPerRequest != 10 || cfg.MaxDurationDays != 30 || cfg.MaxOpenTickets != 3 {
		t.Fatalf("unexpected limits %d %d %d", cfg.MaxItemsPerRequest, cfg.MaxDurationDays, cfg.MaxOpenTickets)
	}

	if cfg.GuestSigninPerHour != 60 {
		t.Fatalf("the guest sign in limit is %d", cfg.GuestSigninPerHour)
	}

	if len(cfg.TrustedProxies) != 0 || cfg.DatabaseURL.Reveal() != sampleDatabaseURL {
		t.Fatal("unexpected proxies or database URL")
	}

	if level, err := cfg.SlogLevel(); err != nil || level != slog.LevelInfo {
		t.Fatalf("level is %v, error %v", level, err)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := parse(with(map[string]string{
		"HTTP_ADDR":             "0.0.0.0:9000",
		"HEALTH_ADDR":           "[::1]:9001",
		"ENVIRONMENT":           "development",
		"LOG_LEVEL":             "DEBUG",
		"MIGRATE_ON_START":      "false",
		"PUBLIC_BASE_URL":       "http://Localhost:5173/",
		"TRUSTED_PROXIES":       "::ffff:10.0.0.2, 172.16.0.0/12,fd00::/8",
		"MAX_ITEMS_PER_REQUEST": "5",
		"MAX_DURATION_DAYS":     "14",
		"MAX_OPEN_TICKETS":      "2",
		"GUEST_SIGNIN_PER_HOUR": "5",
	}))
	if err != nil {
		t.Fatalf("parse failed %v", err)
	}

	if cfg.PublicBaseURL != "http://localhost:5173" {
		t.Fatalf("base URL is %q", cfg.PublicBaseURL)
	}

	wantProxies := []string{"10.0.0.2/32", "172.16.0.0/12", "fd00::/8"}
	gotProxies := make([]string, 0, len(cfg.TrustedProxies))
	for _, prefix := range cfg.TrustedProxies {
		gotProxies = append(gotProxies, prefix.String())
	}

	if !slices.Equal(gotProxies, wantProxies) || cfg.MigrateOnStart || cfg.LogLevel != "debug" {
		t.Fatalf("unexpected values %+v", cfg)
	}

	if cfg.MaxItemsPerRequest != 5 || cfg.MaxDurationDays != 14 || cfg.MaxOpenTickets != 2 || cfg.GuestSigninPerHour != 5 {
		t.Fatal("limits were not read")
	}
}

func TestSecureCookie(t *testing.T) {
	cfg, err := parse(baseEnv())
	if err != nil || !cfg.SecureCookie {
		t.Fatalf("the cookie is secure by default, got %v and %v", cfg.SecureCookie, err)
	}

	cfg, err = parse(with(map[string]string{
		"ENVIRONMENT": "development", "SECURE_COOKIE": "false", "PUBLIC_BASE_URL": "http://localhost:5173",
	}))
	if err != nil || cfg.SecureCookie {
		t.Fatalf("development may turn the flag off, got %v and %v", cfg.SecureCookie, err)
	}
}

func TestPublicBaseURLLeavesOutTheDefaultPort(t *testing.T) {
	tests := map[string]string{
		"https://Portal.Example.org:443":  "https://portal.example.org",
		"https://portal.example.org:8443": "https://portal.example.org:8443",
		"https://portal.example.org":      "https://portal.example.org",
		"https://[::1]:443":               "https://[::1]",
	}

	for in, want := range tests {
		cfg, err := parse(with(map[string]string{"PUBLIC_BASE_URL": in}))
		if err != nil || cfg.PublicBaseURL != want {
			t.Fatalf("%s gave %q and %v, want %q", in, cfg.PublicBaseURL, err, want)
		}
	}

	cfg, err := parse(with(map[string]string{"ENVIRONMENT": "development", "SECURE_COOKIE": "false", "PUBLIC_BASE_URL": "http://localhost:80"}))
	if err != nil || cfg.PublicBaseURL != "http://localhost" {
		t.Fatalf("an http base URL gave %q and %v", cfg.PublicBaseURL, err)
	}
}

func TestBadValues(t *testing.T) {
	tests := []struct {
		name    string
		environ map[string]string
		want    string
	}{
		{"missing database URL", map[string]string{"PUBLIC_BASE_URL": "https://portal.example.org"}, "DATABASE_URL"},
		{"empty database URL", with(map[string]string{"DATABASE_URL": ""}), "DATABASE_URL"},
		{"database URL scheme", with(map[string]string{"DATABASE_URL": "mysql://u:pw-sample@h/db"}), "DATABASE_URL"},
		{"missing base URL", map[string]string{"DATABASE_URL": sampleDatabaseURL}, "PUBLIC_BASE_URL"},
		{"base URL with a path", with(map[string]string{"PUBLIC_BASE_URL": "https://portal.example.org/app"}), "PUBLIC_BASE_URL"},
		{"base URL with a user", with(map[string]string{"PUBLIC_BASE_URL": "https://user:pw-sample@portal.example.org"}), "PUBLIC_BASE_URL"},
		{"base URL without a scheme", with(map[string]string{"PUBLIC_BASE_URL": "portal.example.org"}), "PUBLIC_BASE_URL"},
		{"base URL without a host name", with(map[string]string{"PUBLIC_BASE_URL": "https://:443"}), "PUBLIC_BASE_URL"},
		{"http in production", with(map[string]string{"PUBLIC_BASE_URL": "http://portal.example.org"}), "PUBLIC_BASE_URL"},
		{"environment", with(map[string]string{"ENVIRONMENT": "staging"}), "ENVIRONMENT"},
		{"insecure cookie in production", with(map[string]string{"SECURE_COOKIE": "false"}), "SECURE_COOKIE"},
		{"cookie flag not a bool", with(map[string]string{"SECURE_COOKIE": "sometimes"}), "SECURE_COOKIE"},
		{"log level", with(map[string]string{"LOG_LEVEL": "loud"}), "LOG_LEVEL"},
		{"http address", with(map[string]string{"HTTP_ADDR": "8080"}), "HTTP_ADDR"},
		{"address that holds a secret", with(map[string]string{"HTTP_ADDR": "postgres://app:pw-sample@db/portal"}), "HTTP_ADDR"},
		{"port that holds a secret", with(map[string]string{"HEALTH_ADDR": "db:pw-sample"}), "HEALTH_ADDR"},
		{"environment that holds a secret", with(map[string]string{"ENVIRONMENT": "pw-sample"}), "ENVIRONMENT"},
		{"log level that holds a secret", with(map[string]string{"LOG_LEVEL": "pw-sample"}), "LOG_LEVEL"},
		{"health port", with(map[string]string{"HEALTH_ADDR": "127.0.0.1:0"}), "HEALTH_ADDR"},
		{"same address", with(map[string]string{"HEALTH_ADDR": ":8080"}), "HEALTH_ADDR"},
		{"proxy host name", with(map[string]string{"TRUSTED_PROXIES": "proxy.example.test"}), "TRUSTED_PROXIES"},
		{"proxy range", with(map[string]string{"TRUSTED_PROXIES": "10.0.0.0/33"}), "TRUSTED_PROXIES"},
		{"one bad proxy in a list", with(map[string]string{"TRUSTED_PROXIES": "10.0.0.1,nope"}), "TRUSTED_PROXIES"},
		{"empty proxy entry", with(map[string]string{"TRUSTED_PROXIES": "10.0.0.1,,10.0.0.2"}), "TRUSTED_PROXIES"},
		{"wide mapped range", with(map[string]string{"TRUSTED_PROXIES": "::ffff:0.0.0.0/64"}), "TRUSTED_PROXIES"},
		{"pool size", with(map[string]string{"DB_MAX_CONNS": "0"}), "DB_MAX_CONNS"},
		{"not a number", with(map[string]string{"MAX_OPEN_TICKETS": "many"}), "MAX_OPEN_TICKETS"},
		{"not a bool", with(map[string]string{"MIGRATE_ON_START": "maybe"}), "MIGRATE_ON_START"},
		{"items too high", with(map[string]string{"MAX_ITEMS_PER_REQUEST": "101"}), "MAX_ITEMS_PER_REQUEST"},
		{"days too low", with(map[string]string{"MAX_DURATION_DAYS": "0"}), "MAX_DURATION_DAYS"},
		{"sign in limit too low", with(map[string]string{"GUEST_SIGNIN_PER_HOUR": "0"}), "GUEST_SIGNIN_PER_HOUR"},
		{"sign in limit too high", with(map[string]string{"GUEST_SIGNIN_PER_HOUR": "100001"}), "GUEST_SIGNIN_PER_HOUR"},
		{"sign in limit not a number", with(map[string]string{"GUEST_SIGNIN_PER_HOUR": "many"}), "GUEST_SIGNIN_PER_HOUR"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.environ)
			if err == nil {
				t.Fatal("parse accepted the value")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error %q does not name %s", err, tc.want)
			}

			for _, leak := range []string{"s3cret-sample", "pw-sample"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the error %q holds a secret value", err)
				}
			}
		})
	}
}

func TestSecretIsRedacted(t *testing.T) {
	cfg, err := parse(baseEnv())
	if err != nil {
		t.Fatalf("parse failed %v", err)
	}

	printed := []string{
		cfg.DatabaseURL.String(),
		fmt.Sprintf("%v", cfg.DatabaseURL),
		fmt.Sprintf("%+v", cfg.DatabaseURL),
		fmt.Sprintf("%#v", cfg.DatabaseURL),
		fmt.Sprintf("%s", cfg.DatabaseURL),
		fmt.Sprintf("%q", cfg.DatabaseURL),
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg),
		cfg.DatabaseURL.LogValue().String(),
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal failed %v", err)
	}

	printed = append(printed, string(encoded))

	for _, text := range printed {
		if strings.Contains(text, "s3cret-sample") || strings.Contains(text, "db.example.test") {
			t.Fatalf("a printed form holds the database URL, %q", text)
		}
	}

	if cfg.DatabaseURL.String() != "[redacted]" {
		t.Fatalf("String is %q", cfg.DatabaseURL.String())
	}
}

// TestEverySettingIsDocumented keeps .env.example and the table of the README in step with the settings of the loader.
func TestEverySettingIsDocumented(t *testing.T) {
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example %v", err)
	}

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README.md %v", err)
	}

	for field := range reflect.TypeFor[Config]().Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("env"), ",")
		if name == "" || name == "-" {
			continue
		}

		if !strings.Contains(string(example), "\n"+name+"=") && !strings.HasPrefix(string(example), name+"=") {
			t.Fatalf(".env.example has no line for %s", name)
		}

		if !strings.Contains(string(readme), "| `"+name+"` |") {
			t.Fatalf("the Configuration table of the README has no row for %s", name)
		}
	}
}
