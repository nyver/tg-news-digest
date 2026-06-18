// Package healthcheck provides a health HTTP endpoint for monitoring
// database, LLM, and Telegram Bot API connectivity.
package healthcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/models"
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
	rssCfg  config.RSSConfig
	llmCfg  config.LLMConfig
	botCfg  config.BotConfig
	logger  *slog.Logger
	started time.Time
	port    int
}

// New creates a health checker with the given configuration.
func New(cfg config.Config, store *storage.Store, logger *slog.Logger) *Checker {
	return &Checker{
		store:   store,
		rssCfg:  cfg.RSS,
		llmCfg:  cfg.LLM,
		botCfg:  cfg.Bot,
		logger:  logger,
		started: time.Now(),
		port:    9100, // default health check port
	}
}

type SubscriberCounts struct {
	Active int `json:"active"`
	Total  int `json:"total"`
}

type DashboardData struct {
	GeneratedAt       string                  `json:"generated_at"`
	Uptime            string                  `json:"uptime"`
	Sources           []string                `json:"sources"`
	Subscribers       SubscriberCounts        `json:"subscribers"`
	RecentRSSErrors   []models.RSSError       `json:"recent_rss_errors"`
	RecentDigests     []models.DigestRun      `json:"recent_digests"`
	RecentBroadcasts  []models.BroadcastStats `json:"recent_broadcasts"`
	PopularCategories []models.CategoryStat   `json:"popular_categories"`
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

func (hc *Checker) DashboardJSONHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		data, err := hc.Dashboard(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(data); err != nil && hc.logger != nil {
			hc.logger.Error("dashboard: encode response", slog.String("error", err.Error()))
		}
	}
}

func (hc *Checker) DashboardHandler() http.HandlerFunc {
	tmpl := template.Must(template.New("dashboard").Parse(dashboardHTML))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		data, err := hc.Dashboard(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data); err != nil && hc.logger != nil {
			hc.logger.Error("dashboard: render response", slog.String("error", err.Error()))
		}
	}
}

func (hc *Checker) Dashboard(ctx context.Context) (DashboardData, error) {
	data := DashboardData{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Uptime:      time.Since(hc.started).String(),
		Sources:     append([]string(nil), hc.rssCfg.Feeds...),
	}
	if hc.store == nil {
		return data, fmt.Errorf("store is nil")
	}

	active, total, err := hc.store.CountSubscribers(ctx)
	if err != nil {
		return data, fmt.Errorf("dashboard: count subscribers: %w", err)
	}
	data.Subscribers = SubscriberCounts{Active: active, Total: total}

	if data.RecentRSSErrors, err = hc.store.GetRecentRSSErrors(ctx, 10); err != nil {
		return data, fmt.Errorf("dashboard: rss errors: %w", err)
	}
	if data.RecentDigests, err = hc.store.GetRecentDigestRuns(ctx, 10); err != nil {
		return data, fmt.Errorf("dashboard: digest runs: %w", err)
	}
	if data.RecentBroadcasts, err = hc.store.GetRecentBroadcastStats(ctx, 10); err != nil {
		return data, fmt.Errorf("dashboard: broadcast stats: %w", err)
	}
	if data.PopularCategories, err = hc.store.GetPopularCategories(ctx, 10); err != nil {
		return data, fmt.Errorf("dashboard: categories: %w", err)
	}
	return data, nil
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

	if hc.store == nil {
		return Check{
			Status:   "error",
			Message:  "store is nil",
			Duration: time.Since(start).String(),
		}
	}

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
	var httpCli *resty.Client
	var path string

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
		httpCli = resty.New().
			SetBaseURL("https://openrouter.ai/api/v1").
			SetHeaders(headers).
			SetTimeout(10 * time.Second)
		path = "/chat/completions"
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
		httpCli = resty.New().
			SetBaseURL(hc.llmCfg.Endpoint).
			SetTimeout(10 * time.Second)
		path = "/v1/chat/completions"
	}

	resp, err := httpCli.R().
		SetContext(ctx).
		SetBody(reqBody).
		Post(path)

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

	if resp.StatusCode() == http.StatusOK {
		c.Status = "ok"
		return c
	}

	// 400 (bad model) means the server is reachable for this minimal ping.
	if resp.StatusCode() == http.StatusBadRequest {
		c.Status = "ok"
		c.Message = "server reachable (model not found — expected for minimal ping)"
		return c
	}

	c.Status = "error"
	c.Message = fmt.Sprintf("unexpected HTTP status: %d", resp.StatusCode())
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
	mux.HandleFunc("/dashboard", hc.DashboardHandler())
	mux.HandleFunc("/dashboard.json", hc.DashboardJSONHandler())

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", hc.port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		if hc.logger != nil {
			hc.logger.Info("healthcheck: server started", slog.String("addr", srv.Addr))
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			if hc.logger != nil {
				hc.logger.Error("healthcheck: server error", slog.String("error", err.Error()))
			}
		}
	}()

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}

	return srv, shutdown
}

const dashboardHTML = `<!doctype html>
<html lang="ru">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>TG News Digest Dashboard</title>
  <style>
    body { font-family: system-ui, -apple-system, Segoe UI, sans-serif; margin: 24px; color: #17202a; background: #f7f8fa; }
    h1 { margin: 0 0 4px; font-size: 28px; }
    h2 { margin: 28px 0 12px; font-size: 18px; }
    .muted { color: #65717f; }
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 12px; margin-top: 18px; }
    .card, table { background: white; border: 1px solid #dfe4ea; border-radius: 8px; }
    .card { padding: 14px 16px; }
    .value { font-size: 26px; font-weight: 700; margin-top: 6px; }
    table { width: 100%; border-collapse: collapse; overflow: hidden; }
    th, td { text-align: left; padding: 10px 12px; border-bottom: 1px solid #edf0f3; vertical-align: top; }
    th { font-size: 12px; color: #65717f; text-transform: uppercase; letter-spacing: .04em; background: #fafbfc; }
    tr:last-child td { border-bottom: 0; }
    code { background: #edf0f3; padding: 2px 5px; border-radius: 4px; }
    .empty { background: white; border: 1px solid #dfe4ea; border-radius: 8px; padding: 14px 16px; color: #65717f; }
  </style>
</head>
<body>
  <h1>TG News Digest Dashboard</h1>
  <div class="muted">Generated {{.GeneratedAt}} · uptime {{.Uptime}} · <a href="/dashboard.json">JSON</a></div>

  <div class="grid">
    <div class="card"><div class="muted">Active subscribers</div><div class="value">{{.Subscribers.Active}}</div></div>
    <div class="card"><div class="muted">Total subscribers</div><div class="value">{{.Subscribers.Total}}</div></div>
    <div class="card"><div class="muted">Sources</div><div class="value">{{len .Sources}}</div></div>
  </div>

  <h2>Sources</h2>
  {{if .Sources}}<table><tr><th>Feed URL</th></tr>{{range .Sources}}<tr><td><code>{{.}}</code></td></tr>{{end}}</table>{{else}}<div class="empty">No sources configured.</div>{{end}}

  <h2>Recent RSS Errors</h2>
  {{if .RecentRSSErrors}}<table><tr><th>Time</th><th>Feed</th><th>Error</th></tr>{{range .RecentRSSErrors}}<tr><td>{{.CreatedAt}}</td><td><code>{{.FeedURL}}</code></td><td>{{.Error}}</td></tr>{{end}}</table>{{else}}<div class="empty">No RSS errors recorded.</div>{{end}}

  <h2>Recent Digests</h2>
  {{if .RecentDigests}}<table><tr><th>Time</th><th>Status</th><th>Trigger</th><th>Items</th><th>LLM</th><th>Error</th></tr>{{range .RecentDigests}}<tr><td>{{.RunAt}}</td><td>{{.Status}}</td><td>{{.Trigger}}</td><td>{{.ItemCount}}</td><td>{{.LLMUsed}}</td><td>{{.ErrorMsg}}</td></tr>{{end}}</table>{{else}}<div class="empty">No digest runs recorded.</div>{{end}}

  <h2>Broadcast Delivery</h2>
  {{if .RecentBroadcasts}}<table><tr><th>Time</th><th>Recipients</th><th>Sent</th><th>Failed</th><th>Skipped</th></tr>{{range .RecentBroadcasts}}<tr><td>{{.RunAt}}</td><td>{{.Recipients}}</td><td>{{.SentMessages}}</td><td>{{.FailedMessages}}</td><td>{{.SkippedNoMatch}}</td></tr>{{end}}</table>{{else}}<div class="empty">No broadcast stats recorded.</div>{{end}}

  <h2>Popular Categories</h2>
  {{if .PopularCategories}}<table><tr><th>Category</th><th>Subscribers</th></tr>{{range .PopularCategories}}<tr><td>{{.Category}}</td><td>{{.Count}}</td></tr>{{end}}</table>{{else}}<div class="empty">No selected categories yet.</div>{{end}}
</body>
</html>`
