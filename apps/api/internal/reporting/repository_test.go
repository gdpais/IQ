package reporting

import (
	"encoding/json"
	"testing"
	"time"
)

func TestComputeLiveMetrics(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	firstAlert := now.Add(-65 * time.Minute)
	ack := now.Add(-50 * time.Minute)
	resolved := now.Add(-10 * time.Minute)

	metrics := ComputeLiveMetrics([]metricRow{
		{
			ID:                   "inc-1",
			Severity:             "critical",
			Service:              "checkout",
			Environment:          "prod",
			OwnerTeam:            "sre",
			Status:               "resolved",
			StartedAt:            now.Add(-60 * time.Minute),
			AcknowledgedAt:       &ack,
			ResolvedAt:           &resolved,
			FirstAlertObservedAt: &firstAlert,
			AlertCount:           3,
			ReopenCount:          1,
		},
		{
			ID:          "inc-2",
			Severity:    "high",
			Service:     "identity",
			Environment: "prod",
			OwnerTeam:   "platform",
			Status:      "open",
			StartedAt:   now.Add(-3 * time.Hour),
		},
	}, now)

	if metrics.IncidentCount != 2 {
		t.Fatalf("IncidentCount = %d", metrics.IncidentCount)
	}
	if metrics.AlertCount != 3 {
		t.Fatalf("AlertCount = %d", metrics.AlertCount)
	}
	if metrics.IncidentsWithAlerts != 1 {
		t.Fatalf("IncidentsWithAlerts = %d", metrics.IncidentsWithAlerts)
	}
	if metrics.DuplicateSuppressionCount != 2 {
		t.Fatalf("DuplicateSuppressionCount = %d", metrics.DuplicateSuppressionCount)
	}
	if metrics.ReopenRate != 0.5 {
		t.Fatalf("ReopenRate = %f", metrics.ReopenRate)
	}
	if metrics.MeanTimeToDetectSeconds == nil || *metrics.MeanTimeToDetectSeconds != 300 {
		t.Fatalf("MTTD = %#v", metrics.MeanTimeToDetectSeconds)
	}
	if metrics.MeanTimeToAckSeconds == nil || *metrics.MeanTimeToAckSeconds != 600 {
		t.Fatalf("MTTA = %#v", metrics.MeanTimeToAckSeconds)
	}
	if metrics.MeanTimeToResolveSeconds == nil || *metrics.MeanTimeToResolveSeconds != 3000 {
		t.Fatalf("MTTR = %#v", metrics.MeanTimeToResolveSeconds)
	}
	if metrics.SLAAtRiskCount != 1 {
		t.Fatalf("SLAAtRiskCount = %d", metrics.SLAAtRiskCount)
	}
	if metrics.SLABreachCount != 0 {
		t.Fatalf("SLABreachCount = %d", metrics.SLABreachCount)
	}
}

func TestComputeLiveMetricsBreachedSLA(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	metrics := ComputeLiveMetrics([]metricRow{
		{
			ID:          "inc-1",
			Severity:    "critical",
			Service:     "checkout",
			Environment: "prod",
			OwnerTeam:   "sre",
			Status:      "open",
			StartedAt:   now.Add(-2 * time.Hour),
		},
	}, now)

	if metrics.SLABreachCount != 1 {
		t.Fatalf("SLABreachCount = %d", metrics.SLABreachCount)
	}
}

func TestComputeDORAReportWithDeploymentEvents(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	resolved := now.Add(-30 * time.Minute)

	report := ComputeDORAReport([]metricRow{
		{
			ID:                    "inc-1",
			Service:               "checkout",
			Environment:           "prod",
			Status:                "resolved",
			StartedAt:             now.Add(-90 * time.Minute),
			ResolvedAt:            &resolved,
			LinkedDeploymentCount: 1,
		},
		{
			ID:          "inc-2",
			Service:     "identity",
			Environment: "prod",
			Status:      "open",
			StartedAt:   now.Add(-20 * time.Minute),
		},
	}, 4, now)

	if report.DeploymentCount != 4 {
		t.Fatalf("DeploymentCount = %d", report.DeploymentCount)
	}
	if report.DeploymentLinkedIncidents != 1 {
		t.Fatalf("DeploymentLinkedIncidents = %d", report.DeploymentLinkedIncidents)
	}
	if report.ChangeFailureRate != 0.25 {
		t.Fatalf("ChangeFailureRate = %f", report.ChangeFailureRate)
	}
	if report.TimeToRestoreServiceSeconds == nil || *report.TimeToRestoreServiceSeconds != 3600 {
		t.Fatalf("TimeToRestoreServiceSeconds = %#v", report.TimeToRestoreServiceSeconds)
	}
}

func TestIncidentCSVRowsMatchLiveMetricIncidentTotals(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	rows := []metricRow{
		{
			ID:          "inc-1",
			Title:       "Checkout outage",
			Severity:    "critical",
			Service:     "checkout",
			Environment: "prod",
			OwnerTeam:   "sre",
			Status:      "open",
			StartedAt:   now.Add(-1 * time.Hour),
			AlertCount:  2,
		},
		{
			ID:          "inc-2",
			Title:       "Identity latency",
			Severity:    "high",
			Service:     "identity",
			Environment: "prod",
			OwnerTeam:   "platform",
			Status:      "resolved",
			StartedAt:   now.Add(-2 * time.Hour),
		},
	}

	metrics := ComputeLiveMetrics(rows, now)
	csvRows := IncidentCSVRowsFromMetrics(rows)

	if int64(len(csvRows)) != metrics.IncidentCount {
		t.Fatalf("csv rows = %d, metric incident count = %d", len(csvRows), metrics.IncidentCount)
	}
	if csvRows[0].ID != "inc-1" || csvRows[0].AlertCount != 2 {
		t.Fatalf("unexpected csv row: %#v", csvRows[0])
	}
}

func TestSnapshotPayloadUsesReportSpecificShape(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	payload, err := snapshotPayload("dora", []metricRow{
		{
			ID:                    "inc-1",
			StartedAt:             now.Add(-time.Hour),
			LinkedDeploymentCount: 1,
		},
	}, 2, now)
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["deployment_count"].(float64) != 2 {
		t.Fatalf("deployment_count = %#v", decoded["deployment_count"])
	}
	if decoded["change_failure_rate"].(float64) != 0.5 {
		t.Fatalf("change_failure_rate = %#v", decoded["change_failure_rate"])
	}
}
