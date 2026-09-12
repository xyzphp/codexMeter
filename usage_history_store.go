package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const defaultUsageDatabaseFile = "data/codex-meter.db"

// UsageHistoryStore is the durable store for every scheduled quota sample.
// Presentation-specific deduplication remains in the service layer so the
// raw table never loses samples merely because a chart has a 48-point limit.
type UsageHistoryStore struct {
	db   *sql.DB
	path string
}

func OpenUsageHistoryStore(path string) (*UsageHistoryStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("usage history database path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	store := &UsageHistoryStore{db: db, path: path}
	// A single writer is enough for the collector and avoids unnecessary lock
	// contention in the embedded database while keeping reads deterministic.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := store.configure(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *UsageHistoryStore) configure() error {
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("configure sqlite database: %w", err)
		}
	}
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS quota_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    sampled_at TEXT NOT NULL UNIQUE,
    used_percent REAL NOT NULL CHECK (used_percent >= 0 AND used_percent <= 100),
    five_hour_used_percent REAL CHECK (five_hour_used_percent IS NULL OR (five_hour_used_percent >= 0 AND five_hour_used_percent <= 100)),
    stale INTEGER NOT NULL DEFAULT 0 CHECK (stale IN (0, 1))
);
CREATE INDEX IF NOT EXISTS idx_quota_history_sampled_at ON quota_history(sampled_at);
`); err != nil {
		return fmt.Errorf("initialize sqlite schema: %w", err)
	}
	return nil
}

func (s *UsageHistoryStore) Load(ctx context.Context) ([]HistoryPoint, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("usage history database is not open")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sampled_at, used_percent, five_hour_used_percent, stale
FROM quota_history
ORDER BY sampled_at ASC, id ASC
`)
	if err != nil {
		return nil, fmt.Errorf("query quota history: %w", err)
	}
	defer rows.Close()

	history := make([]HistoryPoint, 0)
	for rows.Next() {
		var (
			point    HistoryPoint
			fiveHour sql.NullFloat64
			stale    int
		)
		if err := rows.Scan(&point.At, &point.UsedPercent, &fiveHour, &stale); err != nil {
			return nil, fmt.Errorf("scan quota history: %w", err)
		}
		if fiveHour.Valid {
			value := fiveHour.Float64
			point.FiveHourUsedPercent = &value
		}
		point.Stale = stale != 0
		history = append(history, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read quota history: %w", err)
	}
	return history, nil
}

func (s *UsageHistoryStore) Insert(ctx context.Context, point HistoryPoint) error {
	if s == nil || s.db == nil {
		return errors.New("usage history database is not open")
	}
	if strings.TrimSpace(point.At) == "" {
		return errors.New("usage history timestamp is empty")
	}
	var fiveHour any
	if point.FiveHourUsedPercent != nil {
		fiveHour = *point.FiveHourUsedPercent
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO quota_history (sampled_at, used_percent, five_hour_used_percent, stale)
VALUES (?, ?, ?, ?)
ON CONFLICT(sampled_at) DO UPDATE SET
    used_percent = excluded.used_percent,
    five_hour_used_percent = excluded.five_hour_used_percent,
    stale = excluded.stale
`, point.At, point.UsedPercent, fiveHour, boolToSQLiteInt(point.Stale))
	if err != nil {
		return fmt.Errorf("insert quota history: %w", err)
	}
	return nil
}

func (s *UsageHistoryStore) Import(ctx context.Context, history []HistoryPoint) error {
	if s == nil || s.db == nil {
		return errors.New("usage history database is not open")
	}
	if len(history) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin quota history import: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `
INSERT INTO quota_history (sampled_at, used_percent, five_hour_used_percent, stale)
VALUES (?, ?, ?, ?)
ON CONFLICT(sampled_at) DO UPDATE SET
    used_percent = excluded.used_percent,
    five_hour_used_percent = excluded.five_hour_used_percent,
    stale = excluded.stale
`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare quota history import: %w", err)
	}
	defer statement.Close()
	for _, point := range history {
		var fiveHour any
		if point.FiveHourUsedPercent != nil {
			fiveHour = *point.FiveHourUsedPercent
		}
		if _, err := statement.ExecContext(ctx, point.At, point.UsedPercent, fiveHour, boolToSQLiteInt(point.Stale)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("import quota history: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit quota history import: %w", err)
	}
	return nil
}

func (s *UsageHistoryStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *UsageHistoryStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *UsageService) Close() error {
	if s == nil || s.historyStore == nil {
		return nil
	}
	return s.historyStore.Close()
}

func boolToSQLiteInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
