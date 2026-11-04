package forecast_test

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/forecast"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func setupTestDB(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "solarguard_test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// helper to populate synthetic days for a plant
func seedPlantData(t *testing.T, st *store.Store, plantID int64, devIDs []int64, numDays int, corruptDay int, corruptVal float64) {
	t.Helper()
	ctx := context.Background()
	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		t.Fatalf("get plant: %v", err)
	}

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	startDate := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	var weatherDays []connector.WeatherDay
	var readings []domain.Reading

	for i := 0; i < numDays; i++ {
		curDate := startDate.AddDate(0, 0, i)
		dateStr := curDate.Format(time.DateOnly)

		irr := 3.0 + 2.0*math.Sin(float64(i)*0.05)
		tmean := 22.0 + 5.0*math.Cos(float64(i)*0.03)
		tmax := tmean + 4.0
		precip := 0.0
		hum := 65.0

		weatherDays = append(weatherDays, connector.WeatherDay{
			Date:             dateStr,
			IrradiationKWhM2: &irr,
			TemperatureMeanC: &tmean,
			TemperatureMaxC:  &tmax,
			PrecipitationMM:  &precip,
			HumidityMeanPct:  &hum,
		})

		startUTC := curDate.UTC()
		endUTC := curDate.AddDate(0, 0, 1).UTC()

		for devIdx, devID := range devIDs {
			k := 8.0 + float64(devIdx)*0.5
			val := k * irr
			q := domain.QualityOK
			reason := "ok"

			if i == corruptDay {
				val = corruptVal
				q = domain.QualityInvalid
				reason = "spike"
			}

			readings = append(readings, domain.Reading{
				DeviceID:      devID,
				Metric:        domain.MetricEnergyKWh,
				PeriodStart:   startUTC,
				PeriodEnd:     endUTC,
				Value:         &val,
				RawValue:      fmt.Sprintf("%.2f", val),
				Quality:       q,
				QualityReason: reason,
			})
		}
	}

	if plant.WeatherLocationID != nil {
		if err := st.UpsertWeatherDays(ctx, *plant.WeatherLocationID, "observed", "inmet", weatherDays); err != nil {
			t.Fatalf("seed weather: %v", err)
		}
	}
	if _, err := st.InsertReadings(ctx, readings); err != nil {
		t.Fatalf("seed readings: %v", err)
	}
}

func TestNoFutureLeakage(t *testing.T) {
	// Test proving that changing a future y does not alter an earlier prediction.
	ctx := context.Background()
	st1 := setupTestDB(t)
	st2 := setupTestDB(t)

	setupPlant := func(st *store.Store, futureVal float64) (*forecast.BacktestResult, error) {
		src, _ := st.EnsureSource(ctx, domain.Source{Name: "src", Kind: "test"})
		wloc, _ := st.EnsureWeatherLocation(ctx, store.WeatherLocation{Name: "loc", Timezone: "America/Sao_Paulo"})
		area := 42.0
		eff := 0.18
		plant, _ := st.UpsertPlant(ctx, domain.Plant{
			SourceID:          src.ID,
			ExternalID:        "P1",
			Name:              "Plant 1",
			Timezone:          "America/Sao_Paulo",
			WeatherLocationID: &wloc.ID,
		})
		dev, _ := st.UpsertDevice(ctx, domain.Device{
			PlantID:     plant.ID,
			ExternalID:  "D1",
			Name:        "D1",
			PanelAreaM2: &area,
			Efficiency:  &eff,
		})

		seedPlantData(t, st, plant.ID, []int64{dev.ID}, 80, -1, 0)

		// In st2, alter a future y (e.g. day 75)
		if futureVal > 0 {
			loc, _ := time.LoadLocation("America/Sao_Paulo")
			d75 := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).AddDate(0, 0, 75).UTC()
			_, _ = st.DB().ExecContext(ctx, `UPDATE readings SET value = ? WHERE device_id = ? AND period_start = ?`, futureVal, dev.ID, d75.Format(time.RFC3339))
		}

		return forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{
			MinTrainDays: 60,
			RefitDays:    7,
		})
	}

	res1, err := setupPlant(st1, 0)
	if err != nil {
		t.Fatalf("backtest 1: %v", err)
	}
	res2, err := setupPlant(st2, 999.0) // radically different future value on day 75
	if err != nil {
		t.Fatalf("backtest 2: %v", err)
	}

	// Compare predictions for day 60..70 (earlier than day 75).
	// They must be bitwise or within float epsilon identical!
	preds1 := res1.DeviceDayPredictions["D1"]
	preds2 := res2.DeviceDayPredictions["D1"]

	for date, p1 := range preds1 {
		if date < "2026-03-15" { // well before day 75
			p2, ok := preds2[date]
			if !ok {
				t.Fatalf("missing date %s in run 2", date)
			}
			for modelName, v1 := range p1 {
				v2 := p2[modelName]
				if math.Abs(v1-v2) > 1e-9 {
					t.Errorf("future leakage detected on date %s for model %s: %f != %f", date, modelName, v1, v2)
				}
			}
		}
	}
}

func TestQualityFilterIgnoresSuspectAndInvalid(t *testing.T) {
	// A suspect or invalid value with a huge number must not change anything.
	ctx := context.Background()
	st1 := setupTestDB(t)
	st2 := setupTestDB(t)

	runTest := func(st *store.Store, corrupt bool) *forecast.BacktestResult {
		src, _ := st.EnsureSource(ctx, domain.Source{Name: "src", Kind: "test"})
		wloc, _ := st.EnsureWeatherLocation(ctx, store.WeatherLocation{Name: "loc", Timezone: "America/Sao_Paulo"})
		area := 42.0
		eff := 0.18
		plant, _ := st.UpsertPlant(ctx, domain.Plant{
			SourceID:          src.ID,
			ExternalID:        "P1",
			Name:              "Plant 1",
			Timezone:          "America/Sao_Paulo",
			WeatherLocationID: &wloc.ID,
		})
		dev, _ := st.UpsertDevice(ctx, domain.Device{
			PlantID:     plant.ID,
			ExternalID:  "D1",
			Name:        "D1",
			PanelAreaM2: &area,
			Efficiency:  &eff,
		})

		corruptDay := -1
		corruptVal := 0.0
		if corrupt {
			corruptDay = 30 // inside initial 60 days
			corruptVal = 9999999.0
		}

		seedPlantData(t, st, plant.ID, []int64{dev.ID}, 80, corruptDay, corruptVal)

		res, err := forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{
			MinTrainDays: 60,
			RefitDays:    7,
		})
		if err != nil {
			t.Fatalf("backtest: %v", err)
		}
		return res
	}

	cleanRes := runTest(st1, false)
	corruptRes := runTest(st2, true)

	// Since row 30 in corruptRes was marked QualityInvalid, it was excluded.
	// Clean has 80 rows, corrupt has 79 ok rows.
	// Ridge and calibrated on corrupt should produce completely reasonable metrics (no huge MAE)
	mClean := cleanRes.WalkForwardPooled["calibrated"]
	mCorrupt := corruptRes.WalkForwardPooled["calibrated"]

	if mCorrupt.MAE > 10.0 {
		t.Errorf("corrupt invalid spike leaked into training! MAE is %f", mCorrupt.MAE)
	}
	t.Logf("clean MAE: %f, corrupt MAE: %f", mClean.MAE, mCorrupt.MAE)
}

func TestAllModelsComparedOnSameDays(t *testing.T) {
	// All models are compared on exactly the same evaluation days per device: the intersection, with n reported.
	ctx := context.Background()
	st := setupTestDB(t)

	src, _ := st.EnsureSource(ctx, domain.Source{Name: "src", Kind: "test"})
	wloc, _ := st.EnsureWeatherLocation(ctx, store.WeatherLocation{Name: "loc", Timezone: "America/Sao_Paulo"})
	area := 42.0
	eff := 0.18
	plant, _ := st.UpsertPlant(ctx, domain.Plant{
		SourceID:          src.ID,
		ExternalID:        "P1",
		Name:              "Plant 1",
		Timezone:          "America/Sao_Paulo",
		WeatherLocationID: &wloc.ID,
	})
	dev, _ := st.UpsertDevice(ctx, domain.Device{
		PlantID:     plant.ID,
		ExternalID:  "D1",
		Name:        "D1",
		PanelAreaM2: &area,
		Efficiency:  &eff,
	})

	seedPlantData(t, st, plant.ID, []int64{dev.ID}, 80, -1, 0)

	res, err := forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{
		MinTrainDays: 60,
		RefitDays:    7,
	})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	// Verify all models have identical N in walk-forward evaluation
	var expectedN int
	first := true
	for modelName, m := range res.WalkForwardPooled {
		if first {
			expectedN = m.N
			first = false
		} else if m.N != expectedN {
			t.Errorf("model %s has N=%d, expected intersection N=%d", modelName, m.N, expectedN)
		}
	}
	if expectedN == 0 {
		t.Errorf("expected non-zero evaluated days N")
	}
}

func TestSelectionChoosesLowestMAEAndRejectsIfLosesToPersistence(t *testing.T) {
	ctx := context.Background()
	st := setupTestDB(t)

	src, _ := st.EnsureSource(ctx, domain.Source{Name: "src", Kind: "test"})
	wloc, _ := st.EnsureWeatherLocation(ctx, store.WeatherLocation{Name: "loc", Timezone: "America/Sao_Paulo"})
	area := 42.0
	eff := 0.18
	plant, _ := st.UpsertPlant(ctx, domain.Plant{
		SourceID:          src.ID,
		ExternalID:        "P1",
		Name:              "Plant 1",
		Timezone:          "America/Sao_Paulo",
		WeatherLocationID: &wloc.ID,
	})
	dev, _ := st.UpsertDevice(ctx, domain.Device{
		PlantID:     plant.ID,
		ExternalID:  "D1",
		Name:        "D1",
		PanelAreaM2: &area,
		Efficiency:  &eff,
	})

	seedPlantData(t, st, plant.ID, []int64{dev.ID}, 80, -1, 0)

	res, err := forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{
		MinTrainDays: 60,
		RefitDays:    7,
	})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if res.ActiveModel == "" {
		t.Fatalf("expected an active model to be selected")
	}
	activeMetric := res.WalkForwardPooled[res.ActiveModel]
	persistMetric := res.WalkForwardPooled["persistence"]

	if activeMetric.MAE >= persistMetric.MAE {
		t.Errorf("active model MAE %f must beat persistence MAE %f", activeMetric.MAE, persistMetric.MAE)
	}

	activeRun, err := st.GetActiveModelRun(ctx, plant.ID)
	if err != nil {
		t.Fatalf("get active model run from store: %v", err)
	}
	if activeRun.Name != res.ActiveModel {
		t.Errorf("store active model %s != res active model %s", activeRun.Name, res.ActiveModel)
	}
}

func TestPlantsIsolation1And5DevicesAndMissingWeather(t *testing.T) {
	ctx := context.Background()
	st := setupTestDB(t)
	src, _ := st.EnsureSource(ctx, domain.Source{Name: "src", Kind: "test"})
	wloc, _ := st.EnsureWeatherLocation(ctx, store.WeatherLocation{Name: "loc", Timezone: "America/Sao_Paulo"})

	// Plant 1: 1 device
	p1, _ := st.UpsertPlant(ctx, domain.Plant{
		SourceID:          src.ID,
		ExternalID:        "P1",
		Name:              "Plant 1",
		Timezone:          "America/Sao_Paulo",
		WeatherLocationID: &wloc.ID,
	})
	d1, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p1.ID, ExternalID: "D1", Name: "D1"})
	seedPlantData(t, st, p1.ID, []int64{d1.ID}, 80, -1, 0)

	// Plant 2: 5 devices
	p2, _ := st.UpsertPlant(ctx, domain.Plant{
		SourceID:          src.ID,
		ExternalID:        "P2",
		Name:              "Plant 2",
		Timezone:          "America/Sao_Paulo",
		WeatherLocationID: &wloc.ID,
	})
	var p2Devs []int64
	for i := 1; i <= 5; i++ {
		d, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p2.ID, ExternalID: fmt.Sprintf("D%d", i), Name: fmt.Sprintf("Dev %d", i)})
		p2Devs = append(p2Devs, d.ID)
	}
	seedPlantData(t, st, p2.ID, p2Devs, 80, -1, 0)

	// Plant 3: No weather link
	p3, _ := st.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "P3",
		Name:       "Plant 3 (No Weather)",
		Timezone:   "America/Sao_Paulo",
	})
	d3, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p3.ID, ExternalID: "D3", Name: "D3"})
	seedPlantData(t, st, p3.ID, []int64{d3.ID}, 80, -1, 0)

	// Plant 4: Device with too few rows (10 rows < 60)
	p4, _ := st.UpsertPlant(ctx, domain.Plant{
		SourceID:          src.ID,
		ExternalID:        "P4",
		Name:              "Plant 4 (Too few rows)",
		Timezone:          "America/Sao_Paulo",
		WeatherLocationID: &wloc.ID,
	})
	d4, _ := st.UpsertDevice(ctx, domain.Device{PlantID: p4.ID, ExternalID: "D4", Name: "D4"})
	seedPlantData(t, st, p4.ID, []int64{d4.ID}, 10, -1, 0)

	// Run backtests:
	// P1 works
	res1, err := forecast.Backtest(ctx, st, p1.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})
	if err != nil {
		t.Fatalf("p1 backtest: %v", err)
	}
	if res1 == nil {
		t.Fatalf("expected res1")
	}

	// P2 works with 5 devices
	res2, err := forecast.Backtest(ctx, st, p2.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})
	if err != nil {
		t.Fatalf("p2 backtest: %v", err)
	}
	if len(res2.WalkForwardDevices) != 5 {
		t.Errorf("expected 5 devices in p2, got %d", len(res2.WalkForwardDevices))
	}

	// P3 reported, not error (no weather link)
	res3, err := forecast.Backtest(ctx, st, p3.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})
	if err != nil {
		t.Fatalf("p3 should not return error, got %v", err)
	}
	if res3.Message == "" {
		t.Errorf("expected warning message about no weather link")
	}

	// P4 reported, not error (too few rows)
	res4, err := forecast.Backtest(ctx, st, p4.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})
	if err != nil {
		t.Fatalf("p4 should not return error, got %v", err)
	}
	if res4.Message == "" {
		t.Errorf("expected warning message about too few rows")
	}
}
