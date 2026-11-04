// Package pipeline orchestrates the nightly processing sequence per plant:
// weather sync, quality classification, forecast backtest & prediction, and alert detection.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/luigiazoreng/solarguard/internal/alerts"
	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/connector/weather/openmeteo"
	"github.com/luigiazoreng/solarguard/internal/forecast"
	"github.com/luigiazoreng/solarguard/internal/quality"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/weather"
)

// Deps defines injected dependencies for pipeline runs.
type Deps struct {
	WeatherProvider connector.WeatherProvider
	Now             func() time.Time
	Logger          *slog.Logger
}

// StepOutcome records the result of a single pipeline step.
type StepOutcome struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // ok, failed, skipped
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// RunPlant runs the nightly pipeline for a single plant.
// Failures in individual steps do not stop subsequent steps; every step's outcome is recorded.
func RunPlant(ctx context.Context, st *store.Store, plantID int64, deps Deps) (*store.PipelineRun, error) {
	nowFn := deps.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	startedAt := nowFn().UTC()

	wp := deps.WeatherProvider
	if wp == nil {
		wp = openmeteo.New()
	}
	weatherSvc := weather.NewService(st, wp)

	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return nil, fmt.Errorf("get plant %d: %w", plantID, err)
	}

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		loc = time.UTC
	}
	plantNow := nowFn().In(loc)
	localDate := plantNow.Format(time.DateOnly)
	yesterdayStr := plantNow.AddDate(0, 0, -1).Format(time.DateOnly)

	var outcomes []StepOutcome
	recordStep := func(name string, fn func() error) {
		t0 := time.Now()
		err := fn()
		duration := time.Since(t0).Milliseconds()
		outcome := StepOutcome{
			Name:       name,
			DurationMS: duration,
		}
		if err != nil {
			outcome.Status = "failed"
			outcome.Error = err.Error()
			if deps.Logger != nil {
				deps.Logger.Warn("pipeline step failed", "plant_id", plantID, "step", name, "err", err)
			}
		} else {
			outcome.Status = "ok"
		}
		outcomes = append(outcomes, outcome)
	}

	// Step 1: Observed weather sync (if plant has coordinates)
	recordStep("observed_weather", func() error {
		if plant.Latitude == nil || plant.Longitude == nil {
			// Plant has no coordinates, skip weather sync
			return nil
		}

		newestDate, err := st.GetPlantNewestWeatherDate(ctx, plantID, "observed")
		if err != nil {
			return fmt.Errorf("check newest weather date: %w", err)
		}

		from := newestDate
		if from == "" {
			firstReading, err := st.GetPlantFirstReadingDate(ctx, plantID)
			if err != nil {
				return fmt.Errorf("check first reading date: %w", err)
			}
			if firstReading != "" {
				from = firstReading
			} else {
				from = yesterdayStr
			}
		}

		if from > yesterdayStr {
			// Already up to date
			return nil
		}

		return weatherSvc.SyncObserved(ctx, plantID, from, yesterdayStr)
	})

	// Step 2: Forecast weather sync for 7 days
	recordStep("forecast_weather", func() error {
		if plant.WeatherLocationID == nil && (plant.Latitude == nil || plant.Longitude == nil) {
			return nil
		}
		return weatherSvc.SyncForecast(ctx, plantID, 7)
	})

	// Step 3: Quality classification
	recordStep("quality", func() error {
		_, err := quality.Classify(ctx, st, plantID, time.Time{}, time.Time{})
		return err
	})

	// Step 4: Forecast Backtest + Predict
	recordStep("forecast", func() error {
		_, err := forecast.Backtest(ctx, st, plantID, forecast.BacktestOpts{
			MinTrainDays: 60,
			RefitDays:    7,
		})
		if err != nil {
			return fmt.Errorf("backtest: %w", err)
		}

		_, err = forecast.Predict(ctx, st, plantID, forecast.PredictOpts{
			Days: 7,
		})
		if err != nil {
			return fmt.Errorf("predict: %w", err)
		}
		return nil
	})

	// Step 5: Alerts detection
	recordStep("alerts", func() error {
		_, err := alerts.Run(ctx, st, plantID, alerts.Options{
			Now: nowFn,
		})
		return err
	})

	finishedAt := nowFn().UTC()

	// Determine overall status
	hasFailed := false
	hasOK := false
	for _, o := range outcomes {
		if o.Status == "failed" {
			hasFailed = true
		} else if o.Status == "ok" {
			hasOK = true
		}
	}

	overallStatus := "success"
	if hasFailed && hasOK {
		overallStatus = "partial"
	} else if hasFailed && !hasOK {
		overallStatus = "failed"
	}

	stepsJSONBytes, _ := json.Marshal(outcomes)
	run := &store.PipelineRun{
		PlantID:    plantID,
		LocalDate:  localDate,
		Status:     overallStatus,
		StepsJSON:  string(stepsJSONBytes),
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
	}

	if err := st.RecordPipelineRun(ctx, run); err != nil {
		if deps.Logger != nil {
			deps.Logger.Error("record pipeline run", "plant_id", plantID, "err", err)
		}
		return run, fmt.Errorf("record pipeline run: %w", err)
	}

	return run, nil
}

// RunAll runs the pipeline across all plants sequentially, continuing after a failure in one plant.
func RunAll(ctx context.Context, st *store.Store, deps Deps) ([]*store.PipelineRun, error) {
	plants, err := st.ListPlants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list plants: %w", err)
	}

	var runs []*store.PipelineRun
	for _, p := range plants {
		if ctx.Err() != nil {
			return runs, ctx.Err()
		}
		run, err := RunPlant(ctx, st, p.ID, deps)
		if err != nil {
			if deps.Logger != nil {
				deps.Logger.Error("pipeline plant run failed", "plant_id", p.ID, "err", err)
			}
			continue
		}
		runs = append(runs, run)
	}
	return runs, nil
}
