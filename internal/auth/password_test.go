package auth

import (
	"strings"
	"testing"
)

const validPassword = "correct horse battery staple"

func TestHashPasswordProducesVerifiableHash(t *testing.T) {
	t.Parallel()

	hash, err := HashPassword(validPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, err := VerifyPassword(validPassword, hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("the correct password did not verify against its own hash")
	}
}

// The salt must be random, or two users with the same password share a hash and
// one rainbow table breaks both.
func TestHashPasswordIsSaltedPerCall(t *testing.T) {
	t.Parallel()

	first, err := HashPassword(validPassword)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(validPassword)
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("the same password hashed twice produced identical output; the salt is not random")
	}

	// Both must still verify.
	for i, hash := range []string{first, second} {
		ok, err := VerifyPassword(validPassword, hash)
		if err != nil || !ok {
			t.Errorf("hash %d did not verify: ok=%v err=%v", i, ok, err)
		}
	}
}

func TestVerifyPasswordRejectsWrongPassword(t *testing.T) {
	t.Parallel()

	hash, err := HashPassword(validPassword)
	if err != nil {
		t.Fatal(err)
	}

	for _, wrong := range []string{
		"correct horse battery stapl",   // one char short
		"correct horse battery staple ", // trailing space
		"Correct horse battery staple",  // case
		"",
	} {
		ok, err := VerifyPassword(wrong, hash)
		if err != nil {
			t.Fatalf("VerifyPassword(%q): %v", wrong, err)
		}
		if ok {
			t.Errorf("password %q verified against a hash of %q", wrong, validPassword)
		}
	}
}

// The hash carries its own parameters, so raising the cost later does not
// invalidate hashes made today.
func TestHashIsPHCFormatted(t *testing.T) {
	t.Parallel()

	hash, err := HashPassword(validPassword)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("hash %q does not carry the expected algorithm, version, and parameters", hash)
	}
	if got := len(strings.Split(hash, "$")); got != 6 {
		t.Errorf("hash has %d $-separated fields, want 6", got)
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":           "",
		"not a hash":      "hunter2",
		"wrong algorithm": "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"bcrypt":          "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
		"unknown version": "$argon2id$v=16$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"missing fields":  "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA",
		"bad base64 salt": "$argon2id$v=19$m=65536,t=3,p=4$!!!!$aGFzaA",
		"bad params":      "$argon2id$v=19$m=abc,t=3,p=4$c2FsdA$aGFzaA",
		"empty salt":      "$argon2id$v=19$m=65536,t=3,p=4$$aGFzaA",
	}

	for name, hash := range tests {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyPassword(validPassword, hash)
			if err == nil {
				t.Errorf("a malformed hash returned no error (ok=%v); it must never be treated as a mismatch", ok)
			}
			if ok {
				t.Error("a malformed hash verified successfully")
			}
		})
	}
}

func TestValidatePasswordEnforcesLengthOnly(t *testing.T) {
	t.Parallel()

	t.Run("too short", func(t *testing.T) {
		if err := ValidatePassword(strings.Repeat("a", MinPasswordLength-1)); err == nil {
			t.Error("a short password was accepted")
		}
	})

	t.Run("too long", func(t *testing.T) {
		if err := ValidatePassword(strings.Repeat("a", MaxPasswordLength+1)); err == nil {
			t.Error("an unbounded password was accepted; Argon2 will happily hash a megabyte")
		}
	})

	// No composition rules, on purpose: NIST SP 800-63B advises against them.
	t.Run("long and simple is fine", func(t *testing.T) {
		if err := ValidatePassword(strings.Repeat("a", MinPasswordLength)); err != nil {
			t.Errorf("a long lowercase password was rejected: %v", err)
		}
	})
}

// The unknown-account path must do the same work as the known one, or response
// latency answers "does this address have an account here?"
func TestBurnPasswordComparisonDoesRealWork(t *testing.T) {
	t.Parallel()

	// If dummyHash were not a real Argon2id hash, this would return an error and
	// the timing defence would be silently absent.
	ok, err := VerifyPassword("anything", dummyHash)
	if err != nil {
		t.Fatalf("the dummy hash is not a valid Argon2id hash: %v", err)
	}
	if ok {
		t.Error("the dummy password was guessed, which should be impossible")
	}

	BurnPasswordComparison("anything") // must not panic
}
