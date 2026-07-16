package liveclass_test

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

	"github.com/ebnsina/muallim-api/internal/liveclass"
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
	sub := "l" + id.String()[:8]
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

func seedUser(t *testing.T, db *database.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'U')`,
			id, "u-"+id.String()[:8]+"@example.test")
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedCourse(t *testing.T, db *database.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	slug := "c-" + uuid.NewString()[:8]
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, $2, 'Course', 'published') RETURNING id`,
			tenantID, slug).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, liveclass.AuditEntry) error { return nil }

func newService(db *database.DB) *liveclass.Service {
	return liveclass.NewService(db, liveclass.NewPostgresRepository(), stubAuditor{})
}

func TestLiveSessionFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)

	host := seedUser(t, db, tenant)
	course := seedCourse(t, db, tenant)
	author := liveclass.Author{UserID: host}

	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	end := start.Add(time.Hour)

	created, err := svc.Create(t.Context(), tenant, course, liveclass.NewSession{
		Title: "Tajweed live", Description: "Bring questions", JoinURL: "https://meet.example.com/abc",
		StartsAt: start, EndsAt: &end, HostUserID: &host,
	}, author)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == uuid.Nil || created.JoinURL != "https://meet.example.com/abc" {
		t.Fatalf("created session is wrong: %+v", created)
	}

	// A second session, sooner, so the DESC keyset order is observable.
	later := start.Add(48 * time.Hour)
	second, err := svc.Create(t.Context(), tenant, course, liveclass.NewSession{
		Title: "Follow-up", StartsAt: later,
	}, author)
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	// Listing a course returns its sessions, latest start first.
	page, err := svc.ListForCourse(t.Context(), tenant, course, liveclass.PageParams{Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].ID != second.ID {
		t.Fatalf("first page should be the latest session, got %+v", page.Sessions)
	}
	if !page.HasMore || page.NextCursor == "" {
		t.Fatalf("expected a next page, got %+v", page)
	}

	next, err := svc.ListForCourse(t.Context(), tenant, course, liveclass.PageParams{Limit: 1, Cursor: page.NextCursor})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(next.Sessions) != 1 || next.Sessions[0].ID != created.ID {
		t.Fatalf("second page should be the earlier session, got %+v", next.Sessions)
	}
	if next.HasMore {
		t.Fatalf("expected no more pages, got %+v", next)
	}

	// Get returns one by id.
	got, err := svc.Get(t.Context(), tenant, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Tajweed live" {
		t.Fatalf("get title %q", got.Title)
	}

	// Update changes the fields it names and leaves the rest.
	newTitle := "Tajweed live (rescheduled)"
	updated, err := svc.Update(t.Context(), tenant, created.ID, liveclass.SessionPatch{
		Title: &newTitle, ClearEndsAt: true,
	}, author)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Title != newTitle || updated.EndsAt != nil {
		t.Fatalf("update did not take: %+v", updated)
	}

	// Delete removes it; a second delete finds nothing.
	if err := svc.Delete(t.Context(), tenant, created.ID, author); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.Delete(t.Context(), tenant, created.ID, author); !errors.Is(err, liveclass.ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	course := seedCourse(t, db, tenant)
	author := liveclass.Author{UserID: uuid.New()}
	start := time.Now().Add(time.Hour)

	// A non-http(s) join URL is refused.
	if _, err := svc.Create(t.Context(), tenant, course, liveclass.NewSession{
		Title: "Bad link", JoinURL: "ftp://nope", StartsAt: start,
	}, author); !errors.Is(err, liveclass.ErrInvalidSession) {
		t.Fatalf("a bad url was accepted: %v", err)
	}

	// An empty title is refused.
	if _, err := svc.Create(t.Context(), tenant, course, liveclass.NewSession{
		Title: "   ", StartsAt: start,
	}, author); !errors.Is(err, liveclass.ErrInvalidSession) {
		t.Fatalf("an empty title was accepted: %v", err)
	}

	// A backwards range is refused.
	before := start.Add(-time.Hour)
	if _, err := svc.Create(t.Context(), tenant, course, liveclass.NewSession{
		Title: "Backwards", StartsAt: start, EndsAt: &before,
	}, author); !errors.Is(err, liveclass.ErrInvalidSession) {
		t.Fatalf("a backwards range was accepted: %v", err)
	}

	// An unknown session is a not-found.
	if _, err := svc.Get(t.Context(), tenant, uuid.New()); !errors.Is(err, liveclass.ErrNotFound) {
		t.Fatalf("get unknown: %v, want ErrNotFound", err)
	}
}
