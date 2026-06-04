// Package main is the IncidentIQ background worker. It polls the
// integration_events table for pending Jira and Microsoft Teams tasks and
// processes them with exponential-backoff retries (up to 5 attempts). The
// worker also handles Teams OAuth2 token refresh and deduplicates channel
// notifications via the incident_notification_deliveries table.
package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	graphBaseURL    = "https://graph.microsoft.com/v1.0"
	integrationJIRA = "jira"
	integrationTeam = "teams"
	maxAttempts     = 5
)

type config struct {
	DBURL              string
	RedisAddr          string
	RedisDB            int
	PollInterval       time.Duration
	JIRA               jiraConfig
	Teams              teamsConfig
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

type teamsConfig struct {
	Enabled            bool
	TenantID           string
	ClientID           string
	ClientSecret       string
	TokenEncryptionKey string
}

type integrationEvent struct {
	ID              string
	IntegrationName string
	Type            string
	Status          string
	Payload         map[string]any
	Attempts        int
	NextRetryAt     *time.Time
}

type incidentRecord struct {
	ID             string
	Title          string
	Summary        string
	Severity       string
	Service        string
	Environment    string
	OwnerTeam      string
	Status         string
	StartedAt      time.Time
	AcknowledgedAt *time.Time
	ResolvedAt     *time.Time
}

type alertRecord struct {
	Service string
}

type eventRecord struct {
	Type    string
	Payload map[string]any
}

type teamsAuthState struct {
	SenderDisplay string
	SenderUPN     string
	AccessToken   string
	RefreshToken  string
	Scopes        string
	ExpiresAt     *time.Time
}

type routeRecipient struct {
	Type          string `json:"type"`
	TeamsObjectID string `json:"teams_object_id"`
	DisplayName   string `json:"display_name"`
	UPN           string `json:"upn,omitempty"`
}

func main() {
	ctx := context.Background()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	db, err := pgxpool.New(ctx, cfg.DBURL)
	if err != nil {
		log.Fatalf("db connect error: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("db ping error: %v", err)
	}

	redisClient := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, DB: cfg.RedisDB})
	defer redisClient.Close()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Printf("redis ping warning: %v", err)
	}

	processor := &worker{
		cfg:    cfg,
		db:     db,
		redis:  redisClient,
		http:   &http.Client{Timeout: 15 * time.Second},
		stopCh: make(chan struct{}),
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	go processor.run(ctx, ticker.C)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	close(processor.stopCh)
}

// worker holds the shared state for the poll-and-process loop.
type worker struct {
	cfg    config
	db     *pgxpool.Pool
	redis  *redis.Client
	http   *http.Client
	jira   jiraConfig
	teams  teamsConfig
	stopCh chan struct{}
}

func (w *worker) run(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticks:
			w.processCycle(ctx)
		}
	}
}

func (w *worker) processCycle(ctx context.Context) {
	for _, integration := range []string{integrationJIRA, integrationTeam} {
		events, err := w.claimPendingIntegrationEvents(ctx, integration, 10)
		if err != nil {
			log.Printf("claim %s events: %v", integration, err)
			continue
		}
		for _, event := range events {
			if err := w.processEvent(ctx, event); err != nil {
				log.Printf("process %s event %s: %v", event.IntegrationName, event.ID, err)
			}
		}
	}
}

func (w *worker) processDueEvents(ctx context.Context) error {
	w.processCycle(ctx)
	return nil
}

func (w *worker) processEvent(ctx context.Context, event integrationEvent) error {
	switch event.IntegrationName {
	case integrationJIRA:
		return w.processJIRAEvent(ctx, event)
	case integrationTeam:
		return w.processTeamsEvent(ctx, event)
	default:
		return w.failIntegrationEvent(ctx, event.ID, mergePayload(event.Payload, map[string]any{"error": "unsupported integration"}))
	}
}

func (w *worker) processJIRAEvent(ctx context.Context, event integrationEvent) error {
	jiraCfg := w.jiraConfig()
	if !jiraCfg.Enabled {
		return w.failIntegrationEvent(ctx, event.ID, mergePayload(event.Payload, map[string]any{"error": "JIRA integration is disabled"}))
	}
	incidentID, _ := event.Payload["incident_id"].(string)
	inc, err := w.getIncident(ctx, incidentID)
	if err != nil {
		return w.retryOrFail(ctx, event, err, transientError(err))
	}
	link, err := w.getJIRALink(ctx, incidentID)
	if err != nil {
		return w.retryOrFail(ctx, event, err, transientError(err))
	}
	issue := jiraIssue{
		IncidentID:  inc.ID,
		Title:       inc.Title,
		Summary:     inc.Summary,
		Severity:    inc.Severity,
		Service:     inc.Service,
		Environment: inc.Environment,
		Status:      inc.Status,
		StartedAt:   inc.StartedAt,
		ResolvedAt:  inc.ResolvedAt,
	}
	client := &jiraClient{cfg: jiraCfg, http: w.http}
	action, _ := event.Payload["action"].(string)
	if issueKey, _ := event.Payload["jira_issue_key"].(string); issueKey != "" {
		issueID, _ := event.Payload["jira_issue_id"].(string)
		if _, err := w.upsertJIRALink(ctx, incidentID, issueKey, issueID); err != nil {
			return w.retryOrFail(ctx, event, err, true)
		}
		return w.completeIntegrationEvent(ctx, event.ID, event.Payload)
	}
	if link == nil || action == "create" {
		created, err := client.CreateIssue(ctx, issue)
		if err != nil {
			return w.retryOrFail(ctx, event, err, transientError(err))
		}
		if _, err := w.upsertJIRALink(ctx, incidentID, created.Key, created.ID); err != nil {
			payload := mergePayload(event.Payload, map[string]any{"jira_issue_key": created.Key, "jira_issue_id": created.ID})
			return w.retryOrFail(ctx, integrationEventWithPayload(event, payload), err, true)
		}
		return w.completeIntegrationEvent(ctx, event.ID, mergePayload(event.Payload, map[string]any{"jira_issue_key": created.Key, "jira_issue_id": created.ID}))
	}
	if err := client.UpdateIssue(ctx, link.JIRAIssueKey, issue); err != nil {
		return w.retryOrFail(ctx, event, err, transientError(err))
	}
	return w.completeIntegrationEvent(ctx, event.ID, mergePayload(event.Payload, map[string]any{"jira_issue_key": link.JIRAIssueKey}))
}

func (w *worker) processTeamsEvent(ctx context.Context, event integrationEvent) error {
	teamsCfg := w.teamsConfig()
	if !teamsCfg.Enabled {
		return w.failIntegrationEvent(ctx, event.ID, mergePayload(event.Payload, map[string]any{"error": "Teams integration is disabled"}))
	}
	incidentID, _ := event.Payload["incident_id"].(string)
	teamID, _ := event.Payload["team_id"].(string)
	channelID, _ := event.Payload["channel_id"].(string)
	reason, _ := event.Payload["reason"].(string)
	recipients := parseRecipients(event.Payload["recipients"])
	if incidentID == "" || teamID == "" || channelID == "" || len(recipients) == 0 {
		return w.failIntegrationEvent(ctx, event.ID, mergePayload(event.Payload, map[string]any{"error": "missing Teams event payload fields"}))
	}

	detail, err := w.getIncidentDetail(ctx, incidentID)
	if err != nil {
		return w.retryOrFail(ctx, event, err, transientError(err))
	}
	auth, err := w.getTeamsAuthState(ctx)
	if err != nil {
		return w.retryOrFail(ctx, event, err, true)
	}
	if auth == nil {
		return w.failIntegrationEvent(ctx, event.ID, mergePayload(event.Payload, map[string]any{"error": "Teams integration is not connected"}))
	}

	reserved, err := w.reserveTeamsDelivery(ctx, incidentID, channelID, reason, map[string]any{
		"team_id":    teamID,
		"channel_id": channelID,
		"recipients": recipients,
	})
	if err != nil {
		return w.retryOrFail(ctx, event, err, true)
	}
	if !reserved {
		if messageID, _ := event.Payload["message_id"].(string); messageID != "" {
			if err := w.completeTeamsDelivery(ctx, incidentID, channelID, reason, messageID, map[string]any{
				"team_id":    teamID,
				"channel_id": channelID,
				"message_id": messageID,
				"recipients": recipients,
			}); err != nil {
				return w.retryOrFail(ctx, event, err, true)
			}
			return w.completeIntegrationEvent(ctx, event.ID, event.Payload)
		}
		return w.completeIntegrationEvent(ctx, event.ID, mergePayload(event.Payload, map[string]any{"duplicate": true}))
	}

	body, mentions := buildTeamsPage(detail, recipients)
	client := &teamsClient{cfg: teamsCfg, http: w.http}
	messageID, updatedAuth, err := client.SendChannelMessage(ctx, auth, teamID, channelID, body, mentions)
	if updatedAuth != nil {
		if upsertErr := w.upsertTeamsAuthState(ctx, *updatedAuth); upsertErr != nil {
			_ = w.rollbackTeamsDelivery(ctx, incidentID, channelID, reason)
			return w.retryOrFail(ctx, event, upsertErr, true)
		}
	}
	if err != nil {
		_ = w.rollbackTeamsDelivery(ctx, incidentID, channelID, reason)
		return w.retryOrFail(ctx, event, err, transientError(err))
	}

	if err := w.completeTeamsDelivery(ctx, incidentID, channelID, reason, messageID, map[string]any{
		"team_id":    teamID,
		"channel_id": channelID,
		"message_id": messageID,
		"recipients": recipients,
	}); err != nil {
		payload := mergePayload(event.Payload, map[string]any{"message_id": messageID})
		return w.retryOrFail(ctx, integrationEventWithPayload(event, payload), err, true)
	}
	return w.completeIntegrationEvent(ctx, event.ID, mergePayload(event.Payload, map[string]any{"message_id": messageID}))
}

// retryOrFail either schedules an exponential-backoff retry (delay doubles per
// attempt, capped at 2^5 = 32 minutes) or permanently fails the event once
// maxAttempts is reached or retry is false.
func (w *worker) retryOrFail(ctx context.Context, event integrationEvent, err error, retry bool) error {
	payload := mergePayload(event.Payload, map[string]any{"error": err.Error()})
	if !retry || event.Attempts >= maxAttempts {
		return w.failIntegrationEvent(ctx, event.ID, payload)
	}
	delay := time.Duration(1<<min(event.Attempts, 5)) * time.Minute
	return w.retryIntegrationEvent(ctx, event.ID, payload, time.Now().UTC().Add(delay))
}

// claimPendingIntegrationEvents selects up to limit pending events for the
// given integration using SELECT … FOR UPDATE SKIP LOCKED so concurrent worker
// instances do not process the same event. It atomically transitions each
// claimed row to status "processing" within the same transaction.
func (w *worker) claimPendingIntegrationEvents(ctx context.Context, integration string, limit int) ([]integrationEvent, error) {
	tx, err := w.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id
		FROM integration_events
		WHERE integration_name = $1
		  AND status = 'pending'
		  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		ORDER BY created_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT $2
	`, integration, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items := []integrationEvent{}
	for _, id := range ids {
		var event integrationEvent
		var payload []byte
		err := tx.QueryRow(ctx, `
			UPDATE integration_events
			SET status = 'processing', attempts = attempts + 1, updated_at = NOW()
			WHERE id = $1
			RETURNING id, integration_name, event_type, status, payload, attempts, next_retry_at
		`, id).Scan(&event.ID, &event.IntegrationName, &event.Type, &event.Status, &payload, &event.Attempts, &event.NextRetryAt)
		if err != nil {
			return nil, err
		}
		event.Payload = decodePayload(payload)
		items = append(items, event)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (w *worker) completeIntegrationEvent(ctx context.Context, id string, payload map[string]any) error {
	_, err := w.db.Exec(ctx, `UPDATE integration_events SET status='completed', payload=$2, next_retry_at=NULL, updated_at=NOW() WHERE id=$1`, id, encodePayload(payload))
	return err
}

func (w *worker) jiraConfig() jiraConfig {
	if w.jira.Enabled || w.jira.BaseURL != "" || w.jira.ProjectKey != "" || w.jira.AcknowledgeTransitionID != "" || w.jira.ResolveTransitionID != "" || w.jira.ReopenTransitionID != "" {
		return w.jira
	}
	return w.cfg.JIRA
}

func (w *worker) teamsConfig() teamsConfig {
	if w.teams.Enabled || w.teams.ClientID != "" || w.teams.TenantID != "" {
		return w.teams
	}
	return w.cfg.Teams
}

func (w *worker) retryIntegrationEvent(ctx context.Context, id string, payload map[string]any, nextRetryAt time.Time) error {
	_, err := w.db.Exec(ctx, `UPDATE integration_events SET status='pending', payload=$2, next_retry_at=$3, updated_at=NOW() WHERE id=$1`, id, encodePayload(payload), nextRetryAt)
	return err
}

func (w *worker) failIntegrationEvent(ctx context.Context, id string, payload map[string]any) error {
	_, err := w.db.Exec(ctx, `UPDATE integration_events SET status='failed', payload=$2, next_retry_at=NULL, updated_at=NOW() WHERE id=$1`, id, encodePayload(payload))
	return err
}

func (w *worker) getIncident(ctx context.Context, id string) (incidentRecord, error) {
	var out incidentRecord
	err := w.db.QueryRow(ctx, `
		SELECT id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at
		FROM incidents
		WHERE id = $1
	`, id).Scan(&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt)
	return out, err
}

func (w *worker) getIncidentDetail(ctx context.Context, id string) (struct {
	Incident incidentRecord
	Alerts   []alertRecord
	Events   []eventRecord
}, error) {
	inc, err := w.getIncident(ctx, id)
	if err != nil {
		return struct {
			Incident incidentRecord
			Alerts   []alertRecord
			Events   []eventRecord
		}{}, err
	}
	alertRows, err := w.db.Query(ctx, `SELECT service FROM alerts WHERE incident_id = $1 ORDER BY observed_at ASC`, id)
	if err != nil {
		return struct {
			Incident incidentRecord
			Alerts   []alertRecord
			Events   []eventRecord
		}{}, err
	}
	defer alertRows.Close()
	alerts := []alertRecord{}
	for alertRows.Next() {
		var alert alertRecord
		if err := alertRows.Scan(&alert.Service); err != nil {
			return struct {
				Incident incidentRecord
				Alerts   []alertRecord
				Events   []eventRecord
			}{}, err
		}
		alerts = append(alerts, alert)
	}
	eventRows, err := w.db.Query(ctx, `SELECT event_type, payload FROM incident_events WHERE incident_id = $1 ORDER BY created_at ASC`, id)
	if err != nil {
		return struct {
			Incident incidentRecord
			Alerts   []alertRecord
			Events   []eventRecord
		}{}, err
	}
	defer eventRows.Close()
	events := []eventRecord{}
	for eventRows.Next() {
		var record eventRecord
		var payload []byte
		if err := eventRows.Scan(&record.Type, &payload); err != nil {
			return struct {
				Incident incidentRecord
				Alerts   []alertRecord
				Events   []eventRecord
			}{}, err
		}
		record.Payload = decodePayload(payload)
		events = append(events, record)
	}
	return struct {
		Incident incidentRecord
		Alerts   []alertRecord
		Events   []eventRecord
	}{Incident: inc, Alerts: alerts, Events: events}, nil
}

type jiraLink struct {
	IncidentID   string
	JIRAIssueKey string
	JIRAIssueID  *string
}

func (w *worker) getJIRALink(ctx context.Context, incidentID string) (*jiraLink, error) {
	var out jiraLink
	err := w.db.QueryRow(ctx, `SELECT incident_id, jira_issue_key, jira_issue_id FROM jira_links WHERE incident_id = $1`, incidentID).Scan(&out.IncidentID, &out.JIRAIssueKey, &out.JIRAIssueID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (w *worker) upsertJIRALink(ctx context.Context, incidentID string, issueKey string, issueID string) (*jiraLink, error) {
	var out jiraLink
	err := w.db.QueryRow(ctx, `
		INSERT INTO jira_links (incident_id, jira_issue_key, jira_issue_id, created_at, updated_at)
		VALUES ($1,$2,$3,NOW(),NOW())
		ON CONFLICT (incident_id) DO UPDATE
		SET jira_issue_key = EXCLUDED.jira_issue_key, jira_issue_id = EXCLUDED.jira_issue_id, updated_at = NOW()
		RETURNING incident_id, jira_issue_key, jira_issue_id
	`, incidentID, issueKey, nullableString(issueID)).Scan(&out.IncidentID, &out.JIRAIssueKey, &out.JIRAIssueID)
	return &out, err
}

func (w *worker) getTeamsAuthState(ctx context.Context) (*teamsAuthState, error) {
	var out teamsAuthState
	err := w.db.QueryRow(ctx, `
		SELECT sender_display_name, sender_upn, access_token_encrypted, refresh_token_encrypted, scopes, expires_at
		FROM teams_auth_state
		WHERE integration_name = 'teams'
	`).Scan(&out.SenderDisplay, &out.SenderUPN, &out.AccessToken, &out.RefreshToken, &out.Scopes, &out.ExpiresAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (w *worker) upsertTeamsAuthState(ctx context.Context, state teamsAuthState) error {
	_, err := w.db.Exec(ctx, `
		INSERT INTO teams_auth_state (integration_name, sender_display_name, sender_upn, access_token_encrypted, refresh_token_encrypted, scopes, expires_at, created_at, updated_at)
		VALUES ('teams',$1,$2,$3,$4,$5,$6,NOW(),NOW())
		ON CONFLICT (integration_name) DO UPDATE
		SET sender_display_name = EXCLUDED.sender_display_name,
		    sender_upn = EXCLUDED.sender_upn,
		    access_token_encrypted = EXCLUDED.access_token_encrypted,
		    refresh_token_encrypted = EXCLUDED.refresh_token_encrypted,
		    scopes = EXCLUDED.scopes,
		    expires_at = EXCLUDED.expires_at,
		    updated_at = NOW()
	`, state.SenderDisplay, state.SenderUPN, state.AccessToken, state.RefreshToken, state.Scopes, state.ExpiresAt)
	return err
}

// reserveTeamsDelivery attempts to insert a delivery record for the
// (incident, channel, reason) tuple using INSERT … ON CONFLICT DO NOTHING.
// It returns true only when the row was inserted, meaning this worker instance
// won the race and should send the message.
func (w *worker) reserveTeamsDelivery(ctx context.Context, incidentID string, channelID string, reason string, payload map[string]any) (bool, error) {
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	cmd, err := w.db.Exec(ctx, `
		INSERT INTO incident_notification_deliveries (id, integration_name, incident_id, channel_id, notification_reason, message_id, payload, created_at)
		VALUES ($1,'teams',$2,$3,$4,'',$5,NOW())
		ON CONFLICT (integration_name, incident_id, channel_id, notification_reason) DO NOTHING
	`, tag, incidentID, channelID, reason, encodePayload(payload))
	if err != nil {
		return false, err
	}
	return cmd.RowsAffected() == 1, nil
}

func (w *worker) completeTeamsDelivery(ctx context.Context, incidentID string, channelID string, reason string, messageID string, payload map[string]any) error {
	_, err := w.db.Exec(ctx, `
		INSERT INTO incident_notification_deliveries (id, integration_name, incident_id, channel_id, notification_reason, message_id, payload, created_at)
		VALUES ($1, 'teams', $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (integration_name, incident_id, channel_id, notification_reason) DO UPDATE
		SET message_id = EXCLUDED.message_id,
		    payload = EXCLUDED.payload
	`, fmt.Sprintf("%d", time.Now().UnixNano()), incidentID, channelID, reason, messageID, encodePayload(payload))
	return err
}

func (w *worker) rollbackTeamsDelivery(ctx context.Context, incidentID string, channelID string, reason string) error {
	_, err := w.db.Exec(ctx, `
		DELETE FROM incident_notification_deliveries
		WHERE integration_name = 'teams' AND incident_id = $1 AND channel_id = $2 AND notification_reason = $3 AND message_id = ''
	`, incidentID, channelID, reason)
	return err
}

type jiraClient struct {
	cfg  jiraConfig
	http *http.Client
}

type jiraIssue struct {
	IncidentID  string
	Title       string
	Summary     string
	Severity    string
	Service     string
	Environment string
	Status      string
	StartedAt   time.Time
	ResolvedAt  *time.Time
}

type issueRef struct {
	Key string `json:"key"`
	ID  string `json:"id,omitempty"`
	URL string `json:"url,omitempty"`
}

func (c *jiraClient) CreateIssue(ctx context.Context, issue jiraIssue) (issueRef, error) {
	payload := map[string]any{
		"fields": map[string]any{
			"project":     map[string]any{"key": c.cfg.ProjectKey},
			"summary":     issue.Title,
			"description": jiraDescription(issue),
			"issuetype":   map[string]any{"name": "Task"},
			"priority":    map[string]any{"name": jiraPriority(issue.Severity)},
			"labels":      jiraLabels(issue),
		},
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/rest/api/2/issue", payload)
	if err != nil {
		return issueRef{}, err
	}
	var out issueRef
	if err := c.doJSON(req, http.StatusCreated, &out); err != nil {
		return issueRef{}, err
	}
	return out, nil
}

func (c *jiraClient) UpdateIssue(ctx context.Context, issueKey string, issue jiraIssue) error {
	payload := map[string]any{
		"fields": map[string]any{
			"summary":     issue.Title,
			"description": jiraDescription(issue),
			"priority":    map[string]any{"name": jiraPriority(issue.Severity)},
			"labels":      jiraLabels(issue),
		},
	}
	req, err := c.newRequest(ctx, http.MethodPut, "/rest/api/2/issue/"+url.PathEscape(issueKey), payload)
	if err != nil {
		return err
	}
	if err := c.doJSON(req, http.StatusNoContent, nil); err != nil {
		return err
	}
	comment := fmt.Sprintf("IncidentIQ sync: incident %s is %s with severity %s.", issue.IncidentID, issue.Status, issue.Severity)
	if err := c.AddComment(ctx, issueKey, comment); err != nil {
		return err
	}
	return c.TransitionIssue(ctx, issueKey, issue)
}

func (c *jiraClient) AddComment(ctx context.Context, issueKey string, body string) error {
	req, err := c.newRequest(ctx, http.MethodPost, "/rest/api/2/issue/"+url.PathEscape(issueKey)+"/comment", map[string]any{"body": body})
	if err != nil {
		return err
	}
	return c.doJSON(req, http.StatusCreated, nil)
}

func (c *jiraClient) TransitionIssue(ctx context.Context, issueKey string, issue jiraIssue) error {
	transitionID := ""
	switch strings.ToLower(strings.TrimSpace(issue.Status)) {
	case "acknowledged":
		transitionID = c.cfg.AcknowledgeTransitionID
	case "resolved":
		transitionID = c.cfg.ResolveTransitionID
	case "open":
		transitionID = c.cfg.ReopenTransitionID
	}
	if transitionID == "" {
		return nil
	}
	payload := map[string]any{
		"transition": map[string]any{"id": transitionID},
	}
	if strings.EqualFold(issue.Status, "resolved") && c.cfg.ResolutionField != "" && c.cfg.ResolutionValue != "" {
		payload["fields"] = map[string]any{c.cfg.ResolutionField: resolutionFieldValue(c.cfg.ResolutionField, c.cfg.ResolutionValue)}
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/rest/api/2/issue/"+url.PathEscape(issueKey)+"/transitions", payload)
	if err != nil {
		return err
	}
	return c.doJSON(req, http.StatusNoContent, nil)
}

func (c *jiraClient) newRequest(ctx context.Context, method string, path string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.BaseURL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.cfg.Username, c.cfg.APIToken)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *jiraClient) doJSON(req *http.Request, wantStatus int, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("JIRA returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type teamsClient struct {
	cfg  teamsConfig
	http *http.Client
}

type teamsMention struct {
	ID          int
	MentionText string
	Type        string
	ObjectID    string
	DisplayName string
}

func (c *teamsClient) SendChannelMessage(ctx context.Context, state *teamsAuthState, teamID string, channelID string, body string, mentions []teamsMention) (string, *teamsAuthState, error) {
	accessToken, updated, err := c.accessToken(ctx, state)
	if err != nil {
		return "", updated, err
	}
	type mentionPayload struct {
		ID          int            `json:"id"`
		MentionText string         `json:"mentionText"`
		Mentioned   map[string]any `json:"mentioned"`
	}
	outMentions := make([]mentionPayload, 0, len(mentions))
	for _, mention := range mentions {
		mentioned := map[string]any{"displayName": mention.DisplayName}
		switch mention.Type {
		case "user":
			mentioned["user"] = map[string]any{"id": mention.ObjectID, "displayName": mention.DisplayName}
		case "tag":
			mentioned["tag"] = map[string]any{"id": mention.ObjectID, "displayName": mention.DisplayName}
		}
		outMentions = append(outMentions, mentionPayload{ID: mention.ID, MentionText: mention.MentionText, Mentioned: mentioned})
	}
	payload := map[string]any{
		"body": map[string]any{"contentType": "html", "content": body},
		"mentions": outMentions,
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphBaseURL+"/teams/"+url.PathEscape(teamID)+"/channels/"+url.PathEscape(channelID)+"/messages", bytes.NewReader(data))
	if err != nil {
		return "", updated, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", updated, err
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", updated, fmt.Errorf("Microsoft Graph returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return "", updated, err
	}
	return out.ID, updated, nil
}

// accessToken returns a valid decrypted access token. If the stored token is
// expired (or expiring within 2 minutes) it attempts a refresh via the
// Microsoft identity platform and returns the updated state for persistence.
func (c *teamsClient) accessToken(ctx context.Context, state *teamsAuthState) (string, *teamsAuthState, error) {
	if state == nil {
		return "", nil, fmt.Errorf("Teams integration is not connected")
	}
	accessToken, err := decrypt(c.cfg.TokenEncryptionKey, state.AccessToken)
	if err != nil {
		return "", nil, err
	}
	if accessToken != "" && (state.ExpiresAt == nil || time.Until(*state.ExpiresAt) > 2*time.Minute) {
		return accessToken, nil, nil
	}
	refreshToken, err := decrypt(c.cfg.TokenEncryptionKey, state.RefreshToken)
	if err != nil {
		return "", nil, err
	}
	if refreshToken == "" {
		return accessToken, nil, nil
	}
	next, err := c.refresh(ctx, refreshToken, state)
	if err != nil {
		return "", nil, err
	}
	token, err := decrypt(c.cfg.TokenEncryptionKey, next.AccessToken)
	if err != nil {
		return "", nil, err
	}
	return token, &next, nil
}

func (c *teamsClient) refresh(ctx context.Context, refreshToken string, state *teamsAuthState) (teamsAuthState, error) {
	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("client_id", c.cfg.ClientID)
	values.Set("client_secret", c.cfg.ClientSecret)
	values.Set("refresh_token", refreshToken)
	if strings.TrimSpace(state.Scopes) != "" {
		values.Set("scope", state.Scopes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://login.microsoftonline.com/"+url.PathEscape(c.cfg.TenantID)+"/oauth2/v2.0/token", strings.NewReader(values.Encode()))
	if err != nil {
		return teamsAuthState{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return teamsAuthState{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return teamsAuthState{}, fmt.Errorf("Microsoft Graph token refresh failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return teamsAuthState{}, err
	}
	encAccess, err := encrypt(c.cfg.TokenEncryptionKey, payload.AccessToken)
	if err != nil {
		return teamsAuthState{}, err
	}
	nextRefresh := refreshToken
	if payload.RefreshToken != "" {
		nextRefresh = payload.RefreshToken
	}
	encRefresh, err := encrypt(c.cfg.TokenEncryptionKey, nextRefresh)
	if err != nil {
		return teamsAuthState{}, err
	}
	expiresAt := time.Now().UTC().Add(time.Duration(payload.ExpiresIn) * time.Second)
	return teamsAuthState{
		SenderDisplay: state.SenderDisplay,
		SenderUPN:     state.SenderUPN,
		AccessToken:   encAccess,
		RefreshToken:  encRefresh,
		Scopes:        firstNonEmpty(payload.Scope, state.Scopes),
		ExpiresAt:     &expiresAt,
	}, nil
}

func buildTeamsPage(detail struct {
	Incident incidentRecord
	Alerts   []alertRecord
	Events   []eventRecord
}, recipients []routeRecipient) (string, []teamsMention) {
	mentions := []teamsMention{}
	prefix := []string{}
	for i, recipient := range recipients {
		if strings.TrimSpace(recipient.DisplayName) == "" {
			continue
		}
		mentions = append(mentions, teamsMention{
			ID:          i,
			MentionText: "@"+recipient.DisplayName,
			Type:        recipient.Type,
			ObjectID:    recipient.TeamsObjectID,
			DisplayName: recipient.DisplayName,
		})
		prefix = append(prefix, `<at id="`+strconv.Itoa(i)+`">`+escapeHTML(recipient.DisplayName)+`</at>`)
	}
	state := "incident opened"
	for _, event := range detail.Events {
		if event.Type == "incident_reopened" {
			state = "incident reopened"
		}
	}
	lines := []string{}
	if len(prefix) > 0 {
		lines = append(lines, strings.Join(prefix, " ")+" "+strings.Title(detail.Incident.Severity)+" "+state)
	} else {
		lines = append(lines, strings.Title(detail.Incident.Severity)+" "+state)
	}
	lines = append(lines, "Services: "+escapeHTML(strings.Join(detailServices(detail), ", ")))
	lines = append(lines, "Started: "+detail.Incident.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
	if summary := detailSummary(detail); summary != "" {
		lines = append(lines, "Issue: "+escapeHTML(summary))
	}
	return strings.Join(lines, "<br/>"), mentions
}

func detailServices(detail struct {
	Incident incidentRecord
	Alerts   []alertRecord
	Events   []eventRecord
}) []string {
	seen := map[string]bool{}
	items := []string{}
	add := func(value string) {
		value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
		if value == "" || seen[strings.ToLower(value)] {
			return
		}
		seen[strings.ToLower(value)] = true
		items = append(items, value)
	}
	add(detail.Incident.Service)
	for _, alert := range detail.Alerts {
		add(alert.Service)
	}
	return items
}

func detailSummary(detail struct {
	Incident incidentRecord
	Alerts   []alertRecord
	Events   []eventRecord
}) string {
	text := strings.Join(strings.Fields(strings.TrimSpace(detail.Incident.Summary)), " ")
	if text == "" {
		for _, event := range detail.Events {
			if summary, ok := event.Payload["summary"].(string); ok && strings.TrimSpace(summary) != "" {
				text = strings.Join(strings.Fields(strings.TrimSpace(summary)), " ")
				break
			}
		}
	}
	if len(text) > 180 {
		text = strings.TrimSpace(text[:177]) + "..."
	}
	return text
}

// encrypt seals value using AES-256-GCM. The key is derived by SHA-256 hashing
// secret so any key length is accepted. The nonce is prepended to the
// ciphertext and the whole thing is base64-encoded for database storage.
func encrypt(secret string, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	block, err := aes.NewCipher(encryptionKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(value), nil)), nil
}

func decrypt(secret string, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(encryptionKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func encryptionKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

func encodePayload(payload map[string]any) []byte {
	if payload == nil {
		payload = map[string]any{}
	}
	data, _ := json.Marshal(payload)
	return data
}

func decodePayload(data []byte) map[string]any {
	if len(data) == 0 {
		return map[string]any{}
	}
	out := map[string]any{}
	_ = json.Unmarshal(data, &out)
	return out
}

func mergePayload(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func integrationEventWithPayload(event integrationEvent, payload map[string]any) integrationEvent {
	event.Payload = payload
	return event
}

func parseRecipients(value any) []routeRecipient {
	items := []routeRecipient{}
	switch raw := value.(type) {
	case []any:
		for _, entry := range raw {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			items = append(items, routeRecipient{
				Type:          stringValue(m["type"]),
				TeamsObjectID: stringValue(m["teams_object_id"]),
				DisplayName:   stringValue(m["display_name"]),
				UPN:           stringValue(m["upn"]),
			})
		}
	}
	return items
}

func stringValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func nullableString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func jiraDescription(issue jiraIssue) string {
	lines := []string{
		"IncidentIQ incident: " + issue.IncidentID,
		"Severity: " + issue.Severity,
		"Service: " + issue.Service,
		"Environment: " + issue.Environment,
		"Status: " + issue.Status,
		"Started: " + issue.StartedAt.UTC().Format(time.RFC3339),
		"",
		issue.Summary,
	}
	if issue.ResolvedAt != nil {
		lines = append(lines, "Resolved: "+issue.ResolvedAt.UTC().Format(time.RFC3339))
	}
	return strings.Join(lines, "\n")
}

func jiraLabels(issue jiraIssue) []string {
	return []string{
		"incidentiq",
		"incidentiq-service-" + sanitizeLabel(issue.Service),
		"incidentiq-env-" + sanitizeLabel(issue.Environment),
		"incidentiq-severity-" + sanitizeLabel(issue.Severity),
		"incidentiq-status-" + sanitizeLabel(issue.Status),
	}
}

func jiraPriority(severity string) string {
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

func priorityForSeverity(severity string) string {
	return jiraPriority(severity)
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

func escapeHTML(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return replacer.Replace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type incident = jiraIssue

func labelsForIncident(inc incident) []string {
	return jiraLabels(jiraIssue(inc))
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

func (w worker) transitionIDForStatus(status string) string {
	cfg := w.jiraConfig()
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "acknowledged":
		return cfg.AcknowledgeTransitionID
	case "resolved":
		return cfg.ResolveTransitionID
	case "open":
		return cfg.ReopenTransitionID
	default:
		return ""
	}
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
	LinkedDeploymentCount int64
}

type doraReport struct {
	DeploymentLinkedIncidents   int64
	ChangeFailureRate           float64
	TimeToRestoreServiceSeconds *int64
}

type liveMetrics struct {
	IncidentCount             int64
	AlertCount                int64
	DuplicateSuppressionCount int64
	MeanTimeToDetectSeconds   *int64
}

func computeDORAReport(rows []reportMetricRow, deploymentCount int64, _ time.Time) doraReport {
	var linked int64
	var restoreTotal int64
	var restoreCount int64
	for _, row := range rows {
		if row.LinkedDeploymentCount > 0 {
			linked++
		}
		if row.ResolvedAt != nil {
			restoreTotal += int64(row.ResolvedAt.Sub(row.StartedAt).Seconds())
			restoreCount++
		}
	}
	out := doraReport{DeploymentLinkedIncidents: linked}
	if deploymentCount > 0 {
		out.ChangeFailureRate = float64(linked) / float64(deploymentCount)
	}
	if restoreCount > 0 {
		avg := restoreTotal / restoreCount
		out.TimeToRestoreServiceSeconds = &avg
	}
	return out
}

func computeLiveMetrics(rows []reportMetricRow, _ time.Time) liveMetrics {
	var incidents int64
	var alerts int64
	var duplicates int64
	var detectTotal int64
	var detectCount int64
	for _, row := range rows {
		incidents++
		alerts += row.AlertCount
		if row.AlertCount > 1 {
			duplicates += row.AlertCount - 1
		}
		if row.FirstAlertObservedAt != nil {
			detectTotal += int64(row.StartedAt.Sub(*row.FirstAlertObservedAt).Seconds())
			detectCount++
		}
	}
	out := liveMetrics{
		IncidentCount:             incidents,
		AlertCount:                alerts,
		DuplicateSuppressionCount: duplicates,
	}
	if detectCount > 0 {
		avg := detectTotal / detectCount
		out.MeanTimeToDetectSeconds = &avg
	}
	return out
}

// transientError reports whether err looks like a temporary network or server
// problem that is worth retrying (HTTP 5xx, rate limit, timeout, connection
// refused, etc.).
func transientError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "http 429"),
		strings.Contains(msg, "http 500"),
		strings.Contains(msg, "http 502"),
		strings.Contains(msg, "http 503"),
		strings.Contains(msg, "http 504"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "temporary"):
		return true
	default:
		return false
	}
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func loadConfig() (config, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return config{}, fmt.Errorf("DATABASE_URL is required")
	}
	redisDB, _ := strconv.Atoi(firstNonEmpty(os.Getenv("REDIS_DB"), "0"))
	poll := 5 * time.Second
	if raw := strings.TrimSpace(os.Getenv("WORKER_POLL_INTERVAL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			poll = parsed
		}
	}
	return config{
		DBURL:        dbURL,
		RedisAddr:    firstNonEmpty(os.Getenv("REDIS_ADDR"), "localhost:6379"),
		RedisDB:      redisDB,
		PollInterval: poll,
		JIRA: jiraConfig{
			Enabled:                 truthy(os.Getenv("JIRA_ENABLED")),
			BaseURL:                 os.Getenv("JIRA_BASE_URL"),
			Username:                os.Getenv("JIRA_USERNAME"),
			APIToken:                os.Getenv("JIRA_API_TOKEN"),
			ProjectKey:              os.Getenv("JIRA_PROJECT_KEY"),
			AcknowledgeTransitionID: os.Getenv("JIRA_ACK_TRANSITION_ID"),
			ResolveTransitionID:     os.Getenv("JIRA_RESOLVE_TRANSITION_ID"),
			ReopenTransitionID:      os.Getenv("JIRA_REOPEN_TRANSITION_ID"),
			ResolutionField:         firstNonEmpty(os.Getenv("JIRA_RESOLUTION_FIELD"), "resolution"),
			ResolutionValue:         firstNonEmpty(os.Getenv("JIRA_RESOLUTION_VALUE"), "Done"),
		},
		Teams: teamsConfig{
			Enabled:            truthy(os.Getenv("TEAMS_ENABLED")),
			TenantID:           os.Getenv("TEAMS_TENANT_ID"),
			ClientID:           os.Getenv("TEAMS_CLIENT_ID"),
			ClientSecret:       os.Getenv("TEAMS_CLIENT_SECRET"),
			TokenEncryptionKey: os.Getenv("TEAMS_TOKEN_ENCRYPTION_KEY"),
		},
	}, nil
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
