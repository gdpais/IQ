package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"incidentiq/apps/api/internal/config"
)

func TestReportingFilterDateOnlyToIsExclusiveNextDay(t *testing.T) {
	req := httptest.NewRequest("GET", "/metrics/live?from=2026-06-01&to=2026-06-02", nil)
	filter, err := reportingFilterFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	wantFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	if filter.From == nil || !filter.From.Equal(wantFrom) {
		t.Fatalf("from = %#v, want %s", filter.From, wantFrom)
	}
	if filter.To == nil || !filter.To.Equal(wantTo) {
		t.Fatalf("to = %#v, want %s", filter.To, wantTo)
	}
}

func TestReportTypesFromQuerySplitsCommaSeparatedValues(t *testing.T) {
	req := httptest.NewRequest("POST", "/reports/snapshots/materialize?type=sre,dora", nil)
	values := reportTypesFromQuery(req)
	if len(values) != 2 || values[0] != "sre" || values[1] != "dora" {
		t.Fatalf("values = %#v", values)
	}
}

func TestCORSAllowsConfiguredOriginPreflight(t *testing.T) {
	server := &Server{cfg: config.Config{CORSAllowedOrigins: "http://localhost:5173"}}
	handler := server.withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/incidents", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d", resp.Code)
	}
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}
}

func TestDecodeSignedJSONAcceptsValidSignature(t *testing.T) {
	body := `{"service":"checkout"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/generic", strings.NewReader(body))
	req.Header.Set("X-IncidentIQ-Signature", testSignature("secret", body))

	var out map[string]string
	if err := decodeSignedJSON(req, "secret", &out); err != nil {
		t.Fatal(err)
	}
	if out["service"] != "checkout" {
		t.Fatalf("service = %q", out["service"])
	}
}

func TestDecodeSignedJSONRejectsInvalidSignature(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/generic", strings.NewReader(`{"service":"checkout"}`))
	req.Header.Set("X-IncidentIQ-Signature", "sha256=bad")

	var out map[string]string
	if err := decodeSignedJSON(req, "secret", &out); err != errInvalidWebhookSignature {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthorizationRequiresBearerTokenWhenConfigured(t *testing.T) {
	server := &Server{cfg: config.Config{APIAuthToken: "token"}}
	handler := withRole(server.withAuthorization(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/incidents", nil))
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d", resp.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/incidents", nil)
	req.Header.Set("Authorization", "Bearer token")
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status with token = %d", resp.Code)
	}
}

func TestRBACBlocksViewerMutation(t *testing.T) {
	server := &Server{cfg: config.Config{APIAuthToken: "token"}}
	handler := withRole(server.withAuthorization(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodPost, "/incidents", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Role", "viewer")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d", resp.Code)
	}
}

func TestClientIPUsesForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/incidents", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.2")
	if got := clientIP(req); got != "203.0.113.1" {
		t.Fatalf("clientIP = %q", got)
	}
}

func testSignature(secret string, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
