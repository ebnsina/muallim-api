package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/assess"
	"github.com/ebnsina/muallim-api/internal/assign"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/automation"
	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/forum"
	"github.com/ebnsina/muallim-api/internal/gamify"
	"github.com/ebnsina/muallim-api/internal/learn"
	"github.com/ebnsina/muallim-api/internal/notify"
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

// assessNotifier and assignNotifier adapt the notify service to the Notifier
// interfaces the grading domains declare. A marked essay or assignment tells the
// learner, in the transaction that recorded the grade.
type assessNotifier struct{ svc *notify.Service }

var _ assess.Notifier = assessNotifier{}

func (n assessNotifier) Notify(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, m assess.Notification) error {
	return n.svc.Record(ctx, tx, tenantID, notify.Notification{
		UserID: m.UserID, Kind: m.Kind, Title: m.Title, Body: m.Body, Link: m.Link,
	})
}

type assignNotifier struct{ svc *notify.Service }

var _ assign.Notifier = assignNotifier{}

func (n assignNotifier) Notify(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, m assign.Notification) error {
	return n.svc.Record(ctx, tx, tenantID, notify.Notification{
		UserID: m.UserID, Kind: m.Kind, Title: m.Title, Body: m.Body, Link: m.Link,
	})
}

// gamifyRewards adapts the gamify service to the Rewards interface enroll
// declares. Finishing a lesson or a course earns points and badges, in the
// transaction that recorded the completion; neither domain imports the other.
type gamifyRewards struct{ svc *gamify.Service }

var _ enroll.Rewards = gamifyRewards{}

func (r gamifyRewards) LessonCompleted(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) error {
	return r.svc.AwardLesson(ctx, tx, tenantID, userID, lessonID)
}

func (r gamifyRewards) CourseCompleted(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) error {
	return r.svc.AwardCourse(ctx, tx, tenantID, userID, courseID)
}

/*
automationAnnouncer adapts the automation service to the Announcer interface
enroll declares, and resolves what a rule's templates need: who enrolled, and in
what. Both lookups run in the caller's transaction, so the values are the ones
that were true when the thing happened.

Nothing here may fail an enrolment. A workspace's welcome note is worth less than
the enrolment it welcomes, so a lookup that fails or a queue that will not take
the message is logged and swallowed — the learner is in the course either way,
and the log is where a workspace finds out its mail is not going out.
*/
type automationAnnouncer struct {
	svc     *automation.Service
	users   *auth.PostgresRepository
	courses *catalog.PostgresRepository
	log     *slog.Logger
}

var _ enroll.Announcer = automationAnnouncer{}

func (a automationAnnouncer) Enrolled(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) error {
	return a.fire(ctx, tx, tenantID, automation.EventEnrolled, userID, courseID)
}

func (a automationAnnouncer) CourseCompleted(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) error {
	return a.fire(ctx, tx, tenantID, automation.EventCompleted, userID, courseID)
}

func (a automationAnnouncer) fire(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, event string, userID, courseID uuid.UUID) error {
	user, _, err := a.users.UserByID(ctx, tx, tenantID, userID)
	if err != nil {
		a.log.ErrorContext(ctx, "automation: cannot name the learner; no message sent",
			slog.String("event", event), slog.Any("error", err))
		return nil
	}
	course, err := a.courses.CourseByID(ctx, tx, tenantID, courseID)
	if err != nil {
		a.log.ErrorContext(ctx, "automation: cannot name the course; no message sent",
			slog.String("event", event), slog.Any("error", err))
		return nil
	}

	if err := a.svc.Fire(ctx, tx, tenantID, event, automation.Message{
		To: user.Email,
		Vars: map[string]string{
			"learner_name": user.Name,
			"course_title": course.Title,
		},
	}); err != nil {
		a.log.ErrorContext(ctx, "automation: message not queued",
			slog.String("event", event), slog.Any("error", err))
	}
	return nil
}
