package coursebuild_test

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

	"github.com/ebnsina/muallim-api/internal/coursebuild"
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

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, coursebuild.AuditEntry) error {
	return nil
}

func newService(db *database.DB) *coursebuild.Service {
	return coursebuild.NewService(db, coursebuild.NewPostgresRepository(), stubAuditor{})
}

// A well-formed structure: one module with two lessons of known kinds.
const goodStructure = `[{"id":"m1","title":"Foundations","lessons":[` +
	`{"id":"l1","title":"Intro","kind":"video","notes":"welcome"},` +
	`{"id":"l2","title":"Quiz","kind":"quiz","notes":""}]}]`

func TestBlueprintLifecycle(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := coursebuild.Author{UserID: uuid.New()}

	// Create with a valid structure.
	b, err := svc.Create(t.Context(), tenant, coursebuild.NewBlueprint{
		Name: "Arabic 101", Description: "A beginner track", Structure: json.RawMessage(goodStructure),
	}, author)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.Name != "Arabic 101" || b.ID == uuid.Nil {
		t.Fatalf("unexpected blueprint: %+v", b)
	}

	// A second, to exercise the keyset listing.
	b2, err := svc.Create(t.Context(), tenant, coursebuild.NewBlueprint{Name: "Fiqh Basics"}, author)
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	// List, newest first, with a page limit of 1 to force a cursor.
	first, err := svc.List(t.Context(), tenant, coursebuild.PageParams{Limit: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(first.Blueprints) != 1 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page wrong: %+v", first)
	}
	if first.Blueprints[0].ID != b2.ID {
		t.Fatalf("newest first violated: got %s, want %s", first.Blueprints[0].ID, b2.ID)
	}
	second, err := svc.List(t.Context(), tenant, coursebuild.PageParams{Limit: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("list page two: %v", err)
	}
	if len(second.Blueprints) != 1 || second.Blueprints[0].ID != b.ID || second.HasMore {
		t.Fatalf("second page wrong: %+v", second)
	}

	// Get returns the whole document.
	got, err := svc.Get(t.Context(), tenant, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Arabic 101" {
		t.Fatalf("get returned %q", got.Name)
	}

	// Update the name only; the structure must survive untouched.
	newName := "Arabic for Beginners"
	upd, err := svc.Update(t.Context(), tenant, b.ID, coursebuild.BlueprintPatch{Name: &newName}, author)
	if err != nil {
		t.Fatalf("update name: %v", err)
	}
	if upd.Name != newName {
		t.Fatalf("name not updated: %q", upd.Name)
	}
	var mods []map[string]any
	if err := json.Unmarshal(upd.Structure, &mods); err != nil || len(mods) != 1 {
		t.Fatalf("structure disturbed by a name edit: %s (%v)", upd.Structure, err)
	}

	// Update the description and the structure together.
	newDesc := "Now with more lessons"
	newStructure := json.RawMessage(`[{"id":"m1","title":"Only module","lessons":[]}]`)
	upd2, err := svc.Update(t.Context(), tenant, b.ID, coursebuild.BlueprintPatch{
		Description: &newDesc, Structure: newStructure,
	}, author)
	if err != nil {
		t.Fatalf("update structure: %v", err)
	}
	if upd2.Description != newDesc {
		t.Fatalf("description not updated: %q", upd2.Description)
	}
	if err := json.Unmarshal(upd2.Structure, &mods); err != nil || len(mods) != 1 {
		t.Fatalf("structure not replaced: %s", upd2.Structure)
	}

	// Delete, then confirm it is gone.
	if err := svc.Delete(t.Context(), tenant, b.ID, author); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(t.Context(), tenant, b.ID); !errors.Is(err, coursebuild.ErrNotFound) {
		t.Fatalf("blueprint survived delete: %v", err)
	}
}

func TestInvalidStructureIsRejected(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := coursebuild.Author{UserID: uuid.New()}

	cases := map[string]string{
		"not an array":         `{"title":"nope"}`,
		"unknown lesson kind":  `[{"id":"m","title":"M","lessons":[{"id":"l","title":"L","kind":"podcast"}]}]`,
		"missing module title": `[{"id":"m","lessons":[]}]`,
	}
	for name, structure := range cases {
		_, err := svc.Create(t.Context(), tenant, coursebuild.NewBlueprint{
			Name: "X", Structure: json.RawMessage(structure),
		}, author)
		if !errors.Is(err, coursebuild.ErrInvalidBlueprint) {
			t.Errorf("%s: accepted an invalid structure: %v", name, err)
		}
	}

	// A blank name is refused as well.
	if _, err := svc.Create(t.Context(), tenant, coursebuild.NewBlueprint{Name: "   "}, author); !errors.Is(err, coursebuild.ErrInvalidBlueprint) {
		t.Errorf("a blank name was accepted: %v", err)
	}
}

func TestUnknownBlueprintIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := coursebuild.Author{UserID: uuid.New()}

	if _, err := svc.Get(t.Context(), tenant, uuid.New()); !errors.Is(err, coursebuild.ErrNotFound) {
		t.Errorf("get of a missing blueprint: %v", err)
	}
	name := "Renamed"
	if _, err := svc.Update(t.Context(), tenant, uuid.New(), coursebuild.BlueprintPatch{Name: &name}, author); !errors.Is(err, coursebuild.ErrNotFound) {
		t.Errorf("update of a missing blueprint: %v", err)
	}
	if err := svc.Delete(t.Context(), tenant, uuid.New(), author); !errors.Is(err, coursebuild.ErrNotFound) {
		t.Errorf("delete of a missing blueprint: %v", err)
	}
}
