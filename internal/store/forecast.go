package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ModelRun represents a row in model_runs.
type ModelRun struct {
	ID          int64
	Name        string
	Version     string
	PlantID     int64
	TrainedAt   time.Time
	TrainStart  *string
	TrainEnd    *string
	TestStart   *string
	TestEnd     *string
	MetricsJSON string
	ParamsJSON  string
	Active      bool
}

// Prediction represents a row in predictions.
type Prediction struct {
	ID          int64
	DeviceID    int64
	Date        string
	ModelRunID  int64
	KWhExpected float64
	KWhLower    *float64
	KWhUpper    *float64
	CreatedAt   time.Time
}

// UpsertModelRun inserts or updates a model run keyed by (name, version, plant_id).
func (s *Store) UpsertModelRun(ctx context.Context, m ModelRun) (ModelRun, error) {
	if m.MetricsJSON == "" {
		m.MetricsJSON = "{}"
	}
	if m.ParamsJSON == "" {
		m.ParamsJSON = "{}"
	}
	activeInt := boolToInt(m.Active)
	trainedAtStr := formatTime(m.TrainedAt)

	err := s.db.QueryRowContext(ctx, `INSERT INTO model_runs
		(name, version, plant_id, trained_at, train_start, train_end, test_start, test_end, metrics_json, params_json, active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name, version, plant_id) DO UPDATE SET
			trained_at = excluded.trained_at,
			train_start = excluded.train_start,
			train_end = excluded.train_end,
			test_start = excluded.test_start,
			test_end = excluded.test_end,
			metrics_json = excluded.metrics_json,
			params_json = excluded.params_json,
			active = excluded.active
		RETURNING id`,
		m.Name, m.Version, m.PlantID, trainedAtStr, m.TrainStart, m.TrainEnd, m.TestStart, m.TestEnd, m.MetricsJSON, m.ParamsJSON, activeInt).
		Scan(&m.ID)
	if err != nil {
		return ModelRun{}, fmt.Errorf("upsert model run: %w", err)
	}
	return m, nil
}

// SetActiveModelRun marks exactly one model run active for a plant, clearing active on all other model runs of that plant.
func (s *Store) SetActiveModelRun(ctx context.Context, plantID, modelRunID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE model_runs SET active = 0 WHERE plant_id = ?`, plantID); err != nil {
		return fmt.Errorf("clear active model runs: %w", err)
	}

	if modelRunID > 0 {
		res, err := tx.ExecContext(ctx, `UPDATE model_runs SET active = 1 WHERE plant_id = ? AND id = ?`, plantID, modelRunID)
		if err != nil {
			return fmt.Errorf("set active model run: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
	}

	return tx.Commit()
}

// GetActiveModelRun returns the active model run for a plant or ErrNotFound.
func (s *Store) GetActiveModelRun(ctx context.Context, plantID int64) (ModelRun, error) {
	var m ModelRun
	var trainedAtStr string
	var activeInt int
	err := s.db.QueryRowContext(ctx, `SELECT id, name, version, plant_id, trained_at, train_start, train_end, test_start, test_end, metrics_json, params_json, active
		FROM model_runs WHERE plant_id = ? AND active = 1`, plantID).
		Scan(&m.ID, &m.Name, &m.Version, &m.PlantID, &trainedAtStr, &m.TrainStart, &m.TrainEnd, &m.TestStart, &m.TestEnd, &m.MetricsJSON, &m.ParamsJSON, &activeInt)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelRun{}, ErrNotFound
	}
	if err != nil {
		return ModelRun{}, fmt.Errorf("get active model run: %w", err)
	}
	m.Active = activeInt != 0
	t, err := parseTime(trainedAtStr)
	if err != nil {
		return ModelRun{}, err
	}
	m.TrainedAt = t
	return m, nil
}

// ListModelRuns returns all model runs for a plant, ordered by ID ascending.
func (s *Store) ListModelRuns(ctx context.Context, plantID int64) ([]ModelRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, version, plant_id, trained_at, train_start, train_end, test_start, test_end, metrics_json, params_json, active
		FROM model_runs WHERE plant_id = ? ORDER BY id ASC`, plantID)
	if err != nil {
		return nil, fmt.Errorf("list model runs: %w", err)
	}
	defer rows.Close()

	var out []ModelRun
	for rows.Next() {
		var m ModelRun
		var trainedAtStr string
		var activeInt int
		if err := rows.Scan(&m.ID, &m.Name, &m.Version, &m.PlantID, &trainedAtStr, &m.TrainStart, &m.TrainEnd, &m.TestStart, &m.TestEnd, &m.MetricsJSON, &m.ParamsJSON, &activeInt); err != nil {
			return nil, fmt.Errorf("scan model run: %w", err)
		}
		m.Active = activeInt != 0
		t, err := parseTime(trainedAtStr)
		if err != nil {
			return nil, err
		}
		m.TrainedAt = t
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpsertPredictions stores predictions in a single transaction.
// Primary key conflict on (device_id, date, model_run_id) updates the prediction values.
func (s *Store) UpsertPredictions(ctx context.Context, preds []Prediction) error {
	if len(preds) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO predictions
		(device_id, date, model_run_id, kwh_expected, kwh_lower, kwh_upper, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_id, date, model_run_id) DO UPDATE SET
			kwh_expected = excluded.kwh_expected,
			kwh_lower = excluded.kwh_lower,
			kwh_upper = excluded.kwh_upper,
			created_at = excluded.created_at`)
	if err != nil {
		return fmt.Errorf("prepare upsert prediction: %w", err)
	}
	defer stmt.Close()

	for _, p := range preds {
		now := p.CreatedAt
		if now.IsZero() {
			now = s.now()
		}
		nowStr := formatTime(now)
		_, err := stmt.ExecContext(ctx, p.DeviceID, p.Date, p.ModelRunID, p.KWhExpected, p.KWhLower, p.KWhUpper, nowStr)
		if err != nil {
			return fmt.Errorf("exec upsert prediction: %w", err)
		}
	}

	return tx.Commit()
}

// ListPredictions returns predictions for a plant within [from, to] inclusive by date.
// If deviceID is non-nil, only predictions for that device are returned.
func (s *Store) ListPredictions(ctx context.Context, plantID int64, deviceID *int64, from, to string) ([]Prediction, error) {
	q := `SELECT p.id, p.device_id, p.date, p.model_run_id, p.kwh_expected, p.kwh_lower, p.kwh_upper, p.created_at
		FROM predictions p
		JOIN devices d ON p.device_id = d.id
		WHERE d.plant_id = ? AND p.date >= ? AND p.date <= ?`
	args := []any{plantID, from, to}
	if deviceID != nil {
		q += " AND p.device_id = ?"
		args = append(args, *deviceID)
	}
	q += " ORDER BY p.date ASC, p.device_id ASC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list predictions: %w", err)
	}
	defer rows.Close()

	var out []Prediction
	for rows.Next() {
		var p Prediction
		var createdAtStr string
		if err := rows.Scan(&p.ID, &p.DeviceID, &p.Date, &p.ModelRunID, &p.KWhExpected, &p.KWhLower, &p.KWhUpper, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan prediction: %w", err)
		}
		t, err := parseTime(createdAtStr)
		if err != nil {
			return nil, err
		}
		p.CreatedAt = t
		out = append(out, p)
	}
	return out, rows.Err()
}
