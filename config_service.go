package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	SetupRequired               bool   `json:"setup_required"`
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
		SetupRequired:               cfg.SetupRequired,
	}
}

func (s *UsageService) UpdateConfig(update ConfigUpdate) (ConfigView, error) {
	old := s.currentConfig()
	next, err := applyConfigUpdateForSave(old, update)
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
	if s.historyStore == nil {
		// Legacy test/manual services do not have a durable source to reload.
		s.rawHistory = nil
		s.history = nil
		s.weeklyHistory = nil
		s.fiveHourHistory = nil
	} else {
		// SQLite history belongs to the running installation and remains
		// available after a credential or proxy refresh.
		s.weeklyHistory = compactUsageHistoryMetric(s.rawHistory, usageHistoryMetricWeekly)
		s.fiveHourHistory = compactUsageHistoryMetric(s.rawHistory, usageHistoryMetricFiveHour)
		s.history = mergeUsageHistories(s.weeklyHistory, s.fiveHourHistory)
	}
	s.resetCached = nil
	s.resetCachedAt = time.Time{}
	s.analyticsCached = nil
	s.analyticsCachedAt = time.Time{}
	s.analyticsCachedKey = ""
	s.cacheMu.Unlock()
	return nil
}

func applyConfigUpdate(old Config, update ConfigUpdate) (Config, error) {
	return applyConfigUpdateWithMode(old, update, false)
}

func applyConfigUpdateForSave(old Config, update ConfigUpdate) (Config, error) {
	return applyConfigUpdateWithMode(old, update, old.SetupRequired)
}

func applyConfigUpdateWithMode(old Config, update ConfigUpdate, allowIncomplete bool) (Config, error) {
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
	if next.AccessToken == "" && !allowIncomplete {
		return Config{}, errors.New("access_token cannot be empty")
	}
	if next.ChatGPTAccountID == "" && !allowIncomplete {
		return Config{}, errors.New("chatgpt_account_id cannot be empty")
	}
	if next.BasicAuthEnabled && (next.BasicAuthUsername == "" || next.BasicAuthPassword == "") {
		return Config{}, errors.New("basic_auth_username and basic_auth_password are required when Basic Auth is enabled")
	}
	next.SetupRequired = next.AccessToken == "" || next.ChatGPTAccountID == ""
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
	if err := writeConfigFile(cfg.ConfigPath, raw); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func writeConfigFile(path string, content []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o600)
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
	if err := writeConfigFile(current.ConfigPath, []byte(content+"\n")); err != nil {
		return ConfigFileView{}, fmt.Errorf("write config: %w", err)
	}
	if err := s.activateConfig(current, next); err != nil {
		return ConfigFileView{}, err
	}
	return s.ReadConfigFile()
}
