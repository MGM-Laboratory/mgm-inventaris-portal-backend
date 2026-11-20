package password

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

func newHasher() *Hasher {
	return New(2, 5*time.Second)
}

// TestVerifyReferenceVectors checks hashes that the reference implementation of Argon2 made, the same vectors that golang.org/x/crypto
// uses. The password is "password" and the salt is "somesalt". It proves that the encoding and the algorithm are the standard ones.
func TestVerifyReferenceVectors(t *testing.T) {
	tests := []struct {
		memory, passes, lanes int
		hash                  string
	}{
		{64, 1, 1, "655ad15eac652dc59f7170a7332bf49b8469be1fdb9c28bb"},
		{64, 2, 1, "068d62b26455936aa6ebe60060b0a65870dbfa3ddf8d41f7"},
		{64, 2, 2, "350ac37222f436ccb5c0972f1ebd3bf6b958bf2071841362"},
	}

	h := newHasher()

	for _, tc := range tests {
		key, err := hex.DecodeString(tc.hash)
		if err != nil {
			t.Fatalf("the vector is not hex %v", err)
		}

		encoded := fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", tc.memory, tc.passes, tc.lanes,
			base64.RawStdEncoding.EncodeToString([]byte("somesalt")), base64.RawStdEncoding.EncodeToString(key))

		if ok, err := h.Verify(context.Background(), "password", encoded); err != nil || !ok {
			t.Fatalf("the reference hash %s gave %v and %v", tc.hash, ok, err)
		}

		if ok, err := h.Verify(context.Background(), "Password", encoded); err != nil || ok {
			t.Fatalf("a wrong password gave %v and %v", ok, err)
		}
	}
}

func TestHashAndVerify(t *testing.T) {
	h := newHasher()
	ctx := context.Background()

	first, err := h.Hash(ctx, "example password one")
	if err != nil {
		t.Fatalf("hash failed %v", err)
	}

	second, err := h.Hash(ctx, "example password one")
	if err != nil {
		t.Fatalf("hash failed %v", err)
	}

	if !strings.HasPrefix(first, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("the hash is %q", first)
	}

	if first == second {
		t.Fatal("two hashes of one password are equal, so the salt is not random")
	}

	for _, tc := range []struct {
		typed string
		want  bool
	}{
		{"example password one", true},
		{"example password two", false},
		{"Example password one", false},
		{"", false},
	} {
		ok, err := h.Verify(ctx, tc.typed, first)
		if err != nil || ok != tc.want {
			t.Fatalf("%q gave %v and %v, want %v", tc.typed, ok, err, tc.want)
		}
	}
}

// TestVerifyReadsTheParametersOfTheHash makes a hash with a cost that this package no longer uses and checks that it still verifies,
// so a later change of the cost does not lock anybody out.
func TestVerifyReadsTheParametersOfTheHash(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("older password"), salt, 1, 8*1024, 2, 20)
	encoded := fmt.Sprintf("$argon2id$v=19$m=8192,t=1,p=2$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))

	h := newHasher()

	ok, err := h.Verify(context.Background(), "older password", encoded)
	if err != nil || !ok {
		t.Fatalf("a hash with other parameters gave %v and %v", ok, err)
	}

	ok, _ = h.Verify(context.Background(), "newer password", encoded)
	if ok {
		t.Fatal("a wrong password matched")
	}
}

func TestVerifyRefusesAMalformedOrGreedyHash(t *testing.T) {
	good := "$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHQ$" + base64.RawStdEncoding.EncodeToString(make([]byte, 16))

	tests := map[string]string{
		"empty":             "",
		"plain text":        "password",
		"other algorithm":   strings.Replace(good, "argon2id", "argon2i", 1),
		"old version":       strings.Replace(good, "v=19", "v=16", 1),
		"too much memory":   strings.Replace(good, "m=64", "m=999999", 1),
		"no memory":         strings.Replace(good, "m=64", "m=0", 1),
		"no passes":         strings.Replace(good, "t=1", "t=0", 1),
		"too many passes":   strings.Replace(good, "t=1", "t=11", 1),
		"too many lanes":    strings.Replace(good, "p=1", "p=5", 1),
		"no lanes":          strings.Replace(good, "p=1", "p=0", 1),
		"salt not base64":   strings.Replace(good, "c29tZXNhbHQ", "!!!", 1),
		"short key":         "$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHQ$AAAA",
		"key not base64":    "$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHQ$!!!!",
		"too many sections": good + "$extra",
		"too few sections":  "$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHQ",
	}

	h := newHasher()

	for name, encoded := range tests {
		ok, err := h.Verify(context.Background(), "anything", encoded)
		if err != nil || ok {
			t.Fatalf("%s gave %v and %v", name, ok, err)
		}
	}
}

// TestBusyHasherGivesUp starts more checks than there are slots. The wait is shorter than one hash, so some of them give up and
// none hangs.
func TestBusyHasherGivesUp(t *testing.T) {
	h := New(1, time.Millisecond)
	encoded, err := encode("example password")
	if err != nil {
		t.Fatalf("encode failed %v", err)
	}

	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		busy, served int
	)

	for range 6 {
		wg.Go(func() {
			ok, err := h.Verify(context.Background(), "example password", encoded)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case errors.Is(err, ErrBusy):
				busy++
			case err == nil && ok:
				served++
			}
		})
	}

	wg.Wait()

	if busy == 0 || served == 0 || busy+served != 6 {
		t.Fatalf("%d were busy and %d were served", busy, served)
	}
}

func TestACancelledContextGivesUpWaiting(t *testing.T) {
	h := New(1, time.Minute)
	h.slots <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.Hash(ctx, "example password"); !errors.Is(err, ErrBusy) {
		t.Fatalf("the error is %v", err)
	}
}

func TestVerifyDummyCostsAHashAndFindsNothing(t *testing.T) {
	h := newHasher()

	start := time.Now()
	if err := h.VerifyDummy(context.Background()); err != nil {
		t.Fatalf("the dummy check failed %v", err)
	}

	// a real check of 19 MiB takes more than a few milliseconds, so a skipped one would show
	if elapsed := time.Since(start); elapsed < 2*time.Millisecond {
		t.Fatalf("the dummy check took %v, so it did no work", elapsed)
	}
}

func TestCheckNew(t *testing.T) {
	tests := []struct {
		candidate, current, want string
	}{
		{strings.Repeat("a", 12), "", ""},
		{strings.Repeat("a", 11), "", ProblemTooShort},
		{"", "", ProblemTooShort},
		{strings.Repeat("a", 128), "", ""},
		{strings.Repeat("a", 129), "", ProblemTooLong},
		{strings.Repeat("é", 12), "", ""},
		{strings.Repeat("é", 128), "", ""},
		{strings.Repeat("é", 129), "", ProblemTooLong},
		{"example password one", "example password one", ProblemUnchanged},
		{"example password one", "example password two", ""},
		{"Example password one", "example password one", ""},
	}

	for _, tc := range tests {
		if got := CheckNew(tc.candidate, tc.current); got != tc.want {
			t.Fatalf("%d characters against %d gave %q, want %q", len([]rune(tc.candidate)), len([]rune(tc.current)), got, tc.want)
		}
	}
}
