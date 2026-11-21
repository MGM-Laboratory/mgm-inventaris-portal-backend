package secretbox

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func key(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, KeyLength)
}

func TestSealAndOpen(t *testing.T) {
	box, err := New(key(1))
	if err != nil {
		t.Fatalf("new failed %v", err)
	}

	plain := []byte("a secret of the example")
	ctx := []byte("row one")

	first, err := box.Seal(plain, ctx)
	if err != nil {
		t.Fatalf("seal failed %v", err)
	}

	second, _ := box.Seal(plain, ctx)
	if first == second {
		t.Fatal("two seals of one value are equal, so the nonce is not random")
	}

	if strings.Contains(first, "secret") {
		t.Fatal("the sealed text shows the value")
	}

	if !strings.HasPrefix(first, "v1.") {
		t.Fatalf("the sealed text %q has no format prefix", first)
	}

	got, err := box.Open(first, ctx)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open gave %q and %v", got, err)
	}
}

func TestOpenRefusesWhatIsNotTheSameSeal(t *testing.T) {
	box, _ := New(key(1))
	other, _ := New(key(2))

	sealed, err := box.Seal([]byte("a secret of the example"), []byte("row one"))
	if err != nil {
		t.Fatalf("seal failed %v", err)
	}

	damaged := []byte(sealed)
	damaged[len(damaged)/2] ^= 1

	tests := map[string]func() error{
		"another context": func() error { _, err := box.Open(sealed, []byte("row two")); return err },
		"another key":     func() error { _, err := other.Open(sealed, []byte("row one")); return err },
		"damaged":         func() error { _, err := box.Open(string(damaged), []byte("row one")); return err },
		"empty":           func() error { _, err := box.Open("", []byte("row one")); return err },
		"not base64":      func() error { _, err := box.Open("!!!", []byte("row one")); return err },
		"too short":       func() error { _, err := box.Open("v1.QUJD", []byte("row one")); return err },
		"no prefix":       func() error { _, err := box.Open(strings.TrimPrefix(sealed, "v1."), []byte("row one")); return err },
		"other prefix": func() error {
			_, err := box.Open("v2."+strings.TrimPrefix(sealed, "v1."), []byte("row one"))
			return err
		},
	}

	for name, run := range tests {
		if err := run(); !errors.Is(err, ErrOpen) {
			t.Fatalf("%s gave %v", name, err)
		}
	}
}

func TestNewNeedsTheRightLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := New(make([]byte, n)); err == nil {
			t.Fatalf("a key of %d bytes was accepted", n)
		}
	}
}

func TestDerive(t *testing.T) {
	box, _ := New(key(1))
	other, _ := New(key(2))

	a := box.Derive(20, []byte("one"), []byte("two"))

	if len(a) != 20 || !bytes.Equal(a, box.Derive(20, []byte("one"), []byte("two"))) {
		t.Fatal("the same parts do not give the same bytes")
	}

	different := map[string][]byte{
		"another part":     box.Derive(20, []byte("one"), []byte("three")),
		"another order":    box.Derive(20, []byte("two"), []byte("one")),
		"fewer parts":      box.Derive(20, []byte("one")),
		"joined parts":     box.Derive(20, []byte("onetwo")),
		"moved boundary":   box.Derive(20, []byte("on"), []byte("etwo")),
		"another key":      other.Derive(20, []byte("one"), []byte("two")),
		"seal key is used": box.Derive(20),
	}

	for name, got := range different {
		if bytes.Equal(a, got) {
			t.Fatalf("%s gave the same bytes", name)
		}
	}

	if len(box.Derive(100, []byte("x"))) != 32 {
		t.Fatal("a long request is not cut to 32 bytes")
	}
}
