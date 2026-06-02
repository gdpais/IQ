//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

func workerTestSetup(t *testing.T) (*pgxpool.Pool, *redis.Client, func()) {
	t.Helper()

	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set — run with TEST_DATABASE_URL=postgres://... go test -tags integration")
	}
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("db connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("db ping: %v", err)
	}

	// Apply up migration
	applyMigrationWorker(t, pool, "0001_init.up.sql")

	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		pool.Close()
		t.Skipf("Redis not reachable at %s — skipping: %v", redisAddr, err)
	}

	return pool, redisClient, func() {
		applyMigrationWorker(t, pool, "0001_init.down.sql")
		pool.Close()
		_ = redisClient.Close()
	}
}

func applyMigrationWorker(t *testing.T, pool *pgxpool.Pool, filename string) {
	t.Helper()
	_, testFile, _, _ := runtime.Caller(0)
	// testFile: .../apps/worker/cmd/worker/integration_test.go
	// migrations: .../apps/api/migrations/
	migrationPath := filepath.Join(filepath.Dir(testFile), "../../../../apps/api/migrations", filename)
	content, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration %s: %v", filename, err)
	}
	if _, err := pool.Exec(context.Background(), string(content)); err != nil {
		t.Fatalf("apply migration %s: %v", filename, err)
	}
}

func insertTestIncident(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := fmt.Sprintf("test-inc-%d", time.Now().UnixNano())
	_, err := pool.Exec(context.Background(), `
		INSERT INTO incidents (id, title, summary, severity, service, environment, owner_team, status, started_at, created_at, updated_at)
		VALUES ($1, $2, '', 'high', 'test-service', 'test', 'sre', 'open', NOW(), NOW(), NOW())
	`, id, "Worker integration test incident")
	if err != nil {
		t.Fatalf("insert test incident: %v", err)
	}
	return id
}

func insertRetryableEvent(t *testing.T, pool *pgxpool.Pool, incidentID string, eventType string) string {
	t.Helper()
	id := fmt.Sprintf("test-evt-%d", time.Now().UnixNano())
	payload, _ := json.Marshal(map[string]any{"incident_id": incidentID, "action": "create", "reason": "integration_test"})
	_, err := pool.Exec(context.Background(), `
		INSERT INTO integration_events (id, integration_name, event_type, status, payload, attempts, next_retry_at, created_at, updated_at)
		VALUES ($1, 'jira', $2, 'pending', $3, 0, NOW() - INTERVAL '1 second', NOW(), NOW())
	`, id, eventType, payload)
	if err != nil {
		t.Fatalf("insert retryable event: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Worker integration tests
// ---------------------------------------------------------------------------

// TestIntegrationWorkerProcessesRetryAndCreatesJIRAIssue verifies that the
// worker picks up a pending retryable event, calls the JIRA API, and marks
// the event as completed.
func TestIntegrationWorkerProcessesRetryAndCreatesJIRAIssue(t *testing.T) {
	pool, redisClient, cleanup := workerTestSetup(t)
	defer cleanup()

	ctx := context.Background()

	// Set up a mock JIRA server
	jiraCallCount := 0
	mockJIRA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jiraCallCount++
		if r.Method == http.MethodPost && r.URL.Path == "/rest/api/2/issue" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"key":"RETRY-1","id":"20001"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockJIRA.Close()

	// Create incident and retryable event in DB
	incidentID := insertTestIncident(t, pool)
	eventID := insertRetryableEvent(t, pool, incidentID, "issue_create")

	// Create worker with mock JIRA
	w := worker{
		db:    pool,
		redis: redisClient,
		jira: jiraConfig{
			Enabled:    true,
			BaseURL:    mockJIRA.URL,
			Username:   "test",
			APIToken:   "test-token",
			ProjectKey: "RETRY",
		},
		http: &http.Client{Timeout: 5 * time.Second},
	}

	// Process due events
	if err := w.processDueEvents(ctx); err != nil {
		t.Fatalf("processDueEvents: %v", err)
	}

	// Verify event was completed
	var status string
	err := pool.QueryRow(ctx, `SELECT status FROM integration_events WHERE id = $1`, eventID).Scan(&status)
	if err != nil {
		t.Fatalf("query event status: %v", err)
	}
	if status != "completed" {
		t.Fatalf("event status = %q, want completed", status)
	}

	// Verify JIRA link was created
	var jiraKey string
	err = pool.QueryRow(ctx, `SELECT jira_issue_key FROM jira_links WHERE incident_id = $1`, incidentID).Scan(&jiraKey)
	if err != nil {
		t.Fatalf("query jira_links: %v", err)
	}
	if jiraKey != "RETRY-1" {
		t.Fatalf("jira_issue_key = %q, want RETRY-1", jiraKey)
	}

	if jiraCallCount == 0 {
		t.Fatal("no JIRA API calls were made")
	}
}

// TestIntegrationWorkerDefersEventOnJIRAFailure verifies that when JIRA is
// unavailable the worker marks the event as pending with an incremented
// attempt count and a future retry time.
func TestIntegrationWorkerDefersEventOnJIRAFailure(t *testing.T) {
	pool, redisClient, cleanup := workerTestSetup(t)
	defer cleanup()

	ctx := context.Background()

	// Mock JIRA that always fails
	mockJIRA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"errorMessages":["JIRA is unavailable"]}`)
	}))
	defer mockJIRA.Close()

	incidentID := insertTestIncident(t, pool)
	eventID := insertRetryableEvent(t, pool, incidentID, "issue_create")

	w := worker{
		db:    pool,
		redis: redisClient,
		jira: jiraConfig{
			Enabled: true, BaseURL: mockJIRA.URL,
			Username: "test", APIToken: "test", ProjectKey: "FAIL",
		},
		http: &http.Client{Timeout: 5 * time.Second},
	}

	_ = w.processDueEvents(ctx)

	// Event must remain pending (deferred for retry) with attempts incremented
	var status string
	var attempts int
	var nextRetryAt *time.Time
	err := pool.QueryRow(ctx,
		`SELECT status, attempts, next_retry_at FROM integration_events WHERE id = $1`, eventID,
	).Scan(&status, &attempts, &nextRetryAt)
	if err != nil {
		t.Fatalf("query event: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want pending (deferred)", status)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 after first failure", attempts)
	}
	if nextRetryAt == nil || !nextRetryAt.After(time.Now()) {
		t.Fatalf("next_retry_at = %v — should be in the future after deferral", nextRetryAt)
	}

	// Verify incident state was NOT corrupted
	var incStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM incidents WHERE id = $1`, incidentID).Scan(&incStatus)
	if incStatus != "open" {
		t.Fatalf("incident status = %q after JIRA failure, want open (unaffected)", incStatus)
	}
}

// TestIntegrationWorkerExhaustsRetriesAndMarksFailed verifies that after
// maxAttempts the worker marks the event as failed and stops retrying.
func TestIntegrationWorkerExhaustsRetriesAndMarksFailed(t *testing.T) {
	pool, redisClient, cleanup := workerTestSetup(t)
	defer cleanup()

	ctx := context.Background()

	// Mock JIRA that always fails
	mockJIRA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mockJIRA.Close()

	incidentID := insertTestIncident(t, pool)

	// Insert an event that is already at maxAttempts - 1
	id := fmt.Sprintf("test-exhaust-%d", time.Now().UnixNano())
	payload, _ := json.Marshal(map[string]any{"incident_id": incidentID})
	_, err := pool.Exec(ctx, `
		INSERT INTO integration_events (id, integration_name, event_type, status, payload, attempts, next_retry_at, created_at, updated_at)
		VALUES ($1, 'jira', 'issue_create', 'pending', $2, $3, NOW() - INTERVAL '1 second', NOW(), NOW())
	`, id, payload, maxAttempts-1) // one attempt away from the limit
	if err != nil {
		t.Fatalf("insert near-exhausted event: %v", err)
	}

	w := worker{
		db: pool, redis: redisClient,
		jira: jiraConfig{
			Enabled: true, BaseURL: mockJIRA.URL,
			Username: "x", APIToken: "x", ProjectKey: "X",
		},
		http: &http.Client{Timeout: 5 * time.Second},
	}

	_ = w.processDueEvents(ctx)

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM integration_events WHERE id = $1`, id).Scan(&status)
	if status != "failed" {
		t.Fatalf("event status = %q after exhausting retries, want failed", status)
	}
}

// ---------------------------------------------------------------------------
// Helper: build request body (not used for HTTP calls here, just for clarity)
// ---------------------------------------------------------------------------

func marshalJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func newJSONRequest(method, url string, body any) *http.Request {
	req, _ := http.NewRequest(method, url, bytes.NewReader(marshalJSON(body)))
	req.Header.Set("Content-Type", "application/json")
	return req
}
