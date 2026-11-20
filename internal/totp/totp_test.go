package totp

import (
	"net/url"
	"testing"
	"time"
)

// rfcSecret is the secret of the test vectors in Appendix B of RFC 6238.
var rfcSecret = []byte("12345678901234567890")

// TestCodeMatchesTheRFCVectors uses the SHA-1 vectors of RFC 6238. The RFC lists 8 digits and this package makes 6, which are the
// last 6 digits of the same number.
func TestCodeMatchesTheRFCVectors(t *testing.T) {
	tests := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}

	for _, tc := range tests {
		step := Step(time.Unix(tc.unix, 0))
		if got := Code(rfcSecret, step); got != tc.want {
			t.Fatalf("at %d the code is %s, want %s", tc.unix, got, tc.want)
		}
	}
}

func TestMatchAcceptsOneStepEachSide(t *testing.T) {
	// 1111111109 is in step 37037036, which the RFC lists as 081804
	at := time.Unix(1111111109, 0)
	now := Step(at)

	tests := []struct {
		name     string
		at       time.Time
		wantStep int64
		wantOK   bool
	}{
		{"same step", at, now, true},
		{"one step later", at.Add(StepSeconds * time.Second), now, true},
		{"one step earlier", at.Add(-StepSeconds * time.Second), now, true},
		{"two steps later", at.Add(2 * StepSeconds * time.Second), 0, false},
		{"two steps earlier", at.Add(-2 * StepSeconds * time.Second), 0, false},
	}

	for _, tc := range tests {
		step, ok := Match(rfcSecret, Code(rfcSecret, now), tc.at)
		if ok != tc.wantOK || step != tc.wantStep {
			t.Fatalf("%s gave %d and %v, want %d and %v", tc.name, step, ok, tc.wantStep, tc.wantOK)
		}
	}
}

func TestMatchRefusesABadCode(t *testing.T) {
	at := time.Unix(1111111109, 0)
	good := Code(rfcSecret, Step(at))

	other := "000000"
	if good == other {
		other = "000001"
	}

	for _, code := range []string{"", other, good[:5], good + "0", "08180a", " 81804", "٠٨١٨٠٤"} {
		if _, ok := Match(rfcSecret, code, at); ok {
			t.Fatalf("%q was accepted", code)
		}
	}

	if _, ok := Match([]byte("another secret of 20 b"), good, at); ok {
		t.Fatal("a code of another secret was accepted")
	}
}

func TestEncodeSecretIsBase32WithoutPadding(t *testing.T) {
	// the Base32 form of the RFC secret, which authenticator apps take
	if got := EncodeSecret(rfcSecret); got != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatalf("the text is %q", got)
	}
}

func TestNewSecret(t *testing.T) {
	first, err := NewSecret()
	if err != nil || len(first) != SecretLength {
		t.Fatalf("the secret has %d bytes and the error %v", len(first), err)
	}

	second, _ := NewSecret()
	if string(first) == string(second) {
		t.Fatal("two secrets are equal")
	}
}

func TestURI(t *testing.T) {
	raw := URI("LabMGM", "admin@example.org", rfcSecret)

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the address does not parse %v", err)
	}

	query := parsed.Query()

	if parsed.Scheme != "otpauth" || parsed.Host != "totp" || parsed.Path != "/LabMGM:admin@example.org" {
		t.Fatalf("the address is %q", raw)
	}

	want := map[string]string{
		"secret": "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "issuer": "LabMGM", "algorithm": "SHA1", "digits": "6", "period": "30",
	}

	for key, value := range want {
		if query.Get(key) != value {
			t.Fatalf("%s is %q, want %q in %q", key, query.Get(key), value, raw)
		}
	}
}
