package main

import "testing"

func TestPriorityForSeverity(t *testing.T) {
	tests := map[string]string{
		"critical": "Highest",
		"high":     "High",
		"medium":   "Medium",
		"low":      "Low",
		"unknown":  "Medium",
	}
	for input, want := range tests {
		if got := priorityForSeverity(input); got != want {
			t.Fatalf("priorityForSeverity(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLabelsForIncident(t *testing.T) {
	labels := labelsForIncident(incident{
		Service:     "Checkout API",
		Environment: "Prod",
		Severity:    "Critical",
		Status:      "Open",
	})

	want := []string{
		"incidentiq",
		"incidentiq-service-checkout-api",
		"incidentiq-env-prod",
		"incidentiq-severity-critical",
		"incidentiq-status-open",
	}
	for i, label := range want {
		if labels[i] != label {
			t.Fatalf("labels[%d] = %q, want %q", i, labels[i], label)
		}
	}
}

func TestJIRAErrorMessage(t *testing.T) {
	got := jiraErrorMessage([]byte(`{"errorMessages":["bad credentials"]}`))
	if got != "bad credentials" {
		t.Fatalf("jiraErrorMessage = %q", got)
	}
}

func TestTransitionIDForStatus(t *testing.T) {
	w := worker{
		jira: jiraConfig{
			AcknowledgeTransitionID: "11",
			ResolveTransitionID:     "21",
			ReopenTransitionID:      "31",
		},
	}
	if got := w.transitionIDForStatus("acknowledged"); got != "11" {
		t.Fatalf("ack transition = %q", got)
	}
	if got := w.transitionIDForStatus("resolved"); got != "21" {
		t.Fatalf("resolve transition = %q", got)
	}
	if got := w.transitionIDForStatus("open"); got != "31" {
		t.Fatalf("reopen transition = %q", got)
	}
}

func TestResolutionFieldValue(t *testing.T) {
	systemValue, ok := resolutionFieldValue("resolution", "Done").(map[string]any)
	if !ok {
		t.Fatalf("system resolution did not use object value")
	}
	if systemValue["name"] != "Done" {
		t.Fatalf("resolution name = %#v", systemValue["name"])
	}
	if custom := resolutionFieldValue("customfield_123", "Resolved by IncidentIQ"); custom != "Resolved by IncidentIQ" {
		t.Fatalf("custom resolution field = %#v", custom)
	}
}
