package weather_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/weather"
)

type mockWeatherProvider struct {
	name         string
	observedDays []connector.WeatherDay
	forecastDays []connector.WeatherDay
	lastQuery    connector.WeatherQuery
}

func (m *mockWeatherProvider) Name() string {
	if m.name == "" {
		return "mock-provider"
	}
	return m.name
}

func (m *mockWeatherProvider) Observed(ctx context.Context, q connector.WeatherQuery, from, to string) ([]connector.WeatherDay, error) {
	m.lastQuery = q
	return m.observedDays, nil
}

func (m *mockWeatherProvider) Forecast(ctx context.Context, q connector.WeatherQuery, days int) ([]connector.WeatherDay, error) {
	m.lastQuery = q
	return m.forecastDays, nil
}

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

func TestSyncObserved_TwoPlantsWithDifferentCoordinates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mock := &mockWeatherProvider{
		observedDays: []connector.WeatherDay{
			{Date: "2026-09-20", IrradiationKWhM2: ptr(5.0)},
		},
	}
	svc := weather.NewService(s, mock)

	src, err := s.EnsureSource(ctx, domain.Source{Name: "test-src", Kind: "test"})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}

	p1, err := s.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "plant-1",
		Name:       "Plant 1",
		Timezone:   "America/Sao_Paulo",
		Latitude:   ptr(-23.4969),
		Longitude:  ptr(-46.6199),
	})
	if err != nil {
		t.Fatalf("upsert plant 1: %v", err)
	}

	p2, err := s.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "plant-2",
		Name:       "Plant 2",
		Timezone:   "America/Sao_Paulo",
		Latitude:   ptr(-15.7894),
		Longitude:  ptr(-47.9258),
	})
	if err != nil {
		t.Fatalf("upsert plant 2: %v", err)
	}

	if err := svc.SyncObserved(ctx, p1.ID, "2026-09-20", "2026-09-20"); err != nil {
		t.Fatalf("sync observed p1: %v", err)
	}
	if err := svc.SyncObserved(ctx, p2.ID, "2026-09-20", "2026-09-20"); err != nil {
		t.Fatalf("sync observed p2: %v", err)
	}

	got1, err := s.GetPlant(ctx, p1.ID)
	if err != nil {
		t.Fatalf("get plant 1: %v", err)
	}
	got2, err := s.GetPlant(ctx, p2.ID)
	if err != nil {
		t.Fatalf("get plant 2: %v", err)
	}

	if got1.WeatherLocationID == nil {
		t.Fatal("expected plant 1 to have WeatherLocationID")
	}
	if got2.WeatherLocationID == nil {
		t.Fatal("expected plant 2 to have WeatherLocationID")
	}

	// Two plants with different coordinates get different locations
	if *got1.WeatherLocationID == *got2.WeatherLocationID {
		t.Errorf("expected different weather location IDs for different coordinates, but got both %d", *got1.WeatherLocationID)
	}
}

func TestSyncObserved_PlantsSharingCoordinates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mock := &mockWeatherProvider{
		observedDays: []connector.WeatherDay{
			{Date: "2026-09-20", IrradiationKWhM2: ptr(5.0)},
		},
	}
	svc := weather.NewService(s, mock)

	src, err := s.EnsureSource(ctx, domain.Source{Name: "test-src", Kind: "test"})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}

	// Two plants whose coordinates round to -23.50 and -46.62
	p1, err := s.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "plant-1",
		Name:       "Plant 1",
		Timezone:   "America/Sao_Paulo",
		Latitude:   ptr(-23.4969), // rounds to -23.50
		Longitude:  ptr(-46.6199), // rounds to -46.62
	})
	if err != nil {
		t.Fatalf("upsert plant 1: %v", err)
	}

	p2, err := s.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "plant-2",
		Name:       "Plant 2",
		Timezone:   "America/Sao_Paulo",
		Latitude:   ptr(-23.5011), // rounds to -23.50
		Longitude:  ptr(-46.6180), // rounds to -46.62
	})
	if err != nil {
		t.Fatalf("upsert plant 2: %v", err)
	}

	if err := svc.SyncObserved(ctx, p1.ID, "2026-09-20", "2026-09-20"); err != nil {
		t.Fatalf("sync observed p1: %v", err)
	}
	if err := svc.SyncObserved(ctx, p2.ID, "2026-09-20", "2026-09-20"); err != nil {
		t.Fatalf("sync observed p2: %v", err)
	}

	got1, err := s.GetPlant(ctx, p1.ID)
	if err != nil {
		t.Fatalf("get plant 1: %v", err)
	}
	got2, err := s.GetPlant(ctx, p2.ID)
	if err != nil {
		t.Fatalf("get plant 2: %v", err)
	}

	if got1.WeatherLocationID == nil || got2.WeatherLocationID == nil {
		t.Fatal("expected both plants to have weather location ID")
	}

	// Plants sharing coordinates rounded to 2 decimals share a location
	if *got1.WeatherLocationID != *got2.WeatherLocationID {
		t.Errorf("expected plants sharing coordinates to share weather location ID %d, got %d and %d",
			*got1.WeatherLocationID, *got1.WeatherLocationID, *got2.WeatherLocationID)
	}
}

func TestSyncObserved_PlantWithNoCoordinates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mock := &mockWeatherProvider{}
	svc := weather.NewService(s, mock)

	src, err := s.EnsureSource(ctx, domain.Source{Name: "test-src", Kind: "test"})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}

	p, err := s.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "plant-no-coords",
		Name:       "Plant No Coords",
		Timezone:   "America/Sao_Paulo",
		Latitude:   nil,
		Longitude:  nil,
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}

	err = svc.SyncObserved(ctx, p.ID, "2026-09-20", "2026-09-20")
	if err == nil {
		t.Fatal("expected error for plant without coordinates, got nil")
	}
	if !strings.Contains(err.Error(), "no coordinates") {
		t.Errorf("expected error message to mention coordinates, got %q", err.Error())
	}
}

func TestSyncForecast(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mock := &mockWeatherProvider{
		forecastDays: []connector.WeatherDay{
			{Date: "2026-11-05", IrradiationKWhM2: ptr(4.5)},
			{Date: "2026-11-06", IrradiationKWhM2: ptr(5.5)},
		},
	}
	svc := weather.NewService(s, mock)

	src, err := s.EnsureSource(ctx, domain.Source{Name: "test-src", Kind: "test"})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}

	p, err := s.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "plant-f",
		Name:       "Plant Forecast",
		Timezone:   "America/Sao_Paulo",
		Latitude:   ptr(-23.5),
		Longitude:  ptr(-46.6),
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}

	if err := svc.SyncForecast(ctx, p.ID, 7); err != nil {
		t.Fatalf("sync forecast: %v", err)
	}

	got, err := s.GetPlant(ctx, p.ID)
	if err != nil {
		t.Fatalf("get plant: %v", err)
	}
	if got.WeatherLocationID == nil {
		t.Fatal("expected plant to have weather location ID")
	}

	days, err := s.ListWeatherDays(ctx, *got.WeatherLocationID, "2026-11-05", "2026-11-06", "forecast")
	if err != nil {
		t.Fatalf("list forecast: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("expected 2 forecast days, got %d", len(days))
	}
	if *days[0].IrradiationKWhM2 != 4.5 {
		t.Errorf("expected 4.5, got %v", *days[0].IrradiationKWhM2)
	}
}

func TestSync_PlantInOtherTimezoneUsesItsOwnZone(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mock := &mockWeatherProvider{
		observedDays: []connector.WeatherDay{
			{Date: "2026-09-20", IrradiationKWhM2: ptr(5.0)},
		},
	}
	svc := weather.NewService(s, mock)

	src, err := s.EnsureSource(ctx, domain.Source{Name: "test-src", Kind: "test"})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}

	p, err := s.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "plant-manaus",
		Name:       "Plant Manaus",
		Timezone:   "America/Manaus",
		Latitude:   ptr(-3.119),
		Longitude:  ptr(-60.021),
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}

	if err := svc.SyncObserved(ctx, p.ID, "2026-09-20", "2026-09-20"); err != nil {
		t.Fatalf("sync observed: %v", err)
	}

	if mock.lastQuery.Timezone != "America/Manaus" {
		t.Errorf("expected query timezone 'America/Manaus', got %q", mock.lastQuery.Timezone)
	}

	got, err := s.GetPlant(ctx, p.ID)
	if err != nil {
		t.Fatalf("get plant: %v", err)
	}
	loc, err := s.GetWeatherLocation(ctx, *got.WeatherLocationID)
	if err != nil {
		t.Fatalf("get weather location: %v", err)
	}
	if loc.Timezone != "America/Manaus" {
		t.Errorf("expected location timezone 'America/Manaus', got %q", loc.Timezone)
	}
}

func TestPlantWithoutTimezoneIsRejected(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src, err := st.EnsureSource(ctx, domain.Source{Kind: "file", Name: "fonte", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	lat, lon := -23.5, -46.6
	p, err := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "SEM-FUSO", Name: "Sem fuso", Latitude: &lat, Longitude: &lon})
	if err != nil {
		t.Fatal(err)
	}
	svc := weather.NewService(st, nil)
	err = svc.SyncObserved(ctx, p.ID, "2026-01-01", "2026-01-02")
	if err == nil || !strings.Contains(err.Error(), "time zone") {
		t.Fatalf("got %v, want a time zone error", err)
	}
}
