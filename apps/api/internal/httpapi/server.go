package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"incidentiq/apps/api/internal/config"
	"incidentiq/apps/api/internal/incident"
	"incidentiq/apps/api/internal/jira"
	"incidentiq/apps/api/internal/reporting"
	"incidentiq/apps/api/internal/tickettemplate"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	cfg       config.Config
	db        *pgxpool.Pool
	redis     *redis.Client
	incident  *incident.Repository
	jira      *jira.Client
	reporting *reporting.Repository
	templates *tickettemplate.Repository
	mux       *http.ServeMux
}

var errInvalidWebhookSignature = errors.New("missing or invalid webhook signature")

func New(cfg config.Config, db *pgxpool.Pool, redisClient *redis.Client) *Server {
	s := &Server{
		cfg:       cfg,
		db:        db,
		redis:     redisClient,
		incident:  incident.NewRepository(db),
		jira:      jira.NewClient(cfg.JIRA, nil),
		reporting: reporting.NewRepository(db),
		templates: tickettemplate.NewRepository(db),
		mux:       http.NewServeMux(),
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
	s.mux.HandleFunc("POST /webhooks/deployments", s.handleDeploymentWebhook)
	s.mux.HandleFunc("GET /integrations", s.handleIntegrationsList)
	s.mux.HandleFunc("GET /integration-events", s.handleIntegrationEventsList)
	s.mux.HandleFunc("/integrations/", s.handleIntegrationByPath)
	s.mux.HandleFunc("GET /ticket-templates", s.handleTicketTemplatesList)
	s.mux.HandleFunc("POST /ticket-templates/validate", s.handleTicketTemplateValidate)
	s.mux.HandleFunc("POST /ticket-templates/preview", s.handleTicketTemplatePreview)
	s.mux.HandleFunc("GET /metrics/live", s.handleMetricsLive)
	s.mux.HandleFunc("GET /reports/sre", s.handleReportSRE)
	s.mux.HandleFunc("GET /reports/dora", s.handleReportDORA)
	s.mux.HandleFunc("GET /reports/executive", s.handleReportExecutive)
	s.mux.HandleFunc("GET /reports/snapshots", s.handleReportSnapshotsList)
	s.mux.HandleFunc("POST /reports/snapshots/materialize", s.handleReportSnapshotsMaterialize)
	s.mux.HandleFunc("GET /exports/incidents.csv", s.handleIncidentsCSV)
}

func (s *Server) Handler() http.Handler {
	return s.withCORS(s.withRateLimit(withRole(s.withAuthorization(s.mux))))
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, origin := range strings.Split(s.cfg.CORSAllowedOrigins, ",") {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			allowed[origin] = true
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowed["*"] || allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if allowed["*"] {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Actor, X-Hub-Signature-256, X-IncidentIQ-Signature, X-Role, X-Signature")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withAuthorization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) || strings.HasPrefix(r.URL.Path, "/webhooks/") {
			next.ServeHTTP(w, r)
			return
		}
		if s.cfg.APIAuthToken != "" && !validBearerToken(r.Header.Get("Authorization"), s.cfg.APIAuthToken) {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid API token"))
			return
		}
		if !roleAllowed(roleFromContext(r.Context()), r.Method, r.URL.Path) {
			writeErr(w, http.StatusForbidden, errors.New("role is not allowed to perform this action"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := s.cfg.RateLimit.APIRequestsPerMinute
		scope := "api"
		if strings.HasPrefix(r.URL.Path, "/webhooks/") {
			limit = s.cfg.RateLimit.WebhookRequestsPerMinute
			scope = "webhook"
		}
		if limit > 0 && s.redis != nil {
			limited, err := s.rateLimited(r.Context(), scope, clientIP(r), limit)
			if err == nil && limited {
				writeErr(w, http.StatusTooManyRequests, errors.New("rate limit exceeded"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimited(ctx context.Context, scope string, client string, limit int) (bool, error) {
	window := time.Now().UTC().Format("200601021504")
	key := "incidentiq:rate:" + scope + ":" + client + ":" + window
	count, err := s.redis.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	if count == 1 {
		_ = s.redis.Expire(ctx, key, 2*time.Minute).Err()
	}
	return count > int64(limit), nil
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
	if strings.TrimSpace(req.Title) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("title is required"))
		return
	}
	if strings.TrimSpace(req.Severity) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("severity is required"))
		return
	}
	if strings.TrimSpace(req.Service) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("service is required"))
		return
	}
	if strings.TrimSpace(req.Environment) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("environment is required"))
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
	if err := s.auditAction(r.Context(), req.Actor, "incident_created", "incident", out.ID, map[string]any{"severity": out.Severity, "service": out.Service}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
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
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, errors.New("incident not found"))
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
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
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, errors.New("incident not found"))
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	s.syncIncidentToJIRA(r.Context(), out, "incident_updated")
	if err := s.auditAction(r.Context(), req.Actor, "incident_updated", "incident", out.ID, map[string]any{"severity": out.Severity, "status": out.Status}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTransition(w http.ResponseWriter, r *http.Request, id string, status incident.Status) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	actor := actorFromRequest(r)
	out, err := s.incident.Transition(r.Context(), id, status, actor)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.syncIncidentToJIRA(r.Context(), out, "incident_"+string(status))
	if err := s.auditAction(r.Context(), actor, "incident_"+string(status), "incident", out.ID, map[string]any{"status": out.Status}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
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
	if err := s.auditAction(r.Context(), req.Actor, "incident_event_created", "incident", id, map[string]any{"event_type": req.Type}); err != nil {
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

func (s *Server) webhookSecret(source string) string {
	switch source {
	case incident.AlertSourceDynatrace:
		return s.cfg.Webhooks.DynatraceSecret
	case incident.AlertSourceELK:
		return s.cfg.Webhooks.ELKSecret
	case incident.AlertSourceGeneric:
		return s.cfg.Webhooks.GenericSecret
	case "deployments":
		if s.cfg.Webhooks.DeploymentSecret != "" {
			return s.cfg.Webhooks.DeploymentSecret
		}
		return s.cfg.Webhooks.GenericSecret
	default:
		return ""
	}
}

func (s *Server) handleDeploymentWebhook(w http.ResponseWriter, r *http.Request) {
	var req reporting.DeploymentRequest
	if err := decodeSignedJSON(r, s.webhookSecret("deployments"), &req); err != nil {
		if errors.Is(err, errInvalidWebhookSignature) {
			writeErr(w, http.StatusUnauthorized, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.reporting.IngestDeployment(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.auditAction(r.Context(), "webhook", "deployment_ingested", "deployment", out.ID, map[string]any{"service": out.Service, "environment": out.Environment, "incident_id": out.IncidentID}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleAlertWebhook(w http.ResponseWriter, r *http.Request, source string) {
	var payload map[string]any
	if err := decodeSignedJSON(r, s.webhookSecret(source), &payload); err != nil {
		if errors.Is(err, errInvalidWebhookSignature) {
			writeErr(w, http.StatusUnauthorized, err)
			return
		}
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
	if err := s.auditAction(r.Context(), source, "alert_ingested", "incident", result.Incident.ID, map[string]any{"alert_id": result.Alert.ID, "duplicate": result.Duplicate, "correlated": result.Correlated}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
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
	if err := s.auditAction(r.Context(), actorFromRequest(r), "integration_tested", "integration", "jira", map[string]any{"success": result.Success, "status": status}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

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
	if err := s.auditAction(r.Context(), actorFromRequest(r), "jira_sync_requested", "incident", id, map[string]any{"retry": result.Retry, "action": result.Action}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type ticketTemplateValidateRequest struct {
	TemplateYAML string `json:"template_yaml"`
}

type ticketTemplatePreviewRequest struct {
	TemplateYAML string                 `json:"template_yaml"`
	Context      tickettemplate.Context `json:"context"`
}

func (s *Server) handleTicketTemplatesList(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be an integer"))
			return
		}
		limit = n
	}
	out, err := s.templates.ListVersions(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTicketTemplateValidate(w http.ResponseWriter, r *http.Request) {
	var req ticketTemplateValidateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	doc, validationErrors, err := tickettemplate.Validate(req.TemplateYAML)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.auditAction(r.Context(), actorFromRequest(r), "ticket_template_validated", "ticket_template", fmt.Sprintf("version-%d", doc.Version), map[string]any{"valid": len(validationErrors) == 0, "overrides": len(doc.Overrides)}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":     len(validationErrors) == 0,
		"errors":    validationErrors,
		"version":   doc.Version,
		"overrides": len(doc.Overrides),
	})
}

func (s *Server) handleTicketTemplatePreview(w http.ResponseWriter, r *http.Request) {
	var req ticketTemplatePreviewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	raw := req.TemplateYAML
	if strings.TrimSpace(raw) == "" {
		latest, err := s.templates.LatestGlobalTemplate(r.Context())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeErr(w, http.StatusBadRequest, errors.New("template_yaml is required when no stored template exists"))
				return
			}
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		raw = latest
	}

	rendered, validationErrors, err := tickettemplate.Render(raw, req.Context)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(validationErrors) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"valid":  false,
			"errors": validationErrors,
		})
		return
	}
	if err := s.auditAction(r.Context(), actorFromRequest(r), "ticket_template_previewed", "ticket_template", "preview", map[string]any{"service": req.Context.Service, "severity": req.Context.Severity}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":    true,
		"rendered": rendered,
	})
}

func (s *Server) handleMetricsLive(w http.ResponseWriter, r *http.Request) {
	filter, err := reportingFilterFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.reporting.LiveMetrics(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReportSRE(w http.ResponseWriter, r *http.Request) {
	filter, err := reportingFilterFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.reporting.SREReport(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReportDORA(w http.ResponseWriter, r *http.Request) {
	filter, err := reportingFilterFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.reporting.DORAReport(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReportExecutive(w http.ResponseWriter, r *http.Request) {
	filter, err := reportingFilterFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.reporting.ExecutiveReport(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReportSnapshotsList(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be an integer"))
			return
		}
		limit = n
	}

	out, err := s.reporting.ListSnapshots(r.Context(), r.URL.Query().Get("type"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReportSnapshotsMaterialize(w http.ResponseWriter, r *http.Request) {
	filter, err := reportingFilterFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req := reporting.SnapshotRequest{Filter: filter}
	if reportTypes := reportTypesFromQuery(r); len(reportTypes) > 0 {
		req.ReportTypes = reportTypes
	}
	if r.ContentLength != 0 {
		var body reporting.SnapshotRequest
		if err := decodeJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if len(body.ReportTypes) > 0 {
			req.ReportTypes = body.ReportTypes
		}
		req.Filter = mergeReportingFilters(req.Filter, body.Filter)
	}

	out, err := s.reporting.MaterializeSnapshots(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.auditAction(r.Context(), actorFromRequest(r), "report_snapshots_materialized", "report_snapshot", "batch", map[string]any{"count": len(out)}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleIncidentsCSV(w http.ResponseWriter, r *http.Request) {
	filter, err := reportingFilterFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rows, err := s.reporting.IncidentCSVRows(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="incidents.csv"`)
	w.WriteHeader(http.StatusOK)

	writer := csv.NewWriter(w)
	_ = writer.Write([]string{"id", "title", "severity", "service", "environment", "owner_team", "status", "started_at", "acknowledged_at", "resolved_at", "alert_count", "reopen_count"})
	for _, row := range rows {
		_ = writer.Write([]string{
			row.ID,
			row.Title,
			row.Severity,
			row.Service,
			row.Environment,
			row.OwnerTeam,
			row.Status,
			formatCSVTime(&row.StartedAt),
			formatCSVTime(row.AcknowledgedAt),
			formatCSVTime(row.ResolvedAt),
			strconv.FormatInt(row.AlertCount, 10),
			strconv.FormatInt(row.ReopenCount, 10),
		})
	}
	writer.Flush()
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

func (s *Server) auditAction(ctx context.Context, actor string, action string, resourceType string, resourceID string, metadata map[string]any) error {
	if s.db == nil {
		return nil
	}
	if strings.TrimSpace(actor) == "" {
		actor = "system"
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO audit_log (id, actor, action, resource_type, resource_id, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,NOW())
	`, uuid.NewString(), actor, action, resourceType, resourceID, mapPayloadToJSON(metadata))
	return err
}

func actorFromRequest(r *http.Request) string {
	if actor := strings.TrimSpace(r.Header.Get("X-Actor")); actor != "" {
		return actor
	}
	if role := strings.TrimSpace(r.Header.Get("X-Role")); role != "" {
		return role
	}
	return "system"
}

func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(out)
}

func decodeSignedJSON(r *http.Request, secret string, out any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil {
		return err
	}
	if secret != "" && !validWebhookSignature(r, secret, body) {
		return errInvalidWebhookSignature
	}
	return json.Unmarshal(body, out)
}

func validWebhookSignature(r *http.Request, secret string, body []byte) bool {
	signature := firstHeaderValue(r, "X-IncidentIQ-Signature", "X-Hub-Signature-256", "X-Signature")
	if signature == "" {
		return false
	}
	signature = strings.TrimSpace(signature)
	signature = strings.TrimPrefix(signature, "sha256=")
	expectedMAC := hmac.New(sha256.New, []byte(secret))
	_, _ = expectedMAC.Write(body)
	expected := expectedMAC.Sum(nil)
	actual, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	return hmac.Equal(actual, expected)
}

func firstHeaderValue(r *http.Request, names ...string) string {
	for _, name := range names {
		if value := r.Header.Get(name); value != "" {
			return value
		}
	}
	return ""
}

func validBearerToken(header string, expected string) bool {
	prefix := "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return hmac.Equal([]byte(token), []byte(expected))
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
		if first != "" {
			return first
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr == "" {
		return "unknown"
	}
	return r.RemoteAddr
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
		if isUpperBoundTimeParam(name) {
			t = t.AddDate(0, 0, 1)
		}
		return &t, nil
	}
	return nil, nil
}

func isUpperBoundTimeParam(name string) bool {
	return strings.HasSuffix(name, "_to") || strings.HasSuffix(name, "_before") || name == "to"
}

func reportingFilterFromRequest(r *http.Request) (reporting.Filter, error) {
	from, err := parseQueryTime(r, "started_from", "date_from", "from")
	if err != nil {
		return reporting.Filter{}, err
	}
	to, err := parseQueryTime(r, "started_to", "date_to", "to")
	if err != nil {
		return reporting.Filter{}, err
	}
	return reporting.Filter{
		Severity:    r.URL.Query().Get("severity"),
		Service:     r.URL.Query().Get("service"),
		Environment: r.URL.Query().Get("environment"),
		OwnerTeam:   firstQueryValue(r, "owner_team", "owner", "team"),
		Status:      r.URL.Query().Get("status"),
		From:        from,
		To:          to,
	}, nil
}

func reportTypesFromQuery(r *http.Request) []string {
	raw := firstQueryValue(r, "type", "report_type")
	if raw == "" {
		return nil
	}
	values := strings.Split(raw, ",")
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func mergeReportingFilters(base reporting.Filter, override reporting.Filter) reporting.Filter {
	if override.Severity != "" {
		base.Severity = override.Severity
	}
	if override.Service != "" {
		base.Service = override.Service
	}
	if override.Environment != "" {
		base.Environment = override.Environment
	}
	if override.OwnerTeam != "" {
		base.OwnerTeam = override.OwnerTeam
	}
	if override.Status != "" {
		base.Status = override.Status
	}
	if override.From != nil {
		base.From = override.From
	}
	if override.To != nil {
		base.To = override.To
	}
	return base
}

func formatCSVTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func mapPayloadToJSON(payload map[string]any) []byte {
	if payload == nil {
		payload = map[string]any{}
	}
	data, _ := json.Marshal(payload)
	return data
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
