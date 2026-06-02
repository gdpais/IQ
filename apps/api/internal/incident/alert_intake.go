package incident

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	AlertSourceDynatrace = "dynatrace"
	AlertSourceELK       = "elk"
	AlertSourceGeneric   = "generic"
)

var errInvalidAlert = errors.New("invalid alert payload")

func NormalizeAlert(source string, payload map[string]any, now time.Time) (NormalizedAlert, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	if source != AlertSourceDynatrace && source != AlertSourceELK && source != AlertSourceGeneric {
		return NormalizedAlert{}, fmt.Errorf("%w: unsupported source %q", errInvalidAlert, source)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	alert := NormalizedAlert{
		Source:          source,
		Payload:         payload,
		RedactedPayload: RedactPayload(payload),
		ObservedAt:      now.UTC(),
	}

	switch source {
	case AlertSourceDynatrace:
		normalizeDynatrace(&alert, payload)
	case AlertSourceELK:
		normalizeELK(&alert, payload)
	case AlertSourceGeneric:
		normalizeGeneric(&alert, payload)
	}

	alert.SourceEventID = fallback(alert.SourceEventID, stablePayloadID(source, payload))
	alert.Service = fallback(normalizeToken(alert.Service), "unknown")
	alert.Environment = fallback(normalizeToken(alert.Environment), "unknown")
	alert.Severity = fallback(normalizeSeverity(alert.Severity), "unknown")
	alert.Summary = fallback(strings.TrimSpace(alert.Summary), fmt.Sprintf("%s alert for %s", source, alert.Service))
	alert.Title = fallback(strings.TrimSpace(alert.Title), alert.Summary)
	alert.Fingerprint = fallback(normalizeToken(alert.Fingerprint), stableFingerprint(alert))

	return alert, nil
}

func normalizeDynatrace(alert *NormalizedAlert, payload map[string]any) {
	alert.SourceEventID = firstString(payload, "eventId", "event_id", "problemId", "problem_id", "PID", "id")
	alert.Fingerprint = firstString(payload, "fingerprint", "problemId", "problem_id", "PID")
	alert.Title = firstString(payload, "title", "displayName", "problemTitle", "problem_title")
	alert.Summary = firstString(payload, "summary", "description", "details", "problemTitle", "displayName")
	alert.Service = firstString(payload, "service", "serviceName", "service_name", "impactedEntity", "impacted_entity", "entityName", "entity_name")
	alert.Environment = firstString(payload, "environment", "env", "stage")
	alert.Severity = firstString(payload, "severity", "severityLevel", "severity_level", "eventType", "event_type")
	if observed := firstTime(payload, "observed_at", "observedAt", "startTime", "start_time", "timestamp"); !observed.IsZero() {
		alert.ObservedAt = observed
	}
}

func normalizeELK(alert *NormalizedAlert, payload map[string]any) {
	alert.SourceEventID = firstString(payload, "alert_id", "alertId", "kibana.alert.uuid", "event.id", "id", "rule.id")
	alert.Fingerprint = firstString(payload, "fingerprint", "kibana.alert.rule.uuid", "rule.id", "event.id")
	alert.Title = firstString(payload, "title", "rule.name", "alert.name", "message")
	alert.Summary = firstString(payload, "summary", "message", "rule.description", "title")
	alert.Service = firstString(payload, "service", "service.name", "serviceName", "labels.service")
	alert.Environment = firstString(payload, "environment", "env", "labels.environment", "service.environment")
	alert.Severity = firstString(payload, "severity", "event.severity", "kibana.alert.severity", "rule.severity")
	if observed := firstTime(payload, "observed_at", "observedAt", "@timestamp", "event.created", "timestamp"); !observed.IsZero() {
		alert.ObservedAt = observed
	}
}

func normalizeGeneric(alert *NormalizedAlert, payload map[string]any) {
	alert.SourceEventID = firstString(payload, "source_event_id", "event_id", "alert_id", "id")
	alert.Fingerprint = firstString(payload, "fingerprint", "dedupe_key", "correlation_key")
	alert.Title = firstString(payload, "title", "name", "summary")
	alert.Summary = firstString(payload, "summary", "description", "message", "title")
	alert.Service = firstString(payload, "service", "service_name")
	alert.Environment = firstString(payload, "environment", "env")
	alert.Severity = firstString(payload, "severity", "priority")
	if observed := firstTime(payload, "observed_at", "observedAt", "timestamp", "created_at"); !observed.IsZero() {
		alert.ObservedAt = observed
	}
}

func RedactPayload(payload map[string]any) map[string]any {
	out, ok := redactValue(payload).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return out
}

func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if isSensitiveKey(key) {
				out[key] = "***REDACTED***"
				continue
			}
			out[key] = redactValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactValue(item)
		}
		return out
	default:
		return typed
	}
}

func isSensitiveKey(key string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(key), "-", "_")
	sensitive := []string{"password", "passwd", "secret", "token", "api_key", "apikey", "authorization", "credential", "private_key", "access_key"}
	for _, item := range sensitive {
		if normalized == item || strings.Contains(normalized, item) {
			return true
		}
	}
	return normalized == "auth"
}

func firstString(payload map[string]any, paths ...string) string {
	for _, path := range paths {
		value, ok := valueAtPath(payload, path)
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		case float64:
			return strings.TrimSpace(fmt.Sprintf("%.0f", typed))
		case int:
			return fmt.Sprintf("%d", typed)
		case int64:
			return fmt.Sprintf("%d", typed)
		}
	}
	return ""
}

func firstTime(payload map[string]any, paths ...string) time.Time {
	for _, path := range paths {
		value, ok := valueAtPath(payload, path)
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if parsed := parseAlertTime(typed); !parsed.IsZero() {
				return parsed
			}
		case float64:
			if parsed := unixAlertTime(typed); !parsed.IsZero() {
				return parsed
			}
		case int64:
			if parsed := unixAlertTime(float64(typed)); !parsed.IsZero() {
				return parsed
			}
		case int:
			if parsed := unixAlertTime(float64(typed)); !parsed.IsZero() {
				return parsed
			}
		}
	}
	return time.Time{}
}

func valueAtPath(payload map[string]any, path string) (any, bool) {
	if value, ok := payload[path]; ok {
		return value, true
	}

	current := any(payload)
	for _, part := range strings.Split(path, ".") {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func parseAlertTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"}
	for _, format := range formats {
		if parsed, err := time.Parse(format, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func unixAlertTime(value float64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	if value > 1_000_000_000_000 {
		return time.UnixMilli(int64(value)).UTC()
	}
	return time.Unix(int64(value), 0).UTC()
}

func stablePayloadID(source string, payload map[string]any) string {
	return source + "-" + stableHash(payload)
}

func stableFingerprint(alert NormalizedAlert) string {
	parts := []string{alert.Source, alert.Service, alert.Environment, alert.Severity, alert.Summary}
	return stableHash(parts)
}

func stableHash(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizeToken(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeSeverity(value string) string {
	value = normalizeToken(value)
	switch value {
	case "sev1", "p1", "critical", "availability", "error":
		return "critical"
	case "sev2", "p2", "high", "performance":
		return "high"
	case "sev3", "p3", "medium", "warning", "warn":
		return "medium"
	case "sev4", "p4", "low", "info", "informational":
		return "low"
	default:
		return value
	}
}

func fallback(value string, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}
