package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	maxAttempts        = 5
	batchSize          = 10
	lockTTL            = 2 * time.Minute
	pollInterval       = 5 * time.Second
	defaultHTTPTimeout = 10 * time.Second
)

type config struct {
	DBURL                  string
	RedisAddr              string
	RedisDB                int
	ReportSnapshotInterval time.Duration
	JIRA                   jiraConfig
}

type jiraConfig struct {
	Enabled                 bool
	BaseURL                 string
	Username                string
	APIToken                string
	ProjectKey              string
	AcknowledgeTransitionID string
	ResolveTransitionID     string
	ReopenTransitionID      string
	ResolutionField         string
	ResolutionValue         string
}

type integrationEvent struct {
	ID       string
	Type     string
	Payload  map[string]any
	Attempts int
}

type incident struct {
	ID             string
	Title          string
	Summary        string
	Severity       string
	Service        string
	Environment    string
	Status         string
	StartedAt      time.Time
	AcknowledgedAt *time.Time
	ResolvedAt     *time.Time
}

type reportMetricRow struct {
	ID                    string
	Severity              string
	Service               string
	Environment           string
	OwnerTeam             string
	Status                string
	StartedAt             time.Time
	AcknowledgedAt        *time.Time
	ResolvedAt            *time.Time
	FirstAlertObservedAt  *time.Time
	AlertCount            int64
	ReopenCount           int64
	LinkedDeploymentCount int64
}

type liveMetrics struct {
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

type sreReport struct {
	liveMetrics
	GeneratedAt time.Time `json:"generated_at"`
}

type doraReport struct {
	DeploymentCount             int64     `json:"deployment_count"`
	DeploymentLinkedIncidents   int64     `json:"deployment_linked_incidents"`
	ChangeFailureRate           float64   `json:"change_failure_rate"`
	TimeToRestoreServiceSeconds *float64  `json:"time_to_restore_service_seconds,omitempty"`
	GeneratedAt                 time.Time `json:"generated_at"`
}

type executiveReport struct {
	IncidentCount       int64            `json:"incident_count"`
	DowntimeSeconds     float64          `json:"downtime_seconds"`
	SLAAtRiskCount      int64            `json:"sla_at_risk_count"`
	SLABreachCount      int64            `json:"sla_breach_count"`
	SeverityTrends      map[string]int64 `json:"severity_trends"`
	ServiceReliability  map[string]int64 `json:"service_reliability"`
	BusinessImpactNotes []string         `json:"business_impact_notes"`
	GeneratedAt         time.Time        `json:"generated_at"`
}

type jiraLink struct {
	IssueKey string
	IssueID  *string
}

type jiraIssueRef struct {
	Key string `json:"key"`
	ID  string `json:"id,omitempty"`
}

type worker struct {
	db    *pgxpool.Pool
	redis *redis.Client
	jira  jiraConfig
	http  *http.Client
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, cfg.DBURL)
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
	}
	defer db.Close()

	redisClient := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
		DB:   cfg.RedisDB,
	})
	defer redisClient.Close()

	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping failed: %v", err)
	}

	w := worker{
		db:    db,
		redis: redisClient,
		jira:  cfg.JIRA,
		http:  &http.Client{Timeout: defaultHTTPTimeout},
	}

	if cfg.ReportSnapshotInterval > 0 {
		if err := w.materializeReportSnapshots(ctx); err != nil {
			log.Printf("report snapshot materialization error: %v", err)
		}
	}

	log.Printf("worker started with redis=%s jira_enabled=%t report_snapshot_interval=%s", cfg.RedisAddr, cfg.JIRA.Enabled, cfg.ReportSnapshotInterval)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var snapshotTicker *time.Ticker
	var snapshotC <-chan time.Time
	if cfg.ReportSnapshotInterval > 0 {
		snapshotTicker = time.NewTicker(cfg.ReportSnapshotInterval)
		snapshotC = snapshotTicker.C
		defer snapshotTicker.Stop()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			if err := w.processDueEvents(ctx); err != nil {
				log.Printf("retry processing error: %v", err)
			}
		case <-snapshotC:
			if err := w.materializeReportSnapshots(ctx); err != nil {
				log.Printf("report snapshot materialization error: %v", err)
			}
		case <-stop:
			log.Printf("worker shutdown")
			return
		}
	}
}

func loadConfig() (config, error) {
	redisDB, err := strconv.Atoi(getenv("REDIS_DB", "0"))
	if err != nil {
		return config{}, fmt.Errorf("invalid REDIS_DB: %w", err)
	}
	reportSnapshotInterval, err := time.ParseDuration(getenv("REPORT_SNAPSHOT_INTERVAL", "5m"))
	if err != nil {
		return config{}, fmt.Errorf("invalid REPORT_SNAPSHOT_INTERVAL: %w", err)
	}
	cfg := config{
		DBURL:                  os.Getenv("DATABASE_URL"),
		RedisAddr:              getenv("REDIS_ADDR", "localhost:6379"),
		RedisDB:                redisDB,
		ReportSnapshotInterval: reportSnapshotInterval,
		JIRA: jiraConfig{
			Enabled:                 boolEnv("JIRA_ENABLED", false),
			BaseURL:                 os.Getenv("JIRA_BASE_URL"),
			Username:                os.Getenv("JIRA_USERNAME"),
			APIToken:                os.Getenv("JIRA_API_TOKEN"),
			ProjectKey:              os.Getenv("JIRA_PROJECT_KEY"),
			AcknowledgeTransitionID: os.Getenv("JIRA_ACK_TRANSITION_ID"),
			ResolveTransitionID:     os.Getenv("JIRA_RESOLVE_TRANSITION_ID"),
			ReopenTransitionID:      os.Getenv("JIRA_REOPEN_TRANSITION_ID"),
			ResolutionField:         getenv("JIRA_RESOLUTION_FIELD", "resolution"),
			ResolutionValue:         getenv("JIRA_RESOLUTION_VALUE", "Done"),
		},
	}
	if cfg.DBURL == "" {
		return config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func (w worker) processDueEvents(ctx context.Context) error {
	events, err := w.listDueEvents(ctx)
	if err != nil {
		return err
	}
	for _, event := range events {
		lockKey := "incidentiq:integration-event:" + event.ID
		claimed, err := w.redis.SetNX(ctx, lockKey, "locked", lockTTL).Result()
		if err != nil {
			log.Printf("redis lock error for event %s: %v", event.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		if err := w.processEvent(ctx, event); err != nil {
			log.Printf("event %s failed: %v", event.ID, err)
		}
		_ = w.redis.Del(ctx, lockKey).Err()
	}
	return nil
}

func (w worker) listDueEvents(ctx context.Context) ([]integrationEvent, error) {
	rows, err := w.db.Query(ctx, `
		SELECT id, event_type, payload, attempts
		FROM integration_events
		WHERE integration_name = 'jira'
		  AND status = 'pending'
		  AND next_retry_at IS NOT NULL
		  AND next_retry_at <= NOW()
		  AND attempts < $1
		ORDER BY next_retry_at ASC
		LIMIT $2
	`, maxAttempts, batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []integrationEvent{}
	for rows.Next() {
		var event integrationEvent
		var payload []byte
		if err := rows.Scan(&event.ID, &event.Type, &payload, &event.Attempts); err != nil {
			return nil, err
		}
		event.Payload = jsonToMap(payload)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (w worker) processEvent(ctx context.Context, event integrationEvent) error {
	if !w.jira.Enabled {
		return w.deferEvent(ctx, event, fmt.Errorf("JIRA integration is disabled"))
	}
	if err := w.markRunning(ctx, event.ID); err != nil {
		return err
	}

	incidentID := stringValue(event.Payload["incident_id"])
	if incidentID == "" {
		return w.markFailed(ctx, event.ID, "missing incident_id")
	}

	inc, err := w.getIncident(ctx, incidentID)
	if err != nil {
		return w.deferEvent(ctx, event, err)
	}
	link, err := w.getJIRALink(ctx, incidentID)
	if err != nil {
		return w.deferEvent(ctx, event, err)
	}

	if event.Type == "issue_create" && link == nil {
		ref, err := w.createIssue(ctx, inc)
		if err != nil {
			return w.deferEvent(ctx, event, err)
		}
		if err := w.upsertJIRALink(ctx, incidentID, ref.Key, ref.ID); err != nil {
			return w.deferEvent(ctx, event, err)
		}
		return w.markCompleted(ctx, event.ID, map[string]any{
			"incident_id":    incidentID,
			"jira_issue_key": ref.Key,
			"retry_event_id": event.ID,
		})
	}

	if link == nil {
		ref, err := w.createIssue(ctx, inc)
		if err != nil {
			return w.deferEvent(ctx, event, err)
		}
		if err := w.upsertJIRALink(ctx, incidentID, ref.Key, ref.ID); err != nil {
			return w.deferEvent(ctx, event, err)
		}
		return w.markCompleted(ctx, event.ID, map[string]any{
			"incident_id":    incidentID,
			"jira_issue_key": ref.Key,
			"retry_event_id": event.ID,
		})
	}

	if err := w.updateIssue(ctx, link.IssueKey, inc); err != nil {
		return w.deferEvent(ctx, event, err)
	}
	return w.markCompleted(ctx, event.ID, map[string]any{
		"incident_id":    incidentID,
		"jira_issue_key": link.IssueKey,
		"retry_event_id": event.ID,
	})
}

func (w worker) getIncident(ctx context.Context, id string) (incident, error) {
	var out incident
	err := w.db.QueryRow(ctx, `
		SELECT id, title, summary, severity, service, environment, status, started_at, acknowledged_at, resolved_at
		FROM incidents
		WHERE id = $1
	`, id).Scan(&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt)
	return out, err
}

func (w worker) getJIRALink(ctx context.Context, incidentID string) (*jiraLink, error) {
	var out jiraLink
	err := w.db.QueryRow(ctx, `
		SELECT jira_issue_key, jira_issue_id
		FROM jira_links
		WHERE incident_id = $1
	`, incidentID).Scan(&out.IssueKey, &out.IssueID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (w worker) upsertJIRALink(ctx context.Context, incidentID string, issueKey string, issueID string) error {
	var issueIDPtr *string
	if issueID != "" {
		issueIDPtr = &issueID
	}
	_, err := w.db.Exec(ctx, `
		INSERT INTO jira_links (incident_id, jira_issue_key, jira_issue_id, created_at, updated_at)
		VALUES ($1,$2,$3,NOW(),NOW())
		ON CONFLICT (incident_id) DO UPDATE
		SET jira_issue_key = EXCLUDED.jira_issue_key,
		    jira_issue_id = EXCLUDED.jira_issue_id,
		    updated_at = NOW()
	`, incidentID, issueKey, issueIDPtr)
	return err
}

func (w worker) markRunning(ctx context.Context, id string) error {
	_, err := w.db.Exec(ctx, `
		UPDATE integration_events
		SET status = 'running', attempts = attempts + 1, updated_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, id)
	return err
}

func (w worker) markCompleted(ctx context.Context, id string, payload map[string]any) error {
	_, err := w.db.Exec(ctx, `
		UPDATE integration_events
		SET status = 'completed', payload = $2, next_retry_at = NULL, updated_at = NOW()
		WHERE id = $1
	`, id, payloadToJSON(payload))
	return err
}

func (w worker) markFailed(ctx context.Context, id string, message string) error {
	_, err := w.db.Exec(ctx, `
		UPDATE integration_events
		SET status = 'failed',
		    payload = jsonb_set(payload, '{error}', to_jsonb($2::text), true),
		    next_retry_at = NULL,
		    updated_at = NOW()
		WHERE id = $1
	`, id, message)
	return err
}

func (w worker) deferEvent(ctx context.Context, event integrationEvent, cause error) error {
	nextAttempts := event.Attempts + 1
	if nextAttempts >= maxAttempts {
		_ = w.markFailed(ctx, event.ID, cause.Error())
		return cause
	}
	delaySeconds := int(math.Pow(2, float64(nextAttempts))) * 60
	nextRetryAt := time.Now().UTC().Add(time.Duration(delaySeconds) * time.Second)
	_, err := w.db.Exec(ctx, `
		UPDATE integration_events
		SET status = 'pending',
		    payload = jsonb_set(payload, '{error}', to_jsonb($2::text), true),
		    next_retry_at = $3,
		    updated_at = NOW()
		WHERE id = $1
	`, event.ID, cause.Error(), nextRetryAt)
	if err != nil {
		return err
	}
	return cause
}

func (w worker) createIssue(ctx context.Context, inc incident) (jiraIssueRef, error) {
	payload := map[string]any{
		"fields": map[string]any{
			"project":     map[string]any{"key": w.jira.ProjectKey},
			"summary":     inc.Title,
			"description": issueDescription(inc),
			"issuetype":   map[string]any{"name": "Task"},
			"priority":    map[string]any{"name": priorityForSeverity(inc.Severity)},
			"labels":      labelsForIncident(inc),
		},
	}
	req, err := w.newJIRARequest(ctx, http.MethodPost, "/rest/api/2/issue", payload)
	if err != nil {
		return jiraIssueRef{}, err
	}
	var out jiraIssueRef
	if err := w.doJIRA(req, http.StatusCreated, &out); err != nil {
		return jiraIssueRef{}, err
	}
	return out, nil
}

func (w worker) updateIssue(ctx context.Context, issueKey string, inc incident) error {
	payload := map[string]any{
		"fields": map[string]any{
			"summary":     inc.Title,
			"description": issueDescription(inc),
			"priority":    map[string]any{"name": priorityForSeverity(inc.Severity)},
			"labels":      labelsForIncident(inc),
		},
	}
	req, err := w.newJIRARequest(ctx, http.MethodPut, "/rest/api/2/issue/"+url.PathEscape(issueKey), payload)
	if err != nil {
		return err
	}
	if err := w.doJIRA(req, http.StatusNoContent, nil); err != nil {
		return err
	}
	comment := fmt.Sprintf("IncidentIQ retry sync: incident %s is %s with severity %s.", inc.ID, inc.Status, inc.Severity)
	if inc.ResolvedAt != nil {
		comment += " Resolved at " + inc.ResolvedAt.UTC().Format(time.RFC3339) + "."
	}
	if err := w.addComment(ctx, issueKey, comment); err != nil {
		return err
	}
	return w.transitionIssue(ctx, issueKey, inc)
}

func (w worker) addComment(ctx context.Context, issueKey string, body string) error {
	req, err := w.newJIRARequest(ctx, http.MethodPost, "/rest/api/2/issue/"+url.PathEscape(issueKey)+"/comment", map[string]any{"body": body})
	if err != nil {
		return err
	}
	return w.doJIRA(req, http.StatusCreated, nil)
}

func (w worker) transitionIssue(ctx context.Context, issueKey string, inc incident) error {
	transitionID := w.transitionIDForStatus(inc.Status)
	if transitionID == "" {
		return nil
	}
	payload := map[string]any{
		"transition": map[string]any{
			"id": transitionID,
		},
	}
	if strings.EqualFold(inc.Status, "resolved") && w.jira.ResolutionField != "" && w.jira.ResolutionValue != "" {
		payload["fields"] = map[string]any{
			w.jira.ResolutionField: resolutionFieldValue(w.jira.ResolutionField, w.jira.ResolutionValue),
		}
	}
	req, err := w.newJIRARequest(ctx, http.MethodPost, "/rest/api/2/issue/"+url.PathEscape(issueKey)+"/transitions", payload)
	if err != nil {
		return err
	}
	return w.doJIRA(req, http.StatusNoContent, nil)
}

func (w worker) transitionIDForStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "acknowledged":
		return w.jira.AcknowledgeTransitionID
	case "resolved":
		return w.jira.ResolveTransitionID
	case "open":
		return w.jira.ReopenTransitionID
	default:
		return ""
	}
}

func (w worker) newJIRARequest(ctx context.Context, method string, path string, payload any) (*http.Request, error) {
	if strings.TrimSpace(w.jira.BaseURL) == "" || strings.TrimSpace(w.jira.Username) == "" || strings.TrimSpace(w.jira.APIToken) == "" || strings.TrimSpace(w.jira.ProjectKey) == "" {
		return nil, fmt.Errorf("JIRA integration is not fully configured")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, joinURL(w.jira.BaseURL, path), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(w.jira.Username, w.jira.APIToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (w worker) doJIRA(req *http.Request, wantStatus int, out any) error {
	resp, err := w.http.Do(req)
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

func (w worker) materializeReportSnapshots(ctx context.Context) error {
	rows, err := w.reportMetricRows(ctx)
	if err != nil {
		return err
	}
	deploymentCount, err := w.deploymentCount(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	payloads := []struct {
		reportType string
		payload    any
	}{
		{reportType: "live", payload: computeLiveMetrics(rows, now)},
		{reportType: "sre", payload: computeSREReport(rows, now)},
		{reportType: "dora", payload: computeDORAReport(rows, deploymentCount, now)},
		{reportType: "executive", payload: computeExecutiveReport(rows, now)},
	}
	for _, item := range payloads {
		if err := w.insertReportSnapshot(ctx, item.reportType, item.payload, now); err != nil {
			return err
		}
	}
	log.Printf("materialized %d report snapshots", len(payloads))
	return nil
}

func (w worker) reportMetricRows(ctx context.Context) ([]reportMetricRow, error) {
	rows, err := w.db.Query(ctx, `
		SELECT
			i.id, i.severity, i.service, i.environment, i.owner_team, i.status,
			i.started_at, i.acknowledged_at, i.resolved_at,
			MIN(a.observed_at) AS first_alert_observed_at,
			COUNT(DISTINCT a.id) AS alert_count,
			COUNT(DISTINCT e.id) FILTER (WHERE e.event_type = 'incident_reopened') AS reopen_count,
			COUNT(DISTINCT d.id) AS linked_deployment_count
		FROM incidents i
		LEFT JOIN alerts a ON a.incident_id = i.id
		LEFT JOIN incident_events e ON e.incident_id = i.id
		LEFT JOIN deployments d ON d.incident_id = i.id
		GROUP BY i.id
		ORDER BY i.started_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []reportMetricRow{}
	for rows.Next() {
		var row reportMetricRow
		if err := rows.Scan(
			&row.ID, &row.Severity, &row.Service, &row.Environment, &row.OwnerTeam, &row.Status,
			&row.StartedAt, &row.AcknowledgedAt, &row.ResolvedAt, &row.FirstAlertObservedAt,
			&row.AlertCount, &row.ReopenCount, &row.LinkedDeploymentCount,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (w worker) deploymentCount(ctx context.Context) (int64, error) {
	var count int64
	err := w.db.QueryRow(ctx, `SELECT COUNT(*) FROM deployments`).Scan(&count)
	return count, err
}

func (w worker) insertReportSnapshot(ctx context.Context, reportType string, payload any, computedAt time.Time) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = w.db.Exec(ctx, `
		INSERT INTO report_snapshots (id, report_type, dimensions, computed_at, payload, created_at)
		VALUES ($1,$2,'{}'::jsonb,$3,$4::jsonb,NOW())
	`, newID(), reportType, computedAt, string(data))
	return err
}

func computeLiveMetrics(rows []reportMetricRow, now time.Time) liveMetrics {
	metrics := liveMetrics{
		CountsBySeverity:    map[string]int64{},
		CountsByService:     map[string]int64{},
		CountsByEnvironment: map[string]int64{},
		CountsByTeam:        map[string]int64{},
		CountsByStatus:      map[string]int64{},
	}

	mttd := []float64{}
	mtta := []float64{}
	mttr := []float64{}
	for _, row := range rows {
		metrics.IncidentCount++
		metrics.AlertCount += row.AlertCount
		metrics.ReopenCount += row.ReopenCount
		metrics.CountsBySeverity[row.Severity]++
		metrics.CountsByService[row.Service]++
		metrics.CountsByEnvironment[row.Environment]++
		metrics.CountsByTeam[row.OwnerTeam]++
		metrics.CountsByStatus[row.Status]++
		switch row.Status {
		case "open":
			metrics.OpenIncidentCount++
		case "acknowledged":
			metrics.AcknowledgedIncidentCount++
		case "resolved":
			metrics.ResolvedIncidentCount++
		}
		if row.AlertCount > 0 {
			metrics.IncidentsWithAlerts++
		}
		if row.FirstAlertObservedAt != nil {
			mttd = append(mttd, nonNegativeSeconds(row.StartedAt.Sub(*row.FirstAlertObservedAt)))
		}
		if row.AcknowledgedAt != nil {
			mtta = append(mtta, nonNegativeSeconds(row.AcknowledgedAt.Sub(row.StartedAt)))
		}
		if row.ResolvedAt != nil {
			mttr = append(mttr, nonNegativeSeconds(row.ResolvedAt.Sub(row.StartedAt)))
		}
		if row.Status != "resolved" {
			age := now.Sub(row.StartedAt)
			if isSLAAtRisk(row.Severity, age) {
				metrics.SLAAtRiskCount++
			}
			if isSLABreached(row.Severity, age) {
				metrics.SLABreachCount++
			}
		}
	}
	metrics.AlertToIncidentConversion = reportRatio(metrics.IncidentsWithAlerts, metrics.AlertCount)
	metrics.DuplicateSuppressionCount = metrics.AlertCount - metrics.IncidentsWithAlerts
	if metrics.DuplicateSuppressionCount < 0 {
		metrics.DuplicateSuppressionCount = 0
	}
	metrics.ReopenRate = reportRatio(metrics.ReopenCount, metrics.IncidentCount)
	metrics.MeanTimeToDetectSeconds = averagePtr(mttd)
	metrics.MeanTimeToAckSeconds = averagePtr(mtta)
	metrics.MeanTimeToResolveSeconds = averagePtr(mttr)
	return metrics
}

func computeSREReport(rows []reportMetricRow, now time.Time) sreReport {
	return sreReport{liveMetrics: computeLiveMetrics(rows, now), GeneratedAt: now}
}

func computeDORAReport(rows []reportMetricRow, deploymentCount int64, now time.Time) doraReport {
	var linked int64
	restoreDurations := []float64{}
	for _, row := range rows {
		if row.LinkedDeploymentCount > 0 {
			linked++
			if row.ResolvedAt != nil {
				restoreDurations = append(restoreDurations, row.ResolvedAt.Sub(row.StartedAt).Seconds())
			}
		}
	}
	return doraReport{
		DeploymentCount:             deploymentCount,
		DeploymentLinkedIncidents:   linked,
		ChangeFailureRate:           reportRatio(linked, deploymentCount),
		TimeToRestoreServiceSeconds: averagePtr(restoreDurations),
		GeneratedAt:                 now,
	}
}

func computeExecutiveReport(rows []reportMetricRow, now time.Time) executiveReport {
	metrics := computeLiveMetrics(rows, now)
	var downtime float64
	notes := []string{}
	for _, row := range rows {
		end := now
		if row.ResolvedAt != nil {
			end = *row.ResolvedAt
		}
		if end.After(row.StartedAt) {
			downtime += end.Sub(row.StartedAt).Seconds()
		}
		if row.Status != "resolved" && (row.Severity == "critical" || row.Severity == "high") {
			notes = append(notes, fmt.Sprintf("%s %s incident remains %s", row.Severity, row.Service, row.Status))
		}
	}
	return executiveReport{
		IncidentCount:       metrics.IncidentCount,
		DowntimeSeconds:     downtime,
		SLAAtRiskCount:      metrics.SLAAtRiskCount,
		SLABreachCount:      metrics.SLABreachCount,
		SeverityTrends:      metrics.CountsBySeverity,
		ServiceReliability:  metrics.CountsByService,
		BusinessImpactNotes: notes,
		GeneratedAt:         now,
	}
}

func isSLAAtRisk(severity string, age time.Duration) bool {
	return age >= slaThreshold(severity)/2
}

func isSLABreached(severity string, age time.Duration) bool {
	return age >= slaThreshold(severity)
}

func slaThreshold(severity string) time.Duration {
	switch strings.ToLower(severity) {
	case "critical":
		return time.Hour
	case "high":
		return 4 * time.Hour
	case "medium":
		return 24 * time.Hour
	default:
		return 72 * time.Hour
	}
}

func nonNegativeSeconds(duration time.Duration) float64 {
	if duration < 0 {
		return 0
	}
	return duration.Seconds()
}

func averagePtr(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	var total float64
	for _, value := range values {
		total += value
	}
	avg := total / float64(len(values))
	return &avg
}

func reportRatio(numerator int64, denominator int64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func issueDescription(inc incident) string {
	lines := []string{
		"IncidentIQ incident: " + inc.ID,
		"Severity: " + inc.Severity,
		"Service: " + inc.Service,
		"Environment: " + inc.Environment,
		"Status: " + inc.Status,
		"Started: " + inc.StartedAt.UTC().Format(time.RFC3339),
		"",
		inc.Summary,
	}
	if inc.ResolvedAt != nil {
		lines = append(lines, "Resolved: "+inc.ResolvedAt.UTC().Format(time.RFC3339))
	}
	return strings.Join(lines, "\n")
}

func labelsForIncident(inc incident) []string {
	return []string{
		"incidentiq",
		"incidentiq-service-" + sanitizeLabel(inc.Service),
		"incidentiq-env-" + sanitizeLabel(inc.Environment),
		"incidentiq-severity-" + sanitizeLabel(inc.Severity),
		"incidentiq-status-" + sanitizeLabel(inc.Status),
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

func jsonToMap(data []byte) map[string]any {
	if len(data) == 0 {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func payloadToJSON(payload map[string]any) []byte {
	if payload == nil {
		payload = map[string]any{}
	}
	data, _ := json.Marshal(payload)
	return data
}

func newID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(data[:])
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func getenv(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func boolEnv(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "TRUE" || v == "yes" || v == "YES"
}
