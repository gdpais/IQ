# IncidentIQ Implementation Tasks

This backlog is derived from the approved MVP plan and is organized for phased execution.

## Phase 0 - Foundation

- [x] Create monorepo structure: `apps/api`, `apps/worker`, `apps/web`, `packages/shared`, `infra`.
- [x] Initialize Go modules for `api` and `worker` with shared internal packages.
- [x] Initialize React + TypeScript + Vite app for `web`.
- [x] Add root task runner scripts for `build`, `test`, `lint`, `dev`.
- [x] Add Dockerfiles for `api`, `worker`, `web`.
- [x] Add Kubernetes base manifests (or Helm chart skeleton) for all services.

### Acceptance criteria

- [x] Repo builds locally for all apps.
- [ ] Containers build for all apps.
- [x] Basic K8s deployment manifests validate.

## Phase 1 - Data and Platform Core

- [x] Provision PostgreSQL schema and migrations framework.
- [x] Provision Redis client setup and runtime configuration.
- [x] Define core tables: `incidents`, `incident_events`, `alerts`, `jira_links`, `services`, `teams`, `deployments`, `integration_events`, `report_snapshots`, `users`, `audit_log`, `ticket_template_versions`.
- [x] Implement DB repository layer for incident lifecycle and event append operations.
- [x] Add config management (env-based) for DB, Redis, JIRA, Dynatrace, ELK, webhook secrets.
- [x] Add structured logging, health endpoints, readiness/liveness checks.
- [x] Add auth/RBAC skeleton roles: viewer, responder, commander, admin.

### Acceptance criteria

- [ ] DB migrations apply/rollback successfully.
- [ ] API boots with DB + Redis connectivity checks.
- [x] Core data model supports incident lifecycle storage and audit append-only events.

## Phase 2 - Incident Register and Lifecycle API

- [x] Implement `POST /incidents` manual incident creation.
- [x] Implement `GET /incidents` list with filters (severity, service, status, owner, date range).
- [x] Implement `GET /incidents/{id}` with full timeline and linked entities.
- [x] Implement `PATCH /incidents/{id}` update rules (non-destructive to event history).
- [ ] Implement lifecycle commands:
  - [x] `POST /incidents/{id}/acknowledge`
  - [x] `POST /incidents/{id}/resolve`
  - [x] `POST /incidents/{id}/reopen`
- [x] Implement `POST /incidents/{id}/events` for explicit timeline events.

### Acceptance criteria

- [x] Lifecycle transitions generate immutable incident events.
- [x] Timestamps required for MTTD/MTTA/MTTR are captured reliably.
- [x] Invalid transitions are rejected with consistent API errors.

## Phase 3 - Alert Intake and Correlation

- [x] Implement webhook endpoints:
  - [x] `POST /webhooks/dynatrace`
  - [x] `POST /webhooks/elk`
  - [x] `POST /webhooks/generic`
- [x] Implement source-specific normalization into common internal alert schema.
- [x] Implement raw payload storage with sensitive-field redaction.
- [x] Implement correlation engine (service, environment, severity, fingerprint, source, time window).
- [ ] Implement duplicate suppression for incidents and downstream ticketing.
- [x] Implement idempotency handling for repeated webhook deliveries.

### Acceptance criteria

- [ ] Same logical alert stream maps to the same active incident when correlation rules match.
- [ ] Repeated deliveries do not produce duplicate incidents.
- [ ] Unmatched alerts create new incidents with complete provenance.

## Phase 4 - JIRA On-Prem Integration + Ticket Templates

- [x] Implement JIRA connector configuration and connectivity test endpoint.
- [x] Implement incident-to-JIRA create workflow.
- [x] Implement incident-to-JIRA update workflow (status, comments, labels, severity, resolution fields).
- [x] Persist external mapping in `jira_links`.
- [x] Implement retryable integration events in `integration_events`.
- [x] Implement worker retry pipeline using Redis-backed job coordination.

- [ ] Implement YAML ticket template loader and versioning.
- [ ] Implement template validation endpoint: `POST /ticket-templates/validate`.
- [ ] Implement template preview endpoint: `POST /ticket-templates/preview`.
- [ ] Implement template retrieval endpoint: `GET /ticket-templates`.
- [ ] Support template scopes: global default + service/team/severity overrides.
- [ ] Support simple variables only:
  - [ ] `{{severity}}`
  - [ ] `{{service}}`
  - [ ] `{{environment}}`
  - [ ] `{{incident_id}}`
  - [ ] `{{started_at}}`
  - [ ] `{{summary}}`
  - [ ] `{{alert_count}}`
  - [ ] `{{dashboard_url}}`

### Acceptance criteria

- [ ] New incidents create exactly one JIRA ticket when integration is enabled.
- [ ] Subsequent incident updates sync to existing JIRA issue without duplicates.
- [ ] Template validation catches unknown variables and invalid override structure.
- [ ] JIRA outage produces retryable events without losing IncidentIQ source-of-truth state.

## Phase 5 - Reporting and Metrics Engine

- [ ] Implement metrics computation from incident lifecycle and event timestamps.
- [ ] Implement live metrics endpoint: `GET /metrics/live`.
- [ ] Implement report endpoints:
  - [ ] `GET /reports/sre`
  - [ ] `GET /reports/dora`
  - [ ] `GET /reports/executive`
- [ ] Implement deployment ingestion endpoint: `POST /webhooks/deployments`.
- [ ] Implement deployment-to-incident linking logic for DORA metrics.
- [ ] Implement report snapshot/materialization jobs for dashboard performance.
- [ ] Implement CSV export endpoint: `GET /exports/incidents.csv`.

### MVP metric set

- [ ] SRE: MTTD, MTTA, MTTR, incident count, reopen rate, SLA impact, alert-to-incident conversion, duplicate suppression.
- [ ] DORA-adjacent: deployment-linked incidents, change failure rate, time to restore service.
- [ ] Executive: downtime, SLA breach/at-risk count, reliability trends, severity trends, business impact rollups.

### Acceptance criteria

- [ ] Metrics match controlled fixture scenarios.
- [ ] Dashboard and CSV exports show consistent totals for equivalent filters.
- [ ] DORA metrics populate when deployment events are present.

## Phase 6 - Frontend Operator Experience

- [ ] Build incident register view with filtering and sorting.
- [ ] Build incident detail view with timeline, lifecycle actions, linked alerts, JIRA link, audit trail.
- [ ] Build reporting dashboards (SRE, DORA, Executive) with time-range and dimension filters.
- [ ] Build integration status/config surfaces and test action UI.
- [ ] Build ticket template preview/validation UI.
- [ ] Build error, loading, and empty states across all major screens.

### Acceptance criteria

- [ ] Operators can run end-to-end incident workflows from the UI.
- [ ] Reporting views update from backend data without manual refresh assumptions.
- [ ] Template preview is usable before enabling config changes.

## Phase 7 - Security, Reliability, and Ops Hardening

- [ ] Add webhook authentication/signature verification.
- [ ] Add API authentication and RBAC enforcement across endpoints.
- [ ] Add audit logging for security-sensitive actions and config changes.
- [ ] Add rate limits and abuse controls on webhook/API ingress.
- [ ] Add backup/restore procedure for PostgreSQL and config versions.
- [ ] Add runbooks for service failure, JIRA outage, Redis outage, and DB failover.
- [ ] Add Redis license/legal checkpoint to release checklist.

### Acceptance criteria

- [ ] Unauthorized actions are blocked and auditable.
- [ ] Integration outages degrade gracefully with retry and operator visibility.
- [ ] Release checklist includes legal review for Redis usage.

## Phase 8 - Testing and Release Readiness

- [ ] Add unit tests for normalization, correlation, lifecycle transitions, metrics computation, template rendering.
- [ ] Add integration tests for webhook -> incident -> JIRA sync flows.
- [ ] Add integration tests for JIRA failure/recovery retries.
- [ ] Add end-to-end tests for dashboard/report consistency and CSV exports.
- [ ] Add performance smoke tests for high-volume alert ingestion.
- [ ] Add CI pipeline gates for lint, unit, integration test suites.
- [ ] Define MVP release checklist and sign-off criteria.

### Acceptance criteria

- [ ] CI is green on required checks.
- [ ] E2E test proves alert-to-report workflow.
- [ ] MVP acceptance sign-off is possible with documented evidence.

## Out of Scope for This MVP

- [ ] Playbooks and automated remediation execution.
- [ ] Security incident response specialization.
- [ ] General ITSM workflows outside production SRE incident handling.
