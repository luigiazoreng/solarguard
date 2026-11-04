package forecast_test

import (
	"context"
	"testing"

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
