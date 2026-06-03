package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) GetTeamsAuthState(ctx context.Context) (*TeamsAuthState, error) {
	var out TeamsAuthState
	err := r.db.QueryRow(ctx, `
		SELECT integration_name, sender_display_name, sender_upn, access_token_encrypted, refresh_token_encrypted, scopes, expires_at, created_at, updated_at
		FROM teams_auth_state
		WHERE integration_name = $1
	`, TeamsIntegrationName).Scan(
		&out.IntegrationName, &out.SenderDisplay, &out.SenderUPN, &out.AccessToken, &out.RefreshToken, &out.Scopes, &out.ExpiresAt, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *Repository) UpsertTeamsAuthState(ctx context.Context, req UpsertTeamsAuthStateRequest) (TeamsAuthState, error) {
	var out TeamsAuthState
	err := r.db.QueryRow(ctx, `
		INSERT INTO teams_auth_state (
			integration_name, sender_display_name, sender_upn, access_token_encrypted, refresh_token_encrypted, scopes, expires_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,NOW(),NOW())
		ON CONFLICT (integration_name) DO UPDATE
		SET sender_display_name = EXCLUDED.sender_display_name,
		    sender_upn = EXCLUDED.sender_upn,
		    access_token_encrypted = EXCLUDED.access_token_encrypted,
		    refresh_token_encrypted = EXCLUDED.refresh_token_encrypted,
		    scopes = EXCLUDED.scopes,
		    expires_at = EXCLUDED.expires_at,
		    updated_at = NOW()
		RETURNING integration_name, sender_display_name, sender_upn, access_token_encrypted, refresh_token_encrypted, scopes, expires_at, created_at, updated_at
	`, TeamsIntegrationName, req.SenderDisplay, req.SenderUPN, req.AccessToken, req.RefreshToken, req.Scopes, req.ExpiresAt).Scan(
		&out.IntegrationName, &out.SenderDisplay, &out.SenderUPN, &out.AccessToken, &out.RefreshToken, &out.Scopes, &out.ExpiresAt, &out.CreatedAt, &out.UpdatedAt,
	)
	return out, err
}

func (r *Repository) ListTeamsRoutes(ctx context.Context) ([]TeamsRoute, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, name, enabled, team_id, team_name, channel_id, channel_name, owner_team, service, environment, severity_min, created_by, updated_by, created_at, updated_at
		FROM teams_routes
		ORDER BY name ASC, created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	routes := []TeamsRoute{}
	for rows.Next() {
		var out TeamsRoute
		if err := rows.Scan(
			&out.ID, &out.Name, &out.Enabled, &out.TeamID, &out.TeamName, &out.ChannelID, &out.ChannelName, &out.OwnerTeam, &out.Service, &out.Environment, &out.SeverityMin, &out.CreatedBy, &out.UpdatedBy, &out.CreatedAt, &out.UpdatedAt,
		); err != nil {
			return nil, err
		}
		routes = append(routes, out)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return routes, nil
	}

	recipients, err := r.listTeamsRecipientsByRouteIDs(ctx, routeIDs(routes))
	if err != nil {
		return nil, err
	}
	for i := range routes {
		routes[i].Recipients = recipients[routes[i].ID]
	}
	return routes, nil
}

func (r *Repository) GetTeamsRoute(ctx context.Context, id string) (*TeamsRoute, error) {
	var out TeamsRoute
	err := r.db.QueryRow(ctx, `
		SELECT id, name, enabled, team_id, team_name, channel_id, channel_name, owner_team, service, environment, severity_min, created_by, updated_by, created_at, updated_at
		FROM teams_routes
		WHERE id = $1
	`, id).Scan(
		&out.ID, &out.Name, &out.Enabled, &out.TeamID, &out.TeamName, &out.ChannelID, &out.ChannelName, &out.OwnerTeam, &out.Service, &out.Environment, &out.SeverityMin, &out.CreatedBy, &out.UpdatedBy, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	recipients, err := r.listTeamsRecipientsByRouteIDs(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	out.Recipients = recipients[id]
	return &out, nil
}

func (r *Repository) CreateTeamsRoute(ctx context.Context, req CreateTeamsRouteRequest) (TeamsRoute, error) {
	if err := validateTeamsRouteRequest(req.Name, req.TeamID, req.ChannelID, req.Recipients); err != nil {
		return TeamsRoute{}, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return TeamsRoute{}, err
	}
	defer tx.Rollback(ctx)

	id := uuid.NewString()
	var out TeamsRoute
	err = tx.QueryRow(ctx, `
		INSERT INTO teams_routes (
			id, name, enabled, team_id, team_name, channel_id, channel_name, owner_team, service, environment, severity_min, created_by, updated_by, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12,NOW(),NOW())
		RETURNING id, name, enabled, team_id, team_name, channel_id, channel_name, owner_team, service, environment, severity_min, created_by, updated_by, created_at, updated_at
	`, id, req.Name, req.Enabled, req.TeamID, req.TeamName, req.ChannelID, req.ChannelName, req.OwnerTeam, req.Service, req.Environment, req.SeverityMin, actorOrSystem(req.Actor)).Scan(
		&out.ID, &out.Name, &out.Enabled, &out.TeamID, &out.TeamName, &out.ChannelID, &out.ChannelName, &out.OwnerTeam, &out.Service, &out.Environment, &out.SeverityMin, &out.CreatedBy, &out.UpdatedBy, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return TeamsRoute{}, err
	}
	if err := insertTeamsRecipientsTx(ctx, tx, out.ID, req.Recipients); err != nil {
		return TeamsRoute{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamsRoute{}, err
	}
	full, err := r.GetTeamsRoute(ctx, out.ID)
	if err != nil {
		return TeamsRoute{}, err
	}
	if full == nil {
		return TeamsRoute{}, nil
	}
	return *full, nil
}

func (r *Repository) UpdateTeamsRoute(ctx context.Context, id string, req UpdateTeamsRouteRequest) (TeamsRoute, error) {
	if err := validateTeamsRouteRequest(req.Name, req.TeamID, req.ChannelID, req.Recipients); err != nil {
		return TeamsRoute{}, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return TeamsRoute{}, err
	}
	defer tx.Rollback(ctx)

	var out TeamsRoute
	err = tx.QueryRow(ctx, `
		UPDATE teams_routes
		SET name = $2,
		    enabled = $3,
		    team_id = $4,
		    team_name = $5,
		    channel_id = $6,
		    channel_name = $7,
		    owner_team = $8,
		    service = $9,
		    environment = $10,
		    severity_min = $11,
		    updated_by = $12,
		    updated_at = NOW()
		WHERE id = $1
		RETURNING id, name, enabled, team_id, team_name, channel_id, channel_name, owner_team, service, environment, severity_min, created_by, updated_by, created_at, updated_at
	`, id, req.Name, req.Enabled, req.TeamID, req.TeamName, req.ChannelID, req.ChannelName, req.OwnerTeam, req.Service, req.Environment, req.SeverityMin, actorOrSystem(req.Actor)).Scan(
		&out.ID, &out.Name, &out.Enabled, &out.TeamID, &out.TeamName, &out.ChannelID, &out.ChannelName, &out.OwnerTeam, &out.Service, &out.Environment, &out.SeverityMin, &out.CreatedBy, &out.UpdatedBy, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return TeamsRoute{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM teams_route_recipients WHERE route_id = $1`, id); err != nil {
		return TeamsRoute{}, err
	}
	if err := insertTeamsRecipientsTx(ctx, tx, id, req.Recipients); err != nil {
		return TeamsRoute{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamsRoute{}, err
	}
	full, err := r.GetTeamsRoute(ctx, out.ID)
	if err != nil {
		return TeamsRoute{}, err
	}
	if full == nil {
		return TeamsRoute{}, nil
	}
	return *full, nil
}

func (r *Repository) DeleteTeamsRoute(ctx context.Context, id string) error {
	cmd, err := r.db.Exec(ctx, `DELETE FROM teams_routes WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *Repository) MatchTeamsRoutes(ctx context.Context, inc Incident) ([]TeamsRoute, error) {
	routes, err := r.ListTeamsRoutes(ctx)
	if err != nil {
		return nil, err
	}
	matched := []TeamsRoute{}
	for _, route := range routes {
		if !route.Enabled {
			continue
		}
		if route.OwnerTeam != "" && !strings.EqualFold(route.OwnerTeam, inc.OwnerTeam) {
			continue
		}
		if route.Service != "" && !strings.EqualFold(route.Service, inc.Service) {
			continue
		}
		if route.Environment != "" && !strings.EqualFold(route.Environment, inc.Environment) {
			continue
		}
		if !meetsSeverity(route.SeverityMin, inc.Severity) {
			continue
		}
		matched = append(matched, route)
	}
	return matched, nil
}

func (r *Repository) CreateTeamsDelivery(ctx context.Context, incidentID string, channelID string, reason string, messageID string, payload map[string]any) (bool, error) {
	tag := uuid.NewString()
	_, err := r.db.Exec(ctx, `
		INSERT INTO incident_notification_deliveries (
			id, integration_name, incident_id, channel_id, notification_reason, message_id, payload, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,NOW())
		ON CONFLICT (integration_name, incident_id, channel_id, notification_reason) DO NOTHING
	`, tag, TeamsIntegrationName, incidentID, channelID, reason, messageID, payloadToJSON(payload))
	if err != nil {
		return false, err
	}
	var exists bool
	err = r.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM incident_notification_deliveries
			WHERE integration_name = $1 AND incident_id = $2 AND channel_id = $3 AND notification_reason = $4 AND id = $5
		)
	`, TeamsIntegrationName, incidentID, channelID, reason, tag).Scan(&exists)
	return exists, err
}

func (r *Repository) ListTeamsDeliveries(ctx context.Context, incidentID string) ([]TeamsDelivery, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, integration_name, incident_id, channel_id, notification_reason, message_id, payload, created_at
		FROM incident_notification_deliveries
		WHERE incident_id = $1
		ORDER BY created_at ASC
	`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []TeamsDelivery{}
	for rows.Next() {
		var out TeamsDelivery
		var payload []byte
		if err := rows.Scan(&out.ID, &out.IntegrationName, &out.IncidentID, &out.ChannelID, &out.NotificationReason, &out.MessageID, &payload, &out.CreatedAt); err != nil {
			return nil, err
		}
		out.Payload = jsonToMap(payload)
		items = append(items, out)
	}
	return items, rows.Err()
}

func insertTeamsRecipientsTx(ctx context.Context, tx pgx.Tx, routeID string, recipients []CreateTeamsRouteRecipient) error {
	for _, recipient := range recipients {
		if strings.TrimSpace(recipient.Type) == "" || strings.TrimSpace(recipient.TeamsObjectID) == "" || strings.TrimSpace(recipient.DisplayName) == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO teams_route_recipients (id, route_id, recipient_type, teams_object_id, display_name, upn, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,NOW())
		`, uuid.NewString(), routeID, recipient.Type, recipient.TeamsObjectID, recipient.DisplayName, recipient.UPN); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) listTeamsRecipientsByRouteIDs(ctx context.Context, ids []string) (map[string][]TeamsRouteRecipient, error) {
	result := map[string][]TeamsRouteRecipient{}
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, route_id, recipient_type, teams_object_id, display_name, upn, created_at
		FROM teams_route_recipients
		WHERE route_id = ANY($1)
		ORDER BY created_at ASC
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var out TeamsRouteRecipient
		if err := rows.Scan(&out.ID, &out.RouteID, &out.Type, &out.TeamsObjectID, &out.DisplayName, &out.UPN, &out.CreatedAt); err != nil {
			return nil, err
		}
		result[out.RouteID] = append(result[out.RouteID], out)
	}
	return result, rows.Err()
}

func routeIDs(routes []TeamsRoute) []string {
	ids := make([]string, 0, len(routes))
	for _, route := range routes {
		ids = append(ids, route.ID)
	}
	return ids
}

func meetsSeverity(minimum string, actual string) bool {
	if strings.TrimSpace(minimum) == "" {
		return true
	}
	return severityOrder(actual) >= severityOrder(minimum)
}

func severityOrder(value string) int {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func actorOrSystem(actor string) string {
	if strings.TrimSpace(actor) == "" {
		return "system"
	}
	return actor
}

func validateTeamsRouteRequest(name string, teamID string, channelID string, recipients []CreateTeamsRouteRecipient) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(teamID) == "" {
		return fmt.Errorf("team_id is required")
	}
	if strings.TrimSpace(channelID) == "" {
		return fmt.Errorf("channel_id is required")
	}
	validRecipients := 0
	for _, recipient := range recipients {
		if strings.TrimSpace(recipient.DisplayName) == "" || strings.TrimSpace(recipient.TeamsObjectID) == "" {
			continue
		}
		if recipient.Type != TeamsRecipientTypeUser && recipient.Type != TeamsRecipientTypeTag {
			return fmt.Errorf("recipient type must be %q or %q", TeamsRecipientTypeUser, TeamsRecipientTypeTag)
		}
		validRecipients++
	}
	if validRecipients == 0 {
		return fmt.Errorf("at least one recipient is required")
	}
	return nil
}
