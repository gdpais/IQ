package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Create(ctx context.Context, req CreateIncidentRequest) (Incident, error) {
	id := uuid.NewString()
	now := time.Now().UTC()

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Incident{}, err
	}
	defer tx.Rollback(ctx)

	var out Incident
	err = tx.QueryRow(ctx, `
		INSERT INTO incidents (
			id, title, summary, severity, service, environment, owner_team, status, started_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		RETURNING id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at, created_at, updated_at
	`, id, req.Title, req.Summary, req.Severity, req.Service, req.Environment, req.OwnerTeam, StatusOpen, now, now).Scan(
		&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return Incident{}, err
	}

	payload := map[string]any{
		"title": req.Title,
	}
	if err := r.insertEventTx(ctx, tx, out.ID, "incident_created", req.Actor, payload); err != nil {
		return Incident{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Incident{}, err
	}
	return out, nil
}

func (r *Repository) Get(ctx context.Context, id string) (Incident, error) {
	var out Incident
	err := r.db.QueryRow(ctx, `
		SELECT id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at, created_at, updated_at
		FROM incidents WHERE id = $1
	`, id).Scan(
		&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt, &out.CreatedAt, &out.UpdatedAt,
	)
	return out, err
}

func (r *Repository) GetDetail(ctx context.Context, id string) (IncidentDetail, error) {
	inc, err := r.Get(ctx, id)
	if err != nil {
		return IncidentDetail{}, err
	}

	events, err := r.ListEvents(ctx, id)
	if err != nil {
		return IncidentDetail{}, err
	}

	alerts, err := r.ListAlerts(ctx, id)
	if err != nil {
		return IncidentDetail{}, err
	}

	jira, err := r.GetJIRALink(ctx, id)
	if err != nil {
		return IncidentDetail{}, err
	}

	return IncidentDetail{
		Incident: inc,
		Events:   events,
		Alerts:   alerts,
		JIRA:     jira,
	}, nil
}

func (r *Repository) List(ctx context.Context, filter IncidentListFilter) ([]Incident, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}

	where := []string{}
	args := []any{}
	addFilter := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if filter.Severity != "" {
		addFilter("severity = $%d", filter.Severity)
	}
	if filter.Service != "" {
		addFilter("service = $%d", filter.Service)
	}
	if filter.Status != "" {
		addFilter("status = $%d", filter.Status)
	}
	if filter.OwnerTeam != "" {
		addFilter("owner_team = $%d", filter.OwnerTeam)
	}
	if filter.StartedAfter != nil {
		addFilter("started_at >= $%d", *filter.StartedAfter)
	}
	if filter.StartedBefore != nil {
		addFilter("started_at < $%d", *filter.StartedBefore)
	}

	sql := `
		SELECT id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at, created_at, updated_at
		FROM incidents`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, filter.Limit)
	sql += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Incident{}
	for rows.Next() {
		var out Incident
		if err := rows.Scan(
			&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt, &out.CreatedAt, &out.UpdatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, out)
	}
	return items, rows.Err()
}

func (r *Repository) ListEvents(ctx context.Context, incidentID string) ([]Event, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, incident_id, event_type, actor, payload, created_at
		FROM incident_events
		WHERE incident_id = $1
		ORDER BY created_at ASC
	`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Event{}
	for rows.Next() {
		var out Event
		var payload []byte
		if err := rows.Scan(&out.ID, &out.IncidentID, &out.Type, &out.Actor, &payload, &out.CreatedAt); err != nil {
			return nil, err
		}
		out.Payload = jsonToMap(payload)
		items = append(items, out)
	}
	return items, rows.Err()
}

func (r *Repository) ListAlerts(ctx context.Context, incidentID string) ([]Alert, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, incident_id, source, source_event_id, fingerprint, service, environment, severity, redacted_payload, observed_at, created_at
		FROM alerts
		WHERE incident_id = $1
		ORDER BY observed_at ASC
	`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Alert{}
	for rows.Next() {
		var out Alert
		var payload []byte
		if err := rows.Scan(
			&out.ID, &out.IncidentID, &out.Source, &out.SourceEventID, &out.Fingerprint, &out.Service, &out.Environment, &out.Severity, &payload, &out.ObservedAt, &out.CreatedAt,
		); err != nil {
			return nil, err
		}
		out.RedactedPayload = jsonToMap(payload)
		items = append(items, out)
	}
	return items, rows.Err()
}

func (r *Repository) GetJIRALink(ctx context.Context, incidentID string) (*JIRALink, error) {
	var out JIRALink
	err := r.db.QueryRow(ctx, `
		SELECT incident_id, jira_issue_key, jira_issue_id, created_at, updated_at
		FROM jira_links
		WHERE incident_id = $1
	`, incidentID).Scan(&out.IncidentID, &out.JIRAIssueKey, &out.JIRAIssueID, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *Repository) Patch(ctx context.Context, id string, req PatchIncidentRequest) (Incident, error) {
	current, err := r.Get(ctx, id)
	if err != nil {
		return Incident{}, err
	}

	title := current.Title
	summary := current.Summary
	severity := current.Severity
	ownerTeam := current.OwnerTeam
	if req.Title != nil {
		title = *req.Title
	}
	if req.Summary != nil {
		summary = *req.Summary
	}
	if req.Severity != nil {
		severity = *req.Severity
	}
	if req.OwnerTeam != nil {
		ownerTeam = *req.OwnerTeam
	}

	var out Incident
	err = r.db.QueryRow(ctx, `
		UPDATE incidents
		SET title=$2, summary=$3, severity=$4, owner_team=$5, updated_at=NOW()
		WHERE id=$1
		RETURNING id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at, created_at, updated_at
	`, id, title, summary, severity, ownerTeam).Scan(
		&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return Incident{}, err
	}

	payload := map[string]any{"patched": true}
	if err := r.InsertEvent(ctx, out.ID, "incident_patched", req.Actor, payload); err != nil {
		return Incident{}, err
	}

	return out, nil
}

func (r *Repository) Transition(ctx context.Context, id string, target Status, actor string) (Incident, error) {
	current, err := r.Get(ctx, id)
	if err != nil {
		return Incident{}, err
	}

	switch target {
	case StatusAcknowledged:
		if current.Status != StatusOpen {
			return Incident{}, fmt.Errorf("invalid transition from %s to %s", current.Status, target)
		}
	case StatusResolved:
		if current.Status != StatusAcknowledged && current.Status != StatusOpen {
			return Incident{}, fmt.Errorf("invalid transition from %s to %s", current.Status, target)
		}
	case StatusOpen:
		if current.Status != StatusResolved {
			return Incident{}, fmt.Errorf("invalid transition from %s to %s", current.Status, target)
		}
	}

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Incident{}, err
	}
	defer tx.Rollback(ctx)

	var out Incident
	switch target {
	case StatusAcknowledged:
		err = tx.QueryRow(ctx, `
			UPDATE incidents
			SET status=$2, acknowledged_at=COALESCE(acknowledged_at, NOW()), updated_at=NOW()
			WHERE id=$1
			RETURNING id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at, created_at, updated_at
		`, id, target).Scan(
			&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt, &out.CreatedAt, &out.UpdatedAt,
		)
	case StatusResolved:
		err = tx.QueryRow(ctx, `
			UPDATE incidents
			SET status=$2, resolved_at=COALESCE(resolved_at, NOW()), updated_at=NOW()
			WHERE id=$1
			RETURNING id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at, created_at, updated_at
		`, id, target).Scan(
			&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt, &out.CreatedAt, &out.UpdatedAt,
		)
	case StatusOpen:
		err = tx.QueryRow(ctx, `
			UPDATE incidents
			SET status=$2, resolved_at=NULL, updated_at=NOW()
			WHERE id=$1
			RETURNING id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at, created_at, updated_at
		`, id, target).Scan(
			&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt, &out.CreatedAt, &out.UpdatedAt,
		)
	}
	if err != nil {
		return Incident{}, err
	}

	eventType := "incident_state_changed"
	switch target {
	case StatusAcknowledged:
		eventType = "incident_acknowledged"
	case StatusResolved:
		eventType = "incident_resolved"
	case StatusOpen:
		eventType = "incident_reopened"
	}
	payload := map[string]any{
		"status": string(target),
	}
	if err := r.insertEventTx(ctx, tx, id, eventType, actor, payload); err != nil {
		return Incident{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Incident{}, err
	}
	return out, nil
}

func (r *Repository) InsertEvent(ctx context.Context, incidentID string, eventType string, actor string, payload map[string]any) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO incident_events (id, incident_id, event_type, actor, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,NOW())
	`, uuid.NewString(), incidentID, eventType, actor, payloadToJSON(payload))
	return err
}

func (r *Repository) insertEventTx(ctx context.Context, tx pgx.Tx, incidentID string, eventType string, actor string, payload map[string]any) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO incident_events (id, incident_id, event_type, actor, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,NOW())
	`, uuid.NewString(), incidentID, eventType, actor, payloadToJSON(payload))
	return err
}

func payloadToJSON(payload map[string]any) []byte {
	if payload == nil {
		payload = map[string]any{}
	}
	data, _ := json.Marshal(payload)
	return data
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
