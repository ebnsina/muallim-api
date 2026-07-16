package httpapi

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/ebnsina/muallim-api/internal/payroll"
)

func TestPayrollSentinelsMapToADeliberateStatus(t *testing.T) {
	t.Parallel()

	tests := map[error]int{
		payroll.ErrNotFound:         http.StatusNotFound,
		payroll.ErrNotDraft:         http.StatusConflict,
		payroll.ErrNoSalary:         http.StatusUnprocessableEntity,
		payroll.ErrInvalidPage:      http.StatusUnprocessableEntity,
		payroll.ErrInvalidStructure: http.StatusUnprocessableEntity,
		payroll.ErrInvalidPayslip:   http.StatusUnprocessableEntity,
	}

	for err, want := range tests {
		if got := statusOf(payrollError(err)); got != want {
			t.Errorf("%v mapped to %d, want %d", err, got, want)
		}
		wrapped := fmt.Errorf("payroll: doing a thing: %w", err)
		if got := statusOf(payrollError(wrapped)); got != want {
			t.Errorf("wrapped %v mapped to %d, want %d", err, got, want)
		}
	}
}

func TestAnUnmappedPayrollErrorIsFiveHundred(t *testing.T) {
	t.Parallel()

	if got := statusOf(payrollError(fmt.Errorf("payroll: something new"))); got != http.StatusInternalServerError {
		t.Errorf("an unmapped payroll error mapped to %d, want 500", got)
	}
}
