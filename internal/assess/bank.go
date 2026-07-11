package assess

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// trimCategory tidies a category name to its stored form.
func trimCategory(c string) string {
	c = strings.TrimSpace(c)
	if len(c) > MaxCategoryLength {
		c = c[:MaxCategoryLength]
	}
	return c
}

// MaxCategoryLength bounds a bank category name.
const MaxCategoryLength = 100

// Bank list bounds.
const (
	DefaultPageSize = 20
	MaxPageSize     = 50
)

// BankQuestion is a reusable question in the content bank, as a list shows it.
type BankQuestion struct {
	ID       uuid.UUID
	Type     string
	Prompt   string
	Points   int
	Category string
}

// SaveQuestionToBank copies an existing quiz question into the bank, so an author
// can reuse it. Category is trimmed; empty is allowed. Requires course:write,
// checked by the transport layer.
func (s *Service) SaveQuestionToBank(ctx context.Context, tenantID, questionID uuid.UUID, category string, author Author) (BankQuestion, error) {
	category = trimCategory(category)

	var saved BankQuestion
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		n, err := s.repo.QuestionAsNew(ctx, tx, tenantID, questionID)
		if err != nil {
			return err
		}
		saved, err = s.repo.SaveToBank(ctx, tx, tenantID, category, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionBankQuestionSaved,
			TargetType: "bank_question", TargetID: saved.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
		})
	})
	return saved, err
}

// BankQuestions lists the bank, newest first, optionally filtered by category.
func (s *Service) BankQuestions(ctx context.Context, tenantID uuid.UUID, category, token string, limit int) (BankPage, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}

	var before *bankCursor
	if token != "" {
		c, err := decodeBankCursor(token)
		if err != nil {
			return BankPage{}, err
		}
		before = &c
	}

	var page BankPage
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.ListBankQuestions(ctx, tx, tenantID, trimCategory(category), before, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1]
			page.NextCursor = bankCursor{CreatedAt: last.CreatedAt, ID: last.ID}.encode()
			rows = rows[:limit]
		}
		page.Questions = make([]BankQuestion, len(rows))
		for i, r := range rows {
			page.Questions[i] = r.BankQuestion
		}
		return nil
	})
	return page, err
}

// BankCategories lists the distinct categories in use, for a filter.
func (s *Service) BankCategories(ctx context.Context, tenantID uuid.UUID) ([]string, error) {
	var cats []string
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cats, err = s.repo.BankCategories(ctx, tx, tenantID)
		return err
	})
	return cats, err
}

// DeleteBankQuestion removes a bank question. Absent reads as ErrNotFound.
func (s *Service) DeleteBankQuestion(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		removed, err := s.repo.DeleteBankQuestion(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if !removed {
			return ErrNotFound
		}
		return nil
	})
}

// AddBankQuestionToQuiz copies a bank question into a lesson's quiz. It is the
// add-question flow with the question coming from the bank rather than a form, so
// the gradebook is synced and the event audited just the same.
func (s *Service) AddBankQuestionToQuiz(ctx context.Context, tenantID, lessonID, bankQuestionID uuid.UUID, author Author) (Question, error) {
	var created Question
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		n, err := s.repo.BankQuestionAsNew(ctx, tx, tenantID, bankQuestionID)
		if err != nil {
			return err
		}
		if err := n.validate(); err != nil {
			return err
		}

		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}
		if created, err = s.repo.CreateQuestion(ctx, tx, tenantID, quiz.ID, n); err != nil {
			return err
		}
		if err := s.syncItem(ctx, tx, tenantID, quiz.ID); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionQuestionCreated,
			TargetType: "question", TargetID: created.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"quiz_id": quiz.ID.String(), "from_bank": bankQuestionID.String()},
		})
	})
	return created, err
}

// BankPage is one keyset page of the bank.
type BankPage struct {
	Questions  []BankQuestion
	NextCursor string
}

// bankRow is a bank question plus its sort key, for keyset paging.
type bankRow struct {
	BankQuestion
	CreatedAt time.Time
}
