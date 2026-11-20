// Package password hashes and checks passwords with Argon2id, and holds the rules for a new password. The memory of one hash is
// about 19 MiB, so a semaphore keeps a burst of sign ins from using the memory of the host.
package password

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// The cost of one hash, which is the first choice of the OWASP password cheat sheet for Argon2id.
const (
	memoryKiB  = 19 * 1024
	iterations = 2
	lanes      = 1
	saltLength = 16
	keyLength  = 32
)

// The most that Verify accepts from a stored hash, so a forged row cannot make one check use the memory of the host.
const (
	maxMemoryKiB  = 4 * memoryKiB
	maxIterations = 10
	maxLanes      = 4
	minKeyLength  = 16
	maxKeyLength  = 64
)

// ErrBusy means that every slot for a hash stayed taken until the wait ended.
var ErrBusy = errors.New("too many passwords are being checked")

// Hasher hashes and checks passwords, and lets only a few run at the same time.
type Hasher struct {
	slots chan struct{}
	wait  time.Duration
}

// New returns a hasher that runs at most concurrency hashes at once. A caller that cannot get a slot within wait gets ErrBusy.
func New(concurrency int, wait time.Duration) *Hasher {
	// the dummy hash is made now, so the first sign in with an unknown email costs the same as every later one
	dummy()

	return &Hasher{slots: make(chan struct{}, max(concurrency, 1)), wait: wait}
}

func (h *Hasher) acquire(ctx context.Context) error {
	timer := time.NewTimer(h.wait)
	defer timer.Stop()

	select {
	case h.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrBusy
	case <-ctx.Done():
		return ErrBusy
	}
}

func (h *Hasher) release() { <-h.slots }

// Hash returns the encoded Argon2id hash of a password, with a new salt and the cost of this package.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()

	return encode(password)
}

func encode(password string) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("make the salt %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, lanes, keyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, memoryKiB, iterations, lanes,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify says whether the password matches the encoded hash. The cost parameters are read from the hash, so a hash that an older
// setting made still verifies. A hash that is malformed or asks for more than this package ever made never matches.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (bool, error) {
	params, ok := parse(encoded)
	if !ok {
		return false, nil
	}

	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()

	// the key length is at most maxKeyLength, which parse checked
	got := argon2.IDKey([]byte(password), params.salt, params.iterations, params.memory, params.lanes, uint32(len(params.key))) //nolint:gosec // bounded by parse

	return subtle.ConstantTimeCompare(got, params.key) == 1, nil
}

// dummy is a hash with the current cost, made once. A sign in with an email that is not an account verifies against it, so the
// answer takes as long as one with a wrong password.
var dummy = sync.OnceValue(func() string {
	hash, err := encode("not the password of anyone")
	if err != nil {
		return ""
	}

	return hash
})

// VerifyDummy spends the time of a real check and always reports a mismatch.
func (h *Hasher) VerifyDummy(ctx context.Context) error {
	_, err := h.Verify(ctx, "another password of nobody", dummy())

	return err
}

type hashParams struct {
	memory, iterations uint32
	lanes              uint8
	salt, key          []byte
}

func parse(encoded string) (hashParams, bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return hashParams{}, false
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return hashParams{}, false
	}

	var p hashParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.lanes); err != nil {
		return hashParams{}, false
	}

	if p.memory == 0 || p.memory > maxMemoryKiB || p.iterations == 0 || p.iterations > maxIterations || p.lanes == 0 || p.lanes > maxLanes {
		return hashParams{}, false
	}

	var err error

	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) == 0 {
		return hashParams{}, false
	}

	if p.key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.key) < minKeyLength || len(p.key) > maxKeyLength {
		return hashParams{}, false
	}

	return p, true
}
