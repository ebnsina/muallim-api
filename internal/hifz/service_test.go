package hifz_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/hifz"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set")
	}
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	sub := "h" + id.String()[:8]
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`, id, sub, "Test "+sub)
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

func seedStudent(t *testing.T, db *database.DB, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO students (tenant_id, admission_no, full_name) VALUES ($1, '2025-001', 'Zayd') RETURNING id`,
			tenant).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed student: %v", err)
	}
	return id
}

func seedUser(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, "u"+id.String()[:8]+"@example.test")
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
			return err
		})
	})
	return id
}

func newService(db *database.DB) *hifz.Service {
	return hifz.NewService(db, hifz.NewPostgresRepository())
}

func TestHifzLogAndSummary(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	student := seedStudent(t, db, tenant)
	author := hifz.Author{UserID: seedUser(t, db)}

	day1 := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)

	// A backwards ayah range is refused.
	if _, err := svc.Log(t.Context(), tenant, hifz.NewEntry{
		StudentID: student, OnDate: day1, Kind: hifz.Sabaq, Surah: 2, AyahFrom: 20, AyahTo: 10,
	}, author); !errors.Is(err, hifz.ErrInvalidEntry) {
		t.Fatalf("a backwards range was accepted: %v", err)
	}

	// Two Sabaq days and a Sabqi revision.
	if _, err := svc.Log(t.Context(), tenant, hifz.NewEntry{
		StudentID: student, OnDate: day1, Kind: hifz.Sabaq, Surah: 2, AyahFrom: 1, AyahTo: 20, Rating: hifz.Good,
	}, author); err != nil {
		t.Fatalf("log day1 sabaq: %v", err)
	}
	if _, err := svc.Log(t.Context(), tenant, hifz.NewEntry{
		StudentID: student, OnDate: day2, Kind: hifz.Sabaq, Surah: 2, AyahFrom: 21, AyahTo: 40, Rating: hifz.Excellent,
	}, author); err != nil {
		t.Fatalf("log day2 sabaq: %v", err)
	}
	if _, err := svc.Log(t.Context(), tenant, hifz.NewEntry{
		StudentID: student, OnDate: day2, Kind: hifz.Sabqi, Surah: 1, AyahFrom: 1, AyahTo: 7,
	}, author); err != nil {
		t.Fatalf("log sabqi: %v", err)
	}

	// The log is newest first.
	page, err := svc.StudentLog(t.Context(), tenant, student, hifz.PageParams{Limit: 50})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(page.Entries) != 3 || !page.Entries[0].OnDate.Equal(day2) {
		t.Fatalf("log not newest first: %+v", page.Entries)
	}

	// The summary's current Sabaq is the latest one, and the counts are by kind.
	summary, err := svc.Summary(t.Context(), tenant, student, day1.AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.CurrentSabaq == nil || summary.CurrentSabaq.AyahFrom != 21 {
		t.Fatalf("current sabaq is %+v, want the day-2 lesson", summary.CurrentSabaq)
	}
	if summary.Counts[hifz.Sabaq] != 2 || summary.Counts[hifz.Sabqi] != 1 {
		t.Fatalf("counts are %+v, want 2 sabaq and 1 sabqi", summary.Counts)
	}
}

func TestHifzSummaryOfAStudentWithNoSabaq(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	student := seedStudent(t, db, tenant)

	// No entries yet: a summary is not an error, it just has no current position.
	summary, err := svc.Summary(t.Context(), tenant, student, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.CurrentSabaq != nil || len(summary.Counts) != 0 {
		t.Fatalf("an empty log should summarise to nothing, got %+v", summary)
	}
}
