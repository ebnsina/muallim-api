package enroll

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared here by its consumer.
type Repository interface {
	CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, string, error)

	CourseFacts(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, courseIDs []uuid.UUID) (map[uuid.UUID]CourseFacts, error)

	Enrol(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID, source string, expiresAt *time.Time) (Enrolment, bool, error)
	Enrolment(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (Enrolment, error)
	CancelEnrolment(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error
	ListEnrolments(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, limit int) ([]EnrolmentWithCourse, error)
	CompleteEnrolment(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (bool, error)
	ReopenEnrolment(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (bool, error)
	CountEnrolments(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (int, error)

	LessonForReader(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID uuid.UUID) (LessonView, error)

	// MissingPrerequisites is read here and written by catalog. Two packages, one
	// table, no import between them.
	MissingPrerequisites(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) ([]MissingCourse, error)
	MarkLesson(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID, courseID uuid.UUID, complete bool) error
	RecomputeProgress(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) (Progress, error)
	ProgressFor(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) (Progress, error)

	UpsertReview(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID, rating int, body string) (Review, error)
	DeleteReview(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error
	ReviewFor(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (Review, error)
	ListReviews(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, limit int) ([]Review, error)
	ReviewSummary(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (ReviewSummary, error)

	CourseStats(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (CourseAnalytics, error)
}

// AuditRecorder appends to the audit trail inside the caller's transaction.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// Service holds the enrolment rules and owns transaction boundaries.
/*
Certificates issues a certificate to a learner who has finished a course.

Declared here, by the package that needs it, and implemented in cmd/ over the
certify service — a domain package may not import a sibling. It takes the caller's
transaction, so the completed enrolment and the certificate commit together. A
certificate written afterwards by a job is a learner who finished a course and
cannot prove it until the queue catches up.

Idempotent at the far end. Re-completing the last lesson issues nothing new.
*/
type Certificates interface {
	IssueIfEarned(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error
}

// Rewards awards a learner points and badges for finishing a lesson or a course,
// in the transaction that recorded the completion. Declared here, satisfied in
// cmd over the gamify service; neither package imports the other. A nil Rewards
// awards nothing, which is what the spec-only build and unconcerned tests pass.
// The far end is idempotent, so a re-completion earns nothing.
type Rewards interface {
	LessonCompleted(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) error
	CourseCompleted(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) error
}

/*
Prices says whether a course costs money.

Declared here by the package that needs it, and satisfied in cmd/ over commerce —
a domain may not import a sibling, and enrolment has no business knowing what a
gateway is. It takes the caller's transaction, so the price is read in the same
transaction that would have written the enrolment: a price set between the check
and the write is a course given away for free.

A Service built without one lets every course be self-enrolled, which is exactly
what this product did before there was any such thing as a price.
*/
type Prices interface {
	Priced(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (bool, error)
}

type Service struct {
	db           *database.DB
	repo         Repository
	audit        AuditRecorder
	certificates Certificates
	rewards      Rewards
	prices       Prices

	now func() time.Time
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder, certificates Certificates, rewards Rewards) *Service {
	return &Service{db: db, repo: repo, audit: recorder, certificates: certificates, rewards: rewards, now: time.Now}
}

// WithPrices teaches the service that courses can cost money. A Service without it
// behaves exactly as this product did before commerce existed.
func (s *Service) WithPrices(p Prices) *Service {
	s.prices = p
	return s
}

// hasLiveEnrolment reports whether the learner is already in the course.
//
// A database error is reported as "no enrolment", which is the conservative
// answer: the caller's next step is a prerequisite check, and a check that runs
// when it need not have is harmless. A caller that skipped it on a failed read
// would let a learner past a gate because the database hiccuped.
func (s *Service) hasLiveEnrolment(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) bool {
	existing, err := s.repo.Enrolment(ctx, tx, tenantID, courseID, userID)
	return err == nil && existing.Live(s.now())
}

// coursePublished is catalog's published status. Restated rather than imported:
// a domain package may not depend on a sibling, and the string is part of the
// schema, not of catalog's API.
const coursePublished = "published"

// Enrol grants a learner access to a published course.
//
// Re-enrolling after cancelling reactivates the original enrolment rather than
// creating a second: progress hangs off (user, course), and a learner who comes
// back expects to find their place.
func (s *Service) Enrol(ctx context.Context, tenantID uuid.UUID, slug string, actor Actor, source string) (Enrolment, error) {
	if source == "" {
		source = SourceSelf
	}

	var enrolment Enrolment
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, status, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}

		// An unpublished course is not "closed for enrolment", it is invisible.
		// Saying anything else tells a stranger that a draft exists.
		if status != coursePublished {
			return ErrNotFound
		}

		// Prerequisites gate self-enrolment only. Grant is a deliberate override by
		// somebody holding user:manage: an administrator placing a learner on a
		// course has already decided the learner belongs there.
		//
		// Checked before the enrolment row is touched, so a refusal leaves nothing
		// behind — and after the published check, so an unfinished prerequisite never
		// reveals that a draft exists.
		//
		// A learner who already holds a live enrolment is not checked at all. They
		// are in the course; a prerequisite added afterwards must not turn their next
		// click into a 403 and their progress into something they cannot reach.
		// A course with a price is not self-enrolled: it is bought. Grant still
		// overrides — an administrator placing somebody on a course has decided they
		// belong there, and a comped seat is a thing schools give.
		if source == SourceSelf && s.prices != nil {
			priced, err := s.prices.Priced(ctx, tx, tenantID, courseID)
			if err != nil {
				return err
			}
			if priced && !s.hasLiveEnrolment(ctx, tx, tenantID, courseID, actor.UserID) {
				return ErrPaymentRequired
			}
		}

		if source == SourceSelf && !s.hasLiveEnrolment(ctx, tx, tenantID, courseID, actor.UserID) {
			missing, err := s.repo.MissingPrerequisites(ctx, tx, tenantID, courseID, actor.UserID)
			if err != nil {
				return err
			}
			if len(missing) > 0 {
				return &UnmetPrerequisites{Missing: missing}
			}
		}

		created := false
		enrolment, created, err = s.repo.Enrol(ctx, tx, tenantID, courseID, actor.UserID, source, nil)
		if err != nil {
			return err
		}

		// A learner re-clicking Enrol is not an event. Only a genuinely new
		// enrolment is audited, or the trail becomes noise nobody reads.
		if !created {
			return nil
		}

		// Establish a progress row now, so a dashboard read never has to decide what
		// "no row" means.
		if _, err := s.repo.RecomputeProgress(ctx, tx, tenantID, actor.UserID, courseID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionEnrolled,
			TargetType: "course", TargetID: courseID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{"slug": slug, "source": source},
		})
	})
	if err != nil {
		return Enrolment{}, err
	}
	return enrolment, nil
}

/*
Cancel ends a learner's access without destroying their progress. Re-enrolling
restores both.

A purchase is not cancellable this way, and that is the whole of ErrPurchased. The
button used to hand back the course and keep the money: the enrolment went, the
paid order stayed paid, and re-enrolling asked the learner to buy what they had
already bought. Money comes back through a refund, which the workspace issues and
which withdraws the enrolment itself — one path, not two that can disagree.
*/
func (s *Service) Cancel(ctx context.Context, tenantID uuid.UUID, slug string, actor Actor) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, _, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}

		enrolment, err := s.repo.Enrolment(ctx, tx, tenantID, courseID, actor.UserID)
		if err != nil {
			return err
		}
		if enrolment.Source == SourcePurchase {
			return ErrPurchased
		}

		if err := s.repo.CancelEnrolment(ctx, tx, tenantID, courseID, actor.UserID); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionEnrolmentEnded,
			TargetType: "course", TargetID: courseID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{"slug": slug},
		})
	})
}

// Enrolments returns a learner's dashboard: every enrolment, its course, and its
// progress, in one query.
func (s *Service) Enrolments(ctx context.Context, tenantID, userID uuid.UUID, limit int) ([]EnrolmentWithCourse, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}

	var out []EnrolmentWithCourse
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.ListEnrolments(ctx, tx, tenantID, userID, limit)
		return err
	})
	return out, err
}

// Lesson returns a lesson if the reader may read it, and reports why.
//
// One query decides everything. The access rule below is the whole product's
// paywall, so it is written once, in full, in one place — rather than assembled
// from three scattered checks that each look reasonable alone.
func (s *Service) Lesson(ctx context.Context, tenantID, lessonID uuid.UUID, reader Reader) (LessonContent, Access, error) {
	var (
		lesson LessonContent
		access Access
	)

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		view, err := s.repo.LessonForReader(ctx, tx, tenantID, lessonID, reader.UserID)
		if err != nil {
			return err
		}

		now := s.now()
		access = decide(view, reader, now)

		// Locked, not absent. The learner is enrolled and can already see this
		// lesson in the curriculum, so telling them it does not exist would be a lie
		// they can disprove by scrolling.
		if access == AccessLocked {
			_, at := unlockAt(view, now)
			return &LessonLocked{AvailableAt: at}
		}

		if !access.Granted() {
			// Not 403. A stranger asking for a lesson they may not read learns
			// nothing about whether it exists.
			return ErrNotFound
		}

		lesson = view.Lesson

		// The date the reader is shown is the one their course's mode actually uses:
		// this learner's own, in after_enrolment mode, computed rather than stored;
		// and none at all when the course does not drip by date. The raw column is
		// not it, and showing it would promise a date the server never enforces.
		lesson.AvailableAt = schedule(view)
		return nil
	})
	if err != nil {
		return LessonContent{}, AccessDenied, err
	}
	return lesson, access, nil
}

// decide is the access rule.
//
//   - An author may read anything, including their own unpublished draft.
//   - Nobody else may read anything in an unpublished course. Not even a lesson
//     flagged as a preview: the course itself has not been released.
//   - A live enrolment reads everything the course has released to them. A lesson
//     still dripping is AccessLocked: they belong here, and it is not open yet.
//   - Otherwise, a preview lesson is a free sample and anyone may read it, signed
//     in or not.
//
// The enrolment is checked *before* the preview flag, and the order is
// load-bearing. Reversed, an enrolled learner reading a preview lesson is
// reported as a mere previewer — and since completing a lesson requires
// AccessEnrolled, no course containing a preview lesson could ever reach 100%.
//
// It is a pure function of facts already loaded, so every branch is enumerable,
// and there is no way to accidentally skip a database check inside it.
func decide(view LessonView, reader Reader, now time.Time) Access {
	if reader.CanAuthor {
		return AccessAuthor
	}
	if view.CourseStatus != coursePublished {
		return AccessDenied
	}

	if !reader.Anonymous() && view.EnrolmentStatus != nil {
		enrolment := Enrolment{Status: *view.EnrolmentStatus, ExpiresAt: view.EnrolmentExpires}
		if enrolment.Live(now) {
			// Drip applies to enrolled learners and to nobody else. An author is
			// already past this, and a stranger reading a preview is reading a sample
			// the course chose to give away — locking that would mean the customer
			// waits for what the passer-by is handed.
			if locked, _ := unlockAt(view, now); locked {
				return AccessLocked
			}
			return AccessEnrolled
		}
	}

	if view.Lesson.IsPreview {
		return AccessPreview
	}
	return AccessDenied
}

// CompleteLesson marks a lesson done and rebuilds the learner's course progress
// in the same transaction, so the roll-up can never disagree with the rows it
// summarises.
//
// Finishing the last lesson completes the enrolment. That transition is audited;
// completing an individual lesson is not, or the audit log becomes a clickstream.
func (s *Service) CompleteLesson(ctx context.Context, tenantID, lessonID uuid.UUID, actor Actor, complete bool) (Progress, error) {
	var progress Progress

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		progress, err = s.completeLessonIn(ctx, tx, tenantID, lessonID, actor, complete)
		return err
	})
	if err != nil {
		return Progress{}, err
	}
	return progress, nil
}

// TryCompleteLesson marks a lesson complete inside a transaction the caller owns,
// and reports whether it did.
//
// It exists for one caller: the assessment package, which completes a lesson when
// its quiz is passed and must do so in the same transaction that recorded the
// grade. A grade that committed without the progress it implies, or progress
// without its grade, is a learner whose course page and gradebook disagree. Its
// signature is assess.Completions, which that package declares and this method
// happens to satisfy — no adapter, and no import in either direction.
//
// The three refusals below are answers, not failures. A learner may cancel their
// enrolment between submitting an attempt and the worker reaching the job, and a
// lesson may lock behind a sequential drip if they reopen an earlier one. Nothing
// about that will change on a retry, so a grading job that failed here would
// retry, dead-letter, and leave the attempt in `grading` for ever — because the
// whole transaction, grade included, rolls back with it. The honest outcome is a
// graded attempt and no progress.
//
// All three are decided in Go before any statement runs, so the caller's
// transaction is still healthy when this returns false. Any other error is a real
// one, and aborts the transaction as it should.
//
// The transaction must already be bound to tenantID.
func (s *Service) TryCompleteLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID uuid.UUID) (bool, error) {
	var locked *LessonLocked

	_, err := s.completeLessonIn(ctx, tx, tenantID, lessonID, Actor{UserID: userID}, true)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotEnrolled), errors.Is(err, ErrNotFound), errors.As(err, &locked):
		return false, nil
	default:
		return false, err
	}
}

// completeLessonIn is CompleteLesson inside a transaction the caller owns. The
// transaction must already be bound to tenantID.
func (s *Service) completeLessonIn(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, actor Actor, complete bool) (Progress, error) {
	view, err := s.repo.LessonForReader(ctx, tx, tenantID, lessonID, actor.UserID)
	if err != nil {
		return Progress{}, err
	}

	// Completing a lesson requires an enrolment, always. A preview is readable
	// without one, but reading a sample is not studying a course, and progress
	// on a course nobody enrolled in is a row nothing can ever use.
	reader := Reader{UserID: actor.UserID}
	now := s.now()
	switch decide(view, reader, now) {
	case AccessEnrolled:
		// The only case that may mark progress.
	case AccessLocked:
		// An unreleased lesson cannot be completed, and saying "you are not
		// enrolled" to somebody who is would send them to buy what they own.
		_, at := unlockAt(view, now)
		return Progress{}, &LessonLocked{AvailableAt: at}
	default:
		return Progress{}, ErrNotEnrolled
	}

	courseID := view.Lesson.CourseID
	if err := s.repo.MarkLesson(ctx, tx, tenantID, actor.UserID, lessonID, courseID, complete); err != nil {
		return Progress{}, err
	}

	// Points for finishing a lesson, once. Only on completion, not on reopening,
	// and idempotent at the far end, so re-finishing earns nothing.
	if complete && s.rewards != nil {
		if err := s.rewards.LessonCompleted(ctx, tx, tenantID, actor.UserID, lessonID); err != nil {
			return Progress{}, err
		}
	}

	progress, err := s.repo.RecomputeProgress(ctx, tx, tenantID, actor.UserID, courseID)
	if err != nil {
		return Progress{}, err
	}

	// The enrolment's status is a roll-up of the same rows, so it is recomputed
	// here too — in both directions. Reopening a lesson un-finishes the course,
	// and an enrolment left saying "completed" while its progress says 80% is a
	// row that answers differently depending on which field you read. It is also
	// what a prerequisite reads.
	if !progress.Complete() {
		reopened, err := s.repo.ReopenEnrolment(ctx, tx, tenantID, courseID, actor.UserID)
		if err != nil {
			return Progress{}, err
		}
		if !reopened {
			return progress, nil
		}

		return progress, s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionCourseReopened,
			TargetType: "course", TargetID: courseID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
		})
	}

	finished, err := s.repo.CompleteEnrolment(ctx, tx, tenantID, courseID, actor.UserID)
	if err != nil {
		return Progress{}, err
	}
	if !finished {
		// Already completed. Re-completing the last lesson is not a new event.
		return progress, nil
	}

	// The certificate, in the transaction that finished the course. A learner who
	// has completed a course and cannot yet prove it is a support ticket.
	if err := s.certificates.IssueIfEarned(ctx, tx, tenantID, courseID, actor.UserID); err != nil {
		return Progress{}, err
	}

	// Points and a badge for finishing the course, in the same transaction.
	if s.rewards != nil {
		if err := s.rewards.CourseCompleted(ctx, tx, tenantID, actor.UserID, courseID); err != nil {
			return Progress{}, err
		}
	}

	return progress, s.audit.Record(ctx, tx, tenantID, AuditEntry{
		ActorID: &actor.UserID, Action: ActionCourseFinished,
		TargetType: "course", TargetID: courseID.String(),
		IP: actor.IP, UserAgent: actor.UserAgent,
		Metadata: map[string]any{"lessons": progress.LessonsTotal},
	})
}

// Progress returns a learner's standing in one course.
func (s *Service) Progress(ctx context.Context, tenantID uuid.UUID, slug string, userID uuid.UUID) (Progress, error) {
	var p Progress

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, status, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		if status != coursePublished {
			return ErrNotFound
		}

		if _, err := s.repo.Enrolment(ctx, tx, tenantID, courseID, userID); err != nil {
			return err
		}

		p, err = s.repo.ProgressFor(ctx, tx, tenantID, userID, courseID)
		return err
	})
	if err != nil {
		return Progress{}, err
	}
	return p, nil
}

// EnrolmentCount reports how many learners a course has. For instructors.
func (s *Service) EnrolmentCount(ctx context.Context, tenantID uuid.UUID, slug string) (int, error) {
	var n int
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, _, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		n, err = s.repo.CountEnrolments(ctx, tx, tenantID, courseID)
		return err
	})
	return n, err
}

// Grant enrols somebody else, on an instructor's authority. Authorisation is the
// transport layer's job; this records the source so support can answer "why does
// this person have access".
func (s *Service) Grant(ctx context.Context, tenantID uuid.UUID, slug string, learnerID uuid.UUID, actor Actor) (Enrolment, error) {
	if learnerID == uuid.Nil {
		return Enrolment{}, fmt.Errorf("%w: no learner", ErrNotFound)
	}

	var enrolment Enrolment
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, status, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		// A grant may target an unpublished course: an instructor previewing with a
		// colleague is exactly why grants exist.
		_ = status

		created := false
		enrolment, created, err = s.repo.Enrol(ctx, tx, tenantID, courseID, learnerID, SourceGranted, nil)
		if err != nil {
			return err
		}
		if !created {
			return ErrAlreadyEnrolled
		}

		if _, err := s.repo.RecomputeProgress(ctx, tx, tenantID, learnerID, courseID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionEnrolled,
			TargetType: "course", TargetID: courseID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{"slug": slug, "source": SourceGranted, "learner_id": learnerID.String()},
		})
	})
	if err != nil {
		return Enrolment{}, err
	}
	return enrolment, nil
}

// Review records or updates a learner's verdict on a course they are enrolled
// in. An empty rating out of 1..5 is refused; the body is optional and trimmed.
func (s *Service) Review(ctx context.Context, tenantID uuid.UUID, slug string, actor Actor, rating int, body string) (Review, error) {
	if rating < MinRating || rating > MaxRating {
		return Review{}, ErrInvalidReview
	}
	body = strings.TrimSpace(body)
	if len(body) > MaxReviewBody {
		return Review{}, ErrInvalidReview
	}

	var review Review
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, _, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		// You review a course you belong to. A live enrolment (active or completed)
		// is the entitlement; a stranger rating a course they never took is noise.
		if !s.hasLiveEnrolment(ctx, tx, tenantID, courseID, actor.UserID) {
			return ErrNotEnrolled
		}
		review, err = s.repo.UpsertReview(ctx, tx, tenantID, courseID, actor.UserID, rating, body)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionReviewed,
			TargetType: "course", TargetID: courseID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{"slug": slug, "rating": rating},
		})
	})
	if err != nil {
		return Review{}, err
	}
	return review, nil
}

// UnReview retracts a learner's own review. Absent is not an error: the end
// state a caller asked for — no review — is already true.
func (s *Service) UnReview(ctx context.Context, tenantID uuid.UUID, slug string, actor Actor) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, _, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		return s.repo.DeleteReview(ctx, tx, tenantID, courseID, actor.UserID)
	})
}

// Reviews returns a course's review wall and its summary, plus the caller's own
// review if they have left one, so the page can prefill the edit form.
func (s *Service) Reviews(ctx context.Context, tenantID uuid.UUID, slug string, userID uuid.UUID, limit int) ([]Review, ReviewSummary, *Review, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}

	var (
		list    []Review
		summary ReviewSummary
		mine    *Review
	)
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, _, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		if list, err = s.repo.ListReviews(ctx, tx, tenantID, courseID, limit); err != nil {
			return err
		}
		if summary, err = s.repo.ReviewSummary(ctx, tx, tenantID, courseID); err != nil {
			return err
		}
		if userID != uuid.Nil {
			r, err := s.repo.ReviewFor(ctx, tx, tenantID, courseID, userID)
			if err != nil && !errors.Is(err, ErrReviewNotFound) {
				return err
			}
			if err == nil {
				mine = &r
			}
		}
		return nil
	})
	return list, summary, mine, err
}

// Analytics summarises a course for the instructor who owns it: its enrolments by
// status, mean progress, and rating — the enrolment stats and the review summary
// gathered in one read-only transaction. It resolves any status, so a draft's
// early numbers are visible to its author.
func (s *Service) Analytics(ctx context.Context, tenantID uuid.UUID, slug string) (CourseAnalytics, error) {
	var out CourseAnalytics
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, _, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
		}
		if out, err = s.repo.CourseStats(ctx, tx, tenantID, courseID); err != nil {
			return err
		}
		out.Reviews, err = s.repo.ReviewSummary(ctx, tx, tenantID, courseID)
		return err
	})
	return out, err
}

/*
CourseFacts is what a course looks like from the outside: how many people are on
it, and what they made of it.

Both belong to this package — an enrolment is one, a review is the other — and a
catalogue that wanted them was reaching for a count per row. They are fetched for
a whole page at once instead, keyed by course.
*/
type CourseFacts struct {
	// Learners counts the active and completed enrolments: the people studying it
	// and the people who finished, which is what "350,392 learners" means.
	Learners int

	// RatingAverage is the mean, 1–5, and is meaningless when RatingCount is zero.
	// A course with no reviews is unrated, not rated nought.
	RatingAverage float64
	RatingCount   int
}

// Facts loads them for a page of courses. One query, whatever the page holds.
func (s *Service) Facts(ctx context.Context, tenantID uuid.UUID, courseIDs []uuid.UUID) (map[uuid.UUID]CourseFacts, error) {
	if len(courseIDs) == 0 {
		return map[uuid.UUID]CourseFacts{}, nil
	}

	var facts map[uuid.UUID]CourseFacts
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		facts, err = s.repo.CourseFacts(ctx, tx, tenantID, courseIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return facts, nil
}

/*
GrantInTx enrols somebody inside a transaction the caller already owns.

For a caller that must enrol as part of something else: `commerce` marks an order
paid and enrols the buyer, and the two have to commit together or a learner is out
of pocket and locked out. Every other caller wants Enrol or Grant, which own their
own transaction and their own audit line.

The source is the caller's, because only they know why: a purchase is not a self
enrolment, and the difference is what a support conversation turns on.
*/
func (s *Service) GrantInTx(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID, source string) error {
	_, _, err := s.repo.Enrol(ctx, tx, tenantID, courseID, userID, source, nil)
	return err
}

// CancelInTx withdraws an enrolment inside the caller's transaction — a refund, and
// nothing else so far. Progress survives: cancelling is not erasing.
func (s *Service) CancelInTx(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return s.repo.CancelEnrolment(ctx, tx, tenantID, courseID, userID)
}
