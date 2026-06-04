// Package reporting defines the data types and query logic for SRE, DORA, and
// executive reports as well as deployment ingestion and snapshotting.
package reporting

import (
	"encoding/json"
	"time"
)

// Filter constrains all reporting queries. Zero-value fields are ignored.
type Filter struct {
	Severity    string     `json:"severity"`
	Service     string     `json:"service"`
	Environment string     `json:"environment"`
	OwnerTeam   string     `json:"owner_team"`
	Status      string     `json:"status"`
	From        *time.Time `json:"from"`
	To          *time.Time `json:"to"`
}

// LiveMetrics provides real-time operational statistics for the dashboard. MTTD,
// MTTA, and MTTR are omitted from the JSON response when no data is available.
type LiveMetrics struct {
	IncidentCount             int64            `json:"incident_count"`
	OpenIncidentCount         int64            `json:"open_incident_count"`
	AcknowledgedIncidentCount int64            `json:"acknowledged_incident_count"`
	ResolvedIncidentCount     int64            `json:"resolved_incident_count"`
	AlertCount                int64            `json:"alert_count"`
	IncidentsWithAlerts       int64            `json:"incidents_with_alerts"`
	AlertToIncidentConversion float64          `json:"alert_to_incident_conversion"`
	DuplicateSuppressionCount int64            `json:"duplicate_suppression_count"`
	ReopenCount               int64            `json:"reopen_count"`
	ReopenRate                float64          `json:"reopen_rate"`
	MeanTimeToDetectSeconds   *float64         `json:"mttd_seconds,omitempty"`
	MeanTimeToAckSeconds      *float64         `json:"mtta_seconds,omitempty"`
	MeanTimeToResolveSeconds  *float64         `json:"mttr_seconds,omitempty"`
	SLAAtRiskCount            int64            `json:"sla_at_risk_count"`
	SLABreachCount            int64            `json:"sla_breach_count"`
	CountsBySeverity          map[string]int64 `json:"counts_by_severity"`
	CountsByService           map[string]int64 `json:"counts_by_service"`
	CountsByEnvironment       map[string]int64 `json:"counts_by_environment"`
	CountsByTeam              map[string]int64 `json:"counts_by_team"`
	CountsByStatus            map[string]int64 `json:"counts_by_status"`
}

type SREReport struct {
	LiveMetrics
	GeneratedAt time.Time `json:"generated_at"`
}

// DORAReport exposes DORA (DevOps Research and Assessment) metrics derived from
// deployments and linked incidents. ChangeFailureRate is the fraction of
// deployments that coincided with an incident.
type DORAReport struct {
	DeploymentCount             int64     `json:"deployment_count"`
	DeploymentLinkedIncidents   int64     `json:"deployment_linked_incidents"`
	ChangeFailureRate           float64   `json:"change_failure_rate"`
	TimeToRestoreServiceSeconds *float64  `json:"time_to_restore_service_seconds,omitempty"`
	GeneratedAt                 time.Time `json:"generated_at"`
}

type ExecutiveReport struct {
	IncidentCount       int64            `json:"incident_count"`
	DowntimeSeconds     float64          `json:"downtime_seconds"`
	SLAAtRiskCount      int64            `json:"sla_at_risk_count"`
	SLABreachCount      int64            `json:"sla_breach_count"`
	SeverityTrends      map[string]int64 `json:"severity_trends"`
	ServiceReliability  map[string]int64 `json:"service_reliability"`
	BusinessImpactNotes []string         `json:"business_impact_notes"`
	GeneratedAt         time.Time        `json:"generated_at"`
}

type DeploymentRequest struct {
	Service     string         `json:"service"`
	Environment string         `json:"environment"`
	Version     string         `json:"version"`
	DeployedAt  *time.Time     `json:"deployed_at"`
	Source      string         `json:"source"`
	Metadata    map[string]any `json:"metadata"`
}

type Deployment struct {
	ID          string         `json:"id"`
	IncidentID  *string        `json:"incident_id,omitempty"`
	Service     string         `json:"service"`
	Environment string         `json:"environment"`
	Version     string         `json:"version"`
	DeployedAt  time.Time      `json:"deployed_at"`
	Source      string         `json:"source"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   time.Time      `json:"created_at"`
}

type IncidentCSVRow struct {
	ID             string
	Title          string
	Severity       string
	Service        string
	Environment    string
	OwnerTeam      string
	Status         string
	StartedAt      time.Time
	AcknowledgedAt *time.Time
	ResolvedAt     *time.Time
	AlertCount     int64
	ReopenCount    int64
}

type SnapshotRequest struct {
	ReportTypes []string `json:"report_types"`
	Filter      Filter   `json:"filter"`
}

// ReportSnapshot is a materialised point-in-time copy of a report stored in
// the database. Payload is kept as raw JSON so any report shape can be stored
// without schema changes.
type ReportSnapshot struct {
	ID         string          `json:"id"`
	ReportType string          `json:"report_type"`
	Dimensions map[string]any  `json:"dimensions"`
	ComputedAt time.Time       `json:"computed_at"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}
