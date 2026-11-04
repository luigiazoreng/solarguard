package forecast

import "math"

// Metrics holds the backtest and evaluation performance indicators.
type Metrics struct {
	N     int      `json:"n"`
	MAE   float64  `json:"mae"`
	RMSE  float64  `json:"rmse"`
	R2    float64  `json:"r2"`
	WAPE  float64  `json:"wape"`
	MAPE  *float64 `json:"mape,omitempty"`
	MAPEN int      `json:"mape_n"`
	Bias  float64  `json:"bias"`
}

// ComputeMetrics calculates n, MAE, RMSE, R2, WAPE, MAPE (>5 kWh), and Bias.
func ComputeMetrics(actuals, preds []float64) Metrics {
	n := len(actuals)
	if n == 0 || len(preds) != n {
		return Metrics{}
	}

	var sumAbsErr, sumSqErr, sumErr, sumActual float64
	var sumMape float64
	var mapeCount int

	for i := 0; i < n; i++ {
		y := actuals[i]
		yHat := preds[i]
		e := yHat - y
		absErr := math.Abs(e)

		sumAbsErr += absErr
		sumSqErr += e * e
		sumErr += e
		sumActual += y

		// MAPE with true value above 5 kWh only
		if y > 5.0 {
			sumMape += (absErr / y) * 100.0
			mapeCount++
		}
	}

	mae := sumAbsErr / float64(n)
	rmse := math.Sqrt(sumSqErr / float64(n))
	bias := sumErr / float64(n)

	wape := 0.0
	if sumActual > 0 {
		wape = sumAbsErr / sumActual
	}

	meanY := sumActual / float64(n)
	var ssTot float64
	for i := 0; i < n; i++ {
		diff := actuals[i] - meanY
		ssTot += diff * diff
	}

	r2 := 0.0
	if ssTot > 0 {
		r2 = 1.0 - (sumSqErr / ssTot)
	} else if sumSqErr == 0 {
		r2 = 1.0
	}

	var mapePtr *float64
	if mapeCount > 0 {
		v := sumMape / float64(mapeCount)
		mapePtr = &v
	}

	return Metrics{
		N:     n,
		MAE:   mae,
		RMSE:  rmse,
		R2:    r2,
		WAPE:  wape,
		MAPE:  mapePtr,
		MAPEN: mapeCount,
		Bias:  bias,
	}
}
