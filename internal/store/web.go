package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
)

// PipelineRun represents a row in pipeline_runs.
type PipelineRun struct {
	ID         int64     `json:"id"`
	PlantID    int64     `json:"plant_id"`
	LocalDate  string    `json:"local_date"` // YYYY-MM-DD
	Status     string    `json:"status"`     // success, partial, failed
	StepsJSON  string    `json:"steps_json"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// RecordPipelineRun inserts a record of a pipeline run.
func (s *Store) RecordPipelineRun(ctx context.Context, r *PipelineRun) error {
	now := s.now().UTC()
	if r.StartedAt.IsZero() {
		r.StartedAt = now
	}
	if r.FinishedAt.IsZero() {
		r.FinishedAt = now
	}
	if r.StepsJSON == "" {
		r.StepsJSON = "[]"
	}

	err := s.db.QueryRowContext(ctx, `INSERT INTO pipeline_runs
		(plant_id, local_date, status, steps_json, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		r.PlantID, r.LocalDate, r.Status, r.StepsJSON,
		formatTime(r.StartedAt), formatTime(r.FinishedAt)).Scan(&r.ID)
	if err != nil {
		return fmt.Errorf("record pipeline run: %w", err)
	}
	return nil
}

// HasPipelineRun returns true if plantID has already run for the given local date.
func (s *Store) HasPipelineRun(ctx context.Context, plantID int64, localDate string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pipeline_runs WHERE plant_id = ? AND local_date = ?`, plantID, localDate).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check pipeline run: %w", err)
	}
	return count > 0, nil
}

// ListPipelineRuns returns the most recent pipeline runs.
func (s *Store) ListPipelineRuns(ctx context.Context, plantID *int64, limit int) ([]PipelineRun, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT id, plant_id, local_date, status, steps_json, started_at, finished_at FROM pipeline_runs`
	var args []any
	if plantID != nil {
		q += ` WHERE plant_id = ?`
		args = append(args, *plantID)
	}
	q += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list pipeline runs: %w", err)
	}
	defer rows.Close()

	var out []PipelineRun
	for rows.Next() {
		var r PipelineRun
		var startedAtStr, finishedAtStr string
		if err := rows.Scan(&r.ID, &r.PlantID, &r.LocalDate, &r.Status, &r.StepsJSON, &startedAtStr, &finishedAtStr); err != nil {
			return nil, fmt.Errorf("scan pipeline run: %w", err)
		}
		if t, err := parseTime(startedAtStr); err == nil {
			r.StartedAt = t
		}
		if t, err := parseTime(finishedAtStr); err == nil {
			r.FinishedAt = t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetLatestPipelineRun returns the newest pipeline run for a plant or nil if none.
func (s *Store) GetLatestPipelineRun(ctx context.Context, plantID int64) (*PipelineRun, error) {
	runs, err := s.ListPipelineRuns(ctx, &plantID, 1)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return &runs[0], nil
}

// GetPlantNewestWeatherDate returns the latest date in weather_days for a plant and kind, or "" if none.
func (s *Store) GetPlantNewestWeatherDate(ctx context.Context, plantID int64, kind string) (string, error) {
	var maxDate sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MAX(wd.date)
		FROM weather_days wd
		JOIN plants p ON wd.location_id = p.weather_location_id
		WHERE p.id = ? AND wd.kind = ?`, plantID, kind).Scan(&maxDate)
	if errors.Is(err, sql.ErrNoRows) || !maxDate.Valid {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get plant newest weather date: %w", err)
	}
	return maxDate.String, nil
}

// GetPlantFirstReadingDate returns the earliest local date of readings for a plant, or "" if none.
func (s *Store) GetPlantFirstReadingDate(ctx context.Context, plantID int64) (string, error) {
	plant, err := s.GetPlant(ctx, plantID)
	if err != nil {
		return "", err
	}
	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		return "", fmt.Errorf("load plant timezone: %w", err)
	}

	var minStart sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT MIN(r.period_start)
		FROM readings r
		JOIN devices d ON r.device_id = d.id
		WHERE d.plant_id = ?`, plantID).Scan(&minStart)
	if errors.Is(err, sql.ErrNoRows) || !minStart.Valid {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get plant first reading date: %w", err)
	}
	t, err := parseTime(minStart.String)
	if err != nil {
		return "", err
	}
	return t.In(loc).Format(time.DateOnly), nil
}

// GetDevice returns a device by its ID or ErrNotFound.
func (s *Store) GetDevice(ctx context.Context, id int64) (domain.Device, error) {
	var d domain.Device
	var online *int
	var lastSeen *string
	err := s.db.QueryRowContext(ctx, `SELECT id, plant_id, external_id, name, kind, capacity_kw, panel_area_m2, efficiency, online, last_seen_at
		FROM devices WHERE id = ?`, id).
		Scan(&d.ID, &d.PlantID, &d.ExternalID, &d.Name, &d.Kind, &d.CapacityKW, &d.PanelAreaM2, &d.Efficiency, &online, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Device{}, ErrNotFound
	}
	if err != nil {
		return domain.Device{}, fmt.Errorf("get device %d: %w", id, err)
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

// UpdateDevice updates the rating/area specifications of a device.
func (s *Store) UpdateDevice(ctx context.Context, id int64, capacityKW, panelAreaM2, efficiency *float64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET
		capacity_kw = ?,
		panel_area_m2 = ?,
		efficiency = ?
		WHERE id = ?`, capacityKW, panelAreaM2, efficiency, id)
	if err != nil {
		return fmt.Errorf("update device %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdatePlantDetails updates editable plant settings: timezone and coordinates.
func (s *Store) UpdatePlantDetails(ctx context.Context, id int64, timezone string, lat, lon *float64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE plants SET
		timezone = ?,
		latitude = ?,
		longitude = ?
		WHERE id = ?`, timezone, lat, lon, id)
	if err != nil {
		return fmt.Errorf("update plant details %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeviceQualitySummary summarizes reading quality and last seen for a device.
type DeviceQualitySummary struct {
	Device        domain.Device
	TotalReadings int
	OKReadings    int
	LastReadingAt *time.Time
}

// GetPlantDeviceSummaries returns quality statistics and last reading for each device in a plant.
func (s *Store) GetPlantDeviceSummaries(ctx context.Context, plantID int64) ([]DeviceQualitySummary, error) {
	devices, err := s.ListDevices(ctx, plantID)
	if err != nil {
		return nil, err
	}

	out := make([]DeviceQualitySummary, 0, len(devices))
	for _, d := range devices {
		var total, okCount int
		var maxPeriodEnd sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
			SUM(CASE WHEN quality = 'ok' THEN 1 ELSE 0 END),
			MAX(period_end)
			FROM readings WHERE device_id = ?`, d.ID).Scan(&total, &okCount, &maxPeriodEnd)
		if err != nil {
			return nil, fmt.Errorf("device summary %d: %w", d.ID, err)
		}

		var lastReading *time.Time
		if maxPeriodEnd.Valid {
			if t, err := parseTime(maxPeriodEnd.String); err == nil {
				lastReading = &t
			}
		}

		out = append(out, DeviceQualitySummary{
			Device:        d,
			TotalReadings: total,
			OKReadings:    okCount,
			LastReadingAt: lastReading,
		})
	}
	return out, nil
}

// PlantYesterdayStats holds yesterday's generation and expectation for a plant.
type PlantYesterdayStats struct {
	YesterdayDate string
	MeasuredKWh   float64
	ExpectedKWh   float64
	HasReadings   bool
	HasExpected   bool
	LastReadingAt *time.Time
}

// GetPlantYesterdayStats computes generation and expected values for local yesterday.
func (s *Store) GetPlantYesterdayStats(ctx context.Context, p domain.Plant, now time.Time) (PlantYesterdayStats, error) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		loc = time.UTC
	}
	yesterdayStr := now.In(loc).AddDate(0, 0, -1).Format(time.DateOnly)

	yStart, yEnd, err := domain.LocalDay(yesterdayStr, p.Timezone)
	if err != nil {
		return PlantYesterdayStats{}, err
	}

	var stats PlantYesterdayStats
	stats.YesterdayDate = yesterdayStr

	// Sum measured kWh for local yesterday across all devices
	var sumMeasured sql.NullFloat64
	var countMeasured int
	err = s.db.QueryRowContext(ctx, `SELECT SUM(r.value), COUNT(r.value)
		FROM readings r
		JOIN devices d ON r.device_id = d.id
		WHERE d.plant_id = ? AND r.metric = 'energy_kwh'
		  AND r.period_start >= ? AND r.period_end <= ?`,
		p.ID, formatTime(yStart), formatTime(yEnd)).Scan(&sumMeasured, &countMeasured)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stats, fmt.Errorf("sum yesterday measured: %w", err)
	}
	if sumMeasured.Valid && countMeasured > 0 {
		stats.MeasuredKWh = sumMeasured.Float64
		stats.HasReadings = true
	}

	// Sum expected kWh from predictions for yesterday
	var sumExpected sql.NullFloat64
	err = s.db.QueryRowContext(ctx, `SELECT SUM(p.kwh_expected)
		FROM predictions p
		JOIN devices d ON p.device_id = d.id
		JOIN model_runs mr ON p.model_run_id = mr.id
		WHERE d.plant_id = ? AND p.date = ? AND mr.active = 1`,
		p.ID, yesterdayStr).Scan(&sumExpected)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stats, fmt.Errorf("sum yesterday expected: %w", err)
	}
	if sumExpected.Valid {
		stats.ExpectedKWh = sumExpected.Float64
		stats.HasExpected = true
	}

	// Last reading timestamp
	var maxEnd sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT MAX(r.period_end)
		FROM readings r
		JOIN devices d ON r.device_id = d.id
		WHERE d.plant_id = ?`, p.ID).Scan(&maxEnd)
	if err == nil && maxEnd.Valid {
		if t, err := parseTime(maxEnd.String); err == nil {
			stats.LastReadingAt = &t
		}
	}

	return stats, nil
}
