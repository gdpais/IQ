//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"incidentiq/apps/api/internal/config"
	"incidentiq/apps/api/internal/httpapi"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

func testServerSetup(t *testing.T) (baseURL string, cleanup func()) {
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

	// Apply up migration (idempotent — all CREATE TABLE IF NOT EXISTS)
	applyMigration(t, pool, "0001_init.up.sql")

	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		pool.Close()
		t.Skipf("Redis not reachable at %s — skipping: %v", redisAddr, err)
	}

	cfg := config.Config{
		Webhooks: config.WebhookSecrets{
			DynatraceSecret: "test-secret",
			ELKSecret:       "test-secret",
			GenericSecret:   "test-secret",
		},
		RateLimit: config.RateLimitConfig{
			APIRequestsPerMinute:     0, // disabled in tests
			WebhookRequestsPerMinute: 0,
		},
		CORSAllowedOrigins: "*",
	}

	srv := httpapi.New(cfg, pool, redisClient)
	ts := httptest.NewServer(srv.Handler())

	return ts.URL, func() {
		ts.Close()
		// Run down migration to clean up test data
		applyMigration(t, pool, "0001_init.down.sql")
		pool.Close()
		_ = redisClient.Close()
	}
}

func applyMigration(t *testing.T, pool *pgxpool.Pool, filename string) {
	t.Helper()
	_, testFile, _, _ := runtime.Caller(0)
	migrationPath := filepath.Join(filepath.Dir(testFile), "../../migrations", filename)
	content, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration %s: %v", filename, err)
	}
	if _, err := pool.Exec(context.Background(), string(content)); err != nil {
		t.Fatalf("apply migration %s: %v", filename, err)
	}
}

func signedWebhookRequest(t *testing.T, method, url, secret string, body any) *http.Request {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(data)
	req.Header.Set("X-IncidentIQ-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func apiRequest(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	var buf *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		buf = bytes.NewReader(data)
	} else {
		buf = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Role", "admin")
	req.Header.Set("X-Actor", "test")
	return req
}

func mustDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Phase 3: Webhook → Incident flow
// ---------------------------------------------------------------------------

// TestIntegrationAlertIngestsToNewIncident verifies the full path from
// webhook ingestion through normalization, correlation, and incident creation.
func TestIntegrationAlertIngestsToNewIncident(t *testing.T) {
	base, cleanup := testServerSetup(t)
	defer cleanup()

	payload := map[string]any{
		"source_event_id": fmt.Sprintf("evt-%d", time.Now().UnixNano()),
		"title":           "Integration test alert",
		"summary":         "Checkout error rate above 5%",
		"service":         "checkout-integration-test",
		"environment":     "test",
		"severity":        "high",
	}

	req := signedWebhookRequest(t, http.MethodPost, base+"/webhooks/generic", "test-secret", payload)
	resp := mustDo(t, req)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var result map[string]any
	decodeBody(t, resp, &result)

	inc, ok := result["incident"].(map[string]any)
	if !ok {
		t.Fatalf("incident missing from response: %#v", result)
	}
	incidentID, _ := inc["id"].(string)
	if incidentID == "" {
		t.Fatal("incident.id is empty")
	}
	if result["created_incident"] != true {
		t.Fatalf("created_incident = %v, want true", result["created_incident"])
	}
	if result["duplicate"] != false {
		t.Fatalf("duplicate = %v, want false", result["duplicate"])
	}
}

// TestIntegrationDuplicateAlertIsIdempotent verifies that sending the exact
// same alert (same source_event_id) twice does not create a second incident.
func TestIntegrationDuplicateAlertIsIdempotent(t *testing.T) {
	base, cleanup := testServerSetup(t)
	defer cleanup()

	payload := map[string]any{
		"source_event_id": fmt.Sprintf("dedup-evt-%d", time.Now().UnixNano()),
		"title":           "Idempotency test alert",
		"service":         "payments-integration-test",
		"environment":     "test",
		"severity":        "critical",
	}

	first := signedWebhookRequest(t, http.MethodPost, base+"/webhooks/generic", "test-secret", payload)
	r1 := mustDo(t, first)
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("first ingest: status = %d, want 201", r1.StatusCode)
	}
	var res1 map[string]any
	decodeBody(t, r1, &res1)

	// Send the same payload again
	second := signedWebhookRequest(t, http.MethodPost, base+"/webhooks/generic", "test-secret", payload)
	r2 := mustDo(t, second)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("duplicate ingest: status = %d, want 200", r2.StatusCode)
	}
	var res2 map[string]any
	decodeBody(t, r2, &res2)

	if res2["duplicate"] != true {
		t.Fatalf("second ingest: duplicate = %v, want true", res2["duplicate"])
	}

	// Both results must reference the same incident
	inc1, _ := res1["incident"].(map[string]any)
	inc2, _ := res2["incident"].(map[string]any)
	if inc1["id"] != inc2["id"] {
		t.Fatalf("first incident.id = %q, second incident.id = %q — should be the same", inc1["id"], inc2["id"])
	}
}

// TestIntegrationCorrelatedAlertUpdatesExistingIncident sends two alerts that
// share fingerprint+service+environment+severity within the correlation window,
// and verifies they are linked to the same incident.
func TestIntegrationCorrelatedAlertUpdatesExistingIncident(t *testing.T) {
	base, cleanup := testServerSetup(t)
	defer cleanup()

	fingerprint := fmt.Sprintf("corr-%d", time.Now().UnixNano())
	base_payload := map[string]any{
		"fingerprint": fingerprint,
		"title":       "Correlation test",
		"service":     "identity-integration-test",
		"environment": "test",
		"severity":    "high",
	}

	// First alert — creates the incident
	p1 := copyMap(base_payload)
	p1["source_event_id"] = "corr-evt-1"
	r1 := mustDo(t, signedWebhookRequest(t, http.MethodPost, base+"/webhooks/generic", "test-secret", p1))
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("first alert: status = %d, want 201", r1.StatusCode)
	}
	var res1 map[string]any
	decodeBody(t, r1, &res1)
	incidentID := res1["incident"].(map[string]any)["id"].(string)

	// Second alert — should correlate to the same incident
	p2 := copyMap(base_payload)
	p2["source_event_id"] = "corr-evt-2"
	r2 := mustDo(t, signedWebhookRequest(t, http.MethodPost, base+"/webhooks/generic", "test-secret", p2))
	var res2 map[string]any
	decodeBody(t, r2, &res2)

	if res2["created_incident"] == true {
		t.Fatalf("second alert should not have created a new incident")
	}
	if res2["correlated"] != true {
		t.Fatalf("correlated = %v, want true", res2["correlated"])
	}
	inc2ID := res2["incident"].(map[string]any)["id"].(string)
	if inc2ID != incidentID {
		t.Fatalf("correlated incident.id = %q, want %q", inc2ID, incidentID)
	}
}

// ---------------------------------------------------------------------------
// Phase 2: Lifecycle transitions
// ---------------------------------------------------------------------------

// TestIntegrationIncidentLifecycleEndToEnd verifies that an incident can be
// acknowledged, resolved, and reopened through the HTTP API.
func TestIntegrationIncidentLifecycleEndToEnd(t *testing.T) {
	base, cleanup := testServerSetup(t)
	defer cleanup()

	// Create an incident
	createReq := apiRequest(t, http.MethodPost, base+"/incidents", map[string]any{
		"title":       "Lifecycle integration test",
		"summary":     "Testing full lifecycle",
		"severity":    "high",
		"service":     "lifecycle-test",
		"environment": "test",
		"owner_team":  "sre",
		"actor":       "test",
	})
	cr := mustDo(t, createReq)
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201", cr.StatusCode)
	}
	var inc map[string]any
	decodeBody(t, cr, &inc)
	id := inc["id"].(string)

	// Acknowledge
	ackReq := apiRequest(t, http.MethodPost, base+"/incidents/"+id+"/acknowledge", nil)
	ackReq.Header.Set("X-Actor", "responder-1")
	ar := mustDo(t, ackReq)
	if ar.StatusCode != http.StatusOK {
		t.Fatalf("acknowledge: status = %d, want 200", ar.StatusCode)
	}
	var acked map[string]any
	decodeBody(t, ar, &acked)
	if acked["status"] != "acknowledged" {
		t.Fatalf("status after ack = %q, want acknowledged", acked["status"])
	}
	if acked["acknowledged_at"] == nil {
		t.Fatal("acknowledged_at is nil after acknowledgement")
	}

	// Resolve
	resolveReq := apiRequest(t, http.MethodPost, base+"/incidents/"+id+"/resolve", nil)
	rr := mustDo(t, resolveReq)
	if rr.StatusCode != http.StatusOK {
		t.Fatalf("resolve: status = %d, want 200", rr.StatusCode)
	}
	var resolved map[string]any
	decodeBody(t, rr, &resolved)
	if resolved["status"] != "resolved" {
		t.Fatalf("status after resolve = %q, want resolved", resolved["status"])
	}
	if resolved["resolved_at"] == nil {
		t.Fatal("resolved_at is nil after resolution")
	}

	// Reopen
	reopenReq := apiRequest(t, http.MethodPost, base+"/incidents/"+id+"/reopen", nil)
	ror := mustDo(t, reopenReq)
	if ror.StatusCode != http.StatusOK {
		t.Fatalf("reopen: status = %d, want 200", ror.StatusCode)
	}
	var reopened map[string]any
	decodeBody(t, ror, &reopened)
	if reopened["status"] != "open" {
		t.Fatalf("status after reopen = %q, want open", reopened["status"])
	}
	if reopened["resolved_at"] != nil {
		t.Fatalf("resolved_at should be nil after reopen, got %v", reopened["resolved_at"])
	}

	// Verify timeline events were recorded
	detailReq := apiRequest(t, http.MethodGet, base+"/incidents/"+id, nil)
	dr := mustDo(t, detailReq)
	var detail map[string]any
	decodeBody(t, dr, &detail)
	events := detail["events"].([]any)
	eventTypes := make([]string, 0, len(events))
	for _, e := range events {
		ev := e.(map[string]any)
		eventTypes = append(eventTypes, ev["type"].(string))
	}
	want := []string{"incident_created", "incident_acknowledged", "incident_resolved", "incident_reopened"}
	for _, wt := range want {
		found := false
		for _, et := range eventTypes {
			if et == wt {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected event type %q in timeline %v", wt, eventTypes)
		}
	}
}

// TestIntegrationInvalidLifecycleTransitionReturns400 verifies that attempting
// an illegal transition returns a 400 and does not corrupt state.
func TestIntegrationInvalidLifecycleTransitionReturns400(t *testing.T) {
	base, cleanup := testServerSetup(t)
	defer cleanup()

	// Create an open incident
	createReq := apiRequest(t, http.MethodPost, base+"/incidents", map[string]any{
		"title": "Bad transition test", "severity": "low",
		"service": "bad-trans-test", "environment": "test", "owner_team": "sre",
	})
	cr := mustDo(t, createReq)
	var inc map[string]any
	decodeBody(t, cr, &inc)
	id := inc["id"].(string)

	// Try to reopen an open incident — illegal
	reopenReq := apiRequest(t, http.MethodPost, base+"/incidents/"+id+"/reopen", nil)
	rr := mustDo(t, reopenReq)
	if rr.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid transition status = %d, want 400", rr.StatusCode)
	}
	var errResp map[string]any
	decodeBody(t, rr, &errResp)
	if errResp["error"] == nil {
		t.Fatal("expected error field in response")
	}
}

// ---------------------------------------------------------------------------
// Phase 4: JIRA sync via incident creation
// ---------------------------------------------------------------------------

// TestIntegrationJIRASyncOnIncidentCreate verifies that creating an incident
// queues a JIRA sync attempt and — when JIRA is configured and reachable —
// records an integration event.
func TestIntegrationJIRASyncCreatesIntegrationEvent(t *testing.T) {
	base, cleanup := testServerSetup(t)
	defer cleanup()

	// Start a mock JIRA server
	jiraRequests := 0
	mockJIRA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jiraRequests++
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issue"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"key":"TEST-1","id":"10001"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockJIRA.Close()

	// Use a new server with JIRA enabled
	dbURL := os.Getenv("TEST_DATABASE_URL")
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("db connect: %v", err)
	}
	defer pool.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer redisClient.Close()

	cfg := config.Config{
		CORSAllowedOrigins: "*",
		JIRA: config.JIRAConfig{
			Enabled:    true,
			BaseURL:    mockJIRA.URL,
			Username:   "test",
			APIToken:   "test-token",
			ProjectKey: "TEST",
		},
	}

	srv := httpapi.New(cfg, pool, redisClient)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Create an incident
	createReq := apiRequest(t, http.MethodPost, ts.URL+"/incidents", map[string]any{
		"title": "JIRA sync test", "severity": "high",
		"service": "jira-test", "environment": "test", "owner_team": "sre",
	})
	cr := mustDo(t, createReq)
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create incident: status = %d, want 201", cr.StatusCode)
	}

	// Verify JIRA was called
	if jiraRequests == 0 {
		t.Fatal("expected at least one JIRA API call")
	}

	// Verify integration event was recorded
	eventsReq := apiRequest(t, http.MethodGet, ts.URL+"/integration-events?integration=jira&limit=10", nil)
	evr := mustDo(t, eventsReq)
	var events []map[string]any
	decodeBody(t, evr, &events)
	if len(events) == 0 {
		t.Fatal("expected integration events to be recorded after JIRA sync")
	}
}

// TestIntegrationJIRAOutageCreatesRetryableEvent verifies that when JIRA is
// unavailable, a retryable integration event is created and the incident state
// is not affected (source-of-truth preservation).
func TestIntegrationJIRAOutageCreatesRetryableEvent(t *testing.T) {
	base, cleanup := testServerSetup(t)
	defer cleanup()

	// Mock JIRA that always fails
	mockJIRA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"errorMessages":["JIRA is down"]}`)
	}))
	defer mockJIRA.Close()

	dbURL := os.Getenv("TEST_DATABASE_URL")
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	ctx := context.Background()
	pool, _ := pgxpool.New(ctx, dbURL)
	defer pool.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer redisClient.Close()

	cfg := config.Config{
		CORSAllowedOrigins: "*",
		JIRA: config.JIRAConfig{
			Enabled: true, BaseURL: mockJIRA.URL,
			Username: "test", APIToken: "test", ProjectKey: "TEST",
		},
	}
	srv := httpapi.New(cfg, pool, redisClient)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Create incident — JIRA sync will fail
	createReq := apiRequest(t, http.MethodPost, ts.URL+"/incidents", map[string]any{
		"title": "JIRA outage test", "severity": "critical",
		"service": "outage-test", "environment": "test", "owner_team": "sre",
	})
	cr := mustDo(t, createReq)

	// Incident must still be created successfully (IncidentIQ is authoritative)
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create incident during JIRA outage: status = %d, want 201", cr.StatusCode)
	}
	var inc map[string]any
	decodeBody(t, cr, &inc)
	if inc["id"] == nil {
		t.Fatal("incident.id missing — incident was not created despite JIRA outage")
	}

	// Retryable integration event must be recorded
	eventsReq := apiRequest(t, http.MethodGet, ts.URL+"/integration-events?integration=jira&status=pending&limit=10", nil)
	evr := mustDo(t, eventsReq)
	var events []map[string]any
	decodeBody(t, evr, &events)
	if len(events) == 0 {
		t.Fatal("expected a pending retryable integration event after JIRA failure")
	}
}

// ---------------------------------------------------------------------------
// Phase 5 / E2E: Alert → Incident → Lifecycle → Metrics consistency
// ---------------------------------------------------------------------------

// TestIntegrationAlertToReportWorkflow is the end-to-end proof that:
//  1. An alert produces an incident
//  2. The incident moves through lifecycle (ack → resolve)
//  3. Live metrics and CSV export reflect the final state consistently
func TestIntegrationAlertToReportWorkflow(t *testing.T) {
	base, cleanup := testServerSetup(t)
	defer cleanup()

	// 1. Ingest an alert via webhook
	alertPayload := map[string]any{
		"source_event_id": fmt.Sprintf("e2e-%d", time.Now().UnixNano()),
		"title":           "E2E test alert",
		"service":         "e2e-service",
		"environment":     "e2e-env",
		"severity":        "critical",
	}
	wr := mustDo(t, signedWebhookRequest(t, http.MethodPost, base+"/webhooks/generic", "test-secret", alertPayload))
	if wr.StatusCode != http.StatusCreated {
		t.Fatalf("alert ingest: status = %d, want 201", wr.StatusCode)
	}
	var wResult map[string]any
	decodeBody(t, wr, &wResult)
	incidentID := wResult["incident"].(map[string]any)["id"].(string)

	// 2. Acknowledge the incident
	ackR := mustDo(t, apiRequest(t, http.MethodPost, base+"/incidents/"+incidentID+"/acknowledge", nil))
	if ackR.StatusCode != http.StatusOK {
		t.Fatalf("acknowledge: status = %d, want 200", ackR.StatusCode)
	}
	ackR.Body.Close()

	// 3. Resolve the incident
	resolveR := mustDo(t, apiRequest(t, http.MethodPost, base+"/incidents/"+incidentID+"/resolve", nil))
	if resolveR.StatusCode != http.StatusOK {
		t.Fatalf("resolve: status = %d, want 200", resolveR.StatusCode)
	}
	resolveR.Body.Close()

	// 4. Verify live metrics reflect the resolved incident
	metricsR := mustDo(t, apiRequest(t, http.MethodGet,
		base+"/metrics/live?service=e2e-service&environment=e2e-env", nil))
	var metrics map[string]any
	decodeBody(t, metricsR, &metrics)

	resolvedCount, _ := metrics["resolved_incident_count"].(float64)
	if resolvedCount < 1 {
		t.Fatalf("resolved_incident_count = %.0f, want >= 1", resolvedCount)
	}

	// 5. Verify CSV export is consistent with metrics
	csvR := mustDo(t, apiRequest(t, http.MethodGet,
		base+"/exports/incidents.csv?service=e2e-service&environment=e2e-env", nil))
	if csvR.StatusCode != http.StatusOK {
		t.Fatalf("CSV export: status = %d, want 200", csvR.StatusCode)
	}
	defer csvR.Body.Close()
	if ct := csvR.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("Content-Type = %q, want text/csv", ct)
	}

	// CSV line count (header + data rows) must be consistent with incident count
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(csvR.Body)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	// lines[0] = header; remaining = data rows
	csvIncidentCount := int64(len(lines) - 1)
	if csvIncidentCount < int64(resolvedCount) {
		t.Fatalf("CSV has %d rows but metrics shows %d resolved incidents", csvIncidentCount, int64(resolvedCount))
	}
}

// ---------------------------------------------------------------------------
// Phase 1: Migration apply/rollback
// ---------------------------------------------------------------------------

// TestIntegrationMigrationsApplyAndRollback verifies that the up migration is
// idempotent and the down migration cleanly removes all objects.
func TestIntegrationMigrationsApplyAndRollback(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	_, testFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(testFile), "../../migrations")

	// Apply up (idempotent)
	upSQL, err := os.ReadFile(filepath.Join(migrationsDir, "0001_init.up.sql"))
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(upSQL)); err != nil {
		t.Fatalf("apply up migration: %v", err)
	}

	// Apply up again — must not error (IF NOT EXISTS)
	if _, err := pool.Exec(ctx, string(upSQL)); err != nil {
		t.Fatalf("re-apply up migration (idempotency check): %v", err)
	}

	// Verify key tables exist
	for _, table := range []string{"incidents", "alerts", "jira_links", "audit_log", "integration_events"} {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists)
		if err != nil || !exists {
			t.Errorf("table %q does not exist after up migration (err=%v)", table, err)
		}
	}

	// Roll back
	downSQL, err := os.ReadFile(filepath.Join(migrationsDir, "0001_init.down.sql"))
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(downSQL)); err != nil {
		t.Fatalf("apply down migration: %v", err)
	}

	// Verify tables are gone
	for _, table := range []string{"incidents", "alerts", "jira_links"} {
		var exists bool
		_ = pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists)
		if exists {
			t.Errorf("table %q still exists after down migration", table)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func copyMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
