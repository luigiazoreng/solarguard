package pipeline_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/pipeline"
	"github.com/luigiazoreng/solarguard/internal/store"
)

type mockWeatherProvider struct {
	name        string
	observedErr error
	forecastErr error
	observed    []connector.WeatherDay
	forecast    []connector.WeatherDay
}

func (m *mockWeatherProvider) Name() string {
	if m.name != "" {
		return m.name
	}
	return "mock_weather"
}

func (m *mockWeatherProvider) Observed(ctx context.Context, q connector.WeatherQuery, from, to string) ([]connector.WeatherDay, error) {
	if m.observedErr != nil {
		return nil, m.observedErr
	}
	return m.observed, nil
}

func (m *mockWeatherProvider) Forecast(ctx context.Context, q connector.WeatherQuery, days int) ([]connector.WeatherDay, error) {
	if m.forecastErr != nil {
		return nil, m.forecastErr
	}
	return m.forecast, nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pipeline_test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func setupTestPlant(t *testing.T, st *store.Store, name, extID, tz string, lat, lon float64) (domain.Plant, domain.Device) {
	t.Helper()
	ctx := context.Background()
	src, err := st.EnsureSource(ctx, domain.Source{Kind: "mock", Name: "src_" + extID, Enabled: true})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}

	p, err := st.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: extID,
		Name:       name,
		Timezone:   tz,
		Latitude:   &lat,
		Longitude:  &lon,
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}

	dev, err := st.UpsertDevice(ctx, domain.Device{
		PlantID:    p.ID,
		ExternalID: "D1",
		Name:       "D1",
		Kind:       "inverter",
	})
	if err != nil {
		t.Fatalf("upsert device: %v", err)
	}

	return p, dev
}

// TestPipelineOneStepFailingStillRunsRest verifies that when one step fails (e.g. weather sync),
// the remaining steps still execute, the run is recorded, and no network is reached.
func TestPipelineOneStepFailingStillRunsRest(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	plant, _ := setupTestPlant(t, st, "Planta Alfa", "P1", "America/Sao_Paulo", -23.5, -46.6)

	mockWP := &mockWeatherProvider{
		observedErr: errors.New("simulated weather provider outage"),
	}

	fixedNow := time.Date(2026, 5, 10, 3, 0, 0, 0, time.UTC)
	deps := pipeline.Deps{
		WeatherProvider: mockWP,
		Now:             func() time.Time { return fixedNow },
	}

	run, err := pipeline.RunPlant(ctx, st, plant.ID, deps)
	if err != nil {
		t.Fatalf("RunPlant unexpected fatal error: %v", err)
	}
	if run == nil {
		t.Fatal("expected non-nil run record")
	}

	if run.Status != "partial" {
		t.Errorf("expected status 'partial' when one step fails, got %q", run.Status)
	}

	// Verify step outcomes recorded
	runs, err := st.ListPipelineRuns(ctx, &plant.ID, 1)
	if err != nil {
		t.Fatalf("list pipeline runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 recorded run, got %d", len(runs))
	}

	r := runs[0]
	if r.Status != "partial" {
		t.Errorf("expected DB status 'partial', got %q", r.Status)
	}
}

// TestPipelineSchedulerAcrossTimeZones verifies that plants in different time zones only run after 02:00
// in their local time zone, and restarts do not rerun the pipeline for the same day.
func TestPipelineSchedulerAcrossTimeZones(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// SP is UTC-3. At 05:30 UTC, it is 02:30 in SP (eligible to run).
	// Manaus is UTC-4. At 05:30 UTC, it is 01:30 in Manaus (NOT eligible yet).
	plantSP, _ := setupTestPlant(t, st, "Planta Sao Paulo", "SP", "America/Sao_Paulo", -23.5, -46.6)
	plantAM, _ := setupTestPlant(t, st, "Planta Manaus", "AM", "America/Manaus", -3.1, -60.0)

	mockWP := &mockWeatherProvider{}
	now := time.Date(2026, 5, 10, 5, 30, 0, 0, time.UTC)
	deps := pipeline.Deps{
		WeatherProvider: mockWP,
		Now:             func() time.Time { return now },
	}

	sched := pipeline.NewScheduler(st, deps)

	// First pass
	if err := sched.CheckAndRunOnce(ctx); err != nil {
		t.Fatalf("first check and run: %v", err)
	}

	// SP should have run
	hasSP, err := st.HasPipelineRun(ctx, plantSP.ID, "2026-05-10")
	if err != nil || !hasSP {
		t.Errorf("expected SP to have run, got hasSP=%v, err=%v", hasSP, err)
	}

	// Manaus should NOT have run yet
	hasAM, err := st.HasPipelineRun(ctx, plantAM.ID, "2026-05-10")
	if err != nil || hasAM {
		t.Errorf("expected Manaus NOT to have run yet, got hasAM=%v, err=%v", hasAM, err)
	}

	// Simulated restart at the same time: should not rerun SP
	spRunsBefore, _ := st.ListPipelineRuns(ctx, &plantSP.ID, 10)
	if err := sched.CheckAndRunOnce(ctx); err != nil {
		t.Fatalf("second check and run after restart: %v", err)
	}
	spRunsAfter, _ := st.ListPipelineRuns(ctx, &plantSP.ID, 10)
	if len(spRunsBefore) != len(spRunsAfter) {
		t.Errorf("restart reran SP: before count=%d, after count=%d", len(spRunsBefore), len(spRunsAfter))
	}

	// Advance time by 1 hour (06:30 UTC = 02:30 in Manaus)
	now = now.Add(1 * time.Hour)
	if err := sched.CheckAndRunOnce(ctx); err != nil {
		t.Fatalf("check and run after 1 hour: %v", err)
	}

	hasAMLater, err := st.HasPipelineRun(ctx, plantAM.ID, "2026-05-10")
	if err != nil || !hasAMLater {
		t.Errorf("expected Manaus to run once local time >= 02:00, got %v, err=%v", hasAMLater, err)
	}
}
