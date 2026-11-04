package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Report is a stored report. PeriodStart and PeriodEnd are local dates.
type Report struct {
	ID          int64
	PlantID     *int64
	Kind        string
	PeriodStart string
	PeriodEnd   string
	Status      string // draft or final
	Author      string // system, ai or user
	Title       string
	BodyMD      string
	DataJSON    string
	CreatedAt   time.Time
}

// ReportFilter selects reports. Zero values match everything.
type ReportFilter struct {
	PlantID *int64
	Status  string
	Author  string
	Limit   int
	Offset  int
}

const reportColumns = `id, plant_id, kind, period_start, period_end, status, author, title, body_md, data_json, created_at`

func scanReport(scan func(...any) error) (Report, error) {
	var r Report
	var createdAt string
	if err := scan(&r.ID, &r.PlantID, &r.Kind, &r.PeriodStart, &r.PeriodEnd, &r.Status, &r.Author, &r.Title, &r.BodyMD, &r.DataJSON, &createdAt); err != nil {
		return Report{}, err
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return Report{}, err
	}
	r.CreatedAt = t
	return r, nil
}

// InsertReport stores a new report and returns it with its ID.
func (s *Store) InsertReport(ctx context.Context, r Report) (Report, error) {
	if r.DataJSON == "" {
		r.DataJSON = "{}"
	}
	if r.Status == "" {
		r.Status = "draft"
	}
	r.CreatedAt = s.now().UTC().Truncate(time.Second)
	err := s.db.QueryRowContext(ctx, `INSERT INTO reports
		(plant_id, kind, period_start, period_end, status, author, title, body_md, data_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		r.PlantID, r.Kind, r.PeriodStart, r.PeriodEnd, r.Status, r.Author, r.Title, r.BodyMD, r.DataJSON, formatTime(r.CreatedAt)).Scan(&r.ID)
	if err != nil {
		return Report{}, fmt.Errorf("insert report: %w", err)
	}
	return r, nil
}

// GetReport returns one report or ErrNotFound.
func (s *Store) GetReport(ctx context.Context, id int64) (Report, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+reportColumns+` FROM reports WHERE id = ?`, id)
	r, err := scanReport(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, ErrNotFound
	}
	if err != nil {
		return Report{}, fmt.Errorf("get report: %w", err)
	}
	return r, nil
}

// ListReports returns matching reports, newest first.
func (s *Store) ListReports(ctx context.Context, f ReportFilter) ([]Report, error) {
	q := `SELECT ` + reportColumns + ` FROM reports WHERE 1=1`
	var args []any
	if f.PlantID != nil {
		q += " AND plant_id = ?"
		args = append(args, *f.PlantID)
	}
	if f.Status != "" {
		q += " AND status = ?"
		args = append(args, f.Status)
	}
	if f.Author != "" {
		q += " AND author = ?"
		args = append(args, f.Author)
	}
	q += " ORDER BY created_at DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, f.Limit, f.Offset)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		r, err := scanReport(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan report: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FinalizeReport marks a draft as final. Only a person may do this; it is not
// exposed to the MCP server.
func (s *Store) FinalizeReport(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE reports SET status = 'final' WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("finalize report: %w", err)
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
