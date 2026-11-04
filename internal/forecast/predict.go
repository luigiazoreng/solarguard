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

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// MinRowsForFailureDayExpected is the minimum number of prior ok rows required
// to compute an expected value on dates without an ok reading (e.g. failure days).
const MinRowsForFailureDayExpected = 30

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

	const minTrainDays = 60
	const refitDays = 7

	// 1. Run backtest to obtain honest walk-forward out-of-fold predictions
	btRes, err := Backtest(ctx, st, plantID, BacktestOpts{MinTrainDays: minTrainDays, RefitDays: refitDays})
	if err != nil {
		return nil, fmt.Errorf("executar backtest para predições honestas: %w", err)
	}

	nowTime := time.Now().UTC()
	var allPreds []store.Prediction
	var futurePreds []store.Prediction
	var messages []string

	// Helper to instantiate a new model of active type for a device
	newModelForDevice := func(d domain.Device) (Model, error) {
		switch activeRun.Name {
		case "calibrated":
			return NewCalibratedModel(), nil
		case "physical":
			return NewPhysicalModel(d.PanelAreaM2, d.Efficiency)
		case "ridge":
			return NewRidgeModel(), nil
		default:
			return nil, fmt.Errorf("modelo desconhecido: %s", activeRun.Name)
		}
	}

	// 2. Fetch all observed weather days for this plant
	allWeatherRows, err := st.PreferredObserved(ctx, *plant.WeatherLocationID, "0000-01-01", "9999-12-31", []string{"inmet", "open-meteo"})
	if err != nil {
		return nil, fmt.Errorf("obter clima observado: %w", err)
	}

	weatherMap := make(map[string]TrainRow)
	var observedDates []string
	latestObservedDate := "0000-00-00"

	for _, w := range allWeatherRows {
		if w.IrradiationKWhM2 == nil {
			continue // A day missing irradiation is skipped
		}
		weatherMap[w.Date] = TrainRow{
			Date:          w.Date,
			Irradiation:   *w.IrradiationKWhM2,
			TempMean:      w.TemperatureMeanC,
			Precipitation: w.PrecipitationMM,
			Humidity:      w.HumidityMeanPct,
		}
		observedDates = append(observedDates, w.Date)
		if w.Date > latestObservedDate {
			latestObservedDate = w.Date
		}
	}
	sort.Strings(observedDates)

	type validDevInfo struct {
		dev    domain.Device
		okRows []TrainRow
		model  Model
		rmse   float64
	}

	var validDevs []validDevInfo

	for _, d := range generatingDevices {
		// Verify model can be built for this device
		fullModel, err := newModelForDevice(d)
		if err != nil {
			messages = append(messages, fmt.Sprintf("dispositivo %s ignorado: %v", d.Name, err))
			continue
		}

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

		var okRows []TrainRow
		var prevKWh *float64
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
			okRows = append(okRows, r)
			currVal := val
			prevKWh = &currVal
		}

		if len(okRows) < MinRowsForFailureDayExpected {
			messages = append(messages, fmt.Sprintf("dispositivo %s possui histórico insuficiente (%d < %d)", d.Name, len(okRows), MinRowsForFailureDayExpected))
			continue
		}

		// Fit model on all ok rows; if fit fails, skip device
		if err := fullModel.Fit(okRows); err != nil {
			messages = append(messages, fmt.Sprintf("dispositivo %s ignorado: erro ao ajustar modelo: %v", d.Name, err))
			continue
		}

		rmse := deviceRMSE[d.Name]
		if rmse == 0 && btRes != nil {
			rmse = btRes.DeviceResidualRMSE[d.Name]
		}
		if rmse == 0 {
			var sse float64
			var count int
			for _, r := range okRows {
				p, err := fullModel.Predict(DayFeatures{
					Date:          r.Date,
					Irradiation:   r.Irradiation,
					TempMean:      r.TempMean,
					Precipitation: r.Precipitation,
					Humidity:      r.Humidity,
					PrevKWh:       r.PrevKWh,
				})
				if err == nil {
					diff := r.KWh - p
					sse += diff * diff
					count++
				}
			}
			if count > 0 {
				rmse = math.Sqrt(sse / float64(count))
			}
		}

		validDevs = append(validDevs, validDevInfo{
			dev:    d,
			okRows: okRows,
			model:  fullModel,
			rmse:   rmse,
		})

		// For every date from the first date with enough history through the latest date with observed weather:
		// - On ok dates: require strictly >= minTrainDays (60) prior ok rows; use honest walk-forward out-of-fold value.
		// - On dates WITHOUT an ok reading (not ok or missing): require strictly >= MinRowsForFailureDayExpected (30) prior ok rows.
		wfDevPreds := btRes.DeviceDayPredictions[d.Name]

		// Pre-index okRows by date for counting prior ok rows
		for _, targetDate := range observedDates {
			// Count how many ok rows have Date < targetDate
			priorCount := sort.Search(len(okRows), func(i int) bool {
				return okRows[i].Date >= targetDate
			})

			_, isOK := readingMap[targetDate]
			if isOK {
				if priorCount < minTrainDays {
					// In-sample / before walk-forward starts: NO prediction row
					continue
				}
				if wfMap, ok := wfDevPreds[targetDate]; ok && wfMap != nil {
					if wfVal, ok := wfMap[activeRun.Name]; ok {
						lower := math.Max(0.0, wfVal-1.645*rmse)
						upper := math.Max(0.0, wfVal+1.645*rmse)
						allPreds = append(allPreds, store.Prediction{
							DeviceID:    d.ID,
							Date:        targetDate,
							ModelRunID:  activeRun.ID,
							KWhExpected: wfVal,
							KWhLower:    &lower,
							KWhUpper:    &upper,
							CreatedAt:   nowTime,
						})
						continue
					}
				}
			} else {
				// Dates WITHOUT an ok reading only require at least MinRowsForFailureDayExpected (30)
				if priorCount < MinRowsForFailureDayExpected {
					continue
				}
			}

			w := weatherMap[targetDate]
			f := DayFeatures{
				Date:          targetDate,
				Irradiation:   w.Irradiation,
				TempMean:      w.TempMean,
				Precipitation: w.Precipitation,
				Humidity:      w.Humidity,
			}

			// Fit active model strictly on ok rows before targetDate
			priorRows := okRows[:priorCount]
			m, err := newModelForDevice(d)
			if err != nil {
				continue
			}
			if err := m.Fit(priorRows); err != nil {
				continue
			}
			p, err := m.Predict(f)
			if err != nil {
				continue
			}

			// Approximate 90% prediction band (+/- 1.645 * RMSE)
			lower := math.Max(0.0, p-1.645*rmse)
			upper := math.Max(0.0, p+1.645*rmse)

			allPreds = append(allPreds, store.Prediction{
				DeviceID:    d.ID,
				Date:        targetDate,
				ModelRunID:  activeRun.ID,
				KWhExpected: p,
				KWhLower:    &lower,
				KWhUpper:    &upper,
				CreatedAt:   nowTime,
			})
		}
	}

	// 3. Future forecast predictions using weather_days kind forecast
	todayStr := time.Now().In(loc).Format(time.DateOnly)
	startDate := todayStr
	if latestObservedDate >= startDate {
		t, err := time.ParseInLocation(time.DateOnly, latestObservedDate, loc)
		if err == nil {
			startDate = t.AddDate(0, 0, 1).Format(time.DateOnly)
		}
	}

	endDate := startDate
	tEnd, err := time.ParseInLocation(time.DateOnly, startDate, loc)
	if err == nil {
		endDate = tEnd.AddDate(0, 0, opts.Days-1).Format(time.DateOnly)
	}

	forecastDays, err := st.ListWeatherDays(ctx, *plant.WeatherLocationID, startDate, endDate, "forecast")
	if err != nil {
		return nil, fmt.Errorf("consultar previsão do tempo: %w", err)
	}

	// Deduplicate and enforce exact Days limit
	var uniqueForecastDates []string
	forecastByDate := make(map[string]connector.WeatherDay)
	for _, fd := range forecastDays {
		if fd.IrradiationKWhM2 == nil {
			continue
		}
		if _, exists := forecastByDate[fd.Date]; !exists {
			uniqueForecastDates = append(uniqueForecastDates, fd.Date)
			forecastByDate[fd.Date] = fd
		}
	}
	sort.Strings(uniqueForecastDates)
	if len(uniqueForecastDates) > opts.Days {
		uniqueForecastDates = uniqueForecastDates[:opts.Days]
	}

	if len(uniqueForecastDates) == 0 {
		messages = append(messages, "nenhuma previsão de clima encontrada; nenhuma previsão futura gerada")
	} else {
		for _, dateStr := range uniqueForecastDates {
			fd := forecastByDate[dateStr]
			f := DayFeatures{
				Date:          fd.Date,
				Irradiation:   *fd.IrradiationKWhM2,
				TempMean:      fd.TemperatureMeanC,
				Precipitation: fd.PrecipitationMM,
				Humidity:      fd.HumidityMeanPct,
			}

			for _, v := range validDevs {
				expected, err := v.model.Predict(f)
				if err != nil {
					continue
				}

				// Approximate 90% prediction band (+/- 1.645 * RMSE)
				lower := math.Max(0.0, expected-1.645*v.rmse)
				upper := math.Max(0.0, expected+1.645*v.rmse)

				pred := store.Prediction{
					DeviceID:    v.dev.ID,
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

	// 4. Upsert into database
	if err := st.UpsertPredictions(ctx, allPreds); err != nil {
		return nil, fmt.Errorf("upsert predições: %w", err)
	}

	res.Predictions = allPreds
	res.FutureDays = futurePreds

	if len(messages) > 0 {
		res.Message = strings.Join(messages, "; ")
	}

	// Sort future days by date and device
	sort.Slice(res.FutureDays, func(i, j int) bool {
		if res.FutureDays[i].Date == res.FutureDays[j].Date {
			return res.FutureDays[i].DeviceID < res.FutureDays[j].DeviceID
		}
		return res.FutureDays[i].Date < res.FutureDays[j].Date
	})

	return res, nil
}
