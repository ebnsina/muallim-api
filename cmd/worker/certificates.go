package main

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/certify"
	"github.com/ebnsina/muallim-api/internal/enroll"
)

/*
The adapter that lets `enroll` issue a certificate.

`enroll` does not import `certify`, and `certify` does not import `enroll`. Each
declares what it needs; this file — which is allowed to know about both — is where
they meet. The same seam the gradebook and the lesson roll-up are reached through.

It takes the caller's transaction, so the completed enrolment and the certificate
commit together, or neither does.
*/
type certificates struct{ svc *certify.Service }

func (c certificates) IssueIfEarned(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return c.svc.IssueIfEarned(ctx, tx, tenantID, courseID, userID)
}

// A compile-time check that the adapter is the shape `enroll` asked for. Without
// it the mismatch shows up at the one line in main.go that wires them, which is
// further from the interface than the error deserves to be.
var _ enroll.Certificates = certificates{}

// certifyAuditor adapts the recorder to the certify package's interface.
type certifyAuditor struct{ recorder *audit.Recorder }

func (a certifyAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e certify.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		Metadata: e.Metadata,
	})
}
