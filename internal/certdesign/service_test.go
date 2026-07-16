package certdesign_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/certdesign"
	"github.com/ebnsina/muallim-api/internal/platform/blob"
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
	sub := "c" + id.String()[:8]
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

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, certdesign.AuditEntry) error {
	return nil
}

func newService(db *database.DB) *certdesign.Service {
	return certdesign.NewService(db, certdesign.NewPostgresRepository(), blob.Unconfigured{}, stubAuditor{})
}

func TestDesignLifecycle(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := certdesign.Author{UserID: uuid.New()}

	layout := json.RawMessage(`[{"id":"a","kind":"title","x":0.1,"y":0.2,"w":0.8,"fontSize":48,"fontWeight":700,"color":"#111","align":"center","text":"Certificate"}]`)

	// Create.
	d, err := svc.Create(t.Context(), tenant, certdesign.NewDesign{
		Name: "Graduation", Orientation: certdesign.OrientationPortrait, Accent: "#4b2e83",
		BackgroundColor: "#ffffff", Layout: layout,
	}, author)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.Orientation != certdesign.OrientationPortrait || d.Name != "Graduation" {
		t.Fatalf("unexpected design: %+v", d)
	}

	// A second design, to list two.
	if _, err := svc.Create(t.Context(), tenant, certdesign.NewDesign{Name: "Award"}, author); err != nil {
		t.Fatalf("create second: %v", err)
	}

	// List, keyset with a page size of one, walking the cursor.
	first, err := svc.List(t.Context(), tenant, certdesign.PageParams{Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(first.Designs) != 1 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	second, err := svc.List(t.Context(), tenant, certdesign.PageParams{Limit: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(second.Designs) != 1 || second.HasMore {
		t.Fatalf("second page: %+v", second)
	}

	// Get.
	got, err := svc.Get(t.Context(), tenant, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != d.ID {
		t.Fatalf("got %v, want %v", got.ID, d.ID)
	}

	// Update name, orientation, and layout.
	name := "Diploma"
	orient := certdesign.OrientationLandscape
	newLayout := json.RawMessage(`[{"id":"b","kind":"learner","x":0.2,"y":0.5,"w":0.6}]`)
	up, err := svc.Update(t.Context(), tenant, d.ID, certdesign.DesignPatch{
		Name: &name, Orientation: &orient, Layout: newLayout,
	}, author)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if up.Name != "Diploma" || up.Orientation != certdesign.OrientationLandscape {
		t.Fatalf("update did not apply: %+v", up)
	}

	// Delete, then a get 404s.
	if err := svc.Delete(t.Context(), tenant, d.ID, author); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(t.Context(), tenant, d.ID); !errors.Is(err, certdesign.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestInvalidOrientationIsRejected(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := certdesign.Author{UserID: uuid.New()}

	if _, err := svc.Create(t.Context(), tenant, certdesign.NewDesign{
		Name: "Bad", Orientation: "diagonal",
	}, author); !errors.Is(err, certdesign.ErrInvalidDesign) {
		t.Fatalf("an invalid orientation was accepted: %v", err)
	}
}

func TestInvalidLayoutIsRejected(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := certdesign.Author{UserID: uuid.New()}

	cases := map[string]json.RawMessage{
		"unknown kind":  json.RawMessage(`[{"id":"a","kind":"banana","x":0.1,"y":0.1,"w":0.5}]`),
		"x out of unit": json.RawMessage(`[{"id":"a","kind":"title","x":1.5,"y":0.1,"w":0.5}]`),
		"not an array":  json.RawMessage(`{"id":"a","kind":"title"}`),
	}
	for name, layout := range cases {
		if _, err := svc.Create(t.Context(), tenant, certdesign.NewDesign{
			Name: name, Layout: layout,
		}, author); !errors.Is(err, certdesign.ErrInvalidLayout) {
			t.Fatalf("%s: an invalid layout was accepted: %v", name, err)
		}
	}
}

func TestGetUnknownIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)

	if _, err := svc.Get(t.Context(), tenant, uuid.New()); !errors.Is(err, certdesign.ErrNotFound) {
		t.Fatalf("an unknown design was found: %v", err)
	}
}
