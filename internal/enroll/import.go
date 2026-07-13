package enroll

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxImport bounds one import. A list longer than this is a file, and a file is a
// job — not a request somebody is standing in front of.
const MaxImport = 500

// ErrNothingToImport means an empty list, or one that was only blank lines.
var ErrNothingToImport = errors.New("enroll: there are no addresses to import")

// ErrTooManyToImport means more addresses than one request may carry.
var ErrTooManyToImport = errors.New("enroll: that is more addresses than one import may carry")

/*
Directory answers "who, in this workspace, holds these addresses".

Declared here by the package that needs it, and satisfied in cmd/ over auth — which
owns users and memberships, and which enroll may not import. It answers for *active
members only*: an address that belongs to nobody here, or to somebody suspended, is
not somebody a course can be given to, and saying so is the whole point of the
outcome this import reports.
*/
type Directory interface {
	MembersByEmail(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, emails []string) (map[string]uuid.UUID, error)
}

// WithDirectory supplies it. Without one, importing is refused rather than half-done.
func (s *Service) WithDirectory(d Directory) *Service {
	s.directory = d
	return s
}

// ImportOutcome is what happened to one address.
const (
	// OutcomeEnrolled: they were not on the course, and now they are.
	OutcomeEnrolled = "enrolled"

	// OutcomeAlready: they were already on it. Not an error, and not a change.
	OutcomeAlready = "already_enrolled"

	// OutcomeUnknown: nobody in this workspace holds that address. The commonest
	// result of a pasted list, and the one the person pasting it must see.
	OutcomeUnknown = "not_a_member"
)

// ImportResult is one line of the report an import hands back.
type ImportResult struct {
	Email   string
	Outcome string
	UserID  uuid.UUID
}

/*
Import enrols a cohort by email address, and reports what became of each.

Three queries for any number of addresses: who they are, whether the course exists,
and one INSERT for all of them. Never a query per row — a school moving four hundred
learners in from another system is exactly when that would matter, and exactly when
nobody is watching the logs.

It does not create accounts. An address nobody here holds is reported as such and
skipped: an import that quietly minted accounts would let anybody with course:write
manufacture members, and joining a workspace is by invitation for a reason.
*/
func (s *Service) Import(ctx context.Context, tenantID uuid.UUID, slug string, emails []string, actor Actor) ([]ImportResult, error) {
	if s.directory == nil {
		return nil, fmt.Errorf("%w: no directory is wired", ErrNotFound)
	}

	wanted := normaliseEmails(emails)
	switch {
	case len(wanted) == 0:
		return nil, ErrNothingToImport
	case len(wanted) > MaxImport:
		return nil, ErrTooManyToImport
	}

	var results []ImportResult

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, _, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}

		members, err := s.directory.MembersByEmail(ctx, tx, tenantID, wanted)
		if err != nil {
			return err
		}

		userIDs := make([]uuid.UUID, 0, len(members))
		for _, id := range members {
			userIDs = append(userIDs, id)
		}

		var added map[uuid.UUID]bool
		if len(userIDs) > 0 {
			if added, err = s.repo.BulkEnrol(ctx, tx, tenantID, courseID, userIDs, SourceImport); err != nil {
				return err
			}
		}

		results = make([]ImportResult, 0, len(wanted))
		enrolled := 0
		for _, email := range wanted {
			userID, isMember := members[email]
			switch {
			case !isMember:
				results = append(results, ImportResult{Email: email, Outcome: OutcomeUnknown})
			case added[userID]:
				enrolled++
				results = append(results, ImportResult{Email: email, Outcome: OutcomeEnrolled, UserID: userID})
			default:
				results = append(results, ImportResult{Email: email, Outcome: OutcomeAlready, UserID: userID})
			}
		}

		// One audit line for the import, not one per learner: the trail records that a
		// person moved a cohort onto a course, which is the thing that was done.
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionEnrolmentImported,
			TargetType: "course", TargetID: courseID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{
				"slug": slug, "addresses": len(wanted), "enrolled": enrolled,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// normaliseEmails lower-cases, trims, drops the blanks, and keeps the first of any
// duplicate — a pasted list has all four, and a caller who lists somebody twice
// should not be told twice.
func normaliseEmails(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))

	for _, email := range raw {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		out = append(out, email)
	}
	return out
}
