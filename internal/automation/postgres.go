package automation

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository is the Postgres-backed Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns a PostgresRepository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const ruleColumns = `id, event, subject, body, enabled, created_at, updated_at`

func scanRule(row pgx.CollectableRow) (Rule, error) {
	var r Rule
	err := row.Scan(&r.ID, &r.Event, &r.Subject, &r.Body, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// Create writes a rule.
func (r *PostgresRepository) Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, rule Rule) (Rule, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO automation_rules (tenant_id, event, subject, body, enabled)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+ruleColumns,
		tenantID, rule.Event, rule.Subject, rule.Body, rule.Enabled)
	if err == nil {
		var created Rule
		created, err = pgx.CollectExactlyOneRow(rows, scanRule)
		if err == nil {
			return created, nil
		}
	}
	return Rule{}, fmt.Errorf("automation: create rule: %w", err)
}

// ByID loads one rule.
func (r *PostgresRepository) ByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Rule, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+ruleColumns+` FROM automation_rules WHERE tenant_id = $1 AND id = $2`,
		tenantID, id)
	if err == nil {
		var rule Rule
		rule, err = pgx.CollectExactlyOneRow(rows, scanRule)
		if err == nil {
			return rule, nil
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	return Rule{}, fmt.Errorf("automation: load rule: %w", err)
}

// List returns a workspace's rules, newest first, bounded.
func (r *PostgresRepository) List(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Rule, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+ruleColumns+` FROM automation_rules
		 WHERE tenant_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2`,
		tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("automation: list rules: %w", err)
	}
	rules, err := pgx.CollectRows(rows, scanRule)
	if err != nil {
		return nil, fmt.Errorf("automation: list rules: %w", err)
	}
	return rules, nil
}

// Firing lists the enabled rules for one event — the query every enrolment and
// every completion runs, served by the partial index on (tenant_id, event).
func (r *PostgresRepository) Firing(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, event string) ([]Rule, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+ruleColumns+` FROM automation_rules
		 WHERE tenant_id = $1 AND event = $2 AND enabled
		 ORDER BY created_at`,
		tenantID, event)
	if err != nil {
		return nil, fmt.Errorf("automation: firing rules: %w", err)
	}
	rules, err := pgx.CollectRows(rows, scanRule)
	if err != nil {
		return nil, fmt.Errorf("automation: firing rules: %w", err)
	}
	return rules, nil
}

// Update applies a patch. A nil field is left as it was — `coalesce` on the
// parameter, so one statement serves every combination of fields.
func (r *PostgresRepository) Update(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p RulePatch) (Rule, error) {
	rows, err := tx.Query(ctx,
		`UPDATE automation_rules
		 SET subject    = coalesce($3, subject),
		     body       = coalesce($4, body),
		     enabled    = coalesce($5, enabled),
		     updated_at = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+ruleColumns,
		tenantID, id, p.Subject, p.Body, p.Enabled)
	if err == nil {
		var updated Rule
		updated, err = pgx.CollectExactlyOneRow(rows, scanRule)
		if err == nil {
			return updated, nil
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	return Rule{}, fmt.Errorf("automation: update rule: %w", err)
}

// Delete removes a rule.
func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM automation_rules WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("automation: delete rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
