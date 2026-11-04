package forecast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// PredictOpts configures prediction generation.
type PredictOpts struct {
	Days int // default 7
}

// PredictResult contains the generated predictions and metadata.
type PredictResult struct {
	PlantID     int64              `json:"plant_id"`
	ActiveModel string             `json:"active_model"`
	Predictions []store.Prediction `json:"predictions"`
	FutureDays  []store.Prediction `json:"future_days"`
	Message     string             `json:"message,omitempty"`
}

// Predict writes honest expected values for past observed days and forecasts for future dates.
func Predict(ctx context.Context, st *store.Store, plantID int64, opts PredictOpts) (*PredictResult, error) {
	if opts.Days <= 0 {
		opts.Days = 7
	}

	res := &PredictResult{
		PlantID: plantID,
	}

	activeRun, err := st.GetActiveModelRun(ctx, plantID)
	if errors.Is(err, store.ErrNotFound) {
		res.Message = fmt.Sprintf("planta %d não possui modelo ativo; nenhuma previsão gravada", plantID)
		return res, nil
	}
	if err != nil {
		return nil, fmt.Errorf("obter modelo ativo da planta %d: %w", plantID, err)
	}

	res.ActiveModel = activeRun.Name

	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return nil, fmt.Errorf("obter planta %d: %w", plantID, err)
	}

	if plant.WeatherLocationID == nil {
		res.Message = fmt.Sprintf("planta %d (%s) não possui local de clima configurado", plant.ID, plant.Name)
		return res, nil
	}

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		return nil, fmt.Errorf("carregar fuso horário %q: %w", plant.Timezone, err)
	}

	devices, err := st.ListDevices(ctx, plantID)
	if err != nil {
		return nil, fmt.Errorf("listar dispositivos: %w", err)
	}

	var generatingDevices []domain.Device
	for _, d := range devices {
		if strings.EqualFold(d.Kind, "plant") {
			continue
		}
		generatingDevices = append(generatingDevices, d)
	}
	if len(generatingDevices) == 0 {
		res.Message = "nenhum dispositivo de geração cadastrado"
		return res, nil
	}

	// Parse RMSE from active model metrics JSON
	deviceRMSE := make(map[string]float64)
	if activeRun.MetricsJSON != "" {
		var payload struct {
			WalkForward struct {
				Devices map[string]Metrics `json:"devices"`
				Pooled  Metrics            `json:"pooled"`
			} `json:"walk_forward"`
		}
		if err := json.Unmarshal([]byte(activeRun.MetricsJSON), &payload); err == nil {
			for devName, m := range payload.WalkForward.Devices {
				deviceRMSE[devName] = m.RMSE
			}
		}
	}

	// 1. Run backtest to obtain honest walk-forward out-of-fold predictions
	btRes, err := Backtest(ctx, st, plantID, BacktestOpts{MinTrainDays: 60, RefitDays: 7})
	if err != nil {
		return nil, fmt.Errorf("executar backtest para predições honestas: %w", err)
	}

	nowTime := time.Now().UTC()
	var allPreds []store.Prediction
	var futurePreds []store.Prediction

	// Gather observed rows per device and fit active model on all ok rows
	type devInfo struct {
		dev        domain.Device
		rows       []TrainRow
		model      Model
		rmse       float64
		latestDate string
	}

	var devInfos []devInfo
	overallLatestObserved := "0000-00-00"

	for _, d := range generatingDevices {
		// Read ok readings
		rowsQuery, err := st.DB().QueryContext(ctx, `SELECT period_start, value
			FROM readings
			WHERE device_id = ? AND metric = 'energy_kwh' AND quality = 'ok' AND value IS NOT NULL
			ORDER BY period_start ASC`, d.ID)
		if err != nil {
			return nil, fmt.Errorf("consultar leituras: %w", err)
		}

		var okDates []string
		readingMap := make(map[string]float64)
		for rowsQuery.Next() {
			var startStr string
			var val float64
			if err := rowsQuery.Scan(&startStr, &val); err != nil {
				rowsQuery.Close()
				return nil, err
			}
			t, err := time.Parse(time.RFC3339, startStr)
			if err != nil {
				rowsQuery.Close()
				return nil, err
			}
			localDay := t.In(loc).Format(time.DateOnly)
			okDates = append(okDates, localDay)
			readingMap[localDay] = val
		}
		rowsQuery.Close()

		if len(okDates) == 0 {
			continue
		}

		// Read observed weather
		weatherDays, err := st.PreferredObserved(ctx, *plant.WeatherLocationID, okDates[0], okDates[len(okDates)-1], []string{"inmet", "open-meteo"})
		if err != nil {
			return nil, fmt.Errorf("obter clima observado: %w", err)
		}

		weatherMap := make(map[string]TrainRow)
		for _, w := range weatherDays {
			if w.IrradiationKWhM2 == nil {
				continue
			}
			weatherMap[w.Date] = TrainRow{
				Date:          w.Date,
				Irradiation:   *w.IrradiationKWhM2,
				TempMean:      w.TemperatureMeanC,
				Precipitation: w.PrecipitationMM,
				Humidity:      w.HumidityMeanPct,
			}
		}

		var combined []TrainRow
		var prevKWh *float64
		latestDevDate := "0000-00-00"

		for _, date := range okDates {
			val := readingMap[date]
			w, ok := weatherMap[date]
			if !ok {
				continue
			}
			r := TrainRow{
				Date:          date,
				KWh:           val,
				Irradiation:   w.Irradiation,
				TempMean:      w.TempMean,
				Precipitation: w.Precipitation,
				Humidity:      w.Humidity,
				PrevKWh:       prevKWh,
			}
			combined = append(combined, r)
			currVal := val
			prevKWh = &currVal
			if date > latestDevDate {
				latestDevDate = date
			}
			if date > overallLatestObserved {
				overallLatestObserved = date
			}
		}

		// Instantiate and fit active model on all ok rows
		var m Model
		switch activeRun.Name {
		case "calibrated":
			c := NewCalibratedModel()
			_ = c.Fit(combined)
			m = c
		case "physical":
			if d.PanelAreaM2 != nil && d.Efficiency != nil {
				p, err := NewPhysicalModel(d.PanelAreaM2, d.Efficiency)
				if err == nil {
					m = p
				}
			}
		case "ridge":
			r := NewRidgeModel()
			_ = r.Fit(combined)
			m = r
		}

		rmse := deviceRMSE[d.Name]
		if rmse == 0 && btRes != nil {
			rmse = btRes.DeviceResidualRMSE[d.Name]
		}

		devInfos = append(devInfos, devInfo{
			dev:        d,
			rows:       combined,
			model:      m,
			rmse:       rmse,
			latestDate: latestDevDate,
		})

		// Generate expected values for observed days:
		// For dates in walk-forward history, use out-of-fold predictions
		wfDevPreds := btRes.DeviceDayPredictions[d.Name]

		for _, r := range combined {
			var expected float64
			if wfMap, ok := wfDevPreds[r.Date]; ok {
				if wfVal, ok := wfMap[activeRun.Name]; ok {
					expected = wfVal
				} else {
					if m != nil {
						p, err := m.Predict(DayFeatures{
							Date:          r.Date,
							Irradiation:   r.Irradiation,
							TempMean:      r.TempMean,
							Precipitation: r.Precipitation,
							Humidity:      r.Humidity,
						})
						if err == nil {
							expected = p
						}
					}
				}
			} else {
				if m != nil {
					p, err := m.Predict(DayFeatures{
						Date:          r.Date,
						Irradiation:   r.Irradiation,
						TempMean:      r.TempMean,
						Precipitation: r.Precipitation,
						Humidity:      r.Humidity,
					})
					if err == nil {
						expected = p
					}
				}
			}

			// Approximate 90% prediction band (+/- 1.645 * RMSE)
			lower := math.Max(0.0, expected-1.645*rmse)
			upper := math.Max(0.0, expected+1.645*rmse)

			allPreds = append(allPreds, store.Prediction{
				DeviceID:    d.ID,
				Date:        r.Date,
				ModelRunID:  activeRun.ID,
				KWhExpected: expected,
				KWhLower:    &lower,
				KWhUpper:    &upper,
				CreatedAt:   nowTime,
			})
		}
	}

	// 2. Future forecast predictions using weather_days kind forecast
	todayStr := time.Now().In(loc).Format(time.DateOnly)
	startDate := todayStr
	if overallLatestObserved >= startDate {
		// Start day after latest observed
		t, err := time.ParseInLocation(time.DateOnly, overallLatestObserved, loc)
		if err == nil {
			startDate = t.AddDate(0, 0, 1).Format(time.DateOnly)
		}
	}

	endDate := startDate
	tEnd, err := time.ParseInLocation(time.DateOnly, startDate, loc)
	if err == nil {
		endDate = tEnd.AddDate(0, 0, opts.Days).Format(time.DateOnly)
	}

	forecastDays, err := st.ListWeatherDays(ctx, *plant.WeatherLocationID, startDate, endDate, "forecast")
	if err != nil {
		return nil, fmt.Errorf("consultar previsão do tempo: %w", err)
	}

	if len(forecastDays) == 0 {
		msg := "nenhuma previsão de clima encontrada; nenhuma previsão futura gerada"
		if res.Message == "" {
			res.Message = msg
		} else {
			res.Message += "; " + msg
		}
	} else {
		for _, fd := range forecastDays {
			if fd.IrradiationKWhM2 == nil {
				continue
			}
			f := DayFeatures{
				Date:          fd.Date,
				Irradiation:   *fd.IrradiationKWhM2,
				TempMean:      fd.TemperatureMeanC,
				Precipitation: fd.PrecipitationMM,
				Humidity:      fd.HumidityMeanPct,
			}

			for _, di := range devInfos {
				if di.model == nil {
					continue
				}
				expected, err := di.model.Predict(f)
				if err != nil {
					continue
				}

				// Approximate 90% prediction band (+/- 1.645 * RMSE)
				lower := math.Max(0.0, expected-1.645*di.rmse)
				upper := math.Max(0.0, expected+1.645*di.rmse)

				pred := store.Prediction{
					DeviceID:    di.dev.ID,
					Date:        fd.Date,
					ModelRunID:  activeRun.ID,
					KWhExpected: expected,
					KWhLower:    &lower,
					KWhUpper:    &upper,
					CreatedAt:   nowTime,
				}
				allPreds = append(allPreds, pred)
				futurePreds = append(futurePreds, pred)
			}
		}
	}

	// 3. Upsert into database
	if err := st.UpsertPredictions(ctx, allPreds); err != nil {
		return nil, fmt.Errorf("upsert predições: %w", err)
	}

	res.Predictions = allPreds
	res.FutureDays = futurePreds

	// Sort future days by date and device
	sort.Slice(res.FutureDays, func(i, j int) bool {
		if res.FutureDays[i].Date == res.FutureDays[j].Date {
			return res.FutureDays[i].DeviceID < res.FutureDays[j].DeviceID
		}
		return res.FutureDays[i].Date < res.FutureDays[j].Date
	})

	return res, nil
}
