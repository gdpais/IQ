# IncidentIQ Runbooks

## API Failure

Symptoms:
- `/health/live` fails or API pods are restarting.
- The web UI cannot load incidents, reports, integrations, or template previews.

Actions:
1. Check API logs for config, migration, database, Redis, or JIRA errors.
2. Verify `DATABASE_URL`, `REDIS_ADDR`, `API_AUTH_TOKEN`, webhook secrets, and JIRA config are present.
3. Check `/health/ready`; it must reach PostgreSQL and Redis.
4. Restart one API pod after config is corrected.
5. Confirm `GET /incidents?limit=1` and `GET /metrics/live` respond.

## Worker Failure

Symptoms:
- JIRA retry events remain `pending`.
- Teams paging events remain `pending`.
- Report snapshots stop refreshing.

Actions:
1. Check worker logs for database, Redis, or JIRA errors.
2. Check worker logs for Microsoft Graph or Teams auth refresh errors.
3. Verify Redis is reachable because worker locks use Redis.
4. Verify `REPORT_SNAPSHOT_INTERVAL` is valid, for example `5m`.
5. Restart worker pods.
6. Confirm `integration_events` move from `pending` to `completed` and `report_snapshots.computed_at` advances.

## Microsoft Teams Paging Failure

Symptoms:
- Incidents are created or reopened but no Teams page appears.
- `integration_events` contains pending or failed `page_incident_opened` or `page_incident_reopened` events.

Actions:
1. Check `GET /integration-events?integration=teams`.
2. Verify `TEAMS_ENABLED`, `TEAMS_TENANT_ID`, `TEAMS_CLIENT_ID`, `TEAMS_CLIENT_SECRET`, and `TEAMS_TOKEN_ENCRYPTION_KEY`.
3. In the web UI Integrations tab, run the Teams connectivity test and confirm the stored sender is connected.
4. Confirm at least one enabled Teams route matches the incident `owner_team`, `service`, `environment`, and severity.
5. If Microsoft Graph tokens expired, reconnect the sender account and watch the worker drain pending events.
6. Use the route test action to validate a specific channel and mention set before re-triggering a real incident.

## JIRA Outage

Symptoms:
- JIRA sync returns 5xx, authentication errors, or connection failures.
- `integration_events` contains pending `issue_create` or `issue_update` events.

Actions:
1. Confirm IncidentIQ incident writes continue; JIRA is not source of truth.
2. Check `GET /integration-events?integration=jira&status=pending`.
3. Verify JIRA credentials, base URL, project key, and transition IDs.
4. After JIRA recovers, watch the worker retry queue drain.
5. Use `POST /incidents/{id}/jira/sync` for a targeted manual retry when needed.

## Redis Outage

Symptoms:
- `/health/ready` reports `redis_unavailable`.
- Rate limiting, worker locks, and retry coordination degrade.

Actions:
1. Restore Redis service connectivity.
2. Confirm `REDIS_ADDR` and network policies are correct.
3. Restart workers after Redis is available.
4. Verify retry events are not duplicated and locks are released.
5. Review pending integration events for retries delayed during the outage.

## PostgreSQL Failure or Failover

Symptoms:
- `/health/ready` reports `db_unavailable`.
- API and worker logs show connection or transaction failures.

Actions:
1. Promote the standby or restore from backup according to the database platform procedure.
2. Update `DATABASE_URL` if the endpoint changed.
3. Scale API and worker services back up.
4. Run the restore validation checks in `docs/ops/backup-restore.md`.
5. Confirm audit log continuity around the outage window.

## Webhook Signature Failures

Symptoms:
- Webhook requests return `401` with `missing or invalid webhook signature`.

Actions:
1. Confirm the source is signing the exact request body with HMAC-SHA256.
2. Confirm the header is `X-IncidentIQ-Signature`, `X-Hub-Signature-256`, or `X-Signature`.
3. Confirm the signature value is either `sha256=<hex>` or raw hex.
4. Rotate the source secret if compromise is suspected.
