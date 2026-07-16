package leads

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer.
//
// Every other domain's repository takes a tenant id, because every other domain's
// rows belong to a workspace. These do not exist yet — the person asking has no
// workspace, which is why they are asking — so this one takes a transaction and
// nothing else, and the service opens it with WithoutTenant.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, r DemoRequest) (DemoRequest, error)
}

// Clock is time, injected so a test can hold it still.
type Clock func() time.Time

// Service records demo requests.
type Service struct {
	db    *database.DB
	repo  Repository
	now   Clock
	newID func() uuid.UUID
}

// NewService wires the service. `now` and `newID` may be nil, and default.
func NewService(db *database.DB, repo Repository, now Clock) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, repo: repo, now: now, newID: uuid.New}
}

// Request records somebody asking for a demo.
//
// There is no deduplication and no "you have already asked" answer, deliberately.
// This endpoint is unauthenticated and anybody may post to it, so any response
// that varied by what we already hold would answer the question "is this address
// in your database?" for a stranger with a list. Every well-formed request is
// accepted identically; the duplicates are a human's problem to skim, and a
// cheaper one than the leak.
func (s *Service) Request(ctx context.Context, n NewDemoRequest) (DemoRequest, error) {
	if err := n.validate(); err != nil {
		return DemoRequest{}, err
	}

	now := s.now().UTC()
	row := DemoRequest{
		ID:        s.newID(),
		Intent:    n.Intent,
		Name:      n.Name,
		Email:     n.Email,
		Phone:     n.Phone,
		AgreedAt:  now,
		CreatedAt: now,
	}

	var saved DemoRequest
	err := s.db.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		saved, err = s.repo.Create(ctx, tx, row)
		return err
	})
	if err != nil {
		return DemoRequest{}, err
	}
	return saved, nil
}
