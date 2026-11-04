package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
)

// TaskRecord represents a row from fetch_tasks.
type TaskRecord struct {
	ID        int64
	SourceID  int64
	TaskKey   string
	TaskJSON  string
	Priority  int
	Status    string
	NotBefore time.Time
	Attempts  int
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// QueueStats summarizes fetch tasks for a source.
type QueueStats struct {
	Pending     int
	Done        int
	Failed      int
	NextDue     *time.Time
	FailedTasks []TaskRecord
}

// SetNow overrides the time provider used by Store (helpful in tests).
func (s *Store) SetNow(now func() time.Time) {
	s.now = now
}

// GetSourceByName returns a source by its unique name or ErrNotFound.
func (s *Store) GetSourceByName(ctx context.Context, name string) (domain.Source, error) {
	var out domain.Source
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT id, kind, name, config_json, credential_env, enabled FROM sources WHERE name = ?`, name).
		Scan(&out.ID, &out.Kind, &out.Name, &out.ConfigJSON, &out.CredentialEnv, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Source{}, ErrNotFound
	}
	if err != nil {
		return domain.Source{}, fmt.Errorf("get source by name: %w", err)
	}
	out.Enabled = enabled != 0
	return out, nil
}

// GetSource returns a source by ID or ErrNotFound.
func (s *Store) GetSource(ctx context.Context, id int64) (domain.Source, error) {
	var out domain.Source
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT id, kind, name, config_json, credential_env, enabled FROM sources WHERE id = ?`, id).
		Scan(&out.ID, &out.Kind, &out.Name, &out.ConfigJSON, &out.CredentialEnv, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Source{}, ErrNotFound
	}
	if err != nil {
		return domain.Source{}, fmt.Errorf("get source: %w", err)
	}
	out.Enabled = enabled != 0
	return out, nil
}

// ListSources returns all sources ordered by ID.
func (s *Store) ListSources(ctx context.Context) ([]domain.Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, name, config_json, credential_env, enabled FROM sources ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()
	var out []domain.Source
	for rows.Next() {
		var src domain.Source
		var enabled int
		if err := rows.Scan(&src.ID, &src.Kind, &src.Name, &src.ConfigJSON, &src.CredentialEnv, &enabled); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		src.Enabled = enabled != 0
		out = append(out, src)
	}
	return out, rows.Err()
}

// ListPlantsBySource returns all plants for a source ordered by name, id.
func (s *Store) ListPlantsBySource(ctx context.Context, sourceID int64) ([]domain.Plant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, source_id, external_id, name, timezone, latitude, longitude, peak_power_kw, weather_location_id
		FROM plants WHERE source_id = ? ORDER BY name, id`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list plants by source: %w", err)
	}
	defer rows.Close()
	var out []domain.Plant
	for rows.Next() {
		var p domain.Plant
		if err := rows.Scan(&p.ID, &p.SourceID, &p.ExternalID, &p.Name, &p.Timezone, &p.Latitude, &p.Longitude, &p.PeakPowerKW, &p.WeatherLocationID); err != nil {
			return nil, fmt.Errorf("scan plant: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPlantByExternalID returns a plant by source ID and external ID.
func (s *Store) GetPlantByExternalID(ctx context.Context, sourceID int64, externalID string) (domain.Plant, error) {
	var p domain.Plant
	err := s.db.QueryRowContext(ctx, `SELECT id, source_id, external_id, name, timezone, latitude, longitude, peak_power_kw, weather_location_id
		FROM plants WHERE source_id = ? AND external_id = ?`, sourceID, externalID).
		Scan(&p.ID, &p.SourceID, &p.ExternalID, &p.Name, &p.Timezone, &p.Latitude, &p.Longitude, &p.PeakPowerKW, &p.WeatherLocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Plant{}, ErrNotFound
	}
	if err != nil {
		return domain.Plant{}, fmt.Errorf("get plant by external id: %w", err)
	}
	return p, nil
}

// GetDeviceByExternalID returns a device by plant ID and external ID.
func (s *Store) GetDeviceByExternalID(ctx context.Context, plantID int64, externalID string) (domain.Device, error) {
	var d domain.Device
	var online *int
	var lastSeen *string
	err := s.db.QueryRowContext(ctx, `SELECT id, plant_id, external_id, name, kind, capacity_kw, panel_area_m2, efficiency, online, last_seen_at
		FROM devices WHERE plant_id = ? AND external_id = ?`, plantID, externalID).
		Scan(&d.ID, &d.PlantID, &d.ExternalID, &d.Name, &d.Kind, &d.CapacityKW, &d.PanelAreaM2, &d.Efficiency, &online, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Device{}, ErrNotFound
	}
	if err != nil {
		return domain.Device{}, fmt.Errorf("get device by external id: %w", err)
	}
	if online != nil {
		v := *online != 0
		d.Online = &v
	}
	if lastSeen != nil {
		t, err := parseTime(*lastSeen)
		if err != nil {
			return domain.Device{}, err
		}
		d.LastSeenAt = &t
	}
	return d, nil
}

// EnqueueTask adds a task to fetch_tasks. If refresh is false, existing tasks are left untouched.
// If refresh is true, done/failed tasks are reset to pending with attempts=0, but already pending
// tasks are never duplicated.
func (s *Store) EnqueueTask(ctx context.Context, sourceID int64, t connector.Task, notBefore time.Time, refresh bool) error {
	taskJSON, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal task json: %w", err)
	}
	nowStr := formatTime(s.now())
	nbStr := formatTime(notBefore)

	if !refresh {
		_, err = s.db.ExecContext(ctx, `INSERT INTO fetch_tasks
			(source_id, task_key, task_json, priority, status, not_before, attempts, last_error, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'pending', ?, 0, '', ?, ?)
			ON CONFLICT (source_id, task_key) DO NOTHING`,
			sourceID, t.LimitKey, string(taskJSON), t.Priority, nbStr, nowStr, nowStr)
		if err != nil {
			return fmt.Errorf("enqueue task: %w", err)
		}
		return nil
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO fetch_tasks
		(source_id, task_key, task_json, priority, status, not_before, attempts, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', ?, 0, '', ?, ?)
		ON CONFLICT (source_id, task_key) DO UPDATE SET
			status = 'pending',
			task_json = excluded.task_json,
			priority = excluded.priority,
			not_before = excluded.not_before,
			attempts = 0,
			last_error = '',
			updated_at = excluded.updated_at
		WHERE fetch_tasks.status != 'pending'`,
		sourceID, t.LimitKey, string(taskJSON), t.Priority, nbStr, nowStr, nowStr)
	if err != nil {
		return fmt.Errorf("enqueue refreshed task: %w", err)
	}
	return nil
}

// PickNextTask selects the highest priority pending task whose not_before is due,
// whose rate limit is not active, and whose daily budget is not exhausted.
// In the same transaction before returning, it records the rate limit and budget usage
// so that restarts cannot spend the limit twice.
func (s *Store) PickNextTask(ctx context.Context, sourceID int64, now time.Time) (*TaskRecord, *connector.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin pick tx: %w", err)
	}
	defer tx.Rollback()

	nowStr := formatTime(now)
	utcDay := now.UTC().Format("2006-01-02")

	query := `SELECT t.id, t.source_id, t.task_key, t.task_json, t.priority, t.status, t.not_before, t.attempts, t.last_error, t.created_at, t.updated_at
		FROM fetch_tasks t
		LEFT JOIN rate_limits rl ON rl.source_id = t.source_id AND rl.limit_key = t.task_key
		LEFT JOIN daily_budgets db ON db.source_id = t.source_id
			AND db.budget_key = json_extract(t.task_json, '$.daily_budget_key')
			AND db.day = ?
		WHERE t.source_id = ?
		  AND t.status = 'pending'
		  AND t.not_before <= ?
		  AND (rl.next_allowed_at IS NULL OR rl.next_allowed_at <= ?)
		  AND (
			  json_extract(t.task_json, '$.daily_budget_key') IS NULL
			  OR json_extract(t.task_json, '$.daily_budget_key') = ''
			  OR json_extract(t.task_json, '$.daily_budget') IS NULL
			  OR CAST(json_extract(t.task_json, '$.daily_budget') AS INTEGER) <= 0
			  OR db.used IS NULL
			  OR db.used < CAST(json_extract(t.task_json, '$.daily_budget') AS INTEGER)
		  )
		ORDER BY t.priority DESC, t.not_before ASC, t.id ASC
		LIMIT 1`

	var rec TaskRecord
	var nbStr, caStr, uaStr string
	err = tx.QueryRowContext(ctx, query, utcDay, sourceID, nowStr, nowStr).
		Scan(&rec.ID, &rec.SourceID, &rec.TaskKey, &rec.TaskJSON, &rec.Priority, &rec.Status,
			&nbStr, &rec.Attempts, &rec.LastError, &caStr, &uaStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("pick next task query: %w", err)
	}

	rec.NotBefore, err = parseTime(nbStr)
	if err != nil {
		return nil, nil, err
	}
	rec.CreatedAt, err = parseTime(caStr)
	if err != nil {
		return nil, nil, err
	}
	rec.UpdatedAt, err = parseTime(uaStr)
	if err != nil {
		return nil, nil, err
	}

	var t connector.Task
	if err := json.Unmarshal([]byte(rec.TaskJSON), &t); err != nil {
		return nil, nil, fmt.Errorf("unmarshal task payload: %w", err)
	}

	// Persist rate limit immediately: now + MinInterval
	nextAllowed := now.Add(t.MinInterval)
	_, err = tx.ExecContext(ctx, `INSERT INTO rate_limits (source_id, limit_key, next_allowed_at)
		VALUES (?, ?, ?)
		ON CONFLICT (source_id, limit_key) DO UPDATE SET next_allowed_at = excluded.next_allowed_at`,
		sourceID, t.LimitKey, formatTime(nextAllowed))
	if err != nil {
		return nil, nil, fmt.Errorf("persist rate limit: %w", err)
	}

	// Persist daily budget if applicable
	if t.DailyBudgetKey != "" && t.DailyBudget > 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO daily_budgets (source_id, budget_key, day, used)
			VALUES (?, ?, ?, 1)
			ON CONFLICT (source_id, budget_key, day) DO UPDATE SET used = daily_budgets.used + 1`,
			sourceID, t.DailyBudgetKey, utcDay)
		if err != nil {
			return nil, nil, fmt.Errorf("increment daily budget: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit pick tx: %w", err)
	}

	return &rec, &t, nil
}

// NextDueTime returns the earliest time when any pending task for the source may become eligible.
// Returns nil if no tasks are pending.
func (s *Store) NextDueTime(ctx context.Context, sourceID int64, now time.Time) (*time.Time, error) {
	utcDay := now.UTC().Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `SELECT t.not_before, rl.next_allowed_at,
		json_extract(t.task_json, '$.daily_budget_key'),
		json_extract(t.task_json, '$.daily_budget'),
		db.used
		FROM fetch_tasks t
		LEFT JOIN rate_limits rl ON rl.source_id = t.source_id AND rl.limit_key = t.task_key
		LEFT JOIN daily_budgets db ON db.source_id = t.source_id
			AND db.budget_key = json_extract(t.task_json, '$.daily_budget_key')
			AND db.day = ?
		WHERE t.source_id = ? AND t.status = 'pending'`,
		utcDay, sourceID)
	if err != nil {
		return nil, fmt.Errorf("query pending tasks: %w", err)
	}
	defer rows.Close()

	var earliest *time.Time
	for rows.Next() {
		var nbStr string
		var rlNext *string
		var budgetKey *string
		var budgetVal *int
		var budgetUsed *int
		if err := rows.Scan(&nbStr, &rlNext, &budgetKey, &budgetVal, &budgetUsed); err != nil {
			return nil, fmt.Errorf("scan pending task: %w", err)
		}
		nb, err := parseTime(nbStr)
		if err != nil {
			return nil, err
		}
		ready := nb
		if rlNext != nil {
			rt, err := parseTime(*rlNext)
			if err != nil {
				return nil, err
			}
			if rt.After(ready) {
				ready = rt
			}
		}
		// If budget is exhausted, task is deferred until next UTC day (00:00:00 UTC)
		if budgetKey != nil && *budgetKey != "" && budgetVal != nil && *budgetVal > 0 && budgetUsed != nil && *budgetUsed >= *budgetVal {
			nextDayUTC := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
			if nextDayUTC.After(ready) {
				ready = nextDayUTC
			}
		}

		if earliest == nil || ready.Before(*earliest) {
			t := ready
			earliest = &t
		}
	}

	return earliest, rows.Err()
}

// MarkTaskDone marks a task as done.
func (s *Store) MarkTaskDone(ctx context.Context, taskID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE fetch_tasks SET status = 'done', updated_at = ? WHERE id = ?`,
		formatTime(now), taskID)
	if err != nil {
		return fmt.Errorf("mark task done: %w", err)
	}
	return nil
}

// MarkTaskRateLimited resets not_before and rate_limits.next_allowed_at to nextAllowed.
// Attempts are unchanged and status stays 'pending'.
func (s *Store) MarkTaskRateLimited(ctx context.Context, sourceID, taskID int64, limitKey string, nextAllowed, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rate limited tx: %w", err)
	}
	defer tx.Rollback()

	nextStr := formatTime(nextAllowed)
	nowStr := formatTime(now)

	_, err = tx.ExecContext(ctx, `UPDATE fetch_tasks SET status = 'pending', not_before = ?, updated_at = ? WHERE id = ?`,
		nextStr, nowStr, taskID)
	if err != nil {
		return fmt.Errorf("update task rate limited: %w", err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO rate_limits (source_id, limit_key, next_allowed_at)
		VALUES (?, ?, ?)
		ON CONFLICT (source_id, limit_key) DO UPDATE SET next_allowed_at = excluded.next_allowed_at`,
		sourceID, limitKey, nextStr)
	if err != nil {
		return fmt.Errorf("update rate limit next allowed: %w", err)
	}

	return tx.Commit()
}

// MarkTaskFailedOrRetry increments attempts, records last_error, and either sets status = 'failed'
// or keeps status = 'pending' with not_before = nextNotBefore.
func (s *Store) MarkTaskFailedOrRetry(ctx context.Context, taskID int64, attempts int, lastErr string, nextNotBefore time.Time, failed bool, now time.Time) error {
	nowStr := formatTime(now)
	if failed {
		_, err := s.db.ExecContext(ctx, `UPDATE fetch_tasks SET status = 'failed', attempts = ?, last_error = ?, updated_at = ? WHERE id = ?`,
			attempts, lastErr, nowStr, taskID)
		if err != nil {
			return fmt.Errorf("mark task failed: %w", err)
		}
		return nil
	}

	nbStr := formatTime(nextNotBefore)
	_, err := s.db.ExecContext(ctx, `UPDATE fetch_tasks SET status = 'pending', attempts = ?, not_before = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		attempts, nbStr, lastErr, nowStr, taskID)
	if err != nil {
		return fmt.Errorf("mark task retry: %w", err)
	}
	return nil
}

// GetQueueStats returns queue counts, earliest next due time, and failed tasks.
func (s *Store) GetQueueStats(ctx context.Context, sourceID int64) (QueueStats, error) {
	var stats QueueStats

	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*) FROM fetch_tasks WHERE source_id = ? GROUP BY status`, sourceID)
	if err != nil {
		return stats, fmt.Errorf("query queue counts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return stats, fmt.Errorf("scan queue counts: %w", err)
		}
		switch status {
		case "pending":
			stats.Pending = count
		case "done":
			stats.Done = count
		case "failed":
			stats.Failed = count
		}
	}

	// Earliest next due time for pending tasks
	var minNb *string
	err = s.db.QueryRowContext(ctx, `SELECT min(not_before) FROM fetch_tasks WHERE source_id = ? AND status = 'pending'`, sourceID).
		Scan(&minNb)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stats, fmt.Errorf("query next due: %w", err)
	}
	if minNb != nil {
		t, err := parseTime(*minNb)
		if err == nil {
			stats.NextDue = &t
		}
	}

	// Failed tasks with last_error
	fRows, err := s.db.QueryContext(ctx, `SELECT id, source_id, task_key, task_json, priority, status, not_before, attempts, last_error, created_at, updated_at
		FROM fetch_tasks WHERE source_id = ? AND status = 'failed' ORDER BY updated_at DESC`, sourceID)
	if err != nil {
		return stats, fmt.Errorf("query failed tasks: %w", err)
	}
	defer fRows.Close()

	for fRows.Next() {
		var rec TaskRecord
		var nbStr, caStr, uaStr string
		if err := fRows.Scan(&rec.ID, &rec.SourceID, &rec.TaskKey, &rec.TaskJSON, &rec.Priority, &rec.Status,
			&nbStr, &rec.Attempts, &rec.LastError, &caStr, &uaStr); err != nil {
			return stats, fmt.Errorf("scan failed task: %w", err)
		}
		if t, err := parseTime(nbStr); err == nil {
			rec.NotBefore = t
		}
		if t, err := parseTime(caStr); err == nil {
			rec.CreatedAt = t
		}
		if t, err := parseTime(uaStr); err == nil {
			rec.UpdatedAt = t
		}
		stats.FailedTasks = append(stats.FailedTasks, rec)
	}

	return stats, nil
}
