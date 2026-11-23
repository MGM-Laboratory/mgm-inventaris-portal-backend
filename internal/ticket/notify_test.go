package ticket

import "testing"

func TestWithoutSecretDropsTheLastPartOfANumber(t *testing.T) {
	tests := map[string]string{
		"MGM-2611-0001-K7QX": "MGM-2611-0001",
		"nodash":             "nodash",
		"":                   "",
	}

	for in, want := range tests {
		if got := WithoutSecret(in); got != want {
			t.Fatalf("%q gives %q, want %q", in, got, want)
		}
	}
}
