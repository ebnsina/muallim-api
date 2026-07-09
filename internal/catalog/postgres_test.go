package catalog_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository tests run against a real Postgres. Mocking a database tests the
// mock: neither row-level security nor a query plan has an opinion about a fake.
//
// Set LMS_TEST_DATABASE_URL to enable. CI always sets it.
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

// seedTenant creates an isolated tenant per test, so tests may run in parallel
// against one database without seeing each other's rows — which is also a live
// exercise of the isolation under test.
func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()

	id := uuid.New()
	sub := "t" + id.String()[:8]

	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`,
			id, sub, "Test "+sub)
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

// seedCourse inserts a course with topics topics, each holding lessonsPer lessons.
func seedCourse(t *testing.T, db *database.DB, tenantID uuid.UUID, slug string, topics, lessonsPer int) uuid.UUID {
	t.Helper()

	var courseID uuid.UUID
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status)
			 VALUES ($1, $2, $3, 'published') RETURNING id`,
			tenantID, slug, "Course "+slug).Scan(&courseID)
		if err != nil {
			return err
		}

		for i := range topics {
			var topicID uuid.UUID
			err := tx.QueryRow(ctx,
				`INSERT INTO topics (tenant_id, course_id, title, position)
				 VALUES ($1, $2, $3, $4) RETURNING id`,
				tenantID, courseID, fmt.Sprintf("Topic %d", i), i).Scan(&topicID)
			if err != nil {
				return err
			}

			for j := range lessonsPer {
				_, err := tx.Exec(ctx,
					`INSERT INTO lessons (tenant_id, topic_id, title, position, duration_seconds)
					 VALUES ($1, $2, $3, $4, $5)`,
					tenantID, topicID, fmt.Sprintf("Lesson %d.%d", i, j), j, 60)
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}
	return courseID
}

func newService(db *database.DB) *catalog.Service {
	return catalog.NewService(db, catalog.NewPostgresRepository())
}

// The regression guard. Loading a curriculum must cost a fixed number of queries
// no matter how large the course is.
//
// If someone replaces the batched lesson fetch with a loop over topics, the query
// count grows with the fixture and this fails — at the size where it is cheap to
// notice, rather than in production at the size where it is not.
func TestCurriculumHasNoNPlusOne(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	svc := newService(db)

	const wantQueries = 3 // course, topics, lessons

	sizes := []struct {
		name               string
		topics, lessonsPer int
	}{
		{"small course", 2, 2},
		{"medium course", 5, 4},
		{"large course", 20, 10},
	}

	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			slug := fmt.Sprintf("c-%d-%d-%s", size.topics, size.lessonsPer, uuid.NewString()[:8])
			seedCourse(t, db, tenantID, slug, size.topics, size.lessonsPer)

			counter := &database.Counter{}
			ctx := database.WithCounter(t.Context(), counter)

			curriculum, err := svc.Curriculum(ctx, tenantID, slug)
			if err != nil {
				t.Fatalf("curriculum: %v", err)
			}

			// Sanity: the fixture really is as big as we claim, so a passing query
			// count cannot be an artefact of loading nothing.
			if got := len(curriculum.Topics); got != size.topics {
				t.Fatalf("topics = %d, want %d", got, size.topics)
			}
			if got := curriculum.LessonCount(); got != size.topics*size.lessonsPer {
				t.Fatalf("lessons = %d, want %d", got, size.topics*size.lessonsPer)
			}

			if got := counter.Count(); got != wantQueries {
				t.Errorf("curriculum of %d topics x %d lessons issued %d queries, want %d — "+
					"the query count must not grow with the size of the course",
					size.topics, size.lessonsPer, got, wantQueries)
			}
		})
	}
}

// A course with no topics must not issue the lesson query at all.
func TestCurriculumSkipsLessonQueryWhenNoTopics(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	svc := newService(db)

	slug := "empty-" + uuid.NewString()[:8]
	seedCourse(t, db, tenantID, slug, 0, 0)

	counter := &database.Counter{}
	ctx := database.WithCounter(t.Context(), counter)

	if _, err := svc.Curriculum(ctx, tenantID, slug); err != nil {
		t.Fatalf("curriculum: %v", err)
	}

	if got := counter.Count(); got != 2 {
		t.Errorf("queries = %d, want 2 (course, topics) — an empty course must not query lessons", got)
	}
}

func TestCurriculumOrdersTopicsAndLessons(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	svc := newService(db)

	slug := "ordered-" + uuid.NewString()[:8]
	seedCourse(t, db, tenantID, slug, 3, 3)

	curriculum, err := svc.Curriculum(t.Context(), tenantID, slug)
	if err != nil {
		t.Fatalf("curriculum: %v", err)
	}

	for i, topic := range curriculum.Topics {
		if topic.Position != i {
			t.Errorf("topic %d has position %d; topics must arrive ordered from the index", i, topic.Position)
		}
		for j, lesson := range topic.Lessons {
			if lesson.Position != j {
				t.Errorf("topic %d lesson %d has position %d; lessons must arrive ordered", i, j, lesson.Position)
			}
		}
	}

	if want := 9 * time.Minute; curriculum.TotalDuration() != want {
		t.Errorf("total duration = %v, want %v", curriculum.TotalDuration(), want)
	}
}

func TestCurriculumNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)

	_, err := newService(db).Curriculum(t.Context(), tenantID, "no-such-course")
	if err == nil {
		t.Fatal("expected an error for a missing course")
	}
}
