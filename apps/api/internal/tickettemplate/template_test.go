package tickettemplate

import "testing"

const validTemplate = `
version: 1
defaults:
  project_key: SRE
  issue_type: Task
  title: "{{severity}} incident on {{service}}"
  description: "{{summary}} started at {{started_at}}"
  labels:
    - incidentiq
    - "{{service}}"
  priority:
    critical: Highest
    high: High
    default: Medium
  components:
    - "{{environment}}"
  custom_fields:
    customfield_10010: "{{dashboard_url}}"
  comments:
    - "Alert count: {{alert_count}}"
overrides:
  - scope:
      type: service
      value: checkout
    template:
      labels:
        - checkout
        - "{{severity}}"
  - scope:
      type: severity
      value: critical
    template:
      issue_type: Incident
`

func TestValidateAcceptsTemplate(t *testing.T) {
	doc, errors, err := Validate(validTemplate)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if len(errors) != 0 {
		t.Fatalf("validation errors = %#v", errors)
	}
	if doc.Version != 1 {
		t.Fatalf("version = %d", doc.Version)
	}
}

func TestValidateRejectsUnknownVariable(t *testing.T) {
	raw := `
version: 1
defaults:
  title: "{{unknown}}"
  description: "{{summary}}"
`
	_, errors, err := Validate(raw)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if len(errors) == 0 {
		t.Fatalf("expected validation errors")
	}
}

func TestValidateRejectsInvalidOverrideScope(t *testing.T) {
	raw := `
version: 1
defaults:
  title: "{{summary}}"
  description: "{{summary}}"
overrides:
  - scope:
      type: script
      value: nope
    template:
      title: nope
`
	_, errors, err := Validate(raw)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if len(errors) == 0 {
		t.Fatalf("expected validation errors")
	}
}

func TestRenderAppliesMatchingOverrides(t *testing.T) {
	rendered, errors, err := Render(validTemplate, Context{
		Severity:     "critical",
		Service:      "checkout",
		Environment:  "prod",
		IncidentID:   "inc-123",
		StartedAt:    "2026-06-02T12:00:00Z",
		Summary:      "Checkout is unavailable",
		AlertCount:   3,
		DashboardURL: "https://dash.example/inc-123",
	})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if len(errors) != 0 {
		t.Fatalf("validation errors = %#v", errors)
	}
	if rendered.Title != "critical incident on checkout" {
		t.Fatalf("title = %q", rendered.Title)
	}
	if rendered.IssueType != "Incident" {
		t.Fatalf("issue type = %q", rendered.IssueType)
	}
	if rendered.Priority != "Highest" {
		t.Fatalf("priority = %q", rendered.Priority)
	}
	if len(rendered.Labels) != 2 || rendered.Labels[0] != "checkout" || rendered.Labels[1] != "critical" {
		t.Fatalf("labels = %#v", rendered.Labels)
	}
	if rendered.CustomFields["customfield_10010"] != "https://dash.example/inc-123" {
		t.Fatalf("custom field = %#v", rendered.CustomFields)
	}
}
