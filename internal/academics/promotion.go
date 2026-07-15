package academics

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Promotion moves a class's active students on at year's end. A target class
// promotes them into it (optionally placing them in a section); no target
// graduates them — the final class has nowhere higher to go.
type Promotion struct {
	ToGradeLevelID *uuid.UUID
	ToSectionID    *uuid.UUID
}

// PromoteClass moves every active student in a class up to the next class, or
// graduates them when no target is given. One statement, so the whole class moves
// together or not at all. Returns how many students moved.
func (s *Service) PromoteClass(ctx context.Context, tenantID, sourceClassID uuid.UUID, p Promotion, author Author) (int, error) {
	if p.ToGradeLevelID != nil && *p.ToGradeLevelID == sourceClassID {
		return 0, ErrInvalidPromotion
	}

	var moved int
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		moved, err = s.repo.PromoteClass(ctx, tx, tenantID, sourceClassID, p)
		if err != nil {
			return err
		}
		md := map[string]any{"moved": moved, "graduated": p.ToGradeLevelID == nil}
		if p.ToGradeLevelID != nil {
			md["to_class"] = p.ToGradeLevelID.String()
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionClassPromoted,
			TargetType: "grade_level", TargetID: sourceClassID.String(),
			Metadata: md,
		})
	})
	return moved, err
}

// PromoteClass performs the move in a single UPDATE: with a target the students
// are placed in the new class (and section, which a null clears); with none they
// are marked graduated. Only active students are touched, so re-running is safe.
func (r *PostgresRepository) PromoteClass(ctx context.Context, tx pgx.Tx, tenantID, sourceClassID uuid.UUID, p Promotion) (int, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE students SET
		     grade_level_id = coalesce($3::uuid, grade_level_id),
		     section_id     = CASE WHEN $3::uuid IS NULL THEN section_id ELSE $4::uuid END,
		     status         = CASE WHEN $3::uuid IS NULL THEN 'graduated' ELSE status END,
		     updated_at     = now()
		 WHERE tenant_id = $1 AND grade_level_id = $2 AND status = 'active'`,
		tenantID, sourceClassID, p.ToGradeLevelID, p.ToSectionID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
