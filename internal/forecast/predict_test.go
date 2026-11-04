package forecast_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/forecast"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func TestPredictIdempotentAndHonestWalkForward(t *testing.T) {
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

	// Add 3 forecast weather days
	irr := 4.5
	tmean := 26.0
	precip := 0.0
	hum := 60.0
	forecastDays := []connector.WeatherDay{
		{Date: "2026-03-23", IrradiationKWhM2: &irr, TemperatureMeanC: &tmean, PrecipitationMM: &precip, HumidityMeanPct: &hum},
		{Date: "2026-03-24", IrradiationKWhM2: &irr, TemperatureMeanC: &tmean, PrecipitationMM: &precip, HumidityMeanPct: &hum},
		{Date: "2026-03-25", IrradiationKWhM2: &irr, TemperatureMeanC: &tmean, PrecipitationMM: &precip, HumidityMeanPct: &hum},
	}
	if err := st.UpsertWeatherDays(ctx, wloc.ID, "forecast", "open-meteo", forecastDays); err != nil {
		t.Fatalf("upsert forecast weather: %v", err)
	}

	// 1. Run backtest first
	btRes, err := forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if btRes.ActiveModel == "" {
		t.Fatalf("expected active model")
	}

	// 2. Run predict
	predRes1, err := forecast.Predict(ctx, st, plant.ID, forecast.PredictOpts{Days: 7})
	if err != nil {
		t.Fatalf("predict run 1: %v", err)
	}
	if predRes1 == nil || len(predRes1.Predictions) == 0 {
		t.Fatalf("expected predictions")
	}

	// Verify honest expected values:
	// For a date inside walk-forward history (e.g. 2026-03-05), the expected value must match the walk-forward prediction!
	wfPreds := btRes.DeviceDayPredictions["D1"]
	for _, p := range predRes1.Predictions {
		if wfVal, ok := wfPreds[p.Date][btRes.ActiveModel]; ok {
			if p.KWhExpected != wfVal {
				t.Errorf("date %s expected value %f != walk-forward out-of-fold prediction %f", p.Date, p.KWhExpected, wfVal)
			}
		}
		// Verify bands: kwh_lower <= kwh_expected <= kwh_upper, clamped at 0
		if p.KWhLower != nil && *p.KWhLower < 0 {
			t.Errorf("kwh_lower %f < 0", *p.KWhLower)
		}
		if p.KWhLower != nil && *p.KWhLower > p.KWhExpected {
			t.Errorf("kwh_lower %f > kwh_expected %f", *p.KWhLower, p.KWhExpected)
		}
		if p.KWhUpper != nil && *p.KWhUpper < p.KWhExpected {
			t.Errorf("kwh_upper %f < kwh_expected %f", *p.KWhUpper, p.KWhExpected)
		}
	}

	// 3. Re-run predict: must be idempotent!
	predRes2, err := forecast.Predict(ctx, st, plant.ID, forecast.PredictOpts{Days: 7})
	if err != nil {
		t.Fatalf("predict run 2: %v", err)
	}
	if len(predRes2.Predictions) != len(predRes1.Predictions) {
		t.Errorf("prediction count changed on rerun: %d != %d", len(predRes2.Predictions), len(predRes1.Predictions))
	}

	// Check stored predictions count in DB
	storedPreds, err := st.ListPredictions(ctx, plant.ID, nil, "2026-01-01", "2026-12-31")
	if err != nil {
		t.Fatalf("list stored predictions: %v", err)
	}
	if len(storedPreds) != len(predRes1.Predictions) {
		t.Errorf("stored predictions count %d != predicted count %d", len(storedPreds), len(predRes1.Predictions))
	}
}

func TestNoActiveModelWritesNoPredictions(t *testing.T) {
	ctx := context.Background()
	st := setupTestDB(t)

	// Plant with no model runs
	src, _ := st.EnsureSource(ctx, domain.Source{Name: "src", Kind: "test"})
	plant, _ := st.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "P1",
		Name:       "Plant 1",
		Timezone:   "America/Sao_Paulo",
	})

	res, err := forecast.Predict(ctx, st, plant.ID, forecast.PredictOpts{Days: 7})
	if err != nil {
		t.Fatalf("predict without active model should not return error, got %v", err)
	}
	if res != nil && len(res.Predictions) > 0 {
		t.Errorf("expected no predictions written when no active model, got %d", len(res.Predictions))
	}
}

// 1. In-sample values in history: dates before walk-forward starts (first 60 ok rows) must get NO prediction row.
func TestNoPredictionBeforeEnoughHistory(t *testing.T) {
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

	// 80 days total: Day 0 (2026-01-01) to Day 79 (2026-03-21)
	seedPlantData(t, st, plant.ID, []int64{dev.ID}, 80, -1, 0)

	_, err := forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	predRes, err := forecast.Predict(ctx, st, plant.ID, forecast.PredictOpts{Days: 7})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}

	// Dates for the first 60 days (2026-01-01 through 2026-03-01) must have NO prediction!
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	startDate := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)

	for i := 0; i < 60; i++ {
		dateStr := startDate.AddDate(0, 0, i).Format(time.DateOnly)
		for _, p := range predRes.Predictions {
			if p.Date == dateStr {
				t.Fatalf("date %s before enough history has in-sample prediction %f; expected no prediction", dateStr, p.KWhExpected)
			}
		}
	}
}

// 2. Failed days have no expected value.
// A device with a long invalid run gets values for all those days once it has enough history.
// Specifically, a device with between 30 and 59 ok rows before failure (e.g. 45 ok rows)
// gets expected values for failure days with MinRowsForFailureDayExpected = 30.
func TestExpectedExistsDuringFailurePeriod(t *testing.T) {
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
	// D1 has 45 ok rows (between 30 and 59) followed by 25 failure days
	dev1, _ := st.UpsertDevice(ctx, domain.Device{
		PlantID:     plant.ID,
		ExternalID:  "D1",
		Name:        "D1",
		PanelAreaM2: &area,
		Efficiency:  &eff,
	})
	// D2 has 80 ok rows so Backtest evaluates walk-forward and selects an active model for the plant
	dev2, _ := st.UpsertDevice(ctx, domain.Device{
		PlantID:     plant.ID,
		ExternalID:  "D2",
		Name:        "D2",
		PanelAreaM2: &area,
		Efficiency:  &eff,
	})

	loc, _ := time.LoadLocation("America/Sao_Paulo")
	startDate := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)

	var weatherDays []connector.WeatherDay
	var readings []domain.Reading

	for i := 0; i < 80; i++ {
		curDate := startDate.AddDate(0, 0, i)
		dateStr := curDate.Format(time.DateOnly)

		irr := 3.0 + 2.0*math.Sin(float64(i)*0.05)
		tmean := 22.0 + 5.0*math.Cos(float64(i)*0.03)
		weatherDays = append(weatherDays, connector.WeatherDay{
			Date:             dateStr,
			IrradiationKWhM2: &irr,
			TemperatureMeanC: &tmean,
		})

		startUTC := curDate.UTC()
		endUTC := curDate.AddDate(0, 0, 1).UTC()

		// D2 has ok readings for all 80 days
		valD2 := 8.0 * irr
		readings = append(readings, domain.Reading{
			DeviceID:      dev2.ID,
			Metric:        domain.MetricEnergyKWh,
			PeriodStart:   startUTC,
			PeriodEnd:     endUTC,
			Value:         &valD2,
			RawValue:      fmt.Sprintf("%.2f", valD2),
			Quality:       domain.QualityOK,
			QualityReason: "ok",
		})

		// D1 has 45 ok days (Day 0..44) followed by 25 invalid days (Day 45..69)
		if i < 70 {
			valD1 := 8.0 * irr
			qD1 := domain.QualityOK
			reasonD1 := "ok"
			if i >= 45 {
				valD1 = 0.0
				qD1 = domain.QualityInvalid
				reasonD1 = "stuck_zero"
			}
			readings = append(readings, domain.Reading{
				DeviceID:      dev1.ID,
				Metric:        domain.MetricEnergyKWh,
				PeriodStart:   startUTC,
				PeriodEnd:     endUTC,
				Value:         &valD1,
				RawValue:      fmt.Sprintf("%.2f", valD1),
				Quality:       qD1,
				QualityReason: reasonD1,
			})
		}
	}

	_ = st.UpsertWeatherDays(ctx, wloc.ID, "observed", "inmet", weatherDays)
	_, _ = st.InsertReadings(ctx, readings)

	btRes, err := forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if btRes.ActiveModel == "" {
		t.Fatalf("expected plant to have active model selected via D2")
	}

	predRes, err := forecast.Predict(ctx, st, plant.ID, forecast.PredictOpts{Days: 7})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}

	// D1 has 45 ok rows (< 60), so D1 must have NO prediction for its first 45 ok days (item 1)
	for i := 0; i < 45; i++ {
		dateStr := startDate.AddDate(0, 0, i).Format(time.DateOnly)
		for _, p := range predRes.Predictions {
			if p.DeviceID == dev1.ID && p.Date == dateStr {
				t.Fatalf("D1 before enough history (day %s) should NOT have prediction, got %f", dateStr, p.KWhExpected)
			}
		}
	}

	// But D1 failure days 45 through 69 (25 days) MUST have predictions, because 45 >= MinRowsForFailureDayExpected (30)!
	for i := 45; i < 70; i++ {
		dateStr := startDate.AddDate(0, 0, i).Format(time.DateOnly)
		found := false
		for _, p := range predRes.Predictions {
			if p.DeviceID == dev1.ID && p.Date == dateStr {
				found = true
				if p.KWhExpected <= 0 {
					t.Errorf("expected positive expected kWh for failure day %s on D1, got %f", dateStr, p.KWhExpected)
				}
				break
			}
		}
		if !found {
			t.Fatalf("missing expected value for failure date %s on D1 (had 45 prior ok rows >= %d)", dateStr, forecast.MinRowsForFailureDayExpected)
		}
	}
}

// 2b. Failure day expected ignores future rows: changing a later ok value does not alter it, nor does the invalid value.
func TestFailureDayExpectedIgnoresFutureRows(t *testing.T) {
	ctx := context.Background()

	setupDB := func(day75Val float64, day85Val float64) float64 {
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

		loc, _ := time.LoadLocation("America/Sao_Paulo")
		startDate := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)

		var weatherDays []connector.WeatherDay
		var readings []domain.Reading

		for i := 0; i < 90; i++ {
			curDate := startDate.AddDate(0, 0, i)
			dateStr := curDate.Format(time.DateOnly)

			irr := 3.0 + 2.0*math.Sin(float64(i)*0.05)
			tmean := 22.0 + 5.0*math.Cos(float64(i)*0.03)
			weatherDays = append(weatherDays, connector.WeatherDay{
				Date:             dateStr,
				IrradiationKWhM2: &irr,
				TemperatureMeanC: &tmean,
			})

			startUTC := curDate.UTC()
			endUTC := curDate.AddDate(0, 0, 1).UTC()

			val := 8.0 * irr
			q := domain.QualityOK
			reason := "ok"

			if i == 75 {
				val = day75Val
				q = domain.QualityInvalid
				reason = "fault"
			} else if i == 85 {
				val = day85Val
			}

			readings = append(readings, domain.Reading{
				DeviceID:      dev.ID,
				Metric:        domain.MetricEnergyKWh,
				PeriodStart:   startUTC,
				PeriodEnd:     endUTC,
				Value:         &val,
				RawValue:      fmt.Sprintf("%.2f", val),
				Quality:       q,
				QualityReason: reason,
			})
		}

		_ = st.UpsertWeatherDays(ctx, wloc.ID, "observed", "inmet", weatherDays)
		_, _ = st.InsertReadings(ctx, readings)

		_, _ = forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})
		predRes, err := forecast.Predict(ctx, st, plant.ID, forecast.PredictOpts{Days: 7})
		if err != nil {
			t.Fatalf("predict: %v", err)
		}

		d75Str := startDate.AddDate(0, 0, 75).Format(time.DateOnly)
		for _, p := range predRes.Predictions {
			if p.Date == d75Str && p.DeviceID == dev.ID {
				return p.KWhExpected
			}
		}
		t.Fatalf("no prediction found for day 75")
		return 0
	}

	pred1 := setupDB(0.0, 32.0)
	pred2 := setupDB(999999.0, 1000.0) // radically different invalid value on day 75 and future value on day 85

	if math.Abs(pred1-pred2) > 1e-9 {
		t.Fatalf("failure day expected leaked future rows or corrupt value: %f != %f", pred1, pred2)
	}
}

// 2c. Missing reading day gets expected.
func TestMissingReadingDayGetsExpected(t *testing.T) {
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

	loc, _ := time.LoadLocation("America/Sao_Paulo")
	startDate := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)

	var weatherDays []connector.WeatherDay
	var readings []domain.Reading

	for i := 0; i < 80; i++ {
		curDate := startDate.AddDate(0, 0, i)
		dateStr := curDate.Format(time.DateOnly)

		irr := 3.0 + 2.0*math.Sin(float64(i)*0.05)
		tmean := 22.0 + 5.0*math.Cos(float64(i)*0.03)
		weatherDays = append(weatherDays, connector.WeatherDay{
			Date:             dateStr,
			IrradiationKWhM2: &irr,
			TemperatureMeanC: &tmean,
		})

		if i == 70 {
			// Day 70 has NO reading at all (missing row)
			continue
		}

		startUTC := curDate.UTC()
		endUTC := curDate.AddDate(0, 0, 1).UTC()
		val := 8.0 * irr

		readings = append(readings, domain.Reading{
			DeviceID:      dev.ID,
			Metric:        domain.MetricEnergyKWh,
			PeriodStart:   startUTC,
			PeriodEnd:     endUTC,
			Value:         &val,
			RawValue:      fmt.Sprintf("%.2f", val),
			Quality:       domain.QualityOK,
			QualityReason: "ok",
		})
	}

	_ = st.UpsertWeatherDays(ctx, wloc.ID, "observed", "inmet", weatherDays)
	_, _ = st.InsertReadings(ctx, readings)

	_, err := forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	predRes, err := forecast.Predict(ctx, st, plant.ID, forecast.PredictOpts{Days: 7})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}

	d70Str := startDate.AddDate(0, 0, 70).Format(time.DateOnly)
	found := false
	for _, p := range predRes.Predictions {
		if p.Date == d70Str && p.DeviceID == dev.ID {
			found = true
			if p.KWhExpected <= 0 {
				t.Errorf("expected positive expected kWh for missing reading day, got %f", p.KWhExpected)
			}
			break
		}
	}
	if !found {
		t.Fatalf("missing reading day %s did not get an expected prediction row", d70Str)
	}
}

// 3. Zero predictions from a nil model: device without model gets no rows and is reported in Message.
func TestDeviceWithoutModelGetsNoRows(t *testing.T) {
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
	// D1 has area/efficiency
	d1, _ := st.UpsertDevice(ctx, domain.Device{
		PlantID:     plant.ID,
		ExternalID:  "D1",
		Name:        "D1",
		PanelAreaM2: &area,
		Efficiency:  &eff,
	})
	// D2 lacks area/efficiency
	d2, _ := st.UpsertDevice(ctx, domain.Device{
		PlantID:    plant.ID,
		ExternalID: "D2",
		Name:       "D2",
	})

	seedPlantData(t, st, plant.ID, []int64{d1.ID, d2.ID}, 80, -1, 0)

	// Set active model to physical
	now := time.Now().UTC()
	trainStart := "2026-01-01"
	trainEnd := "2026-03-21"
	mr, err := st.UpsertModelRun(ctx, store.ModelRun{
		Name:        "physical",
		Version:     "v1",
		PlantID:     plant.ID,
		TrainedAt:   now,
		TrainStart:  &trainStart,
		TrainEnd:    &trainEnd,
		MetricsJSON: `{"walk_forward":{"devices":{"D1":{"rmse": 2.0}}}}`,
		ParamsJSON:  `{}`,
		Active:      true,
	})
	if err != nil {
		t.Fatalf("upsert model run: %v", err)
	}
	_ = st.SetActiveModelRun(ctx, plant.ID, mr.ID)

	predRes, err := forecast.Predict(ctx, st, plant.ID, forecast.PredictOpts{Days: 7})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}

	// D2 must get NO prediction rows!
	for _, p := range predRes.Predictions {
		if p.DeviceID == d2.ID {
			t.Fatalf("device D2 without model got prediction row: %+v", p)
		}
	}

	// Message must report D2
	if predRes.Message == "" || (!containsStr(predRes.Message, "D2")) {
		t.Fatalf("expected message reporting D2 was skipped, got: %q", predRes.Message)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) > 0 && (stringSearch(s, sub)))
}

func stringSearch(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// 4. Forecast range: exactly Days dates.
func TestForecastDaysIsExact(t *testing.T) {
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

	// Add 14 forecast days (starting today through +13 days)
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	today := time.Now().In(loc)
	var forecastDays []connector.WeatherDay
	irr := 4.5
	tmean := 25.0
	for i := 0; i < 14; i++ {
		dStr := today.AddDate(0, 0, i).Format(time.DateOnly)
		forecastDays = append(forecastDays, connector.WeatherDay{
			Date:             dStr,
			IrradiationKWhM2: &irr,
			TemperatureMeanC: &tmean,
		})
	}
	_ = st.UpsertWeatherDays(ctx, wloc.ID, "forecast", "open-meteo", forecastDays)

	_, _ = forecast.Backtest(ctx, st, plant.ID, forecast.BacktestOpts{MinTrainDays: 60, RefitDays: 7})

	reqDays := 7
	predRes, err := forecast.Predict(ctx, st, plant.ID, forecast.PredictOpts{Days: reqDays})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}

	dateSet := make(map[string]bool)
	for _, p := range predRes.FutureDays {
		dateSet[p.Date] = true
	}

	if len(dateSet) != reqDays {
		t.Fatalf("expected exactly %d forecast dates, got %d (dates: %v)", reqDays, len(dateSet), dateSet)
	}
}
