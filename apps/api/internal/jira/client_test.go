package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"incidentiq/apps/api/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestClientStatusHidesCredentials(t *testing.T) {
	client := NewClient(config.JIRAConfig{
		Enabled:    true,
		BaseURL:    "https://jira.example.test",
		Username:   "incidentiq",
		APIToken:   "secret-token",
		ProjectKey: "SRE",
	}, nil)

	status := client.Status()
	if !status.Enabled {
		t.Fatalf("Enabled = false, want true")
	}
	if !status.Configured {
		t.Fatalf("Configured = false, want true")
	}
	if status.BaseURL != "https://jira.example.test" {
		t.Fatalf("BaseURL = %q", status.BaseURL)
	}
}

func TestClientTestRejectsDisabledIntegration(t *testing.T) {
	client := NewClient(config.JIRAConfig{}, nil)

	result, err := client.Test(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	if result.Success {
		t.Fatalf("Success = true, want false")
	}
}

func TestClientTestCallsMyselfWithBasicAuth(t *testing.T) {
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/jira/rest/api/2/myself" {
				t.Fatalf("path = %q", r.URL.Path)
			}
			username, password, ok := r.BasicAuth()
			if !ok {
				t.Fatalf("missing basic auth")
			}
			if username != "incidentiq" || password != "secret-token" {
				t.Fatalf("basic auth = %q/%q", username, password)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(`{"name":"incidentiq"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	client := NewClient(config.JIRAConfig{
		Enabled:    true,
		BaseURL:    "https://jira.example.test/jira",
		Username:   "incidentiq",
		APIToken:   "secret-token",
		ProjectKey: "SRE",
	}, httpClient)

	result, err := client.Test(context.Background())
	if err != nil {
		t.Fatalf("Test returned error: %v", err)
	}
	if !result.Success {
		t.Fatalf("Success = false, want true")
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", result.StatusCode)
	}
}

func TestClientTestReturnsJIRAErrorMessage(t *testing.T) {
	httpClient := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(bytes.NewBufferString(`{"errorMessages":["bad credentials"]}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	client := NewClient(config.JIRAConfig{
		Enabled:    true,
		BaseURL:    "https://jira.example.test",
		Username:   "incidentiq",
		APIToken:   "wrong",
		ProjectKey: "SRE",
	}, httpClient)

	result, err := client.Test(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	if result.Success {
		t.Fatalf("Success = true, want false")
	}
	if result.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", result.StatusCode)
	}
	if result.Message != "JIRA returned HTTP 401: bad credentials" {
		t.Fatalf("Message = %q", result.Message)
	}
}

func TestCreateIssuePostsIncidentFields(t *testing.T) {
	var requestBody map[string]any
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s", r.Method)
			}
			if r.URL.Path != "/rest/api/2/issue" {
				t.Fatalf("path = %q", r.URL.Path)
			}
			if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusCreated,
				Body:       io.NopCloser(bytes.NewBufferString(`{"key":"SRE-123","id":"10001"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	client := NewClient(config.JIRAConfig{
		Enabled:    true,
		BaseURL:    "https://jira.example.test",
		Username:   "incidentiq",
		APIToken:   "secret-token",
		ProjectKey: "SRE",
	}, httpClient)

	ref, err := client.CreateIssue(context.Background(), IncidentIssue{
		IncidentID:  "inc-1",
		Title:       "Checkout outage",
		Summary:     "Checkout is unavailable",
		Severity:    "critical",
		Service:     "checkout",
		Environment: "prod",
		Status:      "open",
		StartedAt:   time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CreateIssue returned error: %v", err)
	}
	if ref.Key != "SRE-123" {
		t.Fatalf("Key = %q", ref.Key)
	}

	fields := requestBody["fields"].(map[string]any)
	if fields["summary"] != "Checkout outage" {
		t.Fatalf("summary = %#v", fields["summary"])
	}
	project := fields["project"].(map[string]any)
	if project["key"] != "SRE" {
		t.Fatalf("project key = %#v", project["key"])
	}
	priority := fields["priority"].(map[string]any)
	if priority["name"] != "Highest" {
		t.Fatalf("priority = %#v", priority["name"])
	}
}

func TestUpdateIssuePutsFieldsAndAddsComment(t *testing.T) {
	paths := []string{}
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.Method+" "+r.URL.Path)
			switch r.Method + " " + r.URL.Path {
			case "PUT /rest/api/2/issue/SRE-123":
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Body:       io.NopCloser(bytes.NewBuffer(nil)),
					Header:     make(http.Header),
				}, nil
			case "POST /rest/api/2/issue/SRE-123/comment":
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatalf("decode comment: %v", err)
				}
				if payload["body"] == "" {
					t.Fatalf("comment body was empty")
				}
				return &http.Response{
					StatusCode: http.StatusCreated,
					Body:       io.NopCloser(bytes.NewBufferString(`{"id":"1"}`)),
					Header:     make(http.Header),
				}, nil
			default:
				t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				return nil, nil
			}
		}),
	}

	client := NewClient(config.JIRAConfig{
		Enabled:    true,
		BaseURL:    "https://jira.example.test",
		Username:   "incidentiq",
		APIToken:   "secret-token",
		ProjectKey: "SRE",
	}, httpClient)

	err := client.UpdateIssue(context.Background(), "SRE-123", IncidentIssue{
		IncidentID:  "inc-1",
		Title:       "Checkout outage",
		Summary:     "Resolved",
		Severity:    "high",
		Service:     "checkout",
		Environment: "prod",
		Status:      "resolved",
		StartedAt:   time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("UpdateIssue returned error: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("request count = %d, paths=%v", len(paths), paths)
	}
}

func TestUpdateIssueTransitionsResolvedWithResolution(t *testing.T) {
	paths := []string{}
	var transitionPayload map[string]any
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.Method+" "+r.URL.Path)
			switch r.Method + " " + r.URL.Path {
			case "PUT /rest/api/2/issue/SRE-123":
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Body:       io.NopCloser(bytes.NewBuffer(nil)),
					Header:     make(http.Header),
				}, nil
			case "POST /rest/api/2/issue/SRE-123/comment":
				return &http.Response{
					StatusCode: http.StatusCreated,
					Body:       io.NopCloser(bytes.NewBufferString(`{"id":"1"}`)),
					Header:     make(http.Header),
				}, nil
			case "POST /rest/api/2/issue/SRE-123/transitions":
				if err := json.NewDecoder(r.Body).Decode(&transitionPayload); err != nil {
					t.Fatalf("decode transition: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Body:       io.NopCloser(bytes.NewBuffer(nil)),
					Header:     make(http.Header),
				}, nil
			default:
				t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				return nil, nil
			}
		}),
	}

	resolvedAt := time.Date(2026, 6, 2, 13, 0, 0, 0, time.UTC)
	client := NewClient(config.JIRAConfig{
		Enabled:             true,
		BaseURL:             "https://jira.example.test",
		Username:            "incidentiq",
		APIToken:            "secret-token",
		ProjectKey:          "SRE",
		ResolveTransitionID: "31",
		ResolutionField:     "resolution",
		ResolutionValue:     "Done",
	}, httpClient)

	err := client.UpdateIssue(context.Background(), "SRE-123", IncidentIssue{
		IncidentID:  "inc-1",
		Title:       "Checkout outage",
		Summary:     "Resolved",
		Severity:    "high",
		Service:     "checkout",
		Environment: "prod",
		Status:      "resolved",
		StartedAt:   time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC),
		ResolvedAt:  &resolvedAt,
	})
	if err != nil {
		t.Fatalf("UpdateIssue returned error: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("request count = %d, paths=%v", len(paths), paths)
	}
	transition := transitionPayload["transition"].(map[string]any)
	if transition["id"] != "31" {
		t.Fatalf("transition id = %#v", transition["id"])
	}
	fields := transitionPayload["fields"].(map[string]any)
	resolution := fields["resolution"].(map[string]any)
	if resolution["name"] != "Done" {
		t.Fatalf("resolution name = %#v", resolution["name"])
	}
}
