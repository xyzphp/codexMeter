package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Window struct {
	UsedPercent       float64   `json:"used_percent"`
	WindowMinutes     int       `json:"window_minutes,omitempty"`
	ResetAfterSeconds int       `json:"reset_after_seconds,omitempty"`
	ResetAt           time.Time `json:"reset_at"`
	RemainingSeconds  int64     `json:"remaining_seconds"`
}

type UsageResponse struct {
	Source                string         `json:"source"`
	PlanType              string         `json:"plan_type,omitempty"`
	Email                 string         `json:"email,omitempty"`
	RateLimitAllowed      bool           `json:"rate_limit_allowed"`
	RateLimitReached      bool           `json:"rate_limit_reached"`
	RateLimitReachedType  string         `json:"rate_limit_reached_type,omitempty"`
	Credits               *Credits       `json:"credits,omitempty"`
	SpendControl          *SpendControl  `json:"spend_control,omitempty"`
	RateLimitResetCredits *ResetCredits  `json:"rate_limit_reset_credits,omitempty"`
	FetchedAt             string         `json:"fetched_at"`
	FromCache             bool           `json:"from_cache"`
	FiveHour              *Window        `json:"five_hour,omitempty"`
	SevenDay              *Window        `json:"seven_day,omitempty"`
	History               []HistoryPoint `json:"history,omitempty"`
	WeeklyHistory         []HistoryPoint `json:"weekly_history,omitempty"`
	FiveHourHistory       []HistoryPoint `json:"five_hour_history,omitempty"`
}

type Credits struct {
	HasCredits          bool  `json:"has_credits"`
	Unlimited           bool  `json:"unlimited"`
	OverageLimitReached bool  `json:"overage_limit_reached"`
	Balance             any   `json:"balance,omitempty"`
	ApproxLocalMessages []int `json:"approx_local_messages,omitempty"`
	ApproxCloudMessages []int `json:"approx_cloud_messages,omitempty"`
}

type SpendControl struct {
	Reached         bool `json:"reached"`
	IndividualLimit any  `json:"individual_limit,omitempty"`
}

type ResetCredits struct {
	AvailableCount           int `json:"available_count"`
	ApplicableAvailableCount int `json:"applicable_available_count"`
}

type HistoryPoint struct {
	At                  string   `json:"at"`
	UsedPercent         float64  `json:"used_percent"`
	FiveHourUsedPercent *float64 `json:"five_hour_used_percent,omitempty"`
	Stale               bool     `json:"stale,omitempty"`
}

type usageHistoryMetric uint8

const (
	usageHistoryMetricWeekly usageHistoryMetric = iota
	usageHistoryMetricFiveHour
)

// UsageAnalytics is the compact date-range payload exposed to the frontend.
// The upstream responses contain a longer history and several nested
// breakdowns; keeping only the fields needed by the dashboard makes the
// Android WebView page faster and avoids exposing the raw upstream payload.
type UsageAnalytics struct {
	Source    string                `json:"source"`
	FetchedAt string                `json:"fetched_at"`
	FromCache bool                  `json:"from_cache"`
	StartDate string                `json:"start_date"`
	EndDate   string                `json:"end_date"`
	Days      []UsageAnalyticsDay   `json:"days"`
	Summary   UsageAnalyticsSummary `json:"summary"`
}

const analyticsDateLayout = "2006-01-02"

type analyticsDateRange struct {
	StartDate string
	EndDate   string
}

func previousAnalyticsDateRange(now time.Time, days int) analyticsDateRange {
	if days < 1 {
		days = 1
	}
	return analyticsDateRange{
		StartDate: now.AddDate(0, 0, -days).Format(analyticsDateLayout),
		EndDate:   now.AddDate(0, 0, -1).Format(analyticsDateLayout),
	}
}

func parseAnalyticsDateRange(values url.Values, now time.Time) (analyticsDateRange, error) {
	startRaw := strings.TrimSpace(values.Get("start_date"))
	endRaw := strings.TrimSpace(values.Get("end_date"))
	if startRaw == "" && endRaw == "" {
		// Keep the legacy API default for the LX04 page. The browser page sends
		// its one-year range explicitly so both layouts can coexist.
		return previousAnalyticsDateRange(now, 7), nil
	}
	if startRaw == "" || endRaw == "" {
		return analyticsDateRange{}, errors.New("start_date and end_date must be provided together")
	}
	start, err := time.ParseInLocation(analyticsDateLayout, startRaw, now.Location())
	if err != nil {
		return analyticsDateRange{}, errors.New("start_date must use YYYY-MM-DD")
	}
	end, err := time.ParseInLocation(analyticsDateLayout, endRaw, now.Location())
	if err != nil {
		return analyticsDateRange{}, errors.New("end_date must use YYYY-MM-DD")
	}
	if start.After(end) {
		return analyticsDateRange{}, errors.New("start_date must not be after end_date")
	}
	if end.After(now) {
		return analyticsDateRange{}, errors.New("end_date must not be in the future")
	}
	if end.After(start.AddDate(0, 0, 366)) {
		return analyticsDateRange{}, errors.New("date range cannot exceed 367 days")
	}
	return analyticsDateRange{StartDate: startRaw, EndDate: endRaw}, nil
}

func (r analyticsDateRange) key() string {
	return r.StartDate + ":" + r.EndDate
}

type UsageAnalyticsDay struct {
	Date                    string                `json:"date"`
	TokenUsagePercent       float64               `json:"token_usage_percent"`
	Credits                 float64               `json:"credits"`
	Users                   int64                 `json:"users"`
	Threads                 int64                 `json:"threads"`
	Turns                   int64                 `json:"turns"`
	UncachedTextInputTokens int64                 `json:"uncached_text_input_tokens"`
	CachedTextInputTokens   int64                 `json:"cached_text_input_tokens"`
	TextOutputTokens        int64                 `json:"text_output_tokens"`
	TextTotalTokens         int64                 `json:"text_total_tokens"`
	Models                  []UsageAnalyticsModel `json:"models,omitempty"`
}

type UsageAnalyticsModel struct {
	Model        string  `json:"model"`
	UsagePercent float64 `json:"usage_percent"`
}

type UsageAnalyticsSummary struct {
	TokenUsagePercent float64 `json:"token_usage_percent"`
	Credits           float64 `json:"credits"`
	Users             int64   `json:"users"`
	Threads           int64   `json:"threads"`
	Turns             int64   `json:"turns"`
	TextTotalTokens   int64   `json:"text_total_tokens"`
}

type dailyTokenUsageEnvelope struct {
	Data []dailyTokenUsagePoint `json:"data"`
}

type dailyTokenUsagePoint struct {
	Date                      string             `json:"date"`
	ProductSurfaceUsageValues map[string]float64 `json:"product_surface_usage_values"`
	Models                    []dailyTokenModel  `json:"models"`
}

type dailyTokenModel struct {
	Model   string  `json:"model"`
	Speed   string  `json:"speed"`
	Credits float64 `json:"credits"`
}

type dailyWorkspaceUsageEnvelope struct {
	Data []dailyWorkspaceUsagePoint `json:"data"`
}

type dailyWorkspaceUsagePoint struct {
	Date   string                    `json:"date"`
	Totals dailyWorkspaceUsageTotals `json:"totals"`
}

type dailyWorkspaceUsageTotals struct {
	Users                   int64   `json:"users"`
	Threads                 int64   `json:"threads"`
	Turns                   int64   `json:"turns"`
	UncachedTextInputTokens int64   `json:"uncached_text_input_tokens"`
	CachedTextInputTokens   int64   `json:"cached_text_input_tokens"`
	TextOutputTokens        int64   `json:"text_output_tokens"`
	TextTotalTokens         int64   `json:"text_total_tokens"`
	Credits                 float64 `json:"credits"`
}

type ResetPrediction struct {
	Source        string       `json:"source"`
	FetchedAt     string       `json:"fetched_at"`
	FromCache     bool         `json:"from_cache"`
	LatestReset   *ResetEvent  `json:"latest_reset,omitempty"`
	ActiveWatch   *ResetWatch  `json:"active_watch,omitempty"`
	CommunityPoll *ResetPoll   `json:"community_poll,omitempty"`
	History       []ResetEvent `json:"history,omitempty"`
	Stats         ResetStats   `json:"stats"`
}

type ResetEvent struct {
	ID          string       `json:"id"`
	ResetType   string       `json:"reset_type"`
	AnnouncedAt string       `json:"announced_at"`
	Text        string       `json:"text"`
	Source      *ResetSource `json:"source,omitempty"`
}

type ResetSource struct {
	Type   string `json:"type"`
	Author string `json:"author"`
	URL    string `json:"url"`
}

type ResetWatch struct {
	ResetChancePercent float64 `json:"reset_chance_percent"`
	ForecastWindow     string  `json:"forecast_window"`
	ObservedAt         string  `json:"observed_at"`
	ExpiresAt          string  `json:"expires_at"`
	Level              string  `json:"level"`
}

type ResetPoll struct {
	YesVotes   int     `json:"yes_votes"`
	NoVotes    int     `json:"no_votes"`
	TotalVotes int     `json:"total_votes"`
	YesPercent float64 `json:"yes_percent"`
}

type ResetStats struct {
	Total           int     `json:"total"`
	LastResetAt     string  `json:"last_reset_at"`
	DaysSinceLast   float64 `json:"days_since_last"`
	AvgIntervalDays float64 `json:"avg_interval_days"`
}

type resetStatusEnvelope struct {
	Data struct {
		LatestReset *ResetEvent `json:"latest_reset"`
		ActiveWatch *ResetWatch `json:"active_watch"`
		Stats       ResetStats  `json:"stats"`
	} `json:"data"`
	Meta struct {
		APIVersion  string `json:"api_version"`
		GeneratedAt string `json:"generated_at"`
	} `json:"meta"`
}

type resetHistoryEnvelope struct {
	Events []resetHistoryEvent `json:"events"`
}

type resetHistoryEvent struct {
	TweetID     string `json:"tweet_id"`
	TweetURL    string `json:"tweet_url"`
	Text        string `json:"text"`
	AnnouncedAt string `json:"announced_at"`
	ResetType   string `json:"reset_type"`
	Source      string `json:"source"`
}

var (
	resetPollTagPattern = regexp.MustCompile(`(?is)<[^>]*\bdata-role\s*=\s*["']watch-poll["'][^>]*>`)
	resetPollYesPattern = regexp.MustCompile(`(?i)\bdata-yes\s*=\s*["']([0-9]+)["']`)
	resetPollNoPattern  = regexp.MustCompile(`(?i)\bdata-no\s*=\s*["']([0-9]+)["']`)
)

type rawWindow struct {
	UsedPercent       *float64
	WindowMinutes     *int
	ResetAfterSeconds *int
	ResetAtUnix       *int64
}

// whamUsageResponse is the read-only quota payload returned by
// GET /backend-api/wham/usage. The fields are pointers because the upstream
// sometimes omits a secondary window or reset value.
type whamUsageResponse struct {
	PlanType              string                    `json:"plan_type"`
	Email                 string                    `json:"email"`
	RateLimit             *whamRateLimit            `json:"rate_limit"`
	CodeReviewRateLimit   *whamRateLimit            `json:"code_review_rate_limit"`
	AdditionalRateLimits  []whamAdditionalRateLimit `json:"additional_rate_limits"`
	Credits               *Credits                  `json:"credits"`
	SpendControl          *SpendControl             `json:"spend_control"`
	RateLimitReachedType  rateLimitReachedType      `json:"rate_limit_reached_type"`
	RateLimitResetCredits *ResetCredits             `json:"rate_limit_reset_credits"`
}

// rateLimitReachedType accepts both the historical string form and the
// object form returned after a five-hour limit is reached.
type rateLimitReachedType string

func (value *rateLimitReachedType) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*value = ""
		return nil
	}
	if trimmed[0] != '{' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*value = rateLimitReachedType(strings.TrimSpace(text))
		return nil
	}
	var payload struct {
		Type    string `json:"type"`
		Details string `json:"details"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	*value = rateLimitReachedType(strings.TrimSpace(payload.Type))
	return nil
}

type whamAdditionalRateLimit struct {
	MeteredFeature string         `json:"metered_feature"`
	RateLimit      *whamRateLimit `json:"rate_limit"`
}

type whamRateLimit struct {
	Allowed         bool        `json:"allowed"`
	LimitReached    bool        `json:"limit_reached"`
	PrimaryWindow   *whamWindow `json:"primary_window"`
	SecondaryWindow *whamWindow `json:"secondary_window"`
}

type whamWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}
