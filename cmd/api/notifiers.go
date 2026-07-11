package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/forum"
	"github.com/ebnsina/lms-api/internal/learn"
	"github.com/ebnsina/lms-api/internal/notify"
)

// learnNotifier adapts the notify service to the Notifier interface learn
// declares. Like the audit adapters, it lives in cmd/ because wiring one domain
// to another — without either importing the other — is what cmd/ is for. It
// forwards on the caller's transaction, so an answer and its notification commit
// together.
type learnNotifier struct{ svc *notify.Service }

var _ learn.Notifier = learnNotifier{}

func (n learnNotifier) Notify(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, m learn.Notification) error {
	return n.svc.Record(ctx, tx, tenantID, notify.Notification{
		UserID: m.UserID,
		Kind:   m.Kind,
		Title:  m.Title,
		Body:   m.Body,
		Link:   m.Link,
	})
}

// forumNotifier adapts the notify service to the Notifier interface forum
// declares, so a reply and its "new reply" notification commit together.
type forumNotifier struct{ svc *notify.Service }

var _ forum.Notifier = forumNotifier{}

func (n forumNotifier) Notify(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, m forum.Notification) error {
	return n.svc.Record(ctx, tx, tenantID, notify.Notification{
		UserID: m.UserID,
		Kind:   m.Kind,
		Title:  m.Title,
		Body:   m.Body,
		Link:   m.Link,
	})
}
