package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_RequiredToken(t *testing.T) {
	yaml := `bot:
  token: ""
rss:
  feeds:
    - "https://example.com/feed.xml"
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(yaml), 0644)
	require.NoError(t, err)

	_, err = Load(cfgPath)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "bot.token is required")
}

func TestLoad_ValidConfig(t *testing.T) {
	yaml := `bot:
  token: "12345:ABC-DEF"
  parse_mode: "HTML"
rss:
  feeds:
    - "https://example.com/feed.xml"
llm:
  endpoint: "http://127.0.0.1:8080/v1/chat/completions"
  context_window: 4096
schedule:
  cron: "0 9 * * *"
app:
  db_path: "./data/bot.db"
  log_level: "info"
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(yaml), 0644)
	require.NoError(t, err)

	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, "12345:ABC-DEF", cfg.Bot.Token)
	assert.Equal(t, "HTML", cfg.Bot.ParseMode)
	assert.Len(t, cfg.RSS.Feeds, 1)
	assert.Equal(t, 4096, cfg.LLM.ContextWindow)
	assert.Equal(t, "0 9 * * *", cfg.Schedule.Cron)
}

func TestLoad_Defaults(t *testing.T) {
	yaml := `bot:
  token: "test-token"
rss:
  feeds:
    - "https://example.com/feed.xml"
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(yaml), 0644)
	require.NoError(t, err)

	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, 50, cfg.RSS.MaxItemsPerFeed)
	assert.Equal(t, 15*time.Second, cfg.RSS.FetchTimeout)
	assert.Equal(t, 24*time.Hour, cfg.RSS.CacheTTL)
	assert.Equal(t, 8192, cfg.LLM.ContextWindow)
	assert.Equal(t, 2000, cfg.LLM.MaxTokens)
	assert.Equal(t, 0.3, cfg.LLM.Temperature)
	assert.Equal(t, 60*time.Second, cfg.LLM.Timeout)
	assert.Equal(t, "0 9 * * *", cfg.Schedule.Cron)
	assert.Equal(t, "Europe/Moscow", cfg.Schedule.Timezone)
	assert.Equal(t, 3, cfg.App.RetryMax)
	assert.Equal(t, 2*time.Second, cfg.App.RetryBackoff)
}

func TestLoad_EnvOverride(t *testing.T) {
	yaml := `bot:
  token: "yaml-token"
rss:
  feeds:
    - "https://example.com/feed.xml"
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(yaml), 0644)
	require.NoError(t, err)

	err = os.Setenv("TG_NEWS_BOT_TOKEN", "env-token")
	require.NoError(t, err)
	defer os.Unsetenv("TG_NEWS_BOT_TOKEN")

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, "env-token", cfg.Bot.Token)
}

func TestValidate_ParseMode(t *testing.T) {
	yaml := `bot:
  token: "test"
  parse_mode: "InvalidMode"
rss:
  feeds: ["https://example.com"]
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(yaml), 0644)
	require.NoError(t, err)

	_, err = Load(cfgPath)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse_mode")
}

func TestValidate_MissingCron(t *testing.T) {
	yaml := `bot:
  token: "test"
rss:
  feeds: ["https://example.com"]
schedule:
  cron: "invalid cron expression"
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	err := os.WriteFile(cfgPath, []byte(yaml), 0644)
	require.NoError(t, err)

	_, err = Load(cfgPath)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "schedule.cron")
}
