package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/commerce"
	"github.com/ebnsina/muallim-api/internal/enroll"
)

// purchases lets a refunded order withdraw its buyer's enrolment. The confirm-refund
// job never reaches this, but the service takes an Enrolments and this is the honest
// one to give it — the same seam cmd/api wires.
type purchases struct{ enrol *enroll.Service }

func (p purchases) GrantPurchase(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return p.enrol.GrantInTx(ctx, tx, tenantID, courseID, userID, enroll.SourcePurchase)
}

func (p purchases) CancelPurchase(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return p.enrol.CancelInTx(ctx, tx, tenantID, courseID, userID)
}

// commerceAuditor lets commerce write to the audit trail without importing it.
type commerceAuditor struct{ recorder *audit.Recorder }

func (a commerceAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e commerce.AuditEntry) error {
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
