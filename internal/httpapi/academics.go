package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/auth"
)

// dateLayout is how the academic layer speaks dates on the wire: a plain calendar
// day, no clock and no zone, because a term starts on a date, not at an instant.
const dateLayout = "2006-01-02"

// YearView is an academic year as a client sees it.
type YearView struct {
	ID        string `json:"id" format:"uuid"`
	Name      string `json:"name"`
	StartsOn  string `json:"starts_on" format:"date"`
	EndsOn    string `json:"ends_on" format:"date"`
	IsCurrent bool   `json:"is_current"`
}

// TermView is a term within a year.
type TermView struct {
	ID       string `json:"id" format:"uuid"`
	Name     string `json:"name"`
	StartsOn string `json:"starts_on" format:"date"`
	EndsOn   string `json:"ends_on" format:"date"`
	Position int    `json:"position"`
}

// ClassView is a class (grade level).
type ClassView struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name"`
	Rank int    `json:"rank"`
}

// SectionView is a section within a class.
type SectionView struct {
	ID       string `json:"id" format:"uuid"`
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

func yearView(y academics.AcademicYear) YearView {
	return YearView{
		ID: y.ID.String(), Name: y.Name,
		StartsOn: y.StartsOn.Format(dateLayout), EndsOn: y.EndsOn.Format(dateLayout),
		IsCurrent: y.IsCurrent,
	}
}

func termView(t academics.Term) TermView {
	return TermView{
		ID: t.ID.String(), Name: t.Name,
		StartsOn: t.StartsOn.Format(dateLayout), EndsOn: t.EndsOn.Format(dateLayout),
		Position: t.Position,
	}
}

func registerAcademics(api huma.API, svc *academics.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	// --- Institution type ------------------------------------------------------

	huma.Register(api, huma.Operation{
		OperationID: "get-institution-type",
		Method:      http.MethodGet,
		Path:        "/v1/academics/institution-type",
		Summary:     "How this workspace describes itself",
		Tags:        []string{"Academics"},
		Security:    admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Type string `json:"type" enum:"school,college,madrasa,coaching"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		kind, err := svc.InstitutionType(ctx, p.TenantID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Type string `json:"type" enum:"school,college,madrasa,coaching"`
			}
		}{}
		out.Body.Type = kind
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-institution-type",
		Method:      http.MethodPut,
		Path:        "/v1/academics/institution-type",
		Summary:     "Set what kind of institution this is",
		Tags:        []string{"Academics"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Type string `json:"type" enum:"school,college,madrasa,coaching"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		if err := svc.SetInstitutionType(ctx, p.TenantID, in.Body.Type); err != nil {
			return nil, academicsError(err)
		}
		return &struct{}{}, nil
	})

	// --- Academic years --------------------------------------------------------

	huma.Register(api, huma.Operation{
		OperationID: "list-academic-years",
		Method:      http.MethodGet,
		Path:        "/v1/academic-years",
		Summary:     "List the academic years, current first",
		Tags:        []string{"Academics"},
		Security:    admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Years []YearView `json:"years"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		years, err := svc.Years(ctx, p.TenantID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Years []YearView `json:"years"`
			}
		}{}
		out.Body.Years = make([]YearView, 0, len(years))
		for _, y := range years {
			out.Body.Years = append(out.Body.Years, yearView(y))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-academic-year",
		Method:        http.MethodPost,
		Path:          "/v1/academic-years",
		Summary:       "Open an academic year",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name     string `json:"name" minLength:"1" maxLength:"120"`
			StartsOn string `json:"starts_on" format:"date"`
			EndsOn   string `json:"ends_on" format:"date"`
		}
	}) (*struct {
		Body struct {
			Year YearView `json:"year"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		starts, ends, err := parseDates(in.Body.StartsOn, in.Body.EndsOn)
		if err != nil {
			return nil, err
		}
		year, err := svc.CreateYear(ctx, p.TenantID,
			academics.NewAcademicYear{Name: in.Body.Name, StartsOn: starts, EndsOn: ends},
			academics.Author{UserID: p.UserID})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Year YearView `json:"year"`
			}
		}{}
		out.Body.Year = yearView(year)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-current-year",
		Method:      http.MethodPut,
		Path:        "/v1/academic-years/{id}/current",
		Summary:     "Make one year the current one",
		Description: "Clears any other current year, so there is always at most one.",
		Tags:        []string{"Academics"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Year YearView `json:"year"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		yearID, err := parseUUID(in.ID, "year")
		if err != nil {
			return nil, err
		}
		year, err := svc.SetCurrentYear(ctx, p.TenantID, yearID, academics.Author{UserID: p.UserID})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Year YearView `json:"year"`
			}
		}{}
		out.Body.Year = yearView(year)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-academic-year",
		Method:        http.MethodDelete,
		Path:          "/v1/academic-years/{id}",
		Summary:       "Remove an academic year and its terms",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		yearID, err := parseUUID(in.ID, "year")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteYear(ctx, p.TenantID, yearID); err != nil {
			return nil, academicsError(err)
		}
		return &struct{}{}, nil
	})

	// --- Terms -----------------------------------------------------------------

	huma.Register(api, huma.Operation{
		OperationID: "list-terms",
		Method:      http.MethodGet,
		Path:        "/v1/academic-years/{id}/terms",
		Summary:     "List a year's terms, in order",
		Tags:        []string{"Academics"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Terms []TermView `json:"terms"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		yearID, err := parseUUID(in.ID, "year")
		if err != nil {
			return nil, err
		}
		terms, err := svc.Terms(ctx, p.TenantID, yearID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Terms []TermView `json:"terms"`
			}
		}{}
		out.Body.Terms = make([]TermView, 0, len(terms))
		for _, t := range terms {
			out.Body.Terms = append(out.Body.Terms, termView(t))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-term",
		Method:        http.MethodPost,
		Path:          "/v1/academic-years/{id}/terms",
		Summary:       "Add a term to a year",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Name     string `json:"name" minLength:"1" maxLength:"120"`
			StartsOn string `json:"starts_on" format:"date"`
			EndsOn   string `json:"ends_on" format:"date"`
		}
	}) (*struct {
		Body struct {
			Term TermView `json:"term"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		yearID, err := parseUUID(in.ID, "year")
		if err != nil {
			return nil, err
		}
		starts, ends, err := parseDates(in.Body.StartsOn, in.Body.EndsOn)
		if err != nil {
			return nil, err
		}
		term, err := svc.CreateTerm(ctx, p.TenantID, yearID,
			academics.NewTerm{Name: in.Body.Name, StartsOn: starts, EndsOn: ends})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Term TermView `json:"term"`
			}
		}{}
		out.Body.Term = termView(term)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-term",
		Method:        http.MethodDelete,
		Path:          "/v1/terms/{id}",
		Summary:       "Remove a term",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		termID, err := parseUUID(in.ID, "term")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteTerm(ctx, p.TenantID, termID); err != nil {
			return nil, academicsError(err)
		}
		return &struct{}{}, nil
	})

	// --- Classes ---------------------------------------------------------------

	huma.Register(api, huma.Operation{
		OperationID: "list-classes",
		Method:      http.MethodGet,
		Path:        "/v1/classes",
		Summary:     "List the classes, junior to senior",
		Tags:        []string{"Academics"},
		Security:    admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Classes []ClassView `json:"classes"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		classes, err := svc.Classes(ctx, p.TenantID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Classes []ClassView `json:"classes"`
			}
		}{}
		out.Body.Classes = make([]ClassView, 0, len(classes))
		for _, c := range classes {
			out.Body.Classes = append(out.Body.Classes, ClassView{ID: c.ID.String(), Name: c.Name, Rank: c.Rank})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-class",
		Method:        http.MethodPost,
		Path:          "/v1/classes",
		Summary:       "Open a class",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name string `json:"name" minLength:"1" maxLength:"120"`
			Rank int    `json:"rank,omitempty" minimum:"0" maximum:"1000"`
		}
	}) (*struct {
		Body struct {
			Class ClassView `json:"class"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		class, err := svc.CreateClass(ctx, p.TenantID,
			academics.NewGradeLevel{Name: in.Body.Name, Rank: in.Body.Rank},
			academics.Author{UserID: p.UserID})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Class ClassView `json:"class"`
			}
		}{}
		out.Body.Class = ClassView{ID: class.ID.String(), Name: class.Name, Rank: class.Rank}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-class",
		Method:        http.MethodDelete,
		Path:          "/v1/classes/{id}",
		Summary:       "Remove a class and its sections",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		classID, err := parseUUID(in.ID, "class")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteClass(ctx, p.TenantID, classID); err != nil {
			return nil, academicsError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "promote-class",
		Method:      http.MethodPost,
		Path:        "/v1/classes/{id}/promote",
		Summary:     "Promote a class's students, or graduate them",
		Description: "Moves every active student in this class to to_grade_level_id " +
			"(optionally into to_section_id). With no target, they are graduated.",
		Tags:     []string{"Academics"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			ToGradeLevelID string `json:"to_grade_level_id,omitempty" format:"uuid"`
			ToSectionID    string `json:"to_section_id,omitempty" format:"uuid"`
		}
	}) (*struct {
		Body struct {
			Moved     int  `json:"moved"`
			Graduated bool `json:"graduated"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		classID, err := parseUUID(in.ID, "class")
		if err != nil {
			return nil, err
		}
		toClass, err := optionalUUIDPtr(in.Body.ToGradeLevelID, "target class")
		if err != nil {
			return nil, err
		}
		toSection, err := optionalUUIDPtr(in.Body.ToSectionID, "target section")
		if err != nil {
			return nil, err
		}
		moved, err := svc.PromoteClass(ctx, p.TenantID, classID,
			academics.Promotion{ToGradeLevelID: toClass, ToSectionID: toSection},
			academics.Author{UserID: p.UserID})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Moved     int  `json:"moved"`
				Graduated bool `json:"graduated"`
			}
		}{}
		out.Body.Moved = moved
		out.Body.Graduated = toClass == nil
		return out, nil
	})

	// --- Sections --------------------------------------------------------------

	huma.Register(api, huma.Operation{
		OperationID: "list-sections",
		Method:      http.MethodGet,
		Path:        "/v1/classes/{id}/sections",
		Summary:     "List a class's sections",
		Tags:        []string{"Academics"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Sections []SectionView `json:"sections"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		classID, err := parseUUID(in.ID, "class")
		if err != nil {
			return nil, err
		}
		sections, err := svc.Sections(ctx, p.TenantID, classID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Sections []SectionView `json:"sections"`
			}
		}{}
		out.Body.Sections = make([]SectionView, 0, len(sections))
		for _, s := range sections {
			out.Body.Sections = append(out.Body.Sections, SectionView{ID: s.ID.String(), Name: s.Name, Capacity: s.Capacity})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-section",
		Method:        http.MethodPost,
		Path:          "/v1/classes/{id}/sections",
		Summary:       "Add a section to a class",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Name     string `json:"name" minLength:"1" maxLength:"120"`
			Capacity int    `json:"capacity,omitempty" minimum:"0" maximum:"100000"`
		}
	}) (*struct {
		Body struct {
			Section SectionView `json:"section"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		classID, err := parseUUID(in.ID, "class")
		if err != nil {
			return nil, err
		}
		section, err := svc.CreateSection(ctx, p.TenantID, classID,
			academics.NewSection{Name: in.Body.Name, Capacity: in.Body.Capacity})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Section SectionView `json:"section"`
			}
		}{}
		out.Body.Section = SectionView{ID: section.ID.String(), Name: section.Name, Capacity: section.Capacity}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-section",
		Method:        http.MethodDelete,
		Path:          "/v1/sections/{id}",
		Summary:       "Remove a section",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		sectionID, err := parseUUID(in.ID, "section")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteSection(ctx, p.TenantID, sectionID); err != nil {
			return nil, academicsError(err)
		}
		return &struct{}{}, nil
	})
}

// SubjectView is a subject in the catalog.
type SubjectView struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name"`
	Code string `json:"code,omitempty"`
}

func registerSubjects(api huma.API, svc *academics.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-subjects",
		Method:      http.MethodGet,
		Path:        "/v1/subjects",
		Summary:     "List the subjects the institution teaches",
		Tags:        []string{"Academics"},
		Security:    admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Subjects []SubjectView `json:"subjects"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		subjects, err := svc.Subjects(ctx, p.TenantID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Subjects []SubjectView `json:"subjects"`
			}
		}{}
		out.Body.Subjects = make([]SubjectView, 0, len(subjects))
		for _, s := range subjects {
			out.Body.Subjects = append(out.Body.Subjects, SubjectView{ID: s.ID.String(), Name: s.Name, Code: s.Code})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-subject",
		Method:        http.MethodPost,
		Path:          "/v1/subjects",
		Summary:       "Add a subject",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name string `json:"name" minLength:"1" maxLength:"120"`
			Code string `json:"code,omitempty" maxLength:"40"`
		}
	}) (*struct {
		Body struct {
			Subject SubjectView `json:"subject"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		subject, err := svc.CreateSubject(ctx, p.TenantID, academics.NewSubject{Name: in.Body.Name, Code: in.Body.Code})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Subject SubjectView `json:"subject"`
			}
		}{}
		out.Body.Subject = SubjectView{ID: subject.ID.String(), Name: subject.Name, Code: subject.Code}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-subject",
		Method:        http.MethodDelete,
		Path:          "/v1/subjects/{id}",
		Summary:       "Remove a subject",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Academics"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		subjectID, err := parseUUID(in.ID, "subject")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteSubject(ctx, p.TenantID, subjectID); err != nil {
			return nil, academicsError(err)
		}
		return &struct{}{}, nil
	})
}

// parseDates reads two calendar days, refusing a malformed one with 422.
func parseDates(startsOn, endsOn string) (time.Time, time.Time, error) {
	starts, err := time.Parse(dateLayout, startsOn)
	if err != nil {
		return time.Time{}, time.Time{}, huma.Error422UnprocessableEntity("starts_on must be a date, as YYYY-MM-DD.")
	}
	ends, err := time.Parse(dateLayout, endsOn)
	if err != nil {
		return time.Time{}, time.Time{}, huma.Error422UnprocessableEntity("ends_on must be a date, as YYYY-MM-DD.")
	}
	return starts, ends, nil
}

// academicsError maps the academics package's sentinels onto status codes.
func academicsError(err error) error {
	switch {
	case errors.Is(err, academics.ErrNotFound):
		return huma.Error404NotFound("Not found.")

	case errors.Is(err, academics.ErrNameTaken):
		return huma.Error409Conflict("That name is already used in this workspace.")

	case errors.Is(err, academics.ErrAdmissionTaken):
		return huma.Error409Conflict("That admission number is already used in this workspace.")

	case errors.Is(err, academics.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")

	case errors.Is(err, academics.ErrInvalidYear),
		errors.Is(err, academics.ErrInvalidTerm),
		errors.Is(err, academics.ErrInvalidClass),
		errors.Is(err, academics.ErrInvalidSection),
		errors.Is(err, academics.ErrInvalidStudent),
		errors.Is(err, academics.ErrInvalidGuardian),
		errors.Is(err, academics.ErrInvalidSubject),
		errors.Is(err, academics.ErrInvalidAttendance),
		errors.Is(err, academics.ErrInvalidPeriod),
		errors.Is(err, academics.ErrInvalidPromotion),
		errors.Is(err, academics.ErrInvalidInstitutionType):
		return huma.Error422UnprocessableEntity(err.Error())

	default:
		return err
	}
}
