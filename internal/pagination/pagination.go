// Package pagination makes and checks the cursor of a list and reads its limit. A cursor holds the sort keys of the last row of a
// page, signed with a key of the service, so a client cannot build one and the service never trusts the keys of a forged value.
package pagination

import (
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

const (
	// DefaultLimit is the page size when a request names none.
	DefaultLimit = 50
	// MaxLimit is the largest page size.
	MaxLimit = 100
	// MaxCursorLength is the length that the contract allows for a cursor.
	MaxCursorLength = 512

	macLength = 16
	macLabel  = "cursor v1"
)

var (
	// ErrLimit means that the limit is not a whole number from 1 to MaxLimit.
	ErrLimit = errors.New("the limit is not valid")
	// ErrCursor means that the cursor is not one that the service made for this list.
	ErrCursor = errors.New("the cursor is not valid")
)

// Signer derives bytes that only the service can compute. The Box of the secretbox package is one.
type Signer interface {
	Derive(length int, parts ...[]byte) []byte
}

// Codec makes and checks the cursors of one service.
type Codec struct {
	signer Signer
}

func NewCodec(signer Signer) *Codec {
	return &Codec{signer: signer}
}

// ParseLimit reads the limit parameter. An empty text means DefaultLimit.
func ParseLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultLimit, nil
	}

	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > MaxLimit {
		return 0, ErrLimit
	}

	return limit, nil
}

func (c *Codec) mac(kind string, payload []byte) []byte {
	return c.signer.Derive(macLength, []byte(macLabel), []byte(kind), payload)
}

// Encode returns the cursor that follows a row with the sort keys. The kind names the list, so a cursor of one list fails on another.
func (c *Codec) Encode(kind string, keys ...string) string {
	payload, _ := json.Marshal(keys)

	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(c.mac(kind, payload))
}

// Decode checks the cursor and returns its sort keys. An empty cursor is the first page and gives nil. A cursor that is not signed
// for this kind, is damaged or holds another number of keys gives ErrCursor.
func (c *Codec) Decode(kind, cursor string, wantKeys int) ([]string, error) {
	if cursor == "" {
		return nil, nil
	}

	if len(cursor) > MaxCursorLength {
		return nil, ErrCursor
	}

	body, tag, ok := strings.Cut(cursor, ".")
	if !ok {
		return nil, ErrCursor
	}

	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrCursor
	}

	sent, err := base64.RawURLEncoding.DecodeString(tag)
	if err != nil || !hmac.Equal(sent, c.mac(kind, payload)) {
		return nil, ErrCursor
	}

	var keys []string
	if err := json.Unmarshal(payload, &keys); err != nil || len(keys) != wantKeys {
		return nil, ErrCursor
	}

	return keys, nil
}

// Page is the answer of every list. NextCursor is absent on the last page.
type Page[T any] struct {
	Items      []T    `json:"items"`
	Total      int    `json:"total"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// Trim cuts the rows of a query that asked for limit plus one. It returns the rows of the page and whether a next page exists.
func Trim[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}

	return rows, false
}
