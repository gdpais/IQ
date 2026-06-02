package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"incidentiq/apps/api/internal/config"
)

type Client struct {
	cfg  config.JIRAConfig
	http *http.Client
}

type Status struct {
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	Configured bool   `json:"configured"`
	BaseURL    string `json:"base_url,omitempty"`
	ProjectKey string `json:"project_key,omitempty"`
}

type TestResult struct {
	Name       string `json:"name"`
	Success    bool   `json:"success"`
	StatusCode int    `json:"status_code,omitempty"`
	Message    string `json:"message"`
}

type IncidentIssue struct {
	IncidentID   string
	Title        string
	Summary      string
	Severity     string
	Service      string
	Environment  string
	Status       string
	StartedAt    time.Time
	ResolvedAt   *time.Time
	DashboardURL string
}

type IssueRef struct {
	Key string `json:"key"`
	ID  string `json:"id,omitempty"`
	URL string `json:"url,omitempty"`
}

func NewClient(cfg config.JIRAConfig, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{cfg: cfg, http: httpClient}
}

func (c *Client) Status() Status {
	return Status{
		Name:       "jira",
		Enabled:    c.cfg.Enabled,
		Configured: c.configured(),
		BaseURL:    c.cfg.BaseURL,
		ProjectKey: c.cfg.ProjectKey,
	}
}

func (c *Client) Test(ctx context.Context) (TestResult, error) {
	if !c.cfg.Enabled {
		return TestResult{Name: "jira", Message: "JIRA integration is disabled"}, fmt.Errorf("JIRA integration is disabled")
	}
	if !c.configured() {
		return TestResult{Name: "jira", Message: "JIRA integration is not fully configured"}, fmt.Errorf("JIRA integration is not fully configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(c.cfg.BaseURL, "/rest/api/2/myself"), nil)
	if err != nil {
		return TestResult{Name: "jira", Message: err.Error()}, err
	}
	req.SetBasicAuth(c.cfg.Username, c.cfg.APIToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return TestResult{Name: "jira", Message: err.Error()}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("JIRA returned HTTP %d", resp.StatusCode)
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if detail := jiraErrorMessage(body); detail != "" {
			message = message + ": " + detail
		}
		return TestResult{Name: "jira", StatusCode: resp.StatusCode, Message: message}, fmt.Errorf("%s", message)
	}

	return TestResult{Name: "jira", Success: true, StatusCode: resp.StatusCode, Message: "JIRA connectivity verified"}, nil
}

func (c *Client) CreateIssue(ctx context.Context, issue IncidentIssue) (IssueRef, error) {
	if err := c.ensureReady(); err != nil {
		return IssueRef{}, err
	}

	payload := map[string]any{
		"fields": map[string]any{
			"project": map[string]any{
				"key": c.cfg.ProjectKey,
			},
			"summary":     issue.Title,
			"description": issueDescription(issue),
			"issuetype": map[string]any{
				"name": "Task",
			},
			"priority": map[string]any{
				"name": priorityForSeverity(issue.Severity),
			},
			"labels": labelsForIssue(issue),
		},
	}

	req, err := c.newJSONRequest(ctx, http.MethodPost, "/rest/api/2/issue", payload)
	if err != nil {
		return IssueRef{}, err
	}
	var out IssueRef
	if err := c.doJSON(req, http.StatusCreated, &out); err != nil {
		return IssueRef{}, err
	}
	if out.URL == "" && out.Key != "" {
		out.URL = joinURL(c.cfg.BaseURL, "/browse/"+out.Key)
	}
	return out, nil
}

func (c *Client) UpdateIssue(ctx context.Context, issueKey string, issue IncidentIssue) error {
	if err := c.ensureReady(); err != nil {
		return err
	}
	if strings.TrimSpace(issueKey) == "" {
		return fmt.Errorf("jira issue key is required")
	}

	payload := map[string]any{
		"fields": map[string]any{
			"summary":     issue.Title,
			"description": issueDescription(issue),
			"priority": map[string]any{
				"name": priorityForSeverity(issue.Severity),
			},
			"labels": labelsForIssue(issue),
		},
	}
	req, err := c.newJSONRequest(ctx, http.MethodPut, "/rest/api/2/issue/"+url.PathEscape(issueKey), payload)
	if err != nil {
		return err
	}
	if err := c.doJSON(req, http.StatusNoContent, nil); err != nil {
		return err
	}

	comment := fmt.Sprintf("IncidentIQ sync: incident %s is %s with severity %s.", issue.IncidentID, issue.Status, issue.Severity)
	if issue.ResolvedAt != nil {
		comment += " Resolved at " + issue.ResolvedAt.UTC().Format(time.RFC3339) + "."
	}
	if err := c.AddComment(ctx, issueKey, comment); err != nil {
		return err
	}
	return c.TransitionIssue(ctx, issueKey, issue)
}

func (c *Client) AddComment(ctx context.Context, issueKey string, body string) error {
	if err := c.ensureReady(); err != nil {
		return err
	}
	if strings.TrimSpace(issueKey) == "" {
		return fmt.Errorf("jira issue key is required")
	}
	req, err := c.newJSONRequest(ctx, http.MethodPost, "/rest/api/2/issue/"+url.PathEscape(issueKey)+"/comment", map[string]any{
		"body": body,
	})
	if err != nil {
		return err
	}
	return c.doJSON(req, http.StatusCreated, nil)
}

func (c *Client) TransitionIssue(ctx context.Context, issueKey string, issue IncidentIssue) error {
	transitionID := c.transitionIDForStatus(issue.Status)
	if transitionID == "" {
		return nil
	}
	payload := map[string]any{
		"transition": map[string]any{
			"id": transitionID,
		},
	}
	if strings.EqualFold(issue.Status, "resolved") && c.cfg.ResolutionField != "" && c.cfg.ResolutionValue != "" {
		payload["fields"] = map[string]any{
			c.cfg.ResolutionField: resolutionFieldValue(c.cfg.ResolutionField, c.cfg.ResolutionValue),
		}
	}
	req, err := c.newJSONRequest(ctx, http.MethodPost, "/rest/api/2/issue/"+url.PathEscape(issueKey)+"/transitions", payload)
	if err != nil {
		return err
	}
	return c.doJSON(req, http.StatusNoContent, nil)
}

func (c *Client) transitionIDForStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "acknowledged":
		return c.cfg.AcknowledgeTransitionID
	case "resolved":
		return c.cfg.ResolveTransitionID
	case "open":
		return c.cfg.ReopenTransitionID
	default:
		return ""
	}
}

func (c *Client) ensureReady() error {
	if !c.cfg.Enabled {
		return fmt.Errorf("JIRA integration is disabled")
	}
	if !c.configured() {
		return fmt.Errorf("JIRA integration is not fully configured")
	}
	return nil
}

func (c *Client) newJSONRequest(ctx context.Context, method string, path string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, joinURL(c.cfg.BaseURL, path), body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.cfg.Username, c.cfg.APIToken)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) doJSON(req *http.Request, wantStatus int, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		message := fmt.Sprintf("JIRA returned HTTP %d", resp.StatusCode)
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if detail := jiraErrorMessage(body); detail != "" {
			message = message + ": " + detail
		}
		return fmt.Errorf("%s", message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) configured() bool {
	return strings.TrimSpace(c.cfg.BaseURL) != "" &&
		strings.TrimSpace(c.cfg.Username) != "" &&
		strings.TrimSpace(c.cfg.APIToken) != "" &&
		strings.TrimSpace(c.cfg.ProjectKey) != ""
}

func joinURL(base string, path string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/") + path
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	return parsed.String()
}

func jiraErrorMessage(body []byte) string {
	var payload struct {
		ErrorMessages []string `json:"errorMessages"`
		Errors        any      `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return strings.TrimSpace(string(body))
	}
	if len(payload.ErrorMessages) > 0 {
		return strings.Join(payload.ErrorMessages, "; ")
	}
	if payload.Errors != nil {
		data, _ := json.Marshal(payload.Errors)
		return string(data)
	}
	return ""
}

func issueDescription(issue IncidentIssue) string {
	lines := []string{
		"IncidentIQ incident: " + issue.IncidentID,
		"Severity: " + issue.Severity,
		"Service: " + issue.Service,
		"Environment: " + issue.Environment,
		"Status: " + issue.Status,
		"Started: " + issue.StartedAt.UTC().Format(time.RFC3339),
		"",
		issue.Summary,
	}
	if issue.ResolvedAt != nil {
		lines = append(lines, "Resolved: "+issue.ResolvedAt.UTC().Format(time.RFC3339))
	}
	if issue.DashboardURL != "" {
		lines = append(lines, "Dashboard: "+issue.DashboardURL)
	}
	return strings.Join(lines, "\n")
}

func labelsForIssue(issue IncidentIssue) []string {
	return []string{
		"incidentiq",
		"incidentiq-service-" + sanitizeLabel(issue.Service),
		"incidentiq-env-" + sanitizeLabel(issue.Environment),
		"incidentiq-severity-" + sanitizeLabel(issue.Severity),
		"incidentiq-status-" + sanitizeLabel(issue.Status),
	}
}

func priorityForSeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return "Highest"
	case "high":
		return "High"
	case "medium":
		return "Medium"
	case "low":
		return "Low"
	default:
		return "Medium"
	}
}

func sanitizeLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer(" ", "-", "_", "-", "/", "-", ".", "-", ":", "-")
	value = replacer.Replace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func resolutionFieldValue(field string, value string) any {
	if strings.EqualFold(field, "resolution") {
		return map[string]any{"name": value}
	}
	return value
}
