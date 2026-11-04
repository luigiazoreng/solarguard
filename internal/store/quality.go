package store

import (
	"context"
	"fmt"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
)

// QualityUpdate holds a reading ID and its new quality classification.
type QualityUpdate struct {
	ReadingID     int64
	Quality       domain.Quality
	QualityReason string
}

// ListReadingsByPlant returns all energy readings for every device of the given plant
// within [from, to), ordered by period start and device ID.
func (s *Store) ListReadingsByPlant(ctx context.Context, plantID int64, from, to time.Time) ([]domain.Reading, error) {
	q := `SELECT r.id, r.device_id, r.metric, r.period_start, r.period_end, r.value, r.raw_value, r.quality, r.quality_reason, r.raw_payload_id
		FROM readings r
		JOIN devices d ON d.id = r.device_id
		WHERE d.plant_id = ? AND r.metric = ?`
	args := []any{plantID, string(domain.MetricEnergyKWh)}
	if !from.IsZero() {
		q += " AND r.period_start >= ?"
		args = append(args, formatTime(from))
	}
	if !to.IsZero() {
		q += " AND r.period_start < ?"
		args = append(args, formatTime(to))
	}
	q += " ORDER BY r.period_start, d.id"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list readings by plant: %w", err)
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

// UpdateReadingQualities applies quality and quality_reason updates in a single transaction.
func (s *Store) UpdateReadingQualities(ctx context.Context, updates []QualityUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE readings SET quality = ?, quality_reason = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	defer stmt.Close()
	for _, u := range updates {
		if _, err := stmt.ExecContext(ctx, string(u.Quality), u.QualityReason, u.ReadingID); err != nil {
			return fmt.Errorf("exec update reading %d: %w", u.ReadingID, err)
		}
	}
	return tx.Commit()
}
