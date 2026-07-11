package assess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// loadAsNew reads a question and its options into the author's NewQuestion shape,
// from either the quiz-questions tables or the bank tables. `owner` is the column
// the options hang off (question_id or bank_question_id).
func loadAsNew(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, qTable, oTable, ownerCol string) (NewQuestion, error) {
	var (
		n        NewQuestion
		accepted []byte
	)
	err := tx.QueryRow(ctx,
		`SELECT type, prompt, points, explanation, case_sensitive, accepted FROM `+qTable+`
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, id).Scan(&n.Type, &n.Prompt, &n.Points, &n.Explanation, &n.CaseSensitive, &accepted)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return NewQuestion{}, ErrNotFound
		}
		return NewQuestion{}, fmt.Errorf("assess: load question for copy: %w", err)
	}
	if err := json.Unmarshal(accepted, &n.Accepted); err != nil {
		return NewQuestion{}, fmt.Errorf("assess: decode accepted: %w", err)
	}

	rows, err := tx.Query(ctx,
		`SELECT content, is_correct, match_content FROM `+oTable+`
		 WHERE tenant_id = $1 AND `+ownerCol+` = $2 ORDER BY position`,
		tenantID, id)
	if err != nil {
		return NewQuestion{}, fmt.Errorf("assess: load options for copy: %w", err)
	}
	defer rows.Close()

	n.Options, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (NewOption, error) {
		var o NewOption
		err := row.Scan(&o.Content, &o.IsCorrect, &o.MatchContent)
		return o, err
	})
	if err != nil {
		return NewQuestion{}, fmt.Errorf("assess: scan options for copy: %w", err)
	}
	return n, nil
}

// QuestionAsNew loads a quiz question in the shape needed to copy it into the bank.
func (r *PostgresRepository) QuestionAsNew(ctx context.Context, tx pgx.Tx, tenantID, questionID uuid.UUID) (NewQuestion, error) {
	return loadAsNew(ctx, tx, tenantID, questionID, "questions", "question_options", "question_id")
}

// BankQuestionAsNew loads a bank question in the shape needed to copy it into a quiz.
func (r *PostgresRepository) BankQuestionAsNew(ctx context.Context, tx pgx.Tx, tenantID, bankQuestionID uuid.UUID) (NewQuestion, error) {
	return loadAsNew(ctx, tx, tenantID, bankQuestionID, "bank_questions", "bank_question_options", "bank_question_id")
}

// SaveToBank writes a question and its options into the bank.
func (r *PostgresRepository) SaveToBank(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, category string, n NewQuestion) (BankQuestion, error) {
	accepted, err := json.Marshal(acceptedOrEmpty(n.Accepted))
	if err != nil {
		return BankQuestion{}, fmt.Errorf("assess: encode accepted: %w", err)
	}

	var b BankQuestion
	err = tx.QueryRow(ctx,
		`INSERT INTO bank_questions (tenant_id, type, prompt, points, explanation, case_sensitive, accepted, category)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, type, prompt, points, category`,
		tenantID, n.Type, n.Prompt, n.Points, n.Explanation, n.CaseSensitive, accepted, category).
		Scan(&b.ID, &b.Type, &b.Prompt, &b.Points, &b.Category)
	if err != nil {
		return BankQuestion{}, fmt.Errorf("assess: save to bank: %w", err)
	}

	if len(n.Options) == 0 {
		return b, nil
	}
	contents := make([]string, len(n.Options))
	correct := make([]bool, len(n.Options))
	matches := make([]string, len(n.Options))
	for i, o := range n.Options {
		contents[i], correct[i], matches[i] = o.Content, o.IsCorrect, o.MatchContent
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO bank_question_options (tenant_id, bank_question_id, content, is_correct, match_content, position)
		 SELECT $1, $2, v.content, v.is_correct, v.match_content, v.rank - 1
		 FROM unnest($3::text[], $4::boolean[], $5::text[]) WITH ORDINALITY AS v(content, is_correct, match_content, rank)`,
		tenantID, b.ID, contents, correct, matches)
	if err != nil {
		return BankQuestion{}, fmt.Errorf("assess: save bank options: %w", err)
	}
	return b, nil
}

const listBankSQL = `
	SELECT id, type, prompt, points, category, created_at
	FROM bank_questions
	WHERE tenant_id = $1 AND ($2 = '' OR category = $2)
	  AND ($3::timestamptz IS NULL OR created_at < $3 OR (created_at = $3 AND id > $4))
	ORDER BY created_at DESC, id
	LIMIT $5`

// ListBankQuestions returns one keyset page of the bank, newest first.
func (r *PostgresRepository) ListBankQuestions(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, category string, before *bankCursor, limit int) ([]bankRow, error) {
	var (
		ts any
		id any
	)
	if before != nil {
		ts = before.CreatedAt
		id = before.ID
	}
	rows, err := tx.Query(ctx, listBankSQL, tenantID, category, ts, id, limit)
	if err != nil {
		return nil, fmt.Errorf("assess: list bank: %w", err)
	}
	defer rows.Close()

	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (bankRow, error) {
		var b bankRow
		err := row.Scan(&b.ID, &b.Type, &b.Prompt, &b.Points, &b.Category, &b.CreatedAt)
		return b, err
	})
	if err != nil {
		return nil, fmt.Errorf("assess: scan bank: %w", err)
	}
	return out, nil
}

// BankCategories lists the distinct non-empty categories in use.
func (r *PostgresRepository) BankCategories(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT category FROM bank_questions WHERE tenant_id = $1 AND category <> '' ORDER BY category`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("assess: bank categories: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var c string
		err := row.Scan(&c)
		return c, err
	})
}

// DeleteBankQuestion removes a bank question, returning whether one was there.
func (r *PostgresRepository) DeleteBankQuestion(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM bank_questions WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return false, fmt.Errorf("assess: delete bank question: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
