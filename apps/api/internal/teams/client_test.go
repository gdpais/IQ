package teams

import (
	"strings"
	"testing"
	"time"

	"incidentiq/apps/api/internal/incident"
)

func TestBuildPageIncludesRequiredFields(t *testing.T) {
	startedAt := time.Date(2026, 6, 3, 14, 21, 0, 0, time.UTC)
	body, mentions := BuildPage(incident.IncidentDetail{
		Incident: incident.Incident{
			ID:        "inc-1",
			Severity:  "critical",
			Service:   "checkout",
			Status:    incident.StatusOpen,
			StartedAt: startedAt,
			Summary:   "Checkout API error rate is elevated.",
		},
		Alerts: []incident.Alert{
			{Service: "payments"},
		},
		Events: []incident.Event{
			{Type: "incident_created"},
		},
	}, []incident.TeamsRouteRecipient{
		{Type: incident.TeamsRecipientTypeUser, TeamsObjectID: "user-1", DisplayName: "alice"},
		{Type: incident.TeamsRecipientTypeTag, TeamsObjectID: "tag-1", DisplayName: "checkout-oncall"},
	})

	if len(mentions) != 2 {
		t.Fatalf("mentions = %d, want 2", len(mentions))
	}
	for _, want := range []string{"Critical incident opened", "Services: checkout, payments", "Started: 2026-06-03 14:21 UTC", "Issue: Checkout API error rate is elevated."} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %q missing %q", body, want)
		}
	}
}

func TestBuildPageMarksReopenedIncidents(t *testing.T) {
	body, _ := BuildPage(incident.IncidentDetail{
		Incident: incident.Incident{
			ID:        "inc-1",
			Severity:  "high",
			Service:   "checkout",
			Status:    incident.StatusOpen,
			StartedAt: time.Now().UTC(),
		},
		Events: []incident.Event{
			{Type: "incident_reopened"},
		},
	}, nil)

	if !strings.Contains(body, "High incident reopened") {
		t.Fatalf("body = %q, want reopened state", body)
	}
}
