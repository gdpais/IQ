package incident

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type IntegrationEvent struct {
	ID              string         `json:"id"`
	IntegrationName string         `json:"integration_name"`
	Type            string         `json:"type"`
	Status          string         `json:"status"`
	Payload         map[string]any `json:"payload"`
	Attempts        int            `json:"attempts"`
	NextRetryAt     *time.Time     `json:"next_retry_at,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

type CreateIntegrationEventRequest struct {
	IntegrationName string
	Type            string
	Status          string
	Payload         map[string]any
	NextRetryAt     *time.Time
}

type IntegrationEventFilter struct {
	IntegrationName string
	Status          string
	Limit           int
}

func (r *Repository) CreateIntegrationEvent(ctx context.Context, req CreateIntegrationEventRequest) (IntegrationEvent, error) {
	if req.IntegrationName == "" {
		req.IntegrationName = "unknown"
	}
	if req.Type == "" {
		req.Type = "unknown"
	}
	if req.Status == "" {
		req.Status = "pending"
	}

	var out IntegrationEvent
	var payload []byte
	err := r.db.QueryRow(ctx, `
		INSERT INTO integration_events (
			id, integration_name, event_type, status, payload, next_retry_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,NOW(),NOW())
		RETURNING id, integration_name, event_type, status, payload, attempts, next_retry_at, created_at, updated_at
	`, uuid.NewString(), req.IntegrationName, req.Type, req.Status, payloadToJSON(req.Payload), req.NextRetryAt).Scan(
		&out.ID, &out.IntegrationName, &out.Type, &out.Status, &payload, &out.Attempts, &out.NextRetryAt, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return IntegrationEvent{}, err
	}
	out.Payload = jsonToMap(payload)
	return out, nil
}

func (r *Repository) CreateRetryableIntegrationEvent(ctx context.Context, integrationName string, eventType string, payload map[string]any) (IntegrationEvent, error) {
	nextRetryAt := time.Now().UTC().Add(1 * time.Minute)
	return r.CreateIntegrationEvent(ctx, CreateIntegrationEventRequest{
		IntegrationName: integrationName,
		Type:            eventType,
		Status:          "pending",
		Payload:         payload,
		NextRetryAt:     &nextRetryAt,
	})
}

func (r *Repository) ListIntegrationEvents(ctx context.Context, filter IntegrationEventFilter) ([]IntegrationEvent, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}

	where := []string{}
	args := []any{}
	addFilter := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if filter.IntegrationName != "" {
		addFilter("integration_name = $%d", filter.IntegrationName)
	}
	if filter.Status != "" {
		addFilter("status = $%d", filter.Status)
	}

	query := `
		SELECT id, integration_name, event_type, status, payload, attempts, next_retry_at, created_at, updated_at
		FROM integration_events`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, filter.Limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []IntegrationEvent{}
	for rows.Next() {
		var out IntegrationEvent
		var payload []byte
		if err := rows.Scan(&out.ID, &out.IntegrationName, &out.Type, &out.Status, &payload, &out.Attempts, &out.NextRetryAt, &out.CreatedAt, &out.UpdatedAt); err != nil {
			return nil, err
		}
		out.Payload = jsonToMap(payload)
		items = append(items, out)
	}
	return items, rows.Err()
}

func (r *Repository) ClaimPendingIntegrationEvents(ctx context.Context, integrationName string, limit int) ([]IntegrationEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}

	tx, err := r.db.Begin(ctx)
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
	`, integrationName, limit)
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
	if len(ids) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}

	events := make([]IntegrationEvent, 0, len(ids))
	for _, id := range ids {
		var out IntegrationEvent
		var payload []byte
		err := tx.QueryRow(ctx, `
			UPDATE integration_events
			SET status = 'processing', attempts = attempts + 1, updated_at = NOW()
			WHERE id = $1
			RETURNING id, integration_name, event_type, status, payload, attempts, next_retry_at, created_at, updated_at
		`, id).Scan(
			&out.ID, &out.IntegrationName, &out.Type, &out.Status, &payload, &out.Attempts, &out.NextRetryAt, &out.CreatedAt, &out.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		out.Payload = jsonToMap(payload)
		events = append(events, out)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return events, nil
}

func (r *Repository) CompleteIntegrationEvent(ctx context.Context, id string, payload map[string]any) error {
	_, err := r.db.Exec(ctx, `
		UPDATE integration_events
		SET status = 'completed', payload = $2, next_retry_at = NULL, updated_at = NOW()
		WHERE id = $1
	`, id, payloadToJSON(payload))
	return err
}

func (r *Repository) RetryIntegrationEvent(ctx context.Context, id string, payload map[string]any, nextRetryAt time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE integration_events
		SET status = 'pending', payload = $2, next_retry_at = $3, updated_at = NOW()
		WHERE id = $1
	`, id, payloadToJSON(payload), nextRetryAt.UTC())
	return err
}

func (r *Repository) FailIntegrationEvent(ctx context.Context, id string, payload map[string]any) error {
	_, err := r.db.Exec(ctx, `
		UPDATE integration_events
		SET status = 'failed', payload = $2, next_retry_at = NULL, updated_at = NOW()
		WHERE id = $1
	`, id, payloadToJSON(payload))
	return err
}
