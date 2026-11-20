package admin

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestNewRecoveryCodes(t *testing.T) {
	codes, hashes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		t.Fatalf("make failed %v", err)
	}

	if len(codes) != 10 || len(hashes) != 10 {
		t.Fatalf("there are %d codes and %d hashes", len(codes), len(hashes))
	}

	shape := regexp.MustCompile(`^[A-HJ-NP-Z2-9]{5}-[A-HJ-NP-Z2-9]{5}$`)

	for i, code := range codes {
		if !shape.MatchString(code) {
			t.Fatalf("the code %d has a bad shape", i)
		}

		if hashes[i] != HashRecoveryCode(code) {
			t.Fatalf("the hash %d is not the hash of its code", i)
		}
	}

	sorted := slices.Clone(codes)
	slices.Sort(sorted)

	if len(slices.Compact(sorted)) != len(codes) {
		t.Fatal("two codes are equal")
	}
}

func TestEveryCharacterOfTheAlphabetIsReachable(t *testing.T) {
	// 32 characters make 5 bits each, which is why a byte can be reduced by a remainder with no bias
	if len(recoveryAlphabet) != 32 || strings.ContainsAny(recoveryAlphabet, "IO01") {
		t.Fatalf("the alphabet is %q", recoveryAlphabet)
	}

	seen := map[rune]bool{}

	for range 40 {
		codes, _, err := NewRecoveryCodes(RecoveryCodeCount)
		if err != nil {
			t.Fatalf("make failed %v", err)
		}

		for _, code := range codes {
			for _, r := range code {
				seen[r] = true
			}
		}
	}

	// 4000 characters leave a gap with a probability far below one in a billion
	if len(seen) != 33 {
		t.Fatalf("%d different characters were seen, want the 32 of the alphabet and the hyphen", len(seen))
	}
}

func TestNormalizeRecoveryCode(t *testing.T) {
	tests := []struct {
		typed, want string
		ok          bool
	}{
		{"ABCDE-FGHJK", "ABCDEFGHJK", true},
		{"abcde-fghjk", "ABCDEFGHJK", true},
		{"  abcde fghjk  ", "ABCDEFGHJK", true},
		{"ABCDEFGHJK", "ABCDEFGHJK", true},
		{"AB-CD-EF-GH-JK", "ABCDEFGHJK", true},
		{"ABCDE-FGHJ0", "ABCDEFGHJ0", false},
		{"ABCDE-FGHJI", "ABCDEFGHJI", false},
		{"ABCDE-FGHJ", "ABCDEFGHJ", false},
		{"ABCDE-FGHJKL", "ABCDEFGHJKL", false},
		{"", "", false},
	}

	for _, tc := range tests {
		got, ok := NormalizeRecoveryCode(tc.typed)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%q gave %q and %v, want %q and %v", tc.typed, got, ok, tc.want, tc.ok)
		}
	}

	if HashRecoveryCode("abcde-fghjk") != HashRecoveryCode("ABCDE FGHJK") || HashRecoveryCode("ABCDE-FGHJK") == HashRecoveryCode("ABCDE-FGHJL") {
		t.Fatal("the hash does not follow the normal form")
	}
}

func TestFindHash(t *testing.T) {
	stored := []string{"aa", "bb", "cc"}

	if got, ok := findHash(stored, "bb"); !ok || got != "bb" {
		t.Fatalf("a stored hash gave %q and %v", got, ok)
	}

	for _, candidate := range []string{"", "b", "bbb", "dd", "AA"} {
		if _, ok := findHash(stored, candidate); ok {
			t.Fatalf("%q was found", candidate)
		}
	}

	if _, ok := findHash(nil, "aa"); ok {
		t.Fatal("a hash was found in an empty list")
	}
}
