package assign

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// KindGrade names the "your assignment was graded" notification.
const KindGrade = "grade"

// Notification is a message this package asks to send — the learner whose
// submission an instructor has just marked. Restated here rather than imported;
// cmd wires an adapter over the notify service. Empty UserID means nobody to tell.
type Notification struct {
	UserID uuid.UUID
	Kind   string
	Title  string
	Body   string
	Link   string
}

// Notifier delivers a Notification inside the caller's transaction, so the mark
// and the notice commit together. Declared here, by the consumer.
type Notifier interface {
	Notify(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n Notification) error
}
