package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/admissions"
	"github.com/ebnsina/muallim-api/internal/assess"
	"github.com/ebnsina/muallim-api/internal/assign"
	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/calendar"
	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/certdesign"
	"github.com/ebnsina/muallim-api/internal/certify"
	"github.com/ebnsina/muallim-api/internal/comms"
	"github.com/ebnsina/muallim-api/internal/coursebuild"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/exams"
	"github.com/ebnsina/muallim-api/internal/fees"
	"github.com/ebnsina/muallim-api/internal/hostel"
	"github.com/ebnsina/muallim-api/internal/idcard"
	"github.com/ebnsina/muallim-api/internal/ledger"
	"github.com/ebnsina/muallim-api/internal/library"
	"github.com/ebnsina/muallim-api/internal/notices"
	"github.com/ebnsina/muallim-api/internal/payroll"
	"github.com/ebnsina/muallim-api/internal/staff"
	"github.com/ebnsina/muallim-api/internal/transport"
)

// The dependency rule forbids a domain package from importing a sibling, so auth
// and catalog each declare the audit interface they need, in their own types.
// These adapters are the seam, and they live in cmd/ because wiring is what cmd/
// is for.
//
// The cost is two five-line translations. The benefit is that catalog can be
// extracted, tested, or replaced without dragging auth or audit along with it.

type authAuditor struct{ recorder *audit.Recorder }

var _ auth.AuditRecorder = authAuditor{}

func (a authAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e auth.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		IP:         e.IP,
		UserAgent:  e.UserAgent,
		Metadata:   e.Metadata,
	})
}

type academicsAuditor struct{ recorder *audit.Recorder }

var _ academics.AuditRecorder = academicsAuditor{}

func (a academicsAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e academics.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type examsAuditor struct{ recorder *audit.Recorder }

var _ exams.AuditRecorder = examsAuditor{}

func (a examsAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e exams.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type feesAuditor struct{ recorder *audit.Recorder }

var _ fees.AuditRecorder = feesAuditor{}

func (a feesAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e fees.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type noticesAuditor struct{ recorder *audit.Recorder }

var _ notices.AuditRecorder = noticesAuditor{}

func (a noticesAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e notices.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

// noticeBroadcaster satisfies notices.Broadcaster by enqueuing each guardian's
// copy through the outbox, in the posting transaction — email as a rendered
// message, sms as a text. Both are the same enqueuer the rest of the system uses.
type noticeBroadcaster struct{ outbox *comms.Enqueuer }

var _ notices.Broadcaster = noticeBroadcaster{}

func (b noticeBroadcaster) SendEmail(ctx context.Context, tx pgx.Tx, to, name, subject, body string) error {
	text := body
	if name != "" {
		text = "Dear " + name + ",\n\n" + body
	}
	return b.outbox.SendRendered(ctx, tx, to, subject, text)
}

func (b noticeBroadcaster) SendSMS(ctx context.Context, tx pgx.Tx, to, text string) error {
	return b.outbox.SendSMS(ctx, tx, to, text)
}

type staffAuditor struct{ recorder *audit.Recorder }

var _ staff.AuditRecorder = staffAuditor{}

func (a staffAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e staff.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type libraryAuditor struct{ recorder *audit.Recorder }

var _ library.AuditRecorder = libraryAuditor{}

func (a libraryAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e library.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type transportAuditor struct{ recorder *audit.Recorder }

var _ transport.AuditRecorder = transportAuditor{}

func (a transportAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e transport.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type hostelAuditor struct{ recorder *audit.Recorder }

var _ hostel.AuditRecorder = hostelAuditor{}

func (a hostelAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e hostel.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type payrollAuditor struct{ recorder *audit.Recorder }

var _ payroll.AuditRecorder = payrollAuditor{}

func (a payrollAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e payroll.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type ledgerAuditor struct{ recorder *audit.Recorder }

var _ ledger.AuditRecorder = ledgerAuditor{}

func (a ledgerAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e ledger.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type idCardAuditor struct{ recorder *audit.Recorder }

var _ idcard.AuditRecorder = idCardAuditor{}

func (a idCardAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e idcard.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type admissionsAuditor struct{ recorder *audit.Recorder }

var _ admissions.AuditRecorder = admissionsAuditor{}

func (a admissionsAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e admissions.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type calendarAuditor struct{ recorder *audit.Recorder }

var _ calendar.AuditRecorder = calendarAuditor{}

func (a calendarAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e calendar.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type certDesignAuditor struct{ recorder *audit.Recorder }

var _ certdesign.AuditRecorder = certDesignAuditor{}

func (a certDesignAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e certdesign.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type courseBuildAuditor struct{ recorder *audit.Recorder }

var _ coursebuild.AuditRecorder = courseBuildAuditor{}

func (a courseBuildAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e coursebuild.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Metadata:   e.Metadata,
	})
}

type catalogAuditor struct{ recorder *audit.Recorder }

var _ catalog.AuditRecorder = catalogAuditor{}

func (a catalogAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e catalog.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		IP:         e.IP,
		UserAgent:  e.UserAgent,
		Metadata:   e.Metadata,
	})
}

type enrolAuditor struct{ recorder *audit.Recorder }

var _ enroll.AuditRecorder = enrolAuditor{}

func (a enrolAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e enroll.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID:    e.ActorID,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		IP:         e.IP,
		UserAgent:  e.UserAgent,
		Metadata:   e.Metadata,
	})
}

// assessAuditor adapts the recorder to the assess package's interface.
type assessAuditor struct{ recorder *audit.Recorder }

func (a assessAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e assess.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

// assignAuditor adapts the recorder to the assign package's interface.
type assignAuditor struct{ recorder *audit.Recorder }

func (a assignAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e assign.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

// certifyAuditor adapts the recorder to the certify package's interface.
type certifyAuditor struct{ recorder *audit.Recorder }

func (a certifyAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e certify.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		Metadata: e.Metadata,
	})
}
