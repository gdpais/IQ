package tickettemplate

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var allowedVariables = map[string]bool{
	"severity":      true,
	"service":       true,
	"environment":   true,
	"incident_id":   true,
	"started_at":    true,
	"summary":       true,
	"alert_count":   true,
	"dashboard_url": true,
}

var variablePattern = regexp.MustCompile(`{{\s*([a-zA-Z0-9_]+)\s*}}`)

type Document struct {
	Version   int        `json:"version" yaml:"version"`
	Defaults  Fields     `json:"defaults" yaml:"defaults"`
	Overrides []Override `json:"overrides,omitempty" yaml:"overrides"`
}

type Override struct {
	Scope    Scope  `json:"scope" yaml:"scope"`
	Template Fields `json:"template" yaml:"template"`
}

type Scope struct {
	Type  string `json:"type" yaml:"type"`
	Value string `json:"value" yaml:"value"`
}

type Fields struct {
	ProjectKey   string            `json:"project_key,omitempty" yaml:"project_key"`
	IssueType    string            `json:"issue_type,omitempty" yaml:"issue_type"`
	Title        string            `json:"title,omitempty" yaml:"title"`
	Description  string            `json:"description,omitempty" yaml:"description"`
	Labels       []string          `json:"labels,omitempty" yaml:"labels"`
	Priority     map[string]string `json:"priority,omitempty" yaml:"priority"`
	Components   []string          `json:"components,omitempty" yaml:"components"`
	CustomFields map[string]string `json:"custom_fields,omitempty" yaml:"custom_fields"`
	Comments     []string          `json:"comments,omitempty" yaml:"comments"`
}

type Context struct {
	Severity     string `json:"severity"`
	Service      string `json:"service"`
	Environment  string `json:"environment"`
	IncidentID   string `json:"incident_id"`
	StartedAt    string `json:"started_at"`
	Summary      string `json:"summary"`
	AlertCount   int    `json:"alert_count"`
	DashboardURL string `json:"dashboard_url"`
	Team         string `json:"team,omitempty"`
}

type Rendered struct {
	ProjectKey   string            `json:"project_key,omitempty"`
	IssueType    string            `json:"issue_type,omitempty"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Labels       []string          `json:"labels"`
	Priority     string            `json:"priority,omitempty"`
	Components   []string          `json:"components,omitempty"`
	CustomFields map[string]string `json:"custom_fields,omitempty"`
	Comments     []string          `json:"comments,omitempty"`
}

func Parse(raw string) (Document, error) {
	var doc Document
	if strings.TrimSpace(raw) == "" {
		return Document{}, fmt.Errorf("template_yaml is required")
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func Validate(raw string) (Document, []string, error) {
	doc, err := Parse(raw)
	if err != nil {
		return Document{}, nil, err
	}

	errors := []string{}
	if doc.Version <= 0 {
		errors = append(errors, "version must be greater than 0")
	}
	if strings.TrimSpace(doc.Defaults.Title) == "" {
		errors = append(errors, "defaults.title is required")
	}
	if strings.TrimSpace(doc.Defaults.Description) == "" {
		errors = append(errors, "defaults.description is required")
	}
	errors = append(errors, validateFields("defaults", doc.Defaults)...)

	for i, override := range doc.Overrides {
		prefix := fmt.Sprintf("overrides[%d]", i)
		if !validScopeType(override.Scope.Type) {
			errors = append(errors, prefix+".scope.type must be service, team, or severity")
		}
		if strings.TrimSpace(override.Scope.Value) == "" {
			errors = append(errors, prefix+".scope.value is required")
		}
		errors = append(errors, validateFields(prefix+".template", override.Template)...)
	}

	if len(errors) > 0 {
		return doc, errors, nil
	}
	return doc, nil, nil
}

func Render(raw string, ctx Context) (Rendered, []string, error) {
	doc, validationErrors, err := Validate(raw)
	if err != nil || len(validationErrors) > 0 {
		return Rendered{}, validationErrors, err
	}
	if ctx.StartedAt == "" {
		ctx.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}

	fields := doc.Defaults
	for _, override := range doc.Overrides {
		if scopeMatches(override.Scope, ctx) {
			fields = mergeFields(fields, override.Template)
		}
	}

	vars := variableValues(ctx)
	rendered := Rendered{
		ProjectKey:   replaceVariables(fields.ProjectKey, vars),
		IssueType:    replaceVariables(fields.IssueType, vars),
		Title:        replaceVariables(fields.Title, vars),
		Description:  replaceVariables(fields.Description, vars),
		Labels:       replaceList(fields.Labels, vars),
		Components:   replaceList(fields.Components, vars),
		CustomFields: replaceMap(fields.CustomFields, vars),
		Comments:     replaceList(fields.Comments, vars),
	}
	if fields.Priority != nil {
		rendered.Priority = fields.Priority[strings.ToLower(ctx.Severity)]
		if rendered.Priority == "" {
			rendered.Priority = fields.Priority["default"]
		}
	}
	return rendered, nil, nil
}

func validateFields(prefix string, fields Fields) []string {
	errors := []string{}
	for _, item := range fieldStrings(fields) {
		for _, match := range variablePattern.FindAllStringSubmatch(item.value, -1) {
			name := match[1]
			if !allowedVariables[name] {
				errors = append(errors, fmt.Sprintf("%s.%s uses unknown variable %q", prefix, item.name, name))
			}
		}
	}
	return errors
}

type namedString struct {
	name  string
	value string
}

func fieldStrings(fields Fields) []namedString {
	items := []namedString{
		{name: "project_key", value: fields.ProjectKey},
		{name: "issue_type", value: fields.IssueType},
		{name: "title", value: fields.Title},
		{name: "description", value: fields.Description},
	}
	for i, label := range fields.Labels {
		items = append(items, namedString{name: fmt.Sprintf("labels[%d]", i), value: label})
	}
	for key, value := range fields.Priority {
		items = append(items, namedString{name: "priority." + key, value: value})
	}
	for i, component := range fields.Components {
		items = append(items, namedString{name: fmt.Sprintf("components[%d]", i), value: component})
	}
	for key, value := range fields.CustomFields {
		items = append(items, namedString{name: "custom_fields." + key, value: value})
	}
	for i, comment := range fields.Comments {
		items = append(items, namedString{name: fmt.Sprintf("comments[%d]", i), value: comment})
	}
	return items
}

func validScopeType(scopeType string) bool {
	switch strings.ToLower(strings.TrimSpace(scopeType)) {
	case "service", "team", "severity":
		return true
	default:
		return false
	}
}

func scopeMatches(scope Scope, ctx Context) bool {
	value := strings.ToLower(strings.TrimSpace(scope.Value))
	switch strings.ToLower(strings.TrimSpace(scope.Type)) {
	case "service":
		return strings.ToLower(ctx.Service) == value
	case "team":
		return strings.ToLower(ctx.Team) == value
	case "severity":
		return strings.ToLower(ctx.Severity) == value
	default:
		return false
	}
}

func mergeFields(base Fields, override Fields) Fields {
	if override.ProjectKey != "" {
		base.ProjectKey = override.ProjectKey
	}
	if override.IssueType != "" {
		base.IssueType = override.IssueType
	}
	if override.Title != "" {
		base.Title = override.Title
	}
	if override.Description != "" {
		base.Description = override.Description
	}
	if len(override.Labels) > 0 {
		base.Labels = override.Labels
	}
	if len(override.Priority) > 0 {
		base.Priority = override.Priority
	}
	if len(override.Components) > 0 {
		base.Components = override.Components
	}
	if len(override.CustomFields) > 0 {
		base.CustomFields = override.CustomFields
	}
	if len(override.Comments) > 0 {
		base.Comments = override.Comments
	}
	return base
}

func variableValues(ctx Context) map[string]string {
	return map[string]string{
		"severity":      ctx.Severity,
		"service":       ctx.Service,
		"environment":   ctx.Environment,
		"incident_id":   ctx.IncidentID,
		"started_at":    ctx.StartedAt,
		"summary":       ctx.Summary,
		"alert_count":   fmt.Sprintf("%d", ctx.AlertCount),
		"dashboard_url": ctx.DashboardURL,
	}
}

func replaceVariables(value string, vars map[string]string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := variablePattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		return vars[parts[1]]
	})
}

func replaceList(items []string, vars map[string]string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, replaceVariables(item, vars))
	}
	return out
}

func replaceMap(items map[string]string, vars map[string]string) map[string]string {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]string, len(items))
	for key, value := range items {
		out[key] = replaceVariables(value, vars)
	}
	return out
}
