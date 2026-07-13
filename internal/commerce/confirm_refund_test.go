package commerce_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/audit"
	"github.com/ebnsina/muallim-api/internal/commerce"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/platform/database"
	"github.com/ebnsina/muallim-api/internal/platform/secret"
)

/*
An asynchronous refund is confirmed, and a cancelled one is not mistaken for done.

SSLCommerz accepts a refund as `processing` and only later says whether the money
moved. Before this, the order was marked refunded and nothing ever checked — a
gateway that cancelled the refund after accepting it left a learner without the
course and without the money, silently. This proves the three outcomes land where
they should, and that the failure is written down.
*/
func TestConfirmRefundRecordsWhatBecameOfTheMoney(t *testing.T) {
	t.Parallel()

	for status, want := range map[string]commerce.RefundState{
		"refunded":   commerce.RefundDone,
		"processing": commerce.RefundPending,
		"cancelled":  commerce.RefundFailed,
	} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()

			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"` + status + `","GatewayPageURL":"x"}`))
			}))
			defer gateway.Close()

			db := testDB(t)
			tenantID := seedTenant(t, db)
			seedCourse(t, db, tenantID)
			learner := seedUser(t, db, tenantID)

			sealer, err := secret.NewSealer(strings.Repeat("cd", 32))
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			ssl, err := commerce.NewSSLCommerz(gateway.URL, "https://api.test/v1/payments/sslcommerz/ipn", gateway.Client())
			if err != nil {
				t.Fatalf("driver: %v", err)
			}

			enrolments := enroll.NewService(db, enroll.NewPostgresRepository(), enrolAuditor{audit.NewRecorder()}, nil, nil)
			svc := commerce.NewService(db, commerce.NewPostgresRepository(),
				commerceAuditor{audit.NewRecorder()}, purchases{enrolments}, sealer, ssl)

			// The workspace's own store credentials, and a refunded order to chase.
			actor := commerce.Actor{UserID: seedUser(t, db, tenantID)}
			if err := svc.SetCredentials(t.Context(), tenantID, commerce.GatewaySSLCommerz,
				commerce.Credentials{PublicID: "store", Secret: "password"}, actor); err != nil {
				t.Fatalf("SetCredentials: %v", err)
			}
			orderID := seedRefundedOrder(t, db, tenantID, learner)

			state, err := svc.ConfirmRefund(t.Context(), tenantID, orderID)
			if err != nil {
				t.Fatalf("ConfirmRefund: %v", err)
			}
			if state != want {
				t.Errorf("status %q → %q, want %q", status, state, want)
			}

			// A terminal outcome is audited; a pending one leaves no trail yet.
			action := auditActionFor(t, db, tenantID, orderID)
			switch want {
			case commerce.RefundDone:
				if action != commerce.ActionRefundConfirmed {
					t.Errorf("done was audited as %q, want %q", action, commerce.ActionRefundConfirmed)
				}
			case commerce.RefundFailed:
				if action != commerce.ActionRefundFailed {
					t.Errorf("a cancelled refund was audited as %q, want %q — the failure must be recorded", action, commerce.ActionRefundFailed)
				}
			case commerce.RefundPending:
				if action != "" {
					t.Errorf("a still-pending refund was audited as %q, want nothing yet", action)
				}
			}
		})
	}
}

// seedRefundedOrder writes a refunded SSLCommerz order with a refund id to chase.
func seedRefundedOrder(t *testing.T, db *database.DB, tenantID, learner uuid.UUID) uuid.UUID {
	t.Helper()

	var orderID uuid.UUID
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var courseID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM courses WHERE tenant_id = $1 LIMIT 1`, tenantID).Scan(&courseID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO orders (tenant_id, course_id, user_id, amount_minor, currency, status, gateway, external_id, payment_external_id, refund_external_id)
			 VALUES ($1, $2, $3, 120000, 'BDT', 'refunded', 'sslcommerz', $4, 'bank_1', 'ref_1')
			 RETURNING id`,
			tenantID, courseID, learner, uuid.NewString()).Scan(&orderID)
	})
	if err != nil {
		t.Fatalf("seed refunded order: %v", err)
	}
	return orderID
}

// auditActionFor returns the latest audit action against an order, or "".
func auditActionFor(t *testing.T, db *database.DB, tenantID, orderID uuid.UUID) string {
	t.Helper()

	var action string
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT action FROM audit_log
			 WHERE tenant_id = $1 AND target_id = $2
			   AND action IN ('order.refund_confirmed', 'order.refund_failed')
			 ORDER BY created_at DESC LIMIT 1`,
			tenantID, orderID.String()).Scan(&action)
	})
	if err != nil {
		return ""
	}
	return action
}
