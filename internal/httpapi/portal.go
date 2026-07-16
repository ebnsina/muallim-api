package httpapi

// The parent-and-pupil portal. A guardian signs in and sees their own child's
// day — attendance, fees, memorisation — and nothing else; a pupil sees their
// own. Every read is gated twice: the `portal:read` permission proves the caller
// is a guardian or student at all, and academics.ChildStudent proves this
// particular student is theirs. A student who is not the caller's is ErrNotFound,
// never their data. The reads themselves reuse the very services the admin
// endpoints use — the difference is who may call them, and about whom.

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/fees"
	"github.com/ebnsina/muallim-api/internal/hifz"
)

func registerPortal(api huma.API, schooling *academics.Service, billing *fees.Service, memorization *hifz.Service) {
	security := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "portal-children",
		Method:      http.MethodGet,
		Path:        "/v1/portal/children",
		Summary:     "The students the signed-in guardian or pupil may see",
		Tags:        []string{"Portal"},
		Security:    security,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Children []StudentView `json:"children"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermPortalRead)
		if err != nil {
			return nil, err
		}
		children, err := schooling.ChildrenFor(ctx, p.TenantID, p.UserID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Children []StudentView `json:"children"`
			}
		}{}
		out.Body.Children = make([]StudentView, 0, len(children))
		for _, s := range children {
			out.Body.Children = append(out.Body.Children, studentView(s))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "portal-child-attendance",
		Method:      http.MethodGet,
		Path:        "/v1/portal/children/{id}/attendance",
		Summary:     "A child's attendance over a range",
		Tags:        []string{"Portal"},
		Security:    security,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		From string `query:"from" format:"date" required:"true"`
		To   string `query:"to" format:"date" required:"true"`
	}) (*struct {
		Body struct {
			Days    []AttendanceDayView   `json:"days"`
			Summary AttendanceSummaryView `json:"summary"`
		}
	}, error) {
		p, studentID, err := portalChild(ctx, schooling, in.ID)
		if err != nil {
			return nil, err
		}
		from, to, err := parseDates(in.From, in.To)
		if err != nil {
			return nil, err
		}
		days, summary, err := schooling.StudentAttendance(ctx, p.TenantID, studentID, from, to)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Days    []AttendanceDayView   `json:"days"`
				Summary AttendanceSummaryView `json:"summary"`
			}
		}{}
		out.Body.Days = make([]AttendanceDayView, 0, len(days))
		for _, d := range days {
			out.Body.Days = append(out.Body.Days, AttendanceDayView{
				OnDate: d.OnDate.Format(dateLayout), SectionID: uuidPtrString(d.SectionID), Status: d.Status,
			})
		}
		out.Body.Summary = AttendanceSummaryView{
			Present: summary.Present, Absent: summary.Absent, Late: summary.Late,
			Excused: summary.Excused, Total: summary.Total,
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "portal-child-fees",
		Method:      http.MethodGet,
		Path:        "/v1/portal/children/{id}/fees",
		Summary:     "A child's invoices and what they still owe",
		Tags:        []string{"Portal"},
		Security:    security,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Ledger LedgerView `json:"ledger"`
		}
	}, error) {
		p, studentID, err := portalChild(ctx, schooling, in.ID)
		if err != nil {
			return nil, err
		}
		ledger, err := billing.StudentLedger(ctx, p.TenantID, studentID)
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

	huma.Register(api, huma.Operation{
		OperationID: "portal-child-hifz",
		Method:      http.MethodGet,
		Path:        "/v1/portal/children/{id}/hifz",
		Summary:     "Where a child's Sabaq stands, and recent activity",
		Tags:        []string{"Portal"},
		Security:    security,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Days int    `query:"days" default:"30" minimum:"1" maximum:"365"`
	}) (*struct {
		Body struct {
			Summary HifzSummaryView `json:"summary"`
		}
	}, error) {
		p, studentID, err := portalChild(ctx, schooling, in.ID)
		if err != nil {
			return nil, err
		}
		summary, err := memorization.Summary(ctx, p.TenantID, studentID, time.Now().AddDate(0, 0, -in.Days))
		if err != nil {
			return nil, hifzError(err)
		}
		out := &struct {
			Body struct {
				Summary HifzSummaryView `json:"summary"`
			}
		}{}
		out.Body.Summary.Counts = summary.Counts
		if out.Body.Summary.Counts == nil {
			out.Body.Summary.Counts = map[string]int{}
		}
		if summary.CurrentSabaq != nil {
			v := hifzEntryView(*summary.CurrentSabaq)
			out.Body.Summary.CurrentSabaq = &v
		}
		return out, nil
	})
}

// LinkGuardianAccount ties a guardian contact to the login they will sign in
// with — an administrative act (academics:manage), the step that turns a contact
// into a portal user. The account is invited through the ordinary member flow
// with the `guardian` role; this records which guardian record it speaks for.
func registerGuardianLink(api huma.API, schooling *academics.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "link-guardian-account",
		Method:        http.MethodPost,
		Path:          "/v1/students/{id}/guardians/{guardian_id}/account",
		Summary:       "Tie a guardian to the login that reads their child's portal",
		Tags:          []string{"Portal"},
		Security:      []map[string][]string{{"bearer": {}}},
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *struct {
		ID         string `path:"id" format:"uuid"`
		GuardianID string `path:"guardian_id" format:"uuid"`
		Body       struct {
			UserID string `json:"user_id" format:"uuid"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.ID, "student")
		if err != nil {
			return nil, err
		}
		guardianID, err := parseUUID(in.GuardianID, "guardian")
		if err != nil {
			return nil, err
		}
		userID, err := parseUUID(in.Body.UserID, "user")
		if err != nil {
			return nil, err
		}

		// The path says this guardian is that student's, so it has to be: the segment
		// was decorative, and a link made under the name of a child who has nothing to
		// do with it is a mistake that succeeds silently. The caller may manage every
		// student here, so this buys no privilege — it makes the address mean what it
		// says, and a mismatch fail while somebody can still see it.
		guardians, err := schooling.Guardians(ctx, p.TenantID, studentID)
		if err != nil {
			return nil, academicsError(err)
		}
		if !slices.ContainsFunc(guardians, func(g academics.Guardian) bool { return g.ID == guardianID }) {
			return nil, huma.Error404NotFound("That guardian is not on this student's record.")
		}

		if err := schooling.LinkGuardianUser(ctx, p.TenantID, guardianID, userID); err != nil {
			return nil, academicsError(err)
		}
		return nil, nil
	})
}

// portalChild is the ownership gate every per-child read passes through: it needs
// portal:read, and it needs the student to be one of the caller's own. Either
// failing yields the same answer a stranger's student would — not this person's.
func portalChild(ctx context.Context, schooling *academics.Service, rawID string) (auth.Principal, uuid.UUID, error) {
	p, err := requirePermission(ctx, auth.PermPortalRead)
	if err != nil {
		return auth.Principal{}, uuid.Nil, err
	}
	studentID, err := parseUUID(rawID, "student")
	if err != nil {
		return auth.Principal{}, uuid.Nil, err
	}
	if _, err := schooling.ChildStudent(ctx, p.TenantID, p.UserID, studentID); err != nil {
		return auth.Principal{}, uuid.Nil, academicsError(err)
	}
	return p, studentID, nil
}
