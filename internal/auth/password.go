package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters, from RFC 9106 §4's second recommended option: the one for
// when 2 GiB of memory per hash is not available, which is every web server.
//
// Argon2id, not bcrypt: bcrypt caps at 72 bytes of input and is cheap to attack
// on a GPU, because it is not memory-hard.
//
// Each verification allocates 64 MiB. That is the point — it is what makes an
// offline attack expensive — but it also means login must be rate-limited, or an
// attacker can exhaust memory with concurrent attempts against any address.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// Password rules. Length beats composition: NIST SP 800-63B recommends a long
// minimum and no mandatory character classes, which only teach users to write
// "Password1!" on a sticky note.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 256 // Argon2 is happy to hash a megabyte; our CPU is not.
)

// Sentinel errors.
var (
	ErrPasswordTooShort = fmt.Errorf("auth: password must be at least %d characters", MinPasswordLength)
	ErrPasswordTooLong  = fmt.Errorf("auth: password must be at most %d characters", MaxPasswordLength)
	ErrInvalidHash      = errors.New("auth: password hash is malformed")
	ErrIncompatibleHash = errors.New("auth: password hash uses an unsupported algorithm")
)

// HashPassword returns an encoded Argon2id hash in the standard PHC string
// format, which carries the parameters and salt alongside the digest so that
// today's hashes stay verifiable after the parameters are raised.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}

	digest := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

// VerifyPassword reports whether password matches encodedHash.
//
// The comparison is constant time. A byte-by-byte comparison leaks, through
// timing, how many leading bytes of the digest were guessed correctly.
func VerifyPassword(password, encodedHash string) (bool, error) {
	params, salt, want, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// ValidatePassword enforces length bounds. It deliberately imposes no character
// classes.
func ValidatePassword(password string) error {
	switch {
	case len(password) < MinPasswordLength:
		return ErrPasswordTooShort
	case len(password) > MaxPasswordLength:
		return ErrPasswordTooLong
	}
	return nil
}

// dummyHash is a real Argon2id hash of a value nobody knows.
//
// Login verifies against it when the address is unknown, so a request for a
// missing account costs the same as one for a real account. Without it, response
// time answers "does this person have an account here?" for anyone who asks —
// and on a school's tenant, that is a roster.
var dummyHash = mustHash("this password exists only to burn the same CPU as a real one")

func mustHash(s string) string {
	h, err := HashPassword(s)
	if err != nil {
		panic("auth: cannot hash the dummy password: " + err.Error())
	}
	return h
}

// BurnPasswordComparison performs the same work as a verification against a real
// hash, and discards the result. Call it on the unknown-user path.
func BurnPasswordComparison(password string) {
	_, _ = VerifyPassword(password, dummyHash)
	runtime.KeepAlive(password)
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

// decodeHash parses the PHC string format:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<digest>
func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrIncompatibleHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return argonParams{}, nil, nil, ErrIncompatibleHash
	}

	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	digest, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if len(salt) == 0 || len(digest) == 0 {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	return p, salt, digest, nil
}
