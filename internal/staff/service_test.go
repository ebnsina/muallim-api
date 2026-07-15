package staff_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
	"github.com/ebnsina/muallim-api/internal/staff"
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

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, staff.AuditEntry) error { return nil }

func newService(db *database.DB) *staff.Service {
	return staff.NewService(db, staff.NewPostgresRepository(), stubAuditor{})
}

func TestStaffRoster(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := staff.Author{UserID: uuid.New()}

	if _, err := svc.Hire(t.Context(), tenant, staff.NewStaff{
		StaffNo: "T-001", FullName: "Karim Uddin", Role: staff.RoleTeacher,
	}, author); err != nil {
		t.Fatalf("hire teacher: %v", err)
	}
	if _, err := svc.Hire(t.Context(), tenant, staff.NewStaff{
		FullName: "Ayesha Begum", Role: staff.RoleAccountant,
	}, author); err != nil {
		t.Fatalf("hire accountant: %v", err)
	}

	// An unknown role is refused.
	if _, err := svc.Hire(t.Context(), tenant, staff.NewStaff{FullName: "Nobody", Role: "wizard"}, author); !errors.Is(err, staff.ErrInvalidStaff) {
		t.Fatalf("an unknown role was accepted: %v", err)
	}

	// A duplicate staff number is refused; a blank one is not deduplicated.
	if _, err := svc.Hire(t.Context(), tenant, staff.NewStaff{StaffNo: "T-001", FullName: "Someone"}, author); !errors.Is(err, staff.ErrStaffNoTaken) {
		t.Fatalf("a duplicate staff number was accepted: %v", err)
	}

	// The roster is by name; the role filter narrows it.
	page, err := svc.Roster(t.Context(), tenant, staff.RosterFilter{}, staff.PageParams{Limit: 50})
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(page.Staff) != 2 || page.Staff[0].FullName != "Ayesha Begum" {
		t.Fatalf("roster not sorted by name: %+v", page.Staff)
	}

	teachers, err := svc.Roster(t.Context(), tenant, staff.RosterFilter{Role: staff.RoleTeacher}, staff.PageParams{Limit: 50})
	if err != nil {
		t.Fatalf("teacher roster: %v", err)
	}
	if len(teachers.Staff) != 1 || teachers.Staff[0].FullName != "Karim Uddin" {
		t.Fatalf("role filter wrong: %+v", teachers.Staff)
	}

	// Editing a record's status sticks.
	id := teachers.Staff[0].ID
	inactive := "inactive"
	edited, err := svc.Edit(t.Context(), tenant, id, staff.StaffPatch{Status: &inactive})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.Status != "inactive" {
		t.Fatalf("status is %q, want inactive", edited.Status)
	}
}
