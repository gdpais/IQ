# IncidentIQ

Production SRE incident management tool with automated alert intake, JIRA integration, Microsoft Teams paging, and DORA/SRE reporting.

## Overview

IncidentIQ is a reporting-focused incident management system for production SRE teams. It is the source of truth for incident lifecycle, metrics, and audit history — not JIRA. Key features:

- **Incident lifecycle**: Track open → acknowledged → resolved → reopen workflow
- **Alert intake**: Auto-correlate Dynatrace, ELK, and generic webhook alerts into incidents
- **JIRA sync**: Auto-create and update JIRA tickets with configurable templates
- **Teams paging**: Send short incident pages to Microsoft Teams channels with user and tag mentions
- **Metrics**: Live MTTD/MTTA/MTTR, SLA tracking, alert-to-incident conversion, DORA metrics
- **Reporting**: SRE, DORA, and executive dashboards with CSV export
- **Audit trail**: Immutable event log of all state changes and admin actions

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ Frontend (React + TypeScript + Vite)                        │
│ - Incident register, detail view, lifecycle actions         │
│ - SRE/DORA/Executive reporting dashboards                   │
│ - Integration status, ticket template preview               │
└─────────────────┬───────────────────────────────────────────┘
                  │
┌─────────────────┴───────────────────────────────────────────┐
│ HTTP API (Go 1.23)                                          │
│ - Incident CRUD + lifecycle transitions                     │
│ - Webhook ingestion (Dynatrace, ELK, generic)               │
│ - Metric computation, reporting, CSV export                 │
│ - JIRA sync + Teams paging + template management            │
│ - RBAC (viewer, responder, commander, admin)                │
└──┬──────────────────────────────┬──────────────────────────┘
   │                              │
   ▼                              ▼
┌──────────────────┐      ┌───────────────────┐
│ PostgreSQL 16    │      │ Redis 7           │
│ - Incidents      │      │ - Rate limits     │
│ - Alerts         │      │ - Worker jobs     │
│ - JIRA links     │      │ - Cache           │
│ - Metrics/       │      │                   │
│   snapshots      │      │                   │
│ - Audit log      │      │                   │
└──────────────────┘      └───────────────────┘
                              ▲
                              │
                    ┌─────────┴─────────┐
                    │ Worker (Go 1.23)  │
                    │ - JIRA retries    │
                    │ - Teams paging    │
                    │ - Snapshot        │
                    │   materialization │
                    └───────────────────┘
```

## Prerequisites

### For local development

- **Go** 1.23+ (download: https://go.dev/dl)
- **Node.js** 20+ (download: https://nodejs.org)
- **PostgreSQL** 16+ (https://www.postgresql.org/download or via Docker)
- **Redis** 7+ (https://redis.io/download or via Docker)

### For Docker/Kubernetes

- **Docker** 24+
- **Docker Compose** 2.20+ (for local dev environment)
- **kubectl** 1.27+ (for Kubernetes deployment)

## Installation

### 1. Clone the repository

```bash
git clone https://github.com/gdpais/IQ.git
cd IQ
```

### 2. Set up environment variables

Copy the example configuration:

```bash
cp .env.example .env
```

Edit `.env` with your settings:

```bash
# Database
DATABASE_URL=postgres://incidentiq:incidentiq@localhost:5432/incidentiq

# Redis
REDIS_ADDR=localhost:6379
REDIS_DB=0

# API
PORT=8080
API_AUTH_TOKEN=your-secret-token-here

# CORS
CORS_ALLOWED_ORIGINS=http://localhost:5173

# JIRA (optional)
JIRA_ENABLED=false
JIRA_BASE_URL=https://jira.example.com
JIRA_USERNAME=incident-bot
JIRA_API_TOKEN=your-jira-token
JIRA_PROJECT_KEY=OPS

# Microsoft Teams (optional)
TEAMS_ENABLED=false
TEAMS_TENANT_ID=your-tenant-id
TEAMS_CLIENT_ID=your-app-client-id
TEAMS_CLIENT_SECRET=your-app-client-secret
TEAMS_TOKEN_ENCRYPTION_KEY=replace-with-strong-secret

# Webhooks
WEBHOOK_DYNATRACE_SECRET=dynatrace-secret
WEBHOOK_ELK_SECRET=elk-secret
WEBHOOK_GENERIC_SECRET=generic-secret
```

## Running the Application

### Option A: Local Development (recommended for development)

#### Start infrastructure (PostgreSQL + Redis)

Using Docker Compose:

```bash
docker compose up -d
```

This starts:
- PostgreSQL 16 at `localhost:5432`
- Redis 7 at `localhost:6379`
- pgAdmin 4 at `http://localhost:5050` (optional UI, login: admin@example.com / admin)

#### Start the API server

```bash
cd apps/api
go run ./cmd/api
```

The API will:
- Apply migrations automatically on startup
- Listen on `http://localhost:8080`
- Check health at `GET http://localhost:8080/health/live`
- Check readiness at `GET http://localhost:8080/health/ready`

#### Start the worker (in a new terminal)

```bash
cd apps/worker
go run ./cmd/worker
```

The worker will:
- Poll for retryable JIRA sync events every 5 seconds
- Poll for pending Microsoft Teams paging events every 5 seconds
- Materialize report snapshots every 5 minutes (configurable)

#### Start the frontend (in a new terminal)

```bash
cd apps/web
npm install
npm run dev
```

The web UI will be available at `http://localhost:5173`

### Option B: Docker Compose (all-in-one)

Build and run the full stack:

```bash
docker compose up --build
```

Access:
- **Web UI**: http://localhost:80
- **API**: http://localhost:8080
- **PostgreSQL**: localhost:5432 (via pgAdmin or `psql`)
- **Redis**: localhost:6379

### Option C: Kubernetes

Deploy to an existing Kubernetes cluster:

```bash
# Create namespace
kubectl create namespace incidentiq

# Create secrets (edit with your values first)
kubectl apply -f infra/k8s/base/secret.example.yaml -n incidentiq

# Apply configuration
kubectl apply -k infra/k8s/base -n incidentiq

# Port-forward to access locally
kubectl port-forward -n incidentiq svc/web 8080:80 &
kubectl port-forward -n incidentiq svc/api 8081:8080 &

# View logs
kubectl logs -n incidentiq deployment/api -f
kubectl logs -n incidentiq deployment/worker -f
kubectl logs -n incidentiq deployment/web -f
```

## Configuration

### Environment Variables

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `DATABASE_URL` | string | required | PostgreSQL connection string |
| `REDIS_ADDR` | string | `localhost:6379` | Redis address |
| `REDIS_DB` | int | `0` | Redis database number |
| `PORT` | string | `8080` | API listen port |
| `API_AUTH_TOKEN` | string | `` | API bearer token (if empty, auth is disabled) |
| `CORS_ALLOWED_ORIGINS` | string | `*` | Comma-separated CORS origins |
| `JIRA_ENABLED` | bool | `false` | Enable JIRA integration |
| `JIRA_BASE_URL` | string | `` | JIRA on-prem base URL |
| `JIRA_USERNAME` | string | `` | JIRA username |
| `JIRA_API_TOKEN` | string | `` | JIRA API token |
| `JIRA_PROJECT_KEY` | string | `` | Default JIRA project key |
| `TEAMS_ENABLED` | bool | `false` | Enable Microsoft Teams paging |
| `TEAMS_TENANT_ID` | string | `` | Microsoft Entra tenant ID |
| `TEAMS_CLIENT_ID` | string | `` | Microsoft Graph app client ID |
| `TEAMS_CLIENT_SECRET` | string | `` | Microsoft Graph app client secret |
| `TEAMS_TOKEN_ENCRYPTION_KEY` | string | `` | Secret used to encrypt stored Teams sender tokens |
| `WEBHOOK_DYNATRACE_SECRET` | string | `` | Dynatrace webhook signature secret |
| `WEBHOOK_ELK_SECRET` | string | `` | ELK webhook signature secret |
| `WEBHOOK_GENERIC_SECRET` | string | `` | Generic webhook signature secret |
| `REPORT_SNAPSHOT_INTERVAL` | duration | `5m` | Snapshot materialization interval (worker only) |

### Teams paging runtime setup

1. Configure `TEAMS_*` environment variables for both `api` and `worker`.
2. Open the **Integrations** tab in the web UI.
3. Save a Teams sender connection using an access token and refresh token for the dedicated sender account.
4. Load Teams and channels from Microsoft Graph, create one or more routes, and attach users and/or Teams tags.
5. Use route filters for `owner_team`, `service`, `environment`, and minimum severity as needed.

Teams pages are queued when an incident is created or reopened. Each message includes the impacted services, the incident start time, and a short issue description when available.

### Database Migrations

Migrations are applied automatically on API startup. To manually apply:

```bash
# Using psql directly
psql -U incidentiq -d incidentiq < apps/api/migrations/0001_init.up.sql

# Or using your preferred migration tool
```

To rollback (destructive):

```bash
psql -U incidentiq -d incidentiq < apps/api/migrations/0001_init.down.sql
```

## Testing

### Unit Tests (no infrastructure required)

```bash
# API tests
cd apps/api && go test -race ./...

# Worker tests
cd apps/worker && go test -race ./...

# Web tests
cd apps/web && npm test
```

### Performance Benchmarks

```bash
cd apps/api
go test -bench=. -benchmem ./internal/incident/...
```

Expected results on modern hardware: ~350K–540K alert normalizations per second.

### Integration Tests (requires Postgres + Redis)

```bash
export TEST_DATABASE_URL=postgres://incidentiq:incidentiq@localhost:5432/incidentiq_test
export TEST_REDIS_ADDR=localhost:6379

# API integration tests
cd apps/api && go test -tags integration -race -v ./internal/httpapi/...

# Worker integration tests
cd apps/worker && go test -tags integration -race -v ./cmd/worker/...
```

### CI/CD Pipeline

Push to any branch or PR to trigger:

- **api**: lint, unit tests, build
- **worker**: lint, unit tests, build
- **web**: type-check, unit tests, build
- **integration** (on master or when `RUN_INTEGRATION_TESTS=true`): full integration suite with Postgres + Redis

View CI status: `gh pr checks 1 --repo gdpais/IQ`

## API Usage

### Authentication

If `API_AUTH_TOKEN` is set, requests must include:

```bash
curl -H "Authorization: Bearer $API_AUTH_TOKEN" \
     -H "X-Role: admin" \
     http://localhost:8080/incidents
```

### Role-Based Access Control

Set the `X-Role` header to one of:

- `viewer` — read-only access
- `responder` — create/acknowledge/resolve incidents, sync JIRA
- `commander` — materialize report snapshots
- `admin` — all operations

### Webhook Examples

#### Dynatrace

```bash
curl -X POST http://localhost:8080/webhooks/dynatrace \
  -H "Content-Type: application/json" \
  -H "X-IncidentIQ-Signature: sha256=<hmac>" \
  -d '{
    "problemId": "P-123",
    "severity": "availability",
    "impactedEntity": "checkout-api",
    "startTime": 1780394100000,
    "problemTitle": "High error rate"
  }'
```

#### ELK

```bash
curl -X POST http://localhost:8080/webhooks/elk \
  -H "Content-Type: application/json" \
  -H "X-IncidentIQ-Signature: sha256=<hmac>" \
  -d '{
    "kibana": {
      "alert": {
        "uuid": "alert-123",
        "severity": "critical"
      }
    },
    "rule": {
      "name": "Error budget burn",
      "id": "rule-7"
    },
    "message": "Error budget exceeded"
  }'
```

#### Generic

```bash
curl -X POST http://localhost:8080/webhooks/generic \
  -H "Content-Type: application/json" \
  -H "X-IncidentIQ-Signature: sha256=<hmac>" \
  -d '{
    "source_event_id": "evt-123",
    "title": "High memory usage",
    "service": "database",
    "environment": "prod",
    "severity": "high"
  }'
```

## Troubleshooting

### API won't start: "db ping failed"

**Cause**: PostgreSQL is not running or DATABASE_URL is wrong.

**Fix**:
```bash
# Check Postgres is running
docker ps | grep postgres

# Test connection
psql -c "SELECT 1" postgresql://incidentiq:incidentiq@localhost:5432/incidentiq

# Update DATABASE_URL in .env if needed
```

### Worker won't start: "redis ping failed"

**Cause**: Redis is not running or REDIS_ADDR is wrong.

**Fix**:
```bash
# Check Redis is running
docker ps | grep redis

# Test connection
redis-cli -h localhost -p 6379 PING

# Should return PONG
```

### JIRA sync not working

**Cause**: JIRA_ENABLED=false or credentials are wrong.

**Fix**:
```bash
# Test JIRA connectivity
curl -X POST http://localhost:8080/integrations/jira/test \
  -H "Authorization: Bearer $API_AUTH_TOKEN" \
  -H "X-Role: admin"

# Check JIRA_* env vars are set correctly
echo $JIRA_BASE_URL $JIRA_USERNAME $JIRA_PROJECT_KEY
```

### Migrations failing

**Cause**: Database schema is corrupted or migrations are out of order.

**Fix**:
```bash
# Check what tables exist
psql -U incidentiq -d incidentiq -c "\dt"

# Force re-apply the initial migration (DESTRUCTIVE)
psql -U incidentiq -d incidentiq < apps/api/migrations/0001_init.down.sql
psql -U incidentiq -d incidentiq < apps/api/migrations/0001_init.up.sql
```

## Development Workflow

### Adding a new endpoint

1. Define the handler in `apps/api/internal/httpapi/server.go`
2. Register the route in `routes()`
3. Add RBAC check in `roleAllowed()` if needed
4. Add audit logging with `auditAction()`
5. Write tests in `*_test.go` files
6. Run tests: `go test -race ./...`

### Deploying to production

1. Ensure all tests pass: `go test ./...`, `npm test`
2. Tag the release: `git tag v1.0.0`
3. Build containers: `docker build -t incidentiq:v1.0.0 .`
4. Push images to registry: `docker push incidentiq:v1.0.0`
5. Update `infra/k8s/base/*.yaml` with new image tag
6. Deploy: `kubectl apply -k infra/k8s/base`
7. Monitor: `kubectl logs -n incidentiq deployment/api -f`

## Contributing

See TASKS.md for the MVP implementation checklist.

## License

See LICENSE file.
