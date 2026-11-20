package config

import (
	"fmt"
	"io"
	"log/slog"
)

const redacted = "[redacted]"

// Secret holds a setting that must never reach a log, an error or a response. Every way of printing it writes [redacted].
type Secret string

// UnmarshalText lets the environment loader fill the setting.
func (s *Secret) UnmarshalText(text []byte) error {
	*s = Secret(text)

	return nil
}

// Reveal returns the real value, for the code that connects with it.
func (s Secret) Reveal() string {
	return string(s)
}

func (Secret) String() string {
	return redacted
}

// Format covers every fmt verb, so %v, %+v, %#v, %s and %q never print the value.
func (Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redacted)
}

// LogValue keeps the value out of structured logs.
func (Secret) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

// MarshalText keeps the value out of JSON and other text encodings.
func (Secret) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}
