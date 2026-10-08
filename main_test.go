package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDeviceWebViewDetection(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want bool
	}{
		{name: "LX04", ua: "Mozilla/5.0 (Linux; Android 8.1.0; LX04 Build/OPM1) AppleWebKit/537.36 Chrome/70.0 Mobile Safari/537.36", want: true},
		{name: "Android WebView", ua: "Mozilla/5.0 (Linux; Android 8.1.0; wv) AppleWebKit/537.36 Version/4.0 Chrome/70.0 Mobile Safari/537.36", want: true},
		{name: "Android Chrome", ua: "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/151.0 Mobile Safari/537.36", want: false},
		{name: "Desktop Chrome", ua: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/151.0 Safari/537.36", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("User-Agent", test.ua)
			if got := isDeviceWebViewRequest(request); got != test.want {
				t.Fatalf("isDeviceWebViewRequest() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMissingConfigEntersSetupMode(t *testing.T) {
	t.Setenv("OPENAI_ACCESS_TOKEN", "")
	t.Setenv("CHATGPT_ACCOUNT_ID", "")
	t.Setenv("BASIC_AUTH_ENABLED", "false")
	configPath := filepath.Join(t.TempDir(), "config.json")

	cfg, err := loadConfigFile(configPath, false)
	if err != nil {
		t.Fatalf("load missing config: %v", err)
	}
	if !cfg.SetupRequired {
		t.Fatalf("SetupRequired = false, want true")
	}
	if cfg.ConfigPath != configPath {
		t.Fatalf("config path = %q, want %q", cfg.ConfigPath, configPath)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("missing config was not created automatically: %v", err)
	}
}

func TestDirectoryConfigPathUsesNestedConfigFile(t *testing.T) {
	t.Setenv("OPENAI_ACCESS_TOKEN", "")
	t.Setenv("CHATGPT_ACCOUNT_ID", "")
	t.Setenv("BASIC_AUTH_ENABLED", "false")
	configDirectory := filepath.Join(t.TempDir(), "config.json")
	if err := os.Mkdir(configDirectory, 0o750); err != nil {
		t.Fatalf("create config directory: %v", err)
	}

	cfg, err := loadConfigFile(configDirectory, false)
	if err != nil {
		t.Fatalf("load directory config path: %v", err)
	}
	wantPath := filepath.Join(configDirectory, "config.json")
	if cfg.ConfigPath != wantPath {
		t.Fatalf("resolved config path = %q, want %q", cfg.ConfigPath, wantPath)
	}
	if !cfg.SetupRequired {
		t.Fatalf("SetupRequired = false, want true")
	}
}

func TestSetupPageIsServedWhenConfigIsMissing(t *testing.T) {
	cfg := Config{SetupRequired: true}
	service := &UsageService{cfg: cfg}
	server := NewServer(cfg, service)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("setup page status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !strings.Contains(recorder.Body.String(), "配置引导") {
		t.Fatalf("setup page does not contain the setup guide")
	}
	if !strings.Contains(recorder.Body.String(), "打开配置页面") {
		t.Fatalf("setup page does not link to settings")
	}
}

func TestSetupModeUpdateCreatesConfigFileBeforeCredentials(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config", "config.json")
	service := &UsageService{cfg: Config{
		ConfigPath:    configPath,
		CacheTTL:      time.Minute,
		UserAgent:     defaultUserAgent,
		SetupRequired: true,
	}}
	proxyURL := "http://127.0.0.1:7890"

	view, err := service.UpdateConfig(ConfigUpdate{ProxyURL: &proxyURL})
	if err != nil {
		t.Fatalf("save setup proxy config: %v", err)
	}
	if !view.SetupRequired {
		t.Fatal("setup should remain required before account credentials are saved")
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file was not created: %v", err)
	}
}

func TestSetupModeUpdateCompletesAfterCredentials(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config", "config.json")
	service := &UsageService{cfg: Config{
		ConfigPath:    configPath,
		CacheTTL:      time.Minute,
		UserAgent:     defaultUserAgent,
		SetupRequired: true,
	}}
	token := "access-token"
	accountID := "account-id"

	view, err := service.UpdateConfig(ConfigUpdate{
		AccessToken:      &token,
		ChatGPTAccountID: &accountID,
	})
	if err != nil {
		t.Fatalf("save setup credentials: %v", err)
	}
	if view.SetupRequired {
		t.Fatal("setup should be complete after account credentials are saved")
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file was not created: %v", err)
	}
}

func TestCredentialGuideAssetIsServedAsPNG(t *testing.T) {
	server := NewServer(Config{}, &UsageService{})
	request := httptest.NewRequest(http.MethodGet, "/assets/account-credentials-guide.png", nil)
	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("guide asset status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("guide asset content type = %q, want image/png", got)
	}
	data := recorder.Body.Bytes()
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("guide asset is not a PNG payload")
	}
}

func TestAPIDocsAndOpenAPISpecAreServed(t *testing.T) {
	server := NewServer(Config{}, &UsageService{})

	pageRequest := httptest.NewRequest(http.MethodGet, "/api-docs", nil)
	pageRecorder := httptest.NewRecorder()
	server.handler.ServeHTTP(pageRecorder, pageRequest)
	if pageRecorder.Code != http.StatusOK {
		t.Fatalf("API docs status = %d, want %d", pageRecorder.Code, http.StatusOK)
	}
	if got := pageRecorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("API docs content type = %q, want HTML", got)
	}
	if !strings.Contains(pageRecorder.Body.String(), "在线调试") {
		t.Fatalf("API docs page does not contain the debugger")
	}

	specRequest := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	specRecorder := httptest.NewRecorder()
	server.handler.ServeHTTP(specRecorder, specRequest)
	if specRecorder.Code != http.StatusOK {
		t.Fatalf("OpenAPI spec status = %d, want %d", specRecorder.Code, http.StatusOK)
	}
	if got := specRecorder.Header().Get("Content-Type"); got != "text/yaml; charset=utf-8" {
		t.Fatalf("OpenAPI content type = %q, want YAML", got)
	}
	if !strings.Contains(specRecorder.Body.String(), "openapi: 3.0.3") {
		t.Fatalf("OpenAPI response does not contain the OpenAPI version")
	}
}

func TestBuildMetadataIsRenderedAndExposed(t *testing.T) {
	metadata := BuildMetadata{
		Version:     "v9.8.7-platform-build",
		Commit:      "abcdef1234567890",
		ShortCommit: "abcdef123456",
		BuildTime:   "2026-09-12T06:45:00Z",
	}
	server := NewServer(Config{}, &UsageService{})
	server.build = metadata

	pageRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	pageRecorder := httptest.NewRecorder()
	server.handler.ServeHTTP(pageRecorder, pageRequest)

	if pageRecorder.Code != http.StatusOK {
		t.Fatalf("page status = %d, want %d", pageRecorder.Code, http.StatusOK)
	}
	pageBody := pageRecorder.Body.String()
	if !strings.Contains(pageBody, ">版本 v9.8.7</span>") || strings.Contains(pageBody, "platform-build") {
		t.Fatalf("page does not contain the rendered build badge")
	}
	if strings.Contains(pageBody, "{{CODEX_METER_") {
		t.Fatalf("page still contains an unresolved build metadata placeholder")
	}
	if got := pageRecorder.Header().Get("X-Codex-Meter-Version"); got != metadata.Version {
		t.Fatalf("version header = %q, want %q", got, metadata.Version)
	}
	if got := pageRecorder.Header().Get("X-Codex-Meter-Commit"); got != metadata.Commit {
		t.Fatalf("commit header = %q, want %q", got, metadata.Commit)
	}
	if got := pageRecorder.Header().Get("X-Codex-Meter-Build-Time"); got != metadata.BuildTime {
		t.Fatalf("build time header = %q, want %q", got, metadata.BuildTime)
	}

	healthRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthRecorder := httptest.NewRecorder()
	server.handler.ServeHTTP(healthRecorder, healthRequest)

	if healthRecorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", healthRecorder.Code, http.StatusOK)
	}
	var health HealthResponse
	if err := json.NewDecoder(healthRecorder.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health.Status != "ok" || health.Version != metadata.Version || health.Commit != metadata.Commit || health.BuildTime != metadata.BuildTime {
		t.Fatalf("health response = %#v, want metadata %#v", health, metadata)
	}
}

func TestRenderHTMLBuildMetadataEscapesValues(t *testing.T) {
	rendered := string(renderHTMLBuildMetadata([]byte(`<span title="{{CODEX_METER_COMMIT}}">{{CODEX_METER_VERSION}}</span>`), BuildMetadata{
		Version: `<script>alert("version")</script>`,
		Commit:  `"><script>alert("commit")</script>`,
	}))

	if strings.Contains(rendered, "<script>") {
		t.Fatalf("rendered metadata was not HTML escaped: %s", rendered)
	}
	if !strings.Contains(rendered, "&lt;script&gt;") || !strings.Contains(rendered, "&#34;&gt;") {
		t.Fatalf("rendered metadata does not contain escaped values: %s", rendered)
	}
}

func TestBuildVersionLabels(t *testing.T) {
	for _, test := range []struct {
		version string
		label   string
	}{
		{"v1.0.10", "v1.0.10"},
		{"1.0.10", "v1.0.10"},
		{"v1.0.10-xyzos", "v1.0.10"},
		{"v1.0.10-0.20261002032908-910a3b0d9707+dirty", "v1.0.10"},
		{" v1.0.10 local-build ", "v1.0.10"},
		{"dev-xyzos", "dev"},
		{"", "dev"},
		{`<script>alert("version")</script>`, "dev"},
	} {
		t.Run(test.version, func(t *testing.T) {
			if got := buildVersionLabel(test.version); got != test.label {
				t.Fatalf("version label = %q, want %q", got, test.label)
			}
		})
	}
	metadata := BuildMetadata{Version: "v1.0.10-xyzos", Commit: "fixture-commit", BuildTime: "fixture-time"}
	for _, page := range [][]byte{indexHTML, browserHTML, settingsHTML, setupHTML, apiDocsHTML} {
		rendered := string(renderHTMLBuildMetadata(page, metadata))
		if !strings.Contains(rendered, ">版本 v1.0.10</span>") || strings.Contains(rendered, "xyzos") || strings.Contains(rendered, "{{CODEX_METER_") {
			t.Fatal("page version badge contains build suffixes or unresolved placeholders")
		}
	}
}

func TestAppAPIKeyEndpointReturnsConfiguredKeyAfterAuthentication(t *testing.T) {
	const appAPIKey = "test-app-key"
	cfg := Config{AppAPIKey: appAPIKey}
	service := &UsageService{cfg: cfg}
	server := NewServer(cfg, service)
	request := httptest.NewRequest(http.MethodGet, "/api/config/app-key", nil)
	request.Header.Set("X-App-API-Key", appAPIKey)
	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("App API key status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response struct {
		Configured bool   `json:"configured"`
		AppAPIKey  string `json:"app_api_key"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode App API key response: %v", err)
	}
	if !response.Configured || response.AppAPIKey != appAPIKey {
		t.Fatalf("App API key response = %#v, want configured key", response)
	}
}

func TestWhamResponseDecodesAndNormalizesWindows(t *testing.T) {
	raw := []byte(`{
        "plan_type":"plus",
        "rate_limit":{
            "primary_window":{"used_percent":12.5,"limit_window_seconds":604800,"reset_after_seconds":123,"reset_at":1890000000},
            "secondary_window":{"used_percent":4,"limit_window_seconds":18000,"reset_after_seconds":456,"reset_at":1890000456}
        }
    }`)

	var response whamUsageResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode wham response: %v", err)
	}
	primary := rawWindowFromWham(response.RateLimit.PrimaryWindow)
	secondary := rawWindowFromWham(response.RateLimit.SecondaryWindow)
	fiveHour, sevenDay := normalizeWindows(primary, secondary, time.Unix(1889999000, 0).UTC())

	if response.PlanType != "plus" {
		t.Fatalf("plan type = %q, want plus", response.PlanType)
	}
	if fiveHour == nil || fiveHour.UsedPercent != 4 || fiveHour.WindowMinutes != 300 {
		t.Fatalf("unexpected five-hour window: %#v", fiveHour)
	}
	if sevenDay == nil || sevenDay.UsedPercent != 12.5 || sevenDay.WindowMinutes != 10080 {
		t.Fatalf("unexpected seven-day window: %#v", sevenDay)
	}
	if fiveHour.ResetAt.Unix() != 1890000456 || sevenDay.ResetAt.Unix() != 1890000000 {
		t.Fatalf("reset_at values were not preserved: five=%v seven=%v", fiveHour.ResetAt, sevenDay.ResetAt)
	}
}

func TestAdditionalCodexRateLimitShapeDecodes(t *testing.T) {
	raw := []byte(`{
        "additional_rate_limits":[{
            "metered_feature":"codex_bengalfox",
            "rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":604800}}
        }]
    }`)

	var response whamUsageResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode additional wham response: %v", err)
	}
	if len(response.AdditionalRateLimits) != 1 || response.AdditionalRateLimits[0].RateLimit == nil {
		t.Fatalf("additional rate limit was not decoded: %#v", response.AdditionalRateLimits)
	}
}

func TestWhamRateLimitReachedTypeDecodesStringAndObject(t *testing.T) {
	var stringForm whamUsageResponse
	if err := json.Unmarshal([]byte(`{"rate_limit_reached_type":"default"}`), &stringForm); err != nil {
		t.Fatalf("decode string rate limit type: %v", err)
	}
	if stringForm.RateLimitReachedType != "default" {
		t.Fatalf("string rate limit type = %q, want default", stringForm.RateLimitReachedType)
	}

	var objectForm whamUsageResponse
	if err := json.Unmarshal([]byte(`{"rate_limit_reached_type":{"type":"rate_limit_reached","details":"default"}}`), &objectForm); err != nil {
		t.Fatalf("decode object rate limit type: %v", err)
	}
	if objectForm.RateLimitReachedType != "rate_limit_reached" {
		t.Fatalf("object rate limit type = %q, want rate_limit_reached", objectForm.RateLimitReachedType)
	}

	var nullForm whamUsageResponse
	if err := json.Unmarshal([]byte(`{"rate_limit_reached_type":null}`), &nullForm); err != nil {
		t.Fatalf("decode null rate limit type: %v", err)
	}
	if nullForm.RateLimitReachedType != "" {
		t.Fatalf("null rate limit type = %q, want empty", nullForm.RateLimitReachedType)
	}
}

func TestUsageHistoryPersistsAndKeepsMostRecentPoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history.jsonl")
	points := make([]HistoryPoint, 0, maxUsageHistoryPoints+2)
	for index := 0; index < maxUsageHistoryPoints+2; index++ {
		points = append(points, HistoryPoint{
			At:          time.Date(2026, time.August, 27, 12, index, 0, 0, time.UTC).Format(time.RFC3339),
			UsedPercent: float64(index),
		})
	}

	for _, point := range points {
		if err := appendUsageHistory(path, point); err != nil {
			t.Fatalf("append usage history: %v", err)
		}
	}
	loaded, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load usage history: %v", err)
	}
	if len(loaded) != maxUsageHistoryPoints {
		t.Fatalf("loaded %d history points, want %d", len(loaded), maxUsageHistoryPoints)
	}
	if loaded[0].UsedPercent != 2 || loaded[len(loaded)-1].UsedPercent != maxUsageHistoryPoints+1 {
		t.Fatalf("loaded history range = %v..%v, want 2..%d", loaded[0].UsedPercent, loaded[len(loaded)-1].UsedPercent, maxUsageHistoryPoints+1)
	}
}

func TestUsageHistoryDeduplicatesBeforeApplyingPointLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history.jsonl")
	const sourcePointCount = maxUsageHistoryPoints + 12
	baseTime := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	for index := 0; index < sourcePointCount; index++ {
		for duplicate := 0; duplicate < 2; duplicate++ {
			point := HistoryPoint{
				At:          baseTime.Add(time.Duration(index*2+duplicate) * time.Minute).Format(time.RFC3339),
				UsedPercent: float64(index),
			}
			if err := appendUsageHistory(path, point); err != nil {
				t.Fatalf("append duplicate usage history: %v", err)
			}
		}
	}

	loaded, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load duplicate usage history: %v", err)
	}
	want := maxUsageHistoryPoints
	if len(loaded) != want {
		t.Fatalf("loaded %d deduplicated points, want %d", len(loaded), want)
	}
	if loaded[0].UsedPercent != 12 || loaded[len(loaded)-1].UsedPercent != float64(sourcePointCount-1) {
		t.Fatalf("deduplicated history range = %v..%v, want 12..%d", loaded[0].UsedPercent, loaded[len(loaded)-1].UsedPercent, sourcePointCount-1)
	}
	if loaded[0].At != baseTime.Add(25*time.Minute).Format(time.RFC3339) || loaded[len(loaded)-1].At != baseTime.Add(119*time.Minute).Format(time.RFC3339) {
		t.Fatalf("deduplicated history did not retain the latest occurrence: %s..%s", loaded[0].At, loaded[len(loaded)-1].At)
	}
}

func TestUsageHistoryKeepsCompleteRawArchiveBeforeWeeklyLimit(t *testing.T) {
	directory := t.TempDir()
	historyPath := filepath.Join(directory, "data", "usage-history.jsonl")
	rawPath := filepath.Join(directory, "data", "usage-history-raw.jsonl")
	service := &UsageService{
		historyFile:    historyPath,
		rawHistoryFile: rawPath,
	}
	const sourcePointCount = maxUsageHistoryPoints + 12
	baseTime := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	fiveHour := 24.0
	for index := 0; index < sourcePointCount; index++ {
		service.persistUsageHistoryPoint(HistoryPoint{
			At:                  baseTime.Add(time.Duration(index) * usageHistorySampleInterval).Format(time.RFC3339),
			UsedPercent:         float64(index),
			FiveHourUsedPercent: &fiveHour,
		})
	}

	raw, err := loadUsageHistoryRecords(rawPath)
	if err != nil {
		t.Fatalf("load raw usage history: %v", err)
	}
	if len(raw) != sourcePointCount {
		t.Fatalf("raw history contains %d points, want all %d scheduled samples", len(raw), sourcePointCount)
	}
	if len(service.weeklyHistory) != maxUsageHistoryPoints {
		t.Fatalf("weekly history contains %d points, want %d derived points", len(service.weeklyHistory), maxUsageHistoryPoints)
	}
	if service.weeklyHistory[0].UsedPercent != 12 || service.weeklyHistory[len(service.weeklyHistory)-1].UsedPercent != sourcePointCount-1 {
		t.Fatalf("weekly history range = %v..%v, want 12..%d", service.weeklyHistory[0].UsedPercent, service.weeklyHistory[len(service.weeklyHistory)-1].UsedPercent, sourcePointCount-1)
	}

	weeklyPath := usageHistoryMetricPath(historyPath, usageHistoryMetricWeekly)
	weeklySnapshot, exists, invalid, err := readUsageHistoryFile(weeklyPath)
	if err != nil || !exists || invalid {
		t.Fatalf("read weekly snapshot: exists=%v invalid=%v err=%v", exists, invalid, err)
	}
	if len(weeklySnapshot) != maxUsageHistoryPoints {
		t.Fatalf("weekly snapshot contains %d points, want %d", len(weeklySnapshot), maxUsageHistoryPoints)
	}
}

func TestSQLiteUsageHistoryStorePersistsAllSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "codex-meter.db")
	store, err := OpenUsageHistoryStore(path)
	if err != nil {
		t.Fatalf("open sqlite usage history: %v", err)
	}
	baseTime := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	for index := 0; index < maxUsageHistoryPoints+12; index++ {
		point := HistoryPoint{
			At:          baseTime.Add(time.Duration(index) * usageHistorySampleInterval).Format(time.RFC3339),
			UsedPercent: float64(index % 20),
			Stale:       index == 3,
		}
		if err := store.Insert(context.Background(), point); err != nil {
			t.Fatalf("insert sqlite usage history: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close sqlite usage history: %v", err)
	}

	store, err = OpenUsageHistoryStore(path)
	if err != nil {
		t.Fatalf("reopen sqlite usage history: %v", err)
	}
	defer store.Close()
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load sqlite usage history: %v", err)
	}
	if len(loaded) != maxUsageHistoryPoints+12 {
		t.Fatalf("sqlite history contains %d samples, want %d raw samples", len(loaded), maxUsageHistoryPoints+12)
	}
	if loaded[0].At != baseTime.Format(time.RFC3339) || loaded[len(loaded)-1].UsedPercent != float64((maxUsageHistoryPoints+11)%20) {
		t.Fatalf("sqlite history order/range = %#v..%#v", loaded[0], loaded[len(loaded)-1])
	}
	if !loaded[3].Stale {
		t.Fatalf("sqlite history did not preserve stale marker: %#v", loaded[3])
	}
	weekly := compactUsageHistoryMetric(loaded, usageHistoryMetricWeekly)
	if len(weekly) != 20 || len(weekly) > maxUsageHistoryPoints {
		t.Fatalf("sqlite source was compacted before weekly derivation: %d points", len(weekly))
	}
}

func TestAppendUsageHistoryPointToMetricMatchesFullRecompute(t *testing.T) {
	baseTime := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	fiveHourValue := 5.0
	// The collector only produces stale samples by cloning the last successful
	// point, so every stale step below reuses a value that already has a
	// successful record in the compacted list.
	steps := []HistoryPoint{
		{UsedPercent: 10, FiveHourUsedPercent: &fiveHourValue},
		{UsedPercent: 10, FiveHourUsedPercent: &fiveHourValue},
		{UsedPercent: 20, FiveHourUsedPercent: &fiveHourValue},
		{UsedPercent: 20, FiveHourUsedPercent: &fiveHourValue, Stale: true},
		{UsedPercent: 30, FiveHourUsedPercent: &fiveHourValue},
		{UsedPercent: 10, FiveHourUsedPercent: &fiveHourValue},
		{UsedPercent: 10, FiveHourUsedPercent: &fiveHourValue, Stale: true},
		{UsedPercent: 20, FiveHourUsedPercent: &fiveHourValue},
	}
	for index := range steps {
		steps[index].At = baseTime.Add(time.Duration(index) * usageHistorySampleInterval).Format(time.RFC3339)
	}

	for _, metric := range []usageHistoryMetric{usageHistoryMetricWeekly, usageHistoryMetricFiveHour} {
		var incremental []HistoryPoint
		for index, point := range steps {
			incremental = appendUsageHistoryPointToMetric(incremental, point, metric)
			want := compactUsageHistoryMetric(steps[:index+1], metric)
			if !usageHistoriesEqual(incremental, want) {
				t.Fatalf("metric %d step %d: incremental list = %#v, want full recompute %#v", metric, index, incremental, want)
			}
		}
	}
}

func TestAppendUsageHistoryPointToMetricAppliesLimitAfterDistinctValues(t *testing.T) {
	baseTime := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	raw := make([]HistoryPoint, 0, maxUsageHistoryPoints+13)
	var incremental []HistoryPoint
	for index := 0; index < maxUsageHistoryPoints+12; index++ {
		point := HistoryPoint{
			At:          baseTime.Add(time.Duration(index) * usageHistorySampleInterval).Format(time.RFC3339),
			UsedPercent: float64(index),
		}
		raw = append(raw, point)
		incremental = appendUsageHistoryPointToMetric(incremental, point, usageHistoryMetricWeekly)
	}
	if len(incremental) != maxUsageHistoryPoints {
		t.Fatalf("incremental list contains %d points, want %d", len(incremental), maxUsageHistoryPoints)
	}

	// A value that left the window because newer distinct values arrived is
	// re-admitted with its newest occurrence, and the oldest value drops out.
	repeated := HistoryPoint{
		At:          baseTime.Add(time.Duration(maxUsageHistoryPoints+12) * usageHistorySampleInterval).Format(time.RFC3339),
		UsedPercent: 12,
	}
	raw = append(raw, repeated)
	incremental = appendUsageHistoryPointToMetric(incremental, repeated, usageHistoryMetricWeekly)
	want := compactUsageHistoryMetric(raw, usageHistoryMetricWeekly)
	if !usageHistoriesEqual(incremental, want) {
		t.Fatalf("incremental list = %#v, want full recompute %#v", incremental, want)
	}
	if incremental[0].UsedPercent != 13 || incremental[len(incremental)-1].UsedPercent != 12 {
		t.Fatalf("incremental range = %v..%v, want 13..12", incremental[0].UsedPercent, incremental[len(incremental)-1].UsedPercent)
	}
}

func TestUsageHistoryRetainsIndependentMetricWindows(t *testing.T) {
	points := make([]HistoryPoint, 0, maxUsageHistoryPoints*2+24)
	for index := 0; index < maxUsageHistoryPoints*2+24; index++ {
		fiveHour := float64(index % 100)
		points = append(points, HistoryPoint{
			At:                  time.Date(2026, time.August, 27, 12, index, 0, 0, time.UTC).Format(time.RFC3339),
			UsedPercent:         float64((index / 2) % 60),
			FiveHourUsedPercent: &fiveHour,
		})
	}

	weekly := compactUsageHistoryMetric(points, usageHistoryMetricWeekly)
	fiveHour := compactUsageHistoryMetric(points, usageHistoryMetricFiveHour)
	if len(weekly) != maxUsageHistoryPoints || len(fiveHour) != maxUsageHistoryPoints {
		t.Fatalf("independent history lengths = weekly %d, five-hour %d; want %d each", len(weekly), len(fiveHour), maxUsageHistoryPoints)
	}
	if weekly[0].UsedPercent != 12 || weekly[1].UsedPercent != 13 || weekly[len(weekly)-1].UsedPercent != 59 {
		t.Fatalf("weekly history range = %v..%v, want 12..59", weekly[0].UsedPercent, weekly[len(weekly)-1].UsedPercent)
	}
	weeklyValues := make(map[float64]struct{}, len(weekly))
	for _, point := range weekly {
		if _, exists := weeklyValues[point.UsedPercent]; exists {
			t.Fatalf("weekly history contains duplicate value %v: %#v", point.UsedPercent, weekly)
		}
		weeklyValues[point.UsedPercent] = struct{}{}
	}
	if fiveHour[0].FiveHourUsedPercent == nil || *fiveHour[0].FiveHourUsedPercent != 72 {
		t.Fatalf("five-hour history starts at %v, want 72", fiveHour[0].FiveHourUsedPercent)
	}
	if fiveHour[0].At == fiveHour[1].At {
		t.Fatalf("five-hour history collapsed distinct timestamps: %#v", fiveHour[:2])
	}
}

func TestUsageHistoryMetricDeduplicationIgnoresOtherWindow(t *testing.T) {
	firstFiveHour := 12.0
	secondFiveHour := 13.0
	firstAt := "2026-08-27T12:00:00Z"
	secondAt := "2026-08-27T12:05:00Z"
	points := []HistoryPoint{
		{At: firstAt, UsedPercent: 55, FiveHourUsedPercent: &firstFiveHour},
		{At: secondAt, UsedPercent: 55, FiveHourUsedPercent: &secondFiveHour},
	}

	weekly := compactUsageHistoryMetric(points, usageHistoryMetricWeekly)
	if len(weekly) != 1 {
		t.Fatalf("weekly history used five-hour changes during deduplication: %#v", weekly)
	}
	if weekly[0].At != secondAt || weekly[0].FiveHourUsedPercent == nil || *weekly[0].FiveHourUsedPercent != secondFiveHour {
		t.Fatalf("weekly history did not retain the latest weekly point: %#v", weekly[0])
	}

	fiveHour := compactUsageHistoryMetric(points, usageHistoryMetricFiveHour)
	if len(fiveHour) != 2 {
		t.Fatalf("five-hour history lost its own value change: %#v", fiveHour)
	}
}

func TestUsageHistoryDeduplicationKeepsLatestRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history.jsonl")
	firstAt := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	latestAt := time.Date(2026, time.August, 27, 12, 5, 0, 0, time.UTC).Format(time.RFC3339)
	for _, point := range []HistoryPoint{
		{At: firstAt, UsedPercent: 12},
		{At: latestAt, UsedPercent: 12},
	} {
		if err := appendUsageHistory(path, point); err != nil {
			t.Fatalf("append repeated usage history: %v", err)
		}
	}

	loaded, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load repeated usage history: %v", err)
	}
	if len(loaded) != 1 || loaded[0].At != latestAt {
		t.Fatalf("deduplicated record = %#v, want latest record at %s", loaded, latestAt)
	}
}

func TestUsageHistoryLoadsFiveHourPercent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history.jsonl")
	fiveHour := 37.5
	point := HistoryPoint{
		At:                  "2026-08-28T01:02:03Z",
		UsedPercent:         12,
		FiveHourUsedPercent: &fiveHour,
	}
	if err := appendUsageHistory(path, point); err != nil {
		t.Fatalf("append usage history: %v", err)
	}
	loaded, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load usage history: %v", err)
	}
	if len(loaded) != 1 || loaded[0].FiveHourUsedPercent == nil || *loaded[0].FiveHourUsedPercent != fiveHour {
		t.Fatalf("loaded five-hour usage = %#v, want %v", loaded, fiveHour)
	}
}

func TestWriteUsageHistoryKeepsPreviousSnapshotAsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history.jsonl")
	initial := []HistoryPoint{
		{At: "2026-08-28T01:00:00Z", UsedPercent: 10},
		{At: "2026-08-28T01:05:00Z", UsedPercent: 11},
	}
	updated := append(append([]HistoryPoint(nil), initial...), HistoryPoint{
		At: "2026-08-28T01:10:00Z", UsedPercent: 12,
	})

	if err := writeUsageHistory(path, initial); err != nil {
		t.Fatalf("write initial usage history: %v", err)
	}
	if err := writeUsageHistory(path, updated); err != nil {
		t.Fatalf("write updated usage history: %v", err)
	}

	current, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load current usage history: %v", err)
	}
	backup, _, invalid, err := readUsageHistoryFile(usageHistoryBackupPath(path))
	if err != nil {
		t.Fatalf("load usage history backup: %v", err)
	}
	if invalid || len(current) != len(updated) || len(backup) != len(initial) {
		t.Fatalf("history snapshots current=%#v backup=%#v invalid=%v", current, backup, invalid)
	}
}

func TestRewriteUsageHistoryIfChangedPersistsCompactedValuesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history-weekly.jsonl")
	raw := []HistoryPoint{
		{At: "2026-08-28T01:00:00Z", UsedPercent: 10},
		{At: "2026-08-28T01:05:00Z", UsedPercent: 10},
		{At: "2026-08-28T01:10:00Z", UsedPercent: 11},
	}
	for _, point := range raw {
		if err := appendUsageHistory(path, point); err != nil {
			t.Fatalf("append raw usage history: %v", err)
		}
	}
	compacted := compactUsageHistoryMetric(raw, usageHistoryMetricWeekly)

	if err := rewriteUsageHistoryIfChanged(path, compacted); err != nil {
		t.Fatalf("rewrite compacted usage history: %v", err)
	}
	if err := rewriteUsageHistoryIfChanged(path, compacted); err != nil {
		t.Fatalf("skip unchanged compacted usage history: %v", err)
	}

	current, exists, invalid, err := readUsageHistoryFile(path)
	if err != nil || !exists || invalid {
		t.Fatalf("read rewritten usage history: exists=%v invalid=%v err=%v", exists, invalid, err)
	}
	if len(current) != 2 || current[0].At != raw[1].At || current[1].At != raw[2].At {
		t.Fatalf("rewritten usage history = %#v, want latest unique values", current)
	}
	backup, backupExists, backupInvalid, err := readUsageHistoryFile(usageHistoryBackupPath(path))
	if err != nil || !backupExists || backupInvalid || len(backup) != len(raw) {
		t.Fatalf("compaction backup = %#v, exists=%v invalid=%v err=%v", backup, backupExists, backupInvalid, err)
	}
}

func TestLoadUsageHistoryFallsBackToBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history.jsonl")
	initial := []HistoryPoint{
		{At: "2026-08-28T01:00:00Z", UsedPercent: 10},
		{At: "2026-08-28T01:05:00Z", UsedPercent: 11},
	}
	updated := append(append([]HistoryPoint(nil), initial...), HistoryPoint{
		At: "2026-08-28T01:10:00Z", UsedPercent: 12,
	})

	if err := writeUsageHistory(path, initial); err != nil {
		t.Fatalf("write initial usage history: %v", err)
	}
	if err := writeUsageHistory(path, updated); err != nil {
		t.Fatalf("write updated usage history: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not-json\n"), 0o600); err != nil {
		t.Fatalf("corrupt current usage history: %v", err)
	}

	loaded, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load fallback usage history: %v", err)
	}
	if len(loaded) != len(initial) || loaded[0].At != initial[0].At || loaded[1].At != initial[1].At {
		t.Fatalf("fallback usage history = %#v, want %#v", loaded, initial)
	}
}

func TestUsageRefreshDoesNotPersistHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history.jsonl")
	service := &UsageService{
		cfg: Config{AccessToken: "oauth-token", UserAgent: "test-user-agent"},
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":604800}}}`)),
				Header:     make(http.Header),
			}, nil
		})},
		historyFile: path,
	}

	usage, err := service.Get(context.Background(), true)
	if err != nil {
		t.Fatalf("refresh usage: %v", err)
	}
	if len(usage.History) != 0 || len(service.history) != 0 {
		t.Fatalf("refresh unexpectedly persisted history: response=%#v service=%#v", usage.History, service.history)
	}

	service.collectUsageHistory(context.Background())
	service.collectUsageHistory(context.Background())
	loaded, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load collected history: %v", err)
	}
	if len(loaded) != 1 || loaded[0].UsedPercent != 12 || len(service.history) != 1 {
		t.Fatalf("collected history = %#v, want one 12%% sample", loaded)
	}
}

func TestNextUsageHistorySampleAtReturnsNextFiveMinuteBoundary(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 8, 28, 14, 37, 29, 0, location)

	next := nextUsageHistorySampleAt(now)
	want := time.Date(2026, 8, 28, 14, 40, 0, 0, location)
	if !next.Equal(want) {
		t.Fatalf("nextUsageHistorySampleAt() = %s, want %s", next, want)
	}
}

func TestNextUsageHistorySampleAtRollsOverToNextHour(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 8, 28, 23, 59, 59, 0, location)

	next := nextUsageHistorySampleAt(now)
	want := time.Date(2026, 8, 29, 0, 0, 0, 0, location)
	if !next.Equal(want) {
		t.Fatalf("nextUsageHistorySampleAt() = %s, want %s", next, want)
	}
}

func TestScheduledUsageCollectionFallsBackToLastSuccessfulPoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history.jsonl")
	fiveHour := 17.0
	lastPoint := HistoryPoint{
		At:                  time.Now().UTC().Add(-usageHistorySampleInterval - time.Minute).Format(time.RFC3339),
		UsedPercent:         42,
		FiveHourUsedPercent: &fiveHour,
	}
	service := &UsageService{
		cfg:         Config{AccessToken: "oauth-token", UserAgent: "test-user-agent"},
		client:      &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("upstream unavailable") })},
		history:     []HistoryPoint{lastPoint},
		historyFile: path,
	}

	service.collectUsageHistory(context.Background())
	loaded, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load fallback history: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Stale || loaded[0].At != lastPoint.At || loaded[0].UsedPercent != lastPoint.UsedPercent || loaded[0].FiveHourUsedPercent == nil || *loaded[0].FiveHourUsedPercent != fiveHour {
		t.Fatalf("fallback history = %#v, want existing successful point %#v", loaded, lastPoint)
	}
	lastSuccessful, ok := service.lastSuccessfulHistoryPoint()
	if !ok || lastSuccessful.Stale || lastSuccessful.At != lastPoint.At || lastSuccessful.UsedPercent != lastPoint.UsedPercent {
		t.Fatalf("last successful history = %#v, %v; want %#v", lastSuccessful, ok, lastPoint)
	}
}

func TestWhamRequestUsesOAuthHeadersAndGET(t *testing.T) {
	var captured *http.Request
	service := &UsageService{
		cfg: Config{
			AccessToken:       "oauth-token",
			UpstreamCookie:    "oai-did=device-id; session=test",
			ChatGPTAccountID:  "account-id",
			ClientBuildNumber: "9758774",
			ClientVersion:     "prod-test",
			DeviceID:          "device-id",
			SessionID:         "session-id",
			ClientObservation: "v1.r.p.test",
			UpstreamReferer:   "https://chatgpt.com/codex/cloud/settings/analytics",
			UserAgent:         "test-user-agent",
		},
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			captured = request
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":604800}}}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	if _, err := service.queryWhamUsage(context.Background(), service.cfg); err != nil {
		t.Fatalf("query wham usage: %v", err)
	}
	if captured == nil {
		t.Fatal("upstream request was not captured")
	}
	if captured.Method != http.MethodGet || captured.URL.Path != "/backend-api/wham/usage" {
		t.Fatalf("request = %s %s, want GET /backend-api/wham/usage", captured.Method, captured.URL.Path)
	}
	wantHeaders := map[string]string{
		"Authorization":               "Bearer oauth-token",
		"Cookie":                      "oai-did=device-id; session=test",
		"Oai-Client-Build-Number":     "9758774",
		"Oai-Client-Version":          "prod-test",
		"Oai-Device-Id":               "device-id",
		"Oai-Session-Id":              "session-id",
		"X-Oai-Is-Client-Observation": "v1.r.p.test",
		"X-Openai-Target-Path":        "/backend-api/wham/usage",
		"X-Openai-Target-Route":       "/backend-api/wham/usage",
		"Oai-Language":                "zh-CN",
		"Cache-Control":               "no-cache",
		"Pragma":                      "no-cache",
		"Accept":                      "*/*",
		"Sec-Fetch-Site":              "same-origin",
		"Sec-Fetch-Mode":              "cors",
		"Sec-Fetch-Dest":              "empty",
		"Priority":                    "u=1, i",
		"Referer":                     "https://chatgpt.com/codex/cloud/settings/analytics",
		"User-Agent":                  "test-user-agent",
	}
	for name, want := range wantHeaders {
		if got := captured.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

func TestConfigViewMasksProxyCredentials(t *testing.T) {
	service := &UsageService{cfg: Config{UpstreamProxy: "socks5://alice:secret-password@127.0.0.1:1080"}}
	view := service.ConfigView()
	if strings.Contains(view.ProxyURL, "secret-password") {
		t.Fatalf("config view leaked proxy credentials: %q", view.ProxyURL)
	}
	if view.ProxyURL != "socks5://alice:****@127.0.0.1:1080" {
		t.Fatalf("masked proxy URL = %q, want %q", view.ProxyURL, "socks5://alice:****@127.0.0.1:1080")
	}

	service.cfg.UpstreamProxy = "http://127.0.0.1:7890"
	if view := service.ConfigView(); view.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("proxy URL without credentials = %q, want unchanged", view.ProxyURL)
	}
}

func TestProxySchemes(t *testing.T) {
	for _, test := range []struct {
		name       string
		raw        string
		wantScheme string
	}{{name: "http", raw: "http://127.0.0.1:7890", wantScheme: "http"},
		{name: "https", raw: "https://127.0.0.1:7890", wantScheme: "https"},
		{name: "socks5", raw: "socks5://127.0.0.1:1080", wantScheme: "socks5"},
		{name: "socket5 alias", raw: "socket5://127.0.0.1:1080", wantScheme: "socks5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseProxyURL(test.raw)
			if err != nil {
				t.Fatalf("parse proxy URL: %v", err)
			}
			if parsed == nil || parsed.Scheme != test.wantScheme {
				t.Fatalf("parsed proxy = %#v, want scheme %q", parsed, test.wantScheme)
			}
			client, err := newUpstreamClient(test.raw)
			if err != nil {
				t.Fatalf("new upstream client: %v", err)
			}
			transport, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
			}
			if test.wantScheme == "socks5" {
				if transport.Dial == nil || transport.Proxy != nil {
					t.Fatalf("SOCKS5 transport was not configured through Dial: %#v", transport)
				}
				return
			}
			proxyFunc, err := buildProxyFunc(test.raw)
			if err != nil {
				t.Fatalf("build proxy function: %v", err)
			}
			proxyURL, err := proxyFunc(httptest.NewRequest(http.MethodGet, "https://chatgpt.com", nil))
			if err != nil || proxyURL == nil || proxyURL.Scheme != test.wantScheme {
				t.Fatalf("proxy function returned %#v, %v", proxyURL, err)
			}
		})
	}
}

func TestProxySchemeValidation(t *testing.T) {
	if _, err := parseProxyURL("socket://127.0.0.1:1080"); err == nil {
		t.Fatal("unsupported proxy scheme was accepted")
	}
	if _, err := parseProxyURL("127.0.0.1:7890"); err == nil {
		t.Fatal("proxy without a scheme was accepted")
	}
}

func TestProxyTestSuccessMessage(t *testing.T) {
	if got := proxyTestSuccessMessage("http://127.0.0.1:7890"); got != "代理连接成功，可访问 chatgpt.com" {
		t.Fatalf("proxy success message = %q", got)
	}
	if got := proxyTestSuccessMessage(""); got != "直连成功，可访问 chatgpt.com" {
		t.Fatalf("direct success message = %q", got)
	}
}

func TestConfigViewDoesNotExposeUsageProvider(t *testing.T) {
	service := &UsageService{cfg: Config{
		AccessToken:      "oauth-token",
		UpstreamCookie:   "session=secret-cookie",
		ChatGPTAccountID: "account-id",
		CacheTTL:         time.Minute,
	}}
	data, err := json.Marshal(service.ConfigView())
	if err != nil {
		t.Fatalf("marshal config view: %v", err)
	}
	if strings.Contains(string(data), `"usage`) {
		t.Fatalf("config view exposes removed usage provider setting: %s", data)
	}
	if strings.Contains(string(data), "secret-cookie") {
		t.Fatalf("config view exposes upstream cookie: %s", data)
	}
	if !strings.Contains(string(data), `"cookie_configured":true`) {
		t.Fatalf("config view did not expose cookie status: %s", data)
	}
}

func TestConfigFileRoundTrip(t *testing.T) {
	service := &UsageService{cfg: Config{
		ConfigPath:       t.TempDir() + "/config.json",
		AccessToken:      "old-token",
		ChatGPTAccountID: "old-account",
		CacheTTL:         time.Minute,
	}}
	content := `{"openai":{"access_token":"new-token","chatgpt_account_id":"new-account"}}`

	view, err := service.UpdateConfigFile(content)
	if err != nil {
		t.Fatalf("update config file: %v", err)
	}
	if view.ConfigFile != service.cfg.ConfigPath {
		t.Fatalf("config file path = %q, want %q", view.ConfigFile, service.cfg.ConfigPath)
	}
	if view.Content != content+"\n" {
		t.Fatalf("config file content = %q, want %q", view.Content, content+"\n")
	}
	if got := service.currentConfig().AccessToken; got != "new-token" {
		t.Fatalf("active access token = %q, want new-token", got)
	}
}

func TestApplyConfigUpdateSupportsCapturedOpenAIContext(t *testing.T) {
	old := Config{
		AccessToken:      "old-token",
		ChatGPTAccountID: "old-account",
		UserAgent:        "old-agent",
		CacheTTL:         time.Minute,
	}
	token := "new-token"
	cookie := "oai-did=device; session=browser"
	account := "new-account"
	build := "9758774"
	version := "prod-test"
	device := "device-id"
	session := "session-id"
	observation := "v1.r.p.test"
	referer := "https://chatgpt.com/codex/cloud/settings/analytics"
	proxy := "http://127.0.0.1:7890"
	update := ConfigUpdate{
		AccessToken:       &token,
		UpstreamCookie:    &cookie,
		ChatGPTAccountID:  &account,
		ClientBuildNumber: &build,
		ClientVersion:     &version,
		DeviceID:          &device,
		SessionID:         &session,
		ClientObservation: &observation,
		Referer:           &referer,
		ProxyURL:          &proxy,
	}

	got, err := applyConfigUpdate(old, update)
	if err != nil {
		t.Fatalf("apply config update: %v", err)
	}
	if got.AccessToken != token || got.UpstreamCookie != cookie || got.ChatGPTAccountID != account || got.ClientBuildNumber != build || got.ClientVersion != version || got.DeviceID != device || got.SessionID != session || got.ClientObservation != observation || got.UpstreamReferer != referer || got.UpstreamProxy != proxy {
		t.Fatalf("captured context was not applied: %#v", got)
	}
}

func TestEmbeddedAudioDecodes(t *testing.T) {
	for _, kind := range []string{"normal", "warning", "critical"} {
		data, err := embeddedAudio(kind)
		if err != nil {
			t.Fatalf("decode %s audio: %v", kind, err)
		}
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
			t.Fatalf("%s audio is not a WAV payload", kind)
		}
	}
}

func TestDailyUsageAnalyticsUsesBothEndpoints(t *testing.T) {
	var paths []string
	var queries []url.Values
	testDate := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	service := &UsageService{
		cfg: Config{
			AccessToken:      "oauth-token",
			ChatGPTAccountID: "account-id",
			UserAgent:        "test-user-agent",
		},
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			paths = append(paths, request.URL.Path)
			queries = append(queries, request.URL.Query())
			body := `{"data":[]}`
			if request.URL.Path == "/backend-api/wham/usage/daily-token-usage-breakdown" {
				body = fmt.Sprintf(`{"data":[{"date":"%s","product_surface_usage_values":{"desktop_app":12.5},"models":[{"model":"gpt-5.6-luna","speed":"standard","credits":10},{"model":"gpt-5.6-luna","speed":"fast","credits":2.5}]}]}`, testDate)
			}
			if request.URL.Path == "/backend-api/wham/analytics/daily-workspace-usage-counts" {
				body = fmt.Sprintf(`{"data":[{"date":"%s","totals":{"users":1,"threads":2,"turns":3,"credits":12.5,"uncached_text_input_tokens":10,"cached_text_input_tokens":20,"text_output_tokens":30,"text_total_tokens":60}}]}`, testDate)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	analytics, err := service.queryUsageAnalytics(context.Background())
	if err != nil {
		t.Fatalf("query usage analytics: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/backend-api/wham/usage/daily-token-usage-breakdown" || paths[1] != "/backend-api/wham/analytics/daily-workspace-usage-counts" {
		t.Fatalf("upstream paths = %#v", paths)
	}
	if len(queries) != 2 || queries[0].Get("start_date") == "" || queries[0].Get("end_date") == "" || queries[0].Get("group_by") != "day" || queries[1].Get("workspace_user") != "true" {
		t.Fatalf("upstream queries = %#v", queries)
	}
	if len(analytics.Days) != 7 || analytics.Days[6].Date != testDate || analytics.Days[6].TokenUsagePercent != 12.5 || analytics.Days[6].Turns != 3 || analytics.Days[6].TextTotalTokens != 60 || len(analytics.Days[6].Models) != 1 || analytics.Days[6].Models[0].Model != "gpt-5.6-luna" || analytics.Days[6].Models[0].UsagePercent != 12.5 {
		t.Fatalf("unexpected analytics payload: %#v", analytics)
	}
}

func TestDailyUsageEndpointAddsDateRange(t *testing.T) {
	rangeValue := analyticsDateRange{StartDate: "2026-08-01", EndDate: "2026-08-30"}
	for _, test := range []struct {
		name          string
		endpoint      string
		workspaceUser bool
	}{
		{name: "token usage", endpoint: dailyTokenUsageEndpoint},
		{name: "workspace usage", endpoint: dailyWorkspaceUsageEndpoint, workspaceUser: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint, err := dailyUsageEndpoint(test.endpoint, rangeValue, test.workspaceUser)
			if err != nil {
				t.Fatalf("dailyUsageEndpoint() error = %v", err)
			}
			parsed, err := url.Parse(endpoint)
			if err != nil {
				t.Fatalf("parse endpoint: %v", err)
			}
			query := parsed.Query()
			if query.Get("start_date") != rangeValue.StartDate || query.Get("end_date") != rangeValue.EndDate || query.Get("group_by") != "day" {
				t.Fatalf("query = %v", query)
			}
			if got := query.Get("workspace_user"); (got == "true") != test.workspaceUser {
				t.Fatalf("workspace_user = %q, want enabled = %v", got, test.workspaceUser)
			}
		})
	}
}

func TestParseAnalyticsDateRangeDefaultsToPreviousSevenDays(t *testing.T) {
	now := time.Date(2026, 8, 27, 14, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	dateRange, err := parseAnalyticsDateRange(nil, now)
	if err != nil {
		t.Fatalf("parseAnalyticsDateRange() error = %v", err)
	}
	if dateRange.StartDate != "2026-08-20" || dateRange.EndDate != "2026-08-26" {
		t.Fatalf("date range = %#v, want 2026-08-20..2026-08-26", dateRange)
	}
}

func TestParseAnalyticsDateRangeValidatesCustomRange(t *testing.T) {
	now := time.Date(2026, 8, 27, 14, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	dateRange, err := parseAnalyticsDateRange(url.Values{
		"start_date": []string{"2026-08-01"},
		"end_date":   []string{"2026-08-27"},
	}, now)
	if err != nil {
		t.Fatalf("parseAnalyticsDateRange() error = %v", err)
	}
	if dateRange.StartDate != "2026-08-01" || dateRange.EndDate != "2026-08-27" {
		t.Fatalf("date range = %#v", dateRange)
	}
	for _, test := range []struct {
		name  string
		query url.Values
	}{
		{name: "missing end", query: url.Values{"start_date": []string{"2026-08-01"}}},
		{name: "reversed", query: url.Values{"start_date": []string{"2026-08-30"}, "end_date": []string{"2026-08-01"}}},
		{name: "future", query: url.Values{"start_date": []string{"2026-08-27"}, "end_date": []string{"2026-08-28"}}},
		{name: "too long", query: url.Values{"start_date": []string{"2025-01-01"}, "end_date": []string{"2026-08-27"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseAnalyticsDateRange(test.query, now); err == nil {
				t.Fatal("parseAnalyticsDateRange() unexpectedly accepted invalid range")
			}
		})
	}
}

func TestMergeUsageAnalyticsUsesRequestedDateRange(t *testing.T) {
	rangeValue := analyticsDateRange{StartDate: "2026-08-01", EndDate: "2026-08-30"}
	analytics := mergeUsageAnalyticsAtRange(nil, nil, rangeValue, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	if len(analytics.Days) != 30 || analytics.Days[0].Date != rangeValue.StartDate || analytics.Days[len(analytics.Days)-1].Date != rangeValue.EndDate {
		t.Fatalf("days = %d, range = %s..%s", len(analytics.Days), analytics.Days[0].Date, analytics.Days[len(analytics.Days)-1].Date)
	}
	if analytics.StartDate != rangeValue.StartDate || analytics.EndDate != rangeValue.EndDate {
		t.Fatalf("response range = %s..%s", analytics.StartDate, analytics.EndDate)
	}
}

func TestMergeUsageAnalyticsUsesPreviousSevenDays(t *testing.T) {
	tokenUsage := make([]dailyTokenUsagePoint, 0, 8)
	workspaceUsage := make([]dailyWorkspaceUsagePoint, 0, 8)
	for day := 1; day <= 8; day++ {
		date := fmt.Sprintf("2026-08-%02d", day)
		tokenUsage = append(tokenUsage, dailyTokenUsagePoint{
			Date:                      date,
			ProductSurfaceUsageValues: map[string]float64{"desktop_app": float64(day)},
		})
		workspaceUsage = append(workspaceUsage, dailyWorkspaceUsagePoint{
			Date: date,
			Totals: dailyWorkspaceUsageTotals{
				Users: 1, Turns: int64(day), TextTotalTokens: int64(day * 100),
			},
		})
	}

	analytics := mergeUsageAnalyticsAt(tokenUsage, workspaceUsage, time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC))
	if len(analytics.Days) != 7 {
		t.Fatalf("days = %d, want 7", len(analytics.Days))
	}
	if analytics.Days[0].Date != "2026-08-01" || analytics.Days[6].Date != "2026-08-07" {
		t.Fatalf("date range = %s..%s, want 2026-08-01..2026-08-07", analytics.Days[0].Date, analytics.Days[6].Date)
	}
	if analytics.Summary.Turns != 28 || analytics.Summary.TextTotalTokens != 2800 || analytics.Summary.Users != 1 {
		t.Fatalf("summary = %#v", analytics.Summary)
	}
}

func TestResetStatusResponseDecodesPrediction(t *testing.T) {
	raw := []byte(`{
        "data":{
            "latest_reset":{
                "id":"2090947107469558188",
                "reset_type":"banked",
                "announced_at":"2026-08-21T23:40:12.000Z",
                "text":"The banked reset will be there by 8pm.",
                "source":{"type":"x_post","author":"thsottiaux","url":"https://x.com/thsottiaux/status/2090947107469558188"}
            },
            "active_watch":null,
            "stats":{"total":45,"last_reset_at":"2026-08-21T23:40:12.000Z","days_since_last":0.4,"avg_interval_days":7.7}
        },
        "meta":{"api_version":"v1","generated_at":"2026-08-22T08:34:14.818Z"}
    }`)

	var envelope resetStatusEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode reset status: %v", err)
	}
	if envelope.Data.LatestReset == nil || envelope.Data.LatestReset.ResetType != "banked" {
		t.Fatalf("unexpected latest reset: %#v", envelope.Data.LatestReset)
	}
	if envelope.Data.LatestReset.Source == nil || envelope.Data.LatestReset.Source.Author != "thsottiaux" {
		t.Fatalf("unexpected reset source: %#v", envelope.Data.LatestReset.Source)
	}
	if envelope.Data.Stats.Total != 45 || envelope.Data.Stats.AvgIntervalDays != 7.7 {
		t.Fatalf("unexpected reset stats: %#v", envelope.Data.Stats)
	}
}

func TestResetPredictionRequestUsesPublicEndpoints(t *testing.T) {
	var captureMu sync.Mutex
	var captured []*http.Request
	service := &UsageService{
		cfg: Config{CacheTTL: time.Minute},
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			captureMu.Lock()
			captured = append(captured, request)
			captureMu.Unlock()
			body := `{"data":{"active_watch":null,"stats":{"total":0}},"meta":{"generated_at":"2026-08-22T08:34:14Z"}}`
			if request.URL.String() == resetHistoryEndpoint {
				body = `{"events":[{"tweet_id":"2090","tweet_url":"https://x.com/thsottiaux/status/2090","text":"Reset complete","announced_at":"2026-08-21T23:40:12Z","reset_type":"regular","source":"webhook"}]}`
			}
			if request.URL.String() == resetHomepageEndpoint {
				body = `<div data-role="watch-poll" data-no="100" data-yes="1042"></div>`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	prediction, err := service.queryResetPrediction(context.Background())
	if err != nil {
		t.Fatalf("query reset prediction: %v", err)
	}
	if prediction.CommunityPoll == nil || prediction.CommunityPoll.YesVotes != 1042 || prediction.CommunityPoll.NoVotes != 100 {
		t.Fatalf("community poll = %#v", prediction.CommunityPoll)
	}
	captureMu.Lock()
	defer captureMu.Unlock()
	if len(captured) != 3 {
		t.Fatalf("captured %d requests, want status, history and homepage", len(captured))
	}
	endpoints := map[string]string{}
	for _, request := range captured {
		endpoints[request.URL.String()] = request.Method
	}
	for endpoint, wantMethod := range map[string]string{
		resetStatusEndpoint:   http.MethodGet,
		resetHistoryEndpoint:  http.MethodGet,
		resetHomepageEndpoint: http.MethodGet,
	} {
		if endpoints[endpoint] != wantMethod {
			t.Fatalf("request %s = %q, want %q", endpoint, endpoints[endpoint], wantMethod)
		}
	}
	if got := captured[0].Header.Get("User-Agent"); got != "codex-usage-dashboard/1.0" {
		t.Fatalf("User-Agent = %q, want dashboard user agent", got)
	}
}

func TestResetStatusResponseDecodesSignals(t *testing.T) {
	tests := []struct {
		name          string
		data          string
		wantScheduled bool
		wantTime      string
		wantWatch     bool
		wantChance    bool
		chance        float64
	}{
		{name: "legacy idle response", data: `{"latest_reset":null,"active_watch":null,"stats":{"total":56}}`},
		{name: "active watch", data: `{"active_watch":{"level":"strong","reset_chance_percent":78,"forecast_window":"next 24 hours","text":"A reset may be coming.","source":{"type":"x_post","url":"https://codex-resets.com/"}}}`, wantWatch: true, wantChance: true, chance: 78},
		{name: "watch without probability", data: `{"active_watch":{"level":"elevated","reset_chance_percent":null,"forecast_window":"next week"}}`, wantWatch: true},
		{name: "scheduled without watch", data: `{"scheduled_reset":{"id":"scheduled","status":"scheduled","reset_type":"regular","announced_at":"2026-10-02T02:00:00Z","scheduled_for":"2026-10-02T17:00:00Z","text":"A reset has been scheduled.","source":{"type":"x_post","author":"thsottiaux","url":"https://codex-resets.com/"}},"active_watch":null}`, wantScheduled: true, wantTime: "2026-10-02T17:00:00Z"},
		{name: "scheduled time not yet announced", data: `{"scheduled_reset":{"id":"scheduled","status":"scheduled","scheduled_for":null}}`, wantScheduled: true},
		{name: "passed time still awaiting evidence", data: `{"scheduled_reset":{"id":"scheduled","status":"scheduled","scheduled_for":"2025-01-01T00:00:00Z"}}`, wantScheduled: true, wantTime: "2025-01-01T00:00:00Z"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var envelope resetStatusEnvelope
			if err := json.Unmarshal([]byte(`{"data":`+test.data+`,"meta":{"api_version":"v1"}}`), &envelope); err != nil {
				t.Fatalf("decode reset status: %v", err)
			}
			scheduled := envelope.Data.ScheduledReset
			if (scheduled != nil) != test.wantScheduled {
				t.Fatalf("scheduled reset = %#v, want present %v", scheduled, test.wantScheduled)
			}
			if scheduled != nil {
				if scheduled.Status != "scheduled" || scheduled.ID != "scheduled" {
					t.Fatalf("scheduled reset identity = %#v", scheduled)
				}
				if test.wantTime == "" {
					if scheduled.ScheduledFor != nil {
						t.Fatalf("scheduled_for = %q, want null", *scheduled.ScheduledFor)
					}
				} else if scheduled.ScheduledFor == nil || *scheduled.ScheduledFor != test.wantTime {
					t.Fatalf("scheduled_for = %v, want %q", scheduled.ScheduledFor, test.wantTime)
				}
			}
			watch := envelope.Data.ActiveWatch
			if (watch != nil) != test.wantWatch {
				t.Fatalf("active watch = %#v, want present %v", watch, test.wantWatch)
			}
			if watch != nil {
				if (watch.ResetChancePercent != nil) != test.wantChance {
					t.Fatalf("probability = %v, want present %v", watch.ResetChancePercent, test.wantChance)
				}
				if test.wantChance && *watch.ResetChancePercent != test.chance {
					t.Fatalf("probability = %v, want %v", *watch.ResetChancePercent, test.chance)
				}
				encoded, err := json.Marshal(watch)
				if err != nil {
					t.Fatalf("encode watch: %v", err)
				}
				if !test.wantChance && !strings.Contains(string(encoded), `"reset_chance_percent":null`) {
					t.Fatalf("unknown probability was not preserved as null: %s", encoded)
				}
			}
		})
	}
}

func TestScheduledPredictionEndpoint(t *testing.T) {
	statusJSON := `{"data":{"latest_reset":{"id":"executed","reset_type":"banked","announced_at":"2026-09-29T19:00:00Z","text":"Reset completed."},"scheduled_reset":{"id":"scheduled","status":"scheduled","reset_type":"regular","announced_at":"2026-10-02T02:00:00Z","scheduled_for":"2026-10-02T17:00:00Z","text":"A reset has been scheduled.","source":{"type":"x_post","author":"thsottiaux","url":"https://codex-resets.com/"}},"active_watch":null,"stats":{"total":56}},"meta":{"generated_at":"2026-10-02T03:00:00Z"}}`
	tests := []struct {
		name           string
		authorized     bool
		upstreamStatus int
		wantStatus     int
	}{
		{name: "scheduled signal on prefixed protected route", authorized: true, upstreamStatus: http.StatusOK, wantStatus: http.StatusOK},
		{name: "authentication is still required", upstreamStatus: http.StatusOK, wantStatus: http.StatusUnauthorized},
		{name: "status failure is not reported as idle", authorized: true, upstreamStatus: http.StatusServiceUnavailable, wantStatus: http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requestMu sync.Mutex
			requests := make(map[string]int)
			cfg := Config{BasePath: "/codex", CacheTTL: time.Minute, BasicAuthEnabled: true, BasicAuthUsername: "test-user", BasicAuthPassword: "test-password"}
			service := &UsageService{cfg: cfg, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requestMu.Lock()
				defer requestMu.Unlock()
				requests[request.URL.String()]++
				if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("X-App-API-Key") != "" {
					t.Errorf("public upstream request unexpectedly contained account credentials")
				}
				body := statusJSON
				status := test.upstreamStatus
				if request.URL.String() == resetHistoryEndpoint {
					status = http.StatusOK
					body = `{"events":[{"tweet_id":"executed","reset_type":"banked","announced_at":"2026-09-29T19:00:00Z","text":"Reset completed."}]}`
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}}
			server := NewServer(cfg, service)
			request := httptest.NewRequest(http.MethodGet, "/codex/api/prediction?force=true", nil)
			if test.authorized {
				request.SetBasicAuth("test-user", "test-password")
			}
			recorder := httptest.NewRecorder()
			server.handler.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if test.wantStatus == http.StatusOK {
				var prediction ResetPrediction
				if err := json.Unmarshal(recorder.Body.Bytes(), &prediction); err != nil {
					t.Fatalf("decode prediction: %v", err)
				}
				if prediction.ScheduledReset == nil || prediction.ScheduledReset.ScheduledFor == nil || *prediction.ScheduledReset.ScheduledFor != "2026-10-02T17:00:00Z" {
					t.Fatalf("scheduled reset missing from API response: %#v", prediction.ScheduledReset)
				}
				if prediction.LatestReset == nil || prediction.LatestReset.ID != "executed" || len(prediction.History) != 1 || prediction.History[0].ID != "executed" || prediction.Stats.Total != 56 {
					t.Fatalf("pending announcement changed executed history or statistics: %#v", prediction)
				}
				if prediction.ActiveWatch != nil || prediction.CommunityPoll != nil {
					t.Fatalf("scheduled reset incorrectly acquired probability or poll: %#v", prediction)
				}
				requestMu.Lock()
				defer requestMu.Unlock()
				if requests[resetStatusEndpoint] != 1 || requests[resetHistoryEndpoint] != 1 || requests[resetHomepageEndpoint] != 0 {
					t.Fatalf("scheduled requests = %v, want status and history without poll homepage", requests)
				}
			}
		})
	}
}

func TestResetPredictionKeepsStatusOnSupplementaryFailures(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "history unavailable", endpoint: resetHistoryEndpoint},
		{name: "homepage unavailable", endpoint: resetHomepageEndpoint},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &UsageService{cfg: Config{CacheTTL: time.Minute}, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				status := http.StatusOK
				body := `{"data":{"latest_reset":{"id":"executed"},"active_watch":{"level":"strong","reset_chance_percent":78},"stats":{"total":56}}}`
				if request.URL.String() == resetHistoryEndpoint {
					body = `{"events":[{"tweet_id":"executed"}]}`
				} else if request.URL.String() == resetHomepageEndpoint {
					body = `<div data-role="watch-poll" data-yes="1042" data-no="100"></div>`
				}
				if request.URL.String() == test.endpoint {
					status = http.StatusServiceUnavailable
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}}
			prediction, err := service.queryResetPrediction(context.Background())
			if err != nil {
				t.Fatalf("supplementary failure suppressed the status: %v", err)
			}
			if prediction.ActiveWatch == nil || prediction.ActiveWatch.ResetChancePercent == nil || *prediction.ActiveWatch.ResetChancePercent != 78 || prediction.LatestReset == nil || prediction.LatestReset.ID != "executed" || prediction.Stats.Total != 56 {
				t.Fatalf("authoritative status was lost: %#v", prediction)
			}
			if test.endpoint == resetHistoryEndpoint && prediction.CommunityPoll == nil {
				t.Fatal("history failure also discarded the available poll")
			}
			if test.endpoint == resetHomepageEndpoint && len(prediction.History) != 1 {
				t.Fatal("poll failure also discarded the available history")
			}
		})
	}
}

func TestGetPredictionKeepsScheduledCacheIsolated(t *testing.T) {
	service := &UsageService{cfg: Config{CacheTTL: time.Minute}, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"data":{"scheduled_reset":{"id":"scheduled","status":"scheduled","scheduled_for":"2026-10-02T17:00:00Z","source":{"url":"https://codex-resets.com/"}},"active_watch":{"reset_chance_percent":78,"source":{"url":"https://codex-resets.com/"}}}}`
		if request.URL.String() == resetHistoryEndpoint {
			body = `{"events":[]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	first, err := service.GetPrediction(context.Background(), false)
	if err != nil {
		t.Fatalf("get prediction: %v", err)
	}
	*first.ScheduledReset.ScheduledFor = "changed"
	first.ScheduledReset.Source.URL = "changed"
	*first.ActiveWatch.ResetChancePercent = 0
	first.ActiveWatch.Source.URL = "changed"
	second, err := service.GetPrediction(context.Background(), false)
	if err != nil {
		t.Fatalf("get cached prediction: %v", err)
	}
	if !second.FromCache || *second.ScheduledReset.ScheduledFor != "2026-10-02T17:00:00Z" || second.ScheduledReset.Source.URL != "https://codex-resets.com/" || *second.ActiveWatch.ResetChancePercent != 78 || second.ActiveWatch.Source.URL != "https://codex-resets.com/" {
		t.Fatalf("returned pointers allowed the cache to be mutated: %#v", second)
	}
}

func TestParseResetPoll(t *testing.T) {
	poll := parseResetPoll([]byte(`<section data-role="watch-poll" data-episode-id="manual" data-yes="1042" data-no="100"></section>`))
	if poll == nil {
		t.Fatal("parseResetPoll returned nil")
	}
	if poll.YesVotes != 1042 || poll.NoVotes != 100 || poll.TotalVotes != 1142 {
		t.Fatalf("poll counts = %#v", poll)
	}
	wantPercent := 1042.0 * 100 / 1142.0
	if poll.YesPercent != wantPercent {
		t.Fatalf("yes percent = %v, want %v", poll.YesPercent, wantPercent)
	}

	if got := parseResetPoll([]byte(`<div data-role="watch-poll" data-yes="0" data-no="0"></div>`)); got != nil {
		t.Fatalf("zero-vote poll = %#v, want nil", got)
	}
	if got := parseResetPoll([]byte(`<div data-role="other" data-yes="10" data-no="2"></div>`)); got != nil {
		t.Fatalf("non-poll data = %#v, want nil", got)
	}
}

func TestNormalizeResetHistory(t *testing.T) {
	history := normalizeResetHistory([]resetHistoryEvent{
		{TweetID: "2090", TweetURL: "https://x.com/2090", ResetType: "regular", Source: "webhook"},
		{TweetID: "2090", TweetURL: "https://x.com/2090", ResetType: "regular", Source: "webhook"},
		{TweetID: "2091", TweetURL: "https://x.com/2091", ResetType: "banked", Source: "observed"},
	})
	if len(history) != 2 {
		t.Fatalf("history length = %d, want 2 after deduplication", len(history))
	}
	if history[0].ID != "2090" || history[0].Source == nil || history[0].Source.Type != "webhook" {
		t.Fatalf("first history event = %#v", history[0])
	}
	if history[1].ResetType != "banked" || history[1].Source.URL != "https://x.com/2091" {
		t.Fatalf("second history event = %#v", history[1])
	}
}

func TestBasicAuth(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("xyz", "test-password")
	if !authorizedBasic(request, "xyz", "test-password") {
		t.Fatal("valid Basic Auth credentials were rejected")
	}
	if authorizedBasic(request, "xyz", "wrong-password") {
		t.Fatal("invalid Basic Auth password was accepted")
	}
}

func TestHealthCheckDoesNotRequireBasicAuth(t *testing.T) {
	cfg := Config{
		BasicAuthEnabled:  true,
		BasicAuthUsername: "xyz",
		BasicAuthPassword: "test-password",
	}
	server := NewServer(cfg, &UsageService{cfg: cfg})
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()

	server.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d without Basic Auth", recorder.Code, http.StatusOK)
	}
}

func TestApplyConfigUpdateBasicAuth(t *testing.T) {
	enabled := true
	username := "admin"
	password := "new-password"
	next, err := applyConfigUpdate(Config{
		AccessToken:      "access-token",
		ChatGPTAccountID: "account-id",
	}, ConfigUpdate{
		BasicAuthEnabled:  &enabled,
		BasicAuthUsername: &username,
		BasicAuthPassword: &password,
	})
	if err != nil {
		t.Fatalf("apply Basic Auth config: %v", err)
	}
	if !next.BasicAuthEnabled || next.BasicAuthUsername != username || next.BasicAuthPassword != password {
		t.Fatalf("Basic Auth config = %#v, want enabled user and password", next)
	}
}

func TestApplyConfigUpdateBasicAuthPreservesPassword(t *testing.T) {
	enabled := true
	username := "new-admin"
	next, err := applyConfigUpdate(Config{
		AccessToken:       "access-token",
		ChatGPTAccountID:  "account-id",
		BasicAuthEnabled:  true,
		BasicAuthUsername: "old-admin",
		BasicAuthPassword: "existing-password",
	}, ConfigUpdate{
		BasicAuthEnabled:  &enabled,
		BasicAuthUsername: &username,
	})
	if err != nil {
		t.Fatalf("apply Basic Auth config without password: %v", err)
	}
	if next.BasicAuthPassword != "existing-password" {
		t.Fatalf("password = %q, want existing password to be preserved", next.BasicAuthPassword)
	}
}

func TestBasicAuthSatisfiesAppAPIKey(t *testing.T) {
	cfg := Config{
		BasicAuthEnabled:  true,
		BasicAuthUsername: "xyz",
		BasicAuthPassword: "test-password",
		AppAPIKey:         "admin-key",
	}
	server := &Server{cfg: cfg}
	handler := server.withMiddleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	request.SetBasicAuth("xyz", "test-password")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("Basic Auth plus configured app key returned HTTP %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestMiddlewareUsesUpdatedBasicAuthConfig(t *testing.T) {
	service := &UsageService{cfg: Config{
		BasicAuthEnabled:  true,
		BasicAuthUsername: "old-admin",
		BasicAuthPassword: "old-password",
		AppAPIKey:         "admin-key",
	}}
	server := &Server{cfg: service.currentConfig(), usage: service}
	handler := server.withMiddleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	request.SetBasicAuth("old-admin", "old-password")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("old Basic Auth credentials returned HTTP %d, want %d before update", response.Code, http.StatusNoContent)
	}

	service.cfgMu.Lock()
	service.cfg.BasicAuthUsername = "new-admin"
	service.cfg.BasicAuthPassword = "new-password"
	service.cfgMu.Unlock()

	request = httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	request.SetBasicAuth("old-admin", "old-password")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("old Basic Auth credentials returned HTTP %d after update, want %d", response.Code, http.StatusUnauthorized)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	request.SetBasicAuth("new-admin", "new-password")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("new Basic Auth credentials returned HTTP %d after update, want %d", response.Code, http.StatusNoContent)
	}
}

func multiAccountFixture(t *testing.T, transport http.RoundTripper) *UsageService {
	t.Helper()
	cfg := Config{ConfigPath: filepath.Join(t.TempDir(), "config.json"), CacheTTL: time.Minute, Accounts: []AccountConfig{
		{ID: "one", Name: "工作账号", OpenAI: OpenAIConfig{AccessToken: "fixture-oauth-one", Cookie: "fixture-cookie-one", ChatGPTAccountID: "upstream-one"}},
		{ID: "two", Name: "个人账号", OpenAI: OpenAIConfig{AccessToken: "fixture-oauth-two", Cookie: "fixture-cookie-two", ChatGPTAccountID: "upstream-two"}},
	}, ActiveAccountID: "one"}
	if err := normalizeAccounts(&cfg); err != nil {
		t.Fatal(err)
	}
	store, err := OpenUsageHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if transport == nil {
		transport = roundTripFunc(func(request *http.Request) (*http.Response, error) { return multiAccountUpstreamResponse(request), nil })
	}
	return &UsageService{cfg: cfg, historyStore: store, client: &http.Client{Transport: transport}}
}

func multiAccountUpstreamResponse(request *http.Request) *http.Response {
	value := 10
	if request.Header.Get("ChatGPT-Account-Id") == "upstream-two" {
		value = 70
	}
	body := fmt.Sprintf(`{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":%d,"limit_window_seconds":18000},"secondary_window":{"used_percent":%d,"limit_window_seconds":604800}}}`, value, value)
	switch request.URL.Path {
	case "/backend-api/wham/usage/daily-token-usage-breakdown":
		body = fmt.Sprintf(`{"data":[{"date":"2026-10-01","product_surface_usage_values":{"codex":%d}}]}`, value)
	case "/backend-api/wham/analytics/daily-workspace-usage-counts":
		body = fmt.Sprintf(`{"data":[{"date":"2026-10-01","totals":{"turns":%d,"text_total_tokens":%d}}]}`, value, value*100)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestMultiAccountConfigurationMigrationAndValidation(t *testing.T) {
	t.Setenv("OPENAI_ACCESS_TOKEN", "")
	t.Setenv("CHATGPT_ACCOUNT_ID", "")
	t.Setenv("BASIC_AUTH_ENABLED", "false")
	tests := []struct {
		name, content, active string
		wantError             bool
	}{
		{"legacy", `{"openai":{"access_token":"fixture-oauth","chatgpt_account_id":"upstream-one"}}`, "default", false},
		{"multiple", `{"active_account_id":"two","accounts":[{"id":"one","name":"工作","openai":{"access_token":"fixture-one","chatgpt_account_id":"one"}},{"id":"two","name":"个人","openai":{"access_token":"fixture-two","chatgpt_account_id":"two"}}]}`, "two", false},
		{"duplicate", `{"accounts":[{"id":"one"},{"id":"one"}]}`, "", true},
		{"unknown active", `{"active_account_id":"missing","accounts":[{"id":"one"}]}`, "", true},
		{"invalid id", `{"accounts":[{"id":"../one"}]}`, "", true},
		{"reserved id", `{"accounts":[{"id":"usage"}]}`, "", true},
		{"invalid basic auth in setup", `{"basic_auth":{"enabled":true},"accounts":[{"id":"one"}]}`, "", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if test.name == "invalid basic auth in setup" {
				t.Setenv("BASIC_AUTH_ENABLED", "")
				t.Setenv("BASIC_AUTH_USER", "")
				t.Setenv("BASIC_AUTH_PASSWORD", "")
			}
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfigFile(path, false)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError %v", err, test.wantError)
			}
			if err == nil && cfg.ActiveAccountID != test.active {
				t.Fatalf("active = %q", cfg.ActiveAccountID)
			}
			if err == nil {
				for _, account := range cfg.Accounts {
					if account.OpenAI.UserAgent != defaultUserAgent {
						t.Fatal("missing per-account user agent default")
					}
				}
			}
		})
	}
}

func TestMultiAccountEnvironmentOverrideStaysOnDefault(t *testing.T) {
	t.Setenv("OPENAI_ACCESS_TOKEN", "fixture-env-token")
	cfg := Config{Accounts: []AccountConfig{
		{ID: "default", OpenAI: OpenAIConfig{AccessToken: "fixture-original", ChatGPTAccountID: "original"}},
		{ID: "two", OpenAI: OpenAIConfig{AccessToken: "fixture-second", ChatGPTAccountID: "second"}},
	}, ActiveAccountID: "two"}
	if err := normalizeAccounts(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := applyEnvironmentConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := normalizeAccounts(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AccessToken != "fixture-env-token" || cfg.Accounts[0].OpenAI.AccessToken != "fixture-env-token" || cfg.ActiveAccountID != "two" {
		t.Fatal("environment override corrupted the active profile")
	}
}

func TestMultiAccountUsageAnalyticsAndCacheIsolation(t *testing.T) {
	var mu sync.Mutex
	counts := make(map[string]int)
	service := multiAccountFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		id := strings.TrimPrefix(request.Header.Get("ChatGPT-Account-Id"), "upstream-")
		if request.Header.Get("Authorization") != "Bearer fixture-oauth-"+id || request.Header.Get("Cookie") != "fixture-cookie-"+id {
			t.Error("upstream credential context crossed accounts")
		}
		mu.Lock()
		counts[id+request.URL.Path]++
		mu.Unlock()
		return multiAccountUpstreamResponse(request), nil
	}))
	dateRange := analyticsDateRange{StartDate: "2026-10-01", EndDate: "2026-10-01"}
	for _, test := range []struct {
		id   string
		used float64
	}{{"one", 10}, {"two", 70}} {
		t.Run(test.id, func(t *testing.T) {
			for index := 0; index < 2; index++ {
				usage, err := service.GetForAccount(context.Background(), test.id, false)
				if err != nil {
					t.Fatal(err)
				}
				if usage.AccountID != test.id || usage.SevenDay.UsedPercent != test.used || usage.FromCache != (index == 1) {
					t.Fatalf("usage = %+v", usage)
				}
				analytics, err := service.GetAnalyticsForAccount(context.Background(), test.id, false, dateRange)
				if err != nil {
					t.Fatal(err)
				}
				if analytics.AccountID != test.id || analytics.Summary.Turns != int64(test.used) {
					t.Fatal("analytics crossed accounts")
				}
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	for _, count := range counts {
		if count != 1 {
			t.Fatalf("cache missed: counts = %v", counts)
		}
	}
}

func TestMultiAccountHistoryMigrationAndIdenticalTimestamps(t *testing.T) {
	store, err := OpenUsageHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	at := "2026-10-01T01:00:00Z"
	if err := store.Insert(ctx, HistoryPoint{At: at, UsedPercent: 10}); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimLegacyHistory(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimLegacyHistory(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	second, err := store.LoadAccount(ctx, "two")
	if err != nil || len(second) != 0 {
		t.Fatal("legacy history migrated to another account")
	}
	if err := store.InsertAccount(ctx, "two", HistoryPoint{At: at, UsedPercent: 70}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id   string
		used float64
	}{{"one", 10}, {"two", 70}} {
		t.Run(test.id, func(t *testing.T) {
			points, err := store.LoadAccount(ctx, test.id)
			if err != nil {
				t.Fatal(err)
			}
			if len(points) != 1 || points[0].UsedPercent != test.used {
				t.Fatalf("points=%+v", points)
			}
		})
	}
}

func TestMultiAccountCollectorSamplesInactiveAccountsAndIsolatesFallback(t *testing.T) {
	var mu sync.Mutex
	failed := false
	service := multiAccountFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		fail := failed && request.Header.Get("ChatGPT-Account-Id") == "upstream-one"
		mu.Unlock()
		if fail {
			return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
		return multiAccountUpstreamResponse(request), nil
	}))
	at := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	service.collectAllAccountsHistoryAt(context.Background(), at)
	mu.Lock()
	failed = true
	mu.Unlock()
	service.collectAllAccountsHistoryAt(context.Background(), at.Add(usageHistorySampleInterval))
	for _, test := range []struct {
		id    string
		used  float64
		stale bool
	}{{"one", 10, true}, {"two", 70, false}} {
		t.Run(test.id, func(t *testing.T) {
			cfg := service.currentConfig()
			points, err := service.historyStore.LoadAccount(context.Background(), accountHistoryKey(cfg.Accounts[accountIndex(cfg, test.id)]))
			if err != nil {
				t.Fatal(err)
			}
			if len(points) != 2 || points[1].UsedPercent != test.used || points[1].Stale != test.stale {
				t.Fatalf("points=%+v", points)
			}
		})
	}
	view := service.GetAccountsUsage(context.Background(), true)
	if view.Accounts[0].Error == "" || !view.Accounts[0].Stale || view.Accounts[0].Usage.SevenDay.UsedPercent != 10 || view.Accounts[1].Error != "" {
		t.Fatalf("partial failure=%+v", view)
	}
}

func TestMultiAccountSwitchDoesNotMixInflightResponse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	service := multiAccountFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("ChatGPT-Account-Id") == "upstream-one" {
			once.Do(func() { close(started); <-release })
		}
		return multiAccountUpstreamResponse(request), nil
	}))
	result := make(chan *UsageResponse, 1)
	failures := make(chan error, 1)
	go func() { usage, err := service.Get(context.Background(), false); result <- usage; failures <- err }()
	<-started
	if _, err := service.SwitchAccount("two"); err != nil {
		close(release)
		t.Fatal(err)
	}
	second, err := service.GetForAccount(context.Background(), "two", false)
	close(release)
	first := <-result
	firstErr := <-failures
	if err != nil || firstErr != nil {
		t.Fatalf("errors=%v,%v", err, firstErr)
	}
	if first.AccountID != "one" || second.AccountID != "two" || second.SevenDay.UsedPercent != 70 {
		t.Fatal("in-flight response crossed accounts")
	}
	cached, err := service.GetForAccount(context.Background(), "two", false)
	if err != nil || cached.AccountID != "two" || cached.SevenDay.UsedPercent != 70 || !cached.FromCache {
		t.Fatal("active cache was overwritten")
	}
}

func TestMultiAccountManagementPreservesOtherCredentialsAndPersistsSwitch(t *testing.T) {
	t.Setenv("OPENAI_ACCESS_TOKEN", "")
	t.Setenv("CHATGPT_ACCOUNT_ID", "")
	t.Setenv("BASIC_AUTH_ENABLED", "false")
	service := multiAccountFixture(t, nil)
	name, token, upstreamID := "备用账号", "fixture-oauth-three", "upstream-three"
	created, err := service.SaveAccount("", AccountUpdate{Name: &name, ConfigUpdate: ConfigUpdate{AccessToken: &token, ChatGPTAccountID: &upstreamID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Accounts) != 3 || created.ActiveAccountID != "one" {
		t.Fatalf("created=%+v", created)
	}
	id := created.Accounts[2].ID
	if _, err := service.SwitchAccount(id); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfigFile(service.currentConfig().ConfigPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveAccountID != id || loaded.AccessToken != "fixture-oauth-one" || loaded.Accounts[accountIndex(loaded, id)].OpenAI.AccessToken != token {
		t.Fatal("switch did not persist all profiles")
	}
	name = "已改名"
	if _, err := service.SaveAccount("two", AccountUpdate{Name: &name}); err != nil {
		t.Fatal(err)
	}
	view, err := service.DeleteAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	if view.ActiveAccountID != "one" || view.Accounts[1].Name != name {
		t.Fatalf("deleted=%+v", view)
	}
	data, _ := json.Marshal(service.ConfigView())
	for _, secret := range []string{"fixture-oauth-one", "fixture-oauth-two", "fixture-cookie-one", "fixture-cookie-two"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("credentials leaked through masked config view")
		}
	}
}

func TestMultiAccountDraftDoesNotInheritActiveCredentials(t *testing.T) {
	service := multiAccountFixture(t, nil)
	empty, token := "", "fixture-new-token"
	for _, test := range []ConfigUpdate{{AccountID: &empty}, {AccountID: &empty, AccessToken: &token}} {
		if _, err := service.TestConfig(context.Background(), test); err == nil {
			t.Fatal("incomplete draft inherited active credentials")
		}
	}
}

func TestMultiAccountRoutesAuthenticationAndErrors(t *testing.T) {
	for _, prefix := range []string{"", "/codex"} {
		t.Run(prefix, func(t *testing.T) {
			tests := []struct {
				method, path, body string
				status             int
			}{
				{"GET", "/api/accounts", "", 200},
				{"GET", "/api/accounts/usage", "", 200},
				{"GET", "/api/accounts/usage?force=invalid", "", 400},
				{"GET", "/api/usage?account_id=missing", "", 404},
				{"GET", "/api/usage/analytics?account_id=missing", "", 404},
				{"POST", "/api/config/test", `{"account_id":"missing"}`, 404},
				{"PUT", "/api/config", `{"account_id":"missing"}`, 404},
				{"POST", "/api/accounts", `{"name":"备用","access_token":"fixture-three","chatgpt_account_id":"third"}`, 201},
				{"PUT", "/api/accounts/two", `{"name":"已改名"}`, 200},
				{"PUT", "/api/accounts/active", `{"account_id":"two"}`, 200},
				{"PUT", "/api/accounts/active", `{"account_id":"missing"}`, 404},
				{"DELETE", "/api/accounts/two", "", 200},
				{"DELETE", "/api/accounts/missing", "", 404},
			}
			for _, test := range tests {
				t.Run(test.method+test.path, func(t *testing.T) {
					service := multiAccountFixture(t, nil)
					service.cfg.AppAPIKey = "fixture-management-key"
					service.cfg.BasePath = prefix
					server := NewServer(service.cfg, service)
					for _, authenticate := range []bool{false, true} {
						request := httptest.NewRequest(test.method, prefix+test.path, strings.NewReader(test.body))
						if authenticate {
							request.Header.Set("X-App-API-Key", "fixture-management-key")
						}
						response := httptest.NewRecorder()
						server.handler.ServeHTTP(response, request)
						want := 401
						if authenticate {
							want = test.status
						}
						if response.Code != want {
							t.Fatalf("authenticated=%v status=%d want=%d body=%s", authenticate, response.Code, want, response.Body.String())
						}
						if !strings.Contains(response.Header().Get("Cache-Control"), "no-store") {
							t.Fatal("account API can be cached")
						}
					}
				})
			}
		})
	}
}

func TestMultiAccountCredentialUpdateInvalidatesOnlyTargetCache(t *testing.T) {
	service := multiAccountFixture(t, nil)
	for _, id := range []string{"one", "two"} {
		if _, err := service.GetForAccount(context.Background(), id, false); err != nil {
			t.Fatal(err)
		}
	}
	id, token := "two", "fixture-renewed-token"
	if _, err := service.UpdateConfig(ConfigUpdate{AccountID: &id, AccessToken: &token}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id        string
		fromCache bool
	}{{"one", true}, {"two", false}} {
		t.Run(test.id, func(t *testing.T) {
			usage, err := service.GetForAccount(context.Background(), test.id, false)
			if err != nil {
				t.Fatal(err)
			}
			if usage.FromCache != test.fromCache {
				t.Fatal("wrong account cache invalidated")
			}
		})
	}
	if service.currentConfig().ActiveAccountID != "one" {
		t.Fatal("targeted credential edit changed active account")
	}
}

func TestMultiAccountRejectsIdentityChangeAndLastDeletion(t *testing.T) {
	service := multiAccountFixture(t, nil)
	upstreamID := "different-upstream"
	if _, err := service.SaveAccount("two", AccountUpdate{ConfigUpdate: ConfigUpdate{ChatGPTAccountID: &upstreamID}}); err == nil {
		t.Fatal("identity replacement allowed mixed history")
	}
	if _, err := service.UpdateConfig(ConfigUpdate{ChatGPTAccountID: &upstreamID}); err == nil {
		t.Fatal("legacy config update allowed mixed history")
	}
	if _, err := service.DeleteAccount("two"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DeleteAccount("one"); err != errLastAccount {
		t.Fatalf("last deletion error=%v", err)
	}
}

func TestMultiAccountHistoryRemainsScopedAfterOfflineIdentityChange(t *testing.T) {
	service := multiAccountFixture(t, nil)
	service.collectAllAccountsHistoryAt(context.Background(), time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC))
	cfg := service.currentConfig()
	cfg.Accounts[0].OpenAI.ChatGPTAccountID = "different-upstream"
	if err := normalizeAccounts(&cfg); err != nil {
		t.Fatal(err)
	}
	// Simulate a restart with an offline-edited config and the same database.
	restarted := &UsageService{cfg: cfg, client: service.currentClient(), historyStore: service.historyStore}
	usage, err := restarted.GetForAccount(context.Background(), "one", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.History) != 0 {
		t.Fatal("new upstream identity inherited the old timeline")
	}
	points, err := service.historyStore.LoadAccount(context.Background(), "one:upstream-one")
	if err != nil || len(points) != 1 {
		t.Fatal("old timeline was lost")
	}
}

func TestMultiAccountValidationLimits(t *testing.T) {
	for _, test := range []struct {
		name      string
		count     int
		label     string
		wantError bool
	}{
		{"maximum", 32, "账号", false}, {"too many", 33, "账号", true}, {"maximum name", 1, strings.Repeat("账", 80), false}, {"name too long", 1, strings.Repeat("账", 81), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{}
			for index := 0; index < test.count; index++ {
				cfg.Accounts = append(cfg.Accounts, AccountConfig{ID: fmt.Sprintf("profile-%d", index), Name: test.label})
			}
			err := normalizeAccounts(&cfg)
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%v", err, test.wantError)
			}
		})
	}
}

func TestMultiAccountCanRenameIncompleteSetupProfile(t *testing.T) {
	cfg := Config{ConfigPath: filepath.Join(t.TempDir(), "config.json"), CacheTTL: defaultCacheTTL}
	if err := normalizeAccounts(&cfg); err != nil {
		t.Fatal(err)
	}
	service := &UsageService{cfg: cfg}
	name := "待配置的工作账号"
	view, err := service.SaveAccount("default", AccountUpdate{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if view.ActiveAccountID != "default" || view.Accounts[0].Name != name || !view.Accounts[0].SetupRequired {
		t.Fatal("renaming setup account changed its credentials")
	}
}

func stringPointer(value string) *string { return &value }

func TestAccountProxyMigrationAndValidation(t *testing.T) {
	t.Setenv("UPSTREAM_PROXY", "")
	for _, test := range []struct {
		name, content string
		want          []string
		invalid       bool
	}{
		{"legacy single", `{"openai":{},"proxy":{"url":"http://proxy.example:8080"}}`, []string{"http://proxy.example:8080"}, false},
		{"legacy multiple with explicit direct", `{"active_account_id":"two","proxy":{"url":"http://proxy.example:8080"},"accounts":[{"id":"one"},{"id":"two","proxy":{"url":""}},{"id":"three","proxy":{"url":"socks5://other.example:1080"}}]}`, []string{"http://proxy.example:8080", "", "socks5://other.example:1080"}, false},
		{"invalid inactive proxy", `{"accounts":[{"id":"one","proxy":{"url":""}},{"id":"two","proxy":{"url":"ftp://invalid.example:21"}}]}`, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfigFile(path, false)
			if (err != nil) != test.invalid {
				t.Fatalf("load error = %v", err)
			}
			if err != nil {
				return
			}
			for i, want := range test.want {
				if cfg.Accounts[i].Proxy == nil || cfg.Accounts[i].Proxy.URL != want {
					t.Fatalf("proxy %d = %#v", i, cfg.Accounts[i].Proxy)
				}
			}
			raw, err := marshalConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var stored map[string]json.RawMessage
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			if _, exists := stored["proxy"]; exists {
				t.Fatal("new configuration still saves a shared proxy")
			}
			if err := persistConfig(cfg); err != nil {
				t.Fatal(err)
			}
			reloaded, err := loadConfigFile(path, false)
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range test.want {
				if reloaded.Accounts[i].Proxy.URL != want {
					t.Fatal("migration did not survive reload")
				}
			}
		})
	}
}

func TestProxyAuthenticationUpdates(t *testing.T) {
	old := "http://alice:fixture-secret@proxy.example:8080"
	for _, test := range []struct {
		name, current      string
		update             ConfigUpdate
		username, password string
		direct, invalid    bool
	}{
		{name: "omitted preserves", current: old, update: ConfigUpdate{ProxyURL: stringPointer("http://other.example:8081")}, username: "alice", password: "fixture-secret"},
		{name: "masked round trip preserves", current: old, update: ConfigUpdate{ProxyURL: stringPointer(maskedProxyURL(old))}, username: "alice", password: "fixture-secret"},
		{name: "special characters", current: old, update: ConfigUpdate{ProxyUsername: stringPointer("test@name"), ProxyPassword: stringPointer(" fixture@:#%/密码 ")}, username: "test@name", password: " fixture@:#%/密码 "},
		{name: "empty password explicit", current: old, update: ConfigUpdate{ProxyPassword: stringPointer("")}, username: "alice", password: ""},
		{name: "clear authentication", current: old, update: ConfigUpdate{ProxyClearAuth: true}},
		{name: "direct", current: old, update: ConfigUpdate{ProxyURL: stringPointer("")}, direct: true},
		{name: "embedded URL", current: old, update: ConfigUpdate{ProxyURL: stringPointer("socks5://bob:new-fixture@other.example:1080")}, username: "bob", password: "new-fixture"},
		{name: "new user requires replacement", current: old, update: ConfigUpdate{ProxyUsername: stringPointer("bob")}, invalid: true},
		{name: "new draft cannot borrow masked password", update: ConfigUpdate{ProxyURL: stringPointer(maskedProxyURL(old))}, invalid: true},
		{name: "no address", update: ConfigUpdate{ProxyPassword: stringPointer("fixture")}, invalid: true},
		{name: "conflicting clear", current: old, update: ConfigUpdate{ProxyClearAuth: true, ProxyPassword: stringPointer("fixture")}, invalid: true},
		{name: "invalid port", update: ConfigUpdate{ProxyURL: stringPointer("http://alice:fixture@proxy.example:99999")}, invalid: true},
		{name: "invalid path", update: ConfigUpdate{ProxyURL: stringPointer("http://alice:fixture@proxy.example:8080/private")}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := applyProxyUpdate(test.current, test.update)
			if (err != nil) != test.invalid {
				t.Fatalf("error = %v", err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "fixture") {
					t.Fatal("validation leaked proxy credentials")
				}
				return
			}
			parsed, err := parseProxyURL(raw)
			if err != nil {
				t.Fatal(err)
			}
			if test.direct {
				if parsed != nil {
					t.Fatal("direct retained proxy")
				}
				return
			}
			if test.username == "" {
				if parsed.User != nil {
					t.Fatal("clear retained authentication")
				}
				return
			}
			password, _ := parsed.User.Password()
			if parsed.User.Username() != test.username || password != test.password {
				t.Fatal("proxy authentication changed unexpectedly")
			}
			if test.password != "" && strings.Contains(maskedProxyURL(raw), url.QueryEscape(test.password)) {
				t.Fatal("masked URL leaked proxy password")
			}
		})
	}
}

func TestPerAccountProxyUpdatesAndCacheIsolation(t *testing.T) {
	service := multiAccountFixture(t, nil)
	for _, id := range []string{"one", "two"} {
		if _, err := service.GetForAccount(context.Background(), id, false); err != nil {
			t.Fatal(err)
		}
	}
	oneBefore, twoBefore := service.accounts["one"], service.accounts["two"]
	raw := "http://alice:fixture-one@127.0.0.1:18081"
	view, err := service.UpdateConfig(ConfigUpdate{AccountID: stringPointer("one"), ProxyURL: &raw})
	if err != nil {
		t.Fatal(err)
	}
	if !view.ProxyPasswordConfigured || strings.Contains(view.ProxyURL, "fixture-one") {
		t.Fatal("proxy view is not safely masked")
	}
	cfg := service.currentConfig()
	if cfg.Accounts[1].Proxy.URL != "" {
		t.Fatal("proxy update leaked to second account")
	}
	two, _, err := service.accountService(cfg, "two")
	if err != nil || two != twoBefore || two.cached == nil {
		t.Fatal("unrelated account cache invalidated")
	}
	one, _, err := service.accountService(cfg, "one")
	if err != nil || one == oneBefore || one.cached != nil {
		t.Fatal("changed account kept old runtime/cache")
	}
	transport := one.currentClient().Transport.(*http.Transport)
	chosen, err := transport.Proxy(httptest.NewRequest(http.MethodGet, "https://chatgpt.com/", nil))
	if err != nil || chosen.String() != raw {
		t.Fatal("first account did not use its proxy")
	}
	twoProxy := "socks5://bob:fixture-two@127.0.0.1:18082"
	if _, err := service.SaveAccount("two", AccountUpdate{ConfigUpdate: ConfigUpdate{ProxyURL: &twoProxy}}); err != nil {
		t.Fatal(err)
	}
	if service.currentConfig().UpstreamProxy != raw {
		t.Fatal("editing inactive account changed active proxy")
	}
	if _, err := service.SwitchAccount("two"); err != nil {
		t.Fatal(err)
	}
	if service.currentConfig().UpstreamProxy != raw || configForAccount(service.currentConfig(), service.currentConfig().Accounts[1]).UpstreamProxy != twoProxy {
		t.Fatal("display switch changed the primary proxy or lost the selected proxy")
	}
	oneAfter, _, err := service.accountService(service.currentConfig(), "one")
	if err != nil || oneAfter != one {
		t.Fatal("switch discarded unchanged proxy runtime")
	}
	if _, err := service.SaveAccount("", AccountUpdate{Name: stringPointer("新账号"), ConfigUpdate: ConfigUpdate{AccessToken: stringPointer("fixture-new"), ChatGPTAccountID: stringPointer("upstream-new")}}); err != nil {
		t.Fatal(err)
	}
	cfg = service.currentConfig()
	if cfg.Accounts[len(cfg.Accounts)-1].Proxy.URL != "" {
		t.Fatal("new account inherited another account proxy")
	}
	persisted, err := loadConfigFile(cfg.ConfigPath, false)
	if err != nil || persisted.Accounts[0].Proxy.URL != raw || persisted.Accounts[1].Proxy.URL != twoProxy {
		t.Fatal("account proxies did not persist independently")
	}
}

func TestProxyDraftScopeAndRoutes(t *testing.T) {
	service := multiAccountFixture(t, nil)
	service.cfg.Accounts[0].Proxy = &ProxyConfig{URL: "http://alice:fixture-one@proxy.example:8080"}
	service.cfg.Accounts[1].Proxy = &ProxyConfig{URL: "socks5://bob:fixture-two@other.example:1080"}
	if err := normalizeAccounts(&service.cfg); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, id, address, username, password string
		invalid                               bool
	}{
		{"inactive saved authentication", "two", "socks5://other.example:1080", "bob", "fixture-two", false},
		{"active saved authentication", "one", "http://proxy.example:8080", "alice", "fixture-one", false},
		{"new draft", "", "http://new.example:8080", "", "", false},
		{"unknown", "missing", "", "", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := service.proxyTestConfig(ProxyTestRequest{AccountID: &test.id, ProxyURL: &test.address})
			if (err != nil) != test.invalid {
				t.Fatalf("error = %v", err)
			}
			if err != nil {
				return
			}
			parsed, _ := parseProxyURL(cfg.UpstreamProxy)
			if test.username == "" {
				if parsed.User != nil {
					t.Fatal("new draft inherited authentication")
				}
				return
			}
			password, _ := parsed.User.Password()
			if parsed.User.Username() != test.username || password != test.password {
				t.Fatal("test uses another account's authentication")
			}
		})
	}
	raw, _ := json.Marshal(service.AccountsView())
	if strings.Contains(string(raw), "fixture-one") || strings.Contains(string(raw), "fixture-two") {
		t.Fatal("account list leaks proxy password")
	}
	service.cfg.AppAPIKey = "fixture-admin-key"
	service.cfg.BasePath = "/codex"
	server := NewServer(service.cfg, service)
	for _, test := range []struct {
		name, body, key string
		status          int
	}{
		{"protected", `{"account_id":"one"}`, "", 401},
		{"unknown", `{"account_id":"missing"}`, "fixture-admin-key", 404},
		{"invalid", `{"account_id":"two","proxy_url":"ftp://invalid.example:21"}`, "fixture-admin-key", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/codex/api/config/test-proxy", strings.NewReader(test.body))
			req.Header.Set("X-App-API-Key", test.key)
			response := httptest.NewRecorder()
			server.handler.ServeHTTP(response, req)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
		})
	}
}

func TestProxyEnvironmentOverrideOnlyDefaultAccount(t *testing.T) {
	t.Setenv("UPSTREAM_PROXY", "http://env.example:8080")
	cfg := Config{ActiveAccountID: "two", Accounts: []AccountConfig{
		{ID: "default", Proxy: &ProxyConfig{URL: "http://first.example:8080"}},
		{ID: "two", Proxy: &ProxyConfig{URL: "socks5://second.example:1080"}},
	}}
	if err := normalizeAccounts(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := applyEnvironmentConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := normalizeAccounts(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Accounts[0].Proxy.URL != "http://env.example:8080" || cfg.UpstreamProxy != "http://env.example:8080" || cfg.Accounts[1].Proxy.URL != "socks5://second.example:1080" {
		t.Fatal("environment proxy changed the active account instead of default")
	}
}

func TestDirectProxyIgnoresProcessProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://process.example:8080")
	client, err := newUpstreamClient("")
	if err != nil {
		t.Fatal(err)
	}
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("direct account uses process-wide proxy")
	}
}

func TestProxyConnectivityAcceptsUpstreamBusinessStatus(t *testing.T) {
	for _, status := range []int{200, 403, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			result := proxyTestResult("http://proxy.example:8080", status)
			if !result.OK || result.StatusCode != status {
				t.Fatal("reachable upstream was misreported as proxy failure")
			}
		})
	}
}

func TestProxyClientsSendAuthentication(t *testing.T) {
	username, password := "fixture@user", "fixture p@ss:#%"
	for _, scheme := range []string{"http", "https", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			captured := make(chan string, 1)
			var address string
			var certificates *x509.CertPool
			if scheme == "socks5" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				address = "socks5://" + listener.Addr().String()
				go func() {
					conn, err := listener.Accept()
					if err != nil {
						captured <- "accept failed"
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					reader := bufio.NewReader(conn)
					greeting := make([]byte, 2)
					if _, err = io.ReadFull(reader, greeting); err != nil {
						captured <- "greeting failed"
						return
					}
					methods := make([]byte, int(greeting[1]))
					_, _ = io.ReadFull(reader, methods)
					_, _ = conn.Write([]byte{5, 2})
					auth := make([]byte, 2)
					_, _ = io.ReadFull(reader, auth)
					user := make([]byte, int(auth[1]))
					_, _ = io.ReadFull(reader, user)
					length, _ := reader.ReadByte()
					pass := make([]byte, int(length))
					_, _ = io.ReadFull(reader, pass)
					if string(user) != username || string(pass) != password {
						captured <- "incorrect SOCKS5 credentials"
						return
					}
					_, _ = conn.Write([]byte{1, 0})
					command := make([]byte, 4)
					_, _ = io.ReadFull(reader, command)
					if command[3] != 3 {
						captured <- "expected proxy-side hostname resolution"
						return
					}
					hostLength, _ := reader.ReadByte()
					target := make([]byte, int(hostLength)+2)
					_, _ = io.ReadFull(reader, target)
					_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
					request, err := http.ReadRequest(reader)
					if err != nil {
						captured <- "read request failed"
						return
					}
					if request.Header.Get("Proxy-Authorization") != "" {
						captured <- "proxy credentials forwarded to upstream"
						return
					}
					_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK")
					captured <- "ok"
				}()
			} else {
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					expected := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
					if r.Header.Get("Proxy-Authorization") != expected {
						captured <- "incorrect HTTP proxy credentials"
						w.WriteHeader(407)
						return
					}
					captured <- "ok"
					_, _ = io.WriteString(w, "OK")
				})
				var server *httptest.Server
				if scheme == "https" {
					server = httptest.NewTLSServer(handler)
					certificates = x509.NewCertPool()
					certificates.AddCert(server.Certificate())
				} else {
					server = httptest.NewServer(handler)
				}
				t.Cleanup(server.Close)
				address = server.URL
			}
			parsed, _ := url.Parse(address)
			parsed.User = url.UserPassword(username, password)
			client, err := newUpstreamClient(parsed.String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.CloseIdleConnections)
			if certificates != nil {
				client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: certificates}
			}
			response, err := client.Get("http://upstream.invalid/usage")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != 200 || string(body) != "OK" {
				t.Fatal("authenticated proxy request failed")
			}
			select {
			case result := <-captured:
				if result != "ok" {
					t.Fatal(result)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("proxy did not receive authenticated request")
			}
		})
	}
}

func TestInactiveAccountRequestsUseItsAuthenticatedProxy(t *testing.T) {
	for _, test := range []struct{ name, method, path, body string }{
		{"credential test", http.MethodPost, "/api/config/test", `{"account_id":"two"}`},
		{"proxy test", http.MethodPost, "/api/config/test-proxy", `{"account_id":"two"}`},
		{"usage", http.MethodGet, "/api/usage?account_id=two", ""},
		{"analytics", http.MethodGet, "/api/usage/analytics?account_id=two", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []int
			makeProxy := func(number int) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					expected := "Basic " + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("fixture-user-%d:fixture-password-%d", number, number)))
					mu.Lock()
					if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" || r.Header.Get("Proxy-Authorization") != expected {
						seen = append(seen, -1)
					} else {
						seen = append(seen, number)
					}
					mu.Unlock()
					// Stop at the local tunnel endpoint: no external connection.
					w.WriteHeader(http.StatusProxyAuthRequired)
				}))
			}
			first, second := makeProxy(1), makeProxy(2)
			defer first.Close()
			defer second.Close()
			service := multiAccountFixture(t, nil)
			for i, server := range []*httptest.Server{first, second} {
				parsed, _ := url.Parse(server.URL)
				parsed.User = url.UserPassword(fmt.Sprintf("fixture-user-%d", i+1), fmt.Sprintf("fixture-password-%d", i+1))
				service.cfg.Accounts[i].Proxy = &ProxyConfig{URL: parsed.String()}
			}
			if err := normalizeAccounts(&service.cfg); err != nil {
				t.Fatal(err)
			}
			handler := NewServer(service.cfg, service).handler
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, expected local proxy rejection", response.Code)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) == 0 {
				t.Fatal("account request did not reach its proxy")
			}
			for _, number := range seen {
				if number != 2 {
					t.Fatal("inactive account used the active account proxy or wrong authentication")
				}
			}
			if strings.Contains(response.Body.String(), "fixture-password") {
				t.Fatal("connection failure leaked proxy password")
			}
		})
	}
}

func TestPrimaryAccountSelectionIsIndependentOfDisplayedAccount(t *testing.T) {
	for _, test := range []struct {
		name            string
		ids             []string
		active, primary string
	}{
		{"legacy default wins over ordering", []string{"two", "default"}, "two", "default"},
		{"custom file uses first", []string{"one", "two"}, "two", "one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{ActiveAccountID: test.active}
			for _, id := range test.ids {
				cfg.Accounts = append(cfg.Accounts, AccountConfig{ID: id, OpenAI: OpenAIConfig{AccessToken: "fixture-" + id, ChatGPTAccountID: "upstream-" + id}, Proxy: &ProxyConfig{URL: "http://" + id + ".example:8080"}})
			}
			if err := normalizeAccounts(&cfg); err != nil {
				t.Fatal(err)
			}
			if primaryAccountID(cfg) != test.primary || cfg.AccessToken != "fixture-"+test.primary || cfg.UpstreamProxy != "http://"+test.primary+".example:8080" || cfg.ActiveAccountID != test.active {
				t.Fatal("displayed profile replaced primary runtime")
			}
		})
	}
}

func TestLegacyApplicationAPIsRemainOnPrimaryAfterDisplaySwitch(t *testing.T) {
	for _, prefix := range []string{"", "/codex"} {
		for _, auth := range []string{"app key", "bearer", "basic"} {
			t.Run(prefix+"/"+auth, func(t *testing.T) {
				var mu sync.Mutex
				counts := map[string]int{}
				service := multiAccountFixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
					mu.Lock()
					counts[r.Header.Get("ChatGPT-Account-Id")+r.URL.Path]++
					mu.Unlock()
					return multiAccountUpstreamResponse(r), nil
				}))
				service.cfg.Accounts[0].ID = defaultAccountID
				service.cfg.ActiveAccountID = defaultAccountID
				service.cfg.BasePath = prefix
				service.cfg.AppAPIKey = "fixture-legacy-app-key"
				service.cfg.BasicAuthEnabled = auth == "basic"
				service.cfg.BasicAuthUsername = "fixture-admin"
				service.cfg.BasicAuthPassword = "fixture-admin-password"
				if err := normalizeAccounts(&service.cfg); err != nil {
					t.Fatal(err)
				}
				handler := NewServer(service.cfg, service).handler
				requestJSON := func(method, path, body string) map[string]json.RawMessage {
					t.Helper()
					request := httptest.NewRequest(method, prefix+path, strings.NewReader(body))
					switch auth {
					case "app key":
						request.Header.Set("X-App-API-Key", "fixture-legacy-app-key")
					case "bearer":
						request.Header.Set("Authorization", "Bearer fixture-legacy-app-key")
					case "basic":
						request.SetBasicAuth("fixture-admin", "fixture-admin-password")
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					if response.Code != 200 {
						t.Fatalf("%s returned HTTP %d", path, response.Code)
					}
					var payload map[string]json.RawMessage
					if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
						t.Fatal(err)
					}
					return payload
				}
				legacyKeys := map[string]bool{}
				for _, key := range []string{"source", "plan_type", "email", "rate_limit_allowed", "rate_limit_reached", "rate_limit_reached_type", "credits", "spend_control", "rate_limit_reset_credits", "fetched_at", "from_cache", "five_hour", "seven_day", "history", "weekly_history", "five_hour_history"} {
					legacyKeys[key] = true
				}
				checkUsage := func(path string, want float64, metadata bool) {
					t.Helper()
					payload := requestJSON(http.MethodGet, path, "")
					var window Window
					if err := json.Unmarshal(payload["seven_day"], &window); err != nil {
						t.Fatal(err)
					}
					if window.UsedPercent != want {
						t.Fatal("old application request followed displayed account")
					}
					if !metadata {
						for key := range payload {
							if !legacyKeys[key] {
								t.Fatalf("legacy usage schema gained field %s", key)
							}
						}
						for _, key := range []string{"source", "fetched_at", "from_cache", "five_hour", "seven_day", "rate_limit_allowed", "rate_limit_reached"} {
							if _, exists := payload[key]; !exists {
								t.Fatalf("legacy field %s disappeared", key)
							}
						}
					} else {
						if _, exists := payload["account_id"]; !exists {
							t.Fatal("explicit profile response lacks account metadata")
						}
					}
				}
				checkUsage("/api/usage", 10, false)
				rootClient := service.currentClient()
				prediction := &ResetPrediction{}
				service.resetCached = prediction
				service.resetCachedAt = time.Now()
				switched := requestJSON(http.MethodPut, "/api/accounts/active", `{"account_id":"two"}`)
				if string(switched["primary_account_id"]) != `"default"` || string(switched["active_account_id"]) != `"two"` {
					t.Fatal("primary or display identity changed unexpectedly")
				}
				if service.currentClient() != rootClient || service.resetCached != prediction {
					t.Fatal("display switch invalidated primary transport or public prediction cache")
				}
				checkUsage("/api/usage", 10, false)
				checkUsage("/api/usage?account_id=", 10, false)
				checkUsage("/api/usage?force=true", 10, false)
				checkUsage("/api/usage?account_id=two", 70, true)
				analytics := requestJSON(http.MethodGet, "/api/usage/analytics?start_date=2026-10-01&end_date=2026-10-01", "")
				if _, exists := analytics["account_id"]; exists {
					t.Fatal("legacy analytics gained profile metadata")
				}
				if _, exists := analytics["account_name"]; exists {
					t.Fatal("legacy analytics gained profile metadata")
				}
				var summary UsageAnalyticsSummary
				if err := json.Unmarshal(analytics["summary"], &summary); err != nil {
					t.Fatal(err)
				}
				if summary.Turns != 10 {
					t.Fatal("legacy analytics followed displayed account")
				}
				explicit := requestJSON(http.MethodGet, "/api/usage/analytics?account_id=two&start_date=2026-10-01&end_date=2026-10-01", "")
				if string(explicit["account_id"]) != `"two"` {
					t.Fatal("explicit analytics lost account selection")
				}
				config := requestJSON(http.MethodGet, "/api/config", "")
				if string(config["chatgpt_account_id"]) != `"upstream-one"` {
					t.Fatal("legacy config view followed displayed account")
				}
				mu.Lock()
				defer mu.Unlock()
				if counts["upstream-one/backend-api/wham/usage"] != 2 || counts["upstream-two/backend-api/wham/usage"] != 1 {
					t.Fatal("legacy cache/force behavior changed or crossed profiles")
				}
			})
		}
	}
}

func TestLegacyDefaultConfigurationUpdatesOnlyPrimary(t *testing.T) {
	service := multiAccountFixture(t, nil)
	if _, err := service.SwitchAccount("two"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateConfig(ConfigUpdate{UserAgent: stringPointer("fixture-primary-agent"), ProxyURL: stringPointer("http://primary.example:8080")}); err != nil {
		t.Fatal(err)
	}
	cfg := service.currentConfig()
	if cfg.ActiveAccountID != "two" || cfg.Accounts[0].OpenAI.UserAgent != "fixture-primary-agent" || cfg.Accounts[1].OpenAI.UserAgent == "fixture-primary-agent" || cfg.Accounts[1].Proxy.URL != "" {
		t.Fatal("old config update changed displayed profile instead of primary")
	}
	draft, err := service.proxyTestConfig(ProxyTestRequest{})
	if err != nil || draft.UpstreamProxy != "http://primary.example:8080" || draft.ChatGPTAccountID != "upstream-one" {
		t.Fatal("old proxy test does not use primary configuration")
	}
	if _, err := service.DeleteAccount("one"); err != errPrimaryAccount {
		t.Fatal("primary account can be silently removed")
	}
}

func TestLegacyApplicationErrorStatusAndDisplaySetupRemainCompatible(t *testing.T) {
	service := multiAccountFixture(t, nil)
	service.cfg.Accounts[0].ID = defaultAccountID
	service.cfg.Accounts[0].OpenAI = OpenAIConfig{}
	service.cfg.ActiveAccountID = "two"
	if err := normalizeAccounts(&service.cfg); err != nil {
		t.Fatal(err)
	}
	server := NewServer(service.cfg, service)
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/api/usage", "", 502},
		{http.MethodGet, "/api/usage/analytics", "", 502},
		{http.MethodGet, "/api/usage?force=invalid", "", 400},
		{http.MethodGet, "/api/usage/analytics?start_date=invalid", "", 400},
		{http.MethodGet, "/api/usage?account_id=missing", "", 404},
		{http.MethodGet, "/api/usage?account_id=default", "", 409},
		{http.MethodDelete, "/api/accounts/default", "", 409},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			server.handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want %d", response.Code, test.status)
			}
		})
	}
	response := httptest.NewRecorder()
	server.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `id="accountSelect"`) {
		t.Fatal("ready displayed account was replaced by primary setup wizard")
	}
}
