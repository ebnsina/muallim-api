package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/certify"
	"github.com/ebnsina/muallim-api/internal/tenant"
)

/*
CertificateView is a certificate, as anybody holding its serial may read it.

There is no email on it, and no user id. The name is here because the name is the
point of a certificate; nothing else about the person is anybody's business,
including whoever they showed the code to.
*/
type CertificateView struct {
	Serial string `json:"serial"`

	LearnerName string    `json:"learner_name"`
	CourseTitle string    `json:"course_title"`
	IssuedAt    time.Time `json:"issued_at"`

	// The words as they were printed. Rendered once, at issue, and stored — a
	// certificate does not change its wording because somebody edited a template.
	Title     string `json:"title"`
	Body      string `json:"body"`
	Signatory string `json:"signatory,omitempty"`

	// A revoked certificate still resolves, and says so. A code that stopped
	// answering would be indistinguishable from a code that was never real.
	Revoked       bool       `json:"revoked"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedReason string     `json:"revoked_reason,omitempty"`
}

// TemplateView is what a certificate says before a name is put on it.
type TemplateView struct {
	ID      string `json:"id,omitempty" format:"uuid"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin" doc:"True for the template this system ships. It cannot be edited or deleted."`

	Title     string `json:"title"`
	Body      string `json:"body"`
	Signatory string `json:"signatory,omitempty"`
}

// CertificateOutput is one certificate.
type CertificateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Certificate CertificateView `json:"certificate"`
	}
}

// CertificatesOutput is a learner's own certificates.
type CertificatesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Certificates []CertificateView `json:"certificates"`

		// NextCursor is opaque. Pass it back as `cursor` for the next page; its
		// absence means this was the last. Empty on a learner's own list, which is
		// bounded rather than paged.
		NextCursor string `json:"next_cursor,omitempty"`
		HasMore    bool   `json:"has_more"`
	}
}

// TemplatesOutput lists a workspace's templates, the built-in one first.
type TemplatesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Templates []TemplateView `json:"templates"`
	}
}

// TemplateOutput confirms a write.
type TemplateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Template TemplateView `json:"template"`
	}
}

// CourseTemplateOutput is which template a course prints. Null means the default.
type CourseTemplateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		TemplateID *string `json:"template_id,omitempty" doc:"Null for the built-in default."`
	}
}

func registerCertificates(api huma.API, svc *certify.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "verify-certificate",
		Method:      http.MethodGet,
		Path:        "/v1/certificates/{serial}",
		Summary:     "Read a certificate by its number",
		Description: "No sign-in. Whoever holds the number may read what it says — that is what a " +
			"certificate is for. A revoked one still answers, and says it is revoked.",
		Tags: []string{"Certificates"},
	}, func(ctx context.Context, in *struct {
		Serial string `path:"serial" minLength:"4" maxLength:"64"`
	}) (*CertificateOutput, error) {
		// The workspace comes from the host, as it does for every request. The serial
		// is unique across the installation, so a code from another workspace is a
		// code this one has never heard of — which is the honest answer.
		certificate, err := svc.Verify(ctx, tenant.ID(ctx), in.Serial)
		if err != nil {
			return nil, certifyError(err)
		}

		/*
			Cacheable, briefly, and shared.

			Nothing about it depends on who asked: the same serial gives the same answer
			to a learner, an employer, and a stranger. Sixty seconds bounds how long a
			revocation takes to be believed by a proxy, which is the only thing that
			changes about a certificate after it is issued.
		*/
		out := &CertificateOutput{CacheControl: "public, max-age=60"}
		out.Body.Certificate = certificateView(certificate)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "my-certificates",
		Method:      http.MethodGet,
		Path:        "/v1/me/certificates",
		Summary:     "The certificates you have earned",
		Tags:        []string{"Certificates"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, _ *struct{}) (*CertificatesOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		// The learner is the caller. No path here takes a user id from a request.
		certificates, err := svc.Mine(ctx, p.TenantID, p.UserID)
		if err != nil {
			return nil, certifyError(err)
		}

		out := &CertificatesOutput{CacheControl: "private, no-store"}
		out.Body.Certificates = make([]CertificateView, 0, len(certificates))
		for _, certificate := range certificates {
			out.Body.Certificates = append(out.Body.Certificates, certificateView(certificate))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-certificates",
		Method:      http.MethodGet,
		Path:        "/v1/certificates",
		Summary:     "What this workspace has issued",
		Description: "Requires course:write. Newest first, keyset-paginated. Until this existed the only way in was a serial — which is what somebody who already holds a certificate has, and not what a registrar looking for the one they issued last week has.",
		Tags:        []string{"Certificates"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor" maxLength:"128" doc:"Opaque cursor from a previous page."`
	}) (*CertificatesOutput, error) {
		p, _, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		page, err := svc.Issued(ctx, p.TenantID, certify.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, certifyError(err)
		}

		out := &CertificatesOutput{CacheControl: "private, no-store"}
		out.Body.Certificates = make([]CertificateView, 0, len(page.Certificates))
		for _, certificate := range page.Certificates {
			out.Body.Certificates = append(out.Body.Certificates, certificateView(certificate))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "revoke-certificate",
		Method:        http.MethodPost,
		Path:          "/v1/certificates/{serial}/revoke",
		Summary:       "Withdraw a certificate",
		DefaultStatus: http.StatusNoContent,
		Description: "It is never deleted: somebody has the number. It keeps answering, and says it " +
			"has been withdrawn. Requires course:write.",
		Tags:     []string{"Certificates"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Serial string `path:"serial" minLength:"4" maxLength:"64"`
		Body   struct {
			Reason string `json:"reason" minLength:"1" maxLength:"500"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		if err := svc.Revoke(ctx, p.TenantID, in.Serial, in.Body.Reason, p.UserID); err != nil {
			return nil, certifyError(err)
		}
		return &struct{}{}, nil
	})

	registerCertificateTemplates(api, svc)
}

func registerCertificateTemplates(api huma.API, svc *certify.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-certificate-templates",
		Method:      http.MethodGet,
		Path:        "/v1/certificate-templates",
		Summary:     "The workspace's certificate templates",
		Description: "The built-in default first. It is not a row and has no id.",
		Tags:        []string{"Certificates"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, _ *struct{}) (*TemplatesOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		templates, err := svc.Templates(ctx, p.TenantID)
		if err != nil {
			return nil, certifyError(err)
		}

		out := &TemplatesOutput{CacheControl: "private, no-store"}
		out.Body.Templates = make([]TemplateView, 0, len(templates))
		for _, template := range templates {
			out.Body.Templates = append(out.Body.Templates, templateView(template))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-certificate-template",
		Method:        http.MethodPost,
		Path:          "/v1/certificate-templates",
		Summary:       "Write a certificate template",
		DefaultStatus: http.StatusCreated,
		Description: "The body may name {{learner}}, {{course}}, {{date}} and {{serial}}. Anything else " +
			"is printed exactly as written, so a typo shows up on the certificate rather than as a gap.",
		Tags:     []string{"Certificates"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name      string `json:"name" minLength:"1" maxLength:"100"`
			Title     string `json:"title" minLength:"1" maxLength:"200"`
			Body      string `json:"body" minLength:"1" maxLength:"4000"`
			Signatory string `json:"signatory,omitempty" maxLength:"200"`
		}
	}) (*TemplateOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		created, err := svc.CreateTemplate(ctx, p.TenantID, certify.Template{
			Name: in.Body.Name, Title: in.Body.Title,
			Body: in.Body.Body, Signatory: in.Body.Signatory,
		})
		if err != nil {
			return nil, certifyError(err)
		}

		out := &TemplateOutput{CacheControl: "private, no-store"}
		out.Body.Template = templateView(created)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-certificate-template",
		Method:        http.MethodDelete,
		Path:          "/v1/certificate-templates/{id}",
		Summary:       "Remove a certificate template",
		DefaultStatus: http.StatusNoContent,
		Description: "Courses printing it fall back to the built-in default. Certificates already " +
			"issued keep the words they were issued with.",
		Tags:     []string{"Certificates"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		templateID, err := parseUUID(in.ID, "template")
		if err != nil {
			return nil, err
		}

		if err := svc.DeleteTemplate(ctx, p.TenantID, templateID); err != nil {
			return nil, certifyError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-course-certificate-template",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/certificate-template",
		Summary:     "Which template a course prints",
		Description: "The template's id, or null for the built-in default. For an editor that shows " +
			"the current choice. Requires course:write.",
		Tags:     []string{"Authoring"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*CourseTemplateOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		id, err := svc.CourseTemplateID(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, certifyError(err)
		}

		out := &CourseTemplateOutput{CacheControl: "private, no-store"}
		if id != nil {
			serial := id.String()
			out.Body.TemplateID = &serial
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-course-certificate-template",
		Method:      http.MethodPut,
		Path:        "/v1/courses/{slug}/certificate-template",
		Summary:     "Choose what a course's certificate says",
		Description: "`null` puts the course back on the built-in default.",
		Tags:        []string{"Authoring"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			TemplateID Optional[string] `json:"template_id,omitempty" doc:"The template's id, or null for the default."`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		var templateID *uuid.UUID
		if in.Body.TemplateID.Sent && !in.Body.TemplateID.Null {
			parsed, err := parseUUID(in.Body.TemplateID.Value, "template")
			if err != nil {
				return nil, err
			}
			templateID = &parsed
		}

		if err := svc.SetCourseTemplate(ctx, p.TenantID, in.Slug, templateID); err != nil {
			return nil, certifyError(err)
		}
		return &struct{}{}, nil
	})
}

func certificateView(c certify.Certificate) CertificateView {
	return CertificateView{
		Serial:      c.Serial,
		LearnerName: c.LearnerName,
		CourseTitle: c.CourseTitle,
		IssuedAt:    c.IssuedAt,
		Title:       c.Title,
		Body:        c.Body,
		Signatory:   c.Signatory,

		Revoked:       c.Revoked(),
		RevokedAt:     c.RevokedAt,
		RevokedReason: c.RevokedReason,
	}
}

func templateView(t certify.Template) TemplateView {
	view := TemplateView{
		Name: t.Name, Builtin: t.Builtin,
		Title: t.Title, Body: t.Body, Signatory: t.Signatory,
	}
	if t.ID != uuid.Nil {
		view.ID = t.ID.String()
	}
	return view
}

// certifyError maps the certify package's sentinels onto status codes. The only
// place that translation happens; the domain never imports net/http.
func certifyError(err error) error {
	switch {
	case errors.Is(err, certify.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not one this API issued.")

	case errors.Is(err, certify.ErrNotFound):
		return huma.Error404NotFound("Not found.")

	case errors.Is(err, certify.ErrTemplateExists):
		return huma.Error409Conflict("A certificate template with that name already exists.")

	case errors.Is(err, certify.ErrInvalidTemplate):
		return huma.Error422UnprocessableEntity(err.Error())

	case errors.Is(err, certify.ErrRevoked):
		// Kept for the day something refuses to act on a revoked certificate. Verifying
		// one is not that day: it answers, and says so.
		return huma.Error409Conflict("That certificate has been withdrawn.")

	default:
		return err
	}
}
