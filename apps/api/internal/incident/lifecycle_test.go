package incident

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ValidateTransition — lifecycle state machine
// ---------------------------------------------------------------------------

func TestValidateTransitionAllowsLegalPaths(t *testing.T) {
	cases := []struct {
		from    Status
		to      Status
		wantErr bool
	}{
		// legal forward paths
		{StatusOpen, StatusAcknowledged, false},
		{StatusOpen, StatusResolved, false},  // resolve directly without ack
		{StatusAcknowledged, StatusResolved, false},
		{StatusResolved, StatusOpen, false}, // reopen

		// illegal backward / nonsensical paths
		{StatusAcknowledged, StatusOpen, true},       // cannot de-acknowledge
		{StatusResolved, StatusAcknowledged, true},   // cannot un-resolve to ack
		{StatusResolved, StatusResolved, true},       // already resolved
		{StatusOpen, StatusOpen, true},               // already open
		{StatusAcknowledged, StatusAcknowledged, true}, // already acknowledged
	}

	for _, tc := range cases {
		err := ValidateTransition(tc.from, tc.to)
		if tc.wantErr && err == nil {
			t.Errorf("ValidateTransition(%s → %s) expected error, got nil", tc.from, tc.to)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ValidateTransition(%s → %s) unexpected error: %v", tc.from, tc.to, err)
		}
	}
}

func TestValidateTransitionRejectsUnknownTargetStatus(t *testing.T) {
	err := ValidateTransition(StatusOpen, "unknown_status")
	if err == nil {
		t.Fatal("expected error for unknown target status, got nil")
	}
}

func TestValidateTransitionErrorMessagesAreDescriptive(t *testing.T) {
	err := ValidateTransition(StatusResolved, StatusAcknowledged)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "resolved") || !strings.Contains(msg, "acknowledged") {
		t.Fatalf("error message lacks status names: %q", msg)
	}
}

// ---------------------------------------------------------------------------
// NormalizeAlert — edge cases and normalization rules
// ---------------------------------------------------------------------------

func TestNormalizeAlertRejectsUnsupportedSource(t *testing.T) {
	_, err := NormalizeAlert("pagerduty", map[string]any{}, time.Now())
	if err == nil {
		t.Fatal("expected error for unsupported source")
	}
}

func TestNormalizeAlertHandlesNilPayload(t *testing.T) {
	alert, err := NormalizeAlert(AlertSourceGeneric, nil, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alert.Service != "unknown" {
		t.Fatalf("Service = %q, want %q", alert.Service, "unknown")
	}
	if alert.Environment != "unknown" {
		t.Fatalf("Environment = %q, want %q", alert.Environment, "unknown")
	}
	if alert.Severity != "unknown" {
		t.Fatalf("Severity = %q, want %q", alert.Severity, "unknown")
	}
	if alert.SourceEventID == "" {
		t.Fatal("SourceEventID must be derived when not provided")
	}
}

func TestNormalizeAlertNormalisesSourceToLowercase(t *testing.T) {
	alert, err := NormalizeAlert("DYNATRACE", map[string]any{"problemId": "P-1"}, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alert.Source != "dynatrace" {
		t.Fatalf("Source = %q, want dynatrace", alert.Source)
	}
}

func TestNormalizeAlertNormalisesServiceAndEnvironmentToLowercase(t *testing.T) {
	alert, err := NormalizeAlert(AlertSourceGeneric, map[string]any{
		"service":     "Order-Service",
		"environment": "PRODUCTION",
		"severity":    "high",
	}, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alert.Service != "order-service" {
		t.Fatalf("Service = %q, want order-service", alert.Service)
	}
	if alert.Environment != "production" {
		t.Fatalf("Environment = %q, want production", alert.Environment)
	}
}

func TestNormalizeSeverityMapsAllKnownValues(t *testing.T) {
	cases := map[string]string{
		"sev1": "critical", "p1": "critical", "critical": "critical",
		"availability": "critical", "error": "critical",
		"sev2": "high", "p2": "high", "high": "high", "performance": "high",
		"sev3": "medium", "p3": "medium", "medium": "medium", "warning": "medium", "warn": "medium",
		"sev4": "low", "p4": "low", "low": "low", "info": "low", "informational": "low",
	}
	for input, want := range cases {
		if got := normalizeSeverity(input); got != want {
			t.Errorf("normalizeSeverity(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeSeverityPassesThroughUnknownValues(t *testing.T) {
	if got := normalizeSeverity("DISASTER"); got != "disaster" {
		t.Fatalf("normalizeSeverity(DISASTER) = %q, want disaster", got)
	}
}

func TestFingerprintIsStableAndNonEmpty(t *testing.T) {
	alert := NormalizedAlert{
		Source:      AlertSourceGeneric,
		Service:     "checkout",
		Environment: "prod",
		Severity:    "high",
		Summary:     "Checkout error rate elevated",
	}
	fp1 := stableFingerprint(alert)
	fp2 := stableFingerprint(alert)
	if fp1 == "" {
		t.Fatal("fingerprint is empty")
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable: %q vs %q", fp1, fp2)
	}
}

func TestFingerprintDiffersForDifferentAlerts(t *testing.T) {
	a := NormalizedAlert{Source: AlertSourceGeneric, Service: "checkout", Environment: "prod", Severity: "high"}
	b := NormalizedAlert{Source: AlertSourceGeneric, Service: "identity", Environment: "prod", Severity: "high"}
	if stableFingerprint(a) == stableFingerprint(b) {
		t.Fatal("different alerts produced identical fingerprints")
	}
}

func TestRedactPayloadIsDeepAndRecursive(t *testing.T) {
	payload := map[string]any{
		"service": "billing",
		"nested": map[string]any{
			"password": "very-secret",
			"message":  "queue depth exceeded",
		},
		"token": "bearer-abc",
	}
	redacted := RedactPayload(payload)

	if redacted["service"] != "billing" {
		t.Errorf("non-sensitive top-level field was changed: %#v", redacted["service"])
	}
	nested, ok := redacted["nested"].(map[string]any)
	if !ok {
		t.Fatal("nested object lost after redaction")
	}
	if nested["password"] != "***REDACTED***" {
		t.Errorf("nested password was not redacted: %#v", nested["password"])
	}
	if nested["message"] != "queue depth exceeded" {
		t.Errorf("non-sensitive nested field was changed: %#v", nested["message"])
	}
	if redacted["token"] != "***REDACTED***" {
		t.Errorf("top-level token was not redacted: %#v", redacted["token"])
	}
}

func TestRedactPayloadHandlesSliceValues(t *testing.T) {
	payload := map[string]any{
		"items": []any{
			map[string]any{"api_key": "key-1", "name": "billing"},
			map[string]any{"api_key": "key-2", "name": "identity"},
		},
	}
	redacted := RedactPayload(payload)
	items, ok := redacted["items"].([]any)
	if !ok {
		t.Fatal("items slice lost after redaction")
	}
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("items[%d] is not a map: %T", i, item)
		}
		if obj["api_key"] != "***REDACTED***" {
			t.Errorf("items[%d].api_key was not redacted: %#v", i, obj["api_key"])
		}
	}
}

func TestELKSourceEventIDPrefersKibanaAlertUUID(t *testing.T) {
	payload := map[string]any{
		"id": "doc-id",
		"kibana": map[string]any{
			"alert": map[string]any{
				"uuid": "kibana-uuid-123",
			},
		},
		"rule": map[string]any{"id": "rule-9"},
	}
	alert, err := NormalizeAlert(AlertSourceELK, payload, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alert.SourceEventID != "kibana-uuid-123" {
		t.Fatalf("SourceEventID = %q, want kibana-uuid-123", alert.SourceEventID)
	}
}

func TestDynatraceMillisecondTimestampParsed(t *testing.T) {
	// Dynatrace sends startTime as unix milliseconds
	msTimestamp := float64(1780394100000) // 2026-06-01 in ms
	payload := map[string]any{
		"problemId":     "P-99",
		"startTime":     msTimestamp,
		"severityLevel": "high",
	}
	alert, err := NormalizeAlert(AlertSourceDynatrace, payload, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be parsed as milliseconds (not seconds — that would be ~56000 CE)
	year := alert.ObservedAt.Year()
	if year < 2025 || year > 2030 {
		t.Fatalf("ObservedAt year = %d (expected ~2026), looks like ms/s confusion", year)
	}
}
