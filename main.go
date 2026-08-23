package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultBindAddr             = "127.0.0.1:8080"
	defaultUserAgent            = "codex-tui/0.146.0 (Ubuntu 22.4.0; x86_64) xterm-256color"
	defaultUsageMode            = "wham"
	defaultCacheTTL             = 10 * time.Minute
	upstreamRequestTimeout      = 15 * time.Second
	resetStatusEndpoint         = "https://codex-resets.com/api/v1/status"
	dailyTokenUsageEndpoint     = "https://chatgpt.com/backend-api/wham/usage/daily-token-usage-breakdown"
	dailyWorkspaceUsageEndpoint = "https://chatgpt.com/backend-api/wham/analytics/daily-workspace-usage-counts"
)

// indexHTML is embedded so the same Go binary serves both the API and the
// LX04 WebView page. There is no CDN or separate frontend build step.
//
//go:embed web/index.html
var indexHTML []byte

//go:embed web/settings.html
var settingsHTML []byte

type Config struct {
	BindAddr          string
	BasePath          string
	BasicAuthEnabled  bool
	BasicAuthUsername string
	BasicAuthPassword string
	AccessToken       string
	ChatGPTAccountID  string
	AppAPIKey         string
	UserAgent         string
	UpstreamProxy     string
	CacheTTL          time.Duration
	CORSOrigin        string
	ConfigPath        string
	UsageMode         string
	FedRAMP           bool
}

type fileConfig struct {
	BindAddr  string `json:"bind_addr"`
	BasePath  string `json:"base_path"`
	AppAPIKey string `json:"app_api_key"`
	BasicAuth struct {
		Enabled  bool   `json:"enabled"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"basic_auth"`
	CacheTTL   string `json:"cache_ttl"`
	UsageMode  string `json:"usage_mode"`
	CORSOrigin string `json:"cors_origin"`
	OpenAI     struct {
		AccessToken      string `json:"access_token"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		UserAgent        string `json:"user_agent"`
		FedRAMP          bool   `json:"fedramp"`
	} `json:"openai"`
	Proxy struct {
		URL string `json:"url"`
	} `json:"proxy"`
}

func loadConfig() (Config, error) {
	configPathFromEnv := strings.TrimSpace(os.Getenv("CONFIG_FILE"))
	configPath := configPathFromEnv
	if configPath == "" {
		configPath = "config.json"
	}

	cfg := Config{
		BindAddr:   defaultBindAddr,
		UserAgent:  defaultUserAgent,
		CacheTTL:   defaultCacheTTL,
		ConfigPath: configPath,
		UsageMode:  defaultUsageMode,
	}

	if raw, err := os.ReadFile(configPath); err == nil {
		var stored fileConfig
		if err := json.Unmarshal(raw, &stored); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", configPath, err)
		}
		applyFileConfig(&cfg, stored)
	} else if !os.IsNotExist(err) || configPathFromEnv != "" {
		return Config{}, fmt.Errorf("read %s: %w", configPath, err)
	}

	if err := applyEnvironmentConfig(&cfg); err != nil {
		return Config{}, err
	}
	basePath, err := normalizeBasePath(cfg.BasePath)
	if err != nil {
		return Config{}, err
	}
	cfg.BasePath = basePath

	if cfg.AccessToken == "" {
		return Config{}, errors.New("OPENAI_ACCESS_TOKEN is required")
	}
	if cfg.ChatGPTAccountID == "" {
		return Config{}, errors.New("CHATGPT_ACCOUNT_ID is required")
	}
	if cfg.UsageMode != defaultUsageMode {
		return Config{}, fmt.Errorf("invalid USAGE_MODE %q; only wham is supported", cfg.UsageMode)
	}
	if cfg.BasicAuthEnabled && (cfg.BasicAuthUsername == "" || cfg.BasicAuthPassword == "") {
		return Config{}, errors.New("BASIC_AUTH_USER and BASIC_AUTH_PASSWORD are required when Basic Auth is enabled")
	}
	return cfg, nil
}

func applyFileConfig(cfg *Config, stored fileConfig) {
	if stored.BindAddr != "" {
		cfg.BindAddr = stored.BindAddr
	}
	if stored.BasePath != "" {
		cfg.BasePath = strings.TrimSpace(stored.BasePath)
	}
	if stored.AppAPIKey != "" {
		cfg.AppAPIKey = strings.TrimSpace(stored.AppAPIKey)
	}
	if stored.BasicAuth.Enabled {
		cfg.BasicAuthEnabled = true
	}
	if stored.BasicAuth.Username != "" {
		cfg.BasicAuthUsername = strings.TrimSpace(stored.BasicAuth.Username)
	}
	if stored.BasicAuth.Password != "" {
		cfg.BasicAuthPassword = stored.BasicAuth.Password
	}
	if stored.CORSOrigin != "" {
		cfg.CORSOrigin = strings.TrimSpace(stored.CORSOrigin)
	}
	if stored.UsageMode != "" {
		cfg.UsageMode = strings.TrimSpace(stored.UsageMode)
	}
	if stored.OpenAI.FedRAMP {
		cfg.FedRAMP = true
	}
	if stored.OpenAI.AccessToken != "" {
		cfg.AccessToken = strings.TrimSpace(stored.OpenAI.AccessToken)
	}
	if stored.OpenAI.ChatGPTAccountID != "" {
		cfg.ChatGPTAccountID = strings.TrimSpace(stored.OpenAI.ChatGPTAccountID)
	}
	if stored.OpenAI.UserAgent != "" {
		cfg.UserAgent = strings.TrimSpace(stored.OpenAI.UserAgent)
	}
	if stored.Proxy.URL != "" {
		cfg.UpstreamProxy = strings.TrimSpace(stored.Proxy.URL)
	}
	if stored.CacheTTL != "" {
		if ttl, err := time.ParseDuration(stored.CacheTTL); err == nil && ttl >= 0 {
			cfg.CacheTTL = ttl
		}
	}
}

func applyEnvironmentConfig(cfg *Config) error {
	overrideString := func(target *string, name string) {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			*target = value
		}
	}
	overrideString(&cfg.BindAddr, "BIND_ADDR")
	overrideString(&cfg.BasePath, "BASE_PATH")
	overrideString(&cfg.AccessToken, "OPENAI_ACCESS_TOKEN")
	overrideString(&cfg.ChatGPTAccountID, "CHATGPT_ACCOUNT_ID")
	overrideString(&cfg.AppAPIKey, "APP_API_KEY")
	overrideString(&cfg.BasicAuthUsername, "BASIC_AUTH_USER")
	overrideString(&cfg.BasicAuthPassword, "BASIC_AUTH_PASSWORD")
	overrideString(&cfg.UserAgent, "OPENAI_USER_AGENT")
	overrideString(&cfg.UpstreamProxy, "UPSTREAM_PROXY")
	overrideString(&cfg.CORSOrigin, "CORS_ORIGIN")
	overrideString(&cfg.UsageMode, "USAGE_MODE")
	if raw := strings.TrimSpace(os.Getenv("OPENAI_FEDRAMP")); raw != "" {
		fedRAMP, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid OPENAI_FEDRAMP %q", raw)
		}
		cfg.FedRAMP = fedRAMP
	}
	if raw := strings.TrimSpace(os.Getenv("BASIC_AUTH_ENABLED")); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid BASIC_AUTH_ENABLED %q", raw)
		}
		cfg.BasicAuthEnabled = enabled
	}

	if raw := strings.TrimSpace(os.Getenv("USAGE_CACHE_TTL")); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil || ttl < 0 {
			return fmt.Errorf("invalid USAGE_CACHE_TTL %q", raw)
		}
		cfg.CacheTTL = ttl
	}
	return nil
}

func normalizeBasePath(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || value == "/" {
		return "", nil
	}
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?#") {
		return "", fmt.Errorf("invalid BASE_PATH %q; use a path such as /codex", raw)
	}
	value = strings.TrimRight(value, "/")
	if value == "" {
		return "", nil
	}
	return value, nil
}

type Window struct {
	UsedPercent       float64   `json:"used_percent"`
	WindowMinutes     int       `json:"window_minutes,omitempty"`
	ResetAfterSeconds int       `json:"reset_after_seconds,omitempty"`
	ResetAt           time.Time `json:"reset_at"`
	RemainingSeconds  int64     `json:"remaining_seconds"`
}

type UsageResponse struct {
	Source                string         `json:"source"`
	PlanType              string         `json:"plan_type,omitempty"`
	Email                 string         `json:"email,omitempty"`
	RateLimitAllowed      bool           `json:"rate_limit_allowed"`
	RateLimitReached      bool           `json:"rate_limit_reached"`
	RateLimitReachedType  string         `json:"rate_limit_reached_type,omitempty"`
	Credits               *Credits       `json:"credits,omitempty"`
	SpendControl          *SpendControl  `json:"spend_control,omitempty"`
	RateLimitResetCredits *ResetCredits  `json:"rate_limit_reset_credits,omitempty"`
	FetchedAt             string         `json:"fetched_at"`
	FromCache             bool           `json:"from_cache"`
	FiveHour              *Window        `json:"five_hour,omitempty"`
	SevenDay              *Window        `json:"seven_day,omitempty"`
	History               []HistoryPoint `json:"history,omitempty"`
}

type Credits struct {
	HasCredits          bool  `json:"has_credits"`
	Unlimited           bool  `json:"unlimited"`
	OverageLimitReached bool  `json:"overage_limit_reached"`
	Balance             any   `json:"balance,omitempty"`
	ApproxLocalMessages []int `json:"approx_local_messages,omitempty"`
	ApproxCloudMessages []int `json:"approx_cloud_messages,omitempty"`
}

type SpendControl struct {
	Reached         bool `json:"reached"`
	IndividualLimit any  `json:"individual_limit,omitempty"`
}

type ResetCredits struct {
	AvailableCount           int `json:"available_count"`
	ApplicableAvailableCount int `json:"applicable_available_count"`
}

type HistoryPoint struct {
	At          string  `json:"at"`
	UsedPercent float64 `json:"used_percent"`
}

// UsageAnalytics is the compact, seven-day payload exposed to the frontend.
// The upstream responses contain a longer history and several nested
// breakdowns; keeping only the fields needed by the dashboard makes the
// Android WebView page faster and avoids exposing the raw upstream payload.
type UsageAnalytics struct {
	Source    string                `json:"source"`
	FetchedAt string                `json:"fetched_at"`
	FromCache bool                  `json:"from_cache"`
	Days      []UsageAnalyticsDay   `json:"days"`
	Summary   UsageAnalyticsSummary `json:"summary"`
}

type UsageAnalyticsDay struct {
	Date                    string                `json:"date"`
	TokenUsagePercent       float64               `json:"token_usage_percent"`
	Credits                 float64               `json:"credits"`
	Users                   int64                 `json:"users"`
	Threads                 int64                 `json:"threads"`
	Turns                   int64                 `json:"turns"`
	UncachedTextInputTokens int64                 `json:"uncached_text_input_tokens"`
	CachedTextInputTokens   int64                 `json:"cached_text_input_tokens"`
	TextOutputTokens        int64                 `json:"text_output_tokens"`
	TextTotalTokens         int64                 `json:"text_total_tokens"`
	Models                  []UsageAnalyticsModel `json:"models,omitempty"`
}

type UsageAnalyticsModel struct {
	Model        string  `json:"model"`
	UsagePercent float64 `json:"usage_percent"`
}

type UsageAnalyticsSummary struct {
	TokenUsagePercent float64 `json:"token_usage_percent"`
	Credits           float64 `json:"credits"`
	Users             int64   `json:"users"`
	Threads           int64   `json:"threads"`
	Turns             int64   `json:"turns"`
	TextTotalTokens   int64   `json:"text_total_tokens"`
}

type dailyTokenUsageEnvelope struct {
	Data []dailyTokenUsagePoint `json:"data"`
}

type dailyTokenUsagePoint struct {
	Date                      string             `json:"date"`
	ProductSurfaceUsageValues map[string]float64 `json:"product_surface_usage_values"`
	Models                    []dailyTokenModel  `json:"models"`
}

type dailyTokenModel struct {
	Model   string  `json:"model"`
	Speed   string  `json:"speed"`
	Credits float64 `json:"credits"`
}

type dailyWorkspaceUsageEnvelope struct {
	Data []dailyWorkspaceUsagePoint `json:"data"`
}

type dailyWorkspaceUsagePoint struct {
	Date   string                    `json:"date"`
	Totals dailyWorkspaceUsageTotals `json:"totals"`
}

type dailyWorkspaceUsageTotals struct {
	Users                   int64   `json:"users"`
	Threads                 int64   `json:"threads"`
	Turns                   int64   `json:"turns"`
	UncachedTextInputTokens int64   `json:"uncached_text_input_tokens"`
	CachedTextInputTokens   int64   `json:"cached_text_input_tokens"`
	TextOutputTokens        int64   `json:"text_output_tokens"`
	TextTotalTokens         int64   `json:"text_total_tokens"`
	Credits                 float64 `json:"credits"`
}

type ResetPrediction struct {
	Source      string      `json:"source"`
	FetchedAt   string      `json:"fetched_at"`
	FromCache   bool        `json:"from_cache"`
	LatestReset *ResetEvent `json:"latest_reset,omitempty"`
	ActiveWatch *ResetWatch `json:"active_watch,omitempty"`
	Stats       ResetStats  `json:"stats"`
}

type ResetEvent struct {
	ID          string       `json:"id"`
	ResetType   string       `json:"reset_type"`
	AnnouncedAt string       `json:"announced_at"`
	Text        string       `json:"text"`
	Source      *ResetSource `json:"source,omitempty"`
}

type ResetSource struct {
	Type   string `json:"type"`
	Author string `json:"author"`
	URL    string `json:"url"`
}

type ResetWatch struct {
	ResetChancePercent float64 `json:"reset_chance_percent"`
	ForecastWindow     string  `json:"forecast_window"`
	ObservedAt         string  `json:"observed_at"`
	ExpiresAt          string  `json:"expires_at"`
	Level              string  `json:"level"`
}

type ResetStats struct {
	Total           int     `json:"total"`
	LastResetAt     string  `json:"last_reset_at"`
	DaysSinceLast   float64 `json:"days_since_last"`
	AvgIntervalDays float64 `json:"avg_interval_days"`
}

type resetStatusEnvelope struct {
	Data struct {
		LatestReset *ResetEvent `json:"latest_reset"`
		ActiveWatch *ResetWatch `json:"active_watch"`
		Stats       ResetStats  `json:"stats"`
	} `json:"data"`
	Meta struct {
		APIVersion  string `json:"api_version"`
		GeneratedAt string `json:"generated_at"`
	} `json:"meta"`
}

type rawWindow struct {
	UsedPercent       *float64
	WindowMinutes     *int
	ResetAfterSeconds *int
	ResetAtUnix       *int64
}

// whamUsageResponse is the read-only quota payload returned by
// GET /backend-api/wham/usage. The fields are pointers because the upstream
// sometimes omits a secondary window or reset value.
type whamUsageResponse struct {
	PlanType              string                    `json:"plan_type"`
	Email                 string                    `json:"email"`
	RateLimit             *whamRateLimit            `json:"rate_limit"`
	CodeReviewRateLimit   *whamRateLimit            `json:"code_review_rate_limit"`
	AdditionalRateLimits  []whamAdditionalRateLimit `json:"additional_rate_limits"`
	Credits               *Credits                  `json:"credits"`
	SpendControl          *SpendControl             `json:"spend_control"`
	RateLimitReachedType  string                    `json:"rate_limit_reached_type"`
	RateLimitResetCredits *ResetCredits             `json:"rate_limit_reset_credits"`
}

type whamAdditionalRateLimit struct {
	MeteredFeature string         `json:"metered_feature"`
	RateLimit      *whamRateLimit `json:"rate_limit"`
}

type whamRateLimit struct {
	Allowed         bool        `json:"allowed"`
	LimitReached    bool        `json:"limit_reached"`
	PrimaryWindow   *whamWindow `json:"primary_window"`
	SecondaryWindow *whamWindow `json:"secondary_window"`
}

type whamWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

type UsageService struct {
	cfgMu              sync.RWMutex
	cfg                Config
	clientMu           sync.RWMutex
	client             *http.Client
	cacheMu            sync.Mutex
	refreshMu          sync.Mutex
	resetRefreshMu     sync.Mutex
	analyticsRefreshMu sync.Mutex
	cached             *UsageResponse
	cachedAt           time.Time
	history            []HistoryPoint
	resetCached        *ResetPrediction
	resetCachedAt      time.Time
	analyticsCached    *UsageAnalytics
	analyticsCachedAt  time.Time
}

func NewUsageService(cfg Config) (*UsageService, error) {
	proxyFunc, err := buildProxyFunc(cfg.UpstreamProxy)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		Proxy:                 proxyFunc,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	return &UsageService{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   upstreamRequestTimeout,
		},
	}, nil
}

func buildProxyFunc(raw string) (func(*http.Request) (*url.URL, error), error) {
	if raw == "" {
		return http.ProxyFromEnvironment, nil
	}
	proxyURL, err := url.Parse(raw)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("invalid UPSTREAM_PROXY")
	}
	return http.ProxyURL(proxyURL), nil
}

func (s *UsageService) Get(ctx context.Context, force bool) (*UsageResponse, error) {
	if !force {
		if cached := s.getFreshCache(); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	}

	// Serialize upstream usage requests and check the cache again after waiting.
	// This prevents concurrent WebView refreshes from producing duplicate requests.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	if !force {
		if cached := s.getFreshCache(); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	}

	usage, err := s.queryWhamUsage(ctx, s.currentConfig())
	if err != nil {
		return nil, err
	}

	s.cacheMu.Lock()
	if usage.SevenDay != nil {
		s.history = append(s.history, HistoryPoint{
			At:          usage.FetchedAt,
			UsedPercent: usage.SevenDay.UsedPercent,
		})
		if len(s.history) > 48 {
			s.history = s.history[len(s.history)-48:]
		}
	}
	usage.History = append([]HistoryPoint(nil), s.history...)
	s.cached = cloneUsage(usage)
	s.cachedAt = time.Now()
	s.cacheMu.Unlock()
	return cloneUsage(usage), nil
}

func (s *UsageService) GetAnalytics(ctx context.Context, force bool) (*UsageAnalytics, error) {
	if !force {
		if cached := s.getFreshAnalyticsCache(); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	}

	s.analyticsRefreshMu.Lock()
	defer s.analyticsRefreshMu.Unlock()

	if !force {
		if cached := s.getFreshAnalyticsCache(); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	}

	analytics, err := s.queryUsageAnalytics(ctx)
	if err != nil {
		return nil, err
	}

	s.cacheMu.Lock()
	s.analyticsCached = cloneUsageAnalytics(analytics)
	s.analyticsCachedAt = time.Now()
	s.cacheMu.Unlock()
	return cloneUsageAnalytics(analytics), nil
}

func (s *UsageService) queryUsageAnalytics(ctx context.Context) (*UsageAnalytics, error) {
	cfg := s.currentConfig()
	var tokenUsage dailyTokenUsageEnvelope
	if err := s.queryWhamJSON(ctx, cfg, dailyTokenUsageEndpoint, &tokenUsage); err != nil {
		return nil, fmt.Errorf("daily token usage request failed: %w", err)
	}

	var workspaceUsage dailyWorkspaceUsageEnvelope
	if err := s.queryWhamJSON(ctx, cfg, dailyWorkspaceUsageEndpoint, &workspaceUsage); err != nil {
		return nil, fmt.Errorf("daily workspace usage request failed: %w", err)
	}

	return mergeUsageAnalytics(tokenUsage.Data, workspaceUsage.Data), nil
}

func mergeUsageAnalytics(tokenUsage []dailyTokenUsagePoint, workspaceUsage []dailyWorkspaceUsagePoint) *UsageAnalytics {
	return mergeUsageAnalyticsAt(tokenUsage, workspaceUsage, time.Now())
}

func mergeUsageAnalyticsAt(tokenUsage []dailyTokenUsagePoint, workspaceUsage []dailyWorkspaceUsagePoint, now time.Time) *UsageAnalytics {
	tokenByDate := make(map[string]dailyTokenUsagePoint, len(tokenUsage))
	workspaceByDate := make(map[string]dailyWorkspaceUsagePoint, len(workspaceUsage))
	for _, point := range tokenUsage {
		if point.Date == "" {
			continue
		}
		tokenByDate[point.Date] = point
	}
	for _, point := range workspaceUsage {
		if point.Date == "" {
			continue
		}
		workspaceByDate[point.Date] = point
	}

	// Return the seven completed calendar days ending yesterday. The upstream
	// analytics endpoints publish the current day's row with a delay, so the
	// dashboard intentionally excludes the incomplete current day.
	dates := make([]string, 0, 7)
	for offset := 7; offset >= 1; offset-- {
		dates = append(dates, now.AddDate(0, 0, -offset).Format("2006-01-02"))
	}

	result := &UsageAnalytics{
		Source:    "wham_daily_usage",
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Days:      make([]UsageAnalyticsDay, 0, len(dates)),
	}
	for _, date := range dates {
		tokenPoint := tokenByDate[date]
		workspacePoint := workspaceByDate[date]
		tokenPercent := sumUsagePercent(tokenPoint.ProductSurfaceUsageValues)
		tokenCredits := sumTokenCredits(tokenPoint.Models)
		day := UsageAnalyticsDay{
			Date:                    date,
			TokenUsagePercent:       tokenPercent,
			Credits:                 workspacePoint.Totals.Credits,
			Users:                   workspacePoint.Totals.Users,
			Threads:                 workspacePoint.Totals.Threads,
			Turns:                   workspacePoint.Totals.Turns,
			UncachedTextInputTokens: workspacePoint.Totals.UncachedTextInputTokens,
			CachedTextInputTokens:   workspacePoint.Totals.CachedTextInputTokens,
			TextOutputTokens:        workspacePoint.Totals.TextOutputTokens,
			TextTotalTokens:         workspacePoint.Totals.TextTotalTokens,
			Models:                  aggregateTokenModels(tokenPoint.Models),
		}
		if day.TokenUsagePercent == 0 {
			day.TokenUsagePercent = tokenCredits
		}
		if day.Credits == 0 {
			day.Credits = tokenCredits
		}
		result.Days = append(result.Days, day)
		result.Summary.TokenUsagePercent += day.TokenUsagePercent
		result.Summary.Credits += day.Credits
		result.Summary.Threads += day.Threads
		result.Summary.Turns += day.Turns
		result.Summary.TextTotalTokens += day.TextTotalTokens
		if day.Users > result.Summary.Users {
			result.Summary.Users = day.Users
		}
	}
	return result
}

func sumUsagePercent(values map[string]float64) float64 {
	var total float64
	for _, value := range values {
		total += value
	}
	return total
}

func sumTokenCredits(models []dailyTokenModel) float64 {
	var total float64
	for _, model := range models {
		total += model.Credits
	}
	return total
}

func aggregateTokenModels(models []dailyTokenModel) []UsageAnalyticsModel {
	byModel := make(map[string]float64, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model.Model)
		if name == "" {
			name = "unknown"
		}
		byModel[name] += model.Credits
	}
	names := make([]string, 0, len(byModel))
	for name := range byModel {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]UsageAnalyticsModel, 0, len(names))
	for _, name := range names {
		result = append(result, UsageAnalyticsModel{Model: name, UsagePercent: byModel[name]})
	}
	return result
}

func newWhamRequest(ctx context.Context, endpoint string, cfg Config) (*http.Request, context.CancelFunc, error) {
	requestCtx, cancel := context.WithTimeout(ctx, upstreamRequestTimeout)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("create wham request: %w", err)
	}

	// These headers are the authentication/context headers used by the
	// ChatGPT web/Codex client. The OAuth access token and account ID are the
	// only account-specific values; the rest are protocol hints.
	request.Host = "chatgpt.com"
	request.Header.Set("Authorization", "Bearer "+cfg.AccessToken)
	request.Header.Set("chatgpt-account-id", cfg.ChatGPTAccountID)
	request.Header.Set("OpenAI-Beta", "codex-1")
	request.Header.Set("oai-language", "zh-CN")
	request.Header.Set("Originator", "Codex Desktop")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Sec-Fetch-Site", "none")
	request.Header.Set("Sec-Fetch-Mode", "no-cors")
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Priority", "u=4, i")
	request.Header.Set("User-Agent", cfg.UserAgent)
	if cfg.FedRAMP {
		request.Header.Set("x-openai-fedramp", "true")
	}
	return request, cancel, nil
}

func (s *UsageService) queryWhamJSON(ctx context.Context, cfg Config, endpoint string, target any) error {
	request, cancel, err := newWhamRequest(ctx, endpoint, cfg)
	if err != nil {
		return err
	}
	defer cancel()
	response, err := s.currentClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("upstream returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(target); err != nil {
		return err
	}
	return nil
}

func (s *UsageService) GetPrediction(ctx context.Context, force bool) (*ResetPrediction, error) {
	if !force {
		if cached := s.getFreshResetCache(); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	}

	s.resetRefreshMu.Lock()
	defer s.resetRefreshMu.Unlock()

	if !force {
		if cached := s.getFreshResetCache(); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	}

	prediction, err := s.queryResetPrediction(ctx)
	if err != nil {
		return nil, err
	}

	s.cacheMu.Lock()
	s.resetCached = cloneResetPrediction(prediction)
	s.resetCachedAt = time.Now()
	s.cacheMu.Unlock()
	return cloneResetPrediction(prediction), nil
}

func (s *UsageService) queryResetPrediction(ctx context.Context) (*ResetPrediction, error) {
	requestCtx, cancel := context.WithTimeout(ctx, upstreamRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, resetStatusEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create reset status request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "codex-usage-dashboard/1.0")

	response, err := s.currentClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("request reset status: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("reset status upstream returned HTTP %d", response.StatusCode)
	}

	var envelope resetStatusEnvelope
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode reset status: %w", err)
	}
	fetchedAt := envelope.Meta.GeneratedAt
	if fetchedAt == "" {
		fetchedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return &ResetPrediction{
		Source:      "codex_resets_status",
		FetchedAt:   fetchedAt,
		LatestReset: envelope.Data.LatestReset,
		ActiveWatch: envelope.Data.ActiveWatch,
		Stats:       envelope.Data.Stats,
	}, nil
}

type ConfigView struct {
	BasePath         string `json:"base_path"`
	BasicAuthEnabled bool   `json:"basic_auth_enabled"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	UsageMode        string `json:"usage_mode"`
	UserAgent        string `json:"user_agent"`
	FedRAMP          bool   `json:"fedramp"`
	TokenConfigured  bool   `json:"token_configured"`
	TokenHint        string `json:"token_hint,omitempty"`
	ProxyURL         string `json:"proxy_url,omitempty"`
	CacheTTL         string `json:"cache_ttl"`
	ConfigFile       string `json:"config_file"`
}

type ConfigUpdate struct {
	AccessToken      *string `json:"access_token"`
	ChatGPTAccountID *string `json:"chatgpt_account_id"`
	UsageMode        *string `json:"usage_mode"`
	UserAgent        *string `json:"user_agent"`
	FedRAMP          *bool   `json:"fedramp"`
	ProxyURL         *string `json:"proxy_url"`
	CacheTTL         *string `json:"cache_ttl"`
}

func (s *UsageService) currentConfig() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *UsageService) currentClient() *http.Client {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()
	return s.client
}

func (s *UsageService) ConfigView() ConfigView {
	cfg := s.currentConfig()
	tokenHint := ""
	if len(cfg.AccessToken) >= 4 {
		tokenHint = "****" + cfg.AccessToken[len(cfg.AccessToken)-4:]
	}
	return ConfigView{
		BasePath:         cfg.BasePath,
		BasicAuthEnabled: cfg.BasicAuthEnabled,
		ChatGPTAccountID: cfg.ChatGPTAccountID,
		UsageMode:        cfg.UsageMode,
		UserAgent:        cfg.UserAgent,
		FedRAMP:          cfg.FedRAMP,
		TokenConfigured:  cfg.AccessToken != "",
		TokenHint:        tokenHint,
		ProxyURL:         cfg.UpstreamProxy,
		CacheTTL:         cfg.CacheTTL.String(),
		ConfigFile:       cfg.ConfigPath,
	}
}

func (s *UsageService) UpdateConfig(update ConfigUpdate) (ConfigView, error) {
	old := s.currentConfig()
	next := old
	if update.AccessToken != nil {
		next.AccessToken = strings.TrimSpace(*update.AccessToken)
	}
	if update.ChatGPTAccountID != nil {
		next.ChatGPTAccountID = strings.TrimSpace(*update.ChatGPTAccountID)
	}
	if update.UsageMode != nil {
		next.UsageMode = strings.ToLower(strings.TrimSpace(*update.UsageMode))
	}
	if update.UserAgent != nil {
		next.UserAgent = strings.TrimSpace(*update.UserAgent)
		if next.UserAgent == "" {
			next.UserAgent = defaultUserAgent
		}
	}
	if update.FedRAMP != nil {
		next.FedRAMP = *update.FedRAMP
	}
	if update.ProxyURL != nil {
		next.UpstreamProxy = strings.TrimSpace(*update.ProxyURL)
	}
	if update.CacheTTL != nil {
		ttl, err := time.ParseDuration(strings.TrimSpace(*update.CacheTTL))
		if err != nil || ttl < 0 {
			return ConfigView{}, fmt.Errorf("invalid cache_ttl")
		}
		next.CacheTTL = ttl
	}
	if next.AccessToken == "" {
		return ConfigView{}, errors.New("access_token cannot be empty")
	}
	if next.ChatGPTAccountID == "" {
		return ConfigView{}, errors.New("chatgpt_account_id cannot be empty")
	}
	if next.UsageMode != defaultUsageMode {
		return ConfigView{}, errors.New("usage_mode only supports wham")
	}

	proxyFunc, err := buildProxyFunc(next.UpstreamProxy)
	if err != nil {
		return ConfigView{}, err
	}
	if err := persistConfig(next); err != nil {
		return ConfigView{}, err
	}

	if next.UpstreamProxy != old.UpstreamProxy {
		transport := &http.Transport{
			Proxy:                 proxyFunc,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		}
		s.clientMu.Lock()
		s.client = &http.Client{Transport: transport, Timeout: upstreamRequestTimeout}
		s.clientMu.Unlock()
	}

	s.cfgMu.Lock()
	s.cfg = next
	s.cfgMu.Unlock()

	// A changed token, account, proxy, or cache policy must not reuse an earlier
	// account snapshot.
	s.cacheMu.Lock()
	s.cached = nil
	s.cachedAt = time.Time{}
	s.history = nil
	s.resetCached = nil
	s.resetCachedAt = time.Time{}
	s.analyticsCached = nil
	s.analyticsCachedAt = time.Time{}
	s.cacheMu.Unlock()
	return s.ConfigView(), nil
}

func persistConfig(cfg Config) error {
	stored := fileConfig{
		BindAddr:   cfg.BindAddr,
		BasePath:   cfg.BasePath,
		AppAPIKey:  cfg.AppAPIKey,
		CacheTTL:   cfg.CacheTTL.String(),
		UsageMode:  cfg.UsageMode,
		CORSOrigin: cfg.CORSOrigin,
	}
	stored.BasicAuth.Enabled = cfg.BasicAuthEnabled
	stored.BasicAuth.Username = cfg.BasicAuthUsername
	stored.BasicAuth.Password = cfg.BasicAuthPassword
	stored.OpenAI.AccessToken = cfg.AccessToken
	stored.OpenAI.ChatGPTAccountID = cfg.ChatGPTAccountID
	stored.OpenAI.UserAgent = cfg.UserAgent
	stored.OpenAI.FedRAMP = cfg.FedRAMP
	stored.Proxy.URL = cfg.UpstreamProxy

	raw, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(cfg.ConfigPath, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func (s *UsageService) getFreshCache() *UsageResponse {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cached == nil || time.Since(s.cachedAt) >= s.currentConfig().CacheTTL {
		return nil
	}
	return cloneUsage(s.cached)
}

func (s *UsageService) getFreshAnalyticsCache() *UsageAnalytics {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.analyticsCached == nil || time.Since(s.analyticsCachedAt) >= s.currentConfig().CacheTTL {
		return nil
	}
	return cloneUsageAnalytics(s.analyticsCached)
}

func (s *UsageService) getFreshResetCache() *ResetPrediction {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.resetCached == nil || time.Since(s.resetCachedAt) >= s.currentConfig().CacheTTL {
		return nil
	}
	return cloneResetPrediction(s.resetCached)
}

func cloneUsageAnalytics(input *UsageAnalytics) *UsageAnalytics {
	if input == nil {
		return nil
	}
	output := *input
	if input.Days != nil {
		output.Days = append([]UsageAnalyticsDay(nil), input.Days...)
	}
	return &output
}

func cloneResetPrediction(input *ResetPrediction) *ResetPrediction {
	if input == nil {
		return nil
	}
	output := *input
	if input.LatestReset != nil {
		event := *input.LatestReset
		if input.LatestReset.Source != nil {
			source := *input.LatestReset.Source
			event.Source = &source
		}
		output.LatestReset = &event
	}
	if input.ActiveWatch != nil {
		watch := *input.ActiveWatch
		output.ActiveWatch = &watch
	}
	return &output
}

func cloneUsage(input *UsageResponse) *UsageResponse {
	if input == nil {
		return nil
	}
	output := *input
	if input.FiveHour != nil {
		window := *input.FiveHour
		output.FiveHour = &window
	}
	if input.SevenDay != nil {
		window := *input.SevenDay
		output.SevenDay = &window
	}
	if input.History != nil {
		output.History = append([]HistoryPoint(nil), input.History...)
	}
	return &output
}

func (s *UsageService) queryWhamUsage(ctx context.Context, cfg Config) (*UsageResponse, error) {
	var upstream whamUsageResponse
	if err := s.queryWhamJSON(ctx, cfg, "https://chatgpt.com/backend-api/wham/usage", &upstream); err != nil {
		return nil, fmt.Errorf("wham request: %w", err)
	}

	rateLimit := upstream.RateLimit
	if rateLimit == nil || (rateLimit.PrimaryWindow == nil && rateLimit.SecondaryWindow == nil) {
		// Some plan variants expose the Codex window under an additional
		// metered feature instead of the top-level rate_limit field.
		for _, additional := range upstream.AdditionalRateLimits {
			if additional.MeteredFeature == "codex_bengalfox" && additional.RateLimit != nil {
				rateLimit = additional.RateLimit
				break
			}
		}
	}
	if rateLimit == nil {
		return nil, errors.New("wham response did not contain rate_limit")
	}

	fetchedAt := time.Now().UTC()
	primary := rawWindowFromWham(rateLimit.PrimaryWindow)
	secondary := rawWindowFromWham(rateLimit.SecondaryWindow)
	usage := &UsageResponse{
		Source:                "wham_usage",
		PlanType:              upstream.PlanType,
		Email:                 upstream.Email,
		RateLimitAllowed:      rateLimit.Allowed,
		RateLimitReached:      rateLimit.LimitReached,
		RateLimitReachedType:  upstream.RateLimitReachedType,
		Credits:               upstream.Credits,
		SpendControl:          upstream.SpendControl,
		RateLimitResetCredits: upstream.RateLimitResetCredits,
		FetchedAt:             fetchedAt.Format(time.RFC3339),
	}
	usage.FiveHour, usage.SevenDay = normalizeWindows(primary, secondary, fetchedAt)
	if usage.FiveHour == nil && usage.SevenDay == nil {
		return nil, errors.New("wham response did not contain usable rate-limit windows")
	}
	return usage, nil
}

func rawWindowFromWham(input *whamWindow) rawWindow {
	if input == nil {
		return rawWindow{}
	}
	raw := rawWindow{
		UsedPercent: input.UsedPercent,
		ResetAtUnix: input.ResetAt,
	}
	if input.LimitWindowSeconds != nil {
		minutes := int(*input.LimitWindowSeconds / 60)
		raw.WindowMinutes = &minutes
	}
	if input.ResetAfterSeconds != nil {
		seconds := int(*input.ResetAfterSeconds)
		raw.ResetAfterSeconds = &seconds
	}
	return raw
}

func normalizeWindows(primary, secondary rawWindow, fetchedAt time.Time) (*Window, *Window) {
	var fiveHour, sevenDay rawWindow
	assign := func(window rawWindow, preferred string) {
		if window.UsedPercent == nil && window.ResetAfterSeconds == nil && window.WindowMinutes == nil && window.ResetAtUnix == nil {
			return
		}
		minutes := 0
		if window.WindowMinutes != nil {
			minutes = *window.WindowMinutes
		}
		if minutes >= 24*60 || (minutes == 0 && preferred == "seven_day") {
			sevenDay = window
		} else {
			fiveHour = window
		}
	}

	assign(primary, "seven_day")
	assign(secondary, "five_hour")

	// If the upstream omits window lengths, retain the repository's usual
	// primary=7d / secondary=5h convention.
	if fiveHour.UsedPercent == nil && fiveHour.ResetAfterSeconds == nil &&
		fiveHour.WindowMinutes == nil && fiveHour.ResetAtUnix == nil {
		fiveHour = secondary
	}
	if sevenDay.UsedPercent == nil && sevenDay.ResetAfterSeconds == nil &&
		sevenDay.WindowMinutes == nil && sevenDay.ResetAtUnix == nil {
		sevenDay = primary
	}

	return makeWindow(fiveHour, fetchedAt), makeWindow(sevenDay, fetchedAt)
}

func makeWindow(raw rawWindow, fetchedAt time.Time) *Window {
	if raw.UsedPercent == nil && raw.ResetAfterSeconds == nil && raw.WindowMinutes == nil && raw.ResetAtUnix == nil {
		return nil
	}
	window := &Window{}
	if raw.UsedPercent != nil {
		window.UsedPercent = *raw.UsedPercent
	}
	if raw.WindowMinutes != nil {
		window.WindowMinutes = maxInt(*raw.WindowMinutes, 0)
	}
	if raw.ResetAfterSeconds != nil {
		window.ResetAfterSeconds = maxInt(*raw.ResetAfterSeconds, 0)
	}
	if raw.ResetAtUnix != nil && *raw.ResetAtUnix > 0 {
		window.ResetAt = time.Unix(*raw.ResetAtUnix, 0).UTC()
	} else if raw.ResetAfterSeconds != nil {
		window.ResetAt = fetchedAt.Add(time.Duration(window.ResetAfterSeconds) * time.Second)
	}
	if !window.ResetAt.IsZero() {
		window.RemainingSeconds = int64(time.Until(window.ResetAt).Seconds())
		if window.RemainingSeconds < 0 {
			window.RemainingSeconds = 0
		}
	}
	return window
}

func maxInt(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

type Server struct {
	cfg      Config
	usage    *UsageService
	basePath string
	handler  http.Handler
}

func NewServer(cfg Config, usage *UsageService) *Server {
	server := &Server{cfg: cfg, usage: usage, basePath: cfg.BasePath}
	mux := http.NewServeMux()
	server.registerRoutes(mux, cfg.BasePath)
	if cfg.BasePath != "" {
		// Also accept root routes for reverse proxies that strip /codex before
		// forwarding the request to this Go process.
		server.registerRoutes(mux, "")
		mux.HandleFunc("GET "+cfg.BasePath, server.handleBaseRedirect)
	}
	server.handler = server.withMiddleware(mux)
	return server
}

func (s *Server) registerRoutes(mux *http.ServeMux, prefix string) {
	route := func(suffix string) string {
		if prefix == "" {
			return suffix
		}
		if suffix == "/" {
			return prefix + "/"
		}
		return prefix + suffix
	}
	mux.HandleFunc("GET "+route("/healthz"), s.handleHealth)
	mux.HandleFunc("GET "+route("/"), s.handleIndex)
	mux.HandleFunc("GET "+route("/settings"), s.handleSettings)
	mux.HandleFunc("GET "+route("/audio"), s.handleAudio)
	mux.HandleFunc("GET "+route("/api/usage"), s.handleUsage)
	mux.HandleFunc("GET "+route("/api/usage/analytics"), s.handleUsageAnalytics)
	mux.HandleFunc("GET "+route("/api/prediction"), s.handlePrediction)
	mux.HandleFunc("GET "+route("/api/config"), s.handleConfigGet)
	mux.HandleFunc("PUT "+route("/api/config"), s.handleConfigPut)
	mux.HandleFunc("OPTIONS "+route("/api/usage"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/usage/analytics"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/prediction"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config"), s.handleOptions)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if s.cfg.CORSOrigin != "" {
			response.Header().Set("Access-Control-Allow-Origin", s.cfg.CORSOrigin)
			response.Header().Set("Vary", "Origin")
			response.Header().Set("Access-Control-Allow-Headers", "Authorization, X-App-API-Key, Content-Type")
			response.Header().Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
		}
		if s.cfg.BasicAuthEnabled && !authorizedBasic(request, s.cfg.BasicAuthUsername, s.cfg.BasicAuthPassword) {
			response.Header().Set("WWW-Authenticate", `Basic realm="Codex Usage"`)
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Basic Auth is the page/API authentication mechanism. If it is enabled
		// and already passed, do not require a second app API key as well. The
		// app key remains available for deployments that do not use Basic Auth.
		appKeyRequired := s.isAPIPath(request.URL.Path) && s.cfg.AppAPIKey != ""
		basicAuthPassed := s.cfg.BasicAuthEnabled && authorizedBasic(request, s.cfg.BasicAuthUsername, s.cfg.BasicAuthPassword)
		if appKeyRequired && !basicAuthPassed && !authorized(request, s.cfg.AppAPIKey) {
			writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) isAPIPath(requestPath string) bool {
	if strings.HasPrefix(requestPath, "/api/") {
		return true
	}
	return s.basePath != "" && strings.HasPrefix(requestPath, s.basePath+"/api/")
}

func authorized(request *http.Request, expected string) bool {
	if provided := strings.TrimSpace(request.Header.Get("X-App-API-Key")); provided != "" {
		return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
	}
	const prefix = "Bearer "
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func authorizedBasic(request *http.Request, expectedUser, expectedPassword string) bool {
	providedUser, providedPassword, ok := request.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(providedUser), []byte(expectedUser)) == 1
	passwordOK := subtle.ConstantTimeCompare([]byte(providedPassword), []byte(expectedPassword)) == 1
	return userOK && passwordOK
}

func (s *Server) handleHealth(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleIndex(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(indexHTML)
}

func (s *Server) handleSettings(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(settingsHTML)
}

func (s *Server) handleAudio(response http.ResponseWriter, request *http.Request) {
	kind := strings.TrimSpace(request.URL.Query().Get("kind"))
	data, err := embeddedAudio(kind)
	if err != nil {
		writeJSON(response, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	response.Header().Set("Content-Type", "audio/wav")
	response.Header().Set("Cache-Control", "public, max-age=3600")
	response.Header().Set("Content-Length", strconv.Itoa(len(data)))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(data)
}

func embeddedAudio(kind string) ([]byte, error) {
	markers := map[string]string{
		"normal":   "quotaAlertAudio = new Audio('data:audio/wav;base64,",
		"warning":  "quotaAlertAudioWarning = new Audio('data:audio/wav;base64,",
		"critical": "quotaAlertAudioCritical = new Audio('data:audio/wav;base64,",
	}
	marker, ok := markers[kind]
	if !ok {
		return nil, fmt.Errorf("unknown audio kind %q", kind)
	}
	start := bytes.Index(indexHTML, []byte(marker))
	if start < 0 {
		return nil, fmt.Errorf("embedded audio %q was not found", kind)
	}
	start += len(marker)
	endOffset := bytes.Index(indexHTML[start:], []byte("');"))
	if endOffset < 0 {
		return nil, fmt.Errorf("embedded audio %q is malformed", kind)
	}
	encoded := bytes.TrimSpace(indexHTML[start : start+endOffset])
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode embedded audio %q: %w", kind, err)
	}
	return decoded, nil
}

func (s *Server) handleBaseRedirect(response http.ResponseWriter, request *http.Request) {
	location := request.URL.Path + "/"
	if request.URL.RawQuery != "" {
		location += "?" + request.URL.RawQuery
	}
	http.Redirect(response, request, location, http.StatusPermanentRedirect)
}

func (s *Server) handleOptions(response http.ResponseWriter, _ *http.Request) {
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUsage(response http.ResponseWriter, request *http.Request) {
	force, err := parseBool(request.URL.Query().Get("force"))
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "force must be true or false"})
		return
	}

	usage, err := s.usage.Get(request.Context(), force)
	if err != nil {
		slog.Error("usage request failed", "error", err)
		writeJSON(response, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, usage)
}

func (s *Server) handleUsageAnalytics(response http.ResponseWriter, request *http.Request) {
	force, err := parseBool(request.URL.Query().Get("force"))
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "force must be true or false"})
		return
	}

	analytics, err := s.usage.GetAnalytics(request.Context(), force)
	if err != nil {
		slog.Error("usage analytics fetch failed", "error", err)
		writeJSON(response, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, analytics)
}

func (s *Server) handlePrediction(response http.ResponseWriter, request *http.Request) {
	force, err := parseBool(request.URL.Query().Get("force"))
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "force must be true or false"})
		return
	}

	prediction, err := s.usage.GetPrediction(request.Context(), force)
	if err != nil {
		slog.Error("reset prediction fetch failed", "error", err)
		writeJSON(response, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, prediction)
}

func (s *Server) handleConfigGet(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.usage.ConfigView())
}

func (s *Server) handleConfigPut(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	var update ConfigUpdate
	if err := json.NewDecoder(io.LimitReader(request.Body, 16<<10)).Decode(&update); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	view, err := s.usage.UpdateConfig(update)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func parseBool(raw string) (bool, error) {
	if strings.TrimSpace(raw) == "" {
		return false, nil
	}
	return strconv.ParseBool(raw)
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	usage, err := NewUsageService(cfg)
	if err != nil {
		slog.Error("failed to initialize usage service", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           NewServer(cfg, usage).handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stopContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("server started", "address", cfg.BindAddr, "cache_ttl", cfg.CacheTTL.String())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped unexpectedly", "error", err)
			stop()
		}
	}()

	<-stopContext.Done()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		slog.Error("server shutdown failed", "error", err)
	}
}
