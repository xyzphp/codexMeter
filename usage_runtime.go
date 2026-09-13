package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

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
	if input.CommunityPoll != nil {
		poll := *input.CommunityPoll
		output.CommunityPoll = &poll
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
	if input.WeeklyHistory != nil {
		output.WeeklyHistory = append([]HistoryPoint(nil), input.WeeklyHistory...)
	}
	if input.FiveHourHistory != nil {
		output.FiveHourHistory = append([]HistoryPoint(nil), input.FiveHourHistory...)
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
		RateLimitReachedType:  string(upstream.RateLimitReachedType),
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
		window.WindowMinutes = max(*raw.WindowMinutes, 0)
	}
	if raw.ResetAfterSeconds != nil {
		window.ResetAfterSeconds = max(*raw.ResetAfterSeconds, 0)
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
