// Package config loads the settings of the service from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/caarlos0/env/v11"
)

// The values that ENVIRONMENT accepts.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

const (
	maxPort        = 65535
	maxDBConns     = 50
	maxItems       = 100
	maxDurationCap = 365
	maxOpenCap     = 50
	maxSigninCap   = 100000
	maxFailureCap  = 1000
	maxHashCap     = 16
	totpKeyBytes   = 32
)

// Config holds the settings of the service. The names of the variables are listed in .env.example.
type Config struct {
	HTTPAddr   string `env:"HTTP_ADDR" envDefault:":8080"`
	HealthAddr string `env:"HEALTH_ADDR" envDefault:"127.0.0.1:8081"`

	// DatabaseURL is the connection URL, and it holds the password of the database.
	DatabaseURL Secret `env:"DATABASE_URL,required,notEmpty"`
	// DBMaxConns is the size of the pool of one copy. Two copies run against one database, so keep the sum well below its limit.
	DBMaxConns     uint `env:"DB_MAX_CONNS" envDefault:"8"`
	MigrateOnStart bool `env:"MIGRATE_ON_START" envDefault:"true"`

	// PublicBaseURL is the public address of the portal. The QR address of a unit is this value, /aset/ and the tag.
	PublicBaseURL string `env:"PUBLIC_BASE_URL,required,notEmpty"`
	// TrustedProxyList is the raw TRUSTED_PROXIES, a comma separated list of IP addresses and CIDR ranges. Behind a published Docker
	// port the peer is the gateway of the bridge, so a range is more useful than one address. Empty trusts no header.
	TrustedProxyList []string `env:"TRUSTED_PROXIES" envSeparator:","`
	// TrustedProxies is the parsed list. Only a request whose peer is in it may set X-Real-IP.
	TrustedProxies []netip.Prefix `env:"-"`

	Environment string `env:"ENVIRONMENT" envDefault:"production"`
	LogLevel    string `env:"LOG_LEVEL" envDefault:"info"`
	// SecureCookie sets the Secure flag of the session cookie. Only development may turn it off, for a browser on plain http.
	SecureCookie bool `env:"SECURE_COOKIE" envDefault:"true"`

	MaxItemsPerRequest int `env:"MAX_ITEMS_PER_REQUEST" envDefault:"10"`
	MaxDurationDays    int `env:"MAX_DURATION_DAYS" envDefault:"30"`
	MaxOpenTickets     int `env:"MAX_OPEN_TICKETS" envDefault:"3"`

	// GuestSigninPerHour is the most guest sign in requests that one client address may send in an hour.
	GuestSigninPerHour int `env:"GUEST_SIGNIN_PER_HOUR" envDefault:"60"`
	// TicketCreatePerHour is the most ticket requests that one client address may send in an hour. Every request counts.
	TicketCreatePerHour int `env:"TICKET_CREATE_PER_HOUR" envDefault:"20"`
	// TicketLookupAddressFailures is the most failed ticket lookups in an hour for one client address.
	TicketLookupAddressFailures int `env:"TICKET_LOOKUP_ADDRESS_FAILURES" envDefault:"20"`
	// TicketLookupNumberFailures is the most failed ticket lookups in an hour for one ticket number without its last four characters
	// together with one email.
	TicketLookupNumberFailures int `env:"TICKET_LOOKUP_NUMBER_FAILURES" envDefault:"10"`

	// AdminSigninPairFailures is the most failed admin sign ins in 15 minutes for one email from one client address. The next request
	// of that pair answers 429 until the window ends.
	AdminSigninPairFailures int `env:"ADMIN_SIGNIN_PAIR_FAILURES" envDefault:"5"`
	// AdminSigninAddressFailures is the most failed admin sign ins in an hour for one client address, whatever the email.
	AdminSigninAddressFailures int `env:"ADMIN_SIGNIN_ADDRESS_FAILURES" envDefault:"20"`
	// AdminAccountFailures is the most failed password or code checks in 15 minutes on the account routes of one admin session.
	AdminAccountFailures int `env:"ADMIN_ACCOUNT_FAILURES" envDefault:"5"`
	// PasswordHashConcurrency is the most Argon2id hashes that run at once in one copy. One hash needs about 19 MiB.
	PasswordHashConcurrency int `env:"PASSWORD_HASH_CONCURRENCY" envDefault:"2"`

	// TOTPEncryptionKey is the key that seals the TOTP secret of every admin before it is stored. It is 32 random bytes in Base64.
	// Losing it makes every stored TOTP secret unreadable, so keep a copy of it apart from the database.
	TOTPEncryptionKey Secret `env:"TOTP_ENCRYPTION_KEY,required,notEmpty"`
}

// Load reads the settings from the environment of the process and refuses a value that the service cannot run with.
func Load() (Config, error) {
	return parse(nil)
}

// parse reads the settings from environ, or from the environment of the process when environ is nil.
func parse(environ map[string]string) (Config, error) {
	var cfg Config

	if err := env.ParseWithOptions(&cfg, env.Options{Environment: environ}); err != nil {
		return Config{}, describe(err)
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if !slices.Contains([]string{EnvDevelopment, EnvProduction}, c.Environment) {
		return fmt.Errorf("ENVIRONMENT must be %s or %s", EnvDevelopment, EnvProduction)
	}

	// the flag comes from the settings and never from the request, so a spoofed X-Forwarded-Proto cannot weaken it
	if c.Environment == EnvProduction && !c.SecureCookie {
		return errors.New("SECURE_COOKIE cannot be false when ENVIRONMENT is production")
	}

	c.LogLevel = strings.ToLower(c.LogLevel)
	if _, err := c.SlogLevel(); err != nil {
		return errors.New("LOG_LEVEL must be debug, info, warn or error")
	}

	if err := checkAddr("HTTP_ADDR", c.HTTPAddr); err != nil {
		return err
	}

	if err := checkAddr("HEALTH_ADDR", c.HealthAddr); err != nil {
		return err
	}

	if c.HTTPAddr == c.HealthAddr {
		return errors.New("HTTP_ADDR and HEALTH_ADDR must be different")
	}

	// the error names the setting only, because the URL holds the password
	dbURL, err := url.Parse(c.DatabaseURL.Reveal())
	if err != nil || (dbURL.Scheme != "postgres" && dbURL.Scheme != "postgresql") {
		return errors.New("DATABASE_URL must be a postgres:// or postgresql:// URL")
	}

	if c.DBMaxConns == 0 || c.DBMaxConns > maxDBConns {
		return fmt.Errorf("DB_MAX_CONNS is %d and must be between 1 and %d", c.DBMaxConns, maxDBConns)
	}

	if err := c.validatePublicBaseURL(); err != nil {
		return err
	}

	proxies, err := parseProxies(c.TrustedProxyList)
	if err != nil {
		return err
	}

	c.TrustedProxies = proxies

	return c.validateLimits()
}

func (c *Config) validatePublicBaseURL() error {
	base, err := url.Parse(c.PublicBaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" {
		return errors.New("PUBLIC_BASE_URL must be an absolute http or https URL such as https://portal.example.org")
	}

	if base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return errors.New("PUBLIC_BASE_URL must hold only the scheme and the host, with no user name, path, query or fragment")
	}

	// the session cookie is Secure, so a production portal that is not on https cannot work
	if c.Environment == EnvProduction && base.Scheme != "https" {
		return errors.New("PUBLIC_BASE_URL must use https in production")
	}

	host := strings.ToLower(base.Host)

	// a browser leaves the default port out of the Origin header, so the stored origin has to leave it out as well
	if (base.Scheme == "https" && base.Port() == "443") || (base.Scheme == "http" && base.Port() == "80") {
		host = strings.ToLower(base.Hostname())
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}

	c.PublicBaseURL = base.Scheme + "://" + host

	return nil
}

func (c *Config) validateLimits() error {
	if c.MaxItemsPerRequest < 1 || c.MaxItemsPerRequest > maxItems {
		return fmt.Errorf("MAX_ITEMS_PER_REQUEST is %d and must be between 1 and %d", c.MaxItemsPerRequest, maxItems)
	}

	if c.MaxDurationDays < 1 || c.MaxDurationDays > maxDurationCap {
		return fmt.Errorf("MAX_DURATION_DAYS is %d and must be between 1 and %d", c.MaxDurationDays, maxDurationCap)
	}

	if c.MaxOpenTickets < 1 || c.MaxOpenTickets > maxOpenCap {
		return fmt.Errorf("MAX_OPEN_TICKETS is %d and must be between 1 and %d", c.MaxOpenTickets, maxOpenCap)
	}

	if c.GuestSigninPerHour < 1 || c.GuestSigninPerHour > maxSigninCap {
		return fmt.Errorf("GUEST_SIGNIN_PER_HOUR is %d and must be between 1 and %d", c.GuestSigninPerHour, maxSigninCap)
	}

	if c.TicketCreatePerHour < 1 || c.TicketCreatePerHour > maxSigninCap {
		return fmt.Errorf("TICKET_CREATE_PER_HOUR is %d and must be between 1 and %d", c.TicketCreatePerHour, maxSigninCap)
	}

	for _, limit := range []struct {
		name  string
		value int
		most  int
	}{
		{"ADMIN_SIGNIN_PAIR_FAILURES", c.AdminSigninPairFailures, maxFailureCap},
		{"ADMIN_SIGNIN_ADDRESS_FAILURES", c.AdminSigninAddressFailures, maxFailureCap},
		{"ADMIN_ACCOUNT_FAILURES", c.AdminAccountFailures, maxFailureCap},
		{"TICKET_LOOKUP_ADDRESS_FAILURES", c.TicketLookupAddressFailures, maxFailureCap},
		{"TICKET_LOOKUP_NUMBER_FAILURES", c.TicketLookupNumberFailures, maxFailureCap},
		{"PASSWORD_HASH_CONCURRENCY", c.PasswordHashConcurrency, maxHashCap},
	} {
		if limit.value < 1 || limit.value > limit.most {
			return fmt.Errorf("%s is %d and must be between 1 and %d", limit.name, limit.value, limit.most)
		}
	}

	return c.validateTOTPKey()
}

// validateTOTPKey checks the shape of the key and never names its value.
func (c *Config) validateTOTPKey() error {
	key, err := base64.StdEncoding.DecodeString(c.TOTPEncryptionKey.Reveal())
	if err != nil || len(key) != totpKeyBytes {
		return fmt.Errorf("TOTP_ENCRYPTION_KEY must be %d random bytes in Base64, for example the output of openssl rand -base64 %d", totpKeyBytes, totpKeyBytes)
	}

	// 32 zero bytes is the placeholder of .env.example, and no random key looks like that
	if !slices.ContainsFunc(key, func(b byte) bool { return b != 0 }) {
		return errors.New("TOTP_ENCRYPTION_KEY is the placeholder of .env.example, so make a key with openssl rand -base64 32")
	}

	return nil
}

// TOTPKey returns the decoded key. Load has checked it, so a Config that Load made never fails here.
func (c Config) TOTPKey() []byte {
	key, err := base64.StdEncoding.DecodeString(c.TOTPEncryptionKey.Reveal())
	if err != nil {
		return nil
	}

	return key
}

func (c Config) SlogLevel() (slog.Level, error) {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errors.New("unknown log level")
	}
}

func checkAddr(name, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s must look like host:port or :port", name)
	}

	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return fmt.Errorf("%s needs a port from 1 to %d", name, maxPort)
	}

	return nil
}

// describe names the setting of each failure and leaves the bad value out, because a value can be a secret.
func describe(err error) error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return errors.New("the settings could not be read from the environment")
	}

	var problems []error

	for _, item := range agg.Errors {
		var (
			parseErr env.ParseError
			unset    env.VarIsNotSetError
			empty    env.EmptyVarError
		)

		switch {
		case errors.As(item, &parseErr):
			problems = append(problems, fmt.Errorf("%s is not a valid %s", settingName(parseErr.Name), parseErr.Type.Kind()))
		case errors.As(item, &unset):
			problems = append(problems, fmt.Errorf("%s is required and is not set", unset.Key))
		case errors.As(item, &empty):
			problems = append(problems, fmt.Errorf("%s must not be empty", empty.Key))
		default:
			problems = append(problems, errors.New("a setting could not be read"))
		}
	}

	return errors.Join(problems...)
}

// settingName turns the name of a field of Config into the name of its environment variable.
func settingName(field string) string {
	if f, ok := reflect.TypeFor[Config]().FieldByName(field); ok {
		if name, _, _ := strings.Cut(f.Tag.Get("env"), ","); name != "" {
			return name
		}
	}

	return field
}
