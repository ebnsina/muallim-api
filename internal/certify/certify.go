// Package certify issues and verifies certificates.
//
// A certificate is a record of an event: this person finished this course on this
// day. It is written when the course is completed, in that transaction, through an
// interface `enroll` declares — the same seam the gradebook is reached through.
//
// Nothing here renders a PDF. The certificate is text and a code; what draws it is
// a client's problem, and a server that owned a page layout would own a font
// licence and a headless browser with it.
package certify

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The sentinels. `internal/httpapi` maps them; nothing else does.
var (
	ErrNotFound        = errors.New("certify: not found")
	ErrTemplateExists  = errors.New("certify: a template with that name already exists")
	ErrInvalidTemplate = errors.New("certify: invalid template")
	ErrRevoked         = errors.New("certify: revoked")
)

/*
The alphabet a serial is spelled in.

Crockford's base32: no I, L, O or U. The first three are read back as 1, 1 and 0
by whoever is typing the code off a printed page, and the fourth spells things
nobody wants on a certificate.

32 characters, so each one carries five bits.
*/
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// How many characters a serial has, after the prefix. Sixteen of them, five bits
// each, is eighty bits: a code nobody guesses, and nobody increments to find the
// next graduate.
const serialLength = 16

// SerialPrefix names what the code is, for whoever finds one written down.
const SerialPrefix = "CERT"

/*
NewSerial returns a certificate's public name.

`crypto/rand`, not `math/rand`. The serial is the whole of the credential: anybody
holding one may read the certificate it names, and a sequence somebody can predict
is a list of every learner in the workspace and what they finished.

Grouped in fours, because it is read aloud and typed in by hand.
*/
func NewSerial() string {
	raw := make([]byte, serialLength)
	// rand.Read from crypto/rand never returns an error; it panics if the system
	// source is unavailable, which is the correct response to having no entropy.
	_, _ = rand.Read(raw)

	var out strings.Builder
	out.WriteString(SerialPrefix)

	for i, b := range raw {
		if i%4 == 0 {
			out.WriteByte('-')
		}
		// `% 32` is unbiased only because 256 divides by 32 exactly. Every byte maps
		// to two characters, each equally likely. Widen the alphabet and this becomes
		// modulo bias, quietly.
		out.WriteByte(alphabet[int(b)%len(alphabet)])
	}

	return out.String()
}

// Template is what a certificate says before a name is put on it.
type Template struct {
	ID   uuid.UUID
	Name string

	Title     string
	Body      string
	Signatory string

	// Builtin marks the template nobody created and nobody may edit.
	Builtin bool
}

/*
DefaultTemplate is what a workspace prints until it says otherwise.

Named rather than nil. A course whose template is unset still has to print
something, and a nil template would put a branch for "no template yet" — the state
most workspaces are in for ever — on every issuing path.
*/
func DefaultTemplate() Template {
	return Template{
		Name:    "Default",
		Builtin: true,
		Title:   "Certificate of Completion",
		Body: "This is to certify that {{learner}} has successfully completed " +
			"the course {{course}} on {{date}}.\n\nCertificate number {{serial}}.",
	}
}

func (t Template) validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("%w: a template needs a name", ErrInvalidTemplate)
	}
	if strings.TrimSpace(t.Title) == "" {
		return fmt.Errorf("%w: a template needs a title", ErrInvalidTemplate)
	}
	if strings.TrimSpace(t.Body) == "" {
		return fmt.Errorf("%w: a template needs something to say", ErrInvalidTemplate)
	}
	return nil
}

// Fields are the things a template may name.
type Fields struct {
	Learner string
	Course  string
	Date    string
	Serial  string
}

/*
Render substitutes the placeholders a template names.

An unknown placeholder is left exactly as it was written. The alternative — a
silent empty string — is a certificate that reads "has completed the course  on 4
March" and is framed by somebody who did not look closely. A visible `{{corse}}`
is a bug that reports itself.

One pass, over the known names. A replacement that itself contains `{{course}}` —
a learner who has renamed themselves that — is not substituted again, because the
pass is over the template and not over its output.
*/
func Render(body string, f Fields) string {
	if body == "" {
		return ""
	}

	var out strings.Builder
	out.Grow(len(body))

	for i := 0; i < len(body); {
		if body[i] != '{' || i+1 >= len(body) || body[i+1] != '{' {
			out.WriteByte(body[i])
			i++
			continue
		}

		end := strings.Index(body[i:], "}}")
		if end < 0 {
			// An unclosed `{{` is the rest of the document, verbatim.
			out.WriteString(body[i:])
			break
		}
		end += i

		name := strings.TrimSpace(body[i+2 : end])
		value, known := f.lookup(name)
		if known {
			out.WriteString(value)
		} else {
			// Left as written, so a typo is visible rather than invisible.
			out.WriteString(body[i : end+2])
		}
		i = end + 2
	}

	return out.String()
}

func (f Fields) lookup(name string) (string, bool) {
	switch strings.ToLower(name) {
	case "learner", "name":
		return f.Learner, true
	case "course":
		return f.Course, true
	case "date":
		return f.Date, true
	case "serial":
		return f.Serial, true
	default:
		return "", false
	}
}

// Certificate is a certificate that was issued.
type Certificate struct {
	ID       uuid.UUID
	CourseID uuid.UUID
	UserID   uuid.UUID

	Serial string

	// Copied at issue, never joined. A person who changes their name has not
	// invalidated what they earned under the old one.
	LearnerName string
	CourseTitle string
	IssuedAt    time.Time

	TemplateID *uuid.UUID
	Title      string
	Body       string
	Signatory  string

	RevokedAt     *time.Time
	RevokedReason string
}

// Revoked answers the only question a verifier has.
func (c Certificate) Revoked() bool { return c.RevokedAt != nil }

// DateFormat is how a certificate prints a day. Long, unambiguous, and not
// 03/04/2026 — which is two different days depending on who is reading it.
const DateFormat = "2 January 2006"
