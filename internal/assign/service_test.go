package assign_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/assign"
	"github.com/ebnsina/lms-api/internal/audit"
	"github.com/ebnsina/lms-api/internal/certify"
	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/grade"
	"github.com/ebnsina/lms-api/internal/platform/blob"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()

	url := os.Getenv("LMS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LMS_TEST_DATABASE_URL is not set; skipping database tests")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4}, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// The real object store, or nothing. A mock would answer Head() with whatever the
// test wanted, which is precisely the question AttachFile exists to ask somebody
// else.
func testStore(t *testing.T) *blob.S3 {
	t.Helper()

	endpoint := os.Getenv("LMS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("LMS_TEST_S3_ENDPOINT is not set; skipping object-store tests")
	}

	store, err := blob.NewS3(blob.Options{
		Endpoint:  endpoint,
		Bucket:    envOr("LMS_TEST_S3_BUCKET", "lms-uploads"),
		AccessKey: envOr("LMS_TEST_S3_ACCESS_KEY", "lms"),
		SecretKey: envOr("LMS_TEST_S3_SECRET_KEY", "lms-secret-key"),
		Region:    "auto",
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("object store: %v", err)
	}
	return store
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type assignAuditor struct{ recorder *audit.Recorder }

func (a assignAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e assign.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

// recordingEnqueuer stands in for River, and records what was queued on the
// transaction it was queued in.
type recordingEnqueuer struct {
	mu   sync.Mutex
	keys []string
}

func (e *recordingEnqueuer) DeleteObjects(_ context.Context, _ pgx.Tx, _ uuid.UUID, keys []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.keys = append(e.keys, keys...)
	return nil
}

func (e *recordingEnqueuer) queued() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.keys...)
}

type enrolAuditor struct{ recorder *audit.Recorder }

func (a enrolAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e enroll.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

type fixture struct {
	db       *database.DB
	svc      *assign.Service
	store    *blob.S3
	jobs     *recordingEnqueuer
	learning *enroll.Service
	grades   *grade.Service

	tenant  uuid.UUID
	lesson  uuid.UUID
	course  string
	learner uuid.UUID
	author  assign.Author
}

// assignmentGrades adapts the real gradebook to `assign.Grades`, as cmd/ does.
// The real one, because "did the mark reach the gradebook" is the question.
type assignmentGrades struct{ svc *grade.Service }

func (g assignmentGrades) RecordMark(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID, sourceID uuid.UUID,
	title string, points, maxPoints int,
) error {
	return g.svc.Record(ctx, tx, tenantID, grade.Score{
		LessonID: lessonID, UserID: userID,
		Source: grade.SourceAssignment, SourceID: sourceID,
		Title: title, Points: points, MaxPoints: maxPoints,
	})
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	db := testDB(t)
	store := testStore(t)
	jobs := &recordingEnqueuer{}

	tenantID := seedTenant(t, db)
	lessonID, slug := seedLesson(t, db, tenantID)

	// The real enrolment service, exactly as cmd/ wires it. A passed assignment
	// completes its lesson, and a stub would leave that untested.
	learning := enroll.NewService(db, enroll.NewPostgresRepository(), enrolAuditor{audit.NewRecorder()}, newIssuer(db))

	grades := grade.NewService(db, grade.NewPostgresRepository())

	return fixture{
		db: db, store: store, jobs: jobs, learning: learning, grades: grades,
		svc: assign.NewService(db, assign.NewPostgresRepository(), store,
			assignAuditor{audit.NewRecorder()}, jobs, learning, assignmentGrades{grades}),
		tenant: tenantID, lesson: lessonID, course: slug,
		learner: seedUser(t, db, tenantID),
		author:  assign.Author{UserID: seedUser(t, db, tenantID)},
	}
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, 'Test')`,
			id, "t"+id.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})
	return id
}

func seedUser(t *testing.T, db *database.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'U')`,
			id, "u-"+id.String()[:8]+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'student')`, tenantID, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedLesson(t *testing.T, db *database.DB, tenantID uuid.UUID) (uuid.UUID, string) {
	t.Helper()

	slug := "c-" + uuid.NewString()[:8]

	var lessonID uuid.UUID
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var courseID, topicID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status)
			 VALUES ($1, $2, 'Course', 'published') RETURNING id`,
			tenantID, slug).Scan(&courseID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO topics (tenant_id, course_id, title, position) VALUES ($1, $2, 'T', 0) RETURNING id`,
			tenantID, courseID).Scan(&topicID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO lessons (tenant_id, topic_id, title, content_type, position)
			 VALUES ($1, $2, 'Hand this in', 'assignment', 0) RETURNING id`,
			tenantID, topicID).Scan(&lessonID)
	})
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	return lessonID, slug
}

func (f fixture) assignment(t *testing.T, n assign.NewAssignment) assign.Assignment {
	t.Helper()

	if n.Title == "" {
		n.Title = "The method, in your own words"
	}
	if n.Points == 0 {
		n.Points = 100
	}
	if n.MaxFiles == 0 {
		n.MaxFiles = 2
	}
	if n.MaxBytes == 0 {
		n.MaxBytes = 1 << 20
	}

	created, err := f.svc.CreateAssignment(t.Context(), f.tenant, f.lesson, n, f.author)
	if err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	return created
}

// upload performs the PUT a browser would, against the signed URL.
func upload(t *testing.T, signed blob.Upload, body []byte) {
	t.Helper()

	request, err := http.NewRequest(signed.Method, signed.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build the upload: %v", err)
	}
	for name, values := range signed.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	if declared := request.Header.Get("Content-Length"); declared != "" {
		request.ContentLength, _ = strconv.ParseInt(declared, 10, 64)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("the store refused the upload: %d", response.StatusCode)
	}
}

// enrol puts the learner in the course, which is what makes a completed lesson a
// row worth writing.
func (f fixture) enrol(t *testing.T) {
	t.Helper()

	if _, err := f.learning.Enrol(t.Context(), f.tenant, f.course, enroll.Actor{UserID: f.learner}, enroll.SourceSelf); err != nil {
		t.Fatalf("enrol: %v", err)
	}
}

// The whole loop: sign, upload, confirm, hand in, mark.
func TestTheSubmissionLoop(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.enrol(t)
	a := f.assignment(t, assign.NewAssignment{Points: 10, PassingPoints: 6})

	body := []byte("The method is the thing that survives the problem.")

	signed, key, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, f.learner, "essay.pdf", int64(len(body)))
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	// Nothing is written until the upload is confirmed. A signed URL is not evidence
	// that anybody used it.
	if _, submission, err := f.svc.MySubmission(t.Context(), f.tenant, f.lesson, f.learner); err != nil {
		t.Fatalf("submission: %v", err)
	} else if len(submission.Files) != 0 {
		t.Fatalf("a file appeared before it was uploaded")
	}

	upload(t, signed, body)

	file, err := f.svc.AttachFile(t.Context(), f.tenant, f.lesson, f.learner, key, "essay.pdf")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if file.Bytes != int64(len(body)) {
		t.Errorf("size = %d, want %d — the number must come from the store", file.Bytes, len(body))
	}

	submitted, err := f.svc.Submit(t.Context(), f.tenant, f.lesson, f.learner, assign.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if submitted.Status != assign.StatusSubmitted || submitted.Late {
		t.Errorf("submitted = %q, late = %v", submitted.Status, submitted.Late)
	}

	// Before marking, the lesson is not complete.
	if progress := f.progress(t); progress.LessonsCompleted != 0 {
		t.Fatalf("an unmarked submission completed the lesson")
	}

	marked, err := f.svc.Mark(t.Context(), f.tenant, submitted.ID, 8, "Good, if a little short.", f.author)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if marked.Status != assign.StatusGraded || marked.Points == nil || *marked.Points != 8 {
		t.Errorf("marked = %+v", marked)
	}
	if !marked.Passed(a) {
		t.Error("8 of 10 did not clear a pass mark of 6")
	}

	// Passing completes the lesson, in the transaction that recorded the mark.
	if progress := f.progress(t); progress.LessonsCompleted != 1 {
		t.Errorf("passing the assignment did not complete the lesson: %+v", progress)
	}

	// And the learner can fetch their own file back.
	url, err := f.svc.DownloadURL(t.Context(), f.tenant, file.ID, f.learner, false)
	if err != nil {
		t.Fatalf("download url: %v", err)
	}

	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer response.Body.Close()

	got, _ := io.ReadAll(response.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("downloaded %q", got)
	}
}

func (f fixture) progress(t *testing.T) enroll.Progress {
	t.Helper()

	progress, err := f.learning.Progress(t.Context(), f.tenant, f.course, f.learner)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	return progress
}

/*
The prefix check is an authorisation check.

A learner who obtains another learner's key — from a log, from a screenshot, by
guessing a uuid — must not be able to attach it to their own submission and read
it back. The prefix is recomputed from the authenticated caller, so the key is
refused before the object store is asked anything about it.
*/
func TestAKeyBelongingToSomebodyElseIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{})

	stranger := seedUser(t, f.db, f.tenant)
	body := []byte("not yours")

	// The stranger signs and uploads honestly, under their own prefix.
	signed, theirKey, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, stranger, "theirs.txt", int64(len(body)))
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	upload(t, signed, body)

	// Our learner tries to claim it.
	if _, err := f.svc.AttachFile(t.Context(), f.tenant, f.lesson, f.learner, theirKey, "mine.txt"); !errors.Is(err, assign.ErrNotYours) {
		t.Fatalf("attaching another learner's key: %v, want ErrNotYours", err)
	}
}

// A key nobody uploaded to is a key that names no file. A row saying otherwise is
// a broken link, and there is no way back from one.
func TestConfirmingAnUploadThatNeverHappened(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{})

	_, key, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, f.learner, "ghost.pdf", 10)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	// Signed, never uploaded.
	if _, err := f.svc.AttachFile(t.Context(), f.tenant, f.lesson, f.learner, key, "ghost.pdf"); !errors.Is(err, assign.ErrUploadMissing) {
		t.Fatalf("attaching an unused key: %v, want ErrUploadMissing", err)
	}
}

// The size limit lives in the signature, and again in the confirm. Asking the
// store how big the object really is costs one HEAD, and it is the only number in
// this flow that did not come from a client.
func TestAFileLargerThanTheAssignmentAllows(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{MaxBytes: 16})

	if _, _, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, f.learner, "big.bin", 17); !errors.Is(err, assign.ErrInvalidFile) {
		t.Fatalf("signing 17 bytes against a 16-byte limit: %v, want ErrInvalidFile", err)
	}
	if _, _, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, f.learner, "empty.bin", 0); !errors.Is(err, assign.ErrInvalidFile) {
		t.Fatalf("signing an empty file: %v, want ErrInvalidFile", err)
	}
}

func TestTheFileCountIsCapped(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{MaxFiles: 1})

	f.attach(t, f.learner, "one.txt", []byte("first"))

	if _, _, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, f.learner, "two.txt", 6); !errors.Is(err, assign.ErrTooManyFiles) {
		t.Fatalf("a second file against a one-file limit: %v, want ErrTooManyFiles", err)
	}
}

// attach signs, uploads and confirms, which is what a browser does.
func (f fixture) attach(t *testing.T, learner uuid.UUID, name string, body []byte) assign.File {
	t.Helper()

	signed, key, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, learner, name, int64(len(body)))
	if err != nil {
		t.Fatalf("presign %s: %v", name, err)
	}
	upload(t, signed, body)

	file, err := f.svc.AttachFile(t.Context(), f.tenant, f.lesson, learner, key, name)
	if err != nil {
		t.Fatalf("attach %s: %v", name, err)
	}
	return file
}

// A submitted assignment is not a draft, and its files are not the learner's to
// change.
func TestASubmittedAssignmentIsFrozen(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{MaxFiles: 3})

	file := f.attach(t, f.learner, "essay.txt", []byte("done"))

	if _, err := f.svc.Submit(t.Context(), f.tenant, f.lesson, f.learner, assign.Author{UserID: f.learner}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if _, _, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, f.learner, "more.txt", 4); !errors.Is(err, assign.ErrAlreadySubmitted) {
		t.Errorf("attaching after handing in: %v", err)
	}
	if err := f.svc.RemoveFile(t.Context(), f.tenant, file.ID, f.learner); !errors.Is(err, assign.ErrAlreadySubmitted) {
		t.Errorf("removing a file after handing in: %v", err)
	}
	if _, err := f.svc.Submit(t.Context(), f.tenant, f.lesson, f.learner, assign.Author{UserID: f.learner}); !errors.Is(err, assign.ErrAlreadySubmitted) {
		t.Errorf("submitting twice: %v", err)
	}
}

func TestNothingToHandIn(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{})

	// No draft at all.
	if _, err := f.svc.Submit(t.Context(), f.tenant, f.lesson, f.learner, assign.Author{UserID: f.learner}); !errors.Is(err, assign.ErrNotFound) {
		t.Errorf("submitting with no draft: %v", err)
	}

	// A draft with no files. `PresignUpload` opens one and writes nothing else.
	if _, _, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, f.learner, "a.txt", 4); err != nil {
		t.Fatalf("presign: %v", err)
	}
	if _, err := f.svc.Submit(t.Context(), f.tenant, f.lesson, f.learner, assign.Author{UserID: f.learner}); !errors.Is(err, assign.ErrNothingToSubmit) {
		t.Errorf("submitting an empty draft: %v", err)
	}
}

// Removing a file deletes the row and queues the object, on the same transaction.
// A row without its job leaves bytes in a bucket nobody will ever name again.
func TestRemovingAFileQueuesItsObject(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{})

	file := f.attach(t, f.learner, "wrong.txt", []byte("oops"))

	if err := f.svc.RemoveFile(t.Context(), f.tenant, file.ID, f.learner); err != nil {
		t.Fatalf("remove: %v", err)
	}

	queued := f.jobs.queued()
	if len(queued) != 1 || queued[0] != file.ObjectKey {
		t.Fatalf("queued %v, want the object's key", queued)
	}

	// And the row is gone.
	if _, submission, err := f.svc.MySubmission(t.Context(), f.tenant, f.lesson, f.learner); err != nil {
		t.Fatalf("submission: %v", err)
	} else if len(submission.Files) != 0 {
		t.Errorf("the file survived its deletion")
	}
}

func TestOnlyItsOwnerMayRemoveAFile(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{})

	file := f.attach(t, f.learner, "mine.txt", []byte("mine"))
	stranger := seedUser(t, f.db, f.tenant)

	// 404, not 403. A stranger learns nothing about whether the file exists.
	if err := f.svc.RemoveFile(t.Context(), f.tenant, file.ID, stranger); !errors.Is(err, assign.ErrNotFound) {
		t.Errorf("a stranger removed a file: %v", err)
	}
	if _, err := f.svc.DownloadURL(t.Context(), f.tenant, file.ID, stranger, false); !errors.Is(err, assign.ErrNotFound) {
		t.Errorf("a stranger downloaded a file: %v", err)
	}

	// A marker may.
	if _, err := f.svc.DownloadURL(t.Context(), f.tenant, file.ID, stranger, true); err != nil {
		t.Errorf("a marker could not download it: %v", err)
	}
}

/*
Lateness is recorded when the work arrives, against the deadline as it stood.

Computing it on read would let an instructor who moves the date make somebody
late — or unmake them — weeks afterwards, and a grade that changes because
somebody edited a form is a grade nobody can appeal.
*/
func TestLatenessIsRecordedNotComputed(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	yesterday := time.Now().Add(-24 * time.Hour)
	f.assignment(t, assign.NewAssignment{DueAt: &yesterday, AllowLate: true})

	f.attach(t, f.learner, "late.txt", []byte("sorry"))

	submitted, err := f.svc.Submit(t.Context(), f.tenant, f.lesson, f.learner, assign.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !submitted.Late {
		t.Fatal("work handed in after the deadline is not marked late")
	}

	// The instructor moves the deadline into the future. The submission stays late.
	tomorrow := time.Now().Add(24 * time.Hour)
	if _, err := f.svc.EditAssignment(t.Context(), f.tenant, f.lesson,
		assign.AssignmentPatch{DueAt: &tomorrow}, f.author); err != nil {
		t.Fatalf("edit: %v", err)
	}

	_, after, err := f.svc.MySubmission(t.Context(), f.tenant, f.lesson, f.learner)
	if err != nil {
		t.Fatalf("submission: %v", err)
	}
	if !after.Late {
		t.Error("moving the deadline unmade somebody's lateness")
	}
}

// An assignment that does not take late work closes at its deadline — for
// uploading as well as for handing in. A learner attaching files to a draft they
// may never submit is a learner being wasted.
func TestAClosedAssignmentTakesNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	yesterday := time.Now().Add(-24 * time.Hour)
	f.assignment(t, assign.NewAssignment{DueAt: &yesterday, AllowLate: false})

	if _, _, err := f.svc.PresignUpload(t.Context(), f.tenant, f.lesson, f.learner, "late.txt", 4); !errors.Is(err, assign.ErrPastDue) {
		t.Errorf("signing an upload after the deadline: %v, want ErrPastDue", err)
	}
}

// A lesson has one assignment.
func TestALessonHasOneAssignment(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{})

	_, err := f.svc.CreateAssignment(t.Context(), f.tenant, f.lesson,
		assign.NewAssignment{Title: "Another", Points: 1, MaxFiles: 1, MaxBytes: 1}, f.author)
	if !errors.Is(err, assign.ErrAssignmentExists) {
		t.Fatalf("a second assignment on one lesson: %v", err)
	}
}

// Deleting the assignment queues every object under it. Bytes nobody will ever
// name again are bytes somebody pays for every month until the end of time.
func TestDeletingAnAssignmentQueuesItsObjects(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{MaxFiles: 2})

	first := f.attach(t, f.learner, "a.txt", []byte("aaaa"))
	second := f.attach(t, f.learner, "b.txt", []byte("bbbb"))

	if err := f.svc.RemoveAssignment(t.Context(), f.tenant, f.lesson, f.author); err != nil {
		t.Fatalf("remove assignment: %v", err)
	}

	queued := f.jobs.queued()
	if len(queued) != 2 {
		t.Fatalf("queued %d objects, want 2: %v", len(queued), queued)
	}

	want := map[string]bool{first.ObjectKey: true, second.ObjectKey: true}
	for _, key := range queued {
		if !want[key] {
			t.Errorf("queued a key that was not a file of this assignment: %s", key)
		}
	}

	if _, err := f.svc.Assignment(t.Context(), f.tenant, f.lesson); !errors.Is(err, assign.ErrNotFound) {
		t.Errorf("the assignment survived: %v", err)
	}
}

// The marking queue holds what has been handed in, never a draft.
func TestTheMarkingQueueExcludesDrafts(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{Points: 10})

	// One learner hands in; another is still attaching.
	f.attach(t, f.learner, "done.txt", []byte("done"))
	if _, err := f.svc.Submit(t.Context(), f.tenant, f.lesson, f.learner, assign.Author{UserID: f.learner}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	drafting := seedUser(t, f.db, f.tenant)
	f.attach(t, drafting, "wip.txt", []byte("wip"))

	waiting, err := f.svc.Submissions(t.Context(), f.tenant, f.lesson, true, 50)
	if err != nil {
		t.Fatalf("submissions: %v", err)
	}
	if len(waiting) != 1 {
		t.Fatalf("the queue holds %d submissions, want 1", len(waiting))
	}
	if waiting[0].Submission.UserID != f.learner || waiting[0].LearnerEmail == "" {
		t.Errorf("the queue holds the wrong submission: %+v", waiting[0])
	}

	// Marking empties the awaiting queue and leaves the history.
	if _, err := f.svc.Mark(t.Context(), f.tenant, waiting[0].Submission.ID, 10, "", f.author); err != nil {
		t.Fatalf("mark: %v", err)
	}

	if waiting, err = f.svc.Submissions(t.Context(), f.tenant, f.lesson, true, 50); err != nil {
		t.Fatalf("submissions: %v", err)
	}
	if len(waiting) != 0 {
		t.Errorf("a marked submission is still waiting")
	}

	all, err := f.svc.Submissions(t.Context(), f.tenant, f.lesson, false, 50)
	if err != nil {
		t.Fatalf("submissions: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("the history holds %d submissions, want 1 — a draft is not a submission", len(all))
	}
}

func TestAMarkMustFitTheAssignment(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{Points: 10})

	f.attach(t, f.learner, "essay.txt", []byte("work"))
	submitted, err := f.svc.Submit(t.Context(), f.tenant, f.lesson, f.learner, assign.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	for _, points := range []int{-1, 11} {
		if _, err := f.svc.Mark(t.Context(), f.tenant, submitted.ID, points, "", f.author); !errors.Is(err, assign.ErrInvalidGrade) {
			t.Errorf("awarding %d of 10: %v, want ErrInvalidGrade", points, err)
		}
	}
}

// There is nothing to mark until it is handed in.
func TestADraftCannotBeMarked(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{Points: 10})
	f.attach(t, f.learner, "wip.txt", []byte("wip"))

	_, submission, err := f.svc.MySubmission(t.Context(), f.tenant, f.lesson, f.learner)
	if err != nil {
		t.Fatalf("submission: %v", err)
	}

	if _, err := f.svc.Mark(t.Context(), f.tenant, submission.ID, 5, "", f.author); !errors.Is(err, assign.ErrNotSubmitted) {
		t.Fatalf("marking a draft: %v, want ErrNotSubmitted", err)
	}
}

/*
An edit reaches the database as the assignment it produces, not as the patch.

`merge` gets this right on its own — there is a table test for it — and for a
while the repository then rebuilt the same decision out of the raw patch with
COALESCE, which cannot write NULL. Every field could be set and the deadline
could never be erased. Two implementations of one rule, and only one of them was
tested.
*/
func TestEditingWritesEveryField(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	next := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	f.assignment(t, assign.NewAssignment{DueAt: &next, AllowLate: true, Points: 100, PassingPoints: 50})

	title, allowLate := "Renamed", false
	edited, err := f.svc.EditAssignment(t.Context(), f.tenant, f.lesson,
		assign.AssignmentPatch{Title: &title, AllowLate: &allowLate}, f.author)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}

	if edited.Title != title {
		t.Errorf("title is %q, want %q", edited.Title, title)
	}
	if edited.AllowLate {
		t.Error("turning late work off did not reach the database")
	}
	// An unmentioned field is untouched.
	if edited.DueAt == nil || !edited.DueAt.Equal(next) {
		t.Errorf("an unmentioned deadline moved to %v, want %v", edited.DueAt, next)
	}
	if edited.Points != 100 {
		t.Errorf("an unmentioned points total became %d", edited.Points)
	}

	// And the deadline comes off.
	cleared, err := f.svc.EditAssignment(t.Context(), f.tenant, f.lesson,
		assign.AssignmentPatch{ClearDueAt: true}, f.author)
	if err != nil {
		t.Fatalf("clear the deadline: %v", err)
	}
	if cleared.DueAt != nil {
		t.Fatalf("the deadline survived being cleared: %v", cleared.DueAt)
	}

	// Not just in the returned row. Read it back.
	reloaded, err := f.svc.Assignment(t.Context(), f.tenant, f.lesson)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DueAt != nil {
		t.Errorf("the stored deadline is %v, want none", reloaded.DueAt)
	}
}

/*
Marking a submission writes the gradebook, in the same transaction.

The seam is an interface `assign` declares and `cmd` satisfies over the grade
service. Neither package imports the other, which means nothing in either of them
would notice if the wiring were dropped — except this.
*/
func TestMarkingReachesTheGradebook(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	assignment := f.assignment(t, assign.NewAssignment{Points: 20, PassingPoints: 10})
	f.enrol(t)
	f.attach(t, f.learner, "essay.txt", []byte("my essay"))

	if _, err := f.svc.Submit(t.Context(), f.tenant, f.lesson, f.learner,
		assign.Author{UserID: f.learner}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	submission, err := f.svc.Submissions(t.Context(), f.tenant, f.lesson, true, 10)
	if err != nil || len(submission) != 1 {
		t.Fatalf("marking queue: %v (%d rows)", err, len(submission))
	}

	if _, err := f.svc.Mark(t.Context(), f.tenant, submission[0].Submission.ID, 15, "Good.", f.author); err != nil {
		t.Fatalf("mark: %v", err)
	}

	grades, err := f.grades.LearnerGrades(t.Context(), f.tenant, f.course, f.learner)
	if err != nil {
		t.Fatalf("grades: %v", err)
	}
	if len(grades.Entries) != 1 {
		t.Fatalf("%d gradebook entries, want 1", len(grades.Entries))
	}
	if grades.Entries[0].Points != 15 || grades.Entries[0].MaxPoints != 20 {
		t.Errorf("recorded %d of %d, want 15 of 20",
			grades.Entries[0].Points, grades.Entries[0].MaxPoints)
	}
	if grades.Items[0].Title != assignment.Title {
		t.Errorf("the item is called %q, want %q", grades.Items[0].Title, assignment.Title)
	}

	// A marker who lowers a grade means to. `assign` does not keep the highest.
	if _, err := f.svc.Mark(t.Context(), f.tenant, submission[0].Submission.ID, 4, "On reflection.", f.author); err != nil {
		t.Fatalf("re-mark: %v", err)
	}

	after, _ := f.grades.LearnerGrades(t.Context(), f.tenant, f.course, f.learner)
	if after.Entries[0].Points != 4 {
		t.Errorf("the gradebook still says %d; the mark was lowered to 4", after.Entries[0].Points)
	}
}

func (g assignmentGrades) EnsureItem(ctx context.Context, tx pgx.Tx, tenantID, lessonID, sourceID uuid.UUID,
	title string, maxPoints int,
) error {
	return g.svc.EnsureItem(ctx, tx, tenantID, grade.Score{
		LessonID: lessonID,
		Source:   grade.SourceAssignment, SourceID: sourceID,
		Title: title, MaxPoints: maxPoints,
	})
}

/*
An assignment appears in the gradebook the moment it exists.

Before this, an item was written only when the first learner was marked — so a
course with one unmarked assignment told its learners it had nothing to grade,
and told its teacher the same. The lie was visible on the page.
*/
func TestAnUnmarkedAssignmentIsAlreadyInTheGradebook(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{Title: "Essay", Points: 30, PassingPoints: 15})

	grades, err := f.grades.LearnerGrades(t.Context(), f.tenant, f.course, f.learner)
	if err != nil {
		t.Fatalf("grades: %v", err)
	}

	if len(grades.Items) != 1 {
		t.Fatalf("%d gradebook items for an assignment nobody has been marked on, want 1", len(grades.Items))
	}
	if grades.Items[0].MaxPoints != 30 {
		t.Errorf("the item is worth %d, want 30", grades.Items[0].MaxPoints)
	}

	// And nobody has a grade for it: an item is not a mark.
	if len(grades.Entries) != 0 {
		t.Errorf("%d entries, want none", len(grades.Entries))
	}
	if grades.Result.Band.Label != "" {
		t.Errorf("an unmarked learner was graded %q", grades.Result.Band.Label)
	}
}

// Reworking an assignment reworks its item, and does not add a second one.
func TestEditingAnAssignmentUpdatesItsGradebookItem(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.assignment(t, assign.NewAssignment{Title: "Essay", Points: 30, PassingPoints: 15})

	title, points := "Essay, revised", 50
	if _, err := f.svc.EditAssignment(t.Context(), f.tenant, f.lesson,
		assign.AssignmentPatch{Title: &title, Points: &points}, f.author); err != nil {
		t.Fatalf("edit: %v", err)
	}

	grades, err := f.grades.LearnerGrades(t.Context(), f.tenant, f.course, f.learner)
	if err != nil {
		t.Fatalf("grades: %v", err)
	}
	if len(grades.Items) != 1 {
		t.Fatalf("%d items after an edit, want 1", len(grades.Items))
	}
	if grades.Items[0].Title != title || grades.Items[0].MaxPoints != points {
		t.Errorf("the item is %q worth %d, want %q worth %d",
			grades.Items[0].Title, grades.Items[0].MaxPoints, title, points)
	}
}

/*
The real certificate service, exactly as cmd/ wires it.

A stub would return nil and prove nothing: whether finishing a course issues a
certificate is the question, and `enroll` cannot answer it — it has never heard of
`certify`.
*/
type issuer struct{ svc *certify.Service }

func (i issuer) IssueIfEarned(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return i.svc.IssueIfEarned(ctx, tx, tenantID, courseID, userID)
}

type certifyAuditor struct{ recorder *audit.Recorder }

func (a certifyAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e certify.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		Metadata: e.Metadata,
	})
}

func newIssuer(db *database.DB) issuer {
	return issuer{certify.NewService(db, certify.NewPostgresRepository(), certifyAuditor{audit.NewRecorder()})}
}
