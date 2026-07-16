package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/library"
)

// BookView is one title the library holds, with its copy counts.
type BookView struct {
	ID              string `json:"id" format:"uuid"`
	Title           string `json:"title"`
	Author          string `json:"author,omitempty"`
	ISBN            string `json:"isbn,omitempty"`
	Category        string `json:"category,omitempty"`
	TotalCopies     int    `json:"total_copies"`
	AvailableCopies int    `json:"available_copies"`
}

// LoanView is one book lent to one student.
type LoanView struct {
	ID         string `json:"id" format:"uuid"`
	BookID     string `json:"book_id" format:"uuid"`
	StudentID  string `json:"student_id" format:"uuid"`
	BorrowedAt string `json:"borrowed_at" format:"date-time"`
	DueAt      string `json:"due_at" format:"date-time"`
	ReturnedAt string `json:"returned_at,omitempty" format:"date-time"`
	Status     string `json:"status" enum:"out,returned"`
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func bookView(b library.Book) BookView {
	return BookView{
		ID: b.ID.String(), Title: b.Title, Author: b.Author, ISBN: strOr(b.ISBN),
		Category: strOr(b.Category), TotalCopies: b.TotalCopies, AvailableCopies: b.AvailableCopies,
	}
}

func loanView(l library.Loan) LoanView {
	v := LoanView{
		ID: l.ID.String(), BookID: l.BookID.String(), StudentID: l.StudentID.String(),
		BorrowedAt: l.BorrowedAt.Format(time.RFC3339), DueAt: l.DueAt.Format(time.RFC3339),
		Status: l.Status,
	}
	if l.ReturnedAt != nil {
		v.ReturnedAt = l.ReturnedAt.Format(time.RFC3339)
	}
	return v
}

func registerLibrary(api huma.API, svc *library.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-library-books",
		Method:      http.MethodGet,
		Path:        "/v1/library/books",
		Summary:     "The library catalogue, newest first",
		Description: "Keyset-paginated. Filter by category.",
		Tags:        []string{"Library"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Category string `query:"category"`
		Limit    int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor   string `query:"cursor"`
	}) (*struct {
		Body struct {
			Books      []BookView `json:"books"`
			NextCursor string     `json:"next_cursor,omitempty"`
			HasMore    bool       `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		page, err := svc.ListBooks(ctx, p.TenantID, library.BookFilter{Category: in.Category},
			library.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, libraryError(err)
		}
		out := &struct {
			Body struct {
				Books      []BookView `json:"books"`
				NextCursor string     `json:"next_cursor,omitempty"`
				HasMore    bool       `json:"has_more"`
			}
		}{}
		out.Body.Books = make([]BookView, 0, len(page.Books))
		for _, b := range page.Books {
			out.Body.Books = append(out.Body.Books, bookView(b))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-library-book",
		Method:        http.MethodPost,
		Path:          "/v1/library/books",
		Summary:       "Add a book to the catalogue",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Library"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Title       string `json:"title" minLength:"1" maxLength:"300"`
			Author      string `json:"author,omitempty" maxLength:"300"`
			ISBN        string `json:"isbn,omitempty" maxLength:"20"`
			Category    string `json:"category,omitempty" maxLength:"120"`
			TotalCopies int    `json:"total_copies,omitempty" minimum:"0" maximum:"100000"`
		}
	}) (*struct {
		Body struct {
			Book BookView `json:"book"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		b, err := svc.AddBook(ctx, p.TenantID, library.NewBook{
			Title: in.Body.Title, Author: in.Body.Author,
			ISBN: optionalStr(in.Body.ISBN), Category: optionalStr(in.Body.Category),
			TotalCopies: in.Body.TotalCopies,
		}, library.Author{UserID: p.UserID})
		if err != nil {
			return nil, libraryError(err)
		}
		out := &struct {
			Body struct {
				Book BookView `json:"book"`
			}
		}{}
		out.Body.Book = bookView(b)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-library-loans",
		Method:      http.MethodGet,
		Path:        "/v1/library/loans",
		Summary:     "Library loans, newest first",
		Description: "Keyset-paginated. Filter by student_id and/or status.",
		Tags:        []string{"Library"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		StudentID string `query:"student_id" format:"uuid"`
		Status    string `query:"status" enum:"out,returned"`
		Limit     int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor    string `query:"cursor"`
	}) (*struct {
		Body struct {
			Loans      []LoanView `json:"loans"`
			NextCursor string     `json:"next_cursor,omitempty"`
			HasMore    bool       `json:"has_more"`
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
		page, err := svc.ListLoans(ctx, p.TenantID, library.LoanFilter{StudentID: student, Status: in.Status},
			library.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, libraryError(err)
		}
		out := &struct {
			Body struct {
				Loans      []LoanView `json:"loans"`
				NextCursor string     `json:"next_cursor,omitempty"`
				HasMore    bool       `json:"has_more"`
			}
		}{}
		out.Body.Loans = make([]LoanView, 0, len(page.Loans))
		for _, l := range page.Loans {
			out.Body.Loans = append(out.Body.Loans, loanView(l))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "issue-library-loan",
		Method:        http.MethodPost,
		Path:          "/v1/library/loans",
		Summary:       "Lend a book to a student",
		Description:   "Draws one available copy. Refused with 409 when no copies are free.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Library"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			BookID    string `json:"book_id" format:"uuid"`
			StudentID string `json:"student_id" format:"uuid"`
			DueAt     string `json:"due_at,omitempty" format:"date-time"`
		}
	}) (*struct {
		Body struct {
			Loan LoanView `json:"loan"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		bookID, err := parseUUID(in.Body.BookID, "book")
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.Body.StudentID, "student")
		if err != nil {
			return nil, err
		}
		var due time.Time
		if in.Body.DueAt != "" {
			due, err = time.Parse(time.RFC3339, in.Body.DueAt)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("The due date is not a valid timestamp.")
			}
		}
		l, err := svc.IssueLoan(ctx, p.TenantID, library.NewLoan{
			BookID: bookID, StudentID: studentID, DueAt: due,
		}, library.Author{UserID: p.UserID})
		if err != nil {
			return nil, libraryError(err)
		}
		out := &struct {
			Body struct {
				Loan LoanView `json:"loan"`
			}
		}{}
		out.Body.Loan = loanView(l)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "return-library-loan",
		Method:      http.MethodPost,
		Path:        "/v1/library/loans/{id}/return",
		Summary:     "Return a borrowed book",
		Description: "Puts the copy back. The out guard makes a double submission harmless.",
		Tags:        []string{"Library"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Loan LoanView `json:"loan"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "loan")
		if err != nil {
			return nil, err
		}
		l, err := svc.ReturnLoan(ctx, p.TenantID, id, library.Author{UserID: p.UserID})
		if err != nil {
			return nil, libraryError(err)
		}
		out := &struct {
			Body struct {
				Loan LoanView `json:"loan"`
			}
		}{}
		out.Body.Loan = loanView(l)
		return out, nil
	})
}

func optionalStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// libraryError maps the library package's sentinels onto status codes.
func libraryError(err error) error {
	switch {
	case errors.Is(err, library.ErrNotFound):
		return huma.Error404NotFound("We couldn't find that book or loan.")
	case errors.Is(err, library.ErrNoCopies):
		return huma.Error409Conflict("No copies of that book are available to lend.")
	case errors.Is(err, library.ErrAlreadyReturned):
		return huma.Error409Conflict("That loan has already been returned.")
	case errors.Is(err, library.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")
	case errors.Is(err, library.ErrInvalidBook),
		errors.Is(err, library.ErrInvalidLoan):
		return huma.Error422UnprocessableEntity("Check the book and loan details and try again.")
	default:
		return err
	}
}
