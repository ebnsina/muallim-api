package main

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/commerce"
	"github.com/ebnsina/muallim-api/internal/enroll"
)

/*
The seam between money and access, in both directions.

`commerce` declares Enrolments and `enroll` satisfies it: a paid order enrols its
buyer, in the transaction that marked the order paid. `enroll` declares Prices and
`commerce` satisfies it: a course with a price is bought, not self-enrolled. Neither
package imports the other, and this file — which is allowed to know about both — is
the only place they meet.
*/

// purchases lets a paid order enrol its buyer.
type purchases struct{ enrol *enroll.Service }

func (p purchases) GrantPurchase(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return p.enrol.GrantInTx(ctx, tx, tenantID, courseID, userID, enroll.SourcePurchase)
}

func (p purchases) CancelPurchase(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return p.enrol.CancelInTx(ctx, tx, tenantID, courseID, userID)
}

// coursePrices lets enrolment ask whether a course costs money, in the transaction
// that would otherwise have given it away.
type coursePrices struct{ repo *commerce.PostgresRepository }

func (c coursePrices) Priced(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (bool, error) {
	_, err := c.repo.Price(ctx, tx, tenantID, courseID)
	if err != nil {
		// No price is the free course, and that is an answer, not a failure.
		if errors.Is(err, commerce.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
