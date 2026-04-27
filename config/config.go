package config

import (
	"fmt"

	"os"

	"strconv"

	"strings"

	"github.com/joho/godotenv"
)

// Config centralizes all runtime settings. Required env vars cause Load() to

// fail-fast at startup so the bot never silently runs with missing secrets.

type Config struct {

	// Slack

	SlackUserToken string

	SlackSigningSecret string

	SlackBotUserID string // empty = auto-detect via auth.test on startup

	// OpenRouter — single API key serves judge + all expert models.

	// Langfuse traces are configured in the OpenRouter dashboard, NOT here —

	// the bot just attaches metadata to each request.

	OpenRouterAPIKey string

	OpenRouterURL string

	// JudgeModel: smallest free model — used for prompt classification AND

	// conversation summarization. Gemma 4 26B has 4B active params (MoE),

	// fast for structured JSON output and short summaries.

	JudgeModel string

	// Memory (SQLite)

	DBPath string

	MemoryMaxTurns int

	MemorySummarizeAt int

	MemorySummarizeFold int

	// Config file paths

	RoutingConfigPath string

	MCPConfigPath string

	// Temporal

	TemporalHostPort string

	TemporalNamespace string

	TemporalTaskQueue string

	// TriggerKeywords, when non-empty, limits MessageEvent responses to messages
	// that contain at least one keyword (case-insensitive). AppMentions always
	// respond regardless. Empty = respond to every message (original behavior).

	TriggerKeywords []string

	// HTTP

	ServerPort string

	// Sentry — for non-LLM errors only (Slack/Temporal/SQLite/MCP failures).

	// LLM observability lives in Langfuse via OpenRouter. Empty DSN = disabled.

	SentryDSN string

	SentryEnvironment string

	SentryRelease string

	SentryTracesSampleRate float64
}

func Load() (*Config, error) {

	_ = godotenv.Load() // .env is optional in production

	cfg := &Config{

		SlackUserToken: os.Getenv("SLACK_USER_TOKEN"),

		SlackSigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),

		SlackBotUserID: os.Getenv("SLACK_USER_ID"),

		OpenRouterAPIKey: os.Getenv("OPENROUTER_API_KEY"),

		OpenRouterURL: envOr("OPENROUTER_URL", "https://openrouter.ai/api/v1"),

		JudgeModel: envOr("JUDGE_MODEL", "openai/gpt-oss-20b:free"),

		DBPath: envOr("DB_PATH", "./data/bot.db"),

		MemoryMaxTurns: envInt("MEMORY_MAX_TURNS", 20),

		MemorySummarizeAt: envInt("MEMORY_SUMMARIZE_AT", 30),

		MemorySummarizeFold: envInt("MEMORY_SUMMARIZE_FOLD", 10),

		RoutingConfigPath: envOr("ROUTING_CONFIG", "./config/routing.yaml"),

		MCPConfigPath: envOr("MCP_CONFIG", "./config/mcp.yaml"),

		TemporalHostPort: envOr("TEMPORAL_HOST_PORT", "localhost:7233"),

		TemporalNamespace: envOr("TEMPORAL_NAMESPACE", "default"),

		TemporalTaskQueue: envOr("TEMPORAL_TASK_QUEUE", "Agent-Go-Go-queue"),

		ServerPort: envOr("SERVER_PORT", "8080"),

		SentryDSN: os.Getenv("SENTRY_DSN"),

		SentryEnvironment: envOr("SENTRY_ENVIRONMENT", "development"),

		SentryRelease: envOr("SENTRY_RELEASE", "dev"),

		SentryTracesSampleRate: envFloat("SENTRY_TRACES_SAMPLE_RATE", 0.2),

		TriggerKeywords: parseKeywords(os.Getenv("TRIGGER_KEYWORDS")),
	}

	var missing []string

	if cfg.SlackUserToken == "" {

		missing = append(missing, "SLACK_USER_TOKEN")

	}

	if cfg.SlackSigningSecret == "" {

		missing = append(missing, "SLACK_SIGNING_SECRET")

	}

	if cfg.OpenRouterAPIKey == "" {

		missing = append(missing, "OPENROUTER_API_KEY")

	}

	if len(missing) > 0 {

		return nil, fmt.Errorf("missing required env vars: %v", missing)

	}

	return cfg, nil

}

func envOr(key, fallback string) string {

	if v := os.Getenv(key); v != "" {

		return v

	}

	return fallback

}

func envInt(key string, fallback int) int {

	if v := os.Getenv(key); v != "" {

		if n, err := strconv.Atoi(v); err == nil {

			return n

		}

	}

	return fallback

}

func envFloat(key string, fallback float64) float64 {

	if v := os.Getenv(key); v != "" {

		if f, err := strconv.ParseFloat(v, 64); err == nil {

			return f

		}

	}

	return fallback

}

// parseKeywords splits a comma-separated string into a lowercased slice,
// dropping empty entries. Returns nil when the input is blank.

func parseKeywords(raw string) []string {

	raw = strings.TrimSpace(raw)

	if raw == "" {

		return nil

	}

	parts := strings.Split(raw, ",")

	out := make([]string, 0, len(parts))

	for _, p := range parts {

		if kw := strings.ToLower(strings.TrimSpace(p)); kw != "" {

			out = append(out, kw)

		}

	}

	return out

}
