// Package config loads and validates application configuration from environment
// variables. All required fields are validated at startup so the binary fails
// fast rather than at request time.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all runtime configuration for the API server.
type Config struct {
	Port               string
	DBURL              string
	RedisAddr          string
	RedisDB            string
	TicketTemplatePath string
	CORSAllowedOrigins string
	APIAuthToken       string
	RateLimit          RateLimitConfig
	JIRA               JIRAConfig
	Teams              TeamsConfig
	Dynatrace          SourceConfig
	ELK                SourceConfig
	Webhooks           WebhookConfig
}

// JIRAConfig holds the connection parameters for the Jira integration.
// Transition IDs map IncidentIQ status changes (open/acknowledged/resolved) to
// the corresponding Jira workflow transitions.
type JIRAConfig struct {
	Enabled                 bool
	BaseURL                 string
	Username                string
	APIToken                string
	ProjectKey              string
	AcknowledgeTransitionID string
	ResolveTransitionID     string
	ReopenTransitionID      string
	ResolutionField         string
	ResolutionValue         string
}

// SourceConfig toggles an optional inbound alert source (e.g. Dynatrace, ELK).
type SourceConfig struct {
	Enabled bool
}

// TeamsConfig holds OAuth2 credentials for the Microsoft Teams integration.
// TokenEncryptionKey is used to AES-GCM encrypt OAuth tokens stored in the DB.
type TeamsConfig struct {
	Enabled            bool
	TenantID           string
	ClientID           string
	ClientSecret       string
	TokenEncryptionKey string
}

// WebhookConfig holds per-source HMAC shared secrets used to verify inbound
// webhook payloads. An empty secret disables signature verification for that
// source.
type WebhookConfig struct {
	DynatraceSecret  string
	ELKSecret        string
	GenericSecret    string
	DeploymentSecret string
}

type WebhookSecrets = WebhookConfig

// RateLimitConfig controls per-client request caps enforced via Redis sliding
// windows. Zero disables rate limiting for that scope.
type RateLimitConfig struct {
	APIRequestsPerMinute     int
	WebhookRequestsPerMinute int
}

// Load reads configuration from environment variables and validates required
// fields. It returns an error if any mandatory value is missing or if an
// integration is enabled without its required fields.
func Load() (Config, error) {
	cfg := Config{
		Port:               getEnv("PORT", "8080"),
		DBURL:              os.Getenv("DATABASE_URL"),
		RedisAddr:          getEnv("REDIS_ADDR", "localhost:6379"),
		RedisDB:            getEnv("REDIS_DB", "0"),
		TicketTemplatePath: os.Getenv("TICKET_TEMPLATE_PATH"),
		CORSAllowedOrigins: getEnv("CORS_ALLOWED_ORIGINS", "http://localhost:5173,http://127.0.0.1:5173"),
		APIAuthToken:       os.Getenv("API_AUTH_TOKEN"),
		RateLimit: RateLimitConfig{
			APIRequestsPerMinute:     getIntEnv("RATE_LIMIT_API_PER_MINUTE", 600),
			WebhookRequestsPerMinute: getIntEnv("RATE_LIMIT_WEBHOOK_PER_MINUTE", 120),
		},
		JIRA: JIRAConfig{
			Enabled:                 getBoolEnv("JIRA_ENABLED", false),
			BaseURL:                 os.Getenv("JIRA_BASE_URL"),
			Username:                os.Getenv("JIRA_USERNAME"),
			APIToken:                os.Getenv("JIRA_API_TOKEN"),
			ProjectKey:              os.Getenv("JIRA_PROJECT_KEY"),
			AcknowledgeTransitionID: os.Getenv("JIRA_ACK_TRANSITION_ID"),
			ResolveTransitionID:     os.Getenv("JIRA_RESOLVE_TRANSITION_ID"),
			ReopenTransitionID:      os.Getenv("JIRA_REOPEN_TRANSITION_ID"),
			ResolutionField:         getEnv("JIRA_RESOLUTION_FIELD", "resolution"),
			ResolutionValue:         getEnv("JIRA_RESOLUTION_VALUE", "Done"),
		},
		Teams: TeamsConfig{
			Enabled:            getBoolEnv("TEAMS_ENABLED", false),
			TenantID:           os.Getenv("TEAMS_TENANT_ID"),
			ClientID:           os.Getenv("TEAMS_CLIENT_ID"),
			ClientSecret:       os.Getenv("TEAMS_CLIENT_SECRET"),
			TokenEncryptionKey: os.Getenv("TEAMS_TOKEN_ENCRYPTION_KEY"),
		},
		Dynatrace: SourceConfig{
			Enabled: getBoolEnv("DYNATRACE_ENABLED", true),
		},
		ELK: SourceConfig{
			Enabled: getBoolEnv("ELK_ENABLED", true),
		},
		Webhooks: WebhookConfig{
			DynatraceSecret:  os.Getenv("DYNATRACE_WEBHOOK_SECRET"),
			ELKSecret:        os.Getenv("ELK_WEBHOOK_SECRET"),
			GenericSecret:    os.Getenv("GENERIC_WEBHOOK_SECRET"),
			DeploymentSecret: os.Getenv("DEPLOYMENT_WEBHOOK_SECRET"),
		},
	}

	if cfg.DBURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.JIRA.Enabled {
		if cfg.JIRA.BaseURL == "" {
			return Config{}, fmt.Errorf("JIRA_BASE_URL is required when JIRA_ENABLED=true")
		}
		if cfg.JIRA.Username == "" {
			return Config{}, fmt.Errorf("JIRA_USERNAME is required when JIRA_ENABLED=true")
		}
		if cfg.JIRA.APIToken == "" {
			return Config{}, fmt.Errorf("JIRA_API_TOKEN is required when JIRA_ENABLED=true")
		}
		if cfg.JIRA.ProjectKey == "" {
			return Config{}, fmt.Errorf("JIRA_PROJECT_KEY is required when JIRA_ENABLED=true")
		}
	}
	if cfg.Teams.Enabled {
		if cfg.Teams.TenantID == "" {
			return Config{}, fmt.Errorf("TEAMS_TENANT_ID is required when TEAMS_ENABLED=true")
		}
		if cfg.Teams.ClientID == "" {
			return Config{}, fmt.Errorf("TEAMS_CLIENT_ID is required when TEAMS_ENABLED=true")
		}
		if cfg.Teams.ClientSecret == "" {
			return Config{}, fmt.Errorf("TEAMS_CLIENT_SECRET is required when TEAMS_ENABLED=true")
		}
		if cfg.Teams.TokenEncryptionKey == "" {
			return Config{}, fmt.Errorf("TEAMS_TOKEN_ENCRYPTION_KEY is required when TEAMS_ENABLED=true")
		}
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func getBoolEnv(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "TRUE" || v == "yes" || v == "YES"
}

func getIntEnv(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}
