package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/fees"
)

// FeeStructureView is a recurring charge a school defines. Amount is minor units
// (BDT poisha by default); the client formats it with the currency.
type FeeStructureView struct {
	ID           string `json:"id" format:"uuid"`
	Name         string `json:"name"`
	Amount       int64  `json:"amount"`
	Currency     string `json:"currency"`
	GradeLevelID string `json:"grade_level_id,omitempty" format:"uuid"`
	Recurrence   string `json:"recurrence" enum:"one_time,monthly,termly,annual"`
}

// InvoiceView is one charge raised against a student.
type InvoiceView struct {
	ID             string `json:"id" format:"uuid"`
	StudentID      string `json:"student_id" format:"uuid"`
	FeeStructureID string `json:"fee_structure_id,omitempty" format:"uuid"`
	Title          string `json:"title"`
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
	Period         string `json:"period,omitempty"`
	DueDate        string `json:"due_date,omitempty" format:"date"`
	Status         string `json:"status" enum:"unpaid,paid,waived,cancelled"`
	PaidAmount     int64  `json:"paid_amount"`
	PaidAt         string `json:"paid_at,omitempty" format:"date-time"`
	Method         string `json:"method,omitempty"`
	Note           string `json:"note,omitempty"`
}

// LedgerView is a student's invoices and outstanding balance per currency.
type LedgerView struct {
	Invoices    []InvoiceView    `json:"invoices"`
	Outstanding map[string]int64 `json:"outstanding"`
}

func feeStructureView(s fees.FeeStructure) FeeStructureView {
	return FeeStructureView{
		ID: s.ID.String(), Name: s.Name, Amount: s.Amount, Currency: s.Currency,
		GradeLevelID: uuidPtrString(s.GradeLevelID), Recurrence: s.Recurrence,
	}
}

func invoiceView(i fees.Invoice) InvoiceView {
	v := InvoiceView{
		ID: i.ID.String(), StudentID: i.StudentID.String(), FeeStructureID: uuidPtrString(i.FeeStructureID),
		Title: i.Title, Amount: i.Amount, Currency: i.Currency, Period: i.Period,
		Status: i.Status, PaidAmount: i.PaidAmount, Method: i.Method, Note: i.Note,
	}
	if i.DueDate != nil {
		v.DueDate = i.DueDate.Format(dateLayout)
	}
	if i.PaidAt != nil {
		v.PaidAt = i.PaidAt.Format(time.RFC3339)
	}
	return v
}

func registerFees(api huma.API, svc *fees.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-fee-structures",
		Method:      http.MethodGet,
		Path:        "/v1/fee-structures",
		Summary:     "The workspace's fee structures",
		Tags:        []string{"Fees"},
		Security:    admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Structures []FeeStructureView `json:"structures"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		list, err := svc.Structures(ctx, p.TenantID)
		if err != nil {
			return nil, feesError(err)
		}
		out := &struct {
			Body struct {
				Structures []FeeStructureView `json:"structures"`
			}
		}{}
		out.Body.Structures = make([]FeeStructureView, 0, len(list))
		for _, s := range list {
			out.Body.Structures = append(out.Body.Structures, feeStructureView(s))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-fee-structure",
		Method:        http.MethodPost,
		Path:          "/v1/fee-structures",
		Summary:       "Define a fee structure",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Fees"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name         string `json:"name" minLength:"1" maxLength:"160"`
			Amount       int64  `json:"amount" minimum:"0"`
			Currency     string `json:"currency,omitempty" minLength:"3" maxLength:"3"`
			GradeLevelID string `json:"grade_level_id,omitempty" format:"uuid"`
			Recurrence   string `json:"recurrence,omitempty" enum:"one_time,monthly,termly,annual"`
		}
	}) (*struct {
		Body struct {
			Structure FeeStructureView `json:"structure"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		grade, err := optionalUUIDPtr(in.Body.GradeLevelID, "grade level")
		if err != nil {
			return nil, err
		}
		fs, err := svc.CreateStructure(ctx, p.TenantID, fees.NewFeeStructure{
			Name: in.Body.Name, Amount: in.Body.Amount, Currency: in.Body.Currency,
			GradeLevelID: grade, Recurrence: in.Body.Recurrence,
		}, fees.Author{UserID: p.UserID})
		if err != nil {
			return nil, feesError(err)
		}
		out := &struct {
			Body struct {
				Structure FeeStructureView `json:"structure"`
			}
		}{}
		out.Body.Structure = feeStructureView(fs)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-fee-structure",
		Method:        http.MethodDelete,
		Path:          "/v1/fee-structures/{id}",
		Summary:       "Remove a fee structure",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Fees"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "fee structure")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteStructure(ctx, p.TenantID, id); err != nil {
			return nil, feesError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "issue-fees",
		Method:      http.MethodPost,
		Path:        "/v1/fee-structures/{id}/issue",
		Summary:     "Bill a fee to students or a class for a period",
		Description: "Idempotent per period: re-running a month bills nobody twice. " +
			"Name student_ids, or leave them empty to bill the structure's class.",
		Tags:     []string{"Fees"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Period       string   `json:"period,omitempty" maxLength:"20"`
			DueDate      string   `json:"due_date,omitempty" format:"date"`
			GradeLevelID string   `json:"grade_level_id,omitempty" format:"uuid"`
			StudentIDs   []string `json:"student_ids,omitempty" maxItems:"2000"`
		}
	}) (*struct {
		Body struct {
			Issued int `json:"issued"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		structureID, err := parseUUID(in.ID, "fee structure")
		if err != nil {
			return nil, err
		}
		grade, err := optionalUUIDPtr(in.Body.GradeLevelID, "grade level")
		if err != nil {
			return nil, err
		}
		due, err := optionalDate(in.Body.DueDate)
		if err != nil {
			return nil, err
		}
		studentIDs := make([]uuid.UUID, 0, len(in.Body.StudentIDs))
		for _, s := range in.Body.StudentIDs {
			id, err := parseUUID(s, "student")
			if err != nil {
				return nil, err
			}
			studentIDs = append(studentIDs, id)
		}
		issued, err := svc.IssueBatch(ctx, p.TenantID, fees.Batch{
			StructureID: structureID, Period: in.Body.Period, DueDate: due,
			GradeLevelID: grade, StudentIDs: studentIDs,
		}, fees.Author{UserID: p.UserID})
		if err != nil {
			return nil, feesError(err)
		}
		out := &struct {
			Body struct {
				Issued int `json:"issued"`
			}
		}{}
		out.Body.Issued = issued
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-fee-invoices",
		Method:      http.MethodGet,
		Path:        "/v1/fee-invoices",
		Summary:     "Fee invoices, newest first",
		Description: "Keyset-paginated. Filter by student_id and/or status.",
		Tags:        []string{"Fees"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		StudentID string `query:"student_id" format:"uuid"`
		Status    string `query:"status" enum:"unpaid,paid,waived,cancelled"`
		Limit     int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor    string `query:"cursor"`
	}) (*struct {
		Body struct {
			Invoices   []InvoiceView `json:"invoices"`
			NextCursor string        `json:"next_cursor,omitempty"`
			HasMore    bool          `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		student, err := optionalUUIDPtr(in.StudentID, "student")
		if err != nil {
			return nil, err
		}
		page, err := svc.Invoices(ctx, p.TenantID, fees.InvoiceFilter{StudentID: student, Status: in.Status},
			fees.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, feesError(err)
		}
		out := &struct {
			Body struct {
				Invoices   []InvoiceView `json:"invoices"`
				NextCursor string        `json:"next_cursor,omitempty"`
				HasMore    bool          `json:"has_more"`
			}
		}{}
		out.Body.Invoices = make([]InvoiceView, 0, len(page.Invoices))
		for _, i := range page.Invoices {
			out.Body.Invoices = append(out.Body.Invoices, invoiceView(i))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-fee-invoice",
		Method:        http.MethodPost,
		Path:          "/v1/fee-invoices",
		Summary:       "Raise an ad-hoc invoice against a student",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Fees"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			StudentID string `json:"student_id" format:"uuid"`
			Title     string `json:"title" minLength:"1" maxLength:"160"`
			Amount    int64  `json:"amount" minimum:"0"`
			Currency  string `json:"currency,omitempty" minLength:"3" maxLength:"3"`
			DueDate   string `json:"due_date,omitempty" format:"date"`
			Note      string `json:"note,omitempty" maxLength:"500"`
		}
	}) (*struct {
		Body struct {
			Invoice InvoiceView `json:"invoice"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.Body.StudentID, "student")
		if err != nil {
			return nil, err
		}
		due, err := optionalDate(in.Body.DueDate)
		if err != nil {
			return nil, err
		}
		inv, err := svc.IssueInvoice(ctx, p.TenantID, fees.NewInvoice{
			StudentID: studentID, Title: in.Body.Title, Amount: in.Body.Amount,
			Currency: in.Body.Currency, DueDate: due, Note: in.Body.Note,
		}, fees.Author{UserID: p.UserID})
		if err != nil {
			return nil, feesError(err)
		}
		out := &struct {
			Body struct {
				Invoice InvoiceView `json:"invoice"`
			}
		}{}
		out.Body.Invoice = invoiceView(inv)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "pay-fee-invoice",
		Method:      http.MethodPost,
		Path:        "/v1/fee-invoices/{id}/pay",
		Summary:     "Record a payment against an invoice",
		Description: "Manual reconciliation. The unpaid guard makes a double submission harmless.",
		Tags:        []string{"Fees"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Amount int64  `json:"amount" minimum:"1"`
			Method string `json:"method,omitempty" maxLength:"40"`
			Note   string `json:"note,omitempty" maxLength:"500"`
		}
	}) (*struct {
		Body struct {
			Invoice InvoiceView `json:"invoice"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "invoice")
		if err != nil {
			return nil, err
		}
		inv, err := svc.RecordPayment(ctx, p.TenantID, id, fees.Payment{
			Amount: in.Body.Amount, Method: in.Body.Method, Note: in.Body.Note,
		}, fees.Author{UserID: p.UserID})
		if err != nil {
			return nil, feesError(err)
		}
		out := &struct {
			Body struct {
				Invoice InvoiceView `json:"invoice"`
			}
		}{}
		out.Body.Invoice = invoiceView(inv)
		return out, nil
	})

	transition := func(op, path, summary string, act func(context.Context, uuid.UUID, uuid.UUID, fees.Author) (fees.Invoice, error)) {
		huma.Register(api, huma.Operation{
			OperationID: op,
			Method:      http.MethodPost,
			Path:        path,
			Summary:     summary,
			Tags:        []string{"Fees"},
			Security:    admin,
		}, func(ctx context.Context, in *struct {
			ID string `path:"id" format:"uuid"`
		}) (*struct {
			Body struct {
				Invoice InvoiceView `json:"invoice"`
			}
		}, error) {
			p, err := requirePermission(ctx, auth.PermAcademicsManage)
			if err != nil {
				return nil, err
			}
			id, err := parseUUID(in.ID, "invoice")
			if err != nil {
				return nil, err
			}
			inv, err := act(ctx, p.TenantID, id, fees.Author{UserID: p.UserID})
			if err != nil {
				return nil, feesError(err)
			}
			out := &struct {
				Body struct {
					Invoice InvoiceView `json:"invoice"`
				}
			}{}
			out.Body.Invoice = invoiceView(inv)
			return out, nil
		})
	}
	transition("waive-fee-invoice", "/v1/fee-invoices/{id}/waive", "Forgive an unpaid invoice", svc.WaiveInvoice)
	transition("cancel-fee-invoice", "/v1/fee-invoices/{id}/cancel", "Void an unpaid invoice", svc.CancelInvoice)

	huma.Register(api, huma.Operation{
		OperationID: "student-fee-ledger",
		Method:      http.MethodGet,
		Path:        "/v1/students/{id}/fees",
		Summary:     "A student's fee ledger and outstanding balance",
		Tags:        []string{"Fees"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Ledger LedgerView `json:"ledger"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.ID, "student")
		if err != nil {
			return nil, err
		}
		ledger, err := svc.StudentLedger(ctx, p.TenantID, studentID)
		if err != nil {
			return nil, feesError(err)
		}
		out := &struct {
			Body struct {
				Ledger LedgerView `json:"ledger"`
			}
		}{}
		out.Body.Ledger.Invoices = make([]InvoiceView, 0, len(ledger.Invoices))
		for _, i := range ledger.Invoices {
			out.Body.Ledger.Invoices = append(out.Body.Ledger.Invoices, invoiceView(i))
		}
		out.Body.Ledger.Outstanding = ledger.Outstanding
		if out.Body.Ledger.Outstanding == nil {
			out.Body.Ledger.Outstanding = map[string]int64{}
		}
		return out, nil
	})
}

// feesError maps the fees package's sentinels onto status codes.
func feesError(err error) error {
	switch {
	case errors.Is(err, fees.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, fees.ErrNotUnpaid):
		return huma.Error409Conflict("Only an unpaid invoice can be changed that way.")
	case errors.Is(err, fees.ErrNoTarget):
		return huma.Error422UnprocessableEntity("Name the students, or a class, to bill.")
	case errors.Is(err, fees.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, fees.ErrInvalidStructure),
		errors.Is(err, fees.ErrInvalidInvoice),
		errors.Is(err, fees.ErrInvalidPayment):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
