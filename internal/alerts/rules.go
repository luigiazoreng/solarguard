package alerts

import (
	"fmt"
	"sort"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
)

type readingItem struct {
	Reading domain.Reading
	DevName string
	Date    string // YYYY-MM-DD in plant timezone
}

type dayData struct {
	Date       string
	Value      *float64
	RawValue   string
	Reason     string
	Quality    domain.Quality
	Expected   *float64
	PeerMedian *float64
}

type incident struct {
	Rule       string
	PlantID    int64
	DeviceID   *int64
	DevName    string
	Severity   string
	Title      string
	FirstDate  string
	LastDate   string
	DetectedOn string
	NumDays    int
	Values     []DayValue
	Params     map[string]any
}

func (inc incident) DedupeKey() string {
	if inc.DeviceID == nil {
		return fmt.Sprintf("%s|plant|%s", inc.Rule, inc.FirstDate)
	}
	return fmt.Sprintf("%s|%d|%s", inc.Rule, *inc.DeviceID, inc.FirstDate)
}

func evaluateStuckValue(plantID int64, dev domain.Device, items []readingItem, cfgParams map[string]any) []incident {
	minDays := getParamInt(cfgParams, "min_days", 3)
	maxGapDays := getParamInt(cfgParams, "max_gap_days", 5)

	effectiveParams := map[string]any{
		"min_days":     minDays,
		"max_gap_days": maxGapDays,
	}

	var incidents []incident
	var current []readingItem

	flush := func() {
		if len(current) >= minDays {
			first := current[0].Date
			last := current[len(current)-1].Date
			detected := current[minDays-1].Date

			values := make([]DayValue, len(current))
			for i, it := range current {
				values[i] = DayValue{
					Date:     it.Date,
					Value:    it.Reading.Value,
					RawValue: it.Reading.RawValue,
					Reason:   it.Reading.QualityReason,
				}
			}

			devID := dev.ID
			incidents = append(incidents, incident{
				Rule:       "stuck_value",
				PlantID:    plantID,
				DeviceID:   &devID,
				DevName:    dev.Name,
				Severity:   "critical",
				Title:      fmt.Sprintf("Leitura travada em %s", dev.Name),
				FirstDate:  first,
				LastDate:   last,
				DetectedOn: detected,
				NumDays:    len(current),
				Values:     values,
				Params:     effectiveParams,
			})
		}
		current = nil
	}

	for _, it := range items {
		if it.Reading.QualityReason == "stuck_value" {
			if len(current) == 0 {
				current = append(current, it)
			} else {
				prev := current[len(current)-1]
				gap := daysBetween(prev.Date, it.Date) - 1
				if gap <= maxGapDays {
					current = append(current, it)
				} else {
					flush()
					current = append(current, it)
				}
			}
		} else {
			if len(current) > 0 {
				flush()
			}
		}
	}
	flush()
	return incidents
}

func evaluateZeroGeneration(plantID int64, dev domain.Device, items []readingItem, cfgParams map[string]any) []incident {
	maxGapDays := getParamInt(cfgParams, "max_gap_days", 5)
	criticalDays := getParamInt(cfgParams, "critical_days", 3)

	effectiveParams := map[string]any{
		"max_gap_days":  maxGapDays,
		"critical_days": criticalDays,
	}

	var incidents []incident
	var current []readingItem

	flush := func() {
		if len(current) >= 1 {
			first := current[0].Date
			last := current[len(current)-1].Date
			detected := current[0].Date

			values := make([]DayValue, len(current))
			for i, it := range current {
				values[i] = DayValue{
					Date:     it.Date,
					Value:    it.Reading.Value,
					RawValue: it.Reading.RawValue,
					Reason:   it.Reading.QualityReason,
				}
			}

			sev := "warning"
			if len(current) >= criticalDays {
				sev = "critical"
			}

			devID := dev.ID
			incidents = append(incidents, incident{
				Rule:       "zero_generation",
				PlantID:    plantID,
				DeviceID:   &devID,
				DevName:    dev.Name,
				Severity:   sev,
				Title:      fmt.Sprintf("Geração zerada em %s", dev.Name),
				FirstDate:  first,
				LastDate:   last,
				DetectedOn: detected,
				NumDays:    len(current),
				Values:     values,
				Params:     effectiveParams,
			})
		}
		current = nil
	}

	for _, it := range items {
		reason := it.Reading.QualityReason
		if reason == "zero_run" || reason == "zero_with_peers_generating" {
			if len(current) == 0 {
				current = append(current, it)
			} else {
				prev := current[len(current)-1]
				gap := daysBetween(prev.Date, it.Date) - 1
				if gap <= maxGapDays {
					current = append(current, it)
				} else {
					flush()
					current = append(current, it)
				}
			}
		} else {
			if len(current) > 0 {
				flush()
			}
		}
	}
	flush()
	return incidents
}

func evaluateImplausibleReading(plantID int64, dev domain.Device, items []readingItem, cfgParams map[string]any) []incident {
	effectiveParams := map[string]any{}

	var incidents []incident
	var current []readingItem

	flush := func() {
		if len(current) >= 1 {
			first := current[0].Date
			last := current[len(current)-1].Date
			detected := current[0].Date

			values := make([]DayValue, len(current))
			for i, it := range current {
				values[i] = DayValue{
					Date:     it.Date,
					Value:    it.Reading.Value,
					RawValue: it.Reading.RawValue,
					Reason:   it.Reading.QualityReason,
				}
			}

			devID := dev.ID
			incidents = append(incidents, incident{
				Rule:       "implausible_reading",
				PlantID:    plantID,
				DeviceID:   &devID,
				DevName:    dev.Name,
				Severity:   "warning",
				Title:      fmt.Sprintf("Leitura implausível em %s", dev.Name),
				FirstDate:  first,
				LastDate:   last,
				DetectedOn: detected,
				NumDays:    len(current),
				Values:     values,
				Params:     effectiveParams,
			})
		}
		current = nil
	}

	for _, it := range items {
		if it.Reading.QualityReason == "implausible_high" {
			if len(current) == 0 {
				current = append(current, it)
			} else {
				prev := current[len(current)-1]
				if daysBetween(prev.Date, it.Date) == 1 {
					current = append(current, it)
				} else {
					flush()
					current = append(current, it)
				}
			}
		} else {
			if len(current) > 0 {
				flush()
			}
		}
	}
	flush()
	return incidents
}

func evaluatePlantNoGeneration(plantID int64, devices []domain.Device, byDate map[string]map[int64]domain.Reading, allDates []string, cfgParams map[string]any) []incident {
	var nonPlantDevs []domain.Device
	for _, d := range devices {
		if d.Kind != "plant" {
			nonPlantDevs = append(nonPlantDevs, d)
		}
	}
	if len(nonPlantDevs) == 0 {
		return nil
	}

	effectiveParams := map[string]any{}

	var incidents []incident
	var current []string

	flush := func() {
		if len(current) >= 1 {
			first := current[0]
			last := current[len(current)-1]
			detected := current[0]

			values := make([]DayValue, len(current))
			for i, dStr := range current {
				values[i] = DayValue{Date: dStr}
			}

			incidents = append(incidents, incident{
				Rule:       "plant_no_generation",
				PlantID:    plantID,
				DeviceID:   nil,
				DevName:    "Usina",
				Severity:   "critical",
				Title:      "Usina sem geração",
				FirstDate:  first,
				LastDate:   last,
				DetectedOn: detected,
				NumDays:    len(current),
				Values:     values,
				Params:     effectiveParams,
			})
		}
		current = nil
	}

	for _, dStr := range allDates {
		devMap := byDate[dStr]
		allZeroOrMissing := true
		hasZeroReason := false

		for _, d := range nonPlantDevs {
			r, exists := devMap[d.ID]
			if !exists {
				// no reading counts as zero or missing
				continue
			}
			if r.Value != nil && *r.Value == 0 {
				if r.QualityReason == "zero_no_peer_evidence" || r.QualityReason == "zero_run" {
					hasZeroReason = true
				}
			} else {
				// device has a non-zero value or nil value
				allZeroOrMissing = false
				break
			}
		}

		if allZeroOrMissing && hasZeroReason {
			if len(current) == 0 {
				current = append(current, dStr)
			} else {
				prev := current[len(current)-1]
				if daysBetween(prev, dStr) == 1 {
					current = append(current, dStr)
				} else {
					flush()
					current = append(current, dStr)
				}
			}
		} else {
			if len(current) > 0 {
				flush()
			}
		}
	}
	flush()
	return incidents
}

func evaluateMissingData(plantID int64, devices []domain.Device, devReadings map[int64][]readingItem, plantEarliest, cutoffDate, todayStr string, cfgParams map[string]any) []incident {
	effectiveParams := map[string]any{}

	var nonPlantDevs []domain.Device
	for _, d := range devices {
		if d.Kind != "plant" {
			nonPlantDevs = append(nonPlantDevs, d)
		}
	}
	if len(nonPlantDevs) == 0 || plantEarliest == "" || cutoffDate == "" {
		return nil
	}

	// Index existing reading dates per device
	devDateSets := make(map[int64]map[string]bool)
	devEarliest := make(map[int64]string)
	devLatest := make(map[int64]string)

	for _, d := range nonPlantDevs {
		devDateSets[d.ID] = make(map[string]bool)
		items := devReadings[d.ID]
		for _, it := range items {
			devDateSets[d.ID][it.Date] = true
			if devEarliest[d.ID] == "" || it.Date < devEarliest[d.ID] {
				devEarliest[d.ID] = it.Date
			}
			if devLatest[d.ID] == "" || it.Date > devLatest[d.ID] {
				devLatest[d.ID] = it.Date
			}
		}
	}

	tEarliest, err1 := time.Parse(time.DateOnly, plantEarliest)
	tCutoff, err2 := time.Parse(time.DateOnly, cutoffDate)
	if err1 != nil || err2 != nil || tCutoff.Before(tEarliest) {
		return nil
	}

	// 1. Identify plant-wide missing dates: all devices have no reading
	plantMissingSet := make(map[string]bool)
	var plantMissingDates []string

	cur := tEarliest
	for !cur.After(tCutoff) {
		dStr := cur.Format(time.DateOnly)
		// Never flag today
		if dStr < todayStr {
			anyPresent := false
			for _, d := range nonPlantDevs {
				if devDateSets[d.ID][dStr] {
					anyPresent = true
					break
				}
			}
			if !anyPresent {
				plantMissingSet[dStr] = true
				plantMissingDates = append(plantMissingDates, dStr)
			}
		}
		cur = cur.AddDate(0, 0, 1)
	}

	var incidents []incident

	// Emit plant-level missing incidents
	var currentPlant []string
	flushPlant := func() {
		if len(currentPlant) >= 1 {
			first := currentPlant[0]
			last := currentPlant[len(currentPlant)-1]
			detected := currentPlant[0]

			values := make([]DayValue, len(currentPlant))
			for i, dStr := range currentPlant {
				values[i] = DayValue{Date: dStr}
			}

			incidents = append(incidents, incident{
				Rule:       "missing_data",
				PlantID:    plantID,
				DeviceID:   nil,
				DevName:    "Usina",
				Severity:   "warning",
				Title:      "Dados ausentes na usina",
				FirstDate:  first,
				LastDate:   last,
				DetectedOn: detected,
				NumDays:    len(currentPlant),
				Values:     values,
				Params:     effectiveParams,
			})
		}
		currentPlant = nil
	}

	for _, dStr := range plantMissingDates {
		if len(currentPlant) == 0 {
			currentPlant = append(currentPlant, dStr)
		} else {
			prev := currentPlant[len(currentPlant)-1]
			if daysBetween(prev, dStr) == 1 {
				currentPlant = append(currentPlant, dStr)
			} else {
				flushPlant()
				currentPlant = append(currentPlant, dStr)
			}
		}
	}
	flushPlant()

	// 2. Identify device-level missing dates
	for _, d := range nonPlantDevs {
		firstD := devEarliest[d.ID]
		lastD := devLatest[d.ID]
		if firstD == "" || firstD == lastD {
			continue
		}

		tDevStart, _ := time.Parse(time.DateOnly, firstD)
		tDevEnd, _ := time.Parse(time.DateOnly, lastD)
		if tDevEnd.After(tCutoff) {
			tDevEnd = tCutoff
		}

		var devMissingDates []string
		curD := tDevStart
		for !curD.After(tDevEnd) {
			dStr := curD.Format(time.DateOnly)
			if dStr < todayStr && !plantMissingSet[dStr] {
				if !devDateSets[d.ID][dStr] {
					devMissingDates = append(devMissingDates, dStr)
				}
			}
			curD = curD.AddDate(0, 0, 1)
		}

		var currentDev []string
		devID := d.ID
		devName := d.Name

		flushDev := func() {
			if len(currentDev) >= 1 {
				first := currentDev[0]
				last := currentDev[len(currentDev)-1]
				detected := currentDev[0]

				values := make([]DayValue, len(currentDev))
				for i, dStr := range currentDev {
					values[i] = DayValue{Date: dStr}
				}

				did := devID
				incidents = append(incidents, incident{
					Rule:       "missing_data",
					PlantID:    plantID,
					DeviceID:   &did,
					DevName:    devName,
					Severity:   "warning",
					Title:      fmt.Sprintf("Dados ausentes em %s", devName),
					FirstDate:  first,
					LastDate:   last,
					DetectedOn: detected,
					NumDays:    len(currentDev),
					Values:     values,
					Params:     effectiveParams,
				})
			}
			currentDev = nil
		}

		for _, dStr := range devMissingDates {
			if len(currentDev) == 0 {
				currentDev = append(currentDev, dStr)
			} else {
				prev := currentDev[len(currentDev)-1]
				if daysBetween(prev, dStr) == 1 {
					currentDev = append(currentDev, dStr)
				} else {
					flushDev()
					currentDev = append(currentDev, dStr)
				}
			}
		}
		flushDev()
	}

	return incidents
}

func evaluatePeerDivergence(plantID int64, devices []domain.Device, byDate map[string]map[int64]domain.Reading, allDates []string, cfgParams map[string]any) []incident {
	minDays := getParamInt(cfgParams, "min_days", 3)
	ratio := getParamFloat(cfgParams, "ratio", 0.70)
	minPeerKWh := getParamFloat(cfgParams, "min_peer_kwh", 5.0)

	effectiveParams := map[string]any{
		"min_days":     minDays,
		"ratio":        ratio,
		"min_peer_kwh": minPeerKWh,
	}

	var nonPlantDevs []domain.Device
	for _, d := range devices {
		if d.Kind != "plant" {
			nonPlantDevs = append(nonPlantDevs, d)
		}
	}
	if len(nonPlantDevs) < 2 {
		return nil // 1-device plant skips peer rules
	}

	var incidents []incident

	for _, d := range nonPlantDevs {
		var current []dayData

		devID := d.ID
		devName := d.Name

		flush := func() {
			if len(current) >= minDays {
				first := current[0].Date
				last := current[len(current)-1].Date
				detected := current[minDays-1].Date

				values := make([]DayValue, len(current))
				for i, it := range current {
					values[i] = DayValue{
						Date:       it.Date,
						Value:      it.Value,
						PeerMedian: it.PeerMedian,
					}
				}

				did := devID
				incidents = append(incidents, incident{
					Rule:       "peer_divergence",
					PlantID:    plantID,
					DeviceID:   &did,
					DevName:    devName,
					Severity:   "warning",
					Title:      fmt.Sprintf("Divergência de pares em %s", devName),
					FirstDate:  first,
					LastDate:   last,
					DetectedOn: detected,
					NumDays:    len(current),
					Values:     values,
					Params:     effectiveParams,
				})
			}
			current = nil
		}

		for _, dStr := range allDates {
			devMap := byDate[dStr]
			r, hasReading := devMap[d.ID]
			if !hasReading || r.Quality != domain.QualityOK || r.Value == nil {
				flush()
				continue
			}

			// Collect OK peer values
			var peerVals []float64
			for _, peer := range nonPlantDevs {
				if peer.ID == d.ID {
					continue
				}
				peerR, peerExists := devMap[peer.ID]
				if peerExists && peerR.Quality == domain.QualityOK && peerR.Value != nil {
					peerVals = append(peerVals, *peerR.Value)
				}
			}

			if len(peerVals) < 1 {
				flush()
				continue
			}

			peerMed := median(peerVals)
			if peerMed >= minPeerKWh && *r.Value < ratio*peerMed {
				medCopy := peerMed
				data := dayData{
					Date:       dStr,
					Value:      r.Value,
					PeerMedian: &medCopy,
				}
				if len(current) == 0 {
					current = append(current, data)
				} else {
					prev := current[len(current)-1]
					if daysBetween(prev.Date, dStr) == 1 {
						current = append(current, data)
					} else {
						flush()
						current = append(current, data)
					}
				}
			} else {
				flush()
			}
		}
		flush()
	}

	return incidents
}

func evaluateUnderExpected(plantID int64, dev domain.Device, items []readingItem, preds map[string]float64, cfgParams map[string]any) []incident {
	minDays := getParamInt(cfgParams, "min_days", 3)
	ratio := getParamFloat(cfgParams, "ratio", 0.70)
	minExpectedKWh := getParamFloat(cfgParams, "min_expected_kwh", 5.0)

	effectiveParams := map[string]any{
		"min_days":         minDays,
		"ratio":            ratio,
		"min_expected_kwh": minExpectedKWh,
	}

	var incidents []incident
	var current []dayData

	devID := dev.ID
	devName := dev.Name

	flush := func() {
		if len(current) >= minDays {
			first := current[0].Date
			last := current[len(current)-1].Date
			detected := current[minDays-1].Date

			values := make([]DayValue, len(current))
			for i, it := range current {
				values[i] = DayValue{
					Date:     it.Date,
					Value:    it.Value,
					Expected: it.Expected,
				}
			}

			did := devID
			incidents = append(incidents, incident{
				Rule:       "under_expected",
				PlantID:    plantID,
				DeviceID:   &did,
				DevName:    devName,
				Severity:   "warning",
				Title:      fmt.Sprintf("Geração abaixo do esperado em %s", devName),
				FirstDate:  first,
				LastDate:   last,
				DetectedOn: detected,
				NumDays:    len(current),
				Values:     values,
				Params:     effectiveParams,
			})
		}
		current = nil
	}

	for _, it := range items {
		if it.Reading.Quality != domain.QualityOK || it.Reading.Value == nil {
			flush()
			continue
		}
		expected, hasPred := preds[it.Date]
		if !hasPred || expected < minExpectedKWh {
			flush()
			continue
		}

		if *it.Reading.Value < ratio*expected {
			expCopy := expected
			data := dayData{
				Date:     it.Date,
				Value:    it.Reading.Value,
				Expected: &expCopy,
			}
			if len(current) == 0 {
				current = append(current, data)
			} else {
				prev := current[len(current)-1]
				if daysBetween(prev.Date, it.Date) == 1 {
					current = append(current, data)
				} else {
					flush()
					current = append(current, data)
				}
			}
		} else {
			flush()
		}
	}
	flush()
	return incidents
}

func evaluateGradualLoss(plantID int64, dev domain.Device, items []readingItem, preds map[string]float64, cfgParams map[string]any) []incident {
	window := getParamInt(cfgParams, "window", 14)
	baseline := getParamInt(cfgParams, "baseline", 60)
	minBaselineDays := getParamInt(cfgParams, "min_baseline_days", 30)
	drop := getParamFloat(cfgParams, "drop", 0.10)

	effectiveParams := map[string]any{
		"window":            window,
		"baseline":          baseline,
		"min_baseline_days": minBaselineDays,
		"drop":              drop,
	}

	type okDay struct {
		Date     string
		Value    float64
		Expected float64
		Ratio    float64
	}

	var okDays []okDay
	for _, it := range items {
		if it.Reading.Quality == domain.QualityOK && it.Reading.Value != nil {
			if expected, ok := preds[it.Date]; ok && expected > 0 {
				okDays = append(okDays, okDay{
					Date:     it.Date,
					Value:    *it.Reading.Value,
					Expected: expected,
					Ratio:    *it.Reading.Value / expected,
				})
			}
		}
	}

	var incidents []incident
	var current []okDay

	devID := dev.ID
	devName := dev.Name

	flush := func() {
		if len(current) >= 1 {
			first := current[0].Date
			last := current[len(current)-1].Date
			detected := current[0].Date

			values := make([]DayValue, len(current))
			for i, it := range current {
				v := it.Value
				exp := it.Expected
				values[i] = DayValue{
					Date:     it.Date,
					Value:    &v,
					Expected: &exp,
				}
			}

			did := devID
			incidents = append(incidents, incident{
				Rule:       "gradual_loss",
				PlantID:    plantID,
				DeviceID:   &did,
				DevName:    devName,
				Severity:   "warning",
				Title:      fmt.Sprintf("Perda gradual de geração em %s", devName),
				FirstDate:  first,
				LastDate:   last,
				DetectedOn: detected,
				NumDays:    len(current),
				Values:     values,
				Params:     effectiveParams,
			})
		}
		current = nil
	}

	for i := window - 1; i < len(okDays); i++ {
		// Window: okDays[i-window+1 : i+1]
		var winRatios []float64
		for k := i - window + 1; k <= i; k++ {
			winRatios = append(winRatios, okDays[k].Ratio)
		}

		// Baseline: immediately preceding the window, up to `baseline` days
		baseEnd := i - window + 1
		baseStart := baseEnd - baseline
		if baseStart < 0 {
			baseStart = 0
		}
		if (baseEnd - baseStart) < minBaselineDays {
			flush()
			continue
		}

		var baseRatios []float64
		for k := baseStart; k < baseEnd; k++ {
			baseRatios = append(baseRatios, okDays[k].Ratio)
		}

		winMed := median(winRatios)
		baseMed := median(baseRatios)

		if (baseMed - winMed) > drop {
			data := okDays[i]
			if len(current) == 0 {
				current = append(current, data)
			} else {
				prev := current[len(current)-1]
				if daysBetween(prev.Date, data.Date) == 1 {
					current = append(current, data)
				} else {
					flush()
					current = append(current, data)
				}
			}
		} else {
			flush()
		}
	}
	flush()
	return incidents
}

func median(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	cp := make([]float64, n)
	copy(cp, vals)
	sort.Float64s(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2.0
}

func daysBetween(d1Str, d2Str string) int {
	t1, err1 := time.Parse(time.DateOnly, d1Str)
	t2, err2 := time.Parse(time.DateOnly, d2Str)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(t2.Sub(t1).Hours() / 24)
}

func getParamFloat(params map[string]any, key string, defaultVal float64) float64 {
	if params == nil {
		return defaultVal
	}
	if v, ok := params[key]; ok {
		switch val := v.(type) {
		case float64:
			return val
		case int:
			return float64(val)
		case int64:
			return float64(val)
		}
	}
	return defaultVal
}

func getParamInt(params map[string]any, key string, defaultVal int) int {
	if params == nil {
		return defaultVal
	}
	if v, ok := params[key]; ok {
		switch val := v.(type) {
		case float64:
			return int(val)
		case int:
			return val
		case int64:
			return int(val)
		}
	}
	return defaultVal
}
