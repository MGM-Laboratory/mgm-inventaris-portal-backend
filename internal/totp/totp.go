// Package totp makes and checks the one time codes of RFC 6238 with SHA-1, 6 digits and a step of 30 seconds, the profile that
// authenticator apps read.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 names HMAC-SHA1 and authenticator apps expect it
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

const (
	// SecretLength is the length of a secret in bytes. RFC 4226 asks for at least 128 bits and recommends 160.
	SecretLength = 20
	// CodeLength is the number of digits of a code.
	CodeLength = 6
	// StepSeconds is the length of one time step.
	StepSeconds = 30
	// skew is the number of steps before and after the present one that a code may come from, for a clock that is a little off.
	skew = 1
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random secret.
func NewSecret() ([]byte, error) {
	secret := make([]byte, SecretLength)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("read random bytes %w", err)
	}

	return secret, nil
}

// EncodeSecret returns the Base32 text of a secret without padding, the form that an authenticator app takes.
func EncodeSecret(secret []byte) string {
	return encoding.EncodeToString(secret)
}

// Step returns the number of the time step that holds t.
func Step(t time.Time) int64 {
	return t.Unix() / StepSeconds
}

// Code returns the code of a time step.
func Code(secret []byte, step int64) string {
	var counter [8]byte

	binary.BigEndian.PutUint64(counter[:], uint64(step)) //nolint:gosec // a step of a real clock is never negative

	mac := hmac.New(sha1.New, secret) //nolint:gosec // see the import
	mac.Write(counter[:])

	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	number := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	return fmt.Sprintf("%0*d", CodeLength, number%1_000_000)
}

// Match checks a code against the steps around t. It returns the step that matched and true, or false when none did. When two steps
// give the same code, which is rare, it returns the later one. Every step is compared in constant time.
func Match(secret []byte, code string, t time.Time) (int64, bool) {
	if len(code) != CodeLength {
		return 0, false
	}

	for _, r := range code {
		if r < '0' || r > '9' {
			return 0, false
		}
	}

	var (
		matched int64
		found   bool
		now     = Step(t)
	)

	// walking the steps in order makes a later match replace an earlier one
	for step := now - skew; step <= now+skew; step++ {
		if subtle.ConstantTimeCompare([]byte(Code(secret, step)), []byte(code)) == 1 {
			matched, found = step, true
		}
	}

	return matched, found
}

// URI returns the otpauth address that an authenticator app reads from a QR code. The issuer and the account name are shown in the
// app.
func URI(issuer, account string, secret []byte) string {
	query := url.Values{}
	query.Set("secret", EncodeSecret(secret))
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", fmt.Sprint(CodeLength))
	query.Set("period", fmt.Sprint(StepSeconds))

	label := url.PathEscape(issuer + ":" + account)

	return "otpauth://totp/" + label + "?" + query.Encode()
}
