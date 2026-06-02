package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"incidentiq/apps/api/internal/config"
	"incidentiq/apps/api/internal/incident"
	"incidentiq/apps/api/internal/jira"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	cfg      config.Config
	db       *pgxpool.Pool
	redis    *redis.Client
	incident *incident.Repository
	jira     *jira.Client
	mux      *http.ServeMux
}

func New(cfg config.Config, db *pgxpool.Pool, redisClient *redis.Client) *Server {
	s := &Server{
		cfg:      cfg,
		db:       db,
		redis:    redisClient,
		incident: incident.NewRepository(db),
		jira:     jira.NewClient(cfg.JIRA, nil),
		mux:      http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health/live", s.handleLive)
	s.mux.HandleFunc("GET /health/ready", s.handleReady)
	s.mux.HandleFunc("GET /incidents", s.handleIncidentsList)
	s.mux.HandleFunc("POST /incidents", s.handleIncidentsCreate)
	s.mux.HandleFunc("/incidents/", s.handleIncidentByPath)
	s.mux.HandleFunc("POST /webhooks/dynatrace", s.handleDynatraceWebhook)
	s.mux.HandleFunc("POST /webhooks/elk", s.handleELKWebhook)
	s.mux.HandleFunc("POST /webhooks/generic", s.handleGenericWebhook)
	s.mux.HandleFunc("GET /integrations", s.handleIntegrationsList)
	s.mux.HandleFunc("GET /integration-events", s.handleIntegrationEventsList)
	s.mux.HandleFunc("/integrations/", s.handleIntegrationByPath)
}

func (s *Server) Handler() http.Handler {
	return withRole(s.mux)
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "db_unavailable"})
		return
	}
	if err := s.redis.Ping(ctx).Err(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "redis_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) handleIncidentsList(w http.ResponseWriter, r *http.Request) {
	filter := incident.IncidentListFilter{
		Severity:  r.URL.Query().Get("severity"),
		Service:   r.URL.Query().Get("service"),
		OwnerTeam: firstQueryValue(r, "owner_team", "owner"),
	}

	if status := r.URL.Query().Get("status"); status != "" {
		filter.Status = incident.Status(status)
	}

	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be an integer"))
			return
		}
		filter.Limit = n
	}

	startedAfter, err := parseQueryTime(r, "started_from", "date_from")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	startedBefore, err := parseQueryTime(r, "started_to", "date_to")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	filter.StartedAfter = startedAfter
	filter.StartedBefore = startedBefore

	out, err := s.incident.List(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleIncidentsCreate(w http.ResponseWriter, r *http.Request) {
	var req incident.CreateIncidentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Actor == "" {
		req.Actor = "system"
	}

	out, err := s.incident.Create(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.syncIncidentToJIRA(r.Context(), out, "incident_created")
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleIncidentByPath(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/incidents/")
	path = strings.Trim(path, "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(path, "/")
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.handleIncidentGet(w, r, id)
		case http.MethodPatch:
			s.handleIncidentPatch(w, r, id)
		default:
			http.NotFound(w, r)
		}
		return
	}

	action := parts[1]
	switch action {
	case "acknowledge":
		s.handleTransition(w, r, id, incident.StatusAcknowledged)
	case "resolve":
		s.handleTransition(w, r, id, incident.StatusResolved)
	case "reopen":
		s.handleTransition(w, r, id, incident.StatusOpen)
	case "events":
		s.handleEventCreate(w, r, id)
	case "jira":
		if len(parts) == 3 && parts[2] == "sync" {
			s.handleJIRASync(w, r, id)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleIncidentGet(w http.ResponseWriter, r *http.Request, id string) {
	out, err := s.incident.GetDetail(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleIncidentPatch(w http.ResponseWriter, r *http.Request, id string) {
	var req incident.PatchIncidentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Actor == "" {
		req.Actor = "system"
	}

	out, err := s.incident.Patch(r.Context(), id, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.syncIncidentToJIRA(r.Context(), out, "incident_updated")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTransition(w http.ResponseWriter, r *http.Request, id string, status incident.Status) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	actor := "system"
	if r.URL.Query().Get("actor") != "" {
		actor = r.URL.Query().Get("actor")
	}
	out, err := s.incident.Transition(r.Context(), id, status, actor)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.syncIncidentToJIRA(r.Context(), out, "incident_"+string(status))
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleEventCreate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req incident.AddEventRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Actor == "" {
		req.Actor = "system"
	}
	if req.Type == "" {
		writeErr(w, http.StatusBadRequest, errors.New("type is required"))
		return
	}
	if err := s.incident.InsertEvent(r.Context(), id, req.Type, req.Actor, req.Payload); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created"})
}

func (s *Server) handleDynatraceWebhook(w http.ResponseWriter, r *http.Request) {
	s.handleAlertWebhook(w, r, incident.AlertSourceDynatrace)
}

func (s *Server) handleELKWebhook(w http.ResponseWriter, r *http.Request) {
	s.handleAlertWebhook(w, r, incident.AlertSourceELK)
}

func (s *Server) handleGenericWebhook(w http.ResponseWriter, r *http.Request) {
	s.handleAlertWebhook(w, r, incident.AlertSourceGeneric)
}

func (s *Server) handleAlertWebhook(w http.ResponseWriter, r *http.Request, source string) {
	var payload map[string]any
	if err := decodeJSON(r, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	alert, err := incident.NormalizeAlert(source, payload, time.Now().UTC())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	result, err := s.incident.IngestAlert(r.Context(), alert)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	status := http.StatusOK
	if result.CreatedIncident {
		status = http.StatusCreated
	}
	if result.CreatedIncident || result.Correlated {
		s.syncIncidentToJIRA(r.Context(), result.Incident, "alert_ingested")
	}
	writeJSON(w, status, result)
}

func (s *Server) handleIntegrationsList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []jira.Status{s.jira.Status()})
}

func (s *Server) handleIntegrationByPath(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/integrations/")
	path = strings.Trim(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] != "jira" || parts[1] != "test" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	result, err := s.jira.Test(r.Context())
	status := "completed"
	code := http.StatusOK
	if err != nil {
		status = "failed"
		code = http.StatusBadGateway
		result.Success = false
		result.Message = err.Error()
	}

	_, _ = s.incident.CreateIntegrationEvent(r.Context(), incident.CreateIntegrationEventRequest{
		IntegrationName: "jira",
		Type:            "connectivity_test",
		Status:          status,
		Payload: map[string]any{
			"success":     result.Success,
			"status_code": result.StatusCode,
			"message":     result.Message,
		},
	})

	writeJSON(w, code, result)
}

func (s *Server) handleIntegrationEventsList(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be an integer"))
			return
		}
		limit = n
	}

	out, err := s.incident.ListIntegrationEvents(r.Context(), incident.IntegrationEventFilter{
		IntegrationName: r.URL.Query().Get("integration"),
		Status:          r.URL.Query().Get("status"),
		Limit:           limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleJIRASync(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	inc, err := s.incident.Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	result, err := s.syncIncidentToJIRA(r.Context(), inc, "manual_sync")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type jiraSyncResult struct {
	Enabled bool               `json:"enabled"`
	Action  string             `json:"action,omitempty"`
	JIRA    *incident.JIRALink `json:"jira,omitempty"`
	Retry   bool               `json:"retry"`
	Message string             `json:"message,omitempty"`
}

func (s *Server) syncIncidentToJIRA(ctx context.Context, inc incident.Incident, reason string) (jiraSyncResult, error) {
	status := s.jira.Status()
	if !status.Enabled {
		return jiraSyncResult{Enabled: false, Message: "JIRA integration is disabled"}, nil
	}

	link, err := s.incident.GetJIRALink(ctx, inc.ID)
	if err != nil {
		return jiraSyncResult{Enabled: true, Retry: false, Message: err.Error()}, err
	}

	action := "update"
	if link == nil {
		action = "create"
	}

	issue := jira.IncidentIssue{
		IncidentID:  inc.ID,
		Title:       inc.Title,
		Summary:     inc.Summary,
		Severity:    inc.Severity,
		Service:     inc.Service,
		Environment: inc.Environment,
		Status:      string(inc.Status),
		StartedAt:   inc.StartedAt,
		ResolvedAt:  inc.ResolvedAt,
	}

	if link == nil {
		created, err := s.jira.CreateIssue(ctx, issue)
		if err != nil {
			s.enqueueJIRARetry(ctx, inc, action, reason, err)
			return jiraSyncResult{Enabled: true, Action: action, Retry: true, Message: err.Error()}, err
		}
		stored, err := s.incident.UpsertJIRALink(ctx, inc.ID, created.Key, created.ID)
		if err != nil {
			return jiraSyncResult{Enabled: true, Action: action, Retry: false, Message: err.Error()}, err
		}
		_, _ = s.incident.CreateIntegrationEvent(ctx, incident.CreateIntegrationEventRequest{
			IntegrationName: "jira",
			Type:            "issue_created",
			Status:          "completed",
			Payload: map[string]any{
				"incident_id":    inc.ID,
				"jira_issue_key": stored.JIRAIssueKey,
				"reason":         reason,
			},
		})
		return jiraSyncResult{Enabled: true, Action: action, JIRA: &stored}, nil
	}

	if err := s.jira.UpdateIssue(ctx, link.JIRAIssueKey, issue); err != nil {
		s.enqueueJIRARetry(ctx, inc, action, reason, err)
		return jiraSyncResult{Enabled: true, Action: action, JIRA: link, Retry: true, Message: err.Error()}, err
	}
	_, _ = s.incident.CreateIntegrationEvent(ctx, incident.CreateIntegrationEventRequest{
		IntegrationName: "jira",
		Type:            "issue_updated",
		Status:          "completed",
		Payload: map[string]any{
			"incident_id":    inc.ID,
			"jira_issue_key": link.JIRAIssueKey,
			"reason":         reason,
		},
	})
	return jiraSyncResult{Enabled: true, Action: action, JIRA: link}, nil
}

func (s *Server) enqueueJIRARetry(ctx context.Context, inc incident.Incident, action string, reason string, err error) {
	_, _ = s.incident.CreateRetryableIntegrationEvent(ctx, "jira", "issue_"+action, map[string]any{
		"incident_id": inc.ID,
		"action":      action,
		"reason":      reason,
		"error":       err.Error(),
	})
}

func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(out)
}

func firstQueryValue(r *http.Request, names ...string) string {
	for _, name := range names {
		if value := r.URL.Query().Get(name); value != "" {
			return value
		}
	}
	return ""
}

func parseQueryTime(r *http.Request, names ...string) (*time.Time, error) {
	for _, name := range names {
		value := r.URL.Query().Get(name)
		if value == "" {
			continue
		}

		if t, err := time.Parse(time.RFC3339, value); err == nil {
			t = t.UTC()
			return &t, nil
		}

		t, err := time.Parse("2006-01-02", value)
		if err != nil {
			return nil, fmt.Errorf("%s must be RFC3339 or YYYY-MM-DD", name)
		}
		t = t.UTC()
		if strings.HasSuffix(name, "_to") {
			t = t.AddDate(0, 0, 1)
		}
		return &t, nil
	}
	return nil, nil
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func HealthCheck(ctx context.Context, db *pgxpool.Pool, redisClient *redis.Client) error {
	if err := db.Ping(ctx); err != nil {
		return err
	}
	return redisClient.Ping(ctx).Err()
}
