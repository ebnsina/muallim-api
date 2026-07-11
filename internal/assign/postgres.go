package assign

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository implements Repository against Postgres.
//
// Every method takes the caller's transaction. None takes a pool: a query outside
// db.WithTenant runs without app.tenant_id bound, and the RLS policy that reads it
// then matches nothing — silently, by returning no rows.
type PostgresRepository struct{}

// NewPostgresRepository returns a repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23503"
}

const assignmentColumns = `id, lesson_id, title, instructions, points, passing_points,
	                       max_files, max_bytes, due_at, allow_late, created_at, updated_at`

func scanAssignment(row pgx.Row) (Assignment, error) {
	var a Assignment
	err := row.Scan(&a.ID, &a.LessonID, &a.Title, &a.Instructions, &a.Points, &a.PassingPoints,
		&a.MaxFiles, &a.MaxBytes, &a.DueAt, &a.AllowLate, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// CreateAssignment attaches an assignment to a lesson.
func (r *PostgresRepository) CreateAssignment(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, n NewAssignment) (Assignment, error) {
	assignment, err := scanAssignment(tx.QueryRow(ctx,
		`INSERT INTO assignments (tenant_id, lesson_id, title, instructions, points,
		                          passing_points, max_files, max_bytes, due_at, allow_late)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING `+assignmentColumns,
		tenantID, lessonID, n.Title, n.Instructions, n.Points, n.PassingPoints,
		n.MaxFiles, n.MaxBytes, n.DueAt, n.AllowLate))

	if err != nil {
		// The unique index on (tenant_id, lesson_id) is what makes "one assignment
		// per lesson" true under concurrency, rather than a check somebody raced.
		if isUniqueViolation(err) {
			return Assignment{}, ErrAssignmentExists
		}
		if isForeignKeyViolation(err) {
			return Assignment{}, ErrNotFound
		}
		return Assignment{}, fmt.Errorf("assign: create assignment: %w", err)
	}
	return assignment, nil
}

// AssignmentByLesson loads a lesson's assignment.
func (r *PostgresRepository) AssignmentByLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (Assignment, error) {
	assignment, err := scanAssignment(tx.QueryRow(ctx,
		`SELECT `+assignmentColumns+` FROM assignments WHERE tenant_id = $1 AND lesson_id = $2`,
		tenantID, lessonID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Assignment{}, ErrNotFound
		}
		return Assignment{}, fmt.Errorf("assign: load assignment: %w", err)
	}
	return assignment, nil
}

// AssignmentByID loads an assignment the caller already has the id of.
func (r *PostgresRepository) AssignmentByID(ctx context.Context, tx pgx.Tx, tenantID, assignmentID uuid.UUID) (Assignment, error) {
	assignment, err := scanAssignment(tx.QueryRow(ctx,
		`SELECT `+assignmentColumns+` FROM assignments WHERE tenant_id = $1 AND id = $2`,
		tenantID, assignmentID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Assignment{}, ErrNotFound
		}
		return Assignment{}, fmt.Errorf("assign: load assignment: %w", err)
	}
	return assignment, nil
}

// CourseSlugForLesson walks a lesson to its course, for a notification's link.
func (r *PostgresRepository) CourseSlugForLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (string, error) {
	var slug string
	err := tx.QueryRow(ctx,
		`SELECT c.slug
		 FROM lessons l
		 JOIN topics t  ON t.id = l.topic_id
		 JOIN courses c ON c.id = t.course_id
		 WHERE l.tenant_id = $1 AND l.id = $2`,
		tenantID, lessonID).Scan(&slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("assign: course slug for lesson: %w", err)
	}
	return slug, nil
}

// UpdateAssignment writes an assignment whole.
//
// It takes the merged result, not the patch. A COALESCE here would be a second
// implementation of `merge`, and it cannot express the one thing merge exists
// for: `coalesce(NULL, due_at)` keeps the deadline, so a field can be set and
// never erased. The validated result is the only thing worth writing.
func (r *PostgresRepository) UpdateAssignment(ctx context.Context, tx pgx.Tx, tenantID, assignmentID uuid.UUID, a NewAssignment) (Assignment, error) {
	assignment, err := scanAssignment(tx.QueryRow(ctx,
		`UPDATE assignments SET
		     title          = $3,
		     instructions   = $4,
		     points         = $5,
		     passing_points = $6,
		     max_files      = $7,
		     max_bytes      = $8,
		     due_at         = $9,
		     allow_late     = $10,
		     updated_at     = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+assignmentColumns,
		tenantID, assignmentID, a.Title, a.Instructions, a.Points, a.PassingPoints,
		a.MaxFiles, a.MaxBytes, a.DueAt, a.AllowLate))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Assignment{}, ErrNotFound
		}
		return Assignment{}, fmt.Errorf("assign: update assignment: %w", err)
	}
	return assignment, nil
}

// DeleteAssignment removes it, and returns the object keys it left behind.
//
// The keys are collected before the delete, in the same statement, because after
// the cascade there is nothing left to ask. Whoever gets them owes the object
// store a deletion.
func (r *PostgresRepository) DeleteAssignment(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (uuid.UUID, []string, error) {
	var assignmentID uuid.UUID
	var keys []string

	err := tx.QueryRow(ctx,
		`WITH doomed AS (
		     SELECT id FROM assignments WHERE tenant_id = $1 AND lesson_id = $2
		 ),
		 orphaned AS (
		     SELECT f.object_key
		     FROM submission_files f
		     JOIN assignment_submissions s ON s.id = f.submission_id AND s.tenant_id = f.tenant_id
		     WHERE f.tenant_id = $1 AND s.assignment_id IN (SELECT id FROM doomed)
		 ),
		 gone AS (
		     DELETE FROM assignments WHERE tenant_id = $1 AND lesson_id = $2 RETURNING id
		 )
		 SELECT (SELECT id FROM gone), coalesce(array_agg(object_key), '{}') FROM orphaned`,
		tenantID, lessonID).Scan(&assignmentID, &keys)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil, ErrNotFound
		}
		return uuid.Nil, nil, fmt.Errorf("assign: delete assignment: %w", err)
	}
	if assignmentID == uuid.Nil {
		return uuid.Nil, nil, ErrNotFound
	}
	return assignmentID, keys, nil
}

const submissionColumns = `id, assignment_id, user_id, status, submitted_at, graded_at,
	                       points, feedback, graded_by, late`

func scanSubmission(row pgx.Row) (Submission, error) {
	var s Submission
	err := row.Scan(&s.ID, &s.AssignmentID, &s.UserID, &s.Status, &s.SubmittedAt, &s.GradedAt,
		&s.Points, &s.Feedback, &s.GradedBy, &s.Late)
	return s, err
}

// OpenDraft returns the learner's submission, creating an empty draft if they
// have none.
//
// An upsert, because two tabs both pressing "attach" is a race, and the unique
// index on (tenant, assignment, learner) is where it is decided. `DO UPDATE` with
// a no-op assignment rather than `DO NOTHING`, because DO NOTHING returns no row
// on conflict and we want the one that is there.
func (r *PostgresRepository) OpenDraft(ctx context.Context, tx pgx.Tx, tenantID, assignmentID, userID uuid.UUID) (Submission, error) {
	submission, err := scanSubmission(tx.QueryRow(ctx,
		`INSERT INTO assignment_submissions (tenant_id, assignment_id, user_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, assignment_id, user_id)
		 DO UPDATE SET updated_at = assignment_submissions.updated_at
		 RETURNING `+submissionColumns,
		tenantID, assignmentID, userID))

	if err != nil {
		if isForeignKeyViolation(err) {
			return Submission{}, ErrNotFound
		}
		return Submission{}, fmt.Errorf("assign: open draft: %w", err)
	}
	return submission, nil
}

// SubmissionFor loads one learner's submission.
func (r *PostgresRepository) SubmissionFor(ctx context.Context, tx pgx.Tx, tenantID, assignmentID, userID uuid.UUID) (Submission, error) {
	submission, err := scanSubmission(tx.QueryRow(ctx,
		`SELECT `+submissionColumns+`
		 FROM assignment_submissions
		 WHERE tenant_id = $1 AND assignment_id = $2 AND user_id = $3`,
		tenantID, assignmentID, userID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Submission{}, ErrNotFound
		}
		return Submission{}, fmt.Errorf("assign: load submission: %w", err)
	}
	return submission, nil
}

// SubmissionByID loads a submission the marker has the id of.
func (r *PostgresRepository) SubmissionByID(ctx context.Context, tx pgx.Tx, tenantID, submissionID uuid.UUID) (Submission, error) {
	submission, err := scanSubmission(tx.QueryRow(ctx,
		`SELECT `+submissionColumns+` FROM assignment_submissions WHERE tenant_id = $1 AND id = $2`,
		tenantID, submissionID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Submission{}, ErrNotFound
		}
		return Submission{}, fmt.Errorf("assign: load submission: %w", err)
	}
	return submission, nil
}

// HandIn moves a draft to submitted.
//
// The transition is the WHERE clause. A double-clicked button matches no row the
// second time, and is told the work is already in.
func (r *PostgresRepository) HandIn(ctx context.Context, tx pgx.Tx, tenantID, submissionID uuid.UUID, late bool) (Submission, error) {
	submission, err := scanSubmission(tx.QueryRow(ctx,
		`UPDATE assignment_submissions
		 SET status = 'submitted', submitted_at = now(), late = $3, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = 'draft'
		 RETURNING `+submissionColumns,
		tenantID, submissionID, late))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Submission{}, ErrAlreadySubmitted
		}
		return Submission{}, fmt.Errorf("assign: hand in: %w", err)
	}
	return submission, nil
}

// Mark records a grade.
//
// `status <> 'draft'` and not `= 'submitted'`: a marker correcting a grade they
// just gave is a thing that happens, and refusing it would mean the only fix is a
// database console.
func (r *PostgresRepository) Mark(ctx context.Context, tx pgx.Tx, tenantID, submissionID, markerID uuid.UUID, points int, feedback string) (Submission, error) {
	submission, err := scanSubmission(tx.QueryRow(ctx,
		`UPDATE assignment_submissions
		 SET status = 'graded', points = $4, feedback = $5, graded_by = $3,
		     graded_at = now(), updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status <> 'draft'
		 RETURNING `+submissionColumns,
		tenantID, submissionID, markerID, points, feedback))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Submission{}, ErrNotSubmitted
		}
		return Submission{}, fmt.Errorf("assign: mark: %w", err)
	}
	return submission, nil
}

const fileColumns = `id, submission_id, object_key, filename, content_type, bytes, uploaded_at`

func scanFile(row pgx.Row) (File, error) {
	var f File
	err := row.Scan(&f.ID, &f.SubmissionID, &f.ObjectKey, &f.Filename, &f.ContentType, &f.Bytes, &f.UploadedAt)
	return f, err
}

// Files lists a submission's files, oldest first.
func (r *PostgresRepository) Files(ctx context.Context, tx pgx.Tx, tenantID, submissionID uuid.UUID) ([]File, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+fileColumns+`
		 FROM submission_files
		 WHERE tenant_id = $1 AND submission_id = $2
		 ORDER BY uploaded_at, id`,
		tenantID, submissionID)
	if err != nil {
		return nil, fmt.Errorf("assign: list files: %w", err)
	}
	defer rows.Close()

	var files []File
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("assign: scan file: %w", err)
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// AttachFile records an object that is already in the store.
func (r *PostgresRepository) AttachFile(ctx context.Context, tx pgx.Tx, tenantID, submissionID uuid.UUID, f File) (File, error) {
	file, err := scanFile(tx.QueryRow(ctx,
		`INSERT INTO submission_files (tenant_id, submission_id, object_key, filename, content_type, bytes)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+fileColumns,
		tenantID, submissionID, f.ObjectKey, f.Filename, f.ContentType, f.Bytes))

	if err != nil {
		// The object key is unique across the installation. Confirming the same
		// upload twice attaches it once.
		if isUniqueViolation(err) {
			return File{}, ErrUploadMissing
		}
		return File{}, fmt.Errorf("assign: attach file: %w", err)
	}
	return file, nil
}

// FileByID loads a file, and the submission it belongs to.
func (r *PostgresRepository) FileByID(ctx context.Context, tx pgx.Tx, tenantID, fileID uuid.UUID) (File, uuid.UUID, error) {
	file, err := scanFile(tx.QueryRow(ctx,
		`SELECT `+fileColumns+` FROM submission_files WHERE tenant_id = $1 AND id = $2`,
		tenantID, fileID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return File{}, uuid.Nil, ErrNotFound
		}
		return File{}, uuid.Nil, fmt.Errorf("assign: load file: %w", err)
	}
	return file, file.SubmissionID, nil
}

// DeleteFile removes the row and returns the key whose object is now orphaned.
func (r *PostgresRepository) DeleteFile(ctx context.Context, tx pgx.Tx, tenantID, fileID uuid.UUID) (string, error) {
	var key string
	err := tx.QueryRow(ctx,
		`DELETE FROM submission_files WHERE tenant_id = $1 AND id = $2 RETURNING object_key`,
		tenantID, fileID).Scan(&key)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("assign: delete file: %w", err)
	}
	return key, nil
}

// ListSubmissions returns what has been handed in, for the person marking it.
//
// One query. Drafts are excluded whatever the caller asks: a marker has no
// business reading a half-attached draft, and the partial index that serves the
// awaiting case covers exactly the rows it wants.
func (r *PostgresRepository) ListSubmissions(ctx context.Context, tx pgx.Tx, tenantID, assignmentID uuid.UUID, onlyAwaiting bool, limit int) ([]Marking, error) {
	rows, err := tx.Query(ctx,
		`SELECT s.id, s.assignment_id, s.user_id, s.status, s.submitted_at, s.graded_at,
		        s.points, s.feedback, s.graded_by, s.late, u.name, u.email
		 FROM assignment_submissions s
		 JOIN users u ON u.id = s.user_id
		 WHERE s.tenant_id = $1 AND s.assignment_id = $2
		   AND s.status <> 'draft'
		   AND ($3 = false OR s.status = 'submitted')
		 ORDER BY s.submitted_at, s.id
		 LIMIT $4`,
		tenantID, assignmentID, onlyAwaiting, limit)
	if err != nil {
		return nil, fmt.Errorf("assign: list submissions: %w", err)
	}
	defer rows.Close()

	var markings []Marking
	for rows.Next() {
		var m Marking
		var s Submission
		if err := rows.Scan(&s.ID, &s.AssignmentID, &s.UserID, &s.Status, &s.SubmittedAt, &s.GradedAt,
			&s.Points, &s.Feedback, &s.GradedBy, &s.Late, &m.LearnerName, &m.LearnerEmail); err != nil {
			return nil, fmt.Errorf("assign: scan submission: %w", err)
		}
		m.Submission = s
		markings = append(markings, m)
	}
	return markings, rows.Err()
}
