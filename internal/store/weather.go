package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/luigiazoreng/solarguard/internal/connector"
)

// WeatherLocation represents a row in weather_locations.
type WeatherLocation struct {
	ID          int64
	Name        string
	Latitude    *float64
	Longitude   *float64
	Timezone    string
	StationCode string
}

// EnsureWeatherLocation creates the location if its name is new, or returns the existing one.
func (s *Store) EnsureWeatherLocation(ctx context.Context, loc WeatherLocation) (WeatherLocation, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO weather_locations
		(name, latitude, longitude, timezone, station_code)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			latitude = COALESCE(excluded.latitude, latitude),
			longitude = COALESCE(excluded.longitude, longitude),
			timezone = CASE WHEN excluded.timezone != '' THEN excluded.timezone ELSE timezone END,
			station_code = CASE WHEN excluded.station_code != '' THEN excluded.station_code ELSE station_code END`,
		loc.Name, loc.Latitude, loc.Longitude, loc.Timezone, loc.StationCode)
	if err != nil {
		return WeatherLocation{}, fmt.Errorf("ensure weather location: %w", err)
	}

	var out WeatherLocation
	err = s.db.QueryRowContext(ctx, `SELECT id, name, latitude, longitude, timezone, station_code
		FROM weather_locations WHERE name = ?`, loc.Name).
		Scan(&out.ID, &out.Name, &out.Latitude, &out.Longitude, &out.Timezone, &out.StationCode)
	if err != nil {
		return WeatherLocation{}, fmt.Errorf("read weather location: %w", err)
	}
	return out, nil
}

// GetWeatherLocation returns a weather location by its ID or ErrNotFound.
func (s *Store) GetWeatherLocation(ctx context.Context, id int64) (WeatherLocation, error) {
	var loc WeatherLocation
	err := s.db.QueryRowContext(ctx, `SELECT id, name, latitude, longitude, timezone, station_code
		FROM weather_locations WHERE id = ?`, id).
		Scan(&loc.ID, &loc.Name, &loc.Latitude, &loc.Longitude, &loc.Timezone, &loc.StationCode)
	if errors.Is(err, sql.ErrNoRows) {
		return WeatherLocation{}, ErrNotFound
	}
	if err != nil {
		return WeatherLocation{}, fmt.Errorf("get weather location: %w", err)
	}
	return loc, nil
}

// LinkPlantWeather sets the weather_location_id of a plant.
func (s *Store) LinkPlantWeather(ctx context.Context, plantID, locationID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE plants SET weather_location_id = ? WHERE id = ?`, locationID, plantID)
	if err != nil {
		return fmt.Errorf("link plant weather: %w", err)
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

// SetPlantLocation updates a plant's latitude and longitude.
func (s *Store) SetPlantLocation(ctx context.Context, plantID int64, lat, lon float64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE plants SET latitude = ?, longitude = ? WHERE id = ?`, lat, lon, plantID)
	if err != nil {
		return fmt.Errorf("set plant location: %w", err)
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

// UpsertWeatherDays inserts or updates daily weather records.
// The primary key is (location_id, date, kind, provider).
// A newer forecast replaces an older forecast for the same provider and date.
// An observed day is never overwritten by a forecast because kind differs.
func (s *Store) UpsertWeatherDays(ctx context.Context, locationID int64, kind string, provider string, days []connector.WeatherDay) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO weather_days
		(location_id, date, kind, provider, irradiation_kwh_m2, temperature_mean_c, temperature_max_c, precipitation_mm, humidity_mean_pct, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (location_id, date, kind, provider) DO UPDATE SET
			irradiation_kwh_m2 = excluded.irradiation_kwh_m2,
			temperature_mean_c = excluded.temperature_mean_c,
			temperature_max_c = excluded.temperature_max_c,
			precipitation_mm = excluded.precipitation_mm,
			humidity_mean_pct = excluded.humidity_mean_pct,
			fetched_at = excluded.fetched_at`)
	if err != nil {
		return fmt.Errorf("prepare upsert weather day: %w", err)
	}
	defer stmt.Close()

	now := formatTime(s.now())
	for _, d := range days {
		_, err := stmt.ExecContext(ctx, locationID, d.Date, kind, provider,
			d.IrradiationKWhM2, d.TemperatureMeanC, d.TemperatureMaxC, d.PrecipitationMM, d.HumidityMeanPct, now)
		if err != nil {
			return fmt.Errorf("insert weather day %s: %w", d.Date, err)
		}
	}

	return tx.Commit()
}

// ListWeatherDays returns weather days for a location within [from, to] inclusive.
// If kind is non-empty, only days matching kind ('observed' or 'forecast') are returned.
func (s *Store) ListWeatherDays(ctx context.Context, locationID int64, from, to string, kind string) ([]connector.WeatherDay, error) {
	q := `SELECT date, irradiation_kwh_m2, temperature_mean_c, temperature_max_c, precipitation_mm, humidity_mean_pct
		FROM weather_days WHERE location_id = ? AND date >= ? AND date <= ?`
	args := []any{locationID, from, to}
	if kind != "" {
		q += " AND kind = ?"
		args = append(args, kind)
	}
	q += " ORDER BY date ASC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list weather days: %w", err)
	}
	defer rows.Close()

	var out []connector.WeatherDay
	for rows.Next() {
		var d connector.WeatherDay
		if err := rows.Scan(&d.Date, &d.IrradiationKWhM2, &d.TemperatureMeanC, &d.TemperatureMaxC, &d.PrecipitationMM, &d.HumidityMeanPct); err != nil {
			return nil, fmt.Errorf("scan weather day: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PreferredObserved returns one observed row per date in [from, to] by provider preference order.
func (s *Store) PreferredObserved(ctx context.Context, locationID int64, from, to string, providers []string) ([]connector.WeatherDay, error) {
	rank := make(map[string]int, len(providers))
	for i, p := range providers {
		rank[p] = i
	}

	rows, err := s.db.QueryContext(ctx, `SELECT date, provider, irradiation_kwh_m2, temperature_mean_c, temperature_max_c, precipitation_mm, humidity_mean_pct
		FROM weather_days WHERE location_id = ? AND kind = 'observed' AND date >= ? AND date <= ?
		ORDER BY date ASC`, locationID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query preferred observed: %w", err)
	}
	defer rows.Close()

	type rankedDay struct {
		day  connector.WeatherDay
		rank int
	}

	best := make(map[string]rankedDay)
	for rows.Next() {
		var d connector.WeatherDay
		var provider string
		if err := rows.Scan(&d.Date, &provider, &d.IrradiationKWhM2, &d.TemperatureMeanC, &d.TemperatureMaxC, &d.PrecipitationMM, &d.HumidityMeanPct); err != nil {
			return nil, fmt.Errorf("scan preferred weather day: %w", err)
		}
		r, ok := rank[provider]
		if !ok {
			continue
		}
		curr, exists := best[d.Date]
		if !exists || r < curr.rank {
			best[d.Date] = rankedDay{day: d, rank: r}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	dates := make([]string, 0, len(best))
	for d := range best {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	out := make([]connector.WeatherDay, len(dates))
	for i, d := range dates {
		out[i] = best[d].day
	}
	return out, nil
}
