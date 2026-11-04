package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/luigiazoreng/solarguard/internal/analytics"
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

type PredictionInput struct {
	Expected float64
	Lower    *float64
	Upper    *float64
}

// AggregatePredictions combines predictions across devices for a single date.
// The 90% band is calculated as: expected total +/- sqrt(sum of squared half-widths),
// where each half-width = upper - expected; lower bound is clamped at 0.
func AggregatePredictions(preds []PredictionInput) (totalExpected float64, bandLower float64, bandUpper float64) {
	if len(preds) == 0 {
		return 0, 0, 0
	}
	var sumExpected float64
	var sumSquaredHW float64
	for _, p := range preds {
		sumExpected += p.Expected
		hw := 0.0
		if p.Upper != nil && *p.Upper >= p.Expected {
			hw = *p.Upper - p.Expected
		} else if p.Lower != nil && *p.Lower <= p.Expected {
			hw = p.Expected - *p.Lower
		}
		sumSquaredHW += hw * hw
	}
	bandHW := math.Sqrt(sumSquaredHW)
	bandLower = math.Max(0, sumExpected-bandHW)
	bandUpper = sumExpected + bandHW
	return sumExpected, bandLower, bandUpper
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

	// Clamp the end of the range for cards to the plant's newest reading date
	newestReadingDate, _ := s.store.GetPlantNewestReadingDate(ctx, plantID)
	cardFromDate := fromDate
	cardToDate := toDate
	var dataUntilStr string

	if newestReadingDate != "" && newestReadingDate < toDate {
		cardToDate = newestReadingDate
		if t, err := time.Parse(time.DateOnly, newestReadingDate); err == nil {
			dataUntilStr = t.Format("02/01/2006")
		} else {
			dataUntilStr = newestReadingDate
		}
	}
	if cardFromDate > cardToDate {
		cardFromDate = cardToDate
	}

	// Compute metrics using single source of truth: internal/analytics
	genSummary, _ := analytics.Generation(ctx, s.store, plantID, devIDPtr, cardFromDate, cardToDate, "day")
	evaSummary, _ := analytics.ExpectedVsActual(ctx, s.store, plantID, devIDPtr, cardFromDate, cardToDate)
	lostSummary, _ := analytics.LostEnergy(ctx, s.store, plantID, cardFromDate, cardToDate)
	dqSummary, _ := analytics.DataQuality(ctx, s.store, plantID, cardFromDate, cardToDate)

	periodMeasured := genSummary.TotalMeasuredKWh

	pairedDeviceDays := evaSummary.PairedDeviceDays
	pairedMeasured := evaSummary.PairedMeasuredKWh
	pairedExpected := evaSummary.PairedExpectedKWh
	hasPaired := pairedDeviceDays > 0 && pairedExpected > 0
	var pairedDeviation float64
	if hasPaired {
		pairedDeviation = ((pairedMeasured - pairedExpected) / pairedExpected) * 100.0
	}

	periodLostEnergy := lostSummary.LostEnergyKWh

	var okReadings, totalReadings int
	var qualityPct float64
	if devIDPtr != nil {
		for _, d := range dqSummary.Devices {
			if d.DeviceID == *devIDPtr {
				okReadings = d.OKDays
				totalReadings = d.TotalDays
				qualityPct = d.PercentOK
				break
			}
		}
	} else {
		for _, d := range dqSummary.Devices {
			okReadings += d.OKDays
			totalReadings += d.TotalDays
		}
		if totalReadings > 0 {
			qualityPct = (float64(okReadings) / float64(totalReadings)) * 100.0
		}
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
	data["TotalDevices"] = len(devices)
	data["Period"] = period
	data["PeriodLabel"] = periodLabel
	data["FromDate"] = fromDate
	data["ToDate"] = toDate
	data["DataUntilStr"] = dataUntilStr
	data["SelectedDevice"] = selectedDevice
	data["PeriodMeasured"] = periodMeasured
	data["HasPaired"] = hasPaired
	data["PairedMeasured"] = pairedMeasured
	data["PairedExpected"] = pairedExpected
	data["PairedDeviation"] = pairedDeviation
	data["PairedDeviceDays"] = pairedDeviceDays
	data["PeriodLostEnergy"] = periodLostEnergy
	data["PeriodQualityPct"] = qualityPct
	data["PeriodOKReadings"] = okReadings
	data["PeriodTotalReadings"] = totalReadings
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

	var measuredOK []*float64
	var measuredNotOK []NotOKReading

	for _, d := range dates {
		entry := byDate[d]
		if entry.hasNotOK {
			measuredOK = append(measuredOK, nil)
			measuredNotOK = append(measuredNotOK, entry.notOKValues...)
		} else if entry.hasOK {
			v := entry.measuredOKSum
			measuredOK = append(measuredOK, &v)
		} else {
			// No reading on this date (offline / missing): gap (nil), not zero!
			measuredOK = append(measuredOK, nil)
		}
	}

	// 3. Fetch predictions in [from, to] + future
	futureTo := tTo.AddDate(0, 0, 14).Format(time.DateOnly)
	allPreds, _ := s.store.ListPredictions(ctx, plantID, devIDPtr, from, futureTo)

	predsByDate := make(map[string][]PredictionInput)
	for _, p := range allPreds {
		predsByDate[p.Date] = append(predsByDate[p.Date], PredictionInput{
			Expected: p.KWhExpected,
			Lower:    p.KWhLower,
			Upper:    p.KWhUpper,
		})
	}

	var expected []*float64
	var bandLower []*float64
	var bandUpper []*float64

	for _, d := range dates {
		items := predsByDate[d]
		if len(items) > 0 {
			totExp, bLow, bUp := AggregatePredictions(items)
			expected = append(expected, &totExp)
			bandLower = append(bandLower, &bLow)
			bandUpper = append(bandUpper, &bUp)
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
		items := predsByDate[dStr]
		if len(items) > 0 {
			totExp, _, _ := AggregatePredictions(items)
			forecastValues = append(forecastValues, &totExp)
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
