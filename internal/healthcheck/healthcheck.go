// Package healthcheck provides a health HTTP endpoint for monitoring
// database, LLM, and Telegram Bot API connectivity.
package healthcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/storage"
)

const openrouterName = "tg-news-digest-bot"

// Check represents the health status of a single component.
type Check struct {
	Status   string `json:"status"` // "ok", "warn", "error"
	Duration string `json:"duration,omitempty"`
	Message  string `json:"message,omitempty"`
}

// HealthResponse is the full health check payload.
type HealthResponse struct {
	Status   string           `json:"status"` // "healthy", "degraded", "unhealthy"
	uptime   time.Duration    `json:"-"`
	Started  string           `json:"started_at"`
	Duration string           `json:"duration"`
	Checks   map[string]Check `json:"checks"`
}

// Checker validates the health of all service components.
type Checker struct {
	store   *storage.Store
	llmCfg  config.LLMConfig
	botCfg  config.BotConfig
	httpCli *resty.Client
	logger  *slog.Logger
	started time.Time
	port    int
}

// New creates a health checker with the given configuration.
func New(cfg config.Config, store *storage.Store, logger *slog.Logger) *Checker {
	return &Checker{
		store:   store,
		llmCfg:  cfg.LLM,
		botCfg:  cfg.Bot,
		httpCli: resty.New().SetTimeout(10 * time.Second),
		logger:  logger,
		started: time.Now(),
		port:    9100, // default health check port
	}
}

// WithPort sets a custom port for the health endpoint.
func (hc *Checker) WithPort(port int) *Checker {
	hc.port = port
	return hc
}

// Handler returns an http.HandlerFunc for the /health endpoint.
func (hc *Checker) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		resp := hc.Check(ctx)

		w.Header().Set("Content-Type", "application/json")
		if resp.Status == "healthy" {
			w.WriteHeader(http.StatusOK)
		} else if resp.Status == "degraded" {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}

		if err := json.NewEncoder(w).Encode(resp); err != nil {
			hc.logger.Error("healthcheck: encode response", slog.String("error", err.Error()))
		}
	}
}

// Check runs all health checks and returns the aggregated result.
func (hc *Checker) Check(ctx context.Context) HealthResponse {
	resp := HealthResponse{
		Started: hc.started.Format(time.RFC3339),
		Checks:  make(map[string]Check),
	}

	// Check database
	resp.Checks["database"] = hc.checkDatabase(ctx)

	// Check LLM
	resp.Checks["llm"] = hc.checkLLM(ctx)

	// Check Telegram
	resp.Checks["telegram"] = hc.checkTelegram(ctx)

	// Aggregate status
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

	if hasError {
		resp.Status = "unhealthy"
	} else if hasWarn {
		resp.Status = "degraded"
	} else {
		resp.Status = "healthy"
	}

	resp.Duration = time.Since(hc.started).String()
	return resp
}

func (hc *Checker) checkDatabase(ctx context.Context) Check {
	start := time.Now()
	err := hc.store.DB().PingContext(ctx)
	dur := time.Since(start)

	c := Check{Duration: dur.String()}
	if err != nil {
		c.Status = "error"
		c.Message = fmt.Sprintf("ping failed: %v", err)
		hc.logger.Warn("healthcheck: database unhealthy", slog.String("error", err.Error()))
	} else {
		c.Status = "ok"
	}
	return c
}

func (hc *Checker) checkLLM(ctx context.Context) Check {
	start := time.Now()

	// Determine provider and endpoint
	provider := hc.llmCfg.Provider
	if provider == "" {
		provider = "llama-cpp"
	}

	var reqBody map[string]interface{}
	var headers map[string]string

	switch provider {
	case "openrouter":
		headers = map[string]string{
			"Authorization": "Bearer " + hc.llmCfg.APIKey,
			"HTTP-Referer":  openrouterName,
			"Content-Type":  "application/json",
		}
		reqBody = map[string]interface{}{
			"model":      "auto",
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
			"max_tokens": 5,
			"stream":     false,
		}
	default:
		if hc.llmCfg.Endpoint == "" {
			return Check{
				Status:   "warn",
				Message:  "LLM endpoint not configured",
				Duration: time.Since(start).String(),
			}
		}
		reqBody = map[string]interface{}{
			"model":      "auto",
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
			"max_tokens": 5,
			"stream":     false,
		}
	}

	httpCli := hc.httpCli
	if provider == "openrouter" {
		httpCli = resty.New().
			SetBaseURL("https://openrouter.ai/api/v1").
			SetHeaders(headers).
			SetTimeout(10 * time.Second)
	}

	resp, err := httpCli.R().
		SetContext(ctx).
		SetBody(reqBody).
		Post("/chat/completions")

	c := Check{Duration: time.Since(start).String()}
	if err != nil {
		c.Status = "error"
		c.Message = fmt.Sprintf("request failed: %v", err)
		return c
	}

	if resp.StatusCode() >= 500 {
		c.Status = "error"
		c.Message = fmt.Sprintf("server error: %d", resp.StatusCode())
		return c
	}

	// 400 (bad model) or 200 both mean the server is reachable
	c.Status = "ok"
	if resp.StatusCode() == 400 {
		c.Message = "server reachable (model not found — expected for minimal ping)"
	}

	return c
}

func (hc *Checker) checkTelegram(ctx context.Context) Check {
	start := time.Now()

	if hc.botCfg.Token == "" {
		return Check{
			Status:   "warn",
			Message:  "Telegram token not configured",
			Duration: time.Since(start).String(),
		}
	}

	// Use a lightweight request to verify the token
	httpClient := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", hc.botCfg.Token)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Check{
			Status:   "error",
			Message:  fmt.Sprintf("request build failed: %v", err),
			Duration: time.Since(start).String(),
		}
	}

	resp, err := httpClient.Do(req)
	dur := time.Since(start)

	c := Check{Duration: dur.String()}
	if err != nil {
		c.Status = "error"
		c.Message = fmt.Sprintf("request failed: %v", err)
		return c
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		c.Status = "error"
		c.Message = fmt.Sprintf("API returned %d", resp.StatusCode)
		return c
	}

	c.Status = "ok"
	return c
}

// StartHTTPServer starts the health check HTTP server on the configured port.
// Returns the server and a function to shut it down.
func (hc *Checker) StartHTTPServer(ctx context.Context) (*http.Server, func()) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", hc.Handler())

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", hc.port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		hc.logger.Info("healthcheck: server started", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			hc.logger.Error("healthcheck: server error", slog.String("error", err.Error()))
		}
	}()

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}

	return srv, shutdown
}
