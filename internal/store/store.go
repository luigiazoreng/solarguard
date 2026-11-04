// Package store persists SolarGuard data in SQLite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/luigiazoreng/solarguard/internal/domain"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// ErrNotFound reports that a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the database handle. It is safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens the database at path, creates parent directories, and applies
// pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle to packages that own their own queries.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.applyMigration(ctx, name); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, name string) error {
	body, err := migrationFiles.ReadFile(name)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&applied); err != nil {
		return err
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, formatTime(s.now())); err != nil {
		return err
	}
	return tx.Commit()
}

// EnsureSource creates the source if its name is new, and returns it.
func (s *Store) EnsureSource(ctx context.Context, src domain.Source) (domain.Source, error) {
	if src.ConfigJSON == "" {
		src.ConfigJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sources (kind, name, config_json, credential_env, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (name) DO NOTHING`,
		src.Kind, src.Name, src.ConfigJSON, src.CredentialEnv, boolToInt(src.Enabled), formatTime(s.now()))
	if err != nil {
		return domain.Source{}, fmt.Errorf("ensure source: %w", err)
	}
	var out domain.Source
	var enabled int
	err = s.db.QueryRowContext(ctx, `SELECT id, kind, name, config_json, credential_env, enabled FROM sources WHERE name = ?`, src.Name).
		Scan(&out.ID, &out.Kind, &out.Name, &out.ConfigJSON, &out.CredentialEnv, &enabled)
	if err != nil {
		return domain.Source{}, fmt.Errorf("read source: %w", err)
	}
	out.Enabled = enabled != 0
	return out, nil
}

// UpsertPlant creates or refreshes a plant, keyed by source and external ID.
// A nil field never erases a value already stored.
func (s *Store) UpsertPlant(ctx context.Context, p domain.Plant) (domain.Plant, error) {
	err := s.db.QueryRowContext(ctx, `INSERT INTO plants
		(source_id, external_id, name, timezone, latitude, longitude, peak_power_kw, weather_location_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_id, external_id) DO UPDATE SET
			name = excluded.name,
			timezone = excluded.timezone,
			latitude = COALESCE(excluded.latitude, latitude),
			longitude = COALESCE(excluded.longitude, longitude),
			peak_power_kw = COALESCE(excluded.peak_power_kw, peak_power_kw),
			weather_location_id = COALESCE(excluded.weather_location_id, weather_location_id)
		RETURNING id`,
		p.SourceID, p.ExternalID, p.Name, p.Timezone, p.Latitude, p.Longitude, p.PeakPowerKW, p.WeatherLocationID, formatTime(s.now())).Scan(&p.ID)
	if err != nil {
		return domain.Plant{}, fmt.Errorf("upsert plant: %w", err)
	}
	return p, nil
}

// UpsertDevice creates or refreshes a device, keyed by plant and external ID.
// A nil field never erases a value already stored.
func (s *Store) UpsertDevice(ctx context.Context, d domain.Device) (domain.Device, error) {
	if d.Kind == "" {
		d.Kind = "inverter"
	}
	var lastSeen *string
	if d.LastSeenAt != nil {
		v := formatTime(*d.LastSeenAt)
		lastSeen = &v
	}
	var online *int
	if d.Online != nil {
		v := boolToInt(*d.Online)
		online = &v
	}
	err := s.db.QueryRowContext(ctx, `INSERT INTO devices
		(plant_id, external_id, name, kind, capacity_kw, panel_area_m2, efficiency, online, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (plant_id, external_id) DO UPDATE SET
			name = excluded.name,
			kind = excluded.kind,
			capacity_kw = COALESCE(excluded.capacity_kw, capacity_kw),
			panel_area_m2 = COALESCE(excluded.panel_area_m2, panel_area_m2),
			efficiency = COALESCE(excluded.efficiency, efficiency),
			online = COALESCE(excluded.online, online),
			last_seen_at = COALESCE(excluded.last_seen_at, last_seen_at)
		RETURNING id`,
		d.PlantID, d.ExternalID, d.Name, d.Kind, d.CapacityKW, d.PanelAreaM2, d.Efficiency, online, lastSeen).Scan(&d.ID)
	if err != nil {
		return domain.Device{}, fmt.Errorf("upsert device: %w", err)
	}
	return d, nil
}

// ListPlants returns every plant, ordered by name.
func (s *Store) ListPlants(ctx context.Context) ([]domain.Plant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, source_id, external_id, name, timezone, latitude, longitude, peak_power_kw, weather_location_id
		FROM plants ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("list plants: %w", err)
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

// GetPlant returns one plant or ErrNotFound.
func (s *Store) GetPlant(ctx context.Context, id int64) (domain.Plant, error) {
	var p domain.Plant
	err := s.db.QueryRowContext(ctx, `SELECT id, source_id, external_id, name, timezone, latitude, longitude, peak_power_kw, weather_location_id
		FROM plants WHERE id = ?`, id).
		Scan(&p.ID, &p.SourceID, &p.ExternalID, &p.Name, &p.Timezone, &p.Latitude, &p.Longitude, &p.PeakPowerKW, &p.WeatherLocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Plant{}, ErrNotFound
	}
	if err != nil {
		return domain.Plant{}, fmt.Errorf("get plant: %w", err)
	}
	return p, nil
}

// ListDevices returns the devices of a plant, ordered by name.
func (s *Store) ListDevices(ctx context.Context, plantID int64) ([]domain.Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, plant_id, external_id, name, kind, capacity_kw, panel_area_m2, efficiency, online, last_seen_at
		FROM devices WHERE plant_id = ? ORDER BY name, id`, plantID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()
	var out []domain.Device
	for rows.Next() {
		var d domain.Device
		var online *int
		var lastSeen *string
		if err := rows.Scan(&d.ID, &d.PlantID, &d.ExternalID, &d.Name, &d.Kind, &d.CapacityKW, &d.PanelAreaM2, &d.Efficiency, &online, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		if online != nil {
			v := *online != 0
			d.Online = &v
		}
		if lastSeen != nil {
			t, err := parseTime(*lastSeen)
			if err != nil {
				return nil, err
			}
			d.LastSeenAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// InsertRawPayload stores a provider response exactly as received.
func (s *Store) InsertRawPayload(ctx context.Context, sourceID int64, endpoint, paramsJSON string, body []byte) (int64, error) {
	if paramsJSON == "" {
		paramsJSON = "{}"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO raw_payloads (source_id, endpoint, params_json, fetched_at, body) VALUES (?, ?, ?, ?, ?)`,
		sourceID, endpoint, paramsJSON, formatTime(s.now()), body)
	if err != nil {
		return 0, fmt.Errorf("insert raw payload: %w", err)
	}
	return res.LastInsertId()
}

// InsertReadings stores readings in one transaction and returns how many were
// new. A reading that already exists is left untouched, so measurements stay
// immutable and repeated imports are harmless.
func (s *Store) InsertReadings(ctx context.Context, readings []domain.Reading) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO readings
		(device_id, metric, period_start, period_end, value, raw_value, quality, quality_reason, raw_payload_id, ingested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_id, metric, period_start, period_end) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()
	inserted := 0
	now := formatTime(s.now())
	for _, r := range readings {
		q := r.Quality
		if q == "" {
			q = domain.QualityUnchecked
		}
		res, err := stmt.ExecContext(ctx, r.DeviceID, string(r.Metric), formatTime(r.PeriodStart), formatTime(r.PeriodEnd),
			r.Value, r.RawValue, string(q), r.QualityReason, r.RawPayloadID, now)
		if err != nil {
			return 0, fmt.Errorf("insert reading: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		inserted += int(n)
	}
	return inserted, tx.Commit()
}

// ReadingFilter selects readings in [From, To) by period start. An empty
// Qualities slice accepts every quality.
type ReadingFilter struct {
	DeviceID  int64
	Metric    domain.Metric
	From, To  time.Time
	Qualities []domain.Quality
}

// ListReadings returns matching readings ordered by period start.
func (s *Store) ListReadings(ctx context.Context, f ReadingFilter) ([]domain.Reading, error) {
	q := `SELECT id, device_id, metric, period_start, period_end, value, raw_value, quality, quality_reason, raw_payload_id
		FROM readings WHERE device_id = ? AND metric = ? AND period_start >= ? AND period_start < ?`
	args := []any{f.DeviceID, string(f.Metric), formatTime(f.From), formatTime(f.To)}
	if len(f.Qualities) > 0 {
		q += " AND quality IN (?"
		args = append(args, string(f.Qualities[0]))
		for _, v := range f.Qualities[1:] {
			q += ", ?"
			args = append(args, string(v))
		}
		q += ")"
	}
	q += " ORDER BY period_start"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list readings: %w", err)
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

// SetReadingQuality classifies a reading. It cannot change the measured value.
func (s *Store) SetReadingQuality(ctx context.Context, id int64, q domain.Quality, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE readings SET quality = ?, quality_reason = ? WHERE id = ?`, string(q), reason, id)
	if err != nil {
		return fmt.Errorf("set reading quality: %w", err)
	}
	return nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time %q: %w", v, err)
	}
	return t, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
