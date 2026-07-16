// Package leads models people asking to be shown the product before they have a
// workspace to ask from. It knows nothing about HTTP, and nothing about tenants:
// a demo request arrives before there is one, which is what makes it the second
// table in the codebase — after `tenants` itself — with no row-level security.
package leads

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to errors_test.go
// in the same commit as a new one.
var (
	ErrInvalidRequest = errors.New("leads: the demo request is not valid")
	ErrNotAgreed      = errors.New("leads: the terms were not agreed")
)

// Intent is who is asking. Closed, and the same set the CHECK in
// 00071_demo_requests.sql enumerates.
//
// Closed on purpose, and mirrored in the database rather than trusted from here:
// a free-text "who are you" column is a column nobody can count, and the whole
// value of asking is being able to tell a madrasa from an agency without reading
// two hundred rows by hand. The web form renders these from the generated
// OpenAPI types, so the set is written once and cannot drift.
const (
	IntentCreator   = "creator"
	IntentSchool    = "school"
	IntentMadrasa   = "madrasa"
	IntentCoaching  = "coaching"
	IntentAgency    = "agency"
	IntentNonprofit = "nonprofit"
	IntentOther     = "other"
)

// Intents is the catalogue, in the order a form should offer them.
var Intents = []string{
	IntentCreator,
	IntentSchool,
	IntentMadrasa,
	IntentCoaching,
	IntentAgency,
	IntentNonprofit,
	IntentOther,
}

// Bounds. The database agrees with each of these; this is the friendly half.
const (
	MaxName  = 200
	MaxEmail = 320
	MaxPhone = 40
)

// DemoRequest is one person asking to be shown the product.
type DemoRequest struct {
	ID        uuid.UUID
	Intent    string
	Name      string
	Email     string
	Phone     string
	AgreedAt  time.Time
	CreatedAt time.Time
}

// NewDemoRequest is an incoming request, before it is anything.
type NewDemoRequest struct {
	Intent string
	Name   string
	Email  string
	Phone  string
	// Agreed is the visitor ticking the box. False is a refusal, not a validation
	// error about a field: we do not hold someone's phone number because they
	// nearly agreed.
	Agreed bool
}

// validate normalises and checks. Messages here are read by a person filling in a
// form — never render err.Error() to them, but these are written as though.
func (n *NewDemoRequest) validate() error {
	n.Intent = strings.TrimSpace(n.Intent)
	n.Name = strings.TrimSpace(n.Name)
	n.Email = strings.ToLower(strings.TrimSpace(n.Email))
	n.Phone = strings.TrimSpace(n.Phone)

	if !n.Agreed {
		return ErrNotAgreed
	}
	if !slices.Contains(Intents, n.Intent) {
		return fmt.Errorf("%w: choose what you are asking about", ErrInvalidRequest)
	}
	if n.Name == "" || len(n.Name) > MaxName {
		return fmt.Errorf("%w: a name is needed, up to %d characters", ErrInvalidRequest, MaxName)
	}
	// The one thing worth checking about an address is that it could be one. A
	// stricter rule than this rejects real addresses, and we are not going to
	// deliver to it — a human is going to read it and reply.
	if !strings.Contains(n.Email, "@") || len(n.Email) > MaxEmail {
		return fmt.Errorf("%w: an email address is needed", ErrInvalidRequest)
	}
	if n.Phone == "" || len(n.Phone) > MaxPhone {
		return fmt.Errorf("%w: a phone number is needed", ErrInvalidRequest)
	}
	return nil
}
