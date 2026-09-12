package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultBindAddr               = "127.0.0.1:8080"
	defaultUserAgent              = "codex-tui/0.146.0 (Ubuntu 22.4.0; x86_64) xterm-256color"
	defaultCacheTTL               = 10 * time.Minute
	defaultUsageHistoryFile       = "data/usage-history.jsonl"
	defaultUsageRawHistoryFile    = "data/usage-history-raw.jsonl"
	maxUsageHistoryPoints         = 48
	maxCombinedUsageHistoryPoints = maxUsageHistoryPoints * 2
	usageHistorySampleInterval    = 5 * time.Minute
	upstreamRequestTimeout        = 15 * time.Second
	resetStatusEndpoint           = "https://codex-resets.com/api/v1/status"
	resetHistoryEndpoint          = "https://codex-resets.com/api/resets"
	resetHomepageEndpoint         = "https://codex-resets.com/"
	dailyTokenUsageEndpoint       = "https://chatgpt.com/backend-api/wham/usage/daily-token-usage-breakdown"
	dailyWorkspaceUsageEndpoint   = "https://chatgpt.com/backend-api/wham/analytics/daily-workspace-usage-counts"
)

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
	defer func() {
		if err := usage.Close(); err != nil {
			slog.Warn("close usage history database failed", "error", err)
		}
	}()

	application := NewServer(cfg, usage)
	server := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           application.handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	stopContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	usage.StartHistoryCollector(stopContext)

	go func() {
		build := application.buildMetadata()
		slog.Info("server started", "address", cfg.BindAddr, "version", build.Version, "commit", build.ShortCommit, "build_time", build.BuildTime, "cache_ttl", cfg.CacheTTL.String(), "history_interval", usageHistorySampleInterval.String())
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
