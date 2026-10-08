package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	errAccountNotFound = errors.New("账号不存在")
	errAccountNotReady = errors.New("账号凭证尚未配置完整")
	errLastAccount     = errors.New("请至少保留一个账号")
	errPrimaryAccount  = errors.New("主账号用于兼容旧应用 API，请保留并更新该账号凭证")
)

// AccountView never contains OAuth tokens, Cookies or other raw credentials.
type AccountView struct {
	ID                      string `json:"id"`
	Name                    string `json:"name"`
	Active                  bool   `json:"active"`
	Primary                 bool   `json:"primary"`
	TokenConfigured         bool   `json:"token_configured"`
	TokenHint               string `json:"token_hint,omitempty"`
	CookieConfigured        bool   `json:"cookie_configured"`
	ChatGPTAccountID        string `json:"chatgpt_account_id"`
	UserAgent               string `json:"user_agent"`
	FedRAMP                 bool   `json:"fedramp"`
	ClientBuildNumber       string `json:"client_build_number,omitempty"`
	ClientVersion           string `json:"client_version,omitempty"`
	DeviceID                string `json:"device_id,omitempty"`
	SessionID               string `json:"session_id,omitempty"`
	ClientObservation       string `json:"client_observation,omitempty"`
	Referer                 string `json:"referer,omitempty"`
	SetupRequired           bool   `json:"setup_required"`
	ProxyURL                string `json:"proxy_url,omitempty"`
	ProxyPasswordConfigured bool   `json:"proxy_password_configured"`
}

type AccountsView struct {
	ActiveAccountID  string        `json:"active_account_id"`
	PrimaryAccountID string        `json:"primary_account_id"`
	Accounts         []AccountView `json:"accounts"`
}

type AccountUpdate struct {
	Name *string `json:"name"`
	ConfigUpdate
}

type AccountUsage struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Active  bool           `json:"active"`
	Primary bool           `json:"primary"`
	Usage   *UsageResponse `json:"usage,omitempty"`
	Error   string         `json:"error,omitempty"`
	Stale   bool           `json:"stale"`
}

type AccountsUsageResponse struct {
	ActiveAccountID  string         `json:"active_account_id"`
	PrimaryAccountID string         `json:"primary_account_id"`
	Accounts         []AccountUsage `json:"accounts"`
}

func accountViews(cfg Config) []AccountView {
	if err := normalizeAccounts(&cfg); err != nil {
		return []AccountView{}
	}
	views := make([]AccountView, 0, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		local := configForAccount(cfg, account)
		views = append(views, AccountView{
			ID: account.ID, Name: account.Name, Active: account.ID == cfg.ActiveAccountID,
			Primary:         account.ID == primaryAccountID(cfg),
			TokenConfigured: local.AccessToken != "", TokenHint: secretHint(local.AccessToken),
			CookieConfigured: local.UpstreamCookie != "", ChatGPTAccountID: local.ChatGPTAccountID,
			UserAgent: local.UserAgent, FedRAMP: local.FedRAMP,
			ClientBuildNumber: local.ClientBuildNumber, ClientVersion: local.ClientVersion,
			DeviceID: local.DeviceID, SessionID: local.SessionID, ClientObservation: local.ClientObservation,
			Referer: local.UpstreamReferer, SetupRequired: local.SetupRequired,
			ProxyURL: maskedProxyURL(local.UpstreamProxy), ProxyPasswordConfigured: proxyPasswordConfigured(local.UpstreamProxy),
		})
	}
	return views
}

func (s *UsageService) AccountsView() AccountsView {
	cfg := s.currentConfig()
	_ = normalizeAccounts(&cfg)
	return AccountsView{ActiveAccountID: cfg.ActiveAccountID, PrimaryAccountID: primaryAccountID(cfg), Accounts: accountViews(cfg)}
}

// Each profile gets its own service instance, refresh locks and caches. A
// configuration change replaces only that profile's runtime; an in-flight
// response cannot populate the cache of a newly selected account.
func (s *UsageService) accountService(cfg Config, id string) (*UsageService, AccountConfig, error) {
	if err := normalizeAccounts(&cfg); err != nil {
		return nil, AccountConfig{}, err
	}
	if id == "" {
		id = primaryAccountID(cfg)
	}
	index := accountIndex(cfg, id)
	if index < 0 {
		return nil, AccountConfig{}, errAccountNotFound
	}
	account := cfg.Accounts[index]
	local := configForAccount(cfg, account)
	if local.SetupRequired {
		return nil, account, errAccountNotReady
	}
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	if s.accounts == nil {
		s.accounts = make(map[string]*UsageService)
	}
	if existing := s.accounts[id]; existing != nil {
		old := existing.currentConfig()
		if openAIConfigFromConfig(old) == account.OpenAI && old.UpstreamProxy == local.UpstreamProxy && old.CacheTTL == local.CacheTTL {
			return existing, account, nil
		}
	}
	// Read the client and its proxy together while an active switch may run.
	s.cfgMu.RLock()
	s.clientMu.RLock()
	client, rootProxy := s.client, s.cfg.UpstreamProxy
	s.clientMu.RUnlock()
	s.cfgMu.RUnlock()
	if client == nil || local.UpstreamProxy != rootProxy {
		var err error
		client, err = newUpstreamClient(local.UpstreamProxy)
		if err != nil {
			return nil, account, err
		}
	}
	worker := &UsageService{cfg: local, client: client, accountID: id, historyAccountID: accountHistoryKey(account), historyStore: s.historyStore}
	if s.historyStore != nil {
		legacyOwner := primaryAccountID(cfg)
		if id == legacyOwner {
			if err := s.historyStore.ClaimLegacyHistory(context.Background(), worker.historyAccountID); err != nil {
				return nil, account, err
			}
		}
		history, err := s.historyStore.LoadAccount(context.Background(), worker.historyAccountID)
		if err != nil {
			return nil, account, err
		}
		worker.rawHistory = ensureOrderedUsageHistoryPoints(history)
		worker.weeklyHistory = compactUsageHistoryMetricOrdered(worker.rawHistory, usageHistoryMetricWeekly)
		worker.fiveHourHistory = compactUsageHistoryMetricOrdered(worker.rawHistory, usageHistoryMetricFiveHour)
		worker.history = mergeUsageHistories(worker.weeklyHistory, worker.fiveHourHistory)
	}
	s.accounts[id] = worker
	return worker, account, nil
}

func (s *UsageService) GetForAccount(ctx context.Context, id string, force bool) (*UsageResponse, error) {
	cfg := s.currentConfig()
	if len(cfg.Accounts) == 0 && (id == "" || id == defaultAccountID) {
		return s.Get(ctx, force)
	}
	worker, account, err := s.accountService(cfg, id)
	if err != nil {
		return nil, err
	}
	usage, err := worker.Get(ctx, force)
	if usage != nil {
		usage.AccountID, usage.AccountName = account.ID, account.Name
	}
	return usage, err
}

func (s *UsageService) GetAnalyticsForAccount(ctx context.Context, id string, force bool, dateRange analyticsDateRange) (*UsageAnalytics, error) {
	cfg := s.currentConfig()
	if len(cfg.Accounts) == 0 && (id == "" || id == defaultAccountID) {
		return s.GetAnalyticsRange(ctx, force, dateRange)
	}
	worker, account, err := s.accountService(cfg, id)
	if err != nil {
		return nil, err
	}
	analytics, err := worker.GetAnalyticsRange(ctx, force, dateRange)
	if analytics != nil {
		analytics.AccountID, analytics.AccountName = account.ID, account.Name
	}
	return analytics, err
}

func parallelAccounts(ctx context.Context, count int, visit func(int)) {
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	for index := 0; index < count; index++ {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case semaphore <- struct{}{}:
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			defer func() { <-semaphore }()
			visit(index)
		}(index)
	}
	wg.Wait()
}

func (s *UsageService) GetAccountsUsage(ctx context.Context, force bool) AccountsUsageResponse {
	cfg := s.currentConfig()
	_ = normalizeAccounts(&cfg)
	result := AccountsUsageResponse{ActiveAccountID: cfg.ActiveAccountID, PrimaryAccountID: primaryAccountID(cfg), Accounts: make([]AccountUsage, len(cfg.Accounts))}
	for index, account := range cfg.Accounts {
		result.Accounts[index] = AccountUsage{ID: account.ID, Name: account.Name, Active: account.ID == cfg.ActiveAccountID, Primary: account.ID == result.PrimaryAccountID, Error: "请求已取消"}
	}
	parallelAccounts(ctx, len(cfg.Accounts), func(index int) {
		entry := &result.Accounts[index]
		worker, account, err := s.accountService(cfg, entry.ID)
		if err == nil {
			entry.Usage, err = worker.Get(ctx, force)
			if err != nil {
				worker.cacheMu.Lock()
				entry.Usage = cloneUsage(worker.cached)
				worker.cacheMu.Unlock()
				if entry.Usage != nil {
					entry.Stale, entry.Usage.FromCache = true, true
				}
			}
		}
		entry.Error = ""
		if err != nil {
			entry.Error = err.Error()
		}
		if entry.Usage != nil {
			entry.Usage.AccountID, entry.Usage.AccountName = account.ID, account.Name
		}
	})
	return result
}

func (s *UsageService) collectAllAccountsHistoryAt(ctx context.Context, sampleAt time.Time) {
	cfg := s.currentConfig()
	if len(cfg.Accounts) == 0 {
		if !cfg.SetupRequired {
			s.collectUsageHistoryAt(ctx, sampleAt)
		}
		return
	}
	parallelAccounts(ctx, len(cfg.Accounts), func(index int) {
		worker, _, err := s.accountService(cfg, cfg.Accounts[index].ID)
		if err == nil {
			worker.collectUsageHistoryAt(ctx, sampleAt)
		}
	})
}

func (s *UsageService) SaveAccount(id string, input AccountUpdate) (AccountsView, error) {
	s.configUpdateMu.Lock()
	defer s.configUpdateMu.Unlock()
	old := s.currentConfig()
	next := old
	if err := normalizeAccounts(&next); err != nil {
		return AccountsView{}, err
	}
	index := accountIndex(next, id)
	creating := id == ""
	if creating {
		if len(next.Accounts) >= maxAccounts {
			return AccountsView{}, fmt.Errorf("最多支持 %d 个账号", maxAccounts)
		}
		var bytes [12]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return AccountsView{}, errors.New("无法生成账号 id")
		}
		id = "account-" + hex.EncodeToString(bytes[:])
		next.Accounts = append(next.Accounts, AccountConfig{ID: id, Name: fmt.Sprintf("账号 %d", len(next.Accounts)+1), OpenAI: OpenAIConfig{UserAgent: defaultUserAgent}, Proxy: &ProxyConfig{}})
		index = len(next.Accounts) - 1
	} else if index < 0 {
		return AccountsView{}, errAccountNotFound
	}
	account := next.Accounts[index]
	if input.Name != nil {
		account.Name = strings.TrimSpace(*input.Name)
		if account.Name == "" {
			return AccountsView{}, errors.New("账号名称不能为空")
		}
	}
	local := configForAccount(next, account)
	updated, err := applyConfigUpdateWithMode(local, input.ConfigUpdate, !creating && local.SetupRequired)
	if err != nil {
		return AccountsView{}, err
	}
	// Updating a token keeps history. A different upstream identity must be
	// added as a new profile so its quota timeline cannot mix with this one.
	if account.OpenAI.ChatGPTAccountID != "" && account.OpenAI.ChatGPTAccountID != updated.ChatGPTAccountID {
		return AccountsView{}, errors.New("Account ID 已改变，请新增账号以保留独立历史")
	}
	account.OpenAI = openAIConfigFromConfig(updated)
	account.Proxy = &ProxyConfig{URL: updated.UpstreamProxy}
	next.Accounts[index] = account
	displaySetupRequired := old.SetupRequired
	if active := accountIndex(old, old.ActiveAccountID); active >= 0 {
		displaySetupRequired = configForAccount(old, old.Accounts[active]).SetupRequired
	}
	if creating && displaySetupRequired {
		next.ActiveAccountID = account.ID
	}
	if err := normalizeAccounts(&next); err != nil {
		return AccountsView{}, err
	}
	if err := persistConfig(next); err != nil {
		return AccountsView{}, err
	}
	if err := s.activateConfig(old, next); err != nil {
		return AccountsView{}, err
	}
	return s.AccountsView(), nil
}

func (s *UsageService) SwitchAccount(id string) (AccountsView, error) {
	s.configUpdateMu.Lock()
	defer s.configUpdateMu.Unlock()
	old := s.currentConfig()
	next := old
	if err := normalizeAccounts(&next); err != nil {
		return AccountsView{}, err
	}
	index := accountIndex(next, strings.TrimSpace(id))
	if index < 0 {
		return AccountsView{}, errAccountNotFound
	}
	if configForAccount(next, next.Accounts[index]).SetupRequired {
		return AccountsView{}, errAccountNotReady
	}
	next.ActiveAccountID = next.Accounts[index].ID
	if err := normalizeAccounts(&next); err != nil {
		return AccountsView{}, err
	}
	if err := persistConfig(next); err != nil {
		return AccountsView{}, err
	}
	if err := s.activateConfig(old, next); err != nil {
		return AccountsView{}, err
	}
	return s.AccountsView(), nil
}

func (s *UsageService) DeleteAccount(id string) (AccountsView, error) {
	s.configUpdateMu.Lock()
	defer s.configUpdateMu.Unlock()
	old := s.currentConfig()
	next := old
	if err := normalizeAccounts(&next); err != nil {
		return AccountsView{}, err
	}
	index := accountIndex(next, id)
	if index < 0 {
		return AccountsView{}, errAccountNotFound
	}
	if len(next.Accounts) == 1 {
		return AccountsView{}, errLastAccount
	}
	if id == primaryAccountID(next) {
		return AccountsView{}, errPrimaryAccount
	}
	next.Accounts = append(next.Accounts[:index], next.Accounts[index+1:]...)
	if next.ActiveAccountID == id {
		next.ActiveAccountID = next.Accounts[0].ID
	}
	if err := normalizeAccounts(&next); err != nil {
		return AccountsView{}, err
	}
	if err := persistConfig(next); err != nil {
		return AccountsView{}, err
	}
	if err := s.activateConfig(old, next); err != nil {
		return AccountsView{}, err
	}
	s.accountsMu.Lock()
	delete(s.accounts, id)
	s.accountsMu.Unlock()
	return s.AccountsView(), nil
}
