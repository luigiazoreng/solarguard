package web

import (
	"context"
	"fmt"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// ComputeLostEnergy calculates the total estimated lost energy (sum of expected kWh on days whose reading is not ok).
func ComputeLostEnergy(ctx context.Context, st *store.Store, plantID int64, deviceID *int64, from, to string) (float64, error) {
	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return 0, fmt.Errorf("get plant %d: %w", plantID, err)
	}

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		loc = time.UTC
	}

	// 1. Fetch predictions for the active model in [from, to]
	preds, err := st.ListPredictions(ctx, plantID, deviceID, from, to)
	if err != nil {
		return 0, fmt.Errorf("list predictions: %w", err)
	}
	if len(preds) == 0 {
		return 0, nil
	}

	// 2. Fetch readings in [from, to]
	fStart, _, err := domain.LocalDay(from, plant.Timezone)
	if err != nil {
		return 0, err
	}
	_, tEnd, err := domain.LocalDay(to, plant.Timezone)
	if err != nil {
		return 0, err
	}

	readings, err := st.ListReadingsByPlant(ctx, plantID, fStart, tEnd)
	if err != nil {
		return 0, fmt.Errorf("list readings: %w", err)
	}

	type devDateKey struct {
		deviceID int64
		date     string
	}

	// Map readings by (deviceID, date)
	okReadings := make(map[devDateKey]bool)
	for _, r := range readings {
		if r.Metric != domain.MetricEnergyKWh {
			continue
		}
		if deviceID != nil && r.DeviceID != *deviceID {
			continue
		}
		dateStr := r.PeriodStart.In(loc).Format(time.DateOnly)
		key := devDateKey{deviceID: r.DeviceID, date: dateStr}
		if r.Value != nil && r.Quality == domain.QualityOK {
			okReadings[key] = true
		}
	}

	// 3. Sum expected kWh on days where reading is missing or not ok
	var lostEnergy float64
	for _, p := range preds {
		key := devDateKey{deviceID: p.DeviceID, date: p.Date}
		if !okReadings[key] {
			lostEnergy += p.KWhExpected
		}
	}

	return lostEnergy, nil
}
