# PostgreSQL Backup and Restore

IncidentIQ treats PostgreSQL as the system of record for incidents, audit history, integration events, ticket templates, and report snapshots. Redis is coordination/cache state and is not part of durable recovery.

## Backup

1. Run logical backups at least daily with `pg_dump`.
2. Store backups outside the cluster with encryption enabled.
3. Keep at least 30 days of daily backups and 7 days of point-in-time recovery logs when production volume justifies WAL archiving.
4. Back up Kubernetes Secrets or the external secret-manager paths that provide:
   - `DATABASE_URL`
   - `API_AUTH_TOKEN`
   - webhook signing secrets
   - JIRA credentials
   - ticket template file source

Example:

```bash
pg_dump "$DATABASE_URL" --format=custom --file=incidentiq-$(date +%Y%m%d%H%M).dump
```

## Restore

1. Stop API and worker writers or scale them to zero.
2. Create a fresh database.
3. Restore the dump.
4. Run migrations against the restored database.
5. Start the API and verify `/health/ready`.
6. Start workers and verify retry events are processing.
7. Confirm recent incidents, audit records, JIRA links, ticket template versions, and report snapshots are present.

Example:

```bash
createdb incidentiq_restore
pg_restore --dbname "$RESTORE_DATABASE_URL" --clean --if-exists incidentiq-YYYYMMDDHHMM.dump
```

## Validation

- `GET /health/ready` returns `ready`.
- `GET /incidents?limit=1` returns recent data.
- `GET /integration-events?limit=5` shows retry state.
- `GET /ticket-templates?limit=1` returns the active template history when configured.
- `GET /reports/snapshots?limit=4` returns recently materialized dashboard snapshots.
