package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
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
	for index := 0; index < maxUsageHistoryPoints+12; index++ {
		if err := appendUsageHistory(path, HistoryPoint{
			At:          time.Date(2026, time.August, 27, 12, index, 0, 0, time.UTC).Format(time.RFC3339),
			UsedPercent: float64(index / 2),
		}); err != nil {
			t.Fatalf("append duplicate usage history: %v", err)
		}
	}

	loaded, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load duplicate usage history: %v", err)
	}
	want := (maxUsageHistoryPoints + 12) / 2
	if len(loaded) != want {
		t.Fatalf("loaded %d deduplicated points, want %d", len(loaded), want)
	}
	if loaded[0].UsedPercent != 0 || loaded[len(loaded)-1].UsedPercent != float64(want-1) {
		t.Fatalf("deduplicated history range = %v..%v, want 0..%d", loaded[0].UsedPercent, loaded[len(loaded)-1].UsedPercent, want-1)
	}
}

func TestUsageHistoryDeduplicationKeepsFirstRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "usage-history.jsonl")
	firstAt := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	secondAt := time.Date(2026, time.August, 27, 12, 5, 0, 0, time.UTC).Format(time.RFC3339)
	for _, point := range []HistoryPoint{
		{At: firstAt, UsedPercent: 12},
		{At: secondAt, UsedPercent: 12},
	} {
		if err := appendUsageHistory(path, point); err != nil {
			t.Fatalf("append repeated usage history: %v", err)
		}
	}

	loaded, err := loadUsageHistory(path)
	if err != nil {
		t.Fatalf("load repeated usage history: %v", err)
	}
	if len(loaded) != 1 || loaded[0].At != firstAt {
		t.Fatalf("deduplicated record = %#v, want first record at %s", loaded, firstAt)
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

func TestNextUsageHistorySampleAtReturnsNextLocalHour(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 8, 28, 14, 37, 29, 0, location)

	next := nextUsageHistorySampleAt(now)
	want := time.Date(2026, 8, 28, 15, 0, 0, 0, location)
	if !next.Equal(want) {
		t.Fatalf("nextUsageHistorySampleAt() = %s, want %s", next, want)
	}
}

func TestNextUsageHistorySampleAtRollsOverToNextDay(t *testing.T) {
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
	if len(loaded) != 2 || !loaded[1].Stale || loaded[1].UsedPercent != lastPoint.UsedPercent || loaded[1].FiveHourUsedPercent == nil || *loaded[1].FiveHourUsedPercent != fiveHour {
		t.Fatalf("fallback history = %#v, want stale copy of %#v", loaded, lastPoint)
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

func TestProxySchemes(t *testing.T) {
	for _, test := range []struct {
		name       string
		raw        string
		wantScheme string
	}{
		{name: "http", raw: "http://127.0.0.1:7890", wantScheme: "http"},
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

func TestResetPredictionRequestUsesPublicStatusEndpoint(t *testing.T) {
	var captured []*http.Request
	service := &UsageService{
		cfg: Config{CacheTTL: time.Minute},
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			captured = append(captured, request)
			body := `{"data":{"active_watch":null,"stats":{"total":0}},"meta":{"generated_at":"2026-08-22T08:34:14Z"}}`
			if request.URL.String() == resetHistoryEndpoint {
				body = `{"events":[{"tweet_id":"2090","tweet_url":"https://x.com/thsottiaux/status/2090","text":"Reset complete","announced_at":"2026-08-21T23:40:12Z","reset_type":"regular","source":"webhook"}]}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	if _, err := service.queryResetPrediction(context.Background()); err != nil {
		t.Fatalf("query reset prediction: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("captured %d requests, want status and history", len(captured))
	}
	if captured[0].Method != http.MethodGet || captured[0].URL.String() != resetStatusEndpoint {
		t.Fatalf("status request = %s %s, want GET %s", captured[0].Method, captured[0].URL, resetStatusEndpoint)
	}
	if captured[1].Method != http.MethodGet || captured[1].URL.String() != resetHistoryEndpoint {
		t.Fatalf("history request = %s %s, want GET %s", captured[1].Method, captured[1].URL, resetHistoryEndpoint)
	}
	if got := captured[0].Header.Get("User-Agent"); got != "codex-usage-dashboard/1.0" {
		t.Fatalf("User-Agent = %q, want dashboard user agent", got)
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
