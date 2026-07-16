package calendar_test

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

	"github.com/ebnsina/muallim-api/internal/calendar"
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
	sub := "s" + id.String()[:8]
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

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, calendar.AuditEntry) error { return nil }

func newService(db *database.DB) *calendar.Service {
	return calendar.NewService(db, calendar.NewPostgresRepository(), stubAuditor{})
}

func date(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}

func TestCalendarLifecycle(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := calendar.Author{UserID: uuid.New()}

	// One event of each kind, on distinct start dates.
	mk := func(title, kind, starts string, ends *string) calendar.Event {
		n := calendar.NewEvent{Title: title, Kind: kind, StartsOn: date(t, starts)}
		if ends != nil {
			e := date(t, *ends)
			n.EndsOn = &e
		}
		ev, err := svc.CreateEvent(t.Context(), tenant, n, author)
		if err != nil {
			t.Fatalf("create %s: %v", kind, err)
		}
		return ev
	}

	winter := "2026-12-31"
	mk("Winter Break", calendar.KindHoliday, "2026-12-20", &winter)
	exam := mk("Midterm", calendar.KindExam, "2026-06-10", nil)
	mk("Sports Day", calendar.KindEvent, "2026-03-15", nil)
	mk("Term One Begins", calendar.KindTermStart, "2026-01-05", nil)
	mk("Term One Ends", calendar.KindTermEnd, "2026-05-30", nil)

	// The full list is newest first, and there are five.
	page, err := svc.ListEvents(t.Context(), tenant, calendar.EventFilter{}, calendar.PageParams{Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Events) != 5 {
		t.Fatalf("want 5 events, got %d", len(page.Events))
	}
	if page.Events[0].Title != "Winter Break" {
		t.Fatalf("not newest first: %+v", page.Events)
	}

	// Filter by kind.
	exams, err := svc.ListEvents(t.Context(), tenant, calendar.EventFilter{Kind: calendar.KindExam}, calendar.PageParams{Limit: 50})
	if err != nil {
		t.Fatalf("list exams: %v", err)
	}
	if len(exams.Events) != 1 || exams.Events[0].ID != exam.ID {
		t.Fatalf("kind filter wrong: %+v", exams.Events)
	}

	// Filter by a date window: only the spring/summer term entries.
	from, to := date(t, "2026-03-01"), date(t, "2026-06-30")
	window, err := svc.ListEvents(t.Context(), tenant,
		calendar.EventFilter{From: &from, To: &to}, calendar.PageParams{Limit: 50})
	if err != nil {
		t.Fatalf("list window: %v", err)
	}
	if len(window.Events) != 3 {
		t.Fatalf("want 3 in window, got %d: %+v", len(window.Events), window.Events)
	}

	// Pagination: one per page walks the whole calendar.
	first, err := svc.ListEvents(t.Context(), tenant, calendar.EventFilter{}, calendar.PageParams{Limit: 1})
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("expected more pages: %+v", first)
	}
	second, err := svc.ListEvents(t.Context(), tenant, calendar.EventFilter{},
		calendar.PageParams{Limit: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(second.Events) != 1 || second.Events[0].ID == first.Events[0].ID {
		t.Fatalf("cursor did not advance: %+v", second.Events)
	}

	// An invalid cursor is refused.
	if _, err := svc.ListEvents(t.Context(), tenant, calendar.EventFilter{},
		calendar.PageParams{Cursor: "not-base64!!"}); !errors.Is(err, calendar.ErrInvalidPage) {
		t.Fatalf("bad cursor accepted: %v", err)
	}

	// Update: rename the exam and give it an end.
	title := "Final Exam"
	end := "2026-06-12"
	endDate := date(t, end)
	updated, err := svc.UpdateEvent(t.Context(), tenant, exam.ID, calendar.EventPatch{
		Title: &title, EndsOn: &endDate,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Title != "Final Exam" || updated.EndsOn == nil {
		t.Fatalf("update did not stick: %+v", updated)
	}

	// Delete removes it; a second delete is not found.
	if err := svc.DeleteEvent(t.Context(), tenant, exam.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.DeleteEvent(t.Context(), tenant, exam.ID); !errors.Is(err, calendar.ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}

	// Updating an unknown id is not found.
	if _, err := svc.UpdateEvent(t.Context(), tenant, uuid.New(), calendar.EventPatch{Title: &title}); !errors.Is(err, calendar.ErrNotFound) {
		t.Fatalf("update unknown: want ErrNotFound, got %v", err)
	}
}

func TestCalendarRejectsInvalidEvents(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := calendar.Author{UserID: uuid.New()}

	// Empty title.
	if _, err := svc.CreateEvent(t.Context(), tenant, calendar.NewEvent{
		Kind: calendar.KindEvent, StartsOn: date(t, "2026-01-01"),
	}, author); !errors.Is(err, calendar.ErrInvalidEvent) {
		t.Fatalf("empty title accepted: %v", err)
	}

	// ends_on before starts_on.
	ends := date(t, "2026-01-01")
	if _, err := svc.CreateEvent(t.Context(), tenant, calendar.NewEvent{
		Title: "Backwards", Kind: calendar.KindEvent, StartsOn: date(t, "2026-02-01"), EndsOn: &ends,
	}, author); !errors.Is(err, calendar.ErrInvalidEvent) {
		t.Fatalf("backwards span accepted: %v", err)
	}

	// An unknown kind.
	if _, err := svc.CreateEvent(t.Context(), tenant, calendar.NewEvent{
		Title: "Odd", Kind: "wizardry", StartsOn: date(t, "2026-01-01"),
	}, author); !errors.Is(err, calendar.ErrInvalidEvent) {
		t.Fatalf("unknown kind accepted: %v", err)
	}
}
