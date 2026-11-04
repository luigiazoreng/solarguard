package forecast

import (
	"errors"
	"fmt"
	"math"
)

// TrainRow represents one day of observations and weather for a device.
type TrainRow struct {
	Date          string
	KWh           float64
	Irradiation   float64
	TempMean      *float64
	Precipitation *float64
	Humidity      *float64
	PrevKWh       *float64
}

// DayFeatures contains the inputs required to predict one day's generation.
type DayFeatures struct {
	Date          string
	Irradiation   float64
	TempMean      *float64
	Precipitation *float64
	Humidity      *float64
	PrevKWh       *float64
}

// Model defines the small interface shared by all forecasting models.
type Model interface {
	Name() string
	Fit(rows []TrainRow) error
	Predict(f DayFeatures) (float64, error)
}

// PersistenceModel predicts the device's previous ok value.
type PersistenceModel struct{}

// NewPersistenceModel creates a new persistence model.
func NewPersistenceModel() *PersistenceModel {
	return &PersistenceModel{}
}

func (m *PersistenceModel) Name() string { return "persistence" }

func (m *PersistenceModel) Fit(rows []TrainRow) error {
	return nil
}

func (m *PersistenceModel) Predict(f DayFeatures) (float64, error) {
	if f.PrevKWh == nil {
		return 0, errors.New("persistence requires previous ok value")
	}
	return *f.PrevKWh, nil
}

// PhysicalModel computes I * area * eta without fitting.
type PhysicalModel struct {
	areaM2     float64
	efficiency float64
}

// NewPhysicalModel creates a physical model. Returns error if area or efficiency is missing.
func NewPhysicalModel(areaM2, efficiency *float64) (*PhysicalModel, error) {
	if areaM2 == nil || efficiency == nil {
		return nil, errors.New("physical model requires device panel area and efficiency")
	}
	return &PhysicalModel{
		areaM2:     *areaM2,
		efficiency: *efficiency,
	}, nil
}

func (m *PhysicalModel) Name() string { return "physical" }

func (m *PhysicalModel) Area() float64       { return m.areaM2 }
func (m *PhysicalModel) Efficiency() float64 { return m.efficiency }

func (m *PhysicalModel) Fit(rows []TrainRow) error {
	return nil
}

func (m *PhysicalModel) Predict(f DayFeatures) (float64, error) {
	tmean := 25.0
	if f.TempMean != nil {
		tmean = *f.TempMean
	}
	tempDiff := math.Max(0.0, tmean-25.0)
	eta := m.efficiency * (1.0 - 0.004*tempDiff)
	pred := f.Irradiation * m.areaM2 * eta
	return math.Max(0.0, pred), nil
}

// CalibratedModel fits k * I via least squares through origin.
type CalibratedModel struct {
	k float64
}

// NewCalibratedModel creates a new calibrated model.
func NewCalibratedModel() *CalibratedModel {
	return &CalibratedModel{}
}

func (m *CalibratedModel) Name() string { return "calibrated" }

func (m *CalibratedModel) K() float64 { return m.k }

func (m *CalibratedModel) Fit(rows []TrainRow) error {
	var sumIY, sumI2 float64
	for _, r := range rows {
		sumIY += r.Irradiation * r.KWh
		sumI2 += r.Irradiation * r.Irradiation
	}
	if sumI2 > 0 {
		m.k = sumIY / sumI2
	} else {
		m.k = 0.0
	}
	return nil
}

func (m *CalibratedModel) Predict(f DayFeatures) (float64, error) {
	pred := m.k * f.Irradiation
	return math.Max(0.0, pred), nil
}

// RidgeModel fits ridge regression on standardized features: [intercept, I, Tmean, precip, hum].
type RidgeModel struct {
	intercept float64
	coefs     []float64 // 4 features: I, Tmean, precip, hum
	means     []float64
	stds      []float64
	fitted    bool
}

// NewRidgeModel creates a new ridge model.
func NewRidgeModel() *RidgeModel {
	return &RidgeModel{
		coefs: make([]float64, 4),
		means: make([]float64, 4),
		stds:  make([]float64, 4),
	}
}

func (m *RidgeModel) Name() string            { return "ridge" }
func (m *RidgeModel) Intercept() float64      { return m.intercept }
func (m *RidgeModel) Coefficients() []float64 { return append([]float64(nil), m.coefs...) }
func (m *RidgeModel) ScalingParams() ([]float64, []float64) {
	return append([]float64(nil), m.means...), append([]float64(nil), m.stds...)
}

func (m *RidgeModel) Fit(rows []TrainRow) error {
	n := len(rows)
	if n == 0 {
		return errors.New("cannot fit ridge with zero rows")
	}

	// 1. Calculate training feature means of non-nil values
	for j := 0; j < 4; j++ {
		var sumVal float64
		var count int
		for _, r := range rows {
			var val *float64
			switch j {
			case 0:
				v := r.Irradiation
				val = &v
			case 1:
				val = r.TempMean
			case 2:
				val = r.Precipitation
			case 3:
				val = r.Humidity
			}
			if val != nil {
				sumVal += *val
				count++
			}
		}
		if count > 0 {
			m.means[j] = sumVal / float64(count)
		} else {
			m.means[j] = 0.0
		}
	}

	// 2. Impute missing values with training mean and calculate standard deviations
	for j := 0; j < 4; j++ {
		var sumSqDiff float64
		for _, r := range rows {
			var v float64
			switch j {
			case 0:
				v = r.Irradiation
			case 1:
				if r.TempMean != nil {
					v = *r.TempMean
				} else {
					v = m.means[j]
				}
			case 2:
				if r.Precipitation != nil {
					v = *r.Precipitation
				} else {
					v = m.means[j]
				}
			case 3:
				if r.Humidity != nil {
					v = *r.Humidity
				} else {
					v = m.means[j]
				}
			}
			diff := v - m.means[j]
			sumSqDiff += diff * diff
		}
		std := math.Sqrt(sumSqDiff / float64(n))
		if std < 1e-12 {
			std = 0.0
		}
		m.stds[j] = std
	}

	// 3. Form design matrix X (5 columns: 1.0, z0, z1, z2, z3)
	// Normal equations: (X^T X + lambda * diag(0, 1, 1, 1, 1)) * beta = X^T y
	const numCols = 5
	A := make([][]float64, numCols)
	for i := range A {
		A[i] = make([]float64, numCols)
	}
	b := make([]float64, numCols)

	// Add lambda = 1.0 penalty for features 1..4 (intercept not penalized)
	for j := 1; j < numCols; j++ {
		A[j][j] = 1.0
	}

	for _, r := range rows {
		rowX := [5]float64{1.0, 0, 0, 0, 0}
		for j := 0; j < 4; j++ {
			var v float64
			switch j {
			case 0:
				v = r.Irradiation
			case 1:
				if r.TempMean != nil {
					v = *r.TempMean
				} else {
					v = m.means[j]
				}
			case 2:
				if r.Precipitation != nil {
					v = *r.Precipitation
				} else {
					v = m.means[j]
				}
			case 3:
				if r.Humidity != nil {
					v = *r.Humidity
				} else {
					v = m.means[j]
				}
			}
			if m.stds[j] > 0 {
				rowX[j+1] = (v - m.means[j]) / m.stds[j]
			} else {
				rowX[j+1] = 0.0
			}
		}

		y := r.KWh
		for j := 0; j < numCols; j++ {
			b[j] += rowX[j] * y
			for k := 0; k < numCols; k++ {
				A[j][k] += rowX[j] * rowX[k]
			}
		}
	}

	sol, err := SolveLinearSystem(A, b)
	if err != nil {
		return fmt.Errorf("solve ridge normal equations: %w", err)
	}

	m.intercept = sol[0]
	copy(m.coefs, sol[1:5])
	m.fitted = true
	return nil
}

func (m *RidgeModel) Predict(f DayFeatures) (float64, error) {
	if !m.fitted {
		return 0, errors.New("ridge model not fitted")
	}

	pred := m.intercept
	for j := 0; j < 4; j++ {
		var v float64
		switch j {
		case 0:
			v = f.Irradiation
		case 1:
			if f.TempMean != nil {
				v = *f.TempMean
			} else {
				v = m.means[j]
			}
		case 2:
			if f.Precipitation != nil {
				v = *f.Precipitation
			} else {
				v = m.means[j]
			}
		case 3:
			if f.Humidity != nil {
				v = *f.Humidity
			} else {
				v = m.means[j]
			}
		}

		var z float64
		if m.stds[j] > 0 {
			z = (v - m.means[j]) / m.stds[j]
		}
		pred += m.coefs[j] * z
	}

	return math.Max(0.0, pred), nil
}
