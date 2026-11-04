package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
)

// ListReadingsForDevice returns all energy readings for a specific device
// within [from, to), ordered by period_start.
func (s *Store) ListReadingsForDevice(ctx context.Context, deviceID int64, from, to time.Time) ([]domain.Reading, error) {
	q := `SELECT id, device_id, metric, period_start, period_end, value, raw_value, quality, quality_reason, raw_payload_id
		FROM readings
		WHERE device_id = ? AND metric = ?`
	args := []any{deviceID, string(domain.MetricEnergyKWh)}
	if !from.IsZero() {
		q += " AND period_start >= ?"
		args = append(args, formatTime(from))
	}
	if !to.IsZero() {
		q += " AND period_start < ?"
		args = append(args, formatTime(to))
	}
	q += " ORDER BY period_start"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list readings for device %d: %w", deviceID, err)
	}
	defer rows.Close()

	var out []domain.Reading
	for rows.Next() {
		var r domain.Reading
		var metric, quality, start, end string
		if err := rows.Scan(&r.ID, &r.DeviceID, &metric, &start, &end, &r.Value, &r.RawValue, &quality, &r.QualityReason, &r.RawPayloadID); err != nil {
			return nil, fmt.Errorf("scan reading: %w", err)
		}
		r.Metric, r.Quality = domain.Metric(metric), domain.Quality(quality)
		if r.PeriodStart, err = parseTime(start); err != nil {
			return nil, err
		}
		if r.PeriodEnd, err = parseTime(end); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListActivePredictionsInRange returns predictions for the active model run of plantID
// within [fromDate, toDate] inclusive.
func (s *Store) ListActivePredictionsInRange(ctx context.Context, plantID int64, fromDate, toDate string) ([]Prediction, error) {
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

	q := `SELECT p.id, p.device_id, p.date, p.model_run_id, p.kwh_expected, p.kwh_lower, p.kwh_upper, p.created_at
		FROM predictions p
		WHERE p.model_run_id = ?`
	args := []any{modelRunID}

	if fromDate != "" {
		q += " AND p.date >= ?"
		args = append(args, fromDate)
	}
	if toDate != "" {
		q += " AND p.date <= ?"
		args = append(args, toDate)
	}
	q += " ORDER BY p.date, p.device_id"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list predictions: %w", err)
	}
	defer rows.Close()

	var out []Prediction
	for rows.Next() {
		var pred Prediction
		var createdAt string
		if err := rows.Scan(&pred.ID, &pred.DeviceID, &pred.Date, &pred.ModelRunID, &pred.KWhExpected, &pred.KWhLower, &pred.KWhUpper, &createdAt); err != nil {
			return nil, fmt.Errorf("scan prediction: %w", err)
		}
		t, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		pred.CreatedAt = t
		out = append(out, pred)
	}
	return out, rows.Err()
}

// GetActivePredictionsMap returns a lookup map[deviceID]map[date]kwh_expected
// for the active model run of plantID within [fromDate, toDate] inclusive.
func (s *Store) GetActivePredictionsMap(ctx context.Context, plantID int64, fromDate, toDate string) (map[int64]map[string]float64, error) {
	preds, err := s.ListActivePredictionsInRange(ctx, plantID, fromDate, toDate)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]map[string]float64)
	for _, p := range preds {
		if out[p.DeviceID] == nil {
			out[p.DeviceID] = make(map[string]float64)
		}
		out[p.DeviceID][p.Date] = p.KWhExpected
	}
	return out, nil
}
