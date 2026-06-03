package main

import (
	"testing"
	"time"
)

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

func TestComputeDORAReportForSnapshots(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	resolved := now.Add(-15 * time.Minute)
	report := computeDORAReport([]reportMetricRow{
		{
			ID:                    "inc-1",
			StartedAt:             now.Add(-45 * time.Minute),
			ResolvedAt:            &resolved,
			LinkedDeploymentCount: 1,
		},
	}, 5, now)

	if report.DeploymentLinkedIncidents != 1 {
		t.Fatalf("DeploymentLinkedIncidents = %d", report.DeploymentLinkedIncidents)
	}
	if report.ChangeFailureRate != 0.2 {
		t.Fatalf("ChangeFailureRate = %f", report.ChangeFailureRate)
	}
	if report.TimeToRestoreServiceSeconds == nil || *report.TimeToRestoreServiceSeconds != 1800 {
		t.Fatalf("TimeToRestoreServiceSeconds = %#v", report.TimeToRestoreServiceSeconds)
	}
}

func TestComputeLiveMetricsForSnapshots(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	firstAlert := now.Add(-70 * time.Minute)
	ack := now.Add(-50 * time.Minute)
	metrics := computeLiveMetrics([]reportMetricRow{
		{
			ID:                   "inc-1",
			Severity:             "critical",
			Service:              "checkout",
			Environment:          "prod",
			OwnerTeam:            "sre",
			Status:               "acknowledged",
			StartedAt:            now.Add(-60 * time.Minute),
			AcknowledgedAt:       &ack,
			FirstAlertObservedAt: &firstAlert,
			AlertCount:           3,
		},
	}, now)

	if metrics.IncidentCount != 1 || metrics.AlertCount != 3 {
		t.Fatalf("metrics = %#v", metrics)
	}
	if metrics.DuplicateSuppressionCount != 2 {
		t.Fatalf("DuplicateSuppressionCount = %d", metrics.DuplicateSuppressionCount)
	}
	if metrics.MeanTimeToDetectSeconds == nil || *metrics.MeanTimeToDetectSeconds != 600 {
		t.Fatalf("MTTD = %#v", metrics.MeanTimeToDetectSeconds)
	}
}

func TestIntegrationEventWithPayload(t *testing.T) {
	event := integrationEvent{ID: "evt-1", Payload: map[string]any{"a": "b"}}
	next := integrationEventWithPayload(event, map[string]any{"message_id": "m-1"})
	if next.ID != "evt-1" {
		t.Fatalf("event ID changed: %q", next.ID)
	}
	if next.Payload["message_id"] != "m-1" {
		t.Fatalf("payload not replaced: %#v", next.Payload)
	}
}
