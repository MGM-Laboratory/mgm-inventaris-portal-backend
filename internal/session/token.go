package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const (
	tokenBytes = 32
	// tokenLength is the length of a token in text, which is 32 bytes in base64 without padding.
	tokenLength = 43
)

// NewToken returns a random 256 bit value in base64url. It serves as a session token and as a CSRF token.
func NewToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// HashToken returns the SHA-256 hash that the table stores in place of the token.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))

	return sum[:]
}

// validToken says whether a cookie value has the shape of a token, so a junk value never reaches the database.
func validToken(token string) bool {
	if len(token) != tokenLength {
		return false
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)

	return err == nil && len(raw) == tokenBytes
}
