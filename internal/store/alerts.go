package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Alert represents an incident detected by deterministic rules.
type Alert struct {
	ID           int64     `json:"id"`
	PlantID      int64     `json:"plant_id"`
	DeviceID     *int64    `json:"device_id"`
	Rule         string    `json:"rule"`
	Severity     string    `json:"severity"` // 'info', 'warning', 'critical'
	Status       string    `json:"status"`   // 'open', 'ack', 'resolved'
	DedupeKey    string    `json:"dedupe_key"`
	Title        string    `json:"title"`
	EvidenceJSON string    `json:"evidence_json"`
	FirstDate    string    `json:"first_date"` // YYYY-MM-DD
	LastDate     string    `json:"last_date"`  // YYYY-MM-DD
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Note         string    `json:"note"`
}

// AlertFilter defines criteria for filtering alerts in ListAlerts.
type AlertFilter struct {
	PlantID  *int64
	DeviceID *int64
	Status   string
	Severity string
	Rule     string
	FromDate string // YYYY-MM-DD
	ToDate   string // YYYY-MM-DD
	Limit    int
	Offset   int
}

// RuleConfig holds configuration overrides for an alert rule.
type RuleConfig struct {
	Rule       string
	PlantID    *int64
	Enabled    bool
	ParamsJSON string
}

// Prediction holds expected generation for a device on a local date.
type Prediction struct {
	DeviceID    int64
	Date        string
	KWHExpected float64
}

// ListAlerts returns alerts matching the filter, ordered by first_date DESC, id DESC.
func (s *Store) ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error) {
	q := `SELECT id, plant_id, device_id, rule, severity, status, dedupe_key, title,
		evidence_json, first_date, last_date, created_at, updated_at, note
		FROM alerts WHERE 1=1`
	var args []any

	if f.PlantID != nil {
		q += " AND plant_id = ?"
		args = append(args, *f.PlantID)
	}
	if f.DeviceID != nil {
		q += " AND device_id = ?"
		args = append(args, *f.DeviceID)
	}
	if f.Status != "" {
		q += " AND status = ?"
		args = append(args, f.Status)
	}
	if f.Severity != "" {
		q += " AND severity = ?"
		args = append(args, f.Severity)
	}
	if f.Rule != "" {
		q += " AND rule = ?"
		args = append(args, f.Rule)
	}
	if f.FromDate != "" {
		q += " AND last_date >= ?"
		args = append(args, f.FromDate)
	}
	if f.ToDate != "" {
		q += " AND first_date <= ?"
		args = append(args, f.ToDate)
	}

	q += " ORDER BY first_date DESC, id DESC"

	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
		if f.Offset > 0 {
			q += " OFFSET ?"
			args = append(args, f.Offset)
		}
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer rows.Close()

	var out []Alert
	for rows.Next() {
		var a Alert
		var devID sql.NullInt64
		var createdAt, updatedAt string
		if err := rows.Scan(&a.ID, &a.PlantID, &devID, &a.Rule, &a.Severity, &a.Status,
			&a.DedupeKey, &a.Title, &a.EvidenceJSON, &a.FirstDate, &a.LastDate,
			&createdAt, &updatedAt, &a.Note); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		if devID.Valid {
			v := devID.Int64
			a.DeviceID = &v
		}
		tCreated, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		a.CreatedAt = tCreated
		tUpdated, err := parseTime(updatedAt)
		if err != nil {
			return nil, err
		}
		a.UpdatedAt = tUpdated

		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAlert returns a single alert by ID or ErrNotFound.
func (s *Store) GetAlert(ctx context.Context, id int64) (Alert, error) {
	var a Alert
	var devID sql.NullInt64
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `SELECT id, plant_id, device_id, rule, severity, status, dedupe_key,
		title, evidence_json, first_date, last_date, created_at, updated_at, note
		FROM alerts WHERE id = ?`, id).
		Scan(&a.ID, &a.PlantID, &devID, &a.Rule, &a.Severity, &a.Status, &a.DedupeKey,
			&a.Title, &a.EvidenceJSON, &a.FirstDate, &a.LastDate, &createdAt, &updatedAt, &a.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, ErrNotFound
	}
	if err != nil {
		return Alert{}, fmt.Errorf("get alert %d: %w", id, err)
	}
	if devID.Valid {
		v := devID.Int64
		a.DeviceID = &v
	}
	tCreated, err := parseTime(createdAt)
	if err != nil {
		return Alert{}, err
	}
	a.CreatedAt = tCreated
	tUpdated, err := parseTime(updatedAt)
	if err != nil {
		return Alert{}, err
	}
	a.UpdatedAt = tUpdated

	return a, nil
}

func (s *Store) setAlertStatus(ctx context.Context, id int64, targetStatus, note string) error {
	var currentID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM alerts WHERE id = ?`, id).Scan(&currentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check alert %d: %w", id, err)
	}

	now := formatTime(s.now())
	_, err = s.db.ExecContext(ctx, `UPDATE alerts SET
		status = ?,
		note = COALESCE(NULLIF(?, ''), note),
		updated_at = ?
		WHERE id = ?`, targetStatus, note, now, id)
	if err != nil {
		return fmt.Errorf("update alert status %d: %w", id, err)
	}
	return nil
}

// AckAlert acknowledges an alert, optionally setting a note.
func (s *Store) AckAlert(ctx context.Context, id int64, note string) error {
	return s.setAlertStatus(ctx, id, "ack", note)
}

// ResolveAlert marks an alert as resolved, optionally setting a note.
func (s *Store) ResolveAlert(ctx context.Context, id int64, note string) error {
	return s.setAlertStatus(ctx, id, "resolved", note)
}

// ReopenAlert reopens an alert, optionally setting a note.
func (s *Store) ReopenAlert(ctx context.Context, id int64, note string) error {
	return s.setAlertStatus(ctx, id, "open", note)
}

// CountOpenAlertsByPlant returns the number of open alerts grouped by plant ID.
func (s *Store) CountOpenAlertsByPlant(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT plant_id, COUNT(*) FROM alerts WHERE status = 'open' GROUP BY plant_id`)
	if err != nil {
		return nil, fmt.Errorf("count open alerts by plant: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]int)
	for rows.Next() {
		var plantID int64
		var count int
		if err := rows.Scan(&plantID, &count); err != nil {
			return nil, fmt.Errorf("scan plant alert count: %w", err)
		}
		out[plantID] = count
	}
	return out, rows.Err()
}

// ListAlertsByPlant returns all alerts for a specific plant.
func (s *Store) ListAlertsByPlant(ctx context.Context, plantID int64) ([]Alert, error) {
	return s.ListAlerts(ctx, AlertFilter{PlantID: &plantID})
}

// SaveAlert inserts a new alert or updates an existing one by ID.
func (s *Store) SaveAlert(ctx context.Context, a *Alert) error {
	now := s.now()
	if a.ID == 0 {
		if a.CreatedAt.IsZero() {
			a.CreatedAt = now
		}
		if a.UpdatedAt.IsZero() {
			a.UpdatedAt = now
		}
		if a.Status == "" {
			a.Status = "open"
		}
		var devID any = nil
		if a.DeviceID != nil {
			devID = *a.DeviceID
		}
		err := s.db.QueryRowContext(ctx, `INSERT INTO alerts
			(plant_id, device_id, rule, severity, status, dedupe_key, title, evidence_json, first_date, last_date, created_at, updated_at, note)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id`,
			a.PlantID, devID, a.Rule, a.Severity, a.Status, a.DedupeKey, a.Title, a.EvidenceJSON,
			a.FirstDate, a.LastDate, formatTime(a.CreatedAt), formatTime(a.UpdatedAt), a.Note).Scan(&a.ID)
		if err != nil {
			return fmt.Errorf("insert alert: %w", err)
		}
		return nil
	}

	a.UpdatedAt = now
	var devID any = nil
	if a.DeviceID != nil {
		devID = *a.DeviceID
	}
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET
		plant_id = ?, device_id = ?, rule = ?, severity = ?, status = ?, dedupe_key = ?,
		title = ?, evidence_json = ?, first_date = ?, last_date = ?, updated_at = ?, note = ?
		WHERE id = ?`,
		a.PlantID, devID, a.Rule, a.Severity, a.Status, a.DedupeKey,
		a.Title, a.EvidenceJSON, a.FirstDate, a.LastDate, formatTime(a.UpdatedAt), a.Note, a.ID)
	if err != nil {
		return fmt.Errorf("update alert %d: %w", a.ID, err)
	}
	return nil
}

// GetRuleConfigs returns all rule configs applicable to plantID, with plant-specific
// overrides taking precedence over global defaults (where plant_id IS NULL).
func (s *Store) GetRuleConfigs(ctx context.Context, plantID int64) (map[string]RuleConfig, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rule, plant_id, enabled, params_json
		FROM alert_rule_config
		WHERE plant_id = ? OR plant_id IS NULL
		ORDER BY (CASE WHEN plant_id IS NULL THEN 0 ELSE 1 END) ASC`, plantID)
	if err != nil {
		return nil, fmt.Errorf("get rule configs: %w", err)
	}
	defer rows.Close()

	out := make(map[string]RuleConfig)
	for rows.Next() {
		var cfg RuleConfig
		var pID sql.NullInt64
		var enabled int
		if err := rows.Scan(&cfg.Rule, &pID, &enabled, &cfg.ParamsJSON); err != nil {
			return nil, fmt.Errorf("scan rule config: %w", err)
		}
		if pID.Valid {
			v := pID.Int64
			cfg.PlantID = &v
		}
		cfg.Enabled = enabled != 0
		out[cfg.Rule] = cfg
	}
	return out, rows.Err()
}

// SetRuleConfig inserts or updates a rule configuration.
func (s *Store) SetRuleConfig(ctx context.Context, cfg RuleConfig) error {
	var pID any = nil
	if cfg.PlantID != nil {
		pID = *cfg.PlantID
	}
	enabledInt := 0
	if cfg.Enabled {
		enabledInt = 1
	}
	if cfg.ParamsJSON == "" {
		cfg.ParamsJSON = "{}"
	}

	_, err := s.db.ExecContext(ctx, `INSERT INTO alert_rule_config
		(rule, plant_id, enabled, params_json)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (rule, plant_id) DO UPDATE SET
			enabled = excluded.enabled,
			params_json = excluded.params_json`,
		cfg.Rule, pID, enabledInt, cfg.ParamsJSON)
	if err != nil {
		return fmt.Errorf("set rule config: %w", err)
	}
	return nil
}

// ListActivePredictions returns predictions for the active model run of plantID.
// If there is no active model run with predictions, it returns an empty slice.
func (s *Store) ListActivePredictions(ctx context.Context, plantID int64) ([]Prediction, error) {
	var modelRunID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM model_runs
		WHERE active = 1 AND (plant_id = ? OR plant_id IS NULL)
		ORDER BY (CASE WHEN plant_id = ? THEN 0 ELSE 1 END), id DESC
		LIMIT 1`, plantID, plantID).Scan(&modelRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find active model run for plant %d: %w", plantID, err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT p.device_id, p.date, p.kwh_expected
		FROM predictions p
		WHERE p.model_run_id = ?
		ORDER BY p.date, p.device_id`, modelRunID)
	if err != nil {
		return nil, fmt.Errorf("list predictions: %w", err)
	}
	defer rows.Close()

	var out []Prediction
	for rows.Next() {
		var pred Prediction
		if err := rows.Scan(&pred.DeviceID, &pred.Date, &pred.KWHExpected); err != nil {
			return nil, fmt.Errorf("scan prediction: %w", err)
		}
		out = append(out, pred)
	}
	return out, rows.Err()
}
