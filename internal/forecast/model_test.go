package forecast_test

import (
	"math"
	"testing"

	"github.com/luigiazoreng/solarguard/internal/forecast"
)

func TestCalibratedRecoversKnownK(t *testing.T) {
	// True relation: y = 8.5 * I
	trueK := 8.5
	rows := make([]forecast.TrainRow, 100)
	for i := 0; i < 100; i++ {
		irr := 1.0 + float64(i)*0.08
		rows[i] = forecast.TrainRow{
			KWh:         trueK * irr,
			Irradiation: irr,
		}
	}

	model := forecast.NewCalibratedModel()
	if err := model.Fit(rows); err != nil {
		t.Fatalf("fit calibrated: %v", err)
	}

	k := model.K()
	if math.Abs(k-trueK) > 1e-9 {
		t.Errorf("expected k=%f, got %f", trueK, k)
	}

	pred, err := model.Predict(forecast.DayFeatures{Irradiation: 5.0})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	expected := trueK * 5.0
	if math.Abs(pred-expected) > 1e-9 {
		t.Errorf("expected pred=%f, got %f", expected, pred)
	}
}

func TestPhysicalAppliesTemperaturePenaltyAbove25C(t *testing.T) {
	area := 42.0
	eff := 0.18
	model, err := forecast.NewPhysicalModel(&area, &eff)
	if err != nil {
		t.Fatalf("new physical model: %v", err)
	}

	// Below or equal to 25C: no penalty. eta = eff = 0.18.
	// E = 5.0 * 42.0 * 0.18 = 37.8
	temp20 := 20.0
	pred20, err := model.Predict(forecast.DayFeatures{
		Irradiation: 5.0,
		TempMean:    &temp20,
	})
	if err != nil {
		t.Fatalf("predict at 20C: %v", err)
	}
	if math.Abs(pred20-37.8) > 1e-6 {
		t.Errorf("expected 37.8 at 20C, got %f", pred20)
	}

	temp25 := 25.0
	pred25, err := model.Predict(forecast.DayFeatures{
		Irradiation: 5.0,
		TempMean:    &temp25,
	})
	if err != nil {
		t.Fatalf("predict at 25C: %v", err)
	}
	if math.Abs(pred25-37.8) > 1e-6 {
		t.Errorf("expected 37.8 at 25C, got %f", pred25)
	}

	// Above 25C: e.g. 35C (10 degrees above 25).
	// Penalty: 1 - 0.004 * 10 = 1 - 0.04 = 0.96.
	// eta = 0.18 * 0.96 = 0.1728
	// E = 5.0 * 42.0 * 0.1728 = 36.288
	temp35 := 35.0
	pred35, err := model.Predict(forecast.DayFeatures{
		Irradiation: 5.0,
		TempMean:    &temp35,
	})
	if err != nil {
		t.Fatalf("predict at 35C: %v", err)
	}
	expected35 := 5.0 * 42.0 * (0.18 * (1.0 - 0.004*10.0))
	if math.Abs(pred35-expected35) > 1e-6 {
		t.Errorf("expected %f at 35C, got %f", expected35, pred35)
	}

	// Missing area or efficiency must report error/skip
	_, errMissing := forecast.NewPhysicalModel(nil, &eff)
	if errMissing == nil {
		t.Errorf("expected error when area is missing")
	}
}

func TestRidgeRecoversKnownLinearRelation(t *testing.T) {
	// Relation: y = 2.0 + 4.0 * I
	// With 1000 data points, lambda=1.0 regularizer is tiny relative to N=1000.
	n := 1000
	rows := make([]forecast.TrainRow, n)
	for i := 0; i < n; i++ {
		irr := 1.0 + float64(i%20)*0.3
		tmean := 22.0 + float64(i%15)*0.5
		precip := 0.0
		hum := 60.0 + float64(i%10)
		y := 2.0 + 4.0*irr
		rows[i] = forecast.TrainRow{
			KWh:           y,
			Irradiation:   irr,
			TempMean:      &tmean,
			Precipitation: &precip,
			Humidity:      &hum,
		}
	}

	model := forecast.NewRidgeModel()
	if err := model.Fit(rows); err != nil {
		t.Fatalf("fit ridge: %v", err)
	}

	// Predict for a new day
	testIrr := 4.5
	testTmean := 25.0
	testPrecip := 0.0
	testHum := 65.0
	pred, err := model.Predict(forecast.DayFeatures{
		Irradiation:   testIrr,
		TempMean:      &testTmean,
		Precipitation: &testPrecip,
		Humidity:      &testHum,
	})
	if err != nil {
		t.Fatalf("predict ridge: %v", err)
	}

	expectedY := 2.0 + 4.0*testIrr // 20.0
	if math.Abs(pred-expectedY) > 0.1 {
		t.Errorf("expected ~%f, got %f", expectedY, pred)
	}
}

func TestRidgeSingularMatrixConstantFeatureDoesNotCrash(t *testing.T) {
	// Constant features: precip is always 0, hum is always 70.
	rows := make([]forecast.TrainRow, 70)
	for i := 0; i < 70; i++ {
		irr := 3.0 + float64(i)*0.05
		tmean := 24.0
		precip := 0.0
		hum := 70.0
		rows[i] = forecast.TrainRow{
			KWh:           2.0 * irr,
			Irradiation:   irr,
			TempMean:      &tmean,
			Precipitation: &precip,
			Humidity:      &hum,
		}
	}

	model := forecast.NewRidgeModel()
	// Must not crash or panic on constant features
	if err := model.Fit(rows); err != nil {
		t.Fatalf("fit ridge with constant features failed: %v", err)
	}

	p := 0.0
	h := 70.0
	pred, err := model.Predict(forecast.DayFeatures{
		Irradiation:   4.0,
		Precipitation: &p,
		Humidity:      &h,
	})
	if err != nil {
		t.Fatalf("predict with constant features failed: %v", err)
	}
	if pred < 0 {
		t.Errorf("predictions must be clamped >= 0, got %f", pred)
	}
}

func TestRidgeScalingUsesTrainingRowsOnly(t *testing.T) {
	// Verify that scaling parameters (mean, std) are derived solely from training rows.
	rows := []forecast.TrainRow{
		{KWh: 10, Irradiation: 2.0},
		{KWh: 20, Irradiation: 4.0},
		{KWh: 30, Irradiation: 6.0},
	}
	model := forecast.NewRidgeModel()
	if err := model.Fit(rows); err != nil {
		t.Fatalf("fit: %v", err)
	}

	means, stds := model.ScalingParams()
	// Feature 0 is Irradiation. Mean of [2, 4, 6] = 4.0
	if math.Abs(means[0]-4.0) > 1e-9 {
		t.Errorf("expected irradiation mean 4.0, got %f", means[0])
	}
	// Population std of [2, 4, 6]: sqrt(((2-4)^2 + (4-4)^2 + (6-4)^2)/3) = sqrt(8/3) ~ 1.63299
	expectedStd := math.Sqrt(8.0 / 3.0)
	if math.Abs(stds[0]-expectedStd) > 1e-4 {
		t.Errorf("expected irradiation std %f, got %f", expectedStd, stds[0])
	}
}

func TestPersistenceModel(t *testing.T) {
	model := forecast.NewPersistenceModel()
	if err := model.Fit(nil); err != nil {
		t.Fatalf("fit: %v", err)
	}

	prev := 14.5
	pred, err := model.Predict(forecast.DayFeatures{PrevKWh: &prev})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	if pred != prev {
		t.Errorf("expected %f, got %f", prev, pred)
	}

	_, err = model.Predict(forecast.DayFeatures{PrevKWh: nil})
	if err == nil {
		t.Errorf("expected error when PrevKWh is nil")
	}
}
