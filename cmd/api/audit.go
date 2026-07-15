package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/assess"
	"github.com/ebnsina/muallim-api/internal/assign"
	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/certify"
	"github.com/ebnsina/muallim-api/internal/enroll"
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
