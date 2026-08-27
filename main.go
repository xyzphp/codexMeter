package main

import (
	"bufio"
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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
)

const (
	defaultBindAddr             = "127.0.0.1:8080"
	defaultUserAgent            = "codex-tui/0.146.0 (Ubuntu 22.4.0; x86_64) xterm-256color"
	defaultCacheTTL             = 10 * time.Minute
	defaultUsageHistoryFile     = "data/usage-history.jsonl"
	maxUsageHistoryPoints       = 48
	upstreamRequestTimeout      = 15 * time.Second
	resetStatusEndpoint         = "https://codex-resets.com/api/v1/status"
	resetHistoryEndpoint        = "https://codex-resets.com/api/resets"
	dailyTokenUsageEndpoint     = "https://chatgpt.com/backend-api/wham/usage/daily-token-usage-breakdown"
	dailyWorkspaceUsageEndpoint = "https://chatgpt.com/backend-api/wham/analytics/daily-workspace-usage-counts"
)

// The two embedded pages intentionally have different layouts: indexHTML is
// the compact LX04 WebView page, while browserHTML is the browser workbench.
// There is no CDN or separate frontend build step.
//
//go:embed web/index.html
var indexHTML []byte

//go:embed web/browser.html
var browserHTML []byte

//go:embed web/settings.html
var settingsHTML []byte

//go:embed web/api-docs.html
var apiDocsHTML []byte

//go:embed openapi.yaml
var openAPISpec []byte

//go:embed web/assets/account-credentials-guide.png
var accountCredentialsGuide []byte

type Config struct {
	BindAddr          string
	BasePath          string
	BasicAuthEnabled  bool
	BasicAuthUsername string
	BasicAuthPassword string
	AccessToken       string
	UpstreamCookie    string
	ChatGPTAccountID  string
	ClientBuildNumber string
	ClientVersion     string
	DeviceID          string
	SessionID         string
	ClientObservation string
	UpstreamReferer   string
	AppAPIKey         string
	UserAgent         string
	UpstreamProxy     string
	CacheTTL          time.Duration
	CORSOrigin        string
	ConfigPath        string
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
	CORSOrigin string `json:"cors_origin"`
	OpenAI     struct {
		AccessToken       string `json:"access_token"`
		Cookie            string `json:"cookie"`
		ChatGPTAccountID  string `json:"chatgpt_account_id"`
		ClientBuildNumber string `json:"client_build_number"`
		ClientVersion     string `json:"client_version"`
		DeviceID          string `json:"device_id"`
		SessionID         string `json:"session_id"`
		ClientObservation string `json:"client_observation"`
		Referer           string `json:"referer"`
		UserAgent         string `json:"user_agent"`
		FedRAMP           bool   `json:"fedramp"`
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
	return loadConfigFile(configPath, configPathFromEnv != "")
}

func loadConfigFile(configPath string, configPathRequired bool) (Config, error) {
	cfg := Config{
		BindAddr:   defaultBindAddr,
		UserAgent:  defaultUserAgent,
		CacheTTL:   defaultCacheTTL,
		ConfigPath: configPath,
	}

	if raw, err := os.ReadFile(configPath); err == nil {
		var stored fileConfig
		if err := json.Unmarshal(raw, &stored); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", configPath, err)
		}
		applyFileConfig(&cfg, stored)
	} else if !os.IsNotExist(err) || configPathRequired {
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
	if stored.OpenAI.FedRAMP {
		cfg.FedRAMP = true
	}
	if stored.OpenAI.AccessToken != "" {
		cfg.AccessToken = strings.TrimSpace(stored.OpenAI.AccessToken)
	}
	if stored.OpenAI.Cookie != "" {
		cfg.UpstreamCookie = strings.TrimSpace(stored.OpenAI.Cookie)
	}
	if stored.OpenAI.ChatGPTAccountID != "" {
		cfg.ChatGPTAccountID = strings.TrimSpace(stored.OpenAI.ChatGPTAccountID)
	}
	if stored.OpenAI.ClientBuildNumber != "" {
		cfg.ClientBuildNumber = strings.TrimSpace(stored.OpenAI.ClientBuildNumber)
	}
	if stored.OpenAI.ClientVersion != "" {
		cfg.ClientVersion = strings.TrimSpace(stored.OpenAI.ClientVersion)
	}
	if stored.OpenAI.DeviceID != "" {
		cfg.DeviceID = strings.TrimSpace(stored.OpenAI.DeviceID)
	}
	if stored.OpenAI.SessionID != "" {
		cfg.SessionID = strings.TrimSpace(stored.OpenAI.SessionID)
	}
	if stored.OpenAI.ClientObservation != "" {
		cfg.ClientObservation = strings.TrimSpace(stored.OpenAI.ClientObservation)
	}
	if stored.OpenAI.Referer != "" {
		cfg.UpstreamReferer = strings.TrimSpace(stored.OpenAI.Referer)
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
	overrideString(&cfg.UpstreamCookie, "OPENAI_COOKIE")
	overrideString(&cfg.ChatGPTAccountID, "CHATGPT_ACCOUNT_ID")
	overrideString(&cfg.ClientBuildNumber, "OPENAI_CLIENT_BUILD_NUMBER")
	overrideString(&cfg.ClientVersion, "OPENAI_CLIENT_VERSION")
	overrideString(&cfg.DeviceID, "OPENAI_DEVICE_ID")
	overrideString(&cfg.SessionID, "OPENAI_SESSION_ID")
	overrideString(&cfg.ClientObservation, "OPENAI_CLIENT_OBSERVATION")
	overrideString(&cfg.UpstreamReferer, "OPENAI_REFERER")
	overrideString(&cfg.AppAPIKey, "APP_API_KEY")
	overrideString(&cfg.BasicAuthUsername, "BASIC_AUTH_USER")
	overrideString(&cfg.BasicAuthPassword, "BASIC_AUTH_PASSWORD")
	overrideString(&cfg.UserAgent, "OPENAI_USER_AGENT")
	overrideString(&cfg.UpstreamProxy, "UPSTREAM_PROXY")
	overrideString(&cfg.CORSOrigin, "CORS_ORIGIN")
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
	At                  string   `json:"at"`
	UsedPercent         float64  `json:"used_percent"`
	FiveHourUsedPercent *float64 `json:"five_hour_used_percent,omitempty"`
}

// UsageAnalytics is the compact date-range payload exposed to the frontend.
// The upstream responses contain a longer history and several nested
// breakdowns; keeping only the fields needed by the dashboard makes the
// Android WebView page faster and avoids exposing the raw upstream payload.
type UsageAnalytics struct {
	Source    string                `json:"source"`
	FetchedAt string                `json:"fetched_at"`
	FromCache bool                  `json:"from_cache"`
	StartDate string                `json:"start_date"`
	EndDate   string                `json:"end_date"`
	Days      []UsageAnalyticsDay   `json:"days"`
	Summary   UsageAnalyticsSummary `json:"summary"`
}

const analyticsDateLayout = "2006-01-02"

type analyticsDateRange struct {
	StartDate string
	EndDate   string
}

func previousAnalyticsDateRange(now time.Time, days int) analyticsDateRange {
	if days < 1 {
		days = 1
	}
	return analyticsDateRange{
		StartDate: now.AddDate(0, 0, -days).Format(analyticsDateLayout),
		EndDate:   now.AddDate(0, 0, -1).Format(analyticsDateLayout),
	}
}

func parseAnalyticsDateRange(values url.Values, now time.Time) (analyticsDateRange, error) {
	startRaw := strings.TrimSpace(values.Get("start_date"))
	endRaw := strings.TrimSpace(values.Get("end_date"))
	if startRaw == "" && endRaw == "" {
		// Keep the legacy API default for the LX04 page. The browser page sends
		// its one-year range explicitly so both layouts can coexist.
		return previousAnalyticsDateRange(now, 7), nil
	}
	if startRaw == "" || endRaw == "" {
		return analyticsDateRange{}, errors.New("start_date and end_date must be provided together")
	}
	start, err := time.ParseInLocation(analyticsDateLayout, startRaw, now.Location())
	if err != nil {
		return analyticsDateRange{}, errors.New("start_date must use YYYY-MM-DD")
	}
	end, err := time.ParseInLocation(analyticsDateLayout, endRaw, now.Location())
	if err != nil {
		return analyticsDateRange{}, errors.New("end_date must use YYYY-MM-DD")
	}
	if start.After(end) {
		return analyticsDateRange{}, errors.New("start_date must not be after end_date")
	}
	if end.After(now) {
		return analyticsDateRange{}, errors.New("end_date must not be in the future")
	}
	if end.After(start.AddDate(0, 0, 366)) {
		return analyticsDateRange{}, errors.New("date range cannot exceed 367 days")
	}
	return analyticsDateRange{StartDate: startRaw, EndDate: endRaw}, nil
}

func (r analyticsDateRange) key() string {
	return r.StartDate + ":" + r.EndDate
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
	Source      string       `json:"source"`
	FetchedAt   string       `json:"fetched_at"`
	FromCache   bool         `json:"from_cache"`
	LatestReset *ResetEvent  `json:"latest_reset,omitempty"`
	ActiveWatch *ResetWatch  `json:"active_watch,omitempty"`
	History     []ResetEvent `json:"history,omitempty"`
	Stats       ResetStats   `json:"stats"`
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

type resetHistoryEnvelope struct {
	Events []resetHistoryEvent `json:"events"`
}

type resetHistoryEvent struct {
	TweetID     string `json:"tweet_id"`
	TweetURL    string `json:"tweet_url"`
	Text        string `json:"text"`
	AnnouncedAt string `json:"announced_at"`
	ResetType   string `json:"reset_type"`
	Source      string `json:"source"`
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
	analyticsCachedKey string
	historyFile        string
}

func NewUsageService(cfg Config) (*UsageService, error) {
	client, err := newUpstreamClient(cfg.UpstreamProxy)
	if err != nil {
		return nil, err
	}
	history, err := loadUsageHistory(defaultUsageHistoryFile)
	if err != nil {
		return nil, fmt.Errorf("load usage history: %w", err)
	}
	return &UsageService{
		cfg:         cfg,
		client:      client,
		history:     history,
		historyFile: defaultUsageHistoryFile,
	}, nil
}

func newUpstreamClient(proxyURL string) (*http.Client, error) {
	parsedProxy, err := parseProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
	if parsedProxy == nil {
		transport.Proxy = http.ProxyFromEnvironment
	} else if isSOCKS5Proxy(parsedProxy) {
		var auth *proxy.Auth
		if parsedProxy.User != nil {
			password, _ := parsedProxy.User.Password()
			auth = &proxy.Auth{User: parsedProxy.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", parsedProxy.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("invalid SOCKS5 proxy: %w", err)
		}
		// http.Transport uses Dial when DialContext is nil. The transport's
		// request timeout still bounds the overall upstream operation.
		transport.Dial = dialer.Dial
	} else {
		transport.Proxy = http.ProxyURL(parsedProxy)
	}
	return &http.Client{Transport: transport, Timeout: upstreamRequestTimeout}, nil
}

func buildProxyFunc(raw string) (func(*http.Request) (*url.URL, error), error) {
	proxyURL, err := parseProxyURL(raw)
	if err != nil {
		return nil, err
	}
	if proxyURL == nil {
		return http.ProxyFromEnvironment, nil
	}
	if isSOCKS5Proxy(proxyURL) {
		// SOCKS5 is installed through Transport.Dial, not Transport.Proxy.
		// Return a no-op proxy function here so callers can still use this
		// helper for URL validation.
		return func(*http.Request) (*url.URL, error) { return nil, nil }, nil
	}
	return http.ProxyURL(proxyURL), nil
}

func parseProxyURL(raw string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	proxyURL, err := url.Parse(value)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("invalid UPSTREAM_PROXY: use http://, https:// or socks5://host:port")
	}
	proxyURL.Scheme = strings.ToLower(proxyURL.Scheme)
	switch proxyURL.Scheme {
	case "http", "https":
		return proxyURL, nil
	case "socks5", "socks5h", "socket5":
		proxyURL.Scheme = "socks5"
		return proxyURL, nil
	default:
		return nil, fmt.Errorf("invalid UPSTREAM_PROXY scheme %q: use http://, https:// or socks5://host:port", proxyURL.Scheme)
	}
}

func isSOCKS5Proxy(proxyURL *url.URL) bool {
	if proxyURL == nil {
		return false
	}
	return strings.EqualFold(proxyURL.Scheme, "socks5") || strings.EqualFold(proxyURL.Scheme, "socks5h") || strings.EqualFold(proxyURL.Scheme, "socket5")
}

func (s *UsageService) Get(ctx context.Context, force bool) (*UsageResponse, error) {
	requestStartedAt := time.Now()
	if !force {
		if cached := s.getFreshCache(); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	}

	// Serialize upstream usage requests and check the cache again after waiting.
	// A forced request that was already in flight when this request started can
	// reuse its result, preventing duplicate samples from concurrent WebViews.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	if !force {
		if cached := s.getFreshCache(); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	} else if cached := s.getCacheUpdatedAfter(requestStartedAt); cached != nil {
		cached.FromCache = true
		return cached, nil
	}

	usage, err := s.queryWhamUsage(ctx, s.currentConfig())
	if err != nil {
		return nil, err
	}

	var latestHistoryPoint HistoryPoint
	hasLatestHistoryPoint := false
	s.cacheMu.Lock()
	if usage.SevenDay != nil {
		latestHistoryPoint = HistoryPoint{
			At:          usage.FetchedAt,
			UsedPercent: usage.SevenDay.UsedPercent,
		}
		if usage.FiveHour != nil {
			fiveHourUsedPercent := usage.FiveHour.UsedPercent
			latestHistoryPoint.FiveHourUsedPercent = &fiveHourUsedPercent
		}
		hasLatestHistoryPoint = true
		s.history = append(s.history, latestHistoryPoint)
		if len(s.history) > maxUsageHistoryPoints {
			s.history = s.history[len(s.history)-maxUsageHistoryPoints:]
		}
	}
	usage.History = append([]HistoryPoint(nil), s.history...)
	s.cached = cloneUsage(usage)
	s.cachedAt = time.Now()
	s.cacheMu.Unlock()
	if s.historyFile != "" && hasLatestHistoryPoint {
		if err := appendUsageHistory(s.historyFile, latestHistoryPoint); err != nil {
			slog.Warn("persist usage history failed", "error", err)
		}
	}
	return cloneUsage(usage), nil
}

func loadUsageHistory(path string) ([]HistoryPoint, error) {
	raw, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer raw.Close()

	history := make([]HistoryPoint, 0, maxUsageHistoryPoints)
	scanner := bufio.NewScanner(raw)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var point HistoryPoint
		if err := json.Unmarshal(line, &point); err != nil {
			continue
		}
		if strings.TrimSpace(point.At) == "" || point.UsedPercent < 0 || point.UsedPercent > 100 {
			continue
		}
		if point.FiveHourUsedPercent != nil && (*point.FiveHourUsedPercent < 0 || *point.FiveHourUsedPercent > 100) {
			continue
		}
		if _, err := time.Parse(time.RFC3339, point.At); err != nil {
			continue
		}
		history = append(history, point)
		if len(history) > maxUsageHistoryPoints {
			history = history[len(history)-maxUsageHistoryPoints:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return history, nil
}

func appendUsageHistory(path string, point HistoryPoint) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("usage history path is empty")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(point); err != nil {
		return err
	}
	return nil
}

func (s *UsageService) GetAnalytics(ctx context.Context, force bool) (*UsageAnalytics, error) {
	return s.GetAnalyticsRange(ctx, force, previousAnalyticsDateRange(time.Now(), 7))
}

func (s *UsageService) GetAnalyticsRange(ctx context.Context, force bool, dateRange analyticsDateRange) (*UsageAnalytics, error) {
	cacheKey := dateRange.key()
	if !force {
		if cached := s.getFreshAnalyticsCache(cacheKey); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	}

	s.analyticsRefreshMu.Lock()
	defer s.analyticsRefreshMu.Unlock()

	if !force {
		if cached := s.getFreshAnalyticsCache(cacheKey); cached != nil {
			cached.FromCache = true
			return cached, nil
		}
	}

	analytics, err := s.queryUsageAnalyticsRange(ctx, dateRange)
	if err != nil {
		return nil, err
	}

	s.cacheMu.Lock()
	s.analyticsCached = cloneUsageAnalytics(analytics)
	s.analyticsCachedAt = time.Now()
	s.analyticsCachedKey = cacheKey
	s.cacheMu.Unlock()
	return cloneUsageAnalytics(analytics), nil
}

func (s *UsageService) queryUsageAnalytics(ctx context.Context) (*UsageAnalytics, error) {
	return s.queryUsageAnalyticsRange(ctx, previousAnalyticsDateRange(time.Now(), 7))
}

func (s *UsageService) queryUsageAnalyticsRange(ctx context.Context, dateRange analyticsDateRange) (*UsageAnalytics, error) {
	cfg := s.currentConfig()
	tokenEndpoint, err := dailyUsageEndpoint(dailyTokenUsageEndpoint, dateRange, false)
	if err != nil {
		return nil, fmt.Errorf("build daily token usage request: %w", err)
	}
	var tokenUsage dailyTokenUsageEnvelope
	if err := s.queryWhamJSON(ctx, cfg, tokenEndpoint, &tokenUsage); err != nil {
		return nil, fmt.Errorf("daily token usage request failed: %w", err)
	}

	workspaceEndpoint, err := dailyUsageEndpoint(dailyWorkspaceUsageEndpoint, dateRange, true)
	if err != nil {
		return nil, fmt.Errorf("build daily workspace usage request: %w", err)
	}
	var workspaceUsage dailyWorkspaceUsageEnvelope
	if err := s.queryWhamJSON(ctx, cfg, workspaceEndpoint, &workspaceUsage); err != nil {
		return nil, fmt.Errorf("daily workspace usage request failed: %w", err)
	}

	return mergeUsageAnalyticsAtRange(tokenUsage.Data, workspaceUsage.Data, dateRange, time.Now()), nil
}

func mergeUsageAnalytics(tokenUsage []dailyTokenUsagePoint, workspaceUsage []dailyWorkspaceUsagePoint) *UsageAnalytics {
	return mergeUsageAnalyticsAtRange(tokenUsage, workspaceUsage, previousAnalyticsDateRange(time.Now(), 7), time.Now())
}

func mergeUsageAnalyticsAt(tokenUsage []dailyTokenUsagePoint, workspaceUsage []dailyWorkspaceUsagePoint, now time.Time) *UsageAnalytics {
	return mergeUsageAnalyticsAtRange(tokenUsage, workspaceUsage, previousAnalyticsDateRange(now, 7), now)
}

func mergeUsageAnalyticsAtRange(tokenUsage []dailyTokenUsagePoint, workspaceUsage []dailyWorkspaceUsagePoint, dateRange analyticsDateRange, now time.Time) *UsageAnalytics {
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

	start, startErr := time.ParseInLocation(analyticsDateLayout, dateRange.StartDate, now.Location())
	end, endErr := time.ParseInLocation(analyticsDateLayout, dateRange.EndDate, now.Location())
	if startErr != nil || endErr != nil || start.After(end) {
		return &UsageAnalytics{
			Source:    "wham_daily_usage",
			FetchedAt: time.Now().UTC().Format(time.RFC3339),
			StartDate: dateRange.StartDate,
			EndDate:   dateRange.EndDate,
			Days:      []UsageAnalyticsDay{},
		}
	}

	dates := make([]string, 0, int(end.Sub(start)/(24*time.Hour))+1)
	for current := start; !current.After(end); current = current.AddDate(0, 0, 1) {
		dates = append(dates, current.Format(analyticsDateLayout))
	}

	result := &UsageAnalytics{
		Source:    "wham_daily_usage",
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		StartDate: dateRange.StartDate,
		EndDate:   dateRange.EndDate,
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

func dailyUsageEndpoint(endpoint string, dateRange analyticsDateRange, workspaceUser bool) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("start_date", dateRange.StartDate)
	query.Set("end_date", dateRange.EndDate)
	query.Set("group_by", "day")
	if workspaceUser {
		query.Set("workspace_user", "true")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
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

	// These headers mirror the browser request context captured from ChatGPT.
	// The target path is derived per endpoint so analytics requests do not send
	// the path of the primary usage endpoint.
	request.Host = "chatgpt.com"
	request.Header.Set("Authorization", "Bearer "+cfg.AccessToken)
	if cfg.UpstreamCookie != "" {
		request.Header.Set("Cookie", cfg.UpstreamCookie)
	}
	if cfg.ClientBuildNumber != "" {
		request.Header.Set("oai-client-build-number", cfg.ClientBuildNumber)
	}
	if cfg.ClientVersion != "" {
		request.Header.Set("oai-client-version", cfg.ClientVersion)
	}
	if cfg.DeviceID != "" {
		request.Header.Set("oai-device-id", cfg.DeviceID)
	}
	if cfg.SessionID != "" {
		request.Header.Set("oai-session-id", cfg.SessionID)
	}
	if cfg.ClientObservation != "" {
		request.Header.Set("x-oai-is-client-observation", cfg.ClientObservation)
	}
	request.Header.Set("x-openai-target-path", request.URL.Path)
	request.Header.Set("x-openai-target-route", request.URL.Path)
	request.Header.Set("oai-language", "zh-CN")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Priority", "u=1, i")
	if cfg.UpstreamReferer != "" {
		request.Header.Set("Referer", cfg.UpstreamReferer)
	}
	request.Header.Set("User-Agent", cfg.UserAgent)
	if cfg.FedRAMP {
		request.Header.Set("x-openai-fedramp", "true")
	}
	return request, cancel, nil
}

func (s *UsageService) queryWhamJSON(ctx context.Context, cfg Config, endpoint string, target any) error {
	return s.queryWhamJSONWithClient(ctx, cfg, endpoint, target, s.currentClient())
}

func (s *UsageService) queryWhamJSONWithClient(ctx context.Context, cfg Config, endpoint string, target any, client *http.Client) error {
	request, cancel, err := newWhamRequest(ctx, endpoint, cfg)
	if err != nil {
		return err
	}
	defer cancel()
	response, err := client.Do(request)
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
	var envelope resetStatusEnvelope
	if err := s.queryPublicResetJSON(ctx, resetStatusEndpoint, &envelope); err != nil {
		return nil, fmt.Errorf("reset status request failed: %w", err)
	}

	var historyEnvelope resetHistoryEnvelope
	if err := s.queryPublicResetJSON(ctx, resetHistoryEndpoint, &historyEnvelope); err != nil {
		return nil, fmt.Errorf("reset history request failed: %w", err)
	}
	history := normalizeResetHistory(historyEnvelope.Events)
	fetchedAt := envelope.Meta.GeneratedAt
	if fetchedAt == "" {
		fetchedAt = time.Now().UTC().Format(time.RFC3339)
	}
	latest := envelope.Data.LatestReset
	if latest == nil && len(history) > 0 {
		latest = &history[0]
	}
	stats := envelope.Data.Stats
	if stats.Total == 0 && len(history) > 0 {
		stats.Total = len(history)
	}
	return &ResetPrediction{
		Source:      "codex_resets_status",
		FetchedAt:   fetchedAt,
		LatestReset: latest,
		ActiveWatch: envelope.Data.ActiveWatch,
		History:     history,
		Stats:       stats,
	}, nil
}

func (s *UsageService) queryPublicResetJSON(ctx context.Context, endpoint string, target any) error {
	requestCtx, cancel := context.WithTimeout(ctx, upstreamRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create public reset request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "codex-usage-dashboard/1.0")

	response, err := s.currentClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("upstream returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func normalizeResetHistory(events []resetHistoryEvent) []ResetEvent {
	result := make([]ResetEvent, 0, len(events))
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		id := strings.TrimSpace(event.TweetID)
		if id == "" {
			id = strings.TrimSpace(event.TweetURL)
		}
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}

		reset := ResetEvent{
			ID:          id,
			ResetType:   strings.TrimSpace(event.ResetType),
			AnnouncedAt: strings.TrimSpace(event.AnnouncedAt),
			Text:        event.Text,
		}
		if event.Source != "" || event.TweetURL != "" {
			reset.Source = &ResetSource{
				Type: strings.TrimSpace(event.Source),
				URL:  strings.TrimSpace(event.TweetURL),
			}
		}
		result = append(result, reset)
	}
	return result
}

type ConfigView struct {
	AppAPIKeyConfigured         bool   `json:"app_api_key_configured"`
	AppAPIKeyHint               string `json:"app_api_key_hint,omitempty"`
	BasicAuthEnabled            bool   `json:"basic_auth_enabled"`
	BasicAuthUsername           string `json:"basic_auth_username,omitempty"`
	BasicAuthPasswordConfigured bool   `json:"basic_auth_password_configured"`
	ChatGPTAccountID            string `json:"chatgpt_account_id"`
	UserAgent                   string `json:"user_agent"`
	FedRAMP                     bool   `json:"fedramp"`
	TokenConfigured             bool   `json:"token_configured"`
	TokenHint                   string `json:"token_hint,omitempty"`
	CookieConfigured            bool   `json:"cookie_configured"`
	CookieHint                  string `json:"cookie_hint,omitempty"`
	ClientBuildNumber           string `json:"client_build_number,omitempty"`
	ClientVersion               string `json:"client_version,omitempty"`
	DeviceID                    string `json:"device_id,omitempty"`
	SessionID                   string `json:"session_id,omitempty"`
	ClientObservation           string `json:"client_observation,omitempty"`
	Referer                     string `json:"referer,omitempty"`
	ProxyURL                    string `json:"proxy_url,omitempty"`
	CacheTTL                    string `json:"cache_ttl"`
	ConfigFile                  string `json:"config_file"`
}

const maxConfigFileSize = 256 << 10

type ConfigFileView struct {
	ConfigFile string `json:"config_file"`
	Content    string `json:"content"`
}

type ConfigFileUpdate struct {
	Content string `json:"content"`
}

func secretHint(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) < 4 {
		return ""
	}
	return "****" + raw[len(raw)-4:]
}

type ConfigUpdate struct {
	AppAPIKey         *string `json:"app_api_key"`
	AccessToken       *string `json:"access_token"`
	UpstreamCookie    *string `json:"cookie"`
	ChatGPTAccountID  *string `json:"chatgpt_account_id"`
	ClientBuildNumber *string `json:"client_build_number"`
	ClientVersion     *string `json:"client_version"`
	DeviceID          *string `json:"device_id"`
	SessionID         *string `json:"session_id"`
	ClientObservation *string `json:"client_observation"`
	Referer           *string `json:"referer"`
	UserAgent         *string `json:"user_agent"`
	FedRAMP           *bool   `json:"fedramp"`
	ProxyURL          *string `json:"proxy_url"`
	CacheTTL          *string `json:"cache_ttl"`
	BasicAuthEnabled  *bool   `json:"basic_auth_enabled"`
	BasicAuthUsername *string `json:"basic_auth_username"`
	BasicAuthPassword *string `json:"basic_auth_password"`
}

type ConfigTestResult struct {
	OK               bool   `json:"ok"`
	Message          string `json:"message"`
	StatusCode       int    `json:"status_code"`
	Email            string `json:"email,omitempty"`
	PlanType         string `json:"plan_type,omitempty"`
	TokenConfigured  bool   `json:"token_configured"`
	CookieConfigured bool   `json:"cookie_configured"`
}

type ProxyTestRequest struct {
	ProxyURL string `json:"proxy_url"`
}

type ProxyTestResult struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	StatusCode int    `json:"status_code"`
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
	tokenHint := secretHint(cfg.AccessToken)
	return ConfigView{
		AppAPIKeyConfigured:         cfg.AppAPIKey != "",
		AppAPIKeyHint:               secretHint(cfg.AppAPIKey),
		BasicAuthEnabled:            cfg.BasicAuthEnabled,
		BasicAuthUsername:           cfg.BasicAuthUsername,
		BasicAuthPasswordConfigured: cfg.BasicAuthPassword != "",
		ChatGPTAccountID:            cfg.ChatGPTAccountID,
		UserAgent:                   cfg.UserAgent,
		FedRAMP:                     cfg.FedRAMP,
		TokenConfigured:             cfg.AccessToken != "",
		TokenHint:                   tokenHint,
		CookieConfigured:            cfg.UpstreamCookie != "",
		CookieHint:                  cookieHint(cfg.UpstreamCookie),
		ClientBuildNumber:           cfg.ClientBuildNumber,
		ClientVersion:               cfg.ClientVersion,
		DeviceID:                    cfg.DeviceID,
		SessionID:                   cfg.SessionID,
		ClientObservation:           cfg.ClientObservation,
		Referer:                     cfg.UpstreamReferer,
		ProxyURL:                    cfg.UpstreamProxy,
		CacheTTL:                    cfg.CacheTTL.String(),
		ConfigFile:                  cfg.ConfigPath,
	}
}

func (s *UsageService) UpdateConfig(update ConfigUpdate) (ConfigView, error) {
	old := s.currentConfig()
	next, err := applyConfigUpdate(old, update)
	if err != nil {
		return ConfigView{}, err
	}
	if _, err := buildProxyFunc(next.UpstreamProxy); err != nil {
		return ConfigView{}, err
	}
	if err := persistConfig(next); err != nil {
		return ConfigView{}, err
	}
	if err := s.activateConfig(old, next); err != nil {
		return ConfigView{}, err
	}
	return s.ConfigView(), nil
}

func (s *UsageService) activateConfig(old, next Config) error {
	if next.UpstreamProxy != old.UpstreamProxy {
		client, err := newUpstreamClient(next.UpstreamProxy)
		if err != nil {
			return err
		}
		s.clientMu.Lock()
		s.client = client
		s.clientMu.Unlock()
	}

	s.cfgMu.Lock()
	s.cfg = next
	s.cfgMu.Unlock()

	// A changed credential, request context, proxy, or cache policy must not
	// reuse an earlier account snapshot.
	s.cacheMu.Lock()
	s.cached = nil
	s.cachedAt = time.Time{}
	s.history = nil
	s.resetCached = nil
	s.resetCachedAt = time.Time{}
	s.analyticsCached = nil
	s.analyticsCachedAt = time.Time{}
	s.analyticsCachedKey = ""
	s.cacheMu.Unlock()
	return nil
}

func applyConfigUpdate(old Config, update ConfigUpdate) (Config, error) {
	next := old
	if update.AppAPIKey != nil {
		next.AppAPIKey = strings.TrimSpace(*update.AppAPIKey)
	}
	if update.AccessToken != nil {
		next.AccessToken = strings.TrimSpace(*update.AccessToken)
	}
	if update.UpstreamCookie != nil {
		next.UpstreamCookie = strings.TrimSpace(*update.UpstreamCookie)
	}
	if update.ChatGPTAccountID != nil {
		next.ChatGPTAccountID = strings.TrimSpace(*update.ChatGPTAccountID)
	}
	if update.ClientBuildNumber != nil {
		next.ClientBuildNumber = strings.TrimSpace(*update.ClientBuildNumber)
	}
	if update.ClientVersion != nil {
		next.ClientVersion = strings.TrimSpace(*update.ClientVersion)
	}
	if update.DeviceID != nil {
		next.DeviceID = strings.TrimSpace(*update.DeviceID)
	}
	if update.SessionID != nil {
		next.SessionID = strings.TrimSpace(*update.SessionID)
	}
	if update.ClientObservation != nil {
		next.ClientObservation = strings.TrimSpace(*update.ClientObservation)
	}
	if update.Referer != nil {
		next.UpstreamReferer = strings.TrimSpace(*update.Referer)
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
			return Config{}, fmt.Errorf("invalid cache_ttl")
		}
		next.CacheTTL = ttl
	}
	if update.BasicAuthEnabled != nil {
		next.BasicAuthEnabled = *update.BasicAuthEnabled
	}
	if update.BasicAuthUsername != nil {
		next.BasicAuthUsername = strings.TrimSpace(*update.BasicAuthUsername)
	}
	if update.BasicAuthPassword != nil {
		next.BasicAuthPassword = *update.BasicAuthPassword
	}
	if next.AccessToken == "" {
		return Config{}, errors.New("access_token cannot be empty")
	}
	if next.ChatGPTAccountID == "" {
		return Config{}, errors.New("chatgpt_account_id cannot be empty")
	}
	if next.BasicAuthEnabled && (next.BasicAuthUsername == "" || next.BasicAuthPassword == "") {
		return Config{}, errors.New("basic_auth_username and basic_auth_password are required when Basic Auth is enabled")
	}
	return next, nil
}

func cookieHint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	count := strings.Count(raw, ";") + 1
	return fmt.Sprintf("已配置（%d 项）", count)
}

func (s *UsageService) TestConfig(ctx context.Context, update ConfigUpdate) (ConfigTestResult, error) {
	current := s.currentConfig()
	draft, err := applyConfigUpdate(current, update)
	if err != nil {
		return ConfigTestResult{}, err
	}
	client, err := newUpstreamClient(draft.UpstreamProxy)
	if err != nil {
		return ConfigTestResult{}, err
	}
	var upstream whamUsageResponse
	if err := s.queryWhamJSONWithClient(ctx, draft, "https://chatgpt.com/backend-api/wham/usage", &upstream, client); err != nil {
		return ConfigTestResult{}, fmt.Errorf("连接 OpenAI 失败：%w", err)
	}
	rateLimit := upstream.RateLimit
	if rateLimit == nil || (rateLimit.PrimaryWindow == nil && rateLimit.SecondaryWindow == nil) {
		for _, additional := range upstream.AdditionalRateLimits {
			if additional.MeteredFeature == "codex_bengalfox" && additional.RateLimit != nil {
				rateLimit = additional.RateLimit
				break
			}
		}
	}
	if rateLimit == nil || (rateLimit.PrimaryWindow == nil && rateLimit.SecondaryWindow == nil) {
		return ConfigTestResult{}, errors.New("上游响应成功，但没有找到可用的额度窗口")
	}
	return ConfigTestResult{
		OK:               true,
		Message:          "连接成功，已读取额度数据",
		StatusCode:       http.StatusOK,
		Email:            upstream.Email,
		PlanType:         upstream.PlanType,
		TokenConfigured:  draft.AccessToken != "",
		CookieConfigured: draft.UpstreamCookie != "",
	}, nil
}

func (s *UsageService) TestProxy(ctx context.Context, rawProxyURL string) (ProxyTestResult, error) {
	client, err := newUpstreamClient(rawProxyURL)
	if err != nil {
		return ProxyTestResult{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/", nil)
	if err != nil {
		return ProxyTestResult{}, fmt.Errorf("create proxy test request: %w", err)
	}
	request.Header.Set("User-Agent", s.currentConfig().UserAgent)

	response, err := client.Do(request)
	if err != nil {
		return ProxyTestResult{}, fmt.Errorf("代理连接失败：%w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))

	if response.StatusCode >= http.StatusInternalServerError {
		return ProxyTestResult{}, fmt.Errorf("代理已连通，但上游返回 HTTP %d", response.StatusCode)
	}

	message := proxyTestSuccessMessage(rawProxyURL)
	return ProxyTestResult{
		OK:         true,
		Message:    message,
		StatusCode: response.StatusCode,
	}, nil
}

func proxyTestSuccessMessage(rawProxyURL string) string {
	if strings.TrimSpace(rawProxyURL) == "" {
		return "直连成功，可访问 chatgpt.com"
	}
	return "代理连接成功，可访问 chatgpt.com"
}

func fileConfigFromConfig(cfg Config) fileConfig {
	stored := fileConfig{
		BindAddr:   cfg.BindAddr,
		BasePath:   cfg.BasePath,
		AppAPIKey:  cfg.AppAPIKey,
		CacheTTL:   cfg.CacheTTL.String(),
		CORSOrigin: cfg.CORSOrigin,
	}
	stored.BasicAuth.Enabled = cfg.BasicAuthEnabled
	stored.BasicAuth.Username = cfg.BasicAuthUsername
	stored.BasicAuth.Password = cfg.BasicAuthPassword
	stored.OpenAI.AccessToken = cfg.AccessToken
	stored.OpenAI.Cookie = cfg.UpstreamCookie
	stored.OpenAI.ChatGPTAccountID = cfg.ChatGPTAccountID
	stored.OpenAI.ClientBuildNumber = cfg.ClientBuildNumber
	stored.OpenAI.ClientVersion = cfg.ClientVersion
	stored.OpenAI.DeviceID = cfg.DeviceID
	stored.OpenAI.SessionID = cfg.SessionID
	stored.OpenAI.ClientObservation = cfg.ClientObservation
	stored.OpenAI.Referer = cfg.UpstreamReferer
	stored.OpenAI.UserAgent = cfg.UserAgent
	stored.OpenAI.FedRAMP = cfg.FedRAMP
	stored.Proxy.URL = cfg.UpstreamProxy
	return stored
}

func marshalConfig(cfg Config) ([]byte, error) {
	stored := fileConfigFromConfig(cfg)
	raw, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return append(raw, '\n'), nil
}

func persistConfig(cfg Config) error {
	raw, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfg.ConfigPath, raw, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func (s *UsageService) ReadConfigFile() (ConfigFileView, error) {
	cfg := s.currentConfig()
	raw, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return ConfigFileView{}, fmt.Errorf("read config: %w", err)
		}
		raw, err = marshalConfig(cfg)
		if err != nil {
			return ConfigFileView{}, err
		}
	}
	return ConfigFileView{ConfigFile: cfg.ConfigPath, Content: string(raw)}, nil
}

func (s *UsageService) UpdateConfigFile(content string) (ConfigFileView, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return ConfigFileView{}, errors.New("config content cannot be empty")
	}
	if len([]byte(content)) > maxConfigFileSize {
		return ConfigFileView{}, fmt.Errorf("config content exceeds %d bytes", maxConfigFileSize)
	}

	var stored fileConfig
	if err := json.Unmarshal([]byte(content), &stored); err != nil {
		return ConfigFileView{}, fmt.Errorf("invalid config JSON: %w", err)
	}

	current := s.currentConfig()
	temp, err := os.CreateTemp("", "codex-usage-config-*.json")
	if err != nil {
		return ConfigFileView{}, fmt.Errorf("create config validation file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.WriteString(content + "\n"); err != nil {
		_ = temp.Close()
		return ConfigFileView{}, fmt.Errorf("write config validation file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return ConfigFileView{}, fmt.Errorf("close config validation file: %w", err)
	}

	next, err := loadConfigFile(tempPath, true)
	if err != nil {
		return ConfigFileView{}, err
	}
	next.ConfigPath = current.ConfigPath
	if _, err := buildProxyFunc(next.UpstreamProxy); err != nil {
		return ConfigFileView{}, err
	}
	if err := os.WriteFile(current.ConfigPath, []byte(content+"\n"), 0o600); err != nil {
		return ConfigFileView{}, fmt.Errorf("write config: %w", err)
	}
	if err := s.activateConfig(current, next); err != nil {
		return ConfigFileView{}, err
	}
	return s.ReadConfigFile()
}

func (s *UsageService) getFreshCache() *UsageResponse {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cached == nil || time.Since(s.cachedAt) >= s.currentConfig().CacheTTL {
		return nil
	}
	return cloneUsage(s.cached)
}

func (s *UsageService) getCacheUpdatedAfter(startedAt time.Time) *UsageResponse {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cached == nil || !s.cachedAt.After(startedAt) {
		return nil
	}
	return cloneUsage(s.cached)
}

func (s *UsageService) getFreshAnalyticsCache(cacheKey string) *UsageAnalytics {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.analyticsCached == nil || s.analyticsCachedKey != cacheKey || time.Since(s.analyticsCachedAt) >= s.currentConfig().CacheTTL {
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
	if input.History != nil {
		output.History = make([]ResetEvent, len(input.History))
		for index, inputEvent := range input.History {
			output.History[index] = inputEvent
			if inputEvent.Source != nil {
				source := *inputEvent.Source
				output.History[index].Source = &source
			}
		}
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
	mux.HandleFunc("GET "+route("/api-docs"), s.handleAPIDocs)
	mux.HandleFunc("GET "+route("/api-docs/"), s.handleAPIDocs)
	mux.HandleFunc("GET "+route("/openapi.yaml"), s.handleOpenAPISpec)
	mux.HandleFunc("GET "+route("/assets/account-credentials-guide.png"), s.handleCredentialGuide)
	mux.HandleFunc("GET "+route("/audio"), s.handleAudio)
	mux.HandleFunc("GET "+route("/api/usage"), s.handleUsage)
	mux.HandleFunc("GET "+route("/api/usage/analytics"), s.handleUsageAnalytics)
	mux.HandleFunc("GET "+route("/api/prediction"), s.handlePrediction)
	mux.HandleFunc("GET "+route("/api/config"), s.handleConfigGet)
	mux.HandleFunc("GET "+route("/api/config/app-key"), s.handleConfigAppKey)
	mux.HandleFunc("GET "+route("/api/config/file"), s.handleConfigFileGet)
	mux.HandleFunc("POST "+route("/api/config/test"), s.handleConfigTest)
	mux.HandleFunc("POST "+route("/api/config/test-proxy"), s.handleProxyTest)
	mux.HandleFunc("PUT "+route("/api/config"), s.handleConfigPut)
	mux.HandleFunc("PUT "+route("/api/config/file"), s.handleConfigFilePut)
	mux.HandleFunc("OPTIONS "+route("/api/usage"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/usage/analytics"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/prediction"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config/app-key"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config/file"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config/test"), s.handleOptions)
	mux.HandleFunc("OPTIONS "+route("/api/config/test-proxy"), s.handleOptions)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		middlewareConfig := s.cfg
		if s.usage != nil {
			middlewareConfig = s.usage.currentConfig()
		}
		if middlewareConfig.CORSOrigin != "" {
			response.Header().Set("Access-Control-Allow-Origin", middlewareConfig.CORSOrigin)
			response.Header().Set("Vary", "Origin")
			response.Header().Set("Access-Control-Allow-Headers", "Authorization, X-App-API-Key, Content-Type")
			response.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		}
		if middlewareConfig.BasicAuthEnabled && !authorizedBasic(request, middlewareConfig.BasicAuthUsername, middlewareConfig.BasicAuthPassword) {
			response.Header().Set("WWW-Authenticate", `Basic realm="Codex Usage"`)
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s.isConfigFilePath(request.URL.Path) && !middlewareConfig.BasicAuthEnabled && middlewareConfig.AppAPIKey == "" {
			writeJSON(response, http.StatusForbidden, map[string]string{"error": "config file access requires Basic Auth or App API Key"})
			return
		}
		// Basic Auth is the page/API authentication mechanism. If it is enabled
		// and already passed, do not require a second app API key as well. The
		// app key remains available for deployments that do not use Basic Auth.
		appKeyRequired := s.isAPIPath(request.URL.Path) && middlewareConfig.AppAPIKey != ""
		basicAuthPassed := middlewareConfig.BasicAuthEnabled && authorizedBasic(request, middlewareConfig.BasicAuthUsername, middlewareConfig.BasicAuthPassword)
		if appKeyRequired && !basicAuthPassed && !authorized(request, middlewareConfig.AppAPIKey) {
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

func (s *Server) isConfigFilePath(requestPath string) bool {
	if requestPath == "/api/config/file" {
		return true
	}
	return s.basePath != "" && requestPath == s.basePath+"/api/config/file"
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

func (s *Server) handleIndex(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	response.WriteHeader(http.StatusOK)
	page := indexHTML
	if !isDeviceWebViewRequest(request) {
		page = browserHTML
	}
	_, _ = response.Write(page)
}

func isDeviceWebViewRequest(request *http.Request) bool {
	if request == nil {
		return false
	}
	userAgent := strings.ToLower(request.Header.Get("User-Agent"))
	if strings.Contains(userAgent, "lx04") {
		return true
	}
	if !strings.Contains(userAgent, "android") {
		return false
	}
	return strings.Contains(userAgent, "; wv") || strings.Contains(userAgent, "version/4.0") || strings.Contains(userAgent, "androidstream")
}

func (s *Server) handleSettings(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(settingsHTML)
}

func (s *Server) handleAPIDocs(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(apiDocsHTML)
}

func (s *Server) handleOpenAPISpec(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	response.Header().Set("Content-Disposition", `inline; filename="openapi.yaml"`)
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(openAPISpec)
}

func (s *Server) handleCredentialGuide(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "image/png")
	response.Header().Set("Cache-Control", "private, max-age=3600")
	response.Header().Set("Content-Length", strconv.Itoa(len(accountCredentialsGuide)))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(accountCredentialsGuide)
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

	dateRange, err := parseAnalyticsDateRange(request.URL.Query(), time.Now())
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	analytics, err := s.usage.GetAnalyticsRange(request.Context(), force, dateRange)
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

func (s *Server) handleConfigAppKey(response http.ResponseWriter, _ *http.Request) {
	// This endpoint is intentionally protected by the same middleware as the
	// other management APIs. It only exposes the explicitly requested App API
	// Key and never logs or caches the value.
	response.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	cfg := s.usage.currentConfig()
	writeJSON(response, http.StatusOK, map[string]any{
		"configured":  cfg.AppAPIKey != "",
		"app_api_key": cfg.AppAPIKey,
	})
}

func (s *Server) handleConfigFileGet(response http.ResponseWriter, _ *http.Request) {
	view, err := s.usage.ReadConfigFile()
	if err != nil {
		writeJSON(response, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (s *Server) handleConfigFilePut(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, maxConfigFileSize+4096)
	var input ConfigFileUpdate
	if err := json.NewDecoder(io.LimitReader(request.Body, maxConfigFileSize+4096)).Decode(&input); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid config file request"})
		return
	}
	view, err := s.usage.UpdateConfigFile(input.Content)
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (s *Server) handleConfigTest(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var update ConfigUpdate
	if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&update); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	result, err := s.usage.TestConfig(request.Context(), update)
	if err != nil {
		// The error intentionally contains only the upstream status or a safe
		// network/validation message; credentials are never included.
		writeJSON(response, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) handleProxyTest(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	var input ProxyTestRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, 8<<10)).Decode(&input); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	result, err := s.usage.TestProxy(request.Context(), input.ProxyURL)
	if err != nil {
		writeJSON(response, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) handleConfigPut(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var update ConfigUpdate
	if err := json.NewDecoder(io.LimitReader(request.Body, 64<<10)).Decode(&update); err != nil {
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
