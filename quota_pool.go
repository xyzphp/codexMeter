package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// A missing configuration includes all accounts, including profiles added later.
type QuotaPoolConfig struct {
	AllAccounts bool     `json:"all_accounts"`
	AccountIDs  []string `json:"account_ids"`
}

type QuotaPoolUsage struct {
	Source       string          `json:"source"`
	FetchedAt    string          `json:"fetched_at"`
	AccountIDs   []string        `json:"account_ids"`
	AccountCount int             `json:"account_count"`
	FiveHour     QuotaPoolWindow `json:"five_hour"`
	SevenDay     QuotaPoolWindow `json:"seven_day"`
}

type QuotaPoolWindow struct {
	AccountCount          int      `json:"account_count"`
	AvailableAccounts     int      `json:"available_accounts"`
	StaleAccounts         int      `json:"stale_accounts"`
	CapacityPercent       float64  `json:"capacity_percent"`
	RemainingTotalPercent float64  `json:"remaining_total_percent"`
	UsedTotalPercent      float64  `json:"used_total_percent"`
	RemainingPercent      *float64 `json:"remaining_percent"`
	UsedPercent           *float64 `json:"used_percent"`
	Complete              bool     `json:"complete"`
}

func quotaPoolConfig(cfg Config) QuotaPoolConfig {
	pool := QuotaPoolConfig{AllAccounts: true, AccountIDs: []string{}}
	if cfg.QuotaPool != nil {
		pool.AllAccounts = cfg.QuotaPool.AllAccounts
		pool.AccountIDs = append(pool.AccountIDs, cfg.QuotaPool.AccountIDs...)
	}
	return pool
}

func normalizeQuotaPool(cfg *Config) error {
	if cfg.QuotaPool == nil {
		return nil
	}
	pool := quotaPoolConfig(*cfg)
	if pool.AllAccounts && len(pool.AccountIDs) > 0 {
		return errors.New("全部账号模式的 account_ids 必须为空")
	}
	seen := make(map[string]bool, len(pool.AccountIDs))
	for i, id := range pool.AccountIDs {
		id = strings.TrimSpace(id)
		if accountIndex(*cfg, id) < 0 {
			return fmt.Errorf("号池包含不存在的账号 %q", id)
		}
		if seen[id] {
			return errors.New("号池账号不能重复")
		}
		seen[id] = true
		pool.AccountIDs[i] = id
	}
	cfg.QuotaPool = &pool
	return nil
}

func (s *UsageService) SaveQuotaPool(pool QuotaPoolConfig) (QuotaPoolConfig, error) {
	s.configUpdateMu.Lock()
	defer s.configUpdateMu.Unlock()
	old := s.currentConfig()
	next := old
	next.QuotaPool = &pool
	if err := normalizeAccounts(&next); err != nil {
		return QuotaPoolConfig{}, err
	}
	if err := persistConfig(next); err != nil {
		return QuotaPoolConfig{}, err
	}
	if err := s.activateConfig(old, next); err != nil {
		return QuotaPoolConfig{}, err
	}
	return quotaPoolConfig(next), nil
}

func aggregateQuotaPool(cfg Config, accounts []AccountUsage, now time.Time) *QuotaPoolUsage {
	pool := quotaPoolConfig(cfg)
	selected := make(map[string]bool, len(pool.AccountIDs))
	for _, id := range pool.AccountIDs {
		selected[id] = true
	}
	entries := make(map[string]AccountUsage, len(accounts))
	for _, account := range accounts {
		entries[account.ID] = account
	}
	result := &QuotaPoolUsage{Source: "account_pool", FetchedAt: now.UTC().Format(time.RFC3339), AccountIDs: []string{}}
	for _, account := range cfg.Accounts {
		if !pool.AllAccounts && !selected[account.ID] {
			continue
		}
		result.AccountIDs = append(result.AccountIDs, account.ID)
		entry := entries[account.ID]
		if entry.Usage != nil {
			addPoolWindow(&result.FiveHour, entry.Usage.FiveHour, entry.Stale || entry.Error != "")
			addPoolWindow(&result.SevenDay, entry.Usage.SevenDay, entry.Stale || entry.Error != "")
		}
	}
	result.AccountCount = len(result.AccountIDs)
	finishPoolWindow(&result.FiveHour, result.AccountCount)
	finishPoolWindow(&result.SevenDay, result.AccountCount)
	return result
}

func addPoolWindow(total *QuotaPoolWindow, window *Window, stale bool) {
	if window == nil || math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) {
		return
	}
	used := math.Max(0, math.Min(100, window.UsedPercent))
	total.AvailableAccounts++
	total.UsedTotalPercent += used
	total.RemainingTotalPercent += 100 - used
	if stale {
		total.StaleAccounts++
	}
}

func finishPoolWindow(window *QuotaPoolWindow, count int) {
	window.AccountCount = count
	window.CapacityPercent = float64(count) * 100
	window.Complete = count > 0 && window.AvailableAccounts == count
	if window.Complete {
		remaining := window.RemainingTotalPercent / window.CapacityPercent * 100
		used := window.UsedTotalPercent / window.CapacityPercent * 100
		window.RemainingPercent, window.UsedPercent = &remaining, &used
	}
}

func (s *Server) handleQuotaPoolGet(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, quotaPoolConfig(s.usage.currentConfig()))
}

func (s *Server) handleQuotaPoolPut(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 4096)
	var input struct {
		AllAccounts *bool     `json:"all_accounts"`
		AccountIDs  *[]string `json:"account_ids"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.AllAccounts == nil || input.AccountIDs == nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "all_accounts 和 account_ids 为必填字段"})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	pool, err := s.usage.SaveQuotaPool(QuotaPoolConfig{AllAccounts: *input.AllAccounts, AccountIDs: *input.AccountIDs})
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, pool)
}
