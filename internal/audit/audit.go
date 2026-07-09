// Package audit records who did what, to which resource, when.
//
// FERPA and GDPR both require an audit trail, and it cannot be added
// retroactively: you cannot backfill history you never recorded. The table is
// append-only at the database level — its policies grant SELECT and INSERT and
// nothing else, so no UPDATE or DELETE reaches a row, even from application code
// that has been compromised.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Action names are declared by the domain package that emits them — auth owns
// "user.logged_in", catalog owns "course.created" — because no domain package
// may import this one. This package defines the shape of an entry and how it is
// stored, not the vocabulary of events.

// Entry is one auditable event.
type Entry struct {
	// ActorID is the user who acted. Nil for an anonymous or failed action, such
	// as a login attempt against an address that does not exist.
	ActorID *uuid.UUID

	Action     string
	TargetType string
	TargetID   string

	IP        netip.Addr
	UserAgent string

	// Metadata carries event-specific detail. It must never contain a password, a
	// token, or anything else that would turn the audit log into a breach.
	Metadata map[string]any
}

// Recorder appends entries to the audit log.
//
// Every method takes the transaction of the operation being audited. An audit
// record that commits separately from the thing it describes will eventually
// disagree with it.
type Recorder struct{}

// NewRecorder returns a Recorder.
func NewRecorder() *Recorder { return &Recorder{} }

const insertSQL = `
	INSERT INTO audit_log (tenant_id, actor_id, action, target_type, target_id, ip, user_agent, metadata)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// Record appends e within tx, so the audit entry and the change it describes
// commit together or not at all.
func (r *Recorder) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e Entry) error {
	metadata := e.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("audit: marshal metadata: %w", err)
	}

	var ip *string
	if e.IP.IsValid() {
		s := e.IP.String()
		ip = &s
	}

	if _, err := tx.Exec(ctx, insertSQL,
		tenantID, e.ActorID, e.Action, e.TargetType, e.TargetID, ip, e.UserAgent, raw,
	); err != nil {
		return fmt.Errorf("audit: record %s: %w", e.Action, err)
	}
	return nil
}
