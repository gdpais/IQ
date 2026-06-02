package main

import (
	"bytes"
	"context"
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
	DBURL     string
	RedisAddr string
	RedisDB   int
	JIRA      jiraConfig
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

type jiraLink struct {
	IssueKey string
	IssueID  *string
}

type jiraIssueRef struct {
	Key string `json:"key"`
	ID  string `json:"id,omitempty"`
}

type worker struct {
	db    *pgx.Conn
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
	db, err := pgx.Connect(ctx, cfg.DBURL)
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
	}
	defer db.Close(ctx)

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

	log.Printf("worker started with redis=%s jira_enabled=%t", cfg.RedisAddr, cfg.JIRA.Enabled)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			if err := w.processDueEvents(ctx); err != nil {
				log.Printf("retry processing error: %v", err)
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
	cfg := config{
		DBURL:     os.Getenv("DATABASE_URL"),
		RedisAddr: getenv("REDIS_ADDR", "localhost:6379"),
		RedisDB:   redisDB,
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
