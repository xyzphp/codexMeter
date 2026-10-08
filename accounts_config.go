package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	defaultAccountID = "default"
	maxAccounts      = 32
)

var accountIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type OpenAIConfig struct {
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
}

type AccountConfig struct {
	ID     string       `json:"id"`
	Name   string       `json:"name"`
	OpenAI OpenAIConfig `json:"openai"`
	Proxy  *ProxyConfig `json:"proxy,omitempty"`
}

type ProxyConfig struct {
	URL string `json:"url"`
}

func openAIConfigFromConfig(cfg Config) OpenAIConfig {
	return OpenAIConfig{
		AccessToken: cfg.AccessToken, Cookie: cfg.UpstreamCookie, ChatGPTAccountID: cfg.ChatGPTAccountID,
		ClientBuildNumber: cfg.ClientBuildNumber, ClientVersion: cfg.ClientVersion,
		DeviceID: cfg.DeviceID, SessionID: cfg.SessionID, ClientObservation: cfg.ClientObservation,
		Referer: cfg.UpstreamReferer, UserAgent: cfg.UserAgent, FedRAMP: cfg.FedRAMP,
	}
}

func setOpenAIConfig(cfg *Config, account OpenAIConfig) {
	cfg.AccessToken = strings.TrimSpace(account.AccessToken)
	cfg.UpstreamCookie = strings.TrimSpace(account.Cookie)
	cfg.ChatGPTAccountID = strings.TrimSpace(account.ChatGPTAccountID)
	cfg.ClientBuildNumber = strings.TrimSpace(account.ClientBuildNumber)
	cfg.ClientVersion = strings.TrimSpace(account.ClientVersion)
	cfg.DeviceID = strings.TrimSpace(account.DeviceID)
	cfg.SessionID = strings.TrimSpace(account.SessionID)
	cfg.ClientObservation = strings.TrimSpace(account.ClientObservation)
	cfg.UpstreamReferer = strings.TrimSpace(account.Referer)
	cfg.UserAgent = strings.TrimSpace(account.UserAgent)
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	cfg.FedRAMP = account.FedRAMP
	cfg.SetupRequired = cfg.AccessToken == "" || cfg.ChatGPTAccountID == ""
}

func accountIndex(cfg Config, id string) int {
	for index, account := range cfg.Accounts {
		if account.ID == id {
			return index
		}
	}
	return -1
}

// The original application API stays attached to the legacy default profile.
// Custom multi-account files use their first profile as the stable primary.
// ActiveAccountID only controls the account displayed by the built-in pages.
func primaryAccountID(cfg Config) string {
	if accountIndex(cfg, defaultAccountID) >= 0 {
		return defaultAccountID
	}
	if len(cfg.Accounts) > 0 {
		return cfg.Accounts[0].ID
	}
	return defaultAccountID
}

func normalizeAccounts(cfg *Config) error {
	// Config is copied by value at request boundaries. Copy its slice as well
	// before normalizing so concurrent account queries never mutate a shared snapshot.
	cfg.Accounts = append([]AccountConfig(nil), cfg.Accounts...)
	if len(cfg.Accounts) == 0 {
		cfg.Accounts = []AccountConfig{{ID: defaultAccountID, Name: "主账号", OpenAI: openAIConfigFromConfig(*cfg)}}
	}
	if len(cfg.Accounts) > maxAccounts {
		return fmt.Errorf("最多支持 %d 个账号", maxAccounts)
	}
	seen := make(map[string]bool, len(cfg.Accounts))
	for index := range cfg.Accounts {
		account := &cfg.Accounts[index]
		account.ID = strings.TrimSpace(account.ID)
		if !accountIDPattern.MatchString(account.ID) || account.ID == "active" || account.ID == "usage" {
			return errors.New("账号 id 必须为 1–64 位字母、数字、下划线或短横线，且不能为 active 或 usage")
		}
		if seen[account.ID] {
			return errors.New("账号 id 不能重复")
		}
		seen[account.ID] = true
		account.Name = strings.TrimSpace(account.Name)
		if account.Name == "" {
			account.Name = fmt.Sprintf("账号 %d", index+1)
		}
		if utf8.RuneCountInString(account.Name) > 80 {
			return errors.New("账号名称不能超过 80 个字符")
		}
		local := Config{}
		setOpenAIConfig(&local, account.OpenAI)
		account.OpenAI = openAIConfigFromConfig(local)
		// Missing proxy settings inherit the old installation proxy once.
		// An explicit empty URL always means direct access for this account.
		proxyURL := cfg.UpstreamProxy
		if account.Proxy != nil {
			proxyURL = account.Proxy.URL
		}
		proxyURL = strings.TrimSpace(proxyURL)
		if _, err := parseProxyURL(proxyURL); err != nil {
			return fmt.Errorf("账号 %s 的代理配置无效: %w", account.ID, err)
		}
		account.Proxy = &ProxyConfig{URL: proxyURL}
	}
	if cfg.ActiveAccountID == "" {
		cfg.ActiveAccountID = cfg.Accounts[0].ID
	}
	index := accountIndex(*cfg, cfg.ActiveAccountID)
	if index < 0 {
		return errors.New("active_account_id 必须指向已配置的账号")
	}
	index = accountIndex(*cfg, primaryAccountID(*cfg))
	setOpenAIConfig(cfg, cfg.Accounts[index].OpenAI)
	cfg.UpstreamProxy = cfg.Accounts[index].Proxy.URL
	return nil
}

func configForAccount(cfg Config, account AccountConfig) Config {
	cfg.Accounts = nil
	cfg.ActiveAccountID = account.ID
	setOpenAIConfig(&cfg, account.OpenAI)
	if account.Proxy != nil {
		cfg.UpstreamProxy = account.Proxy.URL
	}
	return cfg
}

func accountHistoryKey(account AccountConfig) string {
	// Keep offline edits or environment changes to the upstream identity from
	// reusing an unrelated timeline under the same local profile id.
	return account.ID + ":" + account.OpenAI.ChatGPTAccountID
}
