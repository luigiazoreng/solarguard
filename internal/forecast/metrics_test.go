package forecast_test

import (
	"math"
	"testing"

	"github.com/luigiazoreng/solarguard/internal/forecast"
)

func TestComputeMetricsHandComputed(t *testing.T) {
	// Hand-computed example:
	// y = [10.0, 4.0, 20.0, 6.0]
	// y_hat = [12.0, 5.0, 18.0, 6.0]
	actuals := []float64{10.0, 4.0, 20.0, 6.0}
	preds := []float64{12.0, 5.0, 18.0, 6.0}

	m := forecast.ComputeMetrics(actuals, preds)

	if m.N != 4 {
		t.Errorf("expected N=4, got %d", m.N)
	}

	// MAE: (2 + 1 + 2 + 0)/4 = 1.25
	if math.Abs(m.MAE-1.25) > 1e-9 {
		t.Errorf("expected MAE=1.25, got %f", m.MAE)
	}

	// RMSE: sqrt((4 + 1 + 4 + 0)/4) = sqrt(2.25) = 1.5
	if math.Abs(m.RMSE-1.5) > 1e-9 {
		t.Errorf("expected RMSE=1.5, got %f", m.RMSE)
	}

	// Bias: (2 + 1 - 2 + 0)/4 = 0.25
	if math.Abs(m.Bias-0.25) > 1e-9 {
		t.Errorf("expected Bias=0.25, got %f", m.Bias)
	}

	// WAPE: 5.0 / 40.0 = 0.125
	if math.Abs(m.WAPE-0.125) > 1e-9 {
		t.Errorf("expected WAPE=0.125, got %f", m.WAPE)
	}

	// R2: mean y = 10.0. SS_tot = 0^2 + (-6)^2 + 10^2 + (-4)^2 = 152.0.
	// SS_res = 9.0. R2 = 1 - 9/152 = 143/152 ~ 0.940789473684
	expectedR2 := 1.0 - 9.0/152.0
	if math.Abs(m.R2-expectedR2) > 1e-9 {
		t.Errorf("expected R2=%f, got %f", expectedR2, m.R2)
	}

	// MAPE (>5 kWh only):
	// y=10.0: |12-10|/10 = 0.2
	// y=4.0: excluded (<= 5 kWh)
	// y=20.0: |18-20|/20 = 0.1
	// y=6.0: |6-6|/6 = 0.0
	// MAPEN = 3, MAPE = (0.2 + 0.1 + 0.0)/3 = 0.10 (or 10.0%)
	if m.MAPEN != 3 {
		t.Errorf("expected MAPEN=3, got %d", m.MAPEN)
	}
	if m.MAPE == nil {
		t.Fatalf("expected non-nil MAPE")
	}
	// Let's store MAPE as percentage (10.0) or fraction (0.10).
	// Let's check: 10.0 or 0.10. If percentage: 10.0.
	if math.Abs(*m.MAPE-10.0) > 1e-6 && math.Abs(*m.MAPE-0.10) > 1e-6 {
		t.Errorf("expected MAPE=10.0%% or 0.10, got %f", *m.MAPE)
	}
}
