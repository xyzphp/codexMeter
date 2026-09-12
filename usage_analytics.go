package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

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
