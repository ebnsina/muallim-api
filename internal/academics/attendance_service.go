package academics

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MarkAttendance records a section's register for one day. Re-marking a day it has
// already seen updates each student's status rather than stacking a second row —
// the unique index on (tenant, student, day) is what the upsert conflicts on. It
// returns how many rows the mark touched.
func (s *Service) MarkAttendance(ctx context.Context, tenantID uuid.UUID, m AttendanceMark, author Author) (int, error) {
	if err := m.validate(); err != nil {
		return 0, err
	}
	m.MarkedBy = author.UserID

	var marked int
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		marked, err = s.repo.MarkAttendance(ctx, tx, tenantID, m)
		if err != nil {
			return err
		}
		md := map[string]any{"on_date": m.OnDate.Format("2006-01-02"), "count": marked}
		if m.SectionID != nil {
			md["section_id"] = m.SectionID.String()
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionAttendanceMark,
			TargetType: "attendance", TargetID: m.OnDate.Format("2006-01-02"),
			Metadata: md,
		})
	})
	return marked, err
}

// Register returns a section's marks for one day, by student name.
func (s *Service) Register(ctx context.Context, tenantID, sectionID uuid.UUID, on time.Time) ([]RegisterEntry, error) {
	var rows []RegisterEntry
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = s.repo.Register(ctx, tx, tenantID, sectionID, on)
		return err
	})
	return rows, err
}

// StudentAttendance returns one student's history over a date range, oldest first,
// with a summary of the days by status.
func (s *Service) StudentAttendance(ctx context.Context, tenantID, studentID uuid.UUID, from, to time.Time) ([]AttendanceDay, AttendanceSummary, error) {
	var (
		days    []AttendanceDay
		summary AttendanceSummary
	)
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		days, summary, err = s.repo.StudentAttendance(ctx, tx, tenantID, studentID, from, to)
		return err
	})
	return days, summary, err
}
