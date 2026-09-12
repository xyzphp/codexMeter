package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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
	SetupRequired     bool
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
	return loadConfigFile(configPath, false)
}

func loadConfigFile(configPath string, requireComplete bool) (Config, error) {
	resolvedConfigPath, err := resolveConfigPath(configPath)
	if err != nil {
		return Config{}, fmt.Errorf("inspect %s: %w", configPath, err)
	}
	cfg := Config{
		BindAddr:   defaultBindAddr,
		UserAgent:  defaultUserAgent,
		CacheTTL:   defaultCacheTTL,
		ConfigPath: resolvedConfigPath,
	}

	configFileMissing := false
	if raw, err := os.ReadFile(resolvedConfigPath); err == nil {
		var stored fileConfig
		if err := json.Unmarshal(raw, &stored); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", resolvedConfigPath, err)
		}
		applyFileConfig(&cfg, stored)
	} else {
		if !os.IsNotExist(err) {
			return Config{}, fmt.Errorf("read %s: %w", resolvedConfigPath, err)
		}
		configFileMissing = true
	}

	// First-run containers mount an empty configuration directory. Create a
	// credential-free file immediately so the settings page can update it in
	// place and the host bind mount always contains a real file.
	if configFileMissing && !requireComplete {
		if err := persistConfig(cfg); err != nil {
			return Config{}, fmt.Errorf("initialize %s: %w", resolvedConfigPath, err)
		}
	}

	if err := applyEnvironmentConfig(&cfg); err != nil {
		return Config{}, err
	}
	basePath, err := normalizeBasePath(cfg.BasePath)
	if err != nil {
		return Config{}, err
	}
	cfg.BasePath = basePath

	if cfg.AccessToken == "" || cfg.ChatGPTAccountID == "" {
		if requireComplete {
			if cfg.AccessToken == "" {
				return Config{}, errors.New("OPENAI_ACCESS_TOKEN is required")
			}
			return Config{}, errors.New("CHATGPT_ACCOUNT_ID is required")
		}
		cfg.SetupRequired = true
		return cfg, nil
	}
	if cfg.BasicAuthEnabled && (cfg.BasicAuthUsername == "" || cfg.BasicAuthPassword == "") {
		return Config{}, errors.New("BASIC_AUTH_USER and BASIC_AUTH_PASSWORD are required when Basic Auth is enabled")
	}
	return cfg, nil
}

// resolveConfigPath accepts either a direct file path or a mounted configuration
// directory. Directory mounts use a nested config.json that is created during
// first-run startup and then updated by the settings page.
func resolveConfigPath(configPath string) (string, error) {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return "", errors.New("config path is empty")
	}
	info, err := os.Stat(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return configPath, nil
		}
		return "", err
	}
	if !info.IsDir() {
		return configPath, nil
	}

	nestedPath := filepath.Join(configPath, "config.json")
	nestedInfo, err := os.Stat(nestedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nestedPath, nil
		}
		return "", err
	}
	if nestedInfo.IsDir() {
		return "", fmt.Errorf("%s is a directory", nestedPath)
	}
	return nestedPath, nil
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
