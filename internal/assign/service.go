package assign

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/blob"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared here by its consumer.
type Repository interface {
	CreateAssignment(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, n NewAssignment) (Assignment, error)
	AssignmentByLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (Assignment, error)
	AssignmentByID(ctx context.Context, tx pgx.Tx, tenantID, assignmentID uuid.UUID) (Assignment, error)

	// CourseSlugForLesson resolves the course a lesson belongs to, for a
	// notification's link.
	CourseSlugForLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (string, error)
	UpdateAssignment(ctx context.Context, tx pgx.Tx, tenantID, assignmentID uuid.UUID, a NewAssignment) (Assignment, error)
	DeleteAssignment(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (uuid.UUID, []string, error)

	OpenDraft(ctx context.Context, tx pgx.Tx, tenantID, assignmentID, userID uuid.UUID) (Submission, error)
	SubmissionFor(ctx context.Context, tx pgx.Tx, tenantID, assignmentID, userID uuid.UUID) (Submission, error)
	SubmissionByID(ctx context.Context, tx pgx.Tx, tenantID, submissionID uuid.UUID) (Submission, error)
	HandIn(ctx context.Context, tx pgx.Tx, tenantID, submissionID uuid.UUID, late bool) (Submission, error)
	Mark(ctx context.Context, tx pgx.Tx, tenantID, submissionID, markerID uuid.UUID, points int, feedback string) (Submission, error)

	Files(ctx context.Context, tx pgx.Tx, tenantID, submissionID uuid.UUID) ([]File, error)
	AttachFile(ctx context.Context, tx pgx.Tx, tenantID, submissionID uuid.UUID, f File) (File, error)
	FileByID(ctx context.Context, tx pgx.Tx, tenantID, fileID uuid.UUID) (File, uuid.UUID, error)
	DeleteFile(ctx context.Context, tx pgx.Tx, tenantID, fileID uuid.UUID) (string, error)

	ListSubmissions(ctx context.Context, tx pgx.Tx, tenantID, assignmentID uuid.UUID, onlyAwaiting bool, limit int) ([]Marking, error)

	Deadlines(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, from, until time.Time, limit int) ([]Deadline, error)
}

// AuditEntry is one line of the audit trail.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	IP         netip.Addr
	UserAgent  string
	Metadata   map[string]any
}

// AuditRecorder appends to the audit trail inside the caller's transaction.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// Completions marks a lesson complete when its assignment is passed.
//
// Declared here, by the package that needs it, and satisfied by the enrolment
// service — which has never heard of assignments. See assess.Completions, which
// is the same interface for the same reason; they are separate because a package
// declares the interface it needs, and neither of these two knows the other exists.
type Completions interface {
	TryCompleteLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID uuid.UUID) (bool, error)
}

/*
Grades records a mark in the gradebook.

Declared here, by the package that needs it, and implemented in cmd/ over the
grade service — a domain package may not import a sibling. It takes the caller's
transaction, so the mark and the grade commit together. A submission graded 80
whose gradebook says nothing is a learner and a teacher looking at two different
numbers.

Idempotent at the far end, because this is called again every time a submission is
re-marked.
*/
type Grades interface {
	RecordMark(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID, sourceID uuid.UUID,
		title string, points, maxPoints int) error

	// EnsureItem says the assignment exists, before anybody has handed anything in.
	EnsureItem(ctx context.Context, tx pgx.Tx, tenantID, lessonID, sourceID uuid.UUID,
		title string, maxPoints int) error
}

// Enqueuer queues the deletion of an object whose row has gone.
//
// It takes the caller's transaction, so the job and the delete commit together.
// A row deleted without its job leaves bytes in a bucket that nothing will ever
// name again; a job without the row deletes a file the learner can still see.
type Enqueuer interface {
	DeleteObjects(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, keys []string) error
}

// MaxSubmissionsListed bounds the marking queue.
const MaxSubmissionsListed = 50

// Marking is one submission as the person marking it sees it.
type Marking struct {
	Submission Submission

	LearnerName  string
	LearnerEmail string
}

// Service holds the assignment rules and owns transaction boundaries.
type Service struct {
	db          *database.DB
	repo        Repository
	store       blob.Store
	audit       AuditRecorder
	jobs        Enqueuer
	completions Completions
	grades      Grades
	notifier    Notifier

	now func() time.Time
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, store blob.Store, recorder AuditRecorder,
	jobs Enqueuer, completions Completions, grades Grades, notifier Notifier) *Service {
	return &Service{
		db: db, repo: repo, store: store, audit: recorder, jobs: jobs,
		completions: completions, grades: grades, notifier: notifier,
		now: time.Now,
	}
}

// ---------------------------------------------------------------- authoring

// NewAssignment describes an assignment to attach to a lesson.
type NewAssignment struct {
	Title        string
	Instructions string

	Points        int
	PassingPoints int

	MaxFiles int
	MaxBytes int64

	DueAt     *time.Time
	AllowLate bool
}

// AssignmentPatch updates one. A nil field is left alone.
type AssignmentPatch struct {
	Title         *string
	Instructions  *string
	Points        *int
	PassingPoints *int
	MaxFiles      *int
	MaxBytes      *int64
	AllowLate     *bool

	// A deadline has three states, and a pointer only carries two. `DueAt` sets one
	// and `ClearDueAt` removes one; both nil and false leaves whatever is there.
	//
	// Without this, an author who sets a deadline can never take it off again — the
	// omitted field and the erased field are the same nil.
	DueAt      *time.Time
	ClearDueAt bool
}

// Upload bounds. The schema enforces the same numbers; these produce a sentence.
const (
	MinFiles     = 1
	MaxFilesEver = 20
	MaxBytesEver = 1 << 30 // 1 GiB
)

func (n NewAssignment) validate() error {
	switch {
	case strings.TrimSpace(n.Title) == "":
		return fmt.Errorf("%w: an assignment needs a title", ErrInvalidAssignment)
	case n.Points < 0:
		return fmt.Errorf("%w: points cannot be negative", ErrInvalidAssignment)
	case n.PassingPoints < 0 || n.PassingPoints > n.Points:
		return fmt.Errorf("%w: the pass mark must be between zero and %d", ErrInvalidAssignment, n.Points)
	case n.MaxFiles < MinFiles || n.MaxFiles > MaxFilesEver:
		return fmt.Errorf("%w: between %d and %d files", ErrInvalidAssignment, MinFiles, MaxFilesEver)
	case n.MaxBytes < 1 || n.MaxBytes > MaxBytesEver:
		return fmt.Errorf("%w: a file may be up to %d bytes", ErrInvalidAssignment, MaxBytesEver)
	}
	return nil
}

// CreateAssignment attaches an assignment to a lesson.
func (s *Service) CreateAssignment(ctx context.Context, tenantID, lessonID uuid.UUID, n NewAssignment, author Author) (Assignment, error) {
	if err := n.validate(); err != nil {
		return Assignment{}, err
	}

	var created Assignment
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.repo.CreateAssignment(ctx, tx, tenantID, lessonID, n)
		if err != nil {
			return err
		}

		// The gradebook learns of the assignment now, not when the first learner is
		// marked. A course with one unmarked assignment must not tell its learners it
		// has nothing to grade.
		if err := s.grades.EnsureItem(ctx, tx, tenantID, lessonID, created.ID,
			created.Title, created.Points); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionAssignmentCreated,
			TargetType: "assignment", TargetID: created.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"lesson_id": lessonID.String()},
		})
	})
	if err != nil {
		return Assignment{}, err
	}
	return created, nil
}

// EditAssignment applies a patch.
func (s *Service) EditAssignment(ctx context.Context, tenantID, lessonID uuid.UUID, p AssignmentPatch, author Author) (Assignment, error) {
	var updated Assignment

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		existing, err := s.repo.AssignmentByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		// A patch is checked against the assignment it will produce, not against the
		// one it replaces: raising the pass mark above the points, in two requests, is
		// a thing a client would otherwise be allowed to do.
		merged := merge(existing, p)
		if err := merged.validate(); err != nil {
			return err
		}

		updated, err = s.repo.UpdateAssignment(ctx, tx, tenantID, existing.ID, merged)
		if err != nil {
			return err
		}

		// A renamed assignment, or one worth different points, is the same item said
		// differently. Marks already given keep the total they were given out of.
		if err := s.grades.EnsureItem(ctx, tx, tenantID, existing.LessonID, existing.ID,
			updated.Title, updated.Points); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionAssignmentUpdated,
			TargetType: "assignment", TargetID: existing.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
		})
	})
	if err != nil {
		return Assignment{}, err
	}
	return updated, nil
}

func merge(a Assignment, p AssignmentPatch) NewAssignment {
	merged := NewAssignment{
		Title: a.Title, Instructions: a.Instructions,
		Points: a.Points, PassingPoints: a.PassingPoints,
		MaxFiles: a.MaxFiles, MaxBytes: a.MaxBytes,
		DueAt: a.DueAt, AllowLate: a.AllowLate,
	}

	if p.Title != nil {
		merged.Title = *p.Title
	}
	if p.Instructions != nil {
		merged.Instructions = *p.Instructions
	}
	if p.Points != nil {
		merged.Points = *p.Points
	}
	if p.PassingPoints != nil {
		merged.PassingPoints = *p.PassingPoints
	}
	if p.MaxFiles != nil {
		merged.MaxFiles = *p.MaxFiles
	}
	if p.MaxBytes != nil {
		merged.MaxBytes = *p.MaxBytes
	}
	if p.AllowLate != nil {
		merged.AllowLate = *p.AllowLate
	}

	// Clearing wins over setting. They cannot both be asked for — the HTTP layer
	// builds these two out of one JSON field — and if they ever were, removing a
	// deadline is the safer of the two to have meant.
	switch {
	case p.ClearDueAt:
		merged.DueAt = nil
	case p.DueAt != nil:
		merged.DueAt = p.DueAt
	}

	return merged
}

// RemoveAssignment deletes it, and every submission and file under it.
//
// The objects are deleted by a job enqueued on this transaction. Deleting the
// bytes first would strand a learner's submission behind a dead link if the
// transaction then rolled back; deleting them here, one round trip to the object
// store per file, would hold the transaction open across the network.
func (s *Service) RemoveAssignment(ctx context.Context, tenantID, lessonID uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		assignmentID, keys, err := s.repo.DeleteAssignment(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		if len(keys) > 0 {
			if err := s.jobs.DeleteObjects(ctx, tx, tenantID, keys); err != nil {
				return err
			}
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionAssignmentDeleted,
			TargetType: "assignment", TargetID: assignmentID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"lesson_id": lessonID.String(), "files": len(keys)},
		})
	})
}

// Assignment returns the assignment on a lesson.
func (s *Service) Assignment(ctx context.Context, tenantID, lessonID uuid.UUID) (Assignment, error) {
	var assignment Assignment

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		assignment, err = s.repo.AssignmentByLesson(ctx, tx, tenantID, lessonID)
		return err
	})
	if err != nil {
		return Assignment{}, err
	}
	return assignment, nil
}

// ----------------------------------------------------------------- learners

// MySubmission returns the learner's own submission, with its files. A learner
// who has not started one has a zero Submission and no error.
func (s *Service) MySubmission(ctx context.Context, tenantID, lessonID, userID uuid.UUID) (Assignment, Submission, error) {
	var assignment Assignment
	var submission Submission

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if assignment, err = s.repo.AssignmentByLesson(ctx, tx, tenantID, lessonID); err != nil {
			return err
		}

		submission, err = s.repo.SubmissionFor(ctx, tx, tenantID, assignment.ID, userID)
		if errors.Is(err, ErrNotFound) {
			// Not an error. They have not started.
			submission = Submission{}
			return nil
		}
		if err != nil {
			return err
		}

		submission.Files, err = s.repo.Files(ctx, tx, tenantID, submission.ID)
		return err
	})
	if err != nil {
		return Assignment{}, Submission{}, err
	}
	return assignment, submission, nil
}

// PresignUpload signs a URL for exactly one file of exactly `size` bytes, and
// writes nothing.
//
// The row appears when the client comes back to confirm the upload, and only
// after the object store has been asked what is really there. Signing a URL is
// not evidence that anybody used it.
func (s *Service) PresignUpload(ctx context.Context, tenantID, lessonID, userID uuid.UUID, filename string, size int64) (blob.Upload, string, error) {
	var (
		upload blob.Upload
		key    string
	)

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		assignment, err := s.repo.AssignmentByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		if size < 1 || size > assignment.MaxBytes {
			return fmt.Errorf("%w: a file must be between 1 and %d bytes", ErrInvalidFile, assignment.MaxBytes)
		}
		if strings.TrimSpace(filename) == "" {
			return fmt.Errorf("%w: a file needs a name", ErrInvalidFile)
		}
		if err := s.stillOpen(assignment); err != nil {
			return err
		}

		// The draft is created here rather than at confirm time, so the file count
		// below is the count of a submission that exists.
		submission, err := s.repo.OpenDraft(ctx, tx, tenantID, assignment.ID, userID)
		if err != nil {
			return err
		}
		if submission.Status != StatusDraft {
			return ErrAlreadySubmitted
		}

		files, err := s.repo.Files(ctx, tx, tenantID, submission.ID)
		if err != nil {
			return err
		}
		if len(files) >= assignment.MaxFiles {
			return fmt.Errorf("%w: this assignment takes %d", ErrTooManyFiles, assignment.MaxFiles)
		}

		key = objectKey(tenantID, assignment.ID, userID)
		upload, err = s.store.PresignPut(ctx, key, size, UploadTTL)
		return err
	})
	if err != nil {
		return blob.Upload{}, "", err
	}
	return upload, key, nil
}

// AttachFile records a file the client says it uploaded, once the object store
// agrees.
//
// Three checks, and each one is doing work.
//
// The key must sit under this learner's prefix. That is recomputed from the
// authenticated caller, so a key belonging to somebody else's submission — or to
// another workspace — is refused before the store is asked anything.
//
// The object must exist. A client that signs a URL and never uses it has produced
// no file, and a row saying otherwise is a broken link.
//
// The object's real size must be within the assignment's limit. The signature
// already forced it, and this is the belt: it costs one HEAD and it is the only
// number in this flow that came from the store rather than from a client.
func (s *Service) AttachFile(ctx context.Context, tenantID, lessonID, userID uuid.UUID, key, filename string) (File, error) {
	var attached File

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		assignment, err := s.repo.AssignmentByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}
		if err := s.stillOpen(assignment); err != nil {
			return err
		}

		if !strings.HasPrefix(key, keyPrefix(tenantID, assignment.ID, userID)+"/") {
			return ErrNotYours
		}

		submission, err := s.repo.OpenDraft(ctx, tx, tenantID, assignment.ID, userID)
		if err != nil {
			return err
		}
		if submission.Status != StatusDraft {
			return ErrAlreadySubmitted
		}

		files, err := s.repo.Files(ctx, tx, tenantID, submission.ID)
		if err != nil {
			return err
		}
		if len(files) >= assignment.MaxFiles {
			return fmt.Errorf("%w: this assignment takes %d", ErrTooManyFiles, assignment.MaxFiles)
		}

		object, err := s.store.Head(ctx, key)
		if errors.Is(err, blob.ErrNotFound) {
			return ErrUploadMissing
		}
		if err != nil {
			return err
		}
		if object.Size < 1 || object.Size > assignment.MaxBytes {
			return fmt.Errorf("%w: the object is %d bytes", ErrInvalidFile, object.Size)
		}

		attached, err = s.repo.AttachFile(ctx, tx, tenantID, submission.ID, File{
			ObjectKey:   key,
			Filename:    cleanFilename(filename),
			ContentType: object.ContentType,
			Bytes:       object.Size,
		})
		return err
	})
	if err != nil {
		return File{}, err
	}
	return attached, nil
}

// RemoveFile detaches a file from a draft, and queues the object's deletion on
// the same transaction.
func (s *Service) RemoveFile(ctx context.Context, tenantID, fileID, userID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, submissionID, err := s.repo.FileByID(ctx, tx, tenantID, fileID)
		if err != nil {
			return err
		}

		submission, err := s.repo.SubmissionByID(ctx, tx, tenantID, submissionID)
		if err != nil {
			return err
		}

		// Ownership, and then state. A learner who is not the owner learns nothing
		// about whether the file exists.
		if submission.UserID != userID {
			return ErrNotFound
		}
		if submission.Status != StatusDraft {
			return ErrAlreadySubmitted
		}

		key, err := s.repo.DeleteFile(ctx, tx, tenantID, fileID)
		if err != nil {
			return err
		}

		return s.jobs.DeleteObjects(ctx, tx, tenantID, []string{key})
	})
}

// Submit hands the draft in.
//
// Lateness is recorded here, against the deadline as it stands now. Computing it
// on read would let an instructor who moves the date make somebody late — or
// unmake them — long after the fact.
func (s *Service) Submit(ctx context.Context, tenantID, lessonID, userID uuid.UUID, author Author) (Submission, error) {
	var submitted Submission

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		assignment, err := s.repo.AssignmentByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		submission, err := s.repo.SubmissionFor(ctx, tx, tenantID, assignment.ID, userID)
		if err != nil {
			return err
		}
		if submission.Status != StatusDraft {
			return ErrAlreadySubmitted
		}

		now := s.now()
		late := assignment.DueAt != nil && now.After(*assignment.DueAt)
		if late && !assignment.AllowLate {
			return ErrPastDue
		}

		files, err := s.repo.Files(ctx, tx, tenantID, submission.ID)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return ErrNothingToSubmit
		}

		submitted, err = s.repo.HandIn(ctx, tx, tenantID, submission.ID, late)
		if err != nil {
			return err
		}
		submitted.Files = files

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionSubmitted,
			TargetType: "submission", TargetID: submission.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"assignment_id": assignment.ID.String(), "files": len(files), "late": late},
		})
	})
	if err != nil {
		return Submission{}, err
	}
	return submitted, nil
}

// stillOpen refuses a learner whose window has closed.
//
// An assignment that takes late work is always open; one that does not closes on
// its deadline, and closes for uploading as well as for handing in. A learner who
// could still attach files to a draft they may never submit is a learner being
// wasted.
func (s *Service) stillOpen(assignment Assignment) error {
	if assignment.DueAt == nil || assignment.AllowLate {
		return nil
	}
	if s.now().After(*assignment.DueAt) {
		return ErrPastDue
	}
	return nil
}

// ------------------------------------------------------------------ marking

// Submissions lists what has been handed in, for the person marking it.
func (s *Service) Submissions(ctx context.Context, tenantID, lessonID uuid.UUID, onlyAwaiting bool, limit int) ([]Marking, error) {
	if limit <= 0 || limit > MaxSubmissionsListed {
		limit = MaxSubmissionsListed
	}

	var markings []Marking
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		assignment, err := s.repo.AssignmentByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}
		markings, err = s.repo.ListSubmissions(ctx, tx, tenantID, assignment.ID, onlyAwaiting, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return markings, nil
}

// Submission returns one, with its files, for whoever may mark it.
func (s *Service) Submission(ctx context.Context, tenantID, submissionID uuid.UUID) (Assignment, Submission, error) {
	var assignment Assignment
	var submission Submission

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if submission, err = s.repo.SubmissionByID(ctx, tx, tenantID, submissionID); err != nil {
			return err
		}
		if assignment, err = s.repo.AssignmentByID(ctx, tx, tenantID, submission.AssignmentID); err != nil {
			return err
		}
		submission.Files, err = s.repo.Files(ctx, tx, tenantID, submission.ID)
		return err
	})
	if err != nil {
		return Assignment{}, Submission{}, err
	}
	return assignment, submission, nil
}

// Mark records a grade, and completes the lesson if the grade clears the bar.
//
// The completion happens in the transaction that recorded the mark, for the same
// reason a passed quiz completes its lesson there: a grade that committed without
// the progress it implies is a learner whose gradebook and course page disagree.
func (s *Service) Mark(ctx context.Context, tenantID, submissionID uuid.UUID, points int, feedback string, marker Author) (Submission, error) {
	var marked Submission

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		submission, err := s.repo.SubmissionByID(ctx, tx, tenantID, submissionID)
		if err != nil {
			return err
		}
		if submission.Status == StatusDraft {
			return ErrNotSubmitted
		}

		assignment, err := s.repo.AssignmentByID(ctx, tx, tenantID, submission.AssignmentID)
		if err != nil {
			return err
		}
		if points < 0 || points > assignment.Points {
			return fmt.Errorf("%w: %d is not between 0 and %d", ErrInvalidGrade, points, assignment.Points)
		}

		marked, err = s.repo.Mark(ctx, tx, tenantID, submissionID, marker.UserID, points, feedback)
		if err != nil {
			return err
		}

		// The gradebook, in the transaction that awarded the mark. Recorded whatever
		// the grade: a zero is a grade, and an assignment somebody failed still counts
		// against the course total.
		if err := s.grades.RecordMark(ctx, tx, tenantID, assignment.LessonID, submission.UserID,
			assignment.ID, assignment.Title, points, assignment.Points); err != nil {
			return err
		}

		if marked.Passed(assignment) {
			if _, err := s.completions.TryCompleteLesson(ctx, tx, tenantID, assignment.LessonID, submission.UserID); err != nil {
				return err
			}
		}

		// Tell the learner their work was marked. The marker is never the learner.
		if s.notifier != nil && submission.UserID != uuid.Nil && submission.UserID != marker.UserID {
			slug, err := s.repo.CourseSlugForLesson(ctx, tx, tenantID, assignment.LessonID)
			if err != nil {
				return err
			}
			if err := s.notifier.Notify(ctx, tx, tenantID, Notification{
				UserID: submission.UserID,
				Kind:   KindGrade,
				Title:  "Your assignment was graded",
				Body:   fmt.Sprintf("You scored %d of %d on %q.", points, assignment.Points, assignment.Title),
				Link:   "/courses/" + slug + "/grades",
			}); err != nil {
				return err
			}
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &marker.UserID, Action: ActionMarked,
			TargetType: "submission", TargetID: submissionID.String(),
			IP: marker.IP, UserAgent: marker.UserAgent,
			Metadata: map[string]any{"points": points, "of": assignment.Points},
		})
	})
	if err != nil {
		return Submission{}, err
	}
	return marked, nil
}

// ---------------------------------------------------------------- downloads

// DownloadURL signs a short-lived URL for one file.
//
// Who may read it is decided here: the learner who uploaded it, or somebody who
// may mark it. The caller establishes the second — this package does not know what
// a permission is — and the first is this package's business, because it is the
// only one holding the submission.
func (s *Service) DownloadURL(ctx context.Context, tenantID, fileID, viewerID uuid.UUID, canMark bool) (string, error) {
	var url string

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		file, submissionID, err := s.repo.FileByID(ctx, tx, tenantID, fileID)
		if err != nil {
			return err
		}

		submission, err := s.repo.SubmissionByID(ctx, tx, tenantID, submissionID)
		if err != nil {
			return err
		}

		// A stranger learns nothing about whether the file exists.
		if !canMark && submission.UserID != viewerID {
			return ErrNotFound
		}

		url, err = s.store.PresignGet(ctx, file.ObjectKey, file.Filename, DownloadTTL)
		return err
	})
	if err != nil {
		return "", err
	}
	return url, nil
}

/*
Deadlines lists what a learner still owes, from a moment until a horizon.

`from` is deliberately the caller's, not `now`: a deadline missed yesterday is the
one a learner most needs to see, so the dashboard asks from a week back and the
overdue ones come with it. Nothing here decides how they are drawn — `Overdue`
answers that from the same clock the caller is already holding.
*/
func (s *Service) Deadlines(ctx context.Context, tenantID, userID uuid.UUID, from, until time.Time, limit int) ([]Deadline, error) {
	if limit <= 0 || limit > MaxDeadlines {
		limit = MaxDeadlines
	}

	var deadlines []Deadline
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		deadlines, err = s.repo.Deadlines(ctx, tx, tenantID, userID, from, until, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return deadlines, nil
}
