package secret_test

import (
	"strings"
	"testing"

	"github.com/ebnsina/muallim-api/internal/platform/secret"
)

const key = "6f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"

// The context a secret is bound to. In the app it is tenant id + gateway; here it
// only has to be some bytes, and a different some bytes to prove binding works.
var aad = []byte("tenant-42|sslcommerz")

func newSealer(t *testing.T) *secret.Sealer {
	t.Helper()

	s, err := secret.NewSealer(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	return s
}

func TestASealedSecretComesBack(t *testing.T) {
	s := newSealer(t)

	sealed, err := s.Seal("store-password", aad)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if strings.Contains(string(sealed), "store-password") {
		t.Fatal("the plaintext is still readable in the ciphertext")
	}

	opened, err := s.Open(sealed, aad)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened != "store-password" {
		t.Fatalf("opened %q, want %q", opened, "store-password")
	}
}

// The same secret twice must not produce the same bytes, or the table leaks which
// two workspaces share a password.
func TestTheSameSecretSealsDifferentlyEveryTime(t *testing.T) {
	s := newSealer(t)

	first, err := s.Seal("same", aad)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	second, err := s.Seal("same", aad)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if string(first) == string(second) {
		t.Fatal("two seals of one secret are identical — the nonce is not random")
	}
}

func TestAnEditedCiphertextDoesNotOpen(t *testing.T) {
	s := newSealer(t)

	sealed, err := s.Seal("store-password", aad)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	sealed[len(sealed)-1] ^= 0xff
	if _, err := s.Open(sealed, aad); err == nil {
		t.Fatal("a tampered ciphertext opened")
	}
}

func TestAnotherKeyDoesNotOpenIt(t *testing.T) {
	s := newSealer(t)

	sealed, err := s.Seal("store-password", aad)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	other, err := secret.NewSealer(strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	if _, err := other.Open(sealed, aad); err == nil {
		t.Fatal("a ciphertext opened under the wrong key")
	}
}

func TestAKeyThatIsNotAKeyIsRefused(t *testing.T) {
	for name, k := range map[string]string{
		"empty":     "",
		"not hex":   strings.Repeat("z", 64),
		"too short": strings.Repeat("ab", 8),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := secret.NewSealer(k); err == nil {
				t.Fatal("the key was accepted")
			}
		})
	}
}

// The point of the aad: a ciphertext sealed for one context does not open under
// another, even with the right key. It is what stops a row being moved between
// workspaces and decrypted in the wrong one.
func TestACiphertextDoesNotOpenUnderAnotherContext(t *testing.T) {
	s := newSealer(t)

	sealed, err := s.Seal("store-password", []byte("tenant-A|bkash"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := s.Open(sealed, []byte("tenant-B|bkash")); err == nil {
		t.Fatal("a ciphertext opened for a workspace it was not sealed for")
	}
	if _, err := s.Open(sealed, []byte("tenant-A|stripe")); err == nil {
		t.Fatal("a ciphertext opened for a gateway it was not sealed for")
	}
}
