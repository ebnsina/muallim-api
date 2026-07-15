package academics

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AddPeriod puts one slot on a section's weekly timetable.
func (s *Service) AddPeriod(ctx context.Context, tenantID uuid.UUID, n NewPeriod) (Period, error) {
	if err := n.validate(); err != nil {
		return Period{}, err
	}
	var p Period
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = s.repo.CreatePeriod(ctx, tx, tenantID, n)
		return err
	})
	return p, err
}

// SectionTimetable reads a section's week, in grid order (day, then start time).
func (s *Service) SectionTimetable(ctx context.Context, tenantID, sectionID uuid.UUID) ([]Period, error) {
	var periods []Period
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		periods, err = s.repo.SectionTimetable(ctx, tx, tenantID, sectionID)
		return err
	})
	return periods, err
}

// RemovePeriod deletes one slot.
func (s *Service) RemovePeriod(ctx context.Context, tenantID, periodID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeletePeriod(ctx, tx, tenantID, periodID)
	})
}
