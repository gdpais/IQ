# IncidentIQ Reporting-Focused MVP Plan

## Summary

Build IncidentIQ as a reporting-focused incident management tool for production SRE teams.

Use a React + TypeScript frontend, a Go backend, PostgreSQL for durable data, and Redis for caching/background coordination. IncidentIQ is authoritative for incident lifecycle, metrics, audit history, and reporting. JIRA on-prem is synchronized for ticket visibility and work tracking, but JIRA is not the source of truth.

The MVP excludes playbooks and recovery execution. It focuses on automated incident registration, alert correlation, JIRA ticket creation/update, real-time SRE/DORA/executive reporting, and configurable JIRA ticket templates.

## Key Architecture

- Backend:
  - Go API and worker services.
  - PostgreSQL for incidents, events, metrics source data, config, audit logs, and report snapshots.
  - Redis official server for cache, rate limits, short-lived locks, worker coordination, and retry scheduling.
  - Keep Redis usage basic: no Redis Stack modules or advanced proprietary-adjacent features.
  - Add a Redis license review gate before production deployment because Redis licensing changed after older BSD releases.

- Frontend:
  - React + TypeScript + Vite.
  - Dashboard-first internal SRE UI.
  - Views for incident register, live incident detail, reporting, JIRA template config preview, integrations, and audit history.

- Deployment:
  - Kubernetes-ready containers for `web`, `api`, and `worker`.
  - Required services: PostgreSQL and Redis.
  - Secrets/config for JIRA, Dynatrace, ELK, webhook signing, database, and Redis.

- Security and reliability:
  - API token authentication can be enabled for non-webhook endpoints.
  - RBAC roles gate read, responder, commander, and admin operations.
  - Webhooks use HMAC-SHA256 request signatures when source secrets are configured.
  - Redis-backed fixed-window rate limits protect API and webhook ingress.
  - Security-sensitive actions are recorded in `audit_log`.

## Core Capabilities

- Incident register:
  - Auto-create incidents from Dynatrace, ELK, and generic webhooks.
  - Support manual incident creation.
  - Track severity, service, environment, owner team, status, timestamps, linked alerts, linked JIRA issue, SLA/business impact, and resolution notes.
  - Store immutable lifecycle and audit events.

- Alert intake and correlation:
  - Normalize Dynatrace, ELK, and generic webhook payloads into one internal alert schema.
  - Correlate by service, environment, severity, fingerprint, source, and time window.
  - Suppress duplicate incidents and avoid duplicate JIRA tickets.
  - Store raw payloads with redaction.

- JIRA ticketing:
  - Auto-create JIRA tickets from IncidentIQ incidents.
  - Auto-update JIRA comments, status, severity, labels, links, and resolution fields.
  - Retry failed JIRA sync through Redis-backed workers.
  - Store JIRA keys as external references only.

- Ticket templates:
  - Configure templates through versioned YAML files.
  - Support templates for title, description/body, labels, priority mapping, issue type, project key, components, custom fields, and comments.
  - Support global defaults plus service/team/severity-specific overrides.
  - Use simple safe variables only, such as `{{severity}}`, `{{service}}`, `{{environment}}`, `{{incident_id}}`, `{{started_at}}`, `{{summary}}`, `{{alert_count}}`, and `{{dashboard_url}}`.
  - Validate templates at startup and expose a preview endpoint/UI so admins can verify rendered JIRA tickets before enabling changes.
  - Do not allow arbitrary logic, scripts, or network calls inside templates.

- Reporting:
  - Live dashboard by severity, service, environment, team, and status.
  - SRE metrics: MTTD, MTTA, MTTR, incident count, reopen rate, SLA impact, alert-to-incident conversion, duplicate suppression.
  - DORA-adjacent metrics: deployment-linked incidents, change failure rate, time to restore service.
  - Executive metrics: downtime, SLA breach/at-risk incidents, service reliability trends, severity trends, business impact fields, monthly summaries.
  - CSV export in v1.

## Public Interfaces

- Webhooks:
  - `POST /webhooks/dynatrace`
  - `POST /webhooks/elk`
  - `POST /webhooks/generic`
  - `POST /webhooks/deployments`

- Incident API:
  - `GET /incidents`
  - `POST /incidents`
  - `GET /incidents/{id}`
  - `PATCH /incidents/{id}`
  - `POST /incidents/{id}/events`
  - `POST /incidents/{id}/acknowledge`
  - `POST /incidents/{id}/resolve`
  - `POST /incidents/{id}/reopen`

- Reporting API:
  - `GET /reports/sre`
  - `GET /reports/dora`
  - `GET /reports/executive`
  - `GET /metrics/live`
  - `GET /reports/snapshots`
  - `POST /reports/snapshots/materialize`
  - `GET /exports/incidents.csv`

- Template/config API:
  - `GET /ticket-templates`
  - `POST /ticket-templates/validate`
  - `POST /ticket-templates/preview`

- Integration API:
  - `GET /integrations`
  - `PATCH /integrations/{name}`
  - `POST /integrations/{name}/test`
  - `GET /integration-events`

## Data Model Defaults

- Core tables:
  - `incidents`
  - `incident_events`
  - `alerts`
  - `jira_links`
  - `services`
  - `teams`
  - `deployments`
  - `integration_events`
  - `report_snapshots`
  - `users`
  - `audit_log`
  - `ticket_template_versions`

- Metrics are calculated from timestamps and event history.
- Report snapshots can be materialized for dashboard speed, but raw event data remains authoritative.
- YAML ticket templates are versioned and loaded into the database for auditability.

## Test Plan

- Unit tests:
  - Alert normalization.
  - Incident correlation and duplicate suppression.
  - MTTA, MTTD, MTTR, reopen rate, SLA impact, and change failure calculations.
  - JIRA field mapping.
  - Ticket template rendering, validation, override precedence, and missing-variable behavior.

- Integration tests:
  - Dynatrace alert creates one IncidentIQ incident and one JIRA ticket.
  - Correlated ELK alert updates the same incident and JIRA ticket.
  - Generic webhook works without source-specific code.
  - JIRA outage creates retryable integration events.
  - Redis-backed retry worker eventually syncs JIRA after recovery.
  - Deployment webhook links a failed change to an incident.

- End-to-end tests:
  - Alert-to-incident-to-JIRA-to-reporting flow.
  - Manual incident creation with templated JIRA ticket.
  - Template preview before enabling YAML changes.
  - Incident acknowledgement and resolution update live metrics.
  - CSV export matches dashboard filters.

## Assumptions

- IncidentIQ owns incident lifecycle and reporting.
- JIRA is synchronized but not authoritative.
- v1 covers production SRE incidents only.
- No playbooks, remediation, or recovery automation are included.
- Redis official server is included despite licensing concerns, with production legal review required.
- Ticket templates are configured through YAML files, not Admin UI in v1.
- Ticket template syntax is simple variable replacement only.
- DORA metrics require deployment events because no CI/CD platform was specified.
