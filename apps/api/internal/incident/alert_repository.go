package incident

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const correlationWindow = 2 * time.Hour

func (r *Repository) IngestAlert(ctx context.Context, alert NormalizedAlert) (AlertIngestResult, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AlertIngestResult{}, err
	}
	defer tx.Rollback(ctx)

	existingAlert, err := r.getAlertBySourceEventTx(ctx, tx, alert.Source, alert.SourceEventID)
	if err != nil {
		return AlertIngestResult{}, err
	}
	if existingAlert != nil {
		inc, err := r.getIncidentTx(ctx, tx, derefString(existingAlert.IncidentID))
		if err != nil {
			return AlertIngestResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AlertIngestResult{}, err
		}
		return AlertIngestResult{
			Incident:        inc,
			Alert:           *existingAlert,
			CreatedIncident: false,
			Correlated:      true,
			Duplicate:       true,
		}, nil
	}

	incidentID, correlated, err := r.findCorrelatedIncidentTx(ctx, tx, alert)
	if err != nil {
		return AlertIngestResult{}, err
	}

	createdIncident := false
	var inc Incident
	if incidentID == "" {
		inc, err = r.createIncidentFromAlertTx(ctx, tx, alert)
		if err != nil {
			return AlertIngestResult{}, err
		}
		incidentID = inc.ID
		createdIncident = true
	} else {
		inc, err = r.getIncidentTx(ctx, tx, incidentID)
		if err != nil {
			return AlertIngestResult{}, err
		}
	}

	insertedAlert, err := r.insertAlertTx(ctx, tx, incidentID, alert)
	if err != nil {
		return AlertIngestResult{}, err
	}

	eventType := "alert_received"
	if correlated && !createdIncident {
		eventType = "alert_correlated"
	}
	if err := r.insertEventTx(ctx, tx, incidentID, eventType, alert.Source, map[string]any{
		"alert_id":        insertedAlert.ID,
		"source":          alert.Source,
		"source_event_id": alert.SourceEventID,
		"fingerprint":     alert.Fingerprint,
		"severity":        alert.Severity,
	}); err != nil {
		return AlertIngestResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return AlertIngestResult{}, err
	}

	return AlertIngestResult{
		Incident:        inc,
		Alert:           insertedAlert,
		CreatedIncident: createdIncident,
		Correlated:      correlated,
		Duplicate:       false,
	}, nil
}

func (r *Repository) getAlertBySourceEventTx(ctx context.Context, tx pgx.Tx, source string, sourceEventID string) (*Alert, error) {
	var out Alert
	var payload []byte
	err := tx.QueryRow(ctx, `
		SELECT id, incident_id, source, source_event_id, fingerprint, service, environment, severity, redacted_payload, observed_at, created_at
		FROM alerts
		WHERE source = $1 AND source_event_id = $2
	`, source, sourceEventID).Scan(
		&out.ID, &out.IncidentID, &out.Source, &out.SourceEventID, &out.Fingerprint, &out.Service, &out.Environment, &out.Severity, &payload, &out.ObservedAt, &out.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.RedactedPayload = jsonToMap(payload)
	return &out, nil
}

func (r *Repository) findCorrelatedIncidentTx(ctx context.Context, tx pgx.Tx, alert NormalizedAlert) (string, bool, error) {
	var incidentID string
	err := tx.QueryRow(ctx, `
		SELECT i.id
		FROM incidents i
		JOIN alerts a ON a.incident_id = i.id
		WHERE i.status IN ($1, $2)
		  AND a.source = $3
		  AND a.fingerprint = $4
		  AND a.service = $5
		  AND a.environment = $6
		  AND a.severity = $7
		  AND a.observed_at >= $8
		ORDER BY a.observed_at DESC
		LIMIT 1
	`, StatusOpen, StatusAcknowledged, alert.Source, alert.Fingerprint, alert.Service, alert.Environment, alert.Severity, alert.ObservedAt.Add(-correlationWindow)).Scan(&incidentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return incidentID, true, nil
}

func (r *Repository) createIncidentFromAlertTx(ctx context.Context, tx pgx.Tx, alert NormalizedAlert) (Incident, error) {
	id := uuid.NewString()
	now := time.Now().UTC()
	title := alert.Title
	if title == "" {
		title = fmt.Sprintf("[%s] %s alert", alert.Severity, alert.Service)
	}

	var out Incident
	err := tx.QueryRow(ctx, `
		INSERT INTO incidents (
			id, title, summary, severity, service, environment, owner_team, status, started_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		RETURNING id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at, created_at, updated_at
	`, id, title, alert.Summary, alert.Severity, alert.Service, alert.Environment, "unassigned", StatusOpen, alert.ObservedAt, now).Scan(
		&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return Incident{}, err
	}

	if err := r.insertEventTx(ctx, tx, out.ID, "incident_created", alert.Source, map[string]any{
		"title":           title,
		"source":          alert.Source,
		"source_event_id": alert.SourceEventID,
		"fingerprint":     alert.Fingerprint,
	}); err != nil {
		return Incident{}, err
	}

	return out, nil
}

func (r *Repository) insertAlertTx(ctx context.Context, tx pgx.Tx, incidentID string, alert NormalizedAlert) (Alert, error) {
	id := uuid.NewString()
	var out Alert
	var payload []byte
	err := tx.QueryRow(ctx, `
		INSERT INTO alerts (
			id, incident_id, source, source_event_id, fingerprint, service, environment, severity, payload, redacted_payload, observed_at, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NOW())
		RETURNING id, incident_id, source, source_event_id, fingerprint, service, environment, severity, redacted_payload, observed_at, created_at
	`, id, incidentID, alert.Source, alert.SourceEventID, alert.Fingerprint, alert.Service, alert.Environment, alert.Severity, payloadToJSON(alert.Payload), payloadToJSON(alert.RedactedPayload), alert.ObservedAt).Scan(
		&out.ID, &out.IncidentID, &out.Source, &out.SourceEventID, &out.Fingerprint, &out.Service, &out.Environment, &out.Severity, &payload, &out.ObservedAt, &out.CreatedAt,
	)
	if err != nil {
		return Alert{}, err
	}
	out.RedactedPayload = jsonToMap(payload)
	return out, nil
}

func (r *Repository) getIncidentTx(ctx context.Context, tx pgx.Tx, id string) (Incident, error) {
	if id == "" {
		return Incident{}, fmt.Errorf("incident id is required")
	}
	var out Incident
	err := tx.QueryRow(ctx, `
		SELECT id, title, summary, severity, service, environment, owner_team, status, started_at, acknowledged_at, resolved_at, created_at, updated_at
		FROM incidents
		WHERE id = $1
	`, id).Scan(
		&out.ID, &out.Title, &out.Summary, &out.Severity, &out.Service, &out.Environment, &out.OwnerTeam, &out.Status, &out.StartedAt, &out.AcknowledgedAt, &out.ResolvedAt, &out.CreatedAt, &out.UpdatedAt,
	)
	return out, err
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
