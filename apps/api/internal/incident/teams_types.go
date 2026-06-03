package incident

import "time"

type TeamsAuthState struct {
	IntegrationName string     `json:"integration_name"`
	SenderDisplay   string     `json:"sender_display_name"`
	SenderUPN       string     `json:"sender_upn"`
	AccessToken     string     `json:"-"`
	RefreshToken    string     `json:"-"`
	Scopes          string     `json:"scopes"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type TeamsRoute struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	Enabled      bool                  `json:"enabled"`
	TeamID       string                `json:"team_id"`
	TeamName     string                `json:"team_name"`
	ChannelID    string                `json:"channel_id"`
	ChannelName  string                `json:"channel_name"`
	OwnerTeam    string                `json:"owner_team,omitempty"`
	Service      string                `json:"service,omitempty"`
	Environment  string                `json:"environment,omitempty"`
	SeverityMin  string                `json:"severity_min,omitempty"`
	Recipients   []TeamsRouteRecipient `json:"recipients"`
	CreatedBy    string                `json:"created_by"`
	UpdatedBy    string                `json:"updated_by"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
}

type TeamsRouteRecipient struct {
	ID            string    `json:"id"`
	RouteID       string    `json:"route_id"`
	Type          string    `json:"type"`
	TeamsObjectID string    `json:"teams_object_id"`
	DisplayName   string    `json:"display_name"`
	UPN           string    `json:"upn,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type UpsertTeamsAuthStateRequest struct {
	SenderDisplay string
	SenderUPN     string
	AccessToken   string
	RefreshToken  string
	Scopes        string
	ExpiresAt     *time.Time
}

type CreateTeamsRouteRequest struct {
	Name        string
	Enabled     bool
	TeamID      string
	TeamName    string
	ChannelID   string
	ChannelName string
	OwnerTeam   string
	Service     string
	Environment string
	SeverityMin string
	Recipients  []CreateTeamsRouteRecipient
	Actor       string
}

type CreateTeamsRouteRecipient struct {
	Type          string `json:"type"`
	TeamsObjectID string `json:"teams_object_id"`
	DisplayName   string `json:"display_name"`
	UPN           string `json:"upn,omitempty"`
}

type UpdateTeamsRouteRequest struct {
	Name        string
	Enabled     bool
	TeamID      string
	TeamName    string
	ChannelID   string
	ChannelName string
	OwnerTeam   string
	Service     string
	Environment string
	SeverityMin string
	Recipients  []CreateTeamsRouteRecipient
	Actor       string
}

type TeamsDelivery struct {
	ID                 string         `json:"id"`
	IntegrationName    string         `json:"integration_name"`
	IncidentID         string         `json:"incident_id"`
	ChannelID          string         `json:"channel_id"`
	NotificationReason string         `json:"notification_reason"`
	MessageID          string         `json:"message_id"`
	Payload            map[string]any `json:"payload"`
	CreatedAt          time.Time      `json:"created_at"`
}

type TeamsQueueReason string

const (
	TeamsQueueReasonIncidentOpened  TeamsQueueReason = "incident_opened"
	TeamsQueueReasonIncidentReopen  TeamsQueueReason = "incident_reopened"
	TeamsRecipientTypeUser          string           = "user"
	TeamsRecipientTypeTag           string           = "tag"
	TeamsIntegrationName            string           = "teams"
)
