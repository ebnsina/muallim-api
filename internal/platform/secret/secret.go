/*
Package secret seals a value the database is not allowed to know.

A workspace's own gateway credentials — an SSLCommerz store password, a bKash app
secret — are the one thing in this system that is neither ours nor derivable. They
sit in a table, so they sit in every backup, every replica, and every pg_dump that
somebody once emailed themselves. Encrypting them there moves the secret out of the
database and into the environment, which is where a key belongs.

AES-256-GCM: authenticated, so a ciphertext somebody edited will not open, and a
row swapped between tenants will not open either — the account id is bound in as
additional data.
*/
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrNoKey means the deployment holds no key, so nothing can be sealed or opened.
// It is not a reason to store a secret in the clear.
var ErrNoKey = errors.New("secret: no encryption key is configured")

// Sealer encrypts and decrypts with one key.
type Sealer struct {
	aead cipher.AEAD
}

/*
NewSealer takes the key as 64 hex characters — 32 bytes, AES-256.

Hex and not base64 because a key that travels through a shell, a Kubernetes secret,
and a Compose file should have no characters any of them will quietly mangle.
*/
func NewSealer(hexKey string) (*Sealer, error) {
	if hexKey == "" {
		return nil, ErrNoKey
	}

	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("secret: the key is not hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret: the key is %d bytes, not 32", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

/*
Seal encrypts, and prefixes the nonce.

The nonce is not a secret — it is a number that must never repeat under one key, and
crypto/rand is how you get one that will not. Storing it beside the ciphertext is
the ordinary practice; storing it *inside* the ciphertext is a nonce you cannot read
back, which is a ciphertext you cannot open.
*/
func (s *Sealer) Seal(plaintext string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secret: nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts. A ciphertext that was truncated, edited, or sealed under another
// key does not open — GCM refuses it rather than returning plausible rubbish.
func (s *Sealer) Open(ciphertext []byte) (string, error) {
	size := s.aead.NonceSize()
	if len(ciphertext) < size {
		return "", errors.New("secret: the ciphertext is too short to hold a nonce")
	}

	plaintext, err := s.aead.Open(nil, ciphertext[:size], ciphertext[size:], nil)
	if err != nil {
		return "", fmt.Errorf("secret: open: %w", err)
	}
	return string(plaintext), nil
}
