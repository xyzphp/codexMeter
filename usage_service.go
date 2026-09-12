package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

type UsageService struct {
	cfgMu                 sync.RWMutex
	cfg                   Config
	clientMu              sync.RWMutex
	client                *http.Client
	cacheMu               sync.Mutex
	refreshMu             sync.Mutex
	resetRefreshMu        sync.Mutex
	analyticsRefreshMu    sync.Mutex
	cached                *UsageResponse
	cachedAt              time.Time
	rawHistory            []HistoryPoint
	history               []HistoryPoint
	weeklyHistory         []HistoryPoint
	fiveHourHistory       []HistoryPoint
	lastSuccessfulHistory *HistoryPoint
	resetCached           *ResetPrediction
	resetCachedAt         time.Time
	analyticsCached       *UsageAnalytics
	analyticsCachedAt     time.Time
	analyticsCachedKey    string
	historyStore          *UsageHistoryStore
	rawHistoryFile        string
	historyFile           string
	weeklyHistoryFile     string
	fiveHourHistoryFile   string
}

func NewUsageService(cfg Config) (*UsageService, error) {
	client, err := newUpstreamClient(cfg.UpstreamProxy)
	if err != nil {
		return nil, err
	}
	historyStore, err := OpenUsageHistoryStore(defaultUsageDatabaseFile)
	if err != nil {
		return nil, fmt.Errorf("open usage history database: %w", err)
	}
	storeReady := false
	defer func() {
		if !storeReady {
			_ = historyStore.Close()
		}
	}()
	rawHistory, err := historyStore.Load(context.Background())
	if err != nil {
		return nil, fmt.Errorf("load usage history database: %w", err)
	}
	if len(rawHistory) == 0 {
		// Older installations only have JSONL snapshots. Merge every valid
		// current and backup snapshot once to seed SQLite; future scheduled
		// samples are then retained without a point limit.
		weeklyHistoryFile := usageHistoryMetricPath(defaultUsageHistoryFile, usageHistoryMetricWeekly)
		fiveHourHistoryFile := usageHistoryMetricPath(defaultUsageHistoryFile, usageHistoryMetricFiveHour)
		rawHistory, err = loadUsageHistoryMigrationRecords(defaultUsageRawHistoryFile, defaultUsageHistoryFile, weeklyHistoryFile, fiveHourHistoryFile)
		if err != nil {
			return nil, fmt.Errorf("load usage history migration data: %w", err)
		}
		if len(rawHistory) > 0 {
			if err := historyStore.Import(context.Background(), rawHistory); err != nil {
				return nil, fmt.Errorf("import usage history into database: %w", err)
			}
			slog.Info("migrated usage history into sqlite", "path", historyStore.Path(), "points", len(rawHistory))
		}
	}
	rawHistory = orderUsageHistoryPoints(rawHistory)
	weeklyHistory := compactUsageHistoryMetric(rawHistory, usageHistoryMetricWeekly)
	fiveHourHistory := compactUsageHistoryMetric(rawHistory, usageHistoryMetricFiveHour)
	history := mergeUsageHistories(weeklyHistory, fiveHourHistory)
	var lastSuccessfulHistory *HistoryPoint
	if point, ok := latestSuccessfulHistoryPoint(rawHistory); ok {
		lastSuccessfulHistory = &point
	}
	slog.Info("usage history loaded", "database", historyStore.Path(), "raw_points", len(rawHistory), "points", len(history), "weekly_points", len(weeklyHistory), "five_hour_points", len(fiveHourHistory))
	storeReady = true
	return &UsageService{
		cfg:                   cfg,
		client:                client,
		historyStore:          historyStore,
		rawHistory:            rawHistory,
		history:               history,
		weeklyHistory:         weeklyHistory,
		fiveHourHistory:       fiveHourHistory,
		lastSuccessfulHistory: lastSuccessfulHistory,
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

	s.cacheMu.Lock()
	usage.History = append([]HistoryPoint(nil), s.history...)
	usage.WeeklyHistory = append([]HistoryPoint(nil), s.weeklyHistory...)
	usage.FiveHourHistory = append([]HistoryPoint(nil), s.fiveHourHistory...)
	s.cached = cloneUsage(usage)
	s.cachedAt = time.Now()
	s.cacheMu.Unlock()
	return cloneUsage(usage), nil
}

// StartHistoryCollector starts the backend sampler independently from page
// refreshes. The caller owns ctx and should cancel it during shutdown.
func (s *UsageService) StartHistoryCollector(ctx context.Context) {
	go func() {
		for {
			nextSampleAt := nextUsageHistorySampleAt(time.Now())
			timer := time.NewTimer(time.Until(nextSampleAt))
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
				if !s.currentConfig().SetupRequired {
					s.collectUsageHistoryAt(ctx, nextSampleAt)
				}
			}
		}
	}()
}

func (s *UsageService) collectUsageHistory(ctx context.Context) {
	s.collectUsageHistoryAt(ctx, time.Time{})
}

func (s *UsageService) collectUsageHistoryAt(ctx context.Context, sampleAt time.Time) {
	usage, err := s.Get(ctx, true)
	if err != nil {
		slog.Warn("scheduled usage collection failed", "error", err)
		point, ok := s.lastSuccessfulHistoryPoint()
		if !ok {
			return
		}
		point.At = usageHistorySampleTimestamp(sampleAt)
		point.Stale = true
		s.persistUsageHistoryPoint(point)
		return
	}
	point, ok := usageHistoryPoint(usage)
	if !ok {
		return
	}
	if !sampleAt.IsZero() {
		point.At = usageHistorySampleTimestamp(sampleAt)
	}

	s.persistUsageHistoryPoint(point)
}
