package analytics_test

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/analytics"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func setupTestDB(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, ctx
}

func createTestPlant(t *testing.T, ctx context.Context, st *store.Store, name, tz string, devNames ...string) (domain.Plant, []domain.Device) {
	t.Helper()
	src, err := st.EnsureSource(ctx, domain.Source{
		Kind: "test",
		Name: "test-src-" + name,
	})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}

	p, err := st.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "ext-" + name,
		Name:       name,
		Timezone:   tz,
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}

	var devs []domain.Device
	for _, devName := range devNames {
		kind := "inverter"
		if devName == "PLANT_TOTAL" {
			kind = "plant"
		}
		d, err := st.UpsertDevice(ctx, domain.Device{
			PlantID:    p.ID,
			ExternalID: "ext-" + name + "-" + devName,
			Name:       devName,
			Kind:       kind,
		})
		if err != nil {
			t.Fatalf("upsert device %s: %v", devName, err)
		}
		devs = append(devs, d)
	}
	return p, devs
}

func insertDailyReading(t *testing.T, ctx context.Context, st *store.Store, devID int64, tz, dateStr string, val *float64, q domain.Quality, reason string) {
	t.Helper()
	pStart, pEnd, err := domain.LocalDay(dateStr, tz)
	if err != nil {
		t.Fatalf("local day %s: %v", dateStr, err)
	}
	raw := ""
	if val != nil {
		raw = "val"
	}
	_, err = st.InsertReadings(ctx, []domain.Reading{
		{
			DeviceID:      devID,
			Metric:        domain.MetricEnergyKWh,
			PeriodStart:   pStart,
			PeriodEnd:     pEnd,
			Value:         val,
			RawValue:      raw,
			Quality:       q,
			QualityReason: reason,
		},
	})
	if err != nil {
		t.Fatalf("insert reading: %v", err)
	}
}

func floatPtr(f float64) *float64 { return &f }

func almostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// 1. Not-ok values are excluded from totals and missing days are counted.
func TestAnalytics_NotOKValuesExcludedAndMissingDaysCounted(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant1", tz, "D1")
	dev1 := devs[0]

	// 4 days:
	// Day 1: ok (10.0 kWh)
	// Day 2: suspect (25.0 kWh)
	// Day 3: invalid (50.0 kWh)
	// Day 4: missing (no reading stored)
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-03-01", floatPtr(10.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-03-02", floatPtr(25.0), domain.QualitySuspect, "zero_no_peer_evidence")
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-03-03", floatPtr(50.0), domain.QualityInvalid, "implausible_reading")

	gen, err := analytics.Generation(ctx, st, plant.ID, &dev1.ID, "2026-03-01", "2026-03-04", "day")
	if err != nil {
		t.Fatalf("Generation error: %v", err)
	}

	// Hand-computed expected values:
	// Only 10.0 kWh should be in the total (25.0 and 50.0 excluded!)
	if !almostEqual(gen.TotalMeasuredKWh, 10.0, 1e-4) {
		t.Errorf("TotalMeasuredKWh = %f, want 10.0", gen.TotalMeasuredKWh)
	}
	if gen.DaysOK != 1 {
		t.Errorf("DaysOK = %d, want 1", gen.DaysOK)
	}
	if gen.DaysSuspect != 1 {
		t.Errorf("DaysSuspect = %d, want 1", gen.DaysSuspect)
	}
	if gen.DaysInvalid != 1 {
		t.Errorf("DaysInvalid = %d, want 1", gen.DaysInvalid)
	}
	if gen.DaysMissing != 1 {
		t.Errorf("DaysMissing = %d, want 1", gen.DaysMissing)
	}

	// Also check without deviceID (plant level)
	genPlant, err := analytics.Generation(ctx, st, plant.ID, nil, "2026-03-01", "2026-03-04", "day")
	if err != nil {
		t.Fatalf("Plant Generation error: %v", err)
	}
	if !almostEqual(genPlant.TotalMeasuredKWh, 10.0, 1e-4) {
		t.Errorf("Plant TotalMeasuredKWh = %f, want 10.0", genPlant.TotalMeasuredKWh)
	}
	if genPlant.DaysMissing != 1 {
		t.Errorf("Plant DaysMissing = %d, want 1", genPlant.DaysMissing)
	}
}

// 2. ExpectedVsActual: measured when ok, expected from active model, quality and reason, totals on ok days.
func TestAnalytics_ExpectedVsActual(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant1", tz, "D1")
	dev1 := devs[0]

	// Seed active model run and predictions
	mrun, err := st.UpsertModelRun(ctx, store.ModelRun{
		Name:      "ridge",
		Version:   "v1",
		PlantID:   plant.ID,
		TrainedAt: time.Now(),
		Active:    true,
	})
	if err != nil {
		t.Fatalf("upsert model run: %v", err)
	}
	err = st.UpsertPredictions(ctx, []store.Prediction{
		{DeviceID: dev1.ID, Date: "2026-03-01", ModelRunID: mrun.ID, KWhExpected: 12.0},
		{DeviceID: dev1.ID, Date: "2026-03-02", ModelRunID: mrun.ID, KWhExpected: 20.0},
		{DeviceID: dev1.ID, Date: "2026-03-03", ModelRunID: mrun.ID, KWhExpected: 30.0},
		{DeviceID: dev1.ID, Date: "2026-03-04", ModelRunID: mrun.ID, KWhExpected: 15.0},
	})
	if err != nil {
		t.Fatalf("upsert predictions: %v", err)
	}

	// Insert readings:
	// Day 1: ok (10.0 kWh)
	// Day 2: suspect (25.0 kWh)
	// Day 3: invalid (50.0 kWh)
	// Day 4: missing
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-03-01", floatPtr(10.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-03-02", floatPtr(25.0), domain.QualitySuspect, "reason_suspect")
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-03-03", floatPtr(50.0), domain.QualityInvalid, "reason_invalid")

	res, err := analytics.ExpectedVsActual(ctx, st, plant.ID, &dev1.ID, "2026-03-01", "2026-03-04")
	if err != nil {
		t.Fatalf("ExpectedVsActual error: %v", err)
	}

	// Hand-computed totals:
	// Measured on ok days = 10.0 (Day 1 only!)
	// Expected on ok days = 12.0 (Day 1 only!)
	// Ratio = 10.0 / 12.0 = 0.8333...
	if !almostEqual(res.TotalMeasuredKWh, 10.0, 1e-4) {
		t.Errorf("TotalMeasuredKWh = %f, want 10.0", res.TotalMeasuredKWh)
	}
	if !almostEqual(res.TotalExpectedKWh, 12.0, 1e-4) {
		t.Errorf("TotalExpectedKWh = %f, want 12.0", res.TotalExpectedKWh)
	}
	if res.Ratio == nil || !almostEqual(*res.Ratio, 10.0/12.0, 1e-4) {
		t.Errorf("Ratio = %v, want %f", res.Ratio, 10.0/12.0)
	}

	if len(res.Days) != 4 {
		t.Fatalf("got %d days, want 4", len(res.Days))
	}

	// Day 1: ok
	if res.Days[0].Quality != domain.QualityOK || res.Days[0].MeasuredKWh == nil || *res.Days[0].MeasuredKWh != 10.0 {
		t.Errorf("Day 1 mismatch: %+v", res.Days[0])
	}
	// Day 2: suspect (measured must be nil!)
	if res.Days[1].Quality != domain.QualitySuspect || res.Days[1].MeasuredKWh != nil || res.Days[1].QualityReason != "reason_suspect" {
		t.Errorf("Day 2 mismatch: %+v", res.Days[1])
	}
	// Day 4: missing (measured must be nil!)
	if res.Days[3].Quality != "missing" || res.Days[3].MeasuredKWh != nil {
		t.Errorf("Day 4 mismatch: %+v", res.Days[3])
	}
}

// 3. LostEnergy: sum of expected over device-days whose reading is not ok or is missing.
func TestAnalytics_LostEnergy(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant1", tz, "D1")
	dev1 := devs[0]

	mrun, _ := st.UpsertModelRun(ctx, store.ModelRun{
		Name:      "ridge",
		Version:   "v1",
		PlantID:   plant.ID,
		TrainedAt: time.Now(),
		Active:    true,
	})
	_ = st.UpsertPredictions(ctx, []store.Prediction{
		{DeviceID: dev1.ID, Date: "2026-03-01", ModelRunID: mrun.ID, KWhExpected: 12.0}, // Day 1: ok -> not lost
		{DeviceID: dev1.ID, Date: "2026-03-02", ModelRunID: mrun.ID, KWhExpected: 20.0}, // Day 2: suspect -> lost 20.0
		{DeviceID: dev1.ID, Date: "2026-03-03", ModelRunID: mrun.ID, KWhExpected: 30.0}, // Day 3: invalid -> lost 30.0
		{DeviceID: dev1.ID, Date: "2026-03-04", ModelRunID: mrun.ID, KWhExpected: 15.0}, // Day 4: missing -> lost 15.0
	})

	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-03-01", floatPtr(10.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-03-02", floatPtr(25.0), domain.QualitySuspect, "s")
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-03-03", floatPtr(50.0), domain.QualityInvalid, "inv")

	lost, err := analytics.LostEnergy(ctx, st, plant.ID, "2026-03-01", "2026-03-04")
	if err != nil {
		t.Fatalf("LostEnergy error: %v", err)
	}

	// Hand-computed: 20.0 + 30.0 + 15.0 = 65.0 kWh
	if !almostEqual(lost.LostEnergyKWh, 65.0, 1e-4) {
		t.Errorf("LostEnergyKWh = %f, want 65.0", lost.LostEnergyKWh)
	}
	if !lost.IsEstimate {
		t.Errorf("expected IsEstimate = true")
	}
	if lost.Label == "" {
		t.Errorf("expected non-empty estimate Label")
	}
	if lost.DeviceDaysAffected != 3 {
		t.Errorf("DeviceDaysAffected = %d, want 3", lost.DeviceDaysAffected)
	}
	if lost.DevicesCovered != 1 {
		t.Errorf("DevicesCovered = %d, want 1", lost.DevicesCovered)
	}
}

// 4. Fair device comparison with one device down.
// Devices with Kind "plant" are excluded from fleet comparisons.
func TestAnalytics_CompareDevices_FairWithOneDeviceDown(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	// Create D1, D2, and a device with Kind "plant" (which must be excluded)
	plant, devs := createTestPlant(t, ctx, st, "Plant1", tz, "D1", "D2", "PLANT_TOTAL")
	dev1, dev2, devPlant := devs[0], devs[1], devs[2]

	mrun, _ := st.UpsertModelRun(ctx, store.ModelRun{
		Name:      "ridge",
		Version:   "v1",
		PlantID:   plant.ID,
		TrainedAt: time.Now(),
		Active:    true,
	})
	_ = st.UpsertPredictions(ctx, []store.Prediction{
		{DeviceID: dev1.ID, Date: "2026-04-01", ModelRunID: mrun.ID, KWhExpected: 10.0},
		{DeviceID: dev2.ID, Date: "2026-04-01", ModelRunID: mrun.ID, KWhExpected: 20.0},
		{DeviceID: dev1.ID, Date: "2026-04-02", ModelRunID: mrun.ID, KWhExpected: 10.0},
		{DeviceID: dev2.ID, Date: "2026-04-02", ModelRunID: mrun.ID, KWhExpected: 20.0},
		{DeviceID: dev1.ID, Date: "2026-04-03", ModelRunID: mrun.ID, KWhExpected: 12.0},
		{DeviceID: dev2.ID, Date: "2026-04-03", ModelRunID: mrun.ID, KWhExpected: 24.0},
	})

	// Day 1: D1 ok (10.0), D2 ok (20.0), PLANT_TOTAL ok (30.0) -> FAIR DAY
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-04-01", floatPtr(10.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, dev2.ID, tz, "2026-04-01", floatPtr(20.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, devPlant.ID, tz, "2026-04-01", floatPtr(30.0), domain.QualityOK, "")

	// Day 2: D1 ok (10.0), D2 NOT OK (0.0 suspect) -> EXCLUDED DAY!
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-04-02", floatPtr(10.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, dev2.ID, tz, "2026-04-02", floatPtr(0.0), domain.QualitySuspect, "zero_generation")

	// Day 3: D1 ok (12.0), D2 ok (24.0) -> FAIR DAY
	insertDailyReading(t, ctx, st, dev1.ID, tz, "2026-04-03", floatPtr(12.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, dev2.ID, tz, "2026-04-03", floatPtr(24.0), domain.QualityOK, "")

	cmp, err := analytics.CompareDevices(ctx, st, plant.ID, "2026-04-01", "2026-04-03")
	if err != nil {
		t.Fatalf("CompareDevices error: %v", err)
	}

	// Only 2 fair days (Day 1 and Day 3)
	if cmp.CommonOKDays != 2 {
		t.Errorf("CommonOKDays = %d, want 2", cmp.CommonOKDays)
	}

	// PLANT_TOTAL must be excluded! Exactly 2 devices in comparison.
	if len(cmp.Devices) != 2 {
		t.Fatalf("got %d compared devices, want 2 (excluding plant kind)", len(cmp.Devices))
	}

	var d1Res, d2Res *analytics.DeviceComparison
	for i := range cmp.Devices {
		if cmp.Devices[i].DeviceName == "D1" {
			d1Res = &cmp.Devices[i]
		} else if cmp.Devices[i].DeviceName == "D2" {
			d2Res = &cmp.Devices[i]
		}
	}
	if d1Res == nil || d2Res == nil {
		t.Fatalf("D1 or D2 not found in results: %+v", cmp.Devices)
	}

	// Hand-computed for fair days (Day 1 + Day 3):
	// D1 measured: 10.0 + 12.0 = 22.0 kWh
	// D1 expected: 10.0 + 12.0 = 22.0 kWh
	// D2 measured: 20.0 + 24.0 = 44.0 kWh
	// D2 expected: 20.0 + 24.0 = 44.0 kWh
	if !almostEqual(d1Res.MeasuredKWh, 22.0, 1e-4) {
		t.Errorf("D1 MeasuredKWh = %f, want 22.0", d1Res.MeasuredKWh)
	}
	if !almostEqual(d2Res.MeasuredKWh, 44.0, 1e-4) {
		t.Errorf("D2 MeasuredKWh = %f, want 44.0", d2Res.MeasuredKWh)
	}

	// Peer median:
	// For D1, peer is D2 (44.0) -> ratio = 22.0 / 44.0 = 0.5
	// For D2, peer is D1 (22.0) -> ratio = 44.0 / 22.0 = 2.0
	if d1Res.PeerMedianKWh == nil || !almostEqual(*d1Res.PeerMedianKWh, 44.0, 1e-4) {
		t.Errorf("D1 PeerMedianKWh = %v, want 44.0", d1Res.PeerMedianKWh)
	}
	if d1Res.RatioToPeerMedian == nil || !almostEqual(*d1Res.RatioToPeerMedian, 0.5, 1e-4) {
		t.Errorf("D1 RatioToPeerMedian = %v, want 0.5", d1Res.RatioToPeerMedian)
	}
	if d2Res.PeerMedianKWh == nil || !almostEqual(*d2Res.PeerMedianKWh, 22.0, 1e-4) {
		t.Errorf("D2 PeerMedianKWh = %v, want 22.0", d2Res.PeerMedianKWh)
	}
	if d2Res.RatioToPeerMedian == nil || !almostEqual(*d2Res.RatioToPeerMedian, 2.0, 1e-4) {
		t.Errorf("D2 RatioToPeerMedian = %v, want 2.0", d2Res.RatioToPeerMedian)
	}
}

// 5. Two plants isolated.
func TestAnalytics_TwoPlantsIsolated(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant1, devs1 := createTestPlant(t, ctx, st, "Plant1", tz, "D1")
	plant2, devs2 := createTestPlant(t, ctx, st, "Plant2", tz, "D1")

	insertDailyReading(t, ctx, st, devs1[0].ID, tz, "2026-05-01", floatPtr(100.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, devs2[0].ID, tz, "2026-05-01", floatPtr(500.0), domain.QualityOK, "")

	gen1, err := analytics.Generation(ctx, st, plant1.ID, nil, "2026-05-01", "2026-05-01", "day")
	if err != nil {
		t.Fatalf("plant 1 generation: %v", err)
	}
	if !almostEqual(gen1.TotalMeasuredKWh, 100.0, 1e-4) {
		t.Errorf("plant 1 TotalMeasuredKWh = %f, want 100.0", gen1.TotalMeasuredKWh)
	}

	gen2, err := analytics.Generation(ctx, st, plant2.ID, nil, "2026-05-01", "2026-05-01", "day")
	if err != nil {
		t.Fatalf("plant 2 generation: %v", err)
	}
	if !almostEqual(gen2.TotalMeasuredKWh, 500.0, 1e-4) {
		t.Errorf("plant 2 TotalMeasuredKWh = %f, want 500.0", gen2.TotalMeasuredKWh)
	}
}

// 6. Two time zones.
func TestAnalytics_TwoTimeZones(t *testing.T) {
	st, ctx := setupTestDB(t)
	// Plant 1 in UTC-3
	plantSP, devsSP := createTestPlant(t, ctx, st, "PlantSP", "America/Sao_Paulo", "D1")
	// Plant 2 in UTC
	plantUTC, devsUTC := createTestPlant(t, ctx, st, "PlantUTC", "UTC", "D1")

	dateStr := "2026-06-01"
	insertDailyReading(t, ctx, st, devsSP[0].ID, "America/Sao_Paulo", dateStr, floatPtr(15.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, devsUTC[0].ID, "UTC", dateStr, floatPtr(25.0), domain.QualityOK, "")

	genSP, err := analytics.Generation(ctx, st, plantSP.ID, nil, dateStr, dateStr, "day")
	if err != nil {
		t.Fatalf("gen SP: %v", err)
	}
	if !almostEqual(genSP.TotalMeasuredKWh, 15.0, 1e-4) {
		t.Errorf("gen SP TotalMeasuredKWh = %f, want 15.0", genSP.TotalMeasuredKWh)
	}

	genUTC, err := analytics.Generation(ctx, st, plantUTC.ID, nil, dateStr, dateStr, "day")
	if err != nil {
		t.Fatalf("gen UTC: %v", err)
	}
	if !almostEqual(genUTC.TotalMeasuredKWh, 25.0, 1e-4) {
		t.Errorf("gen UTC TotalMeasuredKWh = %f, want 25.0", genUTC.TotalMeasuredKWh)
	}
}

// 7. DataQuality: percent ok per device and reasons count.
func TestAnalytics_DataQuality(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant1", tz, "D1", "D2")
	d1, d2 := devs[0], devs[1]

	// 4 days: 2026-07-01 to 2026-07-04
	// D1:
	// Day 1: ok
	// Day 2: ok
	// Day 3: suspect ("stuck_value")
	// Day 4: missing
	// D1: 2 OK out of 4 days = 50.0%
	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-07-01", floatPtr(10.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-07-02", floatPtr(10.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-07-03", floatPtr(10.0), domain.QualitySuspect, "stuck_value")

	// D2:
	// Day 1: ok
	// Day 2: ok
	// Day 3: ok
	// Day 4: invalid ("implausible_reading")
	// D2: 3 OK out of 4 days = 75.0%
	insertDailyReading(t, ctx, st, d2.ID, tz, "2026-07-01", floatPtr(20.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d2.ID, tz, "2026-07-02", floatPtr(20.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d2.ID, tz, "2026-07-03", floatPtr(20.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d2.ID, tz, "2026-07-04", floatPtr(-5.0), domain.QualityInvalid, "implausible_reading")

	dq, err := analytics.DataQuality(ctx, st, plant.ID, "2026-07-01", "2026-07-04")
	if err != nil {
		t.Fatalf("DataQuality error: %v", err)
	}

	if len(dq.Devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(dq.Devices))
	}

	var d1Q, d2Q *analytics.DeviceDataQuality
	for i := range dq.Devices {
		if dq.Devices[i].DeviceName == "D1" {
			d1Q = &dq.Devices[i]
		} else if dq.Devices[i].DeviceName == "D2" {
			d2Q = &dq.Devices[i]
		}
	}
	if d1Q == nil || d2Q == nil {
		t.Fatalf("D1 or D2 not found in DQ: %+v", dq.Devices)
	}

	if !almostEqual(d1Q.PercentOK, 50.0, 1e-4) {
		t.Errorf("D1 PercentOK = %f, want 50.0", d1Q.PercentOK)
	}
	if d1Q.ReasonsCount["stuck_value"] != 1 {
		t.Errorf("D1 reasons[stuck_value] = %d, want 1", d1Q.ReasonsCount["stuck_value"])
	}
	if d1Q.MissingDays != 1 {
		t.Errorf("D1 MissingDays = %d, want 1", d1Q.MissingDays)
	}

	if !almostEqual(d2Q.PercentOK, 75.0, 1e-4) {
		t.Errorf("D2 PercentOK = %f, want 75.0", d2Q.PercentOK)
	}
	if d2Q.ReasonsCount["implausible_reading"] != 1 {
		t.Errorf("D2 reasons[implausible_reading] = %d, want 1", d2Q.ReasonsCount["implausible_reading"])
	}
}

// 8. Granularity month aggregation.
func TestAnalytics_GranularityMonth(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant1", tz, "D1")
	d1 := devs[0]

	// 2 days in Jan 2026, 1 day in Feb 2026
	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-01-10", floatPtr(10.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-01-11", floatPtr(20.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-02-05", floatPtr(15.0), domain.QualityOK, "")

	gen, err := analytics.Generation(ctx, st, plant.ID, &d1.ID, "2026-01-10", "2026-02-05", "month")
	if err != nil {
		t.Fatalf("Generation month error: %v", err)
	}

	if !almostEqual(gen.TotalMeasuredKWh, 45.0, 1e-4) {
		t.Errorf("TotalMeasuredKWh = %f, want 45.0", gen.TotalMeasuredKWh)
	}

	// Should have 2 month points: "2026-01" and "2026-02"
	if len(gen.Points) != 2 {
		t.Fatalf("got %d points, want 2", len(gen.Points))
	}
	if gen.Points[0].Period != "2026-01" || !almostEqual(gen.Points[0].MeasuredKWh, 30.0, 1e-4) {
		t.Errorf("Jan point mismatch: %+v", gen.Points[0])
	}
	if gen.Points[1].Period != "2026-02" || !almostEqual(gen.Points[1].MeasuredKWh, 15.0, 1e-4) {
		t.Errorf("Feb point mismatch: %+v", gen.Points[1])
	}
}

// 9. ExpectedVsActual and CompareDevices must pair per device-day.
// TestExpectedTotalsArePaired with hand-computed values, including a device with no prediction
// for the first days and a device-day not ok.
func TestExpectedTotalsArePaired(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "PlantPairing", tz, "D1", "D2")
	d1, d2 := devs[0], devs[1]

	mrun, _ := st.UpsertModelRun(ctx, store.ModelRun{
		Name:      "ridge",
		Version:   "v1",
		PlantID:   plant.ID,
		TrainedAt: time.Now(),
		Active:    true,
	})

	// Predictions:
	// D1 has NO prediction on Day 1 (2026-08-01)
	// D1 has prediction on Day 2 (2026-08-02): 10.0 kWh
	// D1 has prediction on Day 3 (2026-08-03): 15.0 kWh
	// D2 has prediction on Day 1 (2026-08-01): 20.0 kWh
	// D2 has prediction on Day 2 (2026-08-02): 20.0 kWh
	// D2 has prediction on Day 3 (2026-08-03): 20.0 kWh
	_ = st.UpsertPredictions(ctx, []store.Prediction{
		{DeviceID: d1.ID, Date: "2026-08-02", ModelRunID: mrun.ID, KWhExpected: 10.0},
		{DeviceID: d1.ID, Date: "2026-08-03", ModelRunID: mrun.ID, KWhExpected: 15.0},
		{DeviceID: d2.ID, Date: "2026-08-01", ModelRunID: mrun.ID, KWhExpected: 20.0},
		{DeviceID: d2.ID, Date: "2026-08-02", ModelRunID: mrun.ID, KWhExpected: 20.0},
		{DeviceID: d2.ID, Date: "2026-08-03", ModelRunID: mrun.ID, KWhExpected: 20.0},
	})

	// Readings:
	// Day 1 (2026-08-01):
	//   D1: ok, 12.0 kWh (unpaired: ok reading, but NO prediction)
	//   D2: ok, 18.0 kWh (paired with 20.0 expected)
	// Day 2 (2026-08-02):
	//   D1: ok, 10.0 kWh (paired with 10.0 expected)
	//   D2: NOT OK: suspect, 0.0 kWh (not paired! device not ok)
	// Day 3 (2026-08-03):
	//   D1: ok, 14.0 kWh (paired with 15.0 expected)
	//   D2: ok, 22.0 kWh (paired with 20.0 expected)
	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-08-01", floatPtr(12.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d2.ID, tz, "2026-08-01", floatPtr(18.0), domain.QualityOK, "")

	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-08-02", floatPtr(10.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d2.ID, tz, "2026-08-02", floatPtr(0.0), domain.QualitySuspect, "zero_generation")

	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-08-03", floatPtr(14.0), domain.QualityOK, "")
	insertDailyReading(t, ctx, st, d2.ID, tz, "2026-08-03", floatPtr(22.0), domain.QualityOK, "")

	res, err := analytics.ExpectedVsActual(ctx, st, plant.ID, nil, "2026-08-01", "2026-08-03")
	if err != nil {
		t.Fatalf("ExpectedVsActual: %v", err)
	}

	// Hand-computed paired device-days:
	// - D2 Day 1: m=18.0, e=20.0
	// - D1 Day 2: m=10.0, e=10.0
	// - D1 Day 3: m=14.0, e=15.0
	// - D2 Day 3: m=22.0, e=20.0
	// Paired totals:
	// PairedMeasuredKWh = 18.0 + 10.0 + 14.0 + 22.0 = 64.0 kWh
	// PairedExpectedKWh = 20.0 + 10.0 + 15.0 + 20.0 = 65.0 kWh
	// PairedDeviceDays = 4
	// PairedRatio = 64.0 / 65.0 = 0.984615...
	// Unpaired (ok reading with no prediction):
	// - D1 Day 1: m=12.0 kWh, e=0
	// UnpairedMeasuredKWh = 12.0 kWh
	// UnpairedDeviceDays = 1
	// Total measured on all ok device-days = 64.0 + 12.0 = 76.0 kWh
	if res.PairedDeviceDays != 4 {
		t.Errorf("PairedDeviceDays = %d, want 4", res.PairedDeviceDays)
	}
	if !almostEqual(res.PairedMeasuredKWh, 64.0, 1e-4) {
		t.Errorf("PairedMeasuredKWh = %f, want 64.0", res.PairedMeasuredKWh)
	}
	if !almostEqual(res.PairedExpectedKWh, 65.0, 1e-4) {
		t.Errorf("PairedExpectedKWh = %f, want 65.0", res.PairedExpectedKWh)
	}
	if res.PairedRatio == nil || !almostEqual(*res.PairedRatio, 64.0/65.0, 1e-4) {
		t.Errorf("PairedRatio = %v, want %f", res.PairedRatio, 64.0/65.0)
	}
	if !almostEqual(res.UnpairedMeasuredKWh, 12.0, 1e-4) {
		t.Errorf("UnpairedMeasuredKWh = %f, want 12.0", res.UnpairedMeasuredKWh)
	}
	if res.UnpairedDeviceDays != 1 {
		t.Errorf("UnpairedDeviceDays = %d, want 1", res.UnpairedDeviceDays)
	}
	if !almostEqual(res.TotalMeasuredKWh, 76.0, 1e-4) {
		t.Errorf("TotalMeasuredKWh = %f, want 76.0", res.TotalMeasuredKWh)
	}

	// Day 2 check: D2 was not ok, so Day 2 dayExpected must NOT include D2's prediction!
	// D1 expected on Day 2 is 10.0. D2 is not ok, so dayExpected must be 10.0 (not 30.0).
	if len(res.Days) >= 2 {
		day2 := res.Days[1]
		if !almostEqual(day2.ExpectedKWh, 10.0, 1e-4) {
			t.Errorf("Day 2 ExpectedKWh = %f, want 10.0 (excluding device that was not ok)", day2.ExpectedKWh)
		}
	}
}

// 10. TestLostEnergySeparatesDaysWithoutExpected: separates device-days not ok or missing
// WITH an expected value from those WITHOUT one.
func TestLostEnergySeparatesDaysWithoutExpected(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "PlantLostSep", tz, "D1")
	d1 := devs[0]

	mrun, _ := st.UpsertModelRun(ctx, store.ModelRun{
		Name:      "ridge",
		Version:   "v1",
		PlantID:   plant.ID,
		TrainedAt: time.Now(),
		Active:    true,
	})

	// Predictions:
	// Day 1 (2026-09-01): NO PREDICTION
	// Day 2 (2026-09-02): 15.0 kWh expected
	// Day 3 (2026-09-03): 12.0 kWh expected
	_ = st.UpsertPredictions(ctx, []store.Prediction{
		{DeviceID: d1.ID, Date: "2026-09-02", ModelRunID: mrun.ID, KWhExpected: 15.0},
		{DeviceID: d1.ID, Date: "2026-09-03", ModelRunID: mrun.ID, KWhExpected: 12.0},
	})

	// Readings:
	// Day 1: suspect, no prediction (problematic WITHOUT expected)
	// Day 2: missing (no reading in DB), has 15.0 expected (problematic WITH expected)
	// Day 3: ok (not problematic)
	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-09-01", floatPtr(5.0), domain.QualitySuspect, "zero_generation")
	insertDailyReading(t, ctx, st, d1.ID, tz, "2026-09-03", floatPtr(12.0), domain.QualityOK, "")

	lost, err := analytics.LostEnergy(ctx, st, plant.ID, "2026-09-01", "2026-09-03")
	if err != nil {
		t.Fatalf("LostEnergy: %v", err)
	}

	// Total problematic device-days: 2
	// Problematic with expected: 1 (Day 2)
	// Problematic without expected: 1 (Day 1)
	// Lost energy sum: 15.0 kWh
	if lost.DeviceDaysAffected != 2 {
		t.Errorf("DeviceDaysAffected = %d, want 2", lost.DeviceDaysAffected)
	}
	if lost.DeviceDaysWithExpected != 1 {
		t.Errorf("DeviceDaysWithExpected = %d, want 1", lost.DeviceDaysWithExpected)
	}
	if lost.DeviceDaysWithoutExpected != 1 {
		t.Errorf("DeviceDaysWithoutExpected = %d, want 1", lost.DeviceDaysWithoutExpected)
	}
	if !almostEqual(lost.LostEnergyKWh, 15.0, 1e-4) {
		t.Errorf("LostEnergyKWh = %f, want 15.0", lost.LostEnergyKWh)
	}
}
