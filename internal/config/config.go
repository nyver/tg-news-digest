package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds the entire application configuration.
type Config struct {
	Bot      BotConfig      `mapstructure:"bot"`
	RSS      RSSConfig      `mapstructure:"rss"`
	LLM      LLMConfig      `mapstructure:"llm"`
	Schedule ScheduleConfig `mapstructure:"schedule"`
	App      AppConfig      `mapstructure:"app"`
}

type MTProxyConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Host    string `mapstructure:"host"`
	Port    int    `mapstructure:"port"`
	Secret  string `mapstructure:"secret"` // Base64-encoded proxy secret (optional)
}

type BotConfig struct {
	Token       string        `mapstructure:"token"`
	ParseMode   string        `mapstructure:"parse_mode"` // HTML or MarkdownV2
	OwnerChatID int64         `mapstructure:"owner_chat_id"`
	MTProxy     MTProxyConfig `mapstructure:"mtproxy"`
}

type RSSConfig struct {
	Feeds           []string      `mapstructure:"feeds"`
	MaxItemsPerFeed int           `mapstructure:"max_items_per_feed"`
	FetchTimeout    time.Duration `mapstructure:"fetch_timeout"`
	CacheTTL        time.Duration `mapstructure:"cache_ttl"`
}

type LLMConfig struct {
	Provider      string        `mapstructure:"provider"`
	Endpoint      string        `mapstructure:"endpoint"`
	Model         string        `mapstructure:"model"`
	APIKey        string        `mapstructure:"api_key"`
	Temperature   float64       `mapstructure:"temperature"`
	MaxTokens     int           `mapstructure:"max_tokens"`
	ContextWindow int           `mapstructure:"context_window"`
	Timeout       time.Duration `mapstructure:"timeout"`
}

type ScheduleConfig struct {
	Cron     string `mapstructure:"cron"`
	Timezone string `mapstructure:"timezone"`
}

type AppConfig struct {
	DBPath        string        `mapstructure:"db_path"`
	LogLevel      string        `mapstructure:"log_level"`
	RetryMax      int           `mapstructure:"retry_max"`
	RetryBackoff  time.Duration `mapstructure:"retry_backoff"`
	DigestLogPath string        `mapstructure:"digest_log_path"`
	HealthPort    int           `mapstructure:"health_port"`
}

// Load reads configuration from file and/or environment variables.
// ENV vars override YAML values. Env var names use TG_NEWS_ prefix.
func Load(path string) (*Config, error) {
	v := viper.New()

	// Config file
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("./configs")
		v.AddConfigPath("./configs/")
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("config: read config file: %w", err)
		}
	}

	// Environment variable overrides
	v.SetEnvPrefix("TG_NEWS")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("config: validation: %w", err)
	}

	return &cfg, nil
}

// Validate checks required fields and reasonable defaults.
func Validate(cfg *Config) error {
	if cfg.Bot.Token == "" {
		return fmt.Errorf("bot.token is required")
	}
	if cfg.RSS.MaxItemsPerFeed <= 0 {
		cfg.RSS.MaxItemsPerFeed = 50
	}
	if cfg.RSS.FetchTimeout <= 0 {
		cfg.RSS.FetchTimeout = 15 * time.Second
	}
	if cfg.RSS.CacheTTL <= 0 {
		cfg.RSS.CacheTTL = 24 * time.Hour
	}
	if cfg.LLM.ContextWindow <= 0 {
		cfg.LLM.ContextWindow = 8192
	}
	if cfg.LLM.MaxTokens <= 0 {
		cfg.LLM.MaxTokens = 2000
	}
	if cfg.LLM.Timeout <= 0 {
		cfg.LLM.Timeout = 60 * time.Second
	}
	if cfg.LLM.Temperature <= 0 || cfg.LLM.Temperature > 2 {
		cfg.LLM.Temperature = 0.3
	}
	if cfg.LLM.Provider == "" {
		cfg.LLM.Provider = "llama-cpp"
	}

	// Validate provider
	switch cfg.LLM.Provider {
	case "llama-cpp", "openrouter":
		// llama-cpp: optional API key (may not be needed for local endpoints)
		// openrouter: api_key is required
	default:
		return fmt.Errorf("llm.provider must be 'llama-cpp' or 'openrouter', got %q", cfg.LLM.Provider)
	}
	if cfg.LLM.Provider == "openrouter" && cfg.LLM.APIKey == "" {
		return fmt.Errorf("llm.api_key is required for openrouter provider")
	}
	if cfg.App.LogLevel == "" {
		cfg.App.LogLevel = "info"
	}
	if cfg.App.RetryMax <= 0 {
		cfg.App.RetryMax = 3
	}
	if cfg.App.RetryBackoff <= 0 {
		cfg.App.RetryBackoff = 2 * time.Second
	}
	if cfg.App.HealthPort <= 0 {
		cfg.App.HealthPort = 9100
	}
	if cfg.Schedule.Cron == "" {
		cfg.Schedule.Cron = "0 9 * * *"
	}
	if cfg.Schedule.Timezone == "" {
		cfg.Schedule.Timezone = "Europe/Moscow"
	}

	// Validate parse_mode
	switch cfg.Bot.ParseMode {
	case "HTML", "MarkdownV2", "":
		if cfg.Bot.ParseMode == "" {
			cfg.Bot.ParseMode = "HTML"
		}
	default:
		return fmt.Errorf("bot.parse_mode must be HTML or MarkdownV2, got %q", cfg.Bot.ParseMode)
	}

	// Validate log level
	switch cfg.App.LogLevel {
	case "debug", "info", "warn", "error", "":
		if cfg.App.LogLevel == "" {
			cfg.App.LogLevel = "info"
		}
	default:
		return fmt.Errorf("app.log_level must be debug, info, warn, or error, got %q", cfg.App.LogLevel)
	}

	// Validate cron expression
	if _, err := parseCron(cfg.Schedule.Cron); err != nil {
		return fmt.Errorf("schedule.cron: %w", err)
	}

	return nil
}

// parseCron validates the cron expression.
func parseCron(expr string) (string, error) {
	// Simple validation: count fields
	parts := splitCron(expr)
	if len(parts) != 5 {
		return "", fmt.Errorf("expected 5 fields, got %d", len(parts))
	}
	return expr, nil
}

func splitCron(s string) []string {
	var parts []string
	var current string
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		} else {
			current += string(r)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

// MustLoad panics if config cannot be loaded. Used in cmd/bot/main.go.
func MustLoad() *Config {
	path := os.Getenv("TG_NEWS_CONFIG")
	cfg, err := Load(path)
	if err != nil {
		panic(fmt.Sprintf("config: failed to load: %v", err))
	}
	return cfg
}
