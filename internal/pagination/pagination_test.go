package pagination_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/secretbox"
)

func newCodec(t *testing.T, seed string) *pagination.Codec {
	t.Helper()

	box, err := secretbox.New([]byte(strings.Repeat(seed, secretbox.KeyLength)[:secretbox.KeyLength]))
	if err != nil {
		t.Fatalf("make the box %v", err)
	}

	return pagination.NewCodec(box)
}

func encodeRaw(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestParseLimit(t *testing.T) {
	tests := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 50, false},
		{"1", 1, false},
		{"100", 100, false},
		{"0", 0, true},
		{"101", 0, true},
		{"-5", 0, true},
		{"abc", 0, true},
		{"10.5", 0, true},
		{" 10", 0, true},
	}

	for _, tc := range tests {
		got, err := pagination.ParseLimit(tc.raw)
		if tc.wantErr != (err != nil) || (err != nil && !errors.Is(err, pagination.ErrLimit)) || got != tc.want {
			t.Fatalf("limit %q gives %d, %v", tc.raw, got, err)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	codec := newCodec(t, "a")

	cursor := codec.Encode("units", "MGM-EXM-003", "second key")

	keys, err := codec.Decode("units", cursor, 2)
	if err != nil || len(keys) != 2 || keys[0] != "MGM-EXM-003" || keys[1] != "second key" {
		t.Fatalf("decode gives %v, %v", keys, err)
	}

	if len(cursor) > pagination.MaxCursorLength {
		t.Fatalf("the cursor has %d characters", len(cursor))
	}

	first, err := codec.Decode("units", "", 2)
	if first != nil || err != nil {
		t.Fatalf("an empty cursor gives %v, %v", first, err)
	}
}

func TestCursorRefusesWhatTheServiceDidNotMake(t *testing.T) {
	codec := newCodec(t, "a")
	good := codec.Encode("units", "MGM-EXM-003")
	body, tag, _ := strings.Cut(good, ".")

	forged, _ := json.Marshal([]string{"MGM-EXM-999"})

	tests := []struct {
		name, kind, cursor string
		keys               int
	}{
		{"another list", "models", good, 1},
		{"another key count", "units", good, 2},
		{"another key of the service", "units", newCodec(t, "b").Encode("units", "MGM-EXM-003"), 1},
		{"a changed payload", "units", encodeRaw(forged) + "." + tag, 1},
		{"a missing tag", "units", body, 1},
		{"an empty tag", "units", body + ".", 1},
		{"a payload that is not Base64", "units", "!!!." + tag, 1},
		{"a tag that is not Base64", "units", body + ".!!!", 1},
		{"text", "units", "hello", 1},
		{"a payload that is not a list", "units", encodeRaw([]byte(`{"a":1}`)) + "." + tag, 1},
		{"a cursor over the length limit", "units", strings.Repeat("a", pagination.MaxCursorLength+1), 1},
	}

	for _, tc := range tests {
		if keys, err := codec.Decode(tc.kind, tc.cursor, tc.keys); !errors.Is(err, pagination.ErrCursor) || keys != nil {
			t.Fatalf("%s gives %v, %v", tc.name, keys, err)
		}
	}
}

func TestTrim(t *testing.T) {
	rows := []int{1, 2, 3}

	if got, more := pagination.Trim(rows, 2); len(got) != 2 || !more {
		t.Fatalf("a long page gives %v, %v", got, more)
	}

	if got, more := pagination.Trim(rows, 3); len(got) != 3 || more {
		t.Fatalf("a full page gives %v, %v", got, more)
	}

	if got, more := pagination.Trim([]int{}, 3); len(got) != 0 || more {
		t.Fatalf("an empty page gives %v, %v", got, more)
	}
}

func TestPageAnswersAnEmptyListAsAnArray(t *testing.T) {
	raw, err := json.Marshal(pagination.Page[int]{Items: []int{}, Total: 0})
	if err != nil || string(raw) != `{"items":[],"total":0}` {
		t.Fatalf("the page is %s, %v", raw, err)
	}
}

func TestTheCursorOfTheLongestNameFitsTheLimit(t *testing.T) {
	codec := newCodec(t, "a")

	// 100 four byte characters, and 100 characters that a default JSON encoder would write as six bytes
	for _, key := range []string{strings.Repeat("\U0001F600", 100), strings.Repeat("<", 100), strings.Repeat("\x01", 100)} {
		cursor := codec.Encode("models", key)

		if len(cursor) > pagination.MaxCursorLength {
			t.Fatalf("a cursor of %d characters is over the limit", len(cursor))
		}

		if keys, err := codec.Decode("models", cursor, 1); err != nil || len(keys) != 1 || keys[0] != key {
			t.Fatalf("the cursor does not read back, %v", err)
		}
	}
}
