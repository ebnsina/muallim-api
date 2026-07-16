package leads

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// fakeRepo records what the service decided to write.
type fakeRepo struct {
	got    DemoRequest
	called int
}

func (f *fakeRepo) Create(_ context.Context, _ pgx.Tx, d DemoRequest) (DemoRequest, error) {
	f.called++
	f.got = d
	return d, nil
}

// validate is the whole of the domain's judgement, and it runs before the
// database is opened — so these cases need no Postgres.
func TestValidateRefusesWhatTheFormMustNotSend(t *testing.T) {
	t.Parallel()

	ok := NewDemoRequest{
		Intent: IntentMadrasa,
		Name:   "Fatima Rahman",
		Email:  "fatima@example.com",
		Phone:  "01712345678",
		Agreed: true,
	}

	tests := map[string]struct {
		mutate func(n *NewDemoRequest)
		want   error
	}{
		"unticked terms are a refusal, not a field error": {
			func(n *NewDemoRequest) { n.Agreed = false }, ErrNotAgreed,
		},
		"an intent the catalogue does not name": {
			func(n *NewDemoRequest) { n.Intent = "wizard" }, ErrInvalidRequest,
		},
		"an empty intent": {
			func(n *NewDemoRequest) { n.Intent = "" }, ErrInvalidRequest,
		},
		"a blank name": {
			func(n *NewDemoRequest) { n.Name = "   " }, ErrInvalidRequest,
		},
		"a name past the column": {
			func(n *NewDemoRequest) { n.Name = strings.Repeat("a", MaxName+1) }, ErrInvalidRequest,
		},
		"something that cannot be an address": {
			func(n *NewDemoRequest) { n.Email = "fatima.example.com" }, ErrInvalidRequest,
		},
		"a blank phone": {
			func(n *NewDemoRequest) { n.Phone = "" }, ErrInvalidRequest,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := ok
			tc.mutate(&n)
			if err := n.validate(); !errors.Is(err, tc.want) {
				t.Fatalf("validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// The terms are the one field where "nearly" is not good enough: an unticked box
// must not leave a phone number in the table.
func TestUntickedTermsNeverReachTheRepository(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	svc := NewService(nil, repo, func() time.Time { return time.Unix(0, 0) })

	_, err := svc.Request(context.Background(), NewDemoRequest{
		Intent: IntentSchool,
		Name:   "Fatima Rahman",
		Email:  "fatima@example.com",
		Phone:  "01712345678",
		Agreed: false,
	})
	if !errors.Is(err, ErrNotAgreed) {
		t.Fatalf("Request() = %v, want ErrNotAgreed", err)
	}
	// The nil *database.DB above is the assertion: reaching the repository would
	// have had to open a transaction on it first, and panicked.
	if repo.called != 0 {
		t.Fatalf("repository was called %d times for a request nobody agreed to", repo.called)
	}
}

func TestValidateNormalisesBeforeItStores(t *testing.T) {
	t.Parallel()

	n := NewDemoRequest{
		Intent: "  madrasa ",
		Name:   "  Fatima Rahman  ",
		Email:  "  Fatima@Example.COM ",
		Phone:  " 01712345678 ",
		Agreed: true,
	}
	if err := n.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}

	// An address that differs only in case is the same address, and a list a human
	// reads should not show it twice.
	if n.Email != "fatima@example.com" {
		t.Errorf("email = %q, want it trimmed and lowercased", n.Email)
	}
	if n.Name != "Fatima Rahman" || n.Phone != "01712345678" || n.Intent != IntentMadrasa {
		t.Errorf("got %+v, want every field trimmed", n)
	}
}

// The catalogue and the CHECK in 00071_demo_requests.sql have to agree: a value
// Go offers that Postgres refuses is a form that fails on submit.
func TestEveryIntentIsNamedOnce(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, i := range Intents {
		if seen[i] {
			t.Errorf("%q is in the catalogue twice", i)
		}
		seen[i] = true
	}
	if len(Intents) != 7 {
		t.Errorf("the catalogue has %d intents; the migration's CHECK names 7", len(Intents))
	}
}
