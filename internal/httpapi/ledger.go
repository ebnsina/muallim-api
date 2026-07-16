package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/ledger"
)

// CategoryView is an income or expense head.
type CategoryView struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name"`
	Kind string `json:"kind" enum:"income,expense"`
}

// LedgerEntryView is one dated amount posted against a category. Amount is minor units
// (BDT poisha by default); the client formats it with the currency.
type LedgerEntryView struct {
	ID          string `json:"id" format:"uuid"`
	CategoryID  string `json:"category_id" format:"uuid"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	OccurredOn  string `json:"occurred_on" format:"date"`
	Description string `json:"description,omitempty"`
}

// TotalView is the income, expense and net for one currency.
type TotalView struct {
	Currency string `json:"currency"`
	Income   int64  `json:"income"`
	Expense  int64  `json:"expense"`
	Net      int64  `json:"net"`
}

func categoryView(c ledger.Category) CategoryView {
	return CategoryView{ID: c.ID.String(), Name: c.Name, Kind: c.Kind}
}

func ledgerEntryView(e ledger.Entry) LedgerEntryView {
	return LedgerEntryView{
		ID: e.ID.String(), CategoryID: e.CategoryID.String(), Amount: e.Amount,
		Currency: e.Currency, OccurredOn: e.OccurredOn.Format(dateLayout), Description: e.Description,
	}
}

func registerLedger(api huma.API, svc *ledger.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-ledger-categories",
		Method:      http.MethodGet,
		Path:        "/v1/ledger/categories",
		Summary:     "The workspace's income and expense heads",
		Tags:        []string{"Ledger"},
		Security:    admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Categories []CategoryView `json:"categories"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		list, err := svc.ListCategories(ctx, p.TenantID)
		if err != nil {
			return nil, ledgerError(err)
		}
		out := &struct {
			Body struct {
				Categories []CategoryView `json:"categories"`
			}
		}{}
		out.Body.Categories = make([]CategoryView, 0, len(list))
		for _, c := range list {
			out.Body.Categories = append(out.Body.Categories, categoryView(c))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-ledger-category",
		Method:        http.MethodPost,
		Path:          "/v1/ledger/categories",
		Summary:       "Define an income or expense head",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Ledger"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name string `json:"name" minLength:"1" maxLength:"160"`
			Kind string `json:"kind" enum:"income,expense"`
		}
	}) (*struct {
		Body struct {
			Category CategoryView `json:"category"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		c, err := svc.CreateCategory(ctx, p.TenantID, ledger.NewCategory{
			Name: in.Body.Name, Kind: in.Body.Kind,
		}, ledger.Author{UserID: p.UserID})
		if err != nil {
			return nil, ledgerError(err)
		}
		out := &struct {
			Body struct {
				Category CategoryView `json:"category"`
			}
		}{}
		out.Body.Category = categoryView(c)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-ledger-entries",
		Method:      http.MethodGet,
		Path:        "/v1/ledger/entries",
		Summary:     "Ledger entries, newest first",
		Description: "Keyset-paginated. Filter by kind, category_id and/or a date range (from/to).",
		Tags:        []string{"Ledger"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Kind       string `query:"kind" enum:"income,expense"`
		CategoryID string `query:"category_id" format:"uuid"`
		From       string `query:"from" format:"date"`
		To         string `query:"to" format:"date"`
		Limit      int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor     string `query:"cursor"`
	}) (*struct {
		Body struct {
			Entries    []LedgerEntryView `json:"entries"`
			NextCursor string            `json:"next_cursor,omitempty"`
			HasMore    bool              `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		filter, err := ledgerFilter(in.Kind, in.CategoryID, in.From, in.To)
		if err != nil {
			return nil, err
		}
		page, err := svc.ListEntries(ctx, p.TenantID, filter, ledger.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, ledgerError(err)
		}
		out := &struct {
			Body struct {
				Entries    []LedgerEntryView `json:"entries"`
				NextCursor string            `json:"next_cursor,omitempty"`
				HasMore    bool              `json:"has_more"`
			}
		}{}
		out.Body.Entries = make([]LedgerEntryView, 0, len(page.Entries))
		for _, e := range page.Entries {
			out.Body.Entries = append(out.Body.Entries, ledgerEntryView(e))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "record-ledger-entry",
		Method:        http.MethodPost,
		Path:          "/v1/ledger/entries",
		Summary:       "Record an income or expense entry",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Ledger"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			CategoryID  string `json:"category_id" format:"uuid"`
			Amount      int64  `json:"amount" minimum:"0"`
			Currency    string `json:"currency,omitempty" minLength:"3" maxLength:"3"`
			OccurredOn  string `json:"occurred_on" format:"date"`
			Description string `json:"description,omitempty" maxLength:"500"`
		}
	}) (*struct {
		Body struct {
			Entry LedgerEntryView `json:"entry"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		categoryID, err := parseUUID(in.Body.CategoryID, "category")
		if err != nil {
			return nil, err
		}
		occurredOn, err := time.Parse(dateLayout, in.Body.OccurredOn)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("occurred_on is not a valid date.")
		}
		e, err := svc.RecordEntry(ctx, p.TenantID, ledger.NewEntry{
			CategoryID: categoryID, Amount: in.Body.Amount, Currency: in.Body.Currency,
			OccurredOn: occurredOn, Description: in.Body.Description,
		}, ledger.Author{UserID: p.UserID})
		if err != nil {
			return nil, ledgerError(err)
		}
		out := &struct {
			Body struct {
				Entry LedgerEntryView `json:"entry"`
			}
		}{}
		out.Body.Entry = ledgerEntryView(e)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "ledger-summary",
		Method:      http.MethodGet,
		Path:        "/v1/ledger/summary",
		Summary:     "Income, expense and net totals per currency",
		Description: "Same filters as the entry listing: kind, category_id and/or a date range (from/to).",
		Tags:        []string{"Ledger"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Kind       string `query:"kind" enum:"income,expense"`
		CategoryID string `query:"category_id" format:"uuid"`
		From       string `query:"from" format:"date"`
		To         string `query:"to" format:"date"`
	}) (*struct {
		Body struct {
			Totals []TotalView `json:"totals"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		filter, err := ledgerFilter(in.Kind, in.CategoryID, in.From, in.To)
		if err != nil {
			return nil, err
		}
		summary, err := svc.Summarise(ctx, p.TenantID, filter)
		if err != nil {
			return nil, ledgerError(err)
		}
		out := &struct {
			Body struct {
				Totals []TotalView `json:"totals"`
			}
		}{}
		out.Body.Totals = make([]TotalView, 0, len(summary.Totals))
		for _, t := range summary.Totals {
			out.Body.Totals = append(out.Body.Totals, TotalView{
				Currency: t.Currency, Income: t.Income, Expense: t.Expense, Net: t.Net,
			})
		}
		return out, nil
	})
}

// ledgerFilter builds an EntryFilter from the shared query parameters.
func ledgerFilter(kind, categoryID, from, to string) (ledger.EntryFilter, error) {
	category, err := optionalUUIDPtr(categoryID, "category")
	if err != nil {
		return ledger.EntryFilter{}, err
	}
	fromDate, err := optionalDate(from)
	if err != nil {
		return ledger.EntryFilter{}, err
	}
	toDate, err := optionalDate(to)
	if err != nil {
		return ledger.EntryFilter{}, err
	}
	return ledger.EntryFilter{Kind: kind, CategoryID: category, From: fromDate, To: toDate}, nil
}

// ledgerError maps the ledger package's sentinels onto status codes.
func ledgerError(err error) error {
	switch {
	case errors.Is(err, ledger.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, ledger.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, ledger.ErrInvalidCategory),
		errors.Is(err, ledger.ErrInvalidEntry):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
