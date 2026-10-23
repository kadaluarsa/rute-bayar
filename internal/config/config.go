package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDBPath       = "./rute-bayar.sqlite3"
	defaultWebhookAddr  = ":8080"
	defaultAPIRateLimit = 120
)

type Config struct {
	Environment       string
	DBPath            string
	WebhookAddr       string
	APIKey            string
	APIAllowedOrigins string
	APIRateLimit      int

	// Wishpull routing plane (PRD/TRD v0.3).
	AgentAPIKey         string
	ExecutorAPIKey      string
	RequireMandate      bool
	RoutingTokenKeys    string
	RoutingTokenTTL     time.Duration
	RoutingConfigPath   string
	EventsURL           string
	EventsSecret        string
	MetricsToken        string
	ShutdownGracePeriod time.Duration
}

func Load() Config {
	fileEnv := readDotEnv(".env")
	return Config{
		Environment:       envOrDefault(fileEnv, "RUTE_BAYAR_ENV", "sandbox"),
		DBPath:            envOrDefault(fileEnv, "RUTE_BAYAR_DB_PATH", defaultDBPath),
		WebhookAddr:       envOrDefault(fileEnv, "RUTE_BAYAR_WEBHOOK_ADDR", defaultWebhookAddr),
		APIKey:            envOrDefault(fileEnv, "RUTE_BAYAR_API_KEY", ""),
		APIAllowedOrigins: envOrDefault(fileEnv, "RUTE_BAYAR_API_ALLOWED_ORIGINS", "*"),
		APIRateLimit:      envIntOrDefault(fileEnv, "RUTE_BAYAR_API_RATE_LIMIT_PER_MIN", defaultAPIRateLimit),

		AgentAPIKey:         envOrDefault(fileEnv, "RUTE_BAYAR_AGENT_API_KEY", ""),
		ExecutorAPIKey:      envOrDefault(fileEnv, "RUTE_BAYAR_EXECUTOR_API_KEY", ""),
		RequireMandate:      envBoolOrDefault(fileEnv, "RUTE_BAYAR_REQUIRE_MANDATE", false),
		RoutingTokenKeys:    envOrDefault(fileEnv, "RUTE_BAYAR_ROUTING_TOKEN_KEYS", ""),
		RoutingTokenTTL:     envDurationOrDefault(fileEnv, "RUTE_BAYAR_ROUTING_TOKEN_TTL", 15*time.Minute),
		RoutingConfigPath:   envOrDefault(fileEnv, "RUTE_BAYAR_ROUTING_CONFIG", ""),
		EventsURL:           envOrDefault(fileEnv, "RUTE_BAYAR_WISHPULL_EVENTS_URL", ""),
		EventsSecret:        envOrDefault(fileEnv, "RUTE_BAYAR_WISHPULL_EVENTS_SECRET", ""),
		MetricsToken:        envOrDefault(fileEnv, "RUTE_BAYAR_METRICS_TOKEN", ""),
		ShutdownGracePeriod: envDurationOrDefault(fileEnv, "RUTE_BAYAR_SHUTDOWN_GRACE", 20*time.Second),
	}
}

func envBoolOrDefault(fileEnv map[string]string, key string, fallback bool) bool {
	raw := strings.TrimSpace(envOrDefault(fileEnv, key, ""))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDurationOrDefault(fileEnv map[string]string, key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(envOrDefault(fileEnv, key, ""))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envOrDefault(fileEnv map[string]string, key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		value = fileEnv[key]
	}
	if value == "" {
		value = fallback
	}
	return value
}

func envIntOrDefault(fileEnv map[string]string, key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		raw = fileEnv[key]
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(trimmed)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func readDotEnv(path string) map[string]string {
	content, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}

	values := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key != "" {
			values[key] = value
		}
	}

	return values
}
