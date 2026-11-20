package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// RecoveryCodeCount is the number of codes that an admin gets at a time.
	RecoveryCodeCount = 10
	// recoveryAlphabet has 32 characters, so one character holds 5 bits. It leaves out I, O, 0 and 1, which look like each other.
	recoveryAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	recoveryGroup    = 5
	recoveryLength   = 2 * recoveryGroup
)

// NewRecoveryCodes returns n codes of the form XXXXX-XXXXX, and the hash of each one in the same order. The hash is what the table
// stores.
func NewRecoveryCodes(n int) ([]string, []string, error) {
	var codes, hashes []string

	for range n {
		raw := make([]byte, recoveryLength)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, fmt.Errorf("read random bytes %w", err)
		}

		var code strings.Builder

		for i, b := range raw {
			if i == recoveryGroup {
				code.WriteByte('-')
			}

			// 256 is a multiple of 32, so the remainder is uniform
			code.WriteByte(recoveryAlphabet[int(b)%len(recoveryAlphabet)])
		}

		codes = append(codes, code.String())
		hashes = append(hashes, HashRecoveryCode(code.String()))
	}

	return codes, hashes, nil
}

// NormalizeRecoveryCode puts what an admin typed in the form of a stored code, with no hyphen or space and in upper case. The second
// value is false when the result cannot be a code.
func NormalizeRecoveryCode(typed string) (string, bool) {
	normal := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(typed))
	if len(normal) != recoveryLength || strings.Trim(normal, recoveryAlphabet) != "" {
		return normal, false
	}

	return normal, true
}

// HashRecoveryCode returns the hex SHA-256 hash of a code after normalization. A plain hash is enough here and a slow one is not
// needed, because a code is random with 50 bits and a guess is limited by the rate limits, so there is no password to guess offline.
func HashRecoveryCode(code string) string {
	normal, _ := NormalizeRecoveryCode(code)
	sum := sha256.Sum256([]byte(normal))

	return hex.EncodeToString(sum[:])
}

// findHash returns the stored hash that equals the candidate. It compares every stored hash in constant time and never stops at the
// first match, so the time does not show which code of the admin was close.
func findHash(stored []string, candidate string) (string, bool) {
	var (
		found string
		ok    int
	)

	for _, hash := range stored {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(candidate)) == 1 {
			found, ok = hash, 1
		}
	}

	return found, ok == 1
}
