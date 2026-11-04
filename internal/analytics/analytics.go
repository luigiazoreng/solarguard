// Package analytics computes deterministic aggregates and comparisons for SolarGuard.
// It is the single source of truth for generation, expected vs actual, lost energy,
// data quality, and device fleet comparisons across plants and devices.
package analytics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// Granularity controls time binning for generation aggregates.
type Granularity string

const (
	GranularityDay   Granularity = "day"
	GranularityMonth Granularity = "month"
)

// GenerationPoint holds generation stats for a single period (day or month).
type GenerationPoint struct {
	Period      string  `json:"period"` // YYYY-MM-DD or YYYY-MM
	MeasuredKWh float64 `json:"measured_kwh"`
	DaysOK      int     `json:"days_ok"`
	DaysSuspect int     `json:"days_suspect"`
	DaysInvalid int     `json:"days_invalid"`
	DaysMissing int     `json:"days_missing"`
}

// GenerationSummary holds measured generation and data availability counts.
type GenerationSummary struct {
	PlantID          int64             `json:"plant_id"`
	DeviceID         *int64            `json:"device_id,omitempty"`
	From             string            `json:"from"`
	To               string            `json:"to"`
	Granularity      string            `json:"granularity"`
	TotalMeasuredKWh float64           `json:"total_measured_kwh"`
	DaysOK           int               `json:"days_ok"`
	DaysSuspect      int               `json:"days_suspect"`
	DaysInvalid      int               `json:"days_invalid"`
	DaysMissing      int               `json:"days_missing"`
	Points           []GenerationPoint `json:"points"`
}

// ExpectedVsActualDay represents one day's measured vs expected comparison.
type ExpectedVsActualDay struct {
	Date          string         `json:"date"`
	MeasuredKWh   *float64       `json:"measured_kwh"` // nil when not ok
	ExpectedKWh   float64        `json:"expected_kwh"`
	Quality       domain.Quality `json:"quality"`
	QualityReason string         `json:"quality_reason,omitempty"`
}

// ExpectedVsActualSummary holds daily comparisons and totals on valid days.
type ExpectedVsActualSummary struct {
	PlantID             int64                 `json:"plant_id"`
	DeviceID            *int64                `json:"device_id,omitempty"`
	From                string                `json:"from"`
	To                  string                `json:"to"`
	TotalMeasuredKWh    float64               `json:"total_measured_kwh"`
	TotalExpectedKWh    float64               `json:"total_expected_kwh"`
	PairedMeasuredKWh   float64               `json:"paired_measured_kwh"`
	PairedExpectedKWh   float64               `json:"paired_expected_kwh"`
	PairedRatio         *float64              `json:"paired_ratio"`
	PairedDeviceDays    int                   `json:"paired_device_days"`
	UnpairedMeasuredKWh float64               `json:"unpaired_measured_kwh"`
	UnpairedDeviceDays  int                   `json:"unpaired_device_days"`
	Ratio               *float64              `json:"ratio"`
	Days                []ExpectedVsActualDay `json:"days"`
}

// LostEnergySummary estimates generation lost due to missing or faulty readings.
type LostEnergySummary struct {
	PlantID                   int64   `json:"plant_id"`
	From                      string  `json:"from"`
	To                        string  `json:"to"`
	LostEnergyKWh             float64 `json:"lost_energy_kwh"`
	DaysCovered               int     `json:"days_covered"`
	DevicesCovered            int     `json:"devices_covered"`
	DeviceDaysAffected        int     `json:"device_days_affected"`
	DeviceDaysWithExpected    int     `json:"device_days_with_expected"`
	DeviceDaysWithoutExpected int     `json:"device_days_without_expected"`
	TotalDeviceDays           int     `json:"total_device_days"`
	IsEstimate                bool    `json:"is_estimate"`
	Label                     string  `json:"label"`
}

// DeviceDataQuality summarizes quality classification for a single device.
type DeviceDataQuality struct {
	DeviceID     int64          `json:"device_id"`
	DeviceName   string         `json:"device_name"`
	TotalDays    int            `json:"total_days"`
	OKDays       int            `json:"ok_days"`
	PercentOK    float64        `json:"percent_ok"`
	SuspectDays  int            `json:"suspect_days"`
	InvalidDays  int            `json:"invalid_days"`
	MissingDays  int            `json:"missing_days"`
	ReasonsCount map[string]int `json:"reasons_count"`
}

// DataQualitySummary holds data quality breakdowns for all plant devices.
type DataQualitySummary struct {
	PlantID int64               `json:"plant_id"`
	From    string              `json:"from"`
	To      string              `json:"to"`
	Devices []DeviceDataQuality `json:"devices"`
}

// DeviceComparison holds fair comparison metrics for a single device.
type DeviceComparison struct {
	DeviceID          int64    `json:"device_id"`
	DeviceName        string   `json:"device_name"`
	MeasuredKWh       float64  `json:"measured_kwh"`
	ExpectedKWh       float64  `json:"expected_kwh"`
	PeerMedianKWh     *float64 `json:"peer_median_kwh"`
	RatioToPeerMedian *float64 `json:"ratio_to_peer_median"`
}

// CompareDevicesSummary holds fair comparisons among fleet devices.
type CompareDevicesSummary struct {
	PlantID      int64              `json:"plant_id"`
	From         string             `json:"from"`
	To           string             `json:"to"`
	CommonOKDays int                `json:"common_ok_days"`
	Devices      []DeviceComparison `json:"devices"`
}

// validateDateRange checks format, chronological order, and the 400-day limit.
func validateDateRange(from, to string) (tFrom, tTo time.Time, dates []string, err error) {
	tFrom, err = time.Parse(time.DateOnly, from)
	if err != nil {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("data inicial inválida %q: use o formato YYYY-MM-DD", from)
	}
	tTo, err = time.Parse(time.DateOnly, to)
	if err != nil {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("data final inválida %q: use o formato YYYY-MM-DD", to)
	}
	if tFrom.After(tTo) {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("intervalo inválido: data inicial (%s) posterior à data final (%s)", from, to)
	}

	days := int(tTo.Sub(tFrom).Hours()/24) + 1
	if days > 400 {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("intervalo de %d dias excede o limite máximo de 400 dias", days)
	}

	dates = make([]string, 0, days)
	curr := tFrom
	for !curr.After(tTo) {
		dates = append(dates, curr.Format(time.DateOnly))
		curr = curr.AddDate(0, 0, 1)
	}
	return tFrom, tTo, dates, nil
}

// getTargetDevices returns the devices to analyze. If deviceID is given, checks it exists.
// Otherwise returns devices excluding kind "plant" (or all devices if only "plant" exists).
func getTargetDevices(ctx context.Context, st *store.Store, plantID int64, deviceID *int64) ([]domain.Device, error) {
	allDevs, err := st.ListDevices(ctx, plantID)
	if err != nil {
		return nil, fmt.Errorf("listar dispositivos da planta %d: %w", plantID, err)
	}
	if deviceID != nil {
		for _, d := range allDevs {
			if d.ID == *deviceID {
				return []domain.Device{d}, nil
			}
		}
		return nil, fmt.Errorf("dispositivo %d não encontrado na planta %d", *deviceID, plantID)
	}

	var generating []domain.Device
	for _, d := range allDevs {
		if d.Kind != "plant" {
			generating = append(generating, d)
		}
	}
	if len(generating) > 0 {
		return generating, nil
	}
	return allDevs, nil
}

// loadPlantReadingsMap returns readings indexed by [deviceID][dateStr].
func loadPlantReadingsMap(ctx context.Context, st *store.Store, plant domain.Plant, fromStr, toStr string) (map[int64]map[string]domain.Reading, error) {
	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		return nil, fmt.Errorf("carregar fuso horário %q: %w", plant.Timezone, err)
	}
	utcStart, _, err := domain.LocalDay(fromStr, plant.Timezone)
	if err != nil {
		return nil, err
	}
	_, utcEnd, err := domain.LocalDay(toStr, plant.Timezone)
	if err != nil {
		return nil, err
	}

	readings, err := st.ListReadingsByPlant(ctx, plant.ID, utcStart, utcEnd)
	if err != nil {
		return nil, fmt.Errorf("listar leituras da planta %d: %w", plant.ID, err)
	}

	byDevDate := make(map[int64]map[string]domain.Reading)
	for _, r := range readings {
		dStr := r.PeriodStart.In(loc).Format(time.DateOnly)
		if byDevDate[r.DeviceID] == nil {
			byDevDate[r.DeviceID] = make(map[string]domain.Reading)
		}
		byDevDate[r.DeviceID][dStr] = r
	}
	return byDevDate, nil
}

// Generation computes measured generation from readings with quality ok,
// plus counts of days ok, suspect, invalid and missing.
// Not-ok values never enter totals.
func Generation(ctx context.Context, st *store.Store, plantID int64, deviceID *int64, from, to string, granularity string) (GenerationSummary, error) {
	if granularity == "" {
		granularity = "day"
	}
	if granularity != "day" && granularity != "month" {
		return GenerationSummary{}, fmt.Errorf("granularidade inválida %q: deve ser 'day' ou 'month'", granularity)
	}

	_, _, dates, err := validateDateRange(from, to)
	if err != nil {
		return GenerationSummary{}, err
	}

	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return GenerationSummary{}, fmt.Errorf("planta %d: %w", plantID, err)
	}

	devices, err := getTargetDevices(ctx, st, plantID, deviceID)
	if err != nil {
		return GenerationSummary{}, err
	}

	byDevDate, err := loadPlantReadingsMap(ctx, st, plant, from, to)
	if err != nil {
		return GenerationSummary{}, err
	}

	// For each calendar date, calculate total measured kWh (ok readings only)
	// and determine day classification.
	type dailyStat struct {
		date        string
		month       string
		measuredKWh float64
		ok          bool
		suspect     bool
		invalid     bool
		missing     bool
	}

	dailyStats := make([]dailyStat, len(dates))
	for i, dStr := range dates {
		var dayMeasured float64
		var numOK, numSuspect, numInvalid, numMissing int

		for _, d := range devices {
			r, exists := byDevDate[d.ID][dStr]
			if !exists || r.Value == nil {
				numMissing++
			} else {
				switch r.Quality {
				case domain.QualityOK:
					numOK++
					dayMeasured += *r.Value
				case domain.QualitySuspect:
					numSuspect++
				case domain.QualityInvalid:
					numInvalid++
				default:
					numInvalid++
				}
			}
		}

		stat := dailyStat{
			date:        dStr,
			month:       dStr[:7],
			measuredKWh: dayMeasured,
		}

		if len(devices) == 0 {
			stat.missing = true
		} else if numOK == len(devices) {
			stat.ok = true
		} else if numInvalid > 0 {
			stat.invalid = true
		} else if numSuspect > 0 {
			stat.suspect = true
		} else {
			stat.missing = true
		}

		dailyStats[i] = stat
	}

	summary := GenerationSummary{
		PlantID:     plantID,
		DeviceID:    deviceID,
		From:        from,
		To:          to,
		Granularity: granularity,
	}

	for _, ds := range dailyStats {
		summary.TotalMeasuredKWh += ds.measuredKWh
		if ds.ok {
			summary.DaysOK++
		} else if ds.invalid {
			summary.DaysInvalid++
		} else if ds.suspect {
			summary.DaysSuspect++
		} else if ds.missing {
			summary.DaysMissing++
		}
	}

	if granularity == "day" {
		summary.Points = make([]GenerationPoint, len(dailyStats))
		for i, ds := range dailyStats {
			p := GenerationPoint{
				Period:      ds.date,
				MeasuredKWh: ds.measuredKWh,
			}
			if ds.ok {
				p.DaysOK = 1
			} else if ds.invalid {
				p.DaysInvalid = 1
			} else if ds.suspect {
				p.DaysSuspect = 1
			} else if ds.missing {
				p.DaysMissing = 1
			}
			summary.Points[i] = p
		}
	} else {
		// Group by month
		type monthAccum struct {
			measured float64
			ok       int
			suspect  int
			invalid  int
			missing  int
		}
		monthsMap := make(map[string]*monthAccum)
		var monthOrder []string

		for _, ds := range dailyStats {
			acc, exists := monthsMap[ds.month]
			if !exists {
				acc = &monthAccum{}
				monthsMap[ds.month] = acc
				monthOrder = append(monthOrder, ds.month)
			}
			acc.measured += ds.measuredKWh
			if ds.ok {
				acc.ok++
			} else if ds.invalid {
				acc.invalid++
			} else if ds.suspect {
				acc.suspect++
			} else if ds.missing {
				acc.missing++
			}
		}

		summary.Points = make([]GenerationPoint, len(monthOrder))
		for i, m := range monthOrder {
			acc := monthsMap[m]
			summary.Points[i] = GenerationPoint{
				Period:      m,
				MeasuredKWh: acc.measured,
				DaysOK:      acc.ok,
				DaysSuspect: acc.suspect,
				DaysInvalid: acc.invalid,
				DaysMissing: acc.missing,
			}
		}
	}

	return summary, nil
}

// ExpectedVsActual computes per-day comparisons and totals on valid days.
func ExpectedVsActual(ctx context.Context, st *store.Store, plantID int64, deviceID *int64, from, to string) (ExpectedVsActualSummary, error) {
	_, _, dates, err := validateDateRange(from, to)
	if err != nil {
		return ExpectedVsActualSummary{}, err
	}

	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return ExpectedVsActualSummary{}, fmt.Errorf("planta %d: %w", plantID, err)
	}

	devices, err := getTargetDevices(ctx, st, plantID, deviceID)
	if err != nil {
		return ExpectedVsActualSummary{}, err
	}

	byDevDate, err := loadPlantReadingsMap(ctx, st, plant, from, to)
	if err != nil {
		return ExpectedVsActualSummary{}, err
	}

	predsMap, err := st.GetActivePredictionsMap(ctx, plantID, from, to)
	if err != nil {
		return ExpectedVsActualSummary{}, fmt.Errorf("obter previsões ativas da planta %d: %w", plantID, err)
	}

	days := make([]ExpectedVsActualDay, len(dates))
	var totalMeasured float64
	var pairedMeasured, pairedExpected float64
	var pairedDeviceDays int
	var unpairedMeasured float64
	var unpairedDeviceDays int

	for i, dStr := range dates {
		var dayExpected float64

		if deviceID != nil {
			dev := devices[0]
			r, exists := byDevDate[dev.ID][dStr]
			isOK := exists && r.Value != nil && r.Quality == domain.QualityOK

			if isOK {
				val := *r.Value
				totalMeasured += val

				exp, hasExp := predsMap[dev.ID][dStr]
				if hasExp {
					dayExpected = exp
					pairedMeasured += val
					pairedExpected += exp
					pairedDeviceDays++
				} else {
					unpairedMeasured += val
					unpairedDeviceDays++
				}
			}

			day := ExpectedVsActualDay{
				Date:        dStr,
				ExpectedKWh: dayExpected,
			}

			if !exists || r.Value == nil {
				day.Quality = "missing"
				day.QualityReason = "dados ausentes"
			} else {
				day.Quality = r.Quality
				day.QualityReason = r.QualityReason
				if isOK {
					val := *r.Value
					day.MeasuredKWh = &val
				}
			}
			days[i] = day
		} else {
			// Plant-level comparison
			var measuredSum float64
			var numOK, numSuspect, numInvalid, numMissing int
			var firstReason string

			for _, d := range devices {
				r, exists := byDevDate[d.ID][dStr]
				isOK := exists && r.Value != nil && r.Quality == domain.QualityOK
				if isOK {
					numOK++
					val := *r.Value
					measuredSum += val
					totalMeasured += val

					exp, hasExp := predsMap[d.ID][dStr]
					if hasExp {
						dayExpected += exp
						pairedMeasured += val
						pairedExpected += exp
						pairedDeviceDays++
					} else {
						unpairedMeasured += val
						unpairedDeviceDays++
					}
				} else {
					if !exists || r.Value == nil {
						numMissing++
						if firstReason == "" {
							firstReason = "dados ausentes"
						}
					} else {
						switch r.Quality {
						case domain.QualitySuspect:
							numSuspect++
						case domain.QualityInvalid:
							numInvalid++
						default:
							numInvalid++
						}
						if firstReason == "" {
							firstReason = r.QualityReason
						}
					}
				}
			}

			day := ExpectedVsActualDay{
				Date:        dStr,
				ExpectedKWh: dayExpected,
			}

			if len(devices) > 0 && numOK == len(devices) {
				day.Quality = domain.QualityOK
				val := measuredSum
				day.MeasuredKWh = &val
			} else if numInvalid > 0 {
				day.Quality = domain.QualityInvalid
				day.QualityReason = firstReason
			} else if numSuspect > 0 {
				day.Quality = domain.QualitySuspect
				day.QualityReason = firstReason
			} else {
				day.Quality = "missing"
				day.QualityReason = firstReason
			}

			days[i] = day
		}
	}

	summary := ExpectedVsActualSummary{
		PlantID:             plantID,
		DeviceID:            deviceID,
		From:                from,
		To:                  to,
		TotalMeasuredKWh:    totalMeasured,
		TotalExpectedKWh:    pairedExpected,
		PairedMeasuredKWh:   pairedMeasured,
		PairedExpectedKWh:   pairedExpected,
		PairedDeviceDays:    pairedDeviceDays,
		UnpairedMeasuredKWh: unpairedMeasured,
		UnpairedDeviceDays:  unpairedDeviceDays,
		Days:                days,
	}

	if pairedDeviceDays > 0 && pairedExpected > 0 {
		r := pairedMeasured / pairedExpected
		summary.PairedRatio = &r
		summary.Ratio = &r
	}

	return summary, nil
}

// LostEnergy calculates estimated energy lost over device-days whose reading is not ok or is missing.
func LostEnergy(ctx context.Context, st *store.Store, plantID int64, from, to string) (LostEnergySummary, error) {
	_, _, dates, err := validateDateRange(from, to)
	if err != nil {
		return LostEnergySummary{}, err
	}

	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return LostEnergySummary{}, fmt.Errorf("planta %d: %w", plantID, err)
	}

	devices, err := getTargetDevices(ctx, st, plantID, nil)
	if err != nil {
		return LostEnergySummary{}, err
	}

	byDevDate, err := loadPlantReadingsMap(ctx, st, plant, from, to)
	if err != nil {
		return LostEnergySummary{}, err
	}

	predsMap, err := st.GetActivePredictionsMap(ctx, plantID, from, to)
	if err != nil {
		return LostEnergySummary{}, fmt.Errorf("obter previsões ativas da planta %d: %w", plantID, err)
	}

	var lostKWh float64
	var deviceDaysAffected int
	var deviceDaysWithExpected int
	var deviceDaysWithoutExpected int
	affectedDays := make(map[string]bool)

	for _, d := range devices {
		for _, dStr := range dates {
			r, exists := byDevDate[d.ID][dStr]
			isOK := exists && r.Value != nil && r.Quality == domain.QualityOK
			if !isOK {
				deviceDaysAffected++
				affectedDays[dStr] = true
				if exp, hasExp := predsMap[d.ID][dStr]; hasExp {
					lostKWh += exp
					deviceDaysWithExpected++
				} else {
					deviceDaysWithoutExpected++
				}
			}
		}
	}

	return LostEnergySummary{
		PlantID:                   plantID,
		From:                      from,
		To:                        to,
		LostEnergyKWh:             lostKWh,
		DaysCovered:               len(affectedDays),
		DevicesCovered:            len(devices),
		DeviceDaysAffected:        deviceDaysAffected,
		DeviceDaysWithExpected:    deviceDaysWithExpected,
		DeviceDaysWithoutExpected: deviceDaysWithoutExpected,
		TotalDeviceDays:           len(devices) * len(dates),
		IsEstimate:                true,
		Label:                     "Estimativa pelo modelo ativo: a energia pode ter sido gerada e não registrada",
	}, nil
}

// DataQuality returns percent ok per device and counts of reasons for non-ok data.
func DataQuality(ctx context.Context, st *store.Store, plantID int64, from, to string) (DataQualitySummary, error) {
	_, _, dates, err := validateDateRange(from, to)
	if err != nil {
		return DataQualitySummary{}, err
	}

	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return DataQualitySummary{}, fmt.Errorf("planta %d: %w", plantID, err)
	}

	devices, err := getTargetDevices(ctx, st, plantID, nil)
	if err != nil {
		return DataQualitySummary{}, err
	}

	byDevDate, err := loadPlantReadingsMap(ctx, st, plant, from, to)
	if err != nil {
		return DataQualitySummary{}, err
	}

	out := DataQualitySummary{
		PlantID: plantID,
		From:    from,
		To:      to,
		Devices: make([]DeviceDataQuality, len(devices)),
	}

	totalDays := len(dates)

	for i, d := range devices {
		reasons := make(map[string]int)
		var okDays, suspectDays, invalidDays, missingDays int

		for _, dStr := range dates {
			r, exists := byDevDate[d.ID][dStr]
			if !exists || r.Value == nil {
				missingDays++
				reasons["dados_ausentes"]++
			} else {
				switch r.Quality {
				case domain.QualityOK:
					okDays++
				case domain.QualitySuspect:
					suspectDays++
					if r.QualityReason != "" {
						reasons[r.QualityReason]++
					} else {
						reasons["suspect"]++
					}
				case domain.QualityInvalid:
					invalidDays++
					if r.QualityReason != "" {
						reasons[r.QualityReason]++
					} else {
						reasons["invalid"]++
					}
				default:
					invalidDays++
					reasons["unchecked"]++
				}
			}
		}

		pctOK := 0.0
		if totalDays > 0 {
			pctOK = (float64(okDays) / float64(totalDays)) * 100.0
		}

		out.Devices[i] = DeviceDataQuality{
			DeviceID:     d.ID,
			DeviceName:   d.Name,
			TotalDays:    totalDays,
			OKDays:       okDays,
			PercentOK:    pctOK,
			SuspectDays:  suspectDays,
			InvalidDays:  invalidDays,
			MissingDays:  missingDays,
			ReasonsCount: reasons,
		}
	}

	return out, nil
}

// CompareDevices performs fair comparisons across fleet devices on days where all compared devices are ok.
func CompareDevices(ctx context.Context, st *store.Store, plantID int64, from, to string) (CompareDevicesSummary, error) {
	_, _, dates, err := validateDateRange(from, to)
	if err != nil {
		return CompareDevicesSummary{}, err
	}

	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return CompareDevicesSummary{}, fmt.Errorf("planta %d: %w", plantID, err)
	}

	// Exclude kind "plant" from fleet comparisons
	allDevs, err := st.ListDevices(ctx, plantID)
	if err != nil {
		return CompareDevicesSummary{}, fmt.Errorf("listar dispositivos: %w", err)
	}
	var devices []domain.Device
	for _, d := range allDevs {
		if d.Kind != "plant" {
			devices = append(devices, d)
		}
	}

	byDevDate, err := loadPlantReadingsMap(ctx, st, plant, from, to)
	if err != nil {
		return CompareDevicesSummary{}, err
	}

	predsMap, err := st.GetActivePredictionsMap(ctx, plantID, from, to)
	if err != nil {
		return CompareDevicesSummary{}, fmt.Errorf("obter previsões ativas: %w", err)
	}

	// 1. Find fair days where EVERY compared device is ok and has an expected value
	var fairDays []string
	if len(devices) > 0 {
		for _, dStr := range dates {
			allOKAndHasExp := true
			for _, d := range devices {
				r, exists := byDevDate[d.ID][dStr]
				if !exists || r.Value == nil || r.Quality != domain.QualityOK {
					allOKAndHasExp = false
					break
				}
				if _, hasExp := predsMap[d.ID][dStr]; !hasExp {
					allOKAndHasExp = false
					break
				}
			}
			if allOKAndHasExp {
				fairDays = append(fairDays, dStr)
			}
		}
	}

	// 2. Sum measured and expected on fair days
	type devFairTotals struct {
		measured float64
		expected float64
	}
	totals := make(map[int64]devFairTotals, len(devices))
	for _, d := range devices {
		var mSum, eSum float64
		for _, dStr := range fairDays {
			r := byDevDate[d.ID][dStr]
			mSum += *r.Value
			eSum += predsMap[d.ID][dStr]
		}
		totals[d.ID] = devFairTotals{measured: mSum, expected: eSum}
	}

	// 3. Compute ratio to peer median
	res := CompareDevicesSummary{
		PlantID:      plantID,
		From:         from,
		To:           to,
		CommonOKDays: len(fairDays),
		Devices:      make([]DeviceComparison, len(devices)),
	}

	for i, d := range devices {
		dTot := totals[d.ID]
		comp := DeviceComparison{
			DeviceID:    d.ID,
			DeviceName:  d.Name,
			MeasuredKWh: dTot.measured,
			ExpectedKWh: dTot.expected,
		}

		// Collect peer measured values
		var peerVals []float64
		for _, other := range devices {
			if other.ID != d.ID {
				peerVals = append(peerVals, totals[other.ID].measured)
			}
		}

		if len(peerVals) > 0 {
			med := calcMedian(peerVals)
			comp.PeerMedianKWh = &med
			if med > 0 {
				ratio := dTot.measured / med
				comp.RatioToPeerMedian = &ratio
			}
		}

		res.Devices[i] = comp
	}

	return res, nil
}

func calcMedian(vals []float64) float64 {
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

// RoundFloat rounds a float to the given number of decimal places.
func RoundFloat(v float64, decimals int) float64 {
	p := math.Pow10(decimals)
	return math.Round(v*p) / p
}
