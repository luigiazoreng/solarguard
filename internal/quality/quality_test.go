package quality_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/quality"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func newTestStore(t *testing.T) (*store.Store, domain.Source) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	src, err := st.EnsureSource(context.Background(), domain.Source{Kind: "test", Name: "test_src", Enabled: true})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	return st, src
}

// floatPtr helper
func floatPtr(v float64) *float64 { return &v }

// localDayHelper returns start and end time for date in America/Sao_Paulo
func localDayHelper(date string) (time.Time, time.Time) {
	start, end, _ := domain.LocalDay(date, "America/Sao_Paulo")
	return start, end
}

// TestRule1ValueMissing verifies rule 1: value_missing when Value is nil.
func TestRule1ValueMissing(t *testing.T) {
	ctx := context.Background()
	st, src := newTestStore(t)

	p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "Plant 1", Timezone: "America/Sao_Paulo"})
	d, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "D1"})

	start, end := localDayHelper("2026-01-01")
	st.InsertReadings(ctx, []domain.Reading{
		{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, PeriodStart: start, PeriodEnd: end, Value: nil, RawValue: "missing"},
	})

	summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if summary.CountsByReason["value_missing"] != 1 {
		t.Errorf("expected 1 value_missing, got %d", summary.CountsByReason["value_missing"])
	}

	readings, _ := st.ListReadings(ctx, store.ReadingFilter{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, From: start, To: end})
	if len(readings) != 1 {
		t.Fatalf("expected 1 reading, got %d", len(readings))
	}
	if readings[0].Quality != domain.QualityInvalid || readings[0].QualityReason != "value_missing" {
		t.Errorf("expected invalid/value_missing, got %s/%s", readings[0].Quality, readings[0].QualityReason)
	}
}

// TestRule2NegativeValue verifies rule 2: negative_value when Value < 0.
func TestRule2NegativeValue(t *testing.T) {
	ctx := context.Background()
	st, src := newTestStore(t)

	p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "Plant 1", Timezone: "America/Sao_Paulo"})
	d, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "D1"})

	start, end := localDayHelper("2026-01-01")
	st.InsertReadings(ctx, []domain.Reading{
		{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, PeriodStart: start, PeriodEnd: end, Value: floatPtr(-5.0), RawValue: "-5.0"},
	})

	summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if summary.CountsByReason["negative_value"] != 1 {
		t.Errorf("expected 1 negative_value, got %d", summary.CountsByReason["negative_value"])
	}

	readings, _ := st.ListReadings(ctx, store.ReadingFilter{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, From: start, To: end})
	if readings[0].Quality != domain.QualityInvalid || readings[0].QualityReason != "negative_value" {
		t.Errorf("expected invalid/negative_value, got %s/%s", readings[0].Quality, readings[0].QualityReason)
	}
}

// TestRule3ImplausibleHigh verifies rule 3 with all three limit options and nil safety.
func TestRule3ImplausibleHigh(t *testing.T) {
	ctx := context.Background()

	t.Run("AreaAndEfficiencyKnown", func(t *testing.T) {
		st, src := newTestStore(t)
		p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"})
		// Area 42, Eff 0.18 -> limit = 42 * 0.18 * 10 * 1.2 = 90.72 kWh
		d, _ := st.UpsertDevice(ctx, domain.Device{
			PlantID:     p.ID,
			ExternalID:  "D1",
			Name:        "D1",
			PanelAreaM2: floatPtr(42.0),
			Efficiency:  floatPtr(0.18),
		})
		s1, e1 := localDayHelper("2026-01-01")
		s2, e2 := localDayHelper("2026-01-02")
		st.InsertReadings(ctx, []domain.Reading{
			{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s1, PeriodEnd: e1, Value: floatPtr(90.0), RawValue: "90.0"},
			{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s2, PeriodEnd: e2, Value: floatPtr(91.0), RawValue: "91.0"}, // exceeds 90.72
		})
		summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if summary.CountsByReason["implausible_high"] != 1 {
			t.Errorf("expected 1 implausible_high, got %d", summary.CountsByReason["implausible_high"])
		}
	})

	t.Run("CapacityKnown", func(t *testing.T) {
		st, src := newTestStore(t)
		p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"})
		// Capacity 10 kW -> limit = 10 * 14 * 1.2 = 168 kWh
		d, _ := st.UpsertDevice(ctx, domain.Device{
			PlantID:    p.ID,
			ExternalID: "D1",
			Name:       "D1",
			CapacityKW: floatPtr(10.0),
		})
		s1, e1 := localDayHelper("2026-01-01")
		s2, e2 := localDayHelper("2026-01-02")
		st.InsertReadings(ctx, []domain.Reading{
			{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s1, PeriodEnd: e1, Value: floatPtr(160.0), RawValue: "160.0"},
			{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s2, PeriodEnd: e2, Value: floatPtr(170.0), RawValue: "170.0"}, // exceeds 168
		})
		summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if summary.CountsByReason["implausible_high"] != 1 {
			t.Errorf("expected 1 implausible_high, got %d", summary.CountsByReason["implausible_high"])
		}
	})

	t.Run("PercentileFallbackAndSkipUnder14", func(t *testing.T) {
		st, src := newTestStore(t)
		p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"})
		// Device with nil capacity, panel area, efficiency
		d, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "D1"})

		// With 10 readings (< 14), rule 3 must be skipped without crashing!
		var readings []domain.Reading
		for i := 1; i <= 10; i++ {
			dateStr := time.Date(2026, 1, i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
			start, end := localDayHelper(dateStr)
			val := float64(i * 10)
			readings = append(readings, domain.Reading{
				DeviceID:    d.ID,
				Metric:      domain.MetricEnergyKWh,
				PeriodStart: start,
				PeriodEnd:   end,
				Value:       &val,
				RawValue:    "val",
			})
		}
		st.InsertReadings(ctx, readings)
		summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("classify under 14 values: %v", err)
		}
		if summary.CountsByReason["implausible_high"] != 0 {
			t.Errorf("expected 0 implausible_high when skipped (<14 values), got %d", summary.CountsByReason["implausible_high"])
		}

		// Now add more readings so total >= 14, including an extreme spike of 50,000 kWh
		var moreReadings []domain.Reading
		for i := 11; i <= 20; i++ {
			dateStr := time.Date(2026, 1, i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
			start, end := localDayHelper(dateStr)
			val := 20.0
			if i == 20 {
				val = 50000.0 // huge outlier
			}
			moreReadings = append(moreReadings, domain.Reading{
				DeviceID:    d.ID,
				Metric:      domain.MetricEnergyKWh,
				PeriodStart: start,
				PeriodEnd:   end,
				Value:       &val,
				RawValue:    "val",
			})
		}
		st.InsertReadings(ctx, moreReadings)
		summary, err = quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("classify >=14 values: %v", err)
		}
		if summary.CountsByReason["implausible_high"] != 1 {
			t.Errorf("expected 1 implausible_high via 95th percentile fallback, got %d", summary.CountsByReason["implausible_high"])
		}
	})
}

// TestRule4StuckValue verifies:
// - a stuck run of 3 is invalid while a run of 2 is ok
// - non-zero raw value condition (consecutive zeros do not trigger stuck_value)
// - all days in run, including the first, are invalid.
func TestRule4StuckValue(t *testing.T) {
	ctx := context.Background()
	st, src := newTestStore(t)

	p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"})
	d, _ := st.UpsertDevice(ctx, domain.Device{
		PlantID:     p.ID,
		ExternalID:  "D1",
		Name:        "D1",
		PanelAreaM2: floatPtr(50.0),
		Efficiency:  floatPtr(0.20),
	})

	// Days 1..2: same raw value "18.11" (run of 2) -> should be OK
	// Day 3: different raw value "25.0"
	// Days 4..6: same raw value "30.0" (run of 3) -> should be stuck_value (all 3 days!)
	// Days 7..9: same raw value "0" (run of 3 zeros) -> not stuck_value!
	testCases := []struct {
		day string
		val float64
		raw string
	}{
		{"2026-02-01", 18.11, "18.11"},
		{"2026-02-02", 18.11, "18.11"},
		{"2026-02-03", 25.00, "25.00"},
		{"2026-02-04", 30.00, "30.00"},
		{"2026-02-05", 30.00, "30.00"},
		{"2026-02-06", 30.00, "30.00"},
		{"2026-02-07", 0.0, "0"},
		{"2026-02-08", 0.0, "0"},
		{"2026-02-09", 0.0, "0"},
	}

	var readings []domain.Reading
	for _, tc := range testCases {
		s, e := localDayHelper(tc.day)
		readings = append(readings, domain.Reading{
			DeviceID:    d.ID,
			Metric:      domain.MetricEnergyKWh,
			PeriodStart: s,
			PeriodEnd:   e,
			Value:       floatPtr(tc.val),
			RawValue:    tc.raw,
		})
	}
	st.InsertReadings(ctx, readings)

	summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}

	if summary.CountsByReason["stuck_value"] != 3 {
		t.Errorf("expected 3 stuck_value (from days 4..6), got %d", summary.CountsByReason["stuck_value"])
	}

	// Verify days 1 and 2 are not stuck_value
	fromD1, toD2 := localDayHelper("2026-02-01")
	_, toD2 = localDayHelper("2026-02-02")
	r12, _ := st.ListReadings(ctx, store.ReadingFilter{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, From: fromD1, To: toD2})
	for _, r := range r12 {
		if r.QualityReason == "stuck_value" {
			t.Errorf("reading on %v should not be stuck_value (run of 2)", r.PeriodStart)
		}
	}

	// Verify days 4, 5, 6 are all stuck_value (including first day!)
	fromD4, _ := localDayHelper("2026-02-04")
	_, toD6 := localDayHelper("2026-02-06")
	r46, _ := st.ListReadings(ctx, store.ReadingFilter{DeviceID: d.ID, Metric: domain.MetricEnergyKWh, From: fromD4, To: toD6})
	if len(r46) != 3 {
		t.Fatalf("expected 3 readings for days 4..6, got %d", len(r46))
	}
	for _, r := range r46 {
		if r.Quality != domain.QualityInvalid || r.QualityReason != "stuck_value" {
			t.Errorf("reading on %v expected invalid/stuck_value, got %s/%s", r.PeriodStart, r.Quality, r.QualityReason)
		}
	}
}

// TestRules5And6And7ZeroHandling verifies:
// - zero_with_peers_generating when Value is 0 and at least 2 peers > 5 kWh
// - zero_no_peer_evidence when Value is 0 and too few peers exist
// - low generation matching peers is ok (e.g. 2.0 kWh when peers report ~2.0 kWh)
// - blackout day where all 3 devices report 0 is ok!
func TestRules5And6And7ZeroHandling(t *testing.T) {
	ctx := context.Background()

	t.Run("ZeroWithPeersGenerating", func(t *testing.T) {
		st, src := newTestStore(t)
		p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"})
		d1, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "D1", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})
		d2, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D2", Name: "D2", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})
		d3, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D3", Name: "D3", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})

		s, e := localDayHelper("2026-07-01")
		st.InsertReadings(ctx, []domain.Reading{
			{DeviceID: d1.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(0.0), RawValue: "0"},
			{DeviceID: d2.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(20.0), RawValue: "20"},
			{DeviceID: d3.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(22.0), RawValue: "22"},
		})

		summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if summary.CountsByReason["zero_with_peers_generating"] != 1 {
			t.Errorf("expected 1 zero_with_peers_generating, got %d", summary.CountsByReason["zero_with_peers_generating"])
		}
		r1, _ := st.ListReadings(ctx, store.ReadingFilter{DeviceID: d1.ID, Metric: domain.MetricEnergyKWh, From: s, To: e})
		if r1[0].Quality != domain.QualityInvalid || r1[0].QualityReason != "zero_with_peers_generating" {
			t.Errorf("expected invalid/zero_with_peers_generating, got %s/%s", r1[0].Quality, r1[0].QualityReason)
		}
	})

	t.Run("ZeroNoPeerEvidenceDueToTooFewPeers", func(t *testing.T) {
		st, src := newTestStore(t)
		p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"})
		// Plant with only 2 devices (so each device has only 1 peer)
		d1, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "D1", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})
		d2, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D2", Name: "D2", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})

		s, e := localDayHelper("2026-07-02")
		st.InsertReadings(ctx, []domain.Reading{
			{DeviceID: d1.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(0.0), RawValue: "0"},
			{DeviceID: d2.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(25.0), RawValue: "25"},
		})

		summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if summary.CountsByReason["zero_no_peer_evidence"] != 1 {
			t.Errorf("expected 1 zero_no_peer_evidence, got %d", summary.CountsByReason["zero_no_peer_evidence"])
		}
		r1, _ := st.ListReadings(ctx, store.ReadingFilter{DeviceID: d1.ID, Metric: domain.MetricEnergyKWh, From: s, To: e})
		if r1[0].Quality != domain.QualitySuspect || r1[0].QualityReason != "zero_no_peer_evidence" {
			t.Errorf("expected suspect/zero_no_peer_evidence, got %s/%s", r1[0].Quality, r1[0].QualityReason)
		}
	})

	t.Run("LowGenerationMatchingPeersIsOK", func(t *testing.T) {
		st, src := newTestStore(t)
		p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"})
		d1, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "D1", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})
		d2, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D2", Name: "D2", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})
		d3, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D3", Name: "D3", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})

		s, e := localDayHelper("2026-07-03")
		// All 3 towers report ~2.0 kWh
		st.InsertReadings(ctx, []domain.Reading{
			{DeviceID: d1.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(2.0), RawValue: "2.0"},
			{DeviceID: d2.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(2.1), RawValue: "2.1"},
			{DeviceID: d3.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(1.9), RawValue: "1.9"},
		})

		summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if summary.CountsByQuality[domain.QualityOK] != 3 {
			t.Errorf("expected 3 OK, got %d", summary.CountsByQuality[domain.QualityOK])
		}
	})

	t.Run("BlackoutDayAllZeroIsOK", func(t *testing.T) {
		st, src := newTestStore(t)
		p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"})
		d1, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "D1", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})
		d2, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D2", Name: "D2", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})
		d3, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D3", Name: "D3", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})

		s, e := localDayHelper("2026-07-04")
		// All 3 towers report 0.0 kWh (peers are available and agree generation is 0)
		st.InsertReadings(ctx, []domain.Reading{
			{DeviceID: d1.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(0.0), RawValue: "0"},
			{DeviceID: d2.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(0.0), RawValue: "0"},
			{DeviceID: d3.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(0.0), RawValue: "0"},
		})

		summary, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if summary.CountsByQuality[domain.QualityOK] != 3 {
			t.Errorf("expected 3 OK on blackout day with agreeing peers, got %d", summary.CountsByQuality[domain.QualityOK])
		}
	})
}

// TestIdempotenceAndValueImmutability verifies that running Classify twice
// produces the identical summary and never changes readings.value.
func TestIdempotenceAndValueImmutability(t *testing.T) {
	ctx := context.Background()
	st, src := newTestStore(t)

	p, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"})
	d, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "D1", Name: "D1", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})

	var initialReadings []domain.Reading
	for i := 1; i <= 5; i++ {
		s, e := localDayHelper(time.Date(2026, 8, i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly))
		initialReadings = append(initialReadings, domain.Reading{
			DeviceID:    d.ID,
			Metric:      domain.MetricEnergyKWh,
			PeriodStart: s,
			PeriodEnd:   e,
			Value:       floatPtr(15.5),
			RawValue:    "15.5",
		})
	}
	st.InsertReadings(ctx, initialReadings)

	s1, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("first classify: %v", err)
	}

	s2, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("second classify: %v", err)
	}

	if s1.TotalReadings != s2.TotalReadings {
		t.Errorf("total readings differ: %d vs %d", s1.TotalReadings, s2.TotalReadings)
	}
	for q, count := range s1.CountsByQuality {
		if s2.CountsByQuality[q] != count {
			t.Errorf("quality %s count differ: %d vs %d", q, count, s2.CountsByQuality[q])
		}
	}
	for reason, count := range s1.CountsByReason {
		if s2.CountsByReason[reason] != count {
			t.Errorf("reason %s count differ: %d vs %d", reason, count, s2.CountsByReason[reason])
		}
	}

	// Verify values never changed
	after, _ := st.ListReadings(ctx, store.ReadingFilter{
		DeviceID: d.ID,
		Metric:   domain.MetricEnergyKWh,
		From:     time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
	})
	for i, r := range after {
		if r.Value == nil || *r.Value != 15.5 {
			t.Errorf("reading %d value changed: expected 15.5, got %v", i, r.Value)
		}
	}
}

// TestPlantIsolation verifies that a stuck value in plant A does not affect plant B.
func TestPlantIsolation(t *testing.T) {
	ctx := context.Background()
	st, src := newTestStore(t)

	pA, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "PA", Name: "Plant A", Timezone: "America/Sao_Paulo"})
	pB, _ := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "PB", Name: "Plant B", Timezone: "America/Sao_Paulo"})

	dA, _ := st.UpsertDevice(ctx, domain.Device{PlantID: pA.ID, ExternalID: "DA", Name: "DA", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})
	dB, _ := st.UpsertDevice(ctx, domain.Device{PlantID: pB.ID, ExternalID: "DB", Name: "DB", PanelAreaM2: floatPtr(50), Efficiency: floatPtr(0.2)})

	// Plant A has stuck values for 3 days ("15.0")
	// Plant B has non-stuck values ("10.0", "15.0", "20.0")
	days := []string{"2026-09-01", "2026-09-02", "2026-09-03"}
	for _, day := range days {
		s, e := localDayHelper(day)
		st.InsertReadings(ctx, []domain.Reading{
			{DeviceID: dA.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s, PeriodEnd: e, Value: floatPtr(15.0), RawValue: "15.0"},
		})
	}
	s1, e1 := localDayHelper("2026-09-01")
	s2, e2 := localDayHelper("2026-09-02")
	s3, e3 := localDayHelper("2026-09-03")
	st.InsertReadings(ctx, []domain.Reading{
		{DeviceID: dB.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s1, PeriodEnd: e1, Value: floatPtr(10.0), RawValue: "10.0"},
		{DeviceID: dB.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s2, PeriodEnd: e2, Value: floatPtr(15.0), RawValue: "15.0"},
		{DeviceID: dB.ID, Metric: domain.MetricEnergyKWh, PeriodStart: s3, PeriodEnd: e3, Value: floatPtr(20.0), RawValue: "20.0"},
	})

	sumA, err := quality.Classify(ctx, st, pA.ID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("classify plant A: %v", err)
	}
	sumB, err := quality.Classify(ctx, st, pB.ID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("classify plant B: %v", err)
	}

	if sumA.CountsByReason["stuck_value"] != 3 {
		t.Errorf("plant A: expected 3 stuck_value, got %d", sumA.CountsByReason["stuck_value"])
	}
	if sumB.CountsByReason["stuck_value"] != 0 {
		t.Errorf("plant B: expected 0 stuck_value, got %d", sumB.CountsByReason["stuck_value"])
	}
}
