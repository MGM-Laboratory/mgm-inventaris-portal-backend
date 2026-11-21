// Package secretbox encrypts a small secret for storage, and derives secrets from the same key. It exists for the TOTP secret of an
// admin, which the service must read back, so a hash would not do.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// KeyLength is the length of the master key in bytes.
const KeyLength = 32

// ErrOpen means that a sealed value could not be opened. A wrong key, a wrong context and a damaged value all give it.
var ErrOpen = errors.New("the sealed value cannot be opened")

// Box seals and opens values and derives secrets. The master key gives one key for sealing and one for deriving, so the two uses
// never share a key.
type Box struct {
	aead   cipher.AEAD
	macKey []byte
}

// New returns a box for a master key of KeyLength bytes.
func New(master []byte) (*Box, error) {
	if len(master) != KeyLength {
		return nil, fmt.Errorf("the key must have %d bytes", KeyLength)
	}

	block, err := aes.NewCipher(subkey(master, "mgm seal v1"))
	if err != nil {
		return nil, fmt.Errorf("make the cipher %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("make the mode %w", err)
	}

	return &Box{aead: aead, macKey: subkey(master, "mgm derive v1")}, nil
}

func subkey(master []byte, label string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte(label))

	return mac.Sum(nil)
}

// sealPrefix names the format of a sealed value, so a later format can live next to this one in the same column.
const sealPrefix = "v1."

// Seal encrypts plaintext with AES-256-GCM and returns text that starts with the format prefix and then holds Base64. The context is
// authenticated and not stored, so a value that was sealed for one row does not open for another.
func (b *Box) Seal(plaintext, context []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("make the nonce %w", err)
	}

	return sealPrefix + base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, plaintext, context)), nil
}

// Open decrypts the text that Seal returned for the same context.
func (b *Box) Open(sealed string, context []byte) ([]byte, error) {
	encoded, found := strings.CutPrefix(sealed, sealPrefix)
	if !found {
		return nil, ErrOpen
	}

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < b.aead.NonceSize()+b.aead.Overhead() {
		return nil, ErrOpen
	}

	plain, err := b.aead.Open(nil, raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():], context)
	if err != nil {
		return nil, ErrOpen
	}

	return plain, nil
}

// Derive returns length bytes that only a holder of the key can compute from the parts. The same parts give the same bytes. Each
// part is length prefixed, so two lists of parts that join to the same text still differ. A length above 32 is cut to 32.
func (b *Box) Derive(length int, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, b.macKey)

	for _, part := range parts {
		mac.Write([]byte(strconv.Itoa(len(part)) + ":"))
		mac.Write(part)
	}

	return mac.Sum(nil)[:min(length, sha256.Size)]
}
