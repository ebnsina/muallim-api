package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/automation"
)

// AutomationRuleView is one "when this happens, send that".
type AutomationRuleView struct {
	ID      string `json:"id" format:"uuid"`
	Event   string `json:"event" enum:"learner.enrolled,course.completed"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Enabled bool   `json:"enabled"`
}

// AutomationEventView is an event a rule may fire on, and the placeholders its
// templates may use. The chooser is built from this rather than from a list the
// client keeps: a client guessing at placeholders writes rules that render blank.
type AutomationEventView struct {
	Event        string   `json:"event"`
	Placeholders []string `json:"placeholders"`
}

func automationRuleView(r automation.Rule) AutomationRuleView {
	return AutomationRuleView{
		ID: r.ID.String(), Event: r.Event, Subject: r.Subject,
		Body: r.Body, Enabled: r.Enabled,
	}
}

// automationError maps this domain's sentinels. Every sentinel needs a case here,
// or it renders as a 500; errors_test.go asserts that.
func automationError(err error) error {
	switch {
	case errors.Is(err, automation.ErrNotFound):
		return huma.Error404NotFound("That automation no longer exists.")
	case errors.Is(err, automation.ErrInvalid):
		// Deliberately not the domain's own sentence: it is wrapped in a package
		// prefix nobody reads. The placeholders an event offers are on
		// /v1/automations/events, which is where the page gets them.
		return huma.Error422UnprocessableEntity(
			"Check the automation: it needs a subject and a message, and can only use the placeholders this event offers.")
	default:
		return err
	}
}

func registerAutomations(api huma.API, svc *automation.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-automation-events",
		Method:      http.MethodGet,
		Path:        "/v1/automations/events",
		Summary:     "The events an automation may fire on, and what each offers a template",
		Description: "The chooser is built from this: a placeholder not listed for an event " +
			"is refused when the rule is saved.",
		Tags:     []string{"Automations"},
		Security: admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Events []AutomationEventView `json:"events"`
		}
	}, error) {
		if _, err := requirePermission(ctx, auth.PermTenantManage); err != nil {
			return nil, err
		}
		out := &struct {
			Body struct {
				Events []AutomationEventView `json:"events"`
			}
		}{}
		for _, event := range automation.Events() {
			out.Body.Events = append(out.Body.Events, AutomationEventView{
				Event: event, Placeholders: automation.PlaceholdersFor(event),
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-automations",
		Method:      http.MethodGet,
		Path:        "/v1/automations",
		Summary:     "A workspace's automations, newest first",
		Description: "Bounded, not paginated: these are written by hand, a few per event.",
		Tags:        []string{"Automations"},
		Security:    admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Rules []AutomationRuleView `json:"rules"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermTenantManage)
		if err != nil {
			return nil, err
		}
		rules, err := svc.List(ctx, p.TenantID)
		if err != nil {
			return nil, automationError(err)
		}
		out := &struct {
			Body struct {
				Rules []AutomationRuleView `json:"rules"`
			}
		}{}
		out.Body.Rules = make([]AutomationRuleView, 0, len(rules))
		for _, rule := range rules {
			out.Body.Rules = append(out.Body.Rules, automationRuleView(rule))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-automation",
		Method:        http.MethodPost,
		Path:          "/v1/automations",
		Summary:       "Write an automation",
		Description:   "Off unless `enabled` says otherwise: a rule being drafted must not fire at the next person who enrols.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Automations"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Event   string `json:"event" enum:"learner.enrolled,course.completed"`
			Subject string `json:"subject" minLength:"1" maxLength:"300"`
			Body    string `json:"body" minLength:"1" maxLength:"5000"`
			Enabled bool   `json:"enabled,omitempty"`
		}
	}) (*struct {
		Body struct {
			Rule AutomationRuleView `json:"rule"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermTenantManage)
		if err != nil {
			return nil, err
		}
		rule, err := svc.Create(ctx, p.TenantID, automation.NewRule{
			Event: in.Body.Event, Subject: in.Body.Subject,
			Body: in.Body.Body, Enabled: in.Body.Enabled,
		}, automation.Author{UserID: p.UserID})
		if err != nil {
			return nil, automationError(err)
		}
		out := &struct {
			Body struct {
				Rule AutomationRuleView `json:"rule"`
			}
		}{}
		out.Body.Rule = automationRuleView(rule)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-automation",
		Method:      http.MethodPut,
		Path:        "/v1/automations/{id}",
		Summary:     "Edit an automation, or switch it on and off",
		Description: "The event is fixed when the rule is written: one that fires on something else is a different rule.",
		Tags:        []string{"Automations"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Subject *string `json:"subject,omitempty" maxLength:"300"`
			Body    *string `json:"body,omitempty" maxLength:"5000"`
			Enabled *bool   `json:"enabled,omitempty"`
		}
	}) (*struct {
		Body struct {
			Rule AutomationRuleView `json:"rule"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermTenantManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "automation")
		if err != nil {
			return nil, err
		}
		rule, err := svc.Update(ctx, p.TenantID, id, automation.RulePatch{
			Subject: in.Body.Subject, Body: in.Body.Body, Enabled: in.Body.Enabled,
		}, automation.Author{UserID: p.UserID})
		if err != nil {
			return nil, automationError(err)
		}
		out := &struct {
			Body struct {
				Rule AutomationRuleView `json:"rule"`
			}
		}{}
		out.Body.Rule = automationRuleView(rule)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-automation",
		Method:        http.MethodDelete,
		Path:          "/v1/automations/{id}",
		Summary:       "Delete an automation",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Automations"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermTenantManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "automation")
		if err != nil {
			return nil, err
		}
		if err := svc.Delete(ctx, p.TenantID, id, automation.Author{UserID: p.UserID}); err != nil {
			return nil, automationError(err)
		}
		return nil, nil
	})
}
