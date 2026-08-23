package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
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

func TestWhamRequestUsesOAuthHeadersAndGET(t *testing.T) {
	var captured *http.Request
	service := &UsageService{
		cfg: Config{
			AccessToken:      "oauth-token",
			ChatGPTAccountID: "account-id",
			UserAgent:        "test-user-agent",
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
		"Authorization":      "Bearer oauth-token",
		"ChatGPT-Account-Id": "account-id",
		"Openai-Beta":        "codex-1",
		"Oai-Language":       "zh-CN",
		"Originator":         "Codex Desktop",
		"Accept":             "application/json",
		"User-Agent":         "test-user-agent",
	}
	for name, want := range wantHeaders {
		if got := captured.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

func TestConfigViewDoesNotExposeUsageProvider(t *testing.T) {
	service := &UsageService{cfg: Config{
		AccessToken:      "oauth-token",
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
	service := &UsageService{
		cfg: Config{
			AccessToken:      "oauth-token",
			ChatGPTAccountID: "account-id",
			UserAgent:        "test-user-agent",
		},
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			paths = append(paths, request.URL.Path)
			body := `{"data":[]}`
			if request.URL.Path == "/backend-api/wham/usage/daily-token-usage-breakdown" {
				body = `{"data":[{"date":"2026-08-22","product_surface_usage_values":{"desktop_app":12.5},"models":[{"model":"gpt-5.6-luna","speed":"standard","credits":10},{"model":"gpt-5.6-luna","speed":"fast","credits":2.5}]}]}`
			}
			if request.URL.Path == "/backend-api/wham/analytics/daily-workspace-usage-counts" {
				body = `{"data":[{"date":"2026-08-22","totals":{"users":1,"threads":2,"turns":3,"credits":12.5,"uncached_text_input_tokens":10,"cached_text_input_tokens":20,"text_output_tokens":30,"text_total_tokens":60}}]}`
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
	if len(analytics.Days) != 7 || analytics.Days[6].Date != "2026-08-22" || analytics.Days[6].TokenUsagePercent != 12.5 || analytics.Days[6].Turns != 3 || analytics.Days[6].TextTotalTokens != 60 || len(analytics.Days[6].Models) != 1 || analytics.Days[6].Models[0].Model != "gpt-5.6-luna" || analytics.Days[6].Models[0].UsagePercent != 12.5 {
		t.Fatalf("unexpected analytics payload: %#v", analytics)
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
	var captured *http.Request
	service := &UsageService{
		cfg: Config{CacheTTL: time.Minute},
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			captured = request
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"data":{"active_watch":null,"stats":{"total":0}},"meta":{"generated_at":"2026-08-22T08:34:14Z"}}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	if _, err := service.queryResetPrediction(context.Background()); err != nil {
		t.Fatalf("query reset prediction: %v", err)
	}
	if captured == nil {
		t.Fatal("reset status request was not captured")
	}
	if captured.Method != http.MethodGet || captured.URL.String() != resetStatusEndpoint {
		t.Fatalf("request = %s %s, want GET %s", captured.Method, captured.URL, resetStatusEndpoint)
	}
	if got := captured.Header.Get("User-Agent"); got != "codex-usage-dashboard/1.0" {
		t.Fatalf("User-Agent = %q, want dashboard user agent", got)
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
