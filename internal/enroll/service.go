package enroll

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is the persistence contract, declared here by its consumer.
type Repository interface {
	CourseBySlug(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (uuid.UUID, string, error)

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
}

// AuditRecorder appends to the audit trail inside the caller's transaction.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// Service holds the enrolment rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder

	now func() time.Time
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder, now: time.Now}
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

// Cancel ends a learner's access without destroying their progress. Re-enrolling
// restores both.
func (s *Service) Cancel(ctx context.Context, tenantID uuid.UUID, slug string, actor Actor) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, _, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug)
		if err != nil {
			return err
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

		access = decide(view, reader, s.now())
		if !access.Granted() {
			// Not 403. A stranger asking for a lesson they may not read learns
			// nothing about whether it exists.
			return ErrNotFound
		}

		lesson = view.Lesson
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
//   - A live enrolment reads everything in the course.
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
		view, err := s.repo.LessonForReader(ctx, tx, tenantID, lessonID, actor.UserID)
		if err != nil {
			return err
		}

		// Completing a lesson requires an enrolment, always. A preview is readable
		// without one, but reading a sample is not studying a course, and progress
		// on a course nobody enrolled in is a row nothing can ever use.
		reader := Reader{UserID: actor.UserID}
		if decide(view, reader, s.now()) != AccessEnrolled {
			return ErrNotEnrolled
		}

		courseID := view.Lesson.CourseID
		if err := s.repo.MarkLesson(ctx, tx, tenantID, actor.UserID, lessonID, courseID, complete); err != nil {
			return err
		}

		progress, err = s.repo.RecomputeProgress(ctx, tx, tenantID, actor.UserID, courseID)
		if err != nil {
			return err
		}

		// The enrolment's status is a roll-up of the same rows, so it is recomputed
		// here too — in both directions. Reopening a lesson un-finishes the course,
		// and an enrolment left saying "completed" while its progress says 80% is a
		// row that answers differently depending on which field you read. It is also
		// what a prerequisite reads.
		if !progress.Complete() {
			reopened, err := s.repo.ReopenEnrolment(ctx, tx, tenantID, courseID, actor.UserID)
			if err != nil {
				return err
			}
			if !reopened {
				return nil
			}

			return s.audit.Record(ctx, tx, tenantID, AuditEntry{
				ActorID: &actor.UserID, Action: ActionCourseReopened,
				TargetType: "course", TargetID: courseID.String(),
				IP: actor.IP, UserAgent: actor.UserAgent,
			})
		}

		finished, err := s.repo.CompleteEnrolment(ctx, tx, tenantID, courseID, actor.UserID)
		if err != nil {
			return err
		}
		if !finished {
			// Already completed. Re-completing the last lesson is not a new event.
			return nil
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor.UserID, Action: ActionCourseFinished,
			TargetType: "course", TargetID: courseID.String(),
			IP: actor.IP, UserAgent: actor.UserAgent,
			Metadata: map[string]any{"lessons": progress.LessonsTotal},
		})
	})
	if err != nil {
		return Progress{}, err
	}
	return progress, nil
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
