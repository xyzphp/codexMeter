package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
	// The community poll homepage is the slowest of the three public requests
	// and only supplementary; fetch it concurrently with the JSON endpoints so
	// a slow homepage does not extend the forecast wait.
	type pollFetch struct {
		page []byte
		err  error
	}
	pollChannel := make(chan pollFetch, 1)
	go func() {
		page, err := s.queryPublicResetPage(ctx)
		pollChannel <- pollFetch{page: page, err: err}
	}()

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
	var communityPoll *ResetPoll
	fetch := <-pollChannel
	if fetch.err != nil {
		// The community poll is supplementary. Keep the forecast available if
		// the public homepage is temporarily unavailable or changes shape.
		slog.Warn("reset poll request failed", "error", fetch.err)
	} else {
		communityPoll = parseResetPoll(fetch.page)
	}
	return &ResetPrediction{
		Source:        "codex_resets_status",
		FetchedAt:     fetchedAt,
		LatestReset:   latest,
		ActiveWatch:   envelope.Data.ActiveWatch,
		CommunityPoll: communityPoll,
		History:       history,
		Stats:         stats,
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

func (s *UsageService) queryPublicResetPage(ctx context.Context) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, upstreamRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, resetHomepageEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create public reset homepage request: %w", err)
	}
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("User-Agent", "codex-usage-dashboard/1.0")

	response, err := s.currentClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("upstream returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}

func parseResetPoll(page []byte) *ResetPoll {
	tag := resetPollTagPattern.Find(page)
	if tag == nil {
		return nil
	}
	yesMatch := resetPollYesPattern.FindSubmatch(tag)
	noMatch := resetPollNoPattern.FindSubmatch(tag)
	if len(yesMatch) != 2 || len(noMatch) != 2 {
		return nil
	}
	yesVotes, yesErr := strconv.Atoi(string(yesMatch[1]))
	noVotes, noErr := strconv.Atoi(string(noMatch[1]))
	if yesErr != nil || noErr != nil || yesVotes < 0 || noVotes < 0 {
		return nil
	}
	totalVotes := yesVotes + noVotes
	if totalVotes == 0 {
		return nil
	}
	return &ResetPoll{
		YesVotes:   yesVotes,
		NoVotes:    noVotes,
		TotalVotes: totalVotes,
		YesPercent: float64(yesVotes) * 100 / float64(totalVotes),
	}
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
