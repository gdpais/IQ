# MVP Release Checklist

## Required Gates

- Root build passes: `npm run build`.
- API tests pass: `go test ./...` in `apps/api`.
- Worker tests pass: `go test ./...` in `apps/worker`.
- Web type check passes: `npm --workspace apps/web run lint`.
- Container images build for `api`, `worker`, and `web`.
- Migrations apply and rollback in a clean database.
- `/health/ready` passes with PostgreSQL and Redis.

## Security

- `API_AUTH_TOKEN` is set in production.
- Webhook secrets are set for enabled webhook sources.
- Webhook senders use HMAC-SHA256 request signing.
- Production CORS origins are explicitly listed.
- RBAC roles are assigned through trusted ingress or identity middleware.
- Security-sensitive actions are written to `audit_log`.
- Rate limits are reviewed for expected alert volume.

## Integrations

- JIRA connectivity test passes.
- JIRA outage retry behavior has been tested.
- Worker retry events drain after JIRA recovery.
- Ticket template YAML validates before rollout.
- Ticket preview matches expected JIRA fields.

## Reporting

- Live metrics match a controlled fixture.
- CSV export totals match equivalent dashboard filters.
- DORA metrics populate when deployment events are ingested.
- Report snapshots are materialized on the expected interval.

## Operations

- PostgreSQL backup and restore procedure has been tested.
- API, worker, JIRA outage, Redis outage, and DB failover runbooks are reviewed.
- Monitoring covers API readiness, worker retry lag, Redis availability, DB availability, JIRA retry queue depth, and webhook 401/429 rates.

## Redis Legal Checkpoint

- Confirm the exact Redis server image and version planned for production.
- Review the current Redis license terms with legal or compliance owners before production deployment.
- Confirm IncidentIQ does not use Redis Stack modules or other non-basic Redis features.
- Record approval or required mitigation in the release sign-off notes.
