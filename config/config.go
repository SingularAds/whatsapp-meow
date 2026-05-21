package config

import (
	"os"
	"strconv"
)

// Config holds all runtime configuration loaded from environment.
type Config struct {
	Port            string
	BridgeUsername  string
	BridgePassword  string
	DBDir           string
	WebhookURL      string
	WebhookSecret   string
	LogLevel        string
	WhatsAppOpTimeoutSeconds int
	MediaTTLSeconds int
	// PublicURL is the externally reachable base URL of this bridge,
	// used to construct inbound media_url values sent in webhooks.
	PublicURL string
	// DefaultSessionID is the session that is always ensured at startup
	// (the global onboarding number, e.g. "smba").
	DefaultSessionID string

	// PostHog analytics configuration.
	// PostHogAPIKey is the project API key from the PostHog dashboard.
	// Leave empty to disable PostHog tracking entirely (no-op).
	PostHogAPIKey string
	// PostHogHost overrides the default PostHog ingest host.
	// Useful for self-hosted PostHog deployments. Leave empty for cloud.
	PostHogHost string
}

// Load reads configuration from environment variables, applying sensible defaults.
func Load() Config {
	port := getEnv("PORT", "3020")
	return Config{
		Port:             port,
		BridgeUsername:   getEnv("BRIDGE_USERNAME", "admin"),
		BridgePassword:   getEnv("BRIDGE_PASSWORD", "changeme"),
		DBDir:            getEnv("DB_DIR", "./data"),
		WebhookURL:       getEnv("WEBHOOK_URL", "http://localhost:8000"),
		WebhookSecret:    getEnv("WEBHOOK_SECRET", ""),
		LogLevel:         getEnv("LOG_LEVEL", "INFO"),
		WhatsAppOpTimeoutSeconds: getEnvInt("WHATSAPP_OP_TIMEOUT_SECONDS", 120),
		MediaTTLSeconds:  getEnvInt("MEDIA_TTL_SECONDS", 3600),
		PublicURL:        getEnv("PUBLIC_URL", "http://localhost:"+port),
		DefaultSessionID: getEnv("DEFAULT_SESSION_ID", "smba"),
		PostHogAPIKey:    getEnv("POSTHOG_API_KEY", ""),
		PostHogHost:      getEnv("POSTHOG_HOST", ""),
	}
}

func getEnv(key, defaultVal string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}
