package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ptr[T any](v T) *T {
	return &v
}

func TestEnsureWeatherLocation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	loc := store.WeatherLocation{
		Name:        "loc_-23.50_-46.62",
		Latitude:    ptr(-23.50),
		Longitude:   ptr(-46.62),
		Timezone:    "America/Sao_Paulo",
		StationCode: "A701",
	}

	created, err := s.EnsureWeatherLocation(ctx, loc)
	if err != nil {
		t.Fatalf("ensure weather location: %v", err)
	}
	if created.ID == 0 {
		t.Errorf("expected non-zero ID, got %d", created.ID)
	}
	if created.Name != loc.Name {
		t.Errorf("expected name %q, got %q", loc.Name, created.Name)
	}

	// Calling again with the same name returns the existing row
	again, err := s.EnsureWeatherLocation(ctx, loc)
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if again.ID != created.ID {
		t.Errorf("expected same ID %d, got %d", created.ID, again.ID)
	}
}

func TestLinkPlantWeather(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	src, err := s.EnsureSource(ctx, domain.Source{Name: "test-src", Kind: "test"})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	p, err := s.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "p1",
		Name:       "Plant 1",
		Timezone:   "America/Sao_Paulo",
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}

	loc, err := s.EnsureWeatherLocation(ctx, store.WeatherLocation{
		Name:     "sp-mirante",
		Timezone: "America/Sao_Paulo",
	})
	if err != nil {
		t.Fatalf("ensure location: %v", err)
	}

	if err := s.LinkPlantWeather(ctx, p.ID, loc.ID); err != nil {
		t.Fatalf("link plant weather: %v", err)
	}

	got, err := s.GetPlant(ctx, p.ID)
	if err != nil {
		t.Fatalf("get plant: %v", err)
	}
	if got.WeatherLocationID == nil || *got.WeatherLocationID != loc.ID {
		t.Errorf("expected weather_location_id %d, got %v", loc.ID, got.WeatherLocationID)
	}
}

func TestUpsertForecastAndObserved(t *testing.T) {
	// Requirements:
	// - Upsert replaces a forecast;
	// - a forecast never changes an observed row;
	// - PreferredObserved returns inmet over open-meteo when both exist.
	ctx := context.Background()
	s := newTestStore(t)

	loc, err := s.EnsureWeatherLocation(ctx, store.WeatherLocation{
		Name:     "loc-test",
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("ensure location: %v", err)
	}

	date := "2026-09-20"

	// 1. Insert observed row from open-meteo
	observedOpenMeteo := []connector.WeatherDay{
		{
			Date:             date,
			IrradiationKWhM2: ptr(3.0),
			TemperatureMeanC: ptr(20.0),
		},
	}
	if err := s.UpsertWeatherDays(ctx, loc.ID, "observed", "open-meteo", observedOpenMeteo); err != nil {
		t.Fatalf("upsert observed open-meteo: %v", err)
	}

	// 2. Insert forecast row from open-meteo for the same date with different values
	forecastV1 := []connector.WeatherDay{
		{
			Date:             date,
			IrradiationKWhM2: ptr(2.5),
			TemperatureMeanC: ptr(19.0),
		},
	}
	if err := s.UpsertWeatherDays(ctx, loc.ID, "forecast", "open-meteo", forecastV1); err != nil {
		t.Fatalf("upsert forecast v1: %v", err)
	}

	// Verify observed row was NOT changed by forecast
	observedList, err := s.ListWeatherDays(ctx, loc.ID, date, date, "observed")
	if err != nil {
		t.Fatalf("list observed: %v", err)
	}
	if len(observedList) != 1 {
		t.Fatalf("expected 1 observed row, got %d", len(observedList))
	}
	if *observedList[0].IrradiationKWhM2 != 3.0 {
		t.Errorf("expected observed irradiation 3.0, got %v", *observedList[0].IrradiationKWhM2)
	}

	// 3. Upsert newer forecast replaces the older forecast
	forecastV2 := []connector.WeatherDay{
		{
			Date:             date,
			IrradiationKWhM2: ptr(2.8),
			TemperatureMeanC: ptr(19.5),
		},
	}
	if err := s.UpsertWeatherDays(ctx, loc.ID, "forecast", "open-meteo", forecastV2); err != nil {
		t.Fatalf("upsert forecast v2: %v", err)
	}

	forecastList, err := s.ListWeatherDays(ctx, loc.ID, date, date, "forecast")
	if err != nil {
		t.Fatalf("list forecast: %v", err)
	}
	if len(forecastList) != 1 {
		t.Fatalf("expected 1 forecast row, got %d", len(forecastList))
	}
	if *forecastList[0].IrradiationKWhM2 != 2.8 {
		t.Errorf("expected updated forecast irradiation 2.8, got %v", *forecastList[0].IrradiationKWhM2)
	}

	// Observed row STILL untouched
	observedListAgain, err := s.ListWeatherDays(ctx, loc.ID, date, date, "observed")
	if err != nil {
		t.Fatalf("list observed again: %v", err)
	}
	if *observedListAgain[0].IrradiationKWhM2 != 3.0 {
		t.Errorf("expected observed row unchanged at 3.0, got %v", *observedListAgain[0].IrradiationKWhM2)
	}

	// 4. Test PreferredObserved returns inmet over open-meteo when both exist
	observedInmet := []connector.WeatherDay{
		{
			Date:             date,
			IrradiationKWhM2: ptr(5.5),
			TemperatureMeanC: ptr(22.0),
		},
		{
			Date:             "2026-09-21",
			IrradiationKWhM2: ptr(6.0),
			TemperatureMeanC: ptr(23.0),
		},
	}
	if err := s.UpsertWeatherDays(ctx, loc.ID, "observed", "inmet", observedInmet); err != nil {
		t.Fatalf("upsert observed inmet: %v", err)
	}

	// Add an open-meteo observed on 2026-09-22 only
	observedOpenMeteoDay22 := []connector.WeatherDay{
		{
			Date:             "2026-09-22",
			IrradiationKWhM2: ptr(4.0),
			TemperatureMeanC: ptr(21.0),
		},
	}
	if err := s.UpsertWeatherDays(ctx, loc.ID, "observed", "open-meteo", observedOpenMeteoDay22); err != nil {
		t.Fatalf("upsert observed open-meteo day 22: %v", err)
	}

	preferred, err := s.PreferredObserved(ctx, loc.ID, "2026-09-20", "2026-09-22", []string{"inmet", "open-meteo"})
	if err != nil {
		t.Fatalf("preferred observed: %v", err)
	}
	if len(preferred) != 3 {
		t.Fatalf("expected 3 dates, got %d", len(preferred))
	}

	// 2026-09-20: both inmet (5.5) and open-meteo (3.0) exist -> inmet preferred!
	if preferred[0].Date != "2026-09-20" || *preferred[0].IrradiationKWhM2 != 5.5 {
		t.Errorf("expected 2026-09-20 preferred to be inmet (5.5), got %v", preferred[0].IrradiationKWhM2)
	}
	// 2026-09-21: inmet (6.0)
	if preferred[1].Date != "2026-09-21" || *preferred[1].IrradiationKWhM2 != 6.0 {
		t.Errorf("expected 2026-09-21 inmet (6.0), got %v", preferred[1].IrradiationKWhM2)
	}
	// 2026-09-22: only open-meteo (4.0) exists -> open-meteo returned
	if preferred[2].Date != "2026-09-22" || *preferred[2].IrradiationKWhM2 != 4.0 {
		t.Errorf("expected 2026-09-22 open-meteo (4.0), got %v", preferred[2].IrradiationKWhM2)
	}
}

func TestSetPlantLocation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	src, err := s.EnsureSource(ctx, domain.Source{Name: "test-src", Kind: "test"})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	p, err := s.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "p1",
		Name:       "Imported Plant",
		Timezone:   "America/Sao_Paulo",
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}

	if err := s.SetPlantLocation(ctx, p.ID, -23.4969, -46.6199); err != nil {
		t.Fatalf("set plant location: %v", err)
	}

	updated, err := s.GetPlant(ctx, p.ID)
	if err != nil {
		t.Fatalf("get plant: %v", err)
	}
	if updated.Latitude == nil || *updated.Latitude != -23.4969 {
		t.Errorf("expected lat -23.4969, got %v", updated.Latitude)
	}
	if updated.Longitude == nil || *updated.Longitude != -46.6199 {
		t.Errorf("expected lon -46.6199, got %v", updated.Longitude)
	}
}
