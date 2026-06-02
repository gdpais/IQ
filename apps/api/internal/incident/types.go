package incident

import "time"

type Status string

const (
	StatusOpen         Status = "open"
	StatusAcknowledged Status = "acknowledged"
	StatusResolved     Status = "resolved"
)

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

type IncidentListFilter struct {
	Severity      string
	Service       string
	Status        Status
	OwnerTeam     string
	StartedAfter  *time.Time
	StartedBefore *time.Time
	Limit         int
}

type IncidentDetail struct {
	Incident Incident  `json:"incident"`
	Events   []Event   `json:"events"`
	Alerts   []Alert   `json:"alerts"`
	JIRA     *JIRALink `json:"jira,omitempty"`
}

type Event struct {
	ID         string         `json:"id"`
	IncidentID string         `json:"incident_id"`
	Type       string         `json:"type"`
	Actor      string         `json:"actor"`
	Payload    map[string]any `json:"payload"`
	CreatedAt  time.Time      `json:"created_at"`
}

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

type AlertIngestResult struct {
	Incident        Incident `json:"incident"`
	Alert           Alert    `json:"alert"`
	CreatedIncident bool     `json:"created_incident"`
	Correlated      bool     `json:"correlated"`
	Duplicate       bool     `json:"duplicate"`
}

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
