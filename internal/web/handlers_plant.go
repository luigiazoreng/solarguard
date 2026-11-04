package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

type NotOKReading struct {
	Date   string  `json:"date"`
	Value  float64 `json:"value"`
	Reason string  `json:"reason"`
}

type ModelMetricRow struct {
	Label string
	MAE   float64
	RMSE  float64
	R2    float64
	WAPE  float64
}

func (s *Server) handlePlantDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	plantID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	plant, err := s.store.GetPlant(ctx, plantID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.cfg.Logger.Error("get plant", "id", plantID, "err", err)
		http.Error(w, "Erro ao carregar usina", http.StatusInternalServerError)
		return
	}

	devices, err := s.store.ListDevices(ctx, plantID)
	if err != nil {
		s.cfg.Logger.Error("list devices", "plant_id", plantID, "err", err)
		http.Error(w, "Erro ao listar dispositivos", http.StatusInternalServerError)
		return
	}

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		loc = time.UTC
	}
	now := s.cfg.Now().In(loc)
	yesterday := now.AddDate(0, 0, -1).Format(time.DateOnly)

	// Period filter
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "30"
	}
	selectedDevice := r.URL.Query().Get("device")

	var fromDate, toDate, periodLabel string
	toDate = yesterday

	switch period {
	case "7":
		fromDate = now.AddDate(0, 0, -7).Format(time.DateOnly)
		periodLabel = "Últimos 7 dias"
	case "90":
		fromDate = now.AddDate(0, 0, -90).Format(time.DateOnly)
		periodLabel = "Últimos 90 dias"
	case "all":
		firstReading, _ := s.store.GetPlantFirstReadingDate(ctx, plantID)
		if firstReading != "" {
			fromDate = firstReading
		} else {
			fromDate = now.AddDate(0, 0, -365).Format(time.DateOnly)
		}
		periodLabel = "Todo o histórico"
	default: // 30
		period = "30"
		fromDate = now.AddDate(0, 0, -30).Format(time.DateOnly)
		periodLabel = "Últimos 30 dias"
	}

	// Device filter for metrics
	var devIDPtr *int64
	if selectedDevice != "" {
		if id, err := strconv.ParseInt(selectedDevice, 10, 64); err == nil {
			devIDPtr = &id
		}
	}

	// Calculate period metrics
	fStart, _, _ := domain.LocalDay(fromDate, plant.Timezone)
	_, tEnd, _ := domain.LocalDay(toDate, plant.Timezone)

	readings, _ := s.store.ListReadingsByPlant(ctx, plantID, fStart, tEnd)
	var periodMeasured float64
	var periodOKCount, periodTotalCount int
	for _, rd := range readings {
		if devIDPtr != nil && rd.DeviceID != *devIDPtr {
			continue
		}
		if rd.Metric == domain.MetricEnergyKWh {
			periodTotalCount++
			if rd.Value != nil {
				periodMeasured += *rd.Value
			}
			if rd.Quality == domain.QualityOK {
				periodOKCount++
			}
		}
	}

	preds, _ := s.store.ListPredictions(ctx, plantID, devIDPtr, fromDate, toDate)
	var periodExpected float64
	for _, p := range preds {
		periodExpected += p.KWhExpected
	}

	lostEnergy, _ := ComputeLostEnergy(ctx, s.store, plantID, devIDPtr, fromDate, toDate)

	var qualityPct float64
	if periodTotalCount > 0 {
		qualityPct = (float64(periodOKCount) / float64(periodTotalCount)) * 100.0
	}

	// Device summaries
	devSummaries, _ := s.store.GetPlantDeviceSummaries(ctx, plantID)

	// Active model & metrics
	activeModel, err := s.store.GetActiveModelRun(ctx, plantID)
	hasActiveModel := err == nil
	var modelMetrics []ModelMetricRow
	activeModelName := "Nenhum"

	if hasActiveModel {
		activeModelName = activeModel.Name
		if activeModel.MetricsJSON != "" {
			var parsed struct {
				WalkForward struct {
					Pooled struct {
						MAE  float64 `json:"mae"`
						RMSE float64 `json:"rmse"`
						R2   float64 `json:"r2"`
						WAPE float64 `json:"wape"`
					} `json:"pooled"`
					Devices map[string]struct {
						MAE  float64 `json:"mae"`
						RMSE float64 `json:"rmse"`
						R2   float64 `json:"r2"`
						WAPE float64 `json:"wape"`
					} `json:"devices"`
				} `json:"walk_forward"`
			}
			if err := json.Unmarshal([]byte(activeModel.MetricsJSON), &parsed); err == nil {
				devNames := make([]string, 0, len(parsed.WalkForward.Devices))
				for name := range parsed.WalkForward.Devices {
					devNames = append(devNames, name)
				}
				sort.Strings(devNames)

				for _, name := range devNames {
					m := parsed.WalkForward.Devices[name]
					modelMetrics = append(modelMetrics, ModelMetricRow{
						Label: fmt.Sprintf("Dispositivo %s", name),
						MAE:   m.MAE,
						RMSE:  m.RMSE,
						R2:    m.R2,
						WAPE:  m.WAPE,
					})
				}
				if parsed.WalkForward.Pooled.MAE > 0 || parsed.WalkForward.Pooled.RMSE > 0 {
					modelMetrics = append(modelMetrics, ModelMetricRow{
						Label: "Geral (Pooled)",
						MAE:   parsed.WalkForward.Pooled.MAE,
						RMSE:  parsed.WalkForward.Pooled.RMSE,
						R2:    parsed.WalkForward.Pooled.R2,
						WAPE:  parsed.WalkForward.Pooled.WAPE,
					})
				}
			}
		}
	}

	data := s.baseTemplateData(r, plant.Name, "overview")
	data["Plant"] = plant
	data["Devices"] = devices
	data["Period"] = period
	data["PeriodLabel"] = periodLabel
	data["FromDate"] = fromDate
	data["ToDate"] = toDate
	data["SelectedDevice"] = selectedDevice
	data["PeriodMeasured"] = periodMeasured
	data["PeriodExpected"] = periodExpected
	data["PeriodLostEnergy"] = lostEnergy
	data["PeriodQualityPct"] = qualityPct
	data["PeriodOKReadings"] = periodOKCount
	data["PeriodTotalReadings"] = periodTotalCount
	data["ActiveModelName"] = activeModelName
	data["DeviceSummaries"] = devSummaries
	if hasActiveModel {
		data["ActiveModel"] = &activeModel
		data["ModelMetrics"] = modelMetrics
	} else {
		data["ActiveModel"] = nil
	}

	if err := s.render(w, "plant", data); err != nil {
		s.cfg.Logger.Error("render plant", "err", err)
	}
}

func (s *Server) handlePlantSeries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	plantID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	plant, err := s.store.GetPlant(ctx, plantID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Erro ao obter usina", http.StatusInternalServerError)
		return
	}

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		loc = time.UTC
	}
	now := s.cfg.Now().In(loc)

	// Validate from and to
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	if from == "" {
		from = now.AddDate(0, 0, -30).Format(time.DateOnly)
	}
	if to == "" {
		to = now.AddDate(0, 0, -1).Format(time.DateOnly)
	}

	tFrom, err := time.Parse(time.DateOnly, from)
	if err != nil {
		http.Error(w, "Data inicial inválida (formato esperado YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	tTo, err := time.Parse(time.DateOnly, to)
	if err != nil {
		http.Error(w, "Data final inválida (formato esperado YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	if tFrom.After(tTo) {
		http.Error(w, "Data inicial posterior à data final", http.StatusBadRequest)
		return
	}

	var devIDPtr *int64
	devParam := r.URL.Query().Get("device")
	if devParam != "" {
		dID, err := strconv.ParseInt(devParam, 10, 64)
		if err != nil {
			http.Error(w, "ID de dispositivo inválido", http.StatusBadRequest)
			return
		}
		devIDPtr = &dID
	}

	// 1. Enumerate all dates in [from, to]
	var dates []string
	cur := tFrom
	for !cur.After(tTo) {
		dates = append(dates, cur.Format(time.DateOnly))
		cur = cur.AddDate(0, 0, 1)
	}

	// 2. Fetch readings in UTC
	fStart, _, _ := domain.LocalDay(from, plant.Timezone)
	_, tEnd, _ := domain.LocalDay(to, plant.Timezone)
	readings, err := s.store.ListReadingsByPlant(ctx, plantID, fStart, tEnd)
	if err != nil {
		http.Error(w, "Erro ao consultar leituras", http.StatusInternalServerError)
		return
	}

	// Group readings by date
	type dayReadings struct {
		measuredOKSum float64
		hasOK         bool
		hasNotOK      bool
		notOKValues   []NotOKReading
	}
	byDate := make(map[string]*dayReadings)
	for _, d := range dates {
		byDate[d] = &dayReadings{}
	}

	for _, rd := range readings {
		if rd.Metric != domain.MetricEnergyKWh {
			continue
		}
		if devIDPtr != nil && rd.DeviceID != *devIDPtr {
			continue
		}
		dStr := rd.PeriodStart.In(loc).Format(time.DateOnly)
		entry, ok := byDate[dStr]
		if !ok {
			continue
		}

		if rd.Quality == domain.QualityOK && rd.Value != nil {
			entry.measuredOKSum += *rd.Value
			entry.hasOK = true
		} else {
			entry.hasNotOK = true
			val := 0.0
			if rd.Value != nil {
				val = *rd.Value
			}
			entry.notOKValues = append(entry.notOKValues, NotOKReading{
				Date:   dStr,
				Value:  val,
				Reason: rd.QualityReason,
			})
		}
	}

	// 3. Fetch predictions in [from, to] + future
	futureTo := tTo.AddDate(0, 0, 14).Format(time.DateOnly)
	allPreds, _ := s.store.ListPredictions(ctx, plantID, devIDPtr, from, futureTo)

	type predEntry struct {
		expected float64
		lower    *float64
		upper    *float64
	}
	predsByDate := make(map[string]*predEntry)
	for _, p := range allPreds {
		pe, exists := predsByDate[p.Date]
		if !exists {
			pe = &predEntry{lower: p.KWhLower, upper: p.KWhUpper}
			predsByDate[p.Date] = pe
		}
		pe.expected += p.KWhExpected
	}

	var measuredOK []*float64
	var measuredNotOK []NotOKReading
	var expected []*float64
	var bandLower []*float64
	var bandUpper []*float64

	for _, d := range dates {
		entry := byDate[d]
		if entry.hasNotOK {
			measuredOK = append(measuredOK, nil)
			measuredNotOK = append(measuredNotOK, entry.notOKValues...)
		} else if entry.hasOK {
			v := entry.measuredOKSum
			measuredOK = append(measuredOK, &v)
		} else {
			measuredOK = append(measuredOK, nil)
		}

		if pe, ok := predsByDate[d]; ok {
			eVal := pe.expected
			expected = append(expected, &eVal)
			bandLower = append(bandLower, pe.lower)
			bandUpper = append(bandUpper, pe.upper)
		} else {
			expected = append(expected, nil)
			bandLower = append(bandLower, nil)
			bandUpper = append(bandUpper, nil)
		}
	}

	// 4. Future forecast
	var forecastDates []string
	var forecastValues []*float64

	for d := tTo.AddDate(0, 0, 1); !d.After(tTo.AddDate(0, 0, 7)); d = d.AddDate(0, 0, 1) {
		dStr := d.Format(time.DateOnly)
		forecastDates = append(forecastDates, dStr)
		if pe, ok := predsByDate[dStr]; ok {
			v := pe.expected
			forecastValues = append(forecastValues, &v)
		} else {
			forecastValues = append(forecastValues, nil)
		}
	}

	resp := map[string]any{
		"plant_id":        plantID,
		"from":            from,
		"to":              to,
		"dates":           dates,
		"measured_ok":     measuredOK,
		"measured_not_ok": measuredNotOK,
		"expected":        expected,
		"band_lower":      bandLower,
		"band_upper":      bandUpper,
		"forecast_dates":  forecastDates,
		"forecast_values": forecastValues,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}
