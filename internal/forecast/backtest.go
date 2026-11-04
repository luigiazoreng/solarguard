package forecast

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// BacktestOpts configures the walk-forward evaluation.
type BacktestOpts struct {
	MinTrainDays int // default 60
	RefitDays    int // default 7
}

// BacktestResult summarizes the evaluation across all models and devices for a plant.
type BacktestResult struct {
	PlantID              int64                                    `json:"plant_id"`
	PlantName            string                                   `json:"plant_name"`
	ActiveModel          string                                   `json:"active_model"`
	Message              string                                   `json:"message,omitempty"`
	WalkForwardPooled    map[string]Metrics                       `json:"walk_forward_pooled"`
	WalkForwardDevices   map[string]map[string]Metrics            `json:"walk_forward_devices"` // deviceName -> modelName -> Metrics
	Split8020Pooled      map[string]Metrics                       `json:"split_8020_pooled"`
	Split8020Devices     map[string]map[string]Metrics            `json:"split_8020_devices"`
	DeviceDayPredictions map[string]map[string]map[string]float64 `json:"-"`                    // deviceName -> date -> modelName -> pred
	DeviceResidualRMSE   map[string]float64                       `json:"device_residual_rmse"` // deviceName -> active model RMSE
	TrainBounds          [2]string                                `json:"train_bounds"`
	TestBounds           [2]string                                `json:"test_bounds"`
}

type deviceData struct {
	dev  domain.Device
	rows []TrainRow
}

// Backtest performs walk-forward evaluation and 80/20 chronological split for a plant.
func Backtest(ctx context.Context, st *store.Store, plantID int64, opts BacktestOpts) (*BacktestResult, error) {
	if opts.MinTrainDays <= 0 {
		opts.MinTrainDays = 60
	}
	if opts.RefitDays <= 0 {
		opts.RefitDays = 7
	}

	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return nil, fmt.Errorf("get plant %d: %w", plantID, err)
	}

	res := &BacktestResult{
		PlantID:              plant.ID,
		PlantName:            plant.Name,
		WalkForwardPooled:    make(map[string]Metrics),
		WalkForwardDevices:   make(map[string]map[string]Metrics),
		Split8020Pooled:      make(map[string]Metrics),
		Split8020Devices:     make(map[string]map[string]Metrics),
		DeviceDayPredictions: make(map[string]map[string]map[string]float64),
		DeviceResidualRMSE:   make(map[string]float64),
	}

	// Check weather link
	if plant.WeatherLocationID == nil {
		res.Message = fmt.Sprintf("planta %d (%s) não possui vínculo de clima; apenas persistência pode ser executada", plant.ID, plant.Name)
		return res, nil
	}

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		return nil, fmt.Errorf("carregar fuso horário %q: %w", plant.Timezone, err)
	}

	devices, err := st.ListDevices(ctx, plantID)
	if err != nil {
		return nil, fmt.Errorf("listar dispositivos da planta %d: %w", plantID, err)
	}

	var generatingDevices []domain.Device
	for _, d := range devices {
		if strings.EqualFold(d.Kind, "plant") {
			continue // Ignore devices with Kind "plant"
		}
		generatingDevices = append(generatingDevices, d)
	}

	if len(generatingDevices) == 0 {
		res.Message = fmt.Sprintf("planta %d (%s) não possui dispositivos de geração cadastrados", plant.ID, plant.Name)
		return res, nil
	}

	// 1. Gather all ok readings per device
	type devReading struct {
		date string
		kwh  float64
	}

	deviceReadings := make(map[int64][]devReading)
	minDate := "9999-99-99"
	maxDate := "0000-00-00"

	for _, d := range generatingDevices {
		rows, err := st.DB().QueryContext(ctx, `SELECT period_start, value
			FROM readings
			WHERE device_id = ? AND metric = 'energy_kwh' AND quality = 'ok' AND value IS NOT NULL
			ORDER BY period_start ASC`, d.ID)
		if err != nil {
			return nil, fmt.Errorf("consultar leituras do dispositivo %d: %w", d.ID, err)
		}

		var list []devReading
		for rows.Next() {
			var startStr string
			var val float64
			if err := rows.Scan(&startStr, &val); err != nil {
				rows.Close()
				return nil, err
			}
			t, err := time.Parse(time.RFC3339, startStr)
			if err != nil {
				rows.Close()
				return nil, err
			}
			localDay := t.In(loc).Format(time.DateOnly)
			list = append(list, devReading{date: localDay, kwh: val})
			if localDay < minDate {
				minDate = localDay
			}
			if localDay > maxDate {
				maxDate = localDay
			}
		}
		rows.Close()
		deviceReadings[d.ID] = list
	}

	if minDate > maxDate {
		res.Message = fmt.Sprintf("planta %d (%s) não possui leituras válidas (quality=ok)", plant.ID, plant.Name)
		return res, nil
	}

	// 2. Fetch observed weather
	weatherDays, err := st.PreferredObserved(ctx, *plant.WeatherLocationID, minDate, maxDate, []string{"inmet", "open-meteo"})
	if err != nil {
		return nil, fmt.Errorf("obter clima observado: %w", err)
	}

	weatherMap := make(map[string]TrainRow)
	for _, w := range weatherDays {
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
	}

	// 3. Assemble training rows per device
	var devDataList []deviceData
	var skippedDevices []string

	for _, d := range generatingDevices {
		list := deviceReadings[d.ID]
		var combined []TrainRow
		var prevKWh *float64

		for _, item := range list {
			w, ok := weatherMap[item.date]
			if !ok {
				continue // missing weather irradiation
			}
			row := TrainRow{
				Date:          item.date,
				KWh:           item.kwh,
				Irradiation:   w.Irradiation,
				TempMean:      w.TempMean,
				Precipitation: w.Precipitation,
				Humidity:      w.Humidity,
				PrevKWh:       prevKWh,
			}
			combined = append(combined, row)
			currVal := item.kwh
			prevKWh = &currVal
		}

		if len(combined) < opts.MinTrainDays {
			skippedDevices = append(skippedDevices, fmt.Sprintf("%s (%d linhas < %d)", d.Name, len(combined), opts.MinTrainDays))
			continue
		}

		devDataList = append(devDataList, deviceData{
			dev:  d,
			rows: combined,
		})
	}

	if len(skippedDevices) > 0 {
		warn := fmt.Sprintf("dispositivos com poucas linhas ignorados: %s", strings.Join(skippedDevices, ", "))
		if res.Message == "" {
			res.Message = warn
		} else {
			res.Message += "; " + warn
		}
	}

	if len(devDataList) == 0 {
		return res, nil
	}

	// Track overall bounds
	res.TrainBounds[0] = minDate
	res.TrainBounds[1] = maxDate

	// 4. Walk-forward per device
	type dayPredMap map[string]float64 // modelName -> predicted value

	// Device name -> modelName -> slice of (actual, pred)
	deviceWalkForwardPairs := make(map[string]map[string][][2]float64)
	device8020Pairs := make(map[string]map[string][][2]float64)

	// Pooled modelName -> slice of (actual, pred)
	pooledWalkForwardPairs := make(map[string][][2]float64)
	pooled8020Pairs := make(map[string][][2]float64)

	earliestTestDate := "9999-99-99"
	latestTestDate := "0000-00-00"

	for _, dd := range devDataList {
		devName := dd.dev.Name
		rows := dd.rows
		nRows := len(rows)

		// Instantiate models for this device
		var models []Model
		models = append(models, NewPersistenceModel())
		models = append(models, NewCalibratedModel())
		models = append(models, NewRidgeModel())

		if dd.dev.PanelAreaM2 != nil && dd.dev.Efficiency != nil {
			phys, err := NewPhysicalModel(dd.dev.PanelAreaM2, dd.dev.Efficiency)
			if err == nil {
				models = append(models, phys)
			}
		}

		// Raw walk-forward predictions: date -> modelName -> prediction
		rawWFPreds := make(map[string]dayPredMap)
		rawActuals := make(map[string]float64)

		for trainEnd := opts.MinTrainDays; trainEnd < nRows; trainEnd += opts.RefitDays {
			testEnd := trainEnd + opts.RefitDays
			if testEnd > nRows {
				testEnd = nRows
			}
			trainSlice := rows[0:trainEnd]

			// Fit models
			for _, m := range models {
				_ = m.Fit(trainSlice)
			}

			// Predict test slice
			for k := trainEnd; k < testEnd; k++ {
				r := rows[k]
				f := DayFeatures{
					Date:          r.Date,
					Irradiation:   r.Irradiation,
					TempMean:      r.TempMean,
					Precipitation: r.Precipitation,
					Humidity:      r.Humidity,
					PrevKWh:       r.PrevKWh,
				}
				if rawWFPreds[r.Date] == nil {
					rawWFPreds[r.Date] = make(dayPredMap)
				}
				rawActuals[r.Date] = r.KWh

				for _, m := range models {
					p, err := m.Predict(f)
					if err == nil {
						rawWFPreds[r.Date][m.Name()] = p
					}
				}
			}
		}

		// Find intersection of evaluation days across ALL evaluated models for this device
		var intersectionDates []string
		for date, predMap := range rawWFPreds {
			hasAll := true
			for _, m := range models {
				if _, ok := predMap[m.Name()]; !ok {
					hasAll = false
					break
				}
			}
			if hasAll {
				intersectionDates = append(intersectionDates, date)
			}
		}
		sort.Strings(intersectionDates)

		// Record day predictions for this device on intersection dates
		if res.DeviceDayPredictions[devName] == nil {
			res.DeviceDayPredictions[devName] = make(map[string]map[string]float64)
		}
		for _, date := range intersectionDates {
			res.DeviceDayPredictions[devName][date] = rawWFPreds[date]
			if date < earliestTestDate {
				earliestTestDate = date
			}
			if date > latestTestDate {
				latestTestDate = date
			}
		}

		// Compute walk-forward metrics on intersection dates
		deviceWalkForwardPairs[devName] = make(map[string][][2]float64)
		res.WalkForwardDevices[devName] = make(map[string]Metrics)

		for _, m := range models {
			mName := m.Name()
			var actuals, preds []float64
			for _, date := range intersectionDates {
				act := rawActuals[date]
				prd := rawWFPreds[date][mName]
				actuals = append(actuals, act)
				preds = append(preds, prd)
				deviceWalkForwardPairs[devName][mName] = append(deviceWalkForwardPairs[devName][mName], [2]float64{act, prd})
				pooledWalkForwardPairs[mName] = append(pooledWalkForwardPairs[mName], [2]float64{act, prd})
			}
			res.WalkForwardDevices[devName][mName] = ComputeMetrics(actuals, preds)
		}

		// 5. 80/20 Chronological split
		splitIdx := int(float64(nRows) * 0.8)
		if splitIdx < 1 {
			splitIdx = 1
		}
		train80 := rows[0:splitIdx]
		test20 := rows[splitIdx:]

		for _, m := range models {
			_ = m.Fit(train80)
		}

		raw8020Preds := make(map[string]dayPredMap)
		raw8020Actuals := make(map[string]float64)

		for _, r := range test20 {
			f := DayFeatures{
				Date:          r.Date,
				Irradiation:   r.Irradiation,
				TempMean:      r.TempMean,
				Precipitation: r.Precipitation,
				Humidity:      r.Humidity,
				PrevKWh:       r.PrevKWh,
			}
			raw8020Preds[r.Date] = make(dayPredMap)
			raw8020Actuals[r.Date] = r.KWh
			for _, m := range models {
				p, err := m.Predict(f)
				if err == nil {
					raw8020Preds[r.Date][m.Name()] = p
				}
			}
		}

		var splitDates []string
		for date, predMap := range raw8020Preds {
			hasAll := true
			for _, m := range models {
				if _, ok := predMap[m.Name()]; !ok {
					hasAll = false
					break
				}
			}
			if hasAll {
				splitDates = append(splitDates, date)
			}
		}
		sort.Strings(splitDates)

		device8020Pairs[devName] = make(map[string][][2]float64)
		res.Split8020Devices[devName] = make(map[string]Metrics)

		for _, m := range models {
			mName := m.Name()
			var actuals, preds []float64
			for _, date := range splitDates {
				act := raw8020Actuals[date]
				prd := raw8020Preds[date][mName]
				actuals = append(actuals, act)
				preds = append(preds, prd)
				device8020Pairs[devName][mName] = append(device8020Pairs[devName][mName], [2]float64{act, prd})
				pooled8020Pairs[mName] = append(pooled8020Pairs[mName], [2]float64{act, prd})
			}
			res.Split8020Devices[devName][mName] = ComputeMetrics(actuals, preds)
		}
	}

	if earliestTestDate <= latestTestDate {
		res.TestBounds[0] = earliestTestDate
		res.TestBounds[1] = latestTestDate
	}

	// 6. Compute pooled metrics
	for mName, pairs := range pooledWalkForwardPairs {
		actuals := make([]float64, len(pairs))
		preds := make([]float64, len(pairs))
		for i, p := range pairs {
			actuals[i] = p[0]
			preds[i] = p[1]
		}
		res.WalkForwardPooled[mName] = ComputeMetrics(actuals, preds)
	}

	for mName, pairs := range pooled8020Pairs {
		actuals := make([]float64, len(pairs))
		preds := make([]float64, len(pairs))
		for i, p := range pairs {
			actuals[i] = p[0]
			preds[i] = p[1]
		}
		res.Split8020Pooled[mName] = ComputeMetrics(actuals, preds)
	}

	// 7. Selection: lowest pooled walk-forward MAE among physical, calibrated, ridge, only if it beats persistence
	persistMetric, hasPersist := res.WalkForwardPooled["persistence"]
	candidateModels := []string{"ridge", "calibrated", "physical"}

	var bestModel string
	bestMAE := math.MaxFloat64

	for _, cand := range candidateModels {
		m, ok := res.WalkForwardPooled[cand]
		if !ok || m.N == 0 {
			continue
		}
		if m.MAE < bestMAE {
			bestMAE = m.MAE
			bestModel = cand
		}
	}

	if hasPersist && bestModel != "" && bestMAE < persistMetric.MAE {
		res.ActiveModel = bestModel
	} else {
		res.ActiveModel = ""
		msg := "nenhum modelo superou a persistência (repeat-yesterday); nenhum modelo ativo definido"
		if res.Message == "" {
			res.Message = msg
		} else {
			res.Message += "; " + msg
		}
	}

	// Record active model residual RMSE per device
	if res.ActiveModel != "" {
		for devName, devMap := range res.WalkForwardDevices {
			if m, ok := devMap[res.ActiveModel]; ok {
				res.DeviceResidualRMSE[devName] = m.RMSE
			}
		}
	}

	// 8. Store model runs in model_runs table
	allEvaluatedModels := []string{"persistence", "physical", "calibrated", "ridge"}
	now := time.Now().UTC()

	var activeRunID int64

	for _, mName := range allEvaluatedModels {
		if _, ok := res.WalkForwardPooled[mName]; !ok {
			continue
		}

		// Prepare metrics JSON
		metricsPayload := map[string]any{
			"walk_forward": map[string]any{
				"pooled":  res.WalkForwardPooled[mName],
				"devices": make(map[string]Metrics),
			},
			"fixed_split_80_20": map[string]any{
				"pooled":  res.Split8020Pooled[mName],
				"devices": make(map[string]Metrics),
			},
		}

		for devName, devMap := range res.WalkForwardDevices {
			if m, ok := devMap[mName]; ok {
				metricsPayload["walk_forward"].(map[string]any)["devices"].(map[string]Metrics)[devName] = m
			}
		}
		for devName, devMap := range res.Split8020Devices {
			if m, ok := devMap[mName]; ok {
				metricsPayload["fixed_split_80_20"].(map[string]any)["devices"].(map[string]Metrics)[devName] = m
			}
		}

		metricsJSONBytes, _ := json.Marshal(metricsPayload)

		// Prepare params JSON by fitting on all ok rows
		paramsPayload := map[string]any{
			"devices": make(map[string]any),
		}
		for _, dd := range devDataList {
			switch mName {
			case "calibrated":
				c := NewCalibratedModel()
				_ = c.Fit(dd.rows)
				paramsPayload["devices"].(map[string]any)[dd.dev.Name] = map[string]float64{"k": c.K()}
			case "physical":
				if dd.dev.PanelAreaM2 != nil && dd.dev.Efficiency != nil {
					paramsPayload["devices"].(map[string]any)[dd.dev.Name] = map[string]float64{
						"panel_area_m2": *dd.dev.PanelAreaM2,
						"efficiency":    *dd.dev.Efficiency,
					}
				}
			case "ridge":
				r := NewRidgeModel()
				_ = r.Fit(dd.rows)
				means, stds := r.ScalingParams()
				paramsPayload["devices"].(map[string]any)[dd.dev.Name] = map[string]any{
					"intercept":    r.Intercept(),
					"coefficients": r.Coefficients(),
					"means":        means,
					"stds":         stds,
				}
			case "persistence":
				paramsPayload["devices"].(map[string]any)[dd.dev.Name] = map[string]any{}
			}
		}
		paramsJSONBytes, _ := json.Marshal(paramsPayload)

		isActive := (mName == res.ActiveModel)
		trainStart := res.TrainBounds[0]
		trainEnd := res.TrainBounds[1]
		testStart := res.TestBounds[0]
		testEnd := res.TestBounds[1]

		mr, err := st.UpsertModelRun(ctx, store.ModelRun{
			Name:        mName,
			Version:     "v1",
			PlantID:     plant.ID,
			TrainedAt:   now,
			TrainStart:  &trainStart,
			TrainEnd:    &trainEnd,
			TestStart:   &testStart,
			TestEnd:     &testEnd,
			MetricsJSON: string(metricsJSONBytes),
			ParamsJSON:  string(paramsJSONBytes),
			Active:      isActive,
		})
		if err != nil {
			return nil, fmt.Errorf("upsert model run %s: %w", mName, err)
		}
		if isActive {
			activeRunID = mr.ID
		}
	}

	// Ensure exactly one active model run in store
	if err := st.SetActiveModelRun(ctx, plant.ID, activeRunID); err != nil {
		return nil, fmt.Errorf("set active model run: %w", err)
	}

	return res, nil
}
