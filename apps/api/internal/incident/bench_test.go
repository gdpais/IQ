package incident

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkNormalizeAlert measures normalization throughput for the generic
// webhook path — the hot path for high-volume alert ingestion.
func BenchmarkNormalizeAlertGeneric(b *testing.B) {
	now := time.Now().UTC()
	payload := map[string]any{
		"source_event_id": "evt-bench",
		"fingerprint":     "payment-latency",
		"title":           "Payment service p99 latency exceeded",
		"summary":         "Payment service p99 latency exceeded 2000ms threshold",
		"service":         "payments",
		"environment":     "production",
		"severity":        "P2",
		"observed_at":     "2026-06-02T10:00:00Z",
		"metadata": map[string]any{
			"api_token":   "super-secret",
			"region":      "us-east-1",
			"instance_id": "i-0abc123",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = NormalizeAlert(AlertSourceGeneric, payload, now)
	}
}

// BenchmarkNormalizeAlertDynatrace measures normalization for Dynatrace payloads,
// which include nested field resolution and millisecond timestamp parsing.
func BenchmarkNormalizeAlertDynatrace(b *testing.B) {
	now := time.Now().UTC()
	payload := map[string]any{
		"problemId":      "P-12345",
		"problemTitle":   "CPU saturation on payments-api",
		"impactedEntity": "payments-api",
		"environment":    "production",
		"severityLevel":  "availability",
		"startTime":      float64(1780394100000),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = NormalizeAlert(AlertSourceDynatrace, payload, now)
	}
}

// BenchmarkNormalizeAlertELK measures normalization for Kibana/ELK payloads,
// which require nested path traversal.
func BenchmarkNormalizeAlertELK(b *testing.B) {
	now := time.Now().UTC()
	payload := map[string]any{
		"kibana": map[string]any{
			"alert": map[string]any{
				"uuid":     "alert-uuid-001",
				"severity": "critical",
				"rule": map[string]any{
					"uuid": "rule-uuid-001",
				},
			},
		},
		"rule": map[string]any{
			"id":          "rule-001",
			"name":        "Error budget burn",
			"description": "Error budget is burning too fast",
		},
		"service": map[string]any{
			"name":        "identity",
			"environment": "production",
		},
		"labels": map[string]any{
			"environment": "production",
		},
		"@timestamp": "2026-06-02T08:15:30Z",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = NormalizeAlert(AlertSourceELK, payload, now)
	}
}

// BenchmarkRedactPayload measures redaction throughput for a moderately complex
// nested payload with several sensitive fields.
func BenchmarkRedactPayload(b *testing.B) {
	payload := map[string]any{
		"service":      "billing",
		"environment":  "production",
		"severity":     "high",
		"api_key":      "ak-secret-123",
		"token":        "bearer-token-456",
		"credentials":  "user:pass",
		"safe_field_1": "safe-value-1",
		"safe_field_2": "safe-value-2",
		"nested": map[string]any{
			"password":    "nested-secret",
			"message":     "queue depth high",
			"safe_nested": "safe",
			"deep": map[string]any{
				"authorization": "Basic xyz",
				"value":         42,
			},
		},
		"items": []any{
			map[string]any{"api_key": "item-key-1", "name": "first"},
			map[string]any{"api_key": "item-key-2", "name": "second"},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RedactPayload(payload)
	}
}

// BenchmarkStableFingerprint measures fingerprint derivation — called on every
// alert that doesn't carry an explicit fingerprint field.
func BenchmarkStableFingerprint(b *testing.B) {
	alert := NormalizedAlert{
		Source:      AlertSourceGeneric,
		Service:     "checkout",
		Environment: "production",
		Severity:    "high",
		Summary:     "Checkout service error rate above 5%",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = stableFingerprint(alert)
	}
}

// BenchmarkNormalizeAlertHighVolume simulates a burst of 1 000 unique alerts
// from different services/environments to measure throughput at scale.
func BenchmarkNormalizeAlertHighVolume(b *testing.B) {
	now := time.Now().UTC()
	services := []string{"checkout", "identity", "payments", "billing", "shipping", "search", "recommendations", "auth", "notifications", "inventory"}
	environments := []string{"prod", "staging", "canary"}
	severities := []string{"critical", "high", "medium", "low"}

	payloads := make([]map[string]any, 100)
	for i := range payloads {
		payloads[i] = map[string]any{
			"source_event_id": fmt.Sprintf("evt-%d", i),
			"title":           fmt.Sprintf("Alert %d for %s", i, services[i%len(services)]),
			"service":         services[i%len(services)],
			"environment":     environments[i%len(environments)],
			"severity":        severities[i%len(severities)],
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		payload := payloads[i%len(payloads)]
		_, _ = NormalizeAlert(AlertSourceGeneric, payload, now)
	}
}
