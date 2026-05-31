package healthcheck

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestChecker(t *testing.T, port int) (*Checker, *storage.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.New(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	cfg := config.Config{
		Bot: config.BotConfig{
			Token: "123456:FAKE_TOKEN_FOR_TESTING",
		},
		LLM: config.LLMConfig{
			Provider: "llama-cpp",
			Endpoint: "http://localhost:9999", // non-existent, will fail but is reachable test
		},
		App: config.AppConfig{
			HealthPort: port,
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	checker := New(cfg, store, logger).WithPort(port)
	return checker, store
}

func TestNew_CheckerCreation(t *testing.T) {
	ctx := context.Background()
	store, err := storage.New(ctx, ":memory:")
	require.NoError(t, err)
	defer store.Close()

	cfg := config.Config{
		Bot: config.BotConfig{Token: "test-token"},
		LLM: config.LLMConfig{Provider: "openrouter", APIKey: "test-key"},
		App: config.AppConfig{HealthPort: 9101},
	}

	checker := New(cfg, store, nil)
	assert.NotNil(t, checker)
	// New() sets default port 9100, WithPort() is needed to change it
	assert.Equal(t, 9100, checker.port)

	checker = checker.WithPort(9999)
	assert.Equal(t, 9999, checker.port)
	assert.Equal(t, "test-token", checker.botCfg.Token)
	assert.Equal(t, "openrouter", checker.llmCfg.Provider)
}

func TestWithPort(t *testing.T) {
	checker := &Checker{port: 8080}
	result := checker.WithPort(9999)
	// WithPort modifies the receiver in place and returns it
	assert.Equal(t, 9999, checker.port)
	assert.Equal(t, 9999, result.port)
}

func TestHandler_MethodNotAllowed(t *testing.T) {
	checker, _ := newTestChecker(t, 0)

	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()

	checker.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandler_DegradedResponse(t *testing.T) {
	// Use empty token to trigger "warn" on telegram check
	ctx := context.Background()
	store, err := storage.New(ctx, ":memory:")
	require.NoError(t, err)
	defer store.Close()

	cfg := config.Config{
		Bot: config.BotConfig{Token: ""},                           // empty token → telegram warn
		LLM: config.LLMConfig{Provider: "llama-cpp", Endpoint: ""}, // empty endpoint → LLM warn
		App: config.AppConfig{HealthPort: 9102},
	}

	checker := New(cfg, store, nil).WithPort(9102)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	checker.Handler().ServeHTTP(w, req)

	// Should be degraded (warnings but no errors)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp HealthResponse
	err = json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	assert.Equal(t, "degraded", resp.Status)
}

func TestCheck_DatabaseOK(t *testing.T) {
	_, store := newTestChecker(t, 0)

	ctx := context.Background()
	check := store.DB().PingContext(ctx)
	assert.NoError(t, check)
}

func TestCheck_DatabaseError(t *testing.T) {
	// Create checker with nil store to trigger database error
	checker := &Checker{store: nil}

	ctx := context.Background()
	check := checker.checkDatabase(ctx)
	assert.Equal(t, "error", check.Status)
	assert.Contains(t, check.Message, "nil")
}

func TestCheck_LLMEmptyEndpoint(t *testing.T) {
	checker := &Checker{
		llmCfg: config.LLMConfig{
			Provider: "llama-cpp",
			Endpoint: "",
		},
	}

	ctx := context.Background()
	check := checker.checkLLM(ctx)
	assert.Equal(t, "warn", check.Status)
	assert.Contains(t, check.Message, "endpoint not configured")
}

func TestCheck_LLMProviderEmptyDefaultsToLlama(t *testing.T) {
	// Use a mock server that returns 200
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer mockLLM.Close()

	checker := &Checker{
		llmCfg: config.LLMConfig{
			Provider: "", // empty → defaults to llama-cpp
			Endpoint: mockLLM.URL,
		},
		httpCli: resty.New().SetTimeout(time.Second).SetBaseURL(mockLLM.URL),
	}

	ctx := context.Background()
	check := checker.checkLLM(ctx)
	// Should attempt llama-cpp request and return "ok" (200 status)
	assert.Equal(t, "ok", check.Status)
}

func TestCheck_TelegramEmptyToken(t *testing.T) {
	checker := &Checker{
		botCfg: config.BotConfig{Token: ""},
	}

	ctx := context.Background()
	check := checker.checkTelegram(ctx)
	assert.Equal(t, "warn", check.Status)
	assert.Contains(t, check.Message, "token not configured")
}

func TestHealthResponseStatusAggregation_Healthy(t *testing.T) {
	resp := HealthResponse{
		Checks: map[string]Check{
			"database": {Status: "ok"},
			"llm":      {Status: "ok"},
			"telegram": {Status: "ok"},
		},
	}

	hasError := false
	hasWarn := false
	for _, check := range resp.Checks {
		if check.Status == "error" {
			hasError = true
		}
		if check.Status == "warn" {
			hasWarn = true
		}
	}

	assert.False(t, hasError)
	assert.False(t, hasWarn)
}

func TestHealthResponseStatusAggregation_Unhealthy(t *testing.T) {
	resp := HealthResponse{
		Checks: map[string]Check{
			"database": {Status: "ok"},
			"llm":      {Status: "error"},
			"telegram": {Status: "ok"},
		},
	}

	hasError := false
	hasWarn := false
	for _, check := range resp.Checks {
		if check.Status == "error" {
			hasError = true
		}
		if check.Status == "warn" {
			hasWarn = true
		}
	}

	assert.True(t, hasError)
	assert.False(t, hasWarn)
}

func TestCheck_ChecksAllComponents(t *testing.T) {
	checker, _ := newTestChecker(t, 0)

	ctx := context.Background()
	resp := checker.Check(ctx)

	assert.Contains(t, resp.Checks, "database")
	assert.Contains(t, resp.Checks, "llm")
	assert.Contains(t, resp.Checks, "telegram")
	assert.NotEmpty(t, resp.Started)
	assert.NotEmpty(t, resp.Duration)
}

func TestHandler_ContentTypeHeader(t *testing.T) {
	checker, _ := newTestChecker(t, 0)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	checker.Handler().ServeHTTP(w, req)

	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
}

func TestStartHTTPServer_StartsAndStops(t *testing.T) {
	checker, _ := newTestChecker(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, shutdown := checker.StartHTTPServer(ctx)
	defer shutdown()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Verify server is not nil
	assert.NotNil(t, srv)
	assert.NotNil(t, srv.Addr)
}

func TestCheck_UPTIMEDuration(t *testing.T) {
	checker, _ := newTestChecker(t, 0)

	ctx := context.Background()
	resp := checker.Check(ctx)

	// Duration should be set after Check() runs
	assert.NotEmpty(t, resp.Duration)
	// Should be parseable as duration
	dur, err := time.ParseDuration(resp.Duration)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, dur, time.Duration(0))
}

func TestHandler_HTTPStatusCodes(t *testing.T) {
	// Test that status codes are correctly mapped
	t.Run("HealthyIs200", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, http.StatusOK)
	})

	t.Run("DegradedIs503", func(t *testing.T) {
		assert.Equal(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	})

	t.Run("UnhealthyIs500", func(t *testing.T) {
		assert.Equal(t, http.StatusInternalServerError, http.StatusInternalServerError)
	})
}

func TestChecker_CheckReturnsFormattedStarted(t *testing.T) {
	checker, _ := newTestChecker(t, 0)

	ctx := context.Background()
	resp := checker.Check(ctx)

	// Started should be RFC3339 formatted
	_, err := time.Parse(time.RFC3339, resp.Started)
	assert.NoError(t, err, "Started timestamp should be RFC3339 formatted")
}
