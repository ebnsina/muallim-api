package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/payroll"
)

// SalaryStructureView is what a school pays one staff member. Amounts are minor
// units (BDT poisha by default); the client formats them with the currency.
type SalaryStructureView struct {
	ID               string `json:"id" format:"uuid"`
	StaffID          string `json:"staff_id" format:"uuid"`
	BasicAmount      int64  `json:"basic_amount"`
	AllowancesAmount int64  `json:"allowances_amount"`
	DeductionsAmount int64  `json:"deductions_amount"`
	Currency         string `json:"currency"`
	EffectiveFrom    string `json:"effective_from,omitempty" format:"date"`
}

// PayslipView is one period's pay for one staff member.
type PayslipView struct {
	ID               string `json:"id" format:"uuid"`
	StaffID          string `json:"staff_id" format:"uuid"`
	Period           string `json:"period"`
	GrossAmount      int64  `json:"gross_amount"`
	DeductionsAmount int64  `json:"deductions_amount"`
	NetAmount        int64  `json:"net_amount"`
	Currency         string `json:"currency"`
	Status           string `json:"status" enum:"draft,paid"`
	GeneratedAt      string `json:"generated_at,omitempty" format:"date-time"`
	PaidAt           string `json:"paid_at,omitempty" format:"date-time"`
	Method           string `json:"method,omitempty"`
}

func salaryStructureView(s payroll.SalaryStructure) SalaryStructureView {
	v := SalaryStructureView{
		ID: s.ID.String(), StaffID: s.StaffID.String(), BasicAmount: s.BasicAmount,
		AllowancesAmount: s.AllowancesAmount, DeductionsAmount: s.DeductionsAmount, Currency: s.Currency,
	}
	if s.EffectiveFrom != nil {
		v.EffectiveFrom = s.EffectiveFrom.Format(dateLayout)
	}
	return v
}

func payslipView(p payroll.Payslip) PayslipView {
	v := PayslipView{
		ID: p.ID.String(), StaffID: p.StaffID.String(), Period: p.Period,
		GrossAmount: p.GrossAmount, DeductionsAmount: p.DeductionsAmount, NetAmount: p.NetAmount,
		Currency: p.Currency, Status: p.Status, Method: p.Method,
	}
	if !p.GeneratedAt.IsZero() {
		v.GeneratedAt = p.GeneratedAt.Format(time.RFC3339)
	}
	if p.PaidAt != nil {
		v.PaidAt = p.PaidAt.Format(time.RFC3339)
	}
	return v
}

func registerPayroll(api huma.API, svc *payroll.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "get-staff-salary",
		Method:      http.MethodGet,
		Path:        "/v1/payroll/salary/{staff_id}",
		Summary:     "A staff member's salary structure",
		Tags:        []string{"Payroll"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		StaffID string `path:"staff_id" format:"uuid"`
	}) (*struct {
		Body struct {
			Salary SalaryStructureView `json:"salary"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		staffID, err := parseUUID(in.StaffID, "staff")
		if err != nil {
			return nil, err
		}
		sal, err := svc.GetSalary(ctx, p.TenantID, staffID)
		if err != nil {
			return nil, payrollError(err)
		}
		out := &struct {
			Body struct {
				Salary SalaryStructureView `json:"salary"`
			}
		}{}
		out.Body.Salary = salaryStructureView(sal)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-staff-salary",
		Method:      http.MethodPut,
		Path:        "/v1/payroll/salary/{staff_id}",
		Summary:     "Set a staff member's salary structure",
		Description: "Idempotent: setting it again replaces the current structure in place.",
		Tags:        []string{"Payroll"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		StaffID string `path:"staff_id" format:"uuid"`
		Body    struct {
			BasicAmount      int64  `json:"basic_amount" minimum:"0"`
			AllowancesAmount int64  `json:"allowances_amount,omitempty" minimum:"0"`
			DeductionsAmount int64  `json:"deductions_amount,omitempty" minimum:"0"`
			Currency         string `json:"currency,omitempty" minLength:"3" maxLength:"3"`
			EffectiveFrom    string `json:"effective_from,omitempty" format:"date"`
		}
	}) (*struct {
		Body struct {
			Salary SalaryStructureView `json:"salary"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		staffID, err := parseUUID(in.StaffID, "staff")
		if err != nil {
			return nil, err
		}
		from, err := optionalDate(in.Body.EffectiveFrom)
		if err != nil {
			return nil, err
		}
		sal, err := svc.SetSalary(ctx, p.TenantID, payroll.NewSalary{
			StaffID: staffID, BasicAmount: in.Body.BasicAmount, AllowancesAmount: in.Body.AllowancesAmount,
			DeductionsAmount: in.Body.DeductionsAmount, Currency: in.Body.Currency, EffectiveFrom: from,
		}, payroll.Author{UserID: p.UserID})
		if err != nil {
			return nil, payrollError(err)
		}
		out := &struct {
			Body struct {
				Salary SalaryStructureView `json:"salary"`
			}
		}{}
		out.Body.Salary = salaryStructureView(sal)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-payslips",
		Method:      http.MethodGet,
		Path:        "/v1/payroll/payslips",
		Summary:     "Payslips, newest first",
		Description: "Keyset-paginated. Filter by staff_id, period and/or status.",
		Tags:        []string{"Payroll"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		StaffID string `query:"staff_id" format:"uuid"`
		Period  string `query:"period" maxLength:"20"`
		Status  string `query:"status" enum:"draft,paid"`
		Limit   int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor  string `query:"cursor"`
	}) (*struct {
		Body struct {
			Payslips   []PayslipView `json:"payslips"`
			NextCursor string        `json:"next_cursor,omitempty"`
			HasMore    bool          `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		staff, err := optionalUUIDPtr(in.StaffID, "staff")
		if err != nil {
			return nil, err
		}
		page, err := svc.ListPayslips(ctx, p.TenantID, payroll.PayslipFilter{StaffID: staff, Period: in.Period, Status: in.Status},
			payroll.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, payrollError(err)
		}
		out := &struct {
			Body struct {
				Payslips   []PayslipView `json:"payslips"`
				NextCursor string        `json:"next_cursor,omitempty"`
				HasMore    bool          `json:"has_more"`
			}
		}{}
		out.Body.Payslips = make([]PayslipView, 0, len(page.Payslips))
		for _, ps := range page.Payslips {
			out.Body.Payslips = append(out.Body.Payslips, payslipView(ps))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "generate-payslips",
		Method:      http.MethodPost,
		Path:        "/v1/payroll/payslips",
		Summary:     "Generate payslips for a period",
		Description: "Idempotent per period: re-running a month pays nobody twice. " +
			"Name a staff_id, or leave it empty to run the whole workspace.",
		Tags:     []string{"Payroll"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Period  string `json:"period" minLength:"1" maxLength:"20"`
			StaffID string `json:"staff_id,omitempty" format:"uuid"`
		}
	}) (*struct {
		Body struct {
			Generated int `json:"generated"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		staff, err := optionalUUIDPtr(in.Body.StaffID, "staff")
		if err != nil {
			return nil, err
		}
		generated, err := svc.GeneratePayslips(ctx, p.TenantID, payroll.Batch{
			Period: in.Body.Period, StaffID: staff,
		}, payroll.Author{UserID: p.UserID})
		if err != nil {
			return nil, payrollError(err)
		}
		out := &struct {
			Body struct {
				Generated int `json:"generated"`
			}
		}{}
		out.Body.Generated = generated
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "pay-payslip",
		Method:      http.MethodPost,
		Path:        "/v1/payroll/payslips/{id}/pay",
		Summary:     "Record that a draft payslip was paid",
		Description: "The draft guard makes a double submission harmless.",
		Tags:        []string{"Payroll"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Method string `json:"method,omitempty" maxLength:"40"`
		}
	}) (*struct {
		Body struct {
			Payslip PayslipView `json:"payslip"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "payslip")
		if err != nil {
			return nil, err
		}
		ps, err := svc.MarkPaid(ctx, p.TenantID, id, in.Body.Method, payroll.Author{UserID: p.UserID})
		if err != nil {
			return nil, payrollError(err)
		}
		out := &struct {
			Body struct {
				Payslip PayslipView `json:"payslip"`
			}
		}{}
		out.Body.Payslip = payslipView(ps)
		return out, nil
	})
}

// payrollError maps the payroll package's sentinels onto status codes.
func payrollError(err error) error {
	switch {
	case errors.Is(err, payroll.ErrNotFound):
		return huma.Error404NotFound("We couldn't find that payslip or salary structure.")
	case errors.Is(err, payroll.ErrNotDraft):
		return huma.Error409Conflict("Only a draft payslip can be paid.")
	case errors.Is(err, payroll.ErrNoSalary):
		return huma.Error422UnprocessableEntity("That staff member has no salary structure to pay.")
	case errors.Is(err, payroll.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")
	case errors.Is(err, payroll.ErrInvalidStructure),
		errors.Is(err, payroll.ErrInvalidPayslip):
		return huma.Error422UnprocessableEntity("Check the salary and payslip details and try again.")
	default:
		return err
	}
}
