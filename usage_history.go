package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func nextUsageHistorySampleAt(now time.Time) time.Time {
	localNow := now.In(now.Location())
	year, month, day := localNow.Date()
	nextMinute := ((localNow.Minute() / 5) + 1) * 5
	return time.Date(year, month, day, localNow.Hour(), nextMinute, 0, 0, localNow.Location())
}

func usageHistorySampleTimestamp(sampleAt time.Time) string {
	if sampleAt.IsZero() {
		return time.Now().UTC().Format(time.RFC3339)
	}
	return sampleAt.UTC().Format(time.RFC3339)
}

func (s *UsageService) persistUsageHistoryPoint(point HistoryPoint) {
	s.cacheMu.Lock()
	if !point.Stale {
		latestSuccessful := cloneHistoryPoint(point)
		s.lastSuccessfulHistory = &latestSuccessful
	}
	if len(s.rawHistory) == 0 {
		// Keep services constructed by older callers compatible while the raw
		// archive is introduced. New services always load the complete archive.
		// The derived lists are rebuilt here as well so the incremental
		// updates below always start from lists that match the raw archive.
		s.rawHistory = mergeUsageHistorySamples(s.history, s.weeklyHistory, s.fiveHourHistory)
		s.rawHistory = ensureOrderedUsageHistoryPoints(s.rawHistory)
		s.weeklyHistory = compactUsageHistoryMetricOrdered(s.rawHistory, usageHistoryMetricWeekly)
		s.fiveHourHistory = compactUsageHistoryMetricOrdered(s.rawHistory, usageHistoryMetricFiveHour)
		s.history = mergeUsageHistories(s.weeklyHistory, s.fiveHourHistory)
	}
	// The raw archive is maintained in chronological order: the sqlite loader
	// returns ordered rows and every accepted append is gated to be at least
	// one sample interval after the newest stored sample. The interval check
	// therefore only needs the newest element, not a re-sorted copy.
	if len(s.rawHistory) > 0 {
		lastAt, parseErr := time.Parse(time.RFC3339, s.rawHistory[len(s.rawHistory)-1].At)
		pointAt, pointErr := time.Parse(time.RFC3339, point.At)
		if parseErr == nil && pointErr == nil && pointAt.Sub(lastAt) < usageHistorySampleInterval {
			s.cacheMu.Unlock()
			return
		}
		if pointErr != nil {
			// An unparsable timestamp cannot be placed in the ordered archive
			// and would corrupt the incremental compaction invariant.
			s.cacheMu.Unlock()
			slog.Warn("skip usage history sample with invalid timestamp", "at", point.At)
			return
		}
	}
	if s.historyStore != nil {
		if err := s.historyStore.Insert(context.Background(), point); err != nil {
			s.cacheMu.Unlock()
			slog.Warn("persist usage history to sqlite failed", "error", err)
			return
		}
	}
	// Retain every scheduled sample in the raw archive. Each API timeline is
	// derived from that complete source, deduplicated by its own metric value,
	// and limited only after deduplication. The derived lists are updated
	// incrementally so a growing archive does not rescan every stored sample.
	s.rawHistory = append(s.rawHistory, cloneHistoryPoint(point))
	s.weeklyHistory = appendUsageHistoryPointToMetric(s.weeklyHistory, point, usageHistoryMetricWeekly)
	s.fiveHourHistory = appendUsageHistoryPointToMetric(s.fiveHourHistory, point, usageHistoryMetricFiveHour)
	s.history = mergeUsageHistories(s.weeklyHistory, s.fiveHourHistory)
	if s.cached != nil {
		s.cached.History = append([]HistoryPoint(nil), s.history...)
		s.cached.WeeklyHistory = append([]HistoryPoint(nil), s.weeklyHistory...)
		s.cached.FiveHourHistory = append([]HistoryPoint(nil), s.fiveHourHistory...)
	}
	rawHistory := append([]HistoryPoint(nil), s.rawHistory...)
	history := append([]HistoryPoint(nil), s.history...)
	weeklyHistory := append([]HistoryPoint(nil), s.weeklyHistory...)
	fiveHourHistory := append([]HistoryPoint(nil), s.fiveHourHistory...)
	s.cacheMu.Unlock()

	if s.historyStore != nil {
		return
	}
	if rawHistoryFile := s.rawHistoryPath(); rawHistoryFile != "" {
		if err := writeUsageHistory(rawHistoryFile, rawHistory); err != nil {
			slog.Warn("persist raw usage history failed", "error", err)
		}
	}
	if s.historyFile != "" {
		if err := writeUsageHistory(s.historyFile, history); err != nil {
			slog.Warn("persist usage history failed", "error", err)
		}
	}
	if weeklyHistoryFile := s.weeklyHistoryPath(); weeklyHistoryFile != "" {
		if err := writeUsageHistory(weeklyHistoryFile, weeklyHistory); err != nil {
			slog.Warn("persist weekly usage history failed", "error", err)
		}
	}
	if fiveHourHistoryFile := s.fiveHourHistoryPath(); fiveHourHistoryFile != "" {
		if err := writeUsageHistory(fiveHourHistoryFile, fiveHourHistory); err != nil {
			slog.Warn("persist five-hour usage history failed", "error", err)
		}
	}
}

func (s *UsageService) lastSuccessfulHistoryPoint() (HistoryPoint, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.lastSuccessfulHistory != nil {
		return cloneHistoryPoint(*s.lastSuccessfulHistory), true
	}
	history := s.rawHistory
	if len(history) == 0 {
		history = mergeUsageHistorySamples(s.history, s.weeklyHistory, s.fiveHourHistory)
	}
	for index := len(history) - 1; index >= 0; index-- {
		point := history[index]
		if point.Stale {
			continue
		}
		latestSuccessful := cloneHistoryPoint(point)
		s.lastSuccessfulHistory = &latestSuccessful
		return cloneHistoryPoint(latestSuccessful), true
	}
	return HistoryPoint{}, false
}

func usageHistoryPoint(usage *UsageResponse) (HistoryPoint, bool) {
	if usage == nil || usage.SevenDay == nil || strings.TrimSpace(usage.FetchedAt) == "" {
		return HistoryPoint{}, false
	}
	point := HistoryPoint{
		At:          usage.FetchedAt,
		UsedPercent: usage.SevenDay.UsedPercent,
	}
	if usage.FiveHour != nil {
		fiveHourUsedPercent := usage.FiveHour.UsedPercent
		point.FiveHourUsedPercent = &fiveHourUsedPercent
	}
	return point, true
}

// deduplicateUsageHistoryMetric keeps the latest successful record for every
// distinct value of one metric, falling back to the latest stale record when
// no successful sample exists. Timestamps, stale markers, and the other quota
// window do not make an unchanged metric value a new chart point.
func deduplicateUsageHistoryMetric(points []HistoryPoint, metric usageHistoryMetric) []HistoryPoint {
	if len(points) == 0 {
		return nil
	}
	compact := make([]HistoryPoint, 0, len(points))
	seen := make(map[string]struct{}, len(points))
	hasSuccessful := make(map[string]bool, len(points))
	for _, point := range points {
		if metric == usageHistoryMetricFiveHour && point.FiveHourUsedPercent == nil {
			continue
		}
		if !point.Stale {
			hasSuccessful[usageHistoryMetricValueKey(point, metric)] = true
		}
	}
	for index := len(points) - 1; index >= 0; index-- {
		point := points[index]
		if metric == usageHistoryMetricFiveHour && point.FiveHourUsedPercent == nil {
			continue
		}
		key := usageHistoryMetricValueKey(point, metric)
		if point.Stale && hasSuccessful[key] {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		compact = append(compact, point)
	}
	for left, right := 0, len(compact)-1; left < right; left, right = left+1, right-1 {
		compact[left], compact[right] = compact[right], compact[left]
	}
	return compact
}

func usageHistoryMetricValueKey(point HistoryPoint, metric usageHistoryMetric) string {
	value := "nil"
	if metric == usageHistoryMetricWeekly {
		value = strconv.FormatFloat(point.UsedPercent, 'g', -1, 64)
	} else if point.FiveHourUsedPercent != nil {
		value = strconv.FormatFloat(*point.FiveHourUsedPercent, 'g', -1, 64)
	}
	// Weekly and five-hour histories are deduplicated only by their own value,
	// so changes in the other quota window cannot consume this metric's limit.
	return value
}

// usageHistoryTimestampLess orders samples chronologically. RFC3339 timestamps
// compare as a fallback so malformed entries keep a stable relative position.
func usageHistoryTimestampLess(left, right HistoryPoint) bool {
	leftAt, leftErr := time.Parse(time.RFC3339, left.At)
	rightAt, rightErr := time.Parse(time.RFC3339, right.At)
	if leftErr == nil && rightErr == nil && !leftAt.Equal(rightAt) {
		return leftAt.Before(rightAt)
	}
	return left.At < right.At
}

func orderUsageHistoryPoints(points []HistoryPoint) []HistoryPoint {
	ordered := append([]HistoryPoint(nil), points...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return usageHistoryTimestampLess(ordered[left], ordered[right])
	})
	return ordered
}

// ensureOrderedUsageHistoryPoints trusts an already ordered archive and only
// pays for a sort when the string-adjacency check detects out-of-order
// samples. Runtime appends are gated to be monotonic, so the steady-state
// cost of loading the database is a single linear scan.
func ensureOrderedUsageHistoryPoints(points []HistoryPoint) []HistoryPoint {
	for index := 1; index < len(points); index++ {
		if points[index-1].At > points[index].At {
			return orderUsageHistoryPoints(points)
		}
	}
	return points
}

// limitUsageHistoryPoints retains the latest distinct values after
// metric-specific deduplication has completed.
func limitUsageHistoryPoints(points []HistoryPoint) []HistoryPoint {
	if len(points) > maxUsageHistoryPoints {
		return points[len(points)-maxUsageHistoryPoints:]
	}
	return points
}

// compactUsageHistoryMetric first keeps the latest successful occurrence of
// every value for one metric, then retains the latest 48 distinct values. The
// other quota window cannot consume this metric's limit.
func compactUsageHistoryMetric(points []HistoryPoint, metric usageHistoryMetric) []HistoryPoint {
	return compactUsageHistoryMetricOrdered(orderUsageHistoryPoints(points), metric)
}

// compactUsageHistoryMetricOrdered expects points in chronological order, as
// produced by the sqlite loader and the runtime append gate, and skips the
// re-sort.
func compactUsageHistoryMetricOrdered(points []HistoryPoint, metric usageHistoryMetric) []HistoryPoint {
	return limitUsageHistoryPoints(deduplicateUsageHistoryMetric(points, metric))
}

// appendUsageHistoryPointToMetric applies one new sample to a metric's already
// compacted list and produces the same result as re-running
// compactUsageHistoryMetric over the extended archive: an unchanged metric
// value only moves that value's kept record forward, a stale sample never
// replaces a successful record of the same value, and the 48-point limit
// applies after the value-level update.
func appendUsageHistoryPointToMetric(list []HistoryPoint, point HistoryPoint, metric usageHistoryMetric) []HistoryPoint {
	if metric == usageHistoryMetricFiveHour && point.FiveHourUsedPercent == nil {
		return list
	}
	key := usageHistoryMetricValueKey(point, metric)
	for index, existing := range list {
		if usageHistoryMetricValueKey(existing, metric) != key {
			continue
		}
		if point.Stale && !existing.Stale {
			return list
		}
		updated := append(make([]HistoryPoint, 0, len(list)), list[:index]...)
		updated = append(updated, list[index+1:]...)
		return limitUsageHistoryPoints(append(updated, cloneHistoryPoint(point)))
	}
	return limitUsageHistoryPoints(append(append(make([]HistoryPoint, 0, len(list)+1), list...), cloneHistoryPoint(point)))
}

func mergeUsageHistorySamples(histories ...[]HistoryPoint) []HistoryPoint {
	total := 0
	for _, history := range histories {
		total += len(history)
	}
	if total == 0 {
		return nil
	}
	merged := make([]HistoryPoint, 0, total)
	indexes := make(map[string]int, total)
	for _, history := range histories {
		for _, point := range history {
			point = cloneHistoryPoint(point)
			if index, ok := indexes[point.At]; ok {
				merged[index].UsedPercent = point.UsedPercent
				merged[index].Stale = point.Stale
				if point.FiveHourUsedPercent != nil {
					fiveHourUsedPercent := *point.FiveHourUsedPercent
					merged[index].FiveHourUsedPercent = &fiveHourUsedPercent
				}
				continue
			}
			indexes[point.At] = len(merged)
			merged = append(merged, point)
		}
	}
	sort.SliceStable(merged, func(left, right int) bool {
		return usageHistoryTimestampLess(merged[left], merged[right])
	})
	return merged
}

func usageHistoryMetricPath(path string, metric usageHistoryMetric) string {
	suffix := "weekly"
	if metric == usageHistoryMetricFiveHour {
		suffix = "five-hour"
	}
	extension := filepath.Ext(path)
	if extension == "" {
		return path + "-" + suffix
	}
	return strings.TrimSuffix(path, extension) + "-" + suffix + extension
}

func usageHistoryRawPath(path string) string {
	extension := filepath.Ext(path)
	if extension == "" {
		return path + "-raw"
	}
	return strings.TrimSuffix(path, extension) + "-raw" + extension
}

func (s *UsageService) rawHistoryPath() string {
	if s.rawHistoryFile != "" {
		return s.rawHistoryFile
	}
	if s.historyFile == "" {
		return ""
	}
	return usageHistoryRawPath(s.historyFile)
}

func (s *UsageService) weeklyHistoryPath() string {
	if s.weeklyHistoryFile != "" {
		return s.weeklyHistoryFile
	}
	if s.historyFile == "" {
		return ""
	}
	return usageHistoryMetricPath(s.historyFile, usageHistoryMetricWeekly)
}

func (s *UsageService) fiveHourHistoryPath() string {
	if s.fiveHourHistoryFile != "" {
		return s.fiveHourHistoryFile
	}
	if s.historyFile == "" {
		return ""
	}
	return usageHistoryMetricPath(s.historyFile, usageHistoryMetricFiveHour)
}

func mergeUsageHistories(weeklyHistory, fiveHourHistory []HistoryPoint) []HistoryPoint {
	if len(weeklyHistory) == 0 && len(fiveHourHistory) == 0 {
		return nil
	}
	merged := make([]HistoryPoint, 0, len(weeklyHistory)+len(fiveHourHistory))
	indexes := make(map[string]int, len(weeklyHistory)+len(fiveHourHistory))
	add := func(point HistoryPoint, weekly bool) {
		point = cloneHistoryPoint(point)
		if index, ok := indexes[point.At]; ok {
			if weekly {
				merged[index].UsedPercent = point.UsedPercent
				merged[index].Stale = point.Stale
			} else if point.FiveHourUsedPercent != nil {
				fiveHourUsedPercent := *point.FiveHourUsedPercent
				merged[index].FiveHourUsedPercent = &fiveHourUsedPercent
			}
			return
		}
		indexes[point.At] = len(merged)
		merged = append(merged, point)
	}
	for _, point := range weeklyHistory {
		add(point, true)
	}
	for _, point := range fiveHourHistory {
		add(point, false)
	}
	sort.SliceStable(merged, func(left, right int) bool {
		return usageHistoryTimestampLess(merged[left], merged[right])
	})
	return merged
}

func cloneHistoryPoint(point HistoryPoint) HistoryPoint {
	if point.FiveHourUsedPercent != nil {
		fiveHourUsedPercent := *point.FiveHourUsedPercent
		point.FiveHourUsedPercent = &fiveHourUsedPercent
	}
	return point
}

// compactUsageHistory keeps the legacy combined history behavior for old
// callers and migration data. Its deduplication is based on the weekly
// metric only; runtime persistence uses compactUsageHistoryMetric so the two
// quota windows have separate limits.
func compactUsageHistory(points []HistoryPoint) []HistoryPoint {
	return compactUsageHistoryMetric(points, usageHistoryMetricWeekly)
}

// latestSuccessfulHistoryPoint scans an ordered archive backwards for the
// newest non-stale sample. Callers must pass chronologically ordered points.
func latestSuccessfulHistoryPoint(points []HistoryPoint) (HistoryPoint, bool) {
	for index := len(points) - 1; index >= 0; index-- {
		if points[index].Stale {
			continue
		}
		return cloneHistoryPoint(points[index]), true
	}
	return HistoryPoint{}, false
}

func loadUsageHistory(path string) ([]HistoryPoint, error) {
	history, err := loadUsageHistoryRecords(path)
	if err != nil {
		return nil, err
	}
	return compactUsageHistory(history), nil
}

func loadUsageHistoryRecords(path string) ([]HistoryPoint, error) {
	history, exists, invalid, err := readUsageHistoryFile(path)
	if err == nil && exists && !invalid && len(history) > 0 {
		return history, nil
	}

	backupPath := usageHistoryBackupPath(path)
	backup, backupExists, backupInvalid, backupErr := readUsageHistoryFile(backupPath)
	if backupErr == nil && backupExists && !backupInvalid && len(backup) > 0 {
		slog.Warn("usage history restored from backup", "path", path, "backup", backupPath, "points", len(backup))
		return backup, nil
	}
	if err != nil {
		return nil, err
	}
	if backupErr != nil {
		slog.Warn("usage history backup could not be read", "path", backupPath, "error", backupErr)
	}
	return history, nil
}

func loadCompleteUsageHistory(path string) ([]HistoryPoint, bool, error) {
	history, exists, invalid, err := readUsageHistoryFile(path)
	if err != nil {
		return nil, false, err
	}
	if exists && !invalid {
		return history, true, nil
	}

	backupPath := usageHistoryBackupPath(path)
	backup, backupExists, backupInvalid, backupErr := readUsageHistoryFile(backupPath)
	if backupErr == nil && backupExists && !backupInvalid {
		slog.Warn("raw usage history restored from backup", "path", path, "backup", backupPath, "points", len(backup))
		return backup, true, nil
	}
	if backupErr != nil {
		slog.Warn("raw usage history backup could not be read", "path", backupPath, "error", backupErr)
	}
	return nil, false, nil
}

func loadUsageHistoryMigrationRecords(paths ...string) ([]HistoryPoint, error) {
	histories := make([][]HistoryPoint, 0, len(paths)*2)
	for _, path := range paths {
		for _, snapshotPath := range []string{path, usageHistoryBackupPath(path)} {
			history, exists, invalid, err := readUsageHistoryFile(snapshotPath)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", snapshotPath, err)
			}
			if exists && !invalid && len(history) > 0 {
				histories = append(histories, history)
			}
		}
	}
	return mergeUsageHistorySamples(histories...), nil
}

func loadUsageHistoryMetric(path string, metric usageHistoryMetric) ([]HistoryPoint, bool, error) {
	history, exists, invalid, err := readUsageHistoryFile(path)
	if err == nil && exists && !invalid && len(history) > 0 {
		return compactUsageHistoryMetric(history, metric), true, nil
	}

	backupPath := usageHistoryBackupPath(path)
	backup, backupExists, backupInvalid, backupErr := readUsageHistoryFile(backupPath)
	if backupErr == nil && backupExists && !backupInvalid && len(backup) > 0 {
		slog.Warn("metric usage history restored from backup", "path", path, "backup", backupPath, "points", len(backup), "metric", metric)
		return compactUsageHistoryMetric(backup, metric), true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if backupErr != nil {
		slog.Warn("metric usage history backup could not be read", "path", backupPath, "error", backupErr, "metric", metric)
	}
	return nil, false, nil
}

func readUsageHistoryFile(path string) ([]HistoryPoint, bool, bool, error) {
	raw, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, false, nil
		}
		return nil, false, false, err
	}
	defer raw.Close()

	history := make([]HistoryPoint, 0, maxCombinedUsageHistoryPoints)
	invalid := false
	scanner := bufio.NewScanner(raw)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var point HistoryPoint
		if err := json.Unmarshal(line, &point); err != nil {
			invalid = true
			continue
		}
		if strings.TrimSpace(point.At) == "" || point.UsedPercent < 0 || point.UsedPercent > 100 {
			invalid = true
			continue
		}
		if point.FiveHourUsedPercent != nil && (*point.FiveHourUsedPercent < 0 || *point.FiveHourUsedPercent > 100) {
			invalid = true
			continue
		}
		if _, err := time.Parse(time.RFC3339, point.At); err != nil {
			invalid = true
			continue
		}
		history = append(history, point)
	}
	if err := scanner.Err(); err != nil {
		return nil, true, invalid, err
	}
	return history, true, invalid, nil
}

func usageHistoryBackupPath(path string) string {
	return path + ".bak"
}

func writeUsageHistory(path string, history []HistoryPoint) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("usage history path is empty")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}

	payload, err := encodeUsageHistory(history)
	if err != nil {
		return err
	}

	current, exists, invalid, readErr := readUsageHistoryFile(path)
	if readErr != nil {
		return fmt.Errorf("read existing usage history: %w", readErr)
	}
	if exists && !invalid && len(current) > 0 {
		backupPayload, encodeErr := encodeUsageHistory(current)
		if encodeErr != nil {
			return encodeErr
		}
		if err := writeUsageHistoryFileAtomic(usageHistoryBackupPath(path), backupPayload); err != nil {
			return fmt.Errorf("write usage history backup: %w", err)
		}
	}

	if err := writeUsageHistoryFileAtomic(path, payload); err != nil {
		return fmt.Errorf("replace usage history: %w", err)
	}
	return nil
}

func rewriteUsageHistoryIfChanged(path string, history []HistoryPoint) error {
	current, exists, invalid, err := readUsageHistoryFile(path)
	if err != nil {
		return err
	}
	if !exists && len(history) == 0 {
		return nil
	}
	if exists && !invalid && usageHistoriesEqual(current, history) {
		return nil
	}
	return writeUsageHistory(path, history)
}

func usageHistoriesEqual(left, right []HistoryPoint) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].At != right[index].At || left[index].UsedPercent != right[index].UsedPercent || left[index].Stale != right[index].Stale {
			return false
		}
		if (left[index].FiveHourUsedPercent == nil) != (right[index].FiveHourUsedPercent == nil) {
			return false
		}
		if left[index].FiveHourUsedPercent != nil && *left[index].FiveHourUsedPercent != *right[index].FiveHourUsedPercent {
			return false
		}
	}
	return true
}

func encodeUsageHistory(history []HistoryPoint) ([]byte, error) {
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	for _, point := range history {
		if err := encoder.Encode(point); err != nil {
			return nil, err
		}
	}
	return payload.Bytes(), nil
}

func writeUsageHistoryFileAtomic(path string, payload []byte) error {
	return writeFileAtomic(path, payload, 0o600)
}

func appendUsageHistory(path string, point HistoryPoint) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("usage history path is empty")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(point); err != nil {
		return err
	}
	return nil
}
