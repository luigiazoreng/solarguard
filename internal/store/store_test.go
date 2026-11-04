package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "sub", "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seed(t *testing.T, s *Store, plantExt string) (domain.Plant, domain.Device) {
	t.Helper()
	ctx := context.Background()
	src, err := s.EnsureSource(ctx, domain.Source{Kind: "file", Name: "fonte", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: plantExt, Name: "Planta " + plantExt, Timezone: "America/Sao_Paulo"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "D1"})
	if err != nil {
		t.Fatal(err)
	}
	return p, d
}

func reading(deviceID int64, day int, v float64) domain.Reading {
	start := time.Date(2026, 1, day, 3, 0, 0, 0, time.UTC)
	return domain.Reading{DeviceID: deviceID, Metric: domain.MetricEnergyKWh, PeriodStart: start, PeriodEnd: start.AddDate(0, 0, 1), Value: &v, RawValue: "bruto"}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	for range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestPlantsAndDevicesAreSeparatedByPlant(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	pa, da := seed(t, s, "A")
	pb, db := seed(t, s, "B")
	if pa.ID == pb.ID || da.ID == db.ID {
		t.Fatal("plants with different external IDs must not merge")
	}
	again, _ := seed(t, s, "A")
	if again.ID != pa.ID {
		t.Fatal("upsert must keep the plant ID")
	}
	plants, err := s.ListPlants(ctx)
	if err != nil || len(plants) != 2 {
		t.Fatalf("plants=%d err=%v", len(plants), err)
	}
	devices, err := s.ListDevices(ctx, pb.ID)
	if err != nil || len(devices) != 1 || devices[0].ID != db.ID {
		t.Fatalf("devices=%v err=%v", devices, err)
	}
}

func TestUpsertDoesNotEraseKnownFields(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	p, d := seed(t, s, "A")
	area := 42.0
	d.PanelAreaM2 = &area
	if _, err := s.UpsertDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "novo"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListDevices(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].PanelAreaM2 == nil || *got[0].PanelAreaM2 != 42 || got[0].Name != "novo" {
		t.Fatalf("got %+v", got[0])
	}
}

func TestReimportKeepsOriginalMeasurement(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, d := seed(t, s, "A")
	n, err := s.InsertReadings(ctx, []domain.Reading{reading(d.ID, 1, 10)})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	n, err = s.InsertReadings(ctx, []domain.Reading{reading(d.ID, 1, 99)})
	if err != nil || n != 0 {
		t.Fatalf("second import n=%d err=%v", n, err)
	}
	got, err := s.ListReadings(ctx, ReadingFilter{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil || len(got) != 1 || *got[0].Value != 10 {
		t.Fatalf("got %+v err=%v", got, err)
	}
}

func TestDatabaseRefusesToChangeMeasurement(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, d := seed(t, s, "A")
	if _, err := s.InsertReadings(ctx, []domain.Reading{reading(d.ID, 1, 10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE readings SET value = 11`); err == nil {
		t.Fatal("expected trigger to refuse the update")
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE readings SET raw_value = 'x'`); err == nil {
		t.Fatal("expected trigger to refuse the raw value update")
	}
}

func TestQualityChangeKeepsValueAndFilterWorks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, d := seed(t, s, "A")
	if _, err := s.InsertReadings(ctx, []domain.Reading{reading(d.ID, 1, 10), reading(d.ID, 2, 20)}); err != nil {
		t.Fatal(err)
	}
	f := ReadingFilter{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}
	all, err := s.ListReadings(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetReadingQuality(ctx, all[0].ID, domain.QualityInvalid, "pico"); err != nil {
		t.Fatal(err)
	}
	f.Qualities = []domain.Quality{domain.QualityInvalid}
	got, err := s.ListReadings(ctx, f)
	if err != nil || len(got) != 1 || *got[0].Value != 10 || got[0].QualityReason != "pico" {
		t.Fatalf("got %+v err=%v", got, err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, d := seed(t, s, "A")
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for day := 1; day <= 20; day++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.InsertReadings(ctx, []domain.Reading{reading(d.ID, day, float64(day))})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestGetPlantNotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.GetPlant(context.Background(), 999); err != ErrNotFound {
		t.Fatalf("got %v", err)
	}
}
