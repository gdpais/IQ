CREATE TABLE IF NOT EXISTS teams_auth_state (
  integration_name TEXT PRIMARY KEY,
  sender_display_name TEXT NOT NULL DEFAULT '',
  sender_upn TEXT NOT NULL DEFAULT '',
  access_token_encrypted TEXT NOT NULL DEFAULT '',
  refresh_token_encrypted TEXT NOT NULL DEFAULT '',
  scopes TEXT NOT NULL DEFAULT '',
  expires_at TIMESTAMPTZ NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS teams_routes (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  team_id TEXT NOT NULL,
  team_name TEXT NOT NULL DEFAULT '',
  channel_id TEXT NOT NULL,
  channel_name TEXT NOT NULL DEFAULT '',
  owner_team TEXT NOT NULL DEFAULT '',
  service TEXT NOT NULL DEFAULT '',
  environment TEXT NOT NULL DEFAULT '',
  severity_min TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT 'system',
  updated_by TEXT NOT NULL DEFAULT 'system',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS teams_route_recipients (
  id TEXT PRIMARY KEY,
  route_id TEXT NOT NULL REFERENCES teams_routes(id) ON DELETE CASCADE,
  recipient_type TEXT NOT NULL,
  teams_object_id TEXT NOT NULL,
  display_name TEXT NOT NULL,
  upn TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_teams_route_recipients_unique
  ON teams_route_recipients(route_id, recipient_type, teams_object_id);

CREATE TABLE IF NOT EXISTS incident_notification_deliveries (
  id TEXT PRIMARY KEY,
  integration_name TEXT NOT NULL,
  incident_id TEXT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  channel_id TEXT NOT NULL,
  notification_reason TEXT NOT NULL,
  message_id TEXT NOT NULL DEFAULT '',
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_incident_notification_deliveries_unique
  ON incident_notification_deliveries(integration_name, incident_id, channel_id, notification_reason);

CREATE INDEX IF NOT EXISTS idx_teams_routes_lookup
  ON teams_routes(enabled, owner_team, service, environment, severity_min);
