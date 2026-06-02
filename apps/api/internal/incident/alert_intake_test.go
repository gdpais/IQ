package incident

import (
	"testing"
	"time"
)

func TestNormalizeGenericAlert(t *testing.T) {
	now := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	payload := map[string]any{
		"source_event_id": "evt-123",
		"fingerprint":     "checkout-latency",
		"title":           "Checkout latency",
		"summary":         "Checkout p95 latency exceeded threshold",
		"service":         "Checkout",
		"environment":     "Prod",
		"severity":        "P2",
		"observed_at":     "2026-06-02T09:55:00Z",
		"metadata": map[string]any{
			"api_token": "super-secret",
		},
	}

	alert, err := NormalizeAlert(AlertSourceGeneric, payload, now)
	if err != nil {
		t.Fatalf("NormalizeAlert returned error: %v", err)
	}

	if alert.SourceEventID != "evt-123" {
		t.Fatalf("SourceEventID = %q, want evt-123", alert.SourceEventID)
	}
	if alert.Service != "checkout" {
		t.Fatalf("Service = %q, want checkout", alert.Service)
	}
	if alert.Environment != "prod" {
		t.Fatalf("Environment = %q, want prod", alert.Environment)
	}
	if alert.Severity != "high" {
		t.Fatalf("Severity = %q, want high", alert.Severity)
	}
	if !alert.ObservedAt.Equal(time.Date(2026, 6, 2, 9, 55, 0, 0, time.UTC)) {
		t.Fatalf("ObservedAt = %s", alert.ObservedAt)
	}

	metadata, ok := alert.RedactedPayload["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("redacted metadata missing or wrong type")
	}
	if metadata["api_token"] != "***REDACTED***" {
		t.Fatalf("api_token was not redacted: %#v", metadata["api_token"])
	}
}

func TestNormalizeDynatraceAlert(t *testing.T) {
	now := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	payload := map[string]any{
		"problemId":      "P-42",
		"problemTitle":   "CPU saturation",
		"impactedEntity": "payments-api",
		"environment":    "production",
		"severityLevel":  "availability",
		"startTime":      float64(1780394100000),
	}

	alert, err := NormalizeAlert(AlertSourceDynatrace, payload, now)
	if err != nil {
		t.Fatalf("NormalizeAlert returned error: %v", err)
	}

	if alert.SourceEventID != "P-42" {
		t.Fatalf("SourceEventID = %q, want P-42", alert.SourceEventID)
	}
	if alert.Fingerprint != "p-42" {
		t.Fatalf("Fingerprint = %q, want p-42", alert.Fingerprint)
	}
	if alert.Severity != "critical" {
		t.Fatalf("Severity = %q, want critical", alert.Severity)
	}
}

func TestNormalizeELKAlertReadsNestedFields(t *testing.T) {
	now := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	payload := map[string]any{
		"rule": map[string]any{
			"id":   "rule-7",
			"name": "Error budget burn",
		},
		"service": map[string]any{
			"name": "identity",
		},
		"labels": map[string]any{
			"environment": "prod",
		},
		"kibana": map[string]any{
			"alert": map[string]any{
				"uuid":     "alert-7",
				"severity": "warning",
			},
		},
		"@timestamp": "2026-06-02T08:15:30Z",
	}

	alert, err := NormalizeAlert(AlertSourceELK, payload, now)
	if err != nil {
		t.Fatalf("NormalizeAlert returned error: %v", err)
	}

	if alert.SourceEventID != "alert-7" {
		t.Fatalf("SourceEventID = %q, want alert-7", alert.SourceEventID)
	}
	if alert.Fingerprint != "rule-7" {
		t.Fatalf("Fingerprint = %q, want rule-7", alert.Fingerprint)
	}
	if alert.Service != "identity" {
		t.Fatalf("Service = %q, want identity", alert.Service)
	}
	if alert.Severity != "medium" {
		t.Fatalf("Severity = %q, want medium", alert.Severity)
	}
}

func TestNormalizeAlertDerivesStableSourceEventID(t *testing.T) {
	now := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	payload := map[string]any{
		"summary":     "Queue depth high",
		"service":     "billing",
		"environment": "prod",
		"severity":    "high",
	}

	first, err := NormalizeAlert(AlertSourceGeneric, payload, now)
	if err != nil {
		t.Fatalf("NormalizeAlert returned error: %v", err)
	}
	second, err := NormalizeAlert(AlertSourceGeneric, payload, now)
	if err != nil {
		t.Fatalf("NormalizeAlert returned error: %v", err)
	}

	if first.SourceEventID == "" {
		t.Fatalf("SourceEventID was empty")
	}
	if first.SourceEventID != second.SourceEventID {
		t.Fatalf("SourceEventID is not stable: %q != %q", first.SourceEventID, second.SourceEventID)
	}
}
