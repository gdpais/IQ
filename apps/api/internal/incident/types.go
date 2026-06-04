// Package incident defines the core domain types, lifecycle rules, and database
// repository for incidents, alerts, and related integration state.
package incident

import (
	"fmt"
	"time"
)

// Status represents the lifecycle state of an incident.
type Status string

const (
	StatusOpen         Status = "open"
	StatusAcknowledged Status = "acknowledged"
	StatusResolved     Status = "resolved"
)

// ValidateTransition checks whether moving an incident from the current status
// to the target status is a legal lifecycle step. It returns nil on success and
// a descriptive error on invalid transitions.
func ValidateTransition(from Status, to Status) error {
	switch to {
	case StatusAcknowledged:
		if from != StatusOpen {
			return fmt.Errorf("invalid transition from %s to %s: must be open first", from, to)
		}
	case StatusResolved:
		if from != StatusAcknowledged && from != StatusOpen {
			return fmt.Errorf("invalid transition from %s to %s: must be open or acknowledged", from, to)
		}
	case StatusOpen:
		if from != StatusResolved {
			return fmt.Errorf("invalid transition from %s to %s: can only reopen resolved incidents", from, to)
		}
	default:
		return fmt.Errorf("unknown target status %q", to)
	}
	return nil
}

// Incident is the central domain object representing a production incident.
type Incident struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Summary        string     `json:"summary"`
	Severity       string     `json:"severity"`
	Service        string     `json:"service"`
	Environment    string     `json:"environment"`
	OwnerTeam      string     `json:"owner_team"`
	Status         Status     `json:"status"`
	StartedAt      time.Time  `json:"started_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// IncidentListFilter constrains the results returned by Repository.List.
// Zero values are ignored (no filter applied).
type IncidentListFilter struct {
	Severity      string
	Service       string
	Status        Status
	OwnerTeam     string
	StartedAfter  *time.Time
	StartedBefore *time.Time
	Limit         int
}

// IncidentDetail is the full view of an incident including its timeline events,
// correlated alerts, and optional Jira issue link.
type IncidentDetail struct {
	Incident Incident  `json:"incident"`
	Events   []Event   `json:"events"`
	Alerts   []Alert   `json:"alerts"`
	JIRA     *JIRALink `json:"jira,omitempty"`
}

// Event records a discrete action or state change on an incident (e.g.
// "incident_acknowledged", "incident_resolved"). It forms the audit trail shown
// in the detail view.
type Event struct {
	ID         string         `json:"id"`
	IncidentID string         `json:"incident_id"`
	Type       string         `json:"type"`
	Actor      string         `json:"actor"`
	Payload    map[string]any `json:"payload"`
	CreatedAt  time.Time      `json:"created_at"`
}

// Alert is a normalized inbound signal from an external monitoring source. An
// alert may create a new incident or be correlated to an existing one.
// RedactedPayload strips sensitive keys before persisting to the database.
type Alert struct {
	ID              string         `json:"id"`
	IncidentID      *string        `json:"incident_id,omitempty"`
	Source          string         `json:"source"`
	SourceEventID   string         `json:"source_event_id"`
	Fingerprint     string         `json:"fingerprint"`
	Service         string         `json:"service"`
	Environment     string         `json:"environment"`
	Severity        string         `json:"severity"`
	RedactedPayload map[string]any `json:"redacted_payload"`
	ObservedAt      time.Time      `json:"observed_at"`
	CreatedAt       time.Time      `json:"created_at"`
}

// NormalizedAlert is the source-agnostic representation produced by
// NormalizeAlert before the alert is persisted or matched against existing
// incidents.
type NormalizedAlert struct {
	Source          string         `json:"source"`
	SourceEventID   string         `json:"source_event_id"`
	Fingerprint     string         `json:"fingerprint"`
	Title           string         `json:"title"`
	Summary         string         `json:"summary"`
	Service         string         `json:"service"`
	Environment     string         `json:"environment"`
	Severity        string         `json:"severity"`
	Payload         map[string]any `json:"payload"`
	RedactedPayload map[string]any `json:"redacted_payload"`
	ObservedAt      time.Time      `json:"observed_at"`
}

// AlertIngestResult summarises what happened when an alert was ingested.
// Exactly one of CreatedIncident, Correlated, or Duplicate will be true.
type AlertIngestResult struct {
	Incident        Incident `json:"incident"`
	Alert           Alert    `json:"alert"`
	CreatedIncident bool     `json:"created_incident"`
	Correlated      bool     `json:"correlated"`
	Duplicate       bool     `json:"duplicate"`
}

// JIRALink records the mapping between an IncidentIQ incident and its
// corresponding Jira issue. JIRAIssueID may be nil for links created before the
// ID field was introduced.
type JIRALink struct {
	IncidentID   string    `json:"incident_id"`
	JIRAIssueKey string    `json:"jira_issue_key"`
	JIRAIssueID  *string   `json:"jira_issue_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type CreateIncidentRequest struct {
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Severity    string `json:"severity"`
	Service     string `json:"service"`
	Environment string `json:"environment"`
	OwnerTeam   string `json:"owner_team"`
	Actor       string `json:"actor"`
}

type PatchIncidentRequest struct {
	Title     *string `json:"title"`
	Summary   *string `json:"summary"`
	Severity  *string `json:"severity"`
	OwnerTeam *string `json:"owner_team"`
	Actor     string  `json:"actor"`
}

type AddEventRequest struct {
	Type    string         `json:"type"`
	Actor   string         `json:"actor"`
	Payload map[string]any `json:"payload"`
}
