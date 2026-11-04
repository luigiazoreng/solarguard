// Package quality classifies energy readings for data quality and defects.
package quality

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// Reason constants.
const (
	ReasonValueMissing            = "value_missing"
	ReasonNegativeValue           = "negative_value"
	ReasonImplausibleHigh         = "implausible_high"
	ReasonStuckValue              = "stuck_value"
	ReasonZeroRun                 = "zero_run"
	ReasonZeroWithPeersGenerating = "zero_with_peers_generating"
	ReasonZeroNoPeerEvidence      = "zero_no_peer_evidence"
	ReasonOK                      = "ok"
)

// Summary holds the aggregated quality classification counts.
type Summary struct {
	PlantID         int64
	TotalReadings   int
	CountsByQuality map[domain.Quality]int
	CountsByReason  map[string]int
	ByDevice        map[string]map[string]int // Device name -> Reason -> Count
}

type classifiedReading struct {
	reading domain.Reading
	devName string
	dateStr string // local day YYYY-MM-DD
	quality domain.Quality
	reason  string // stable reason string (ReasonOK or defect)
}

// Classify evaluates all readings of a plant in [from, to) and sets their quality and quality_reason.
// It is deterministic and idempotent, and never modifies readings.value.
func Classify(ctx context.Context, st *store.Store, plantID int64, from, to time.Time) (Summary, error) {
	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return Summary{}, fmt.Errorf("get plant %d: %w", plantID, err)
	}

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		return Summary{}, fmt.Errorf("load plant timezone %q: %w", plant.Timezone, err)
	}

	devices, err := st.ListDevices(ctx, plantID)
	if err != nil {
		return Summary{}, fmt.Errorf("list devices: %w", err)
	}
	devMap := make(map[int64]domain.Device)
	for _, d := range devices {
		devMap[d.ID] = d
	}

	rawReadings, err := st.ListReadingsByPlant(ctx, plantID, from, to)
	if err != nil {
		return Summary{}, fmt.Errorf("list plant readings: %w", err)
	}

	items := make([]classifiedReading, len(rawReadings))
	for i, r := range rawReadings {
		dev := devMap[r.DeviceID]
		dayStr := r.PeriodStart.In(loc).Format(time.DateOnly)
		items[i] = classifiedReading{
			reading: r,
			devName: dev.Name,
			dateStr: dayStr,
		}
	}

	// Index by device
	byDevice := make(map[int64][]int)
	for i, it := range items {
		byDevice[it.reading.DeviceID] = append(byDevice[it.reading.DeviceID], i)
	}

	// Sort each device's readings chronologically by local day
	for devID, indices := range byDevice {
		sort.Slice(indices, func(i, j int) bool {
			return items[indices[i]].dateStr < items[indices[j]].dateStr
		})
		byDevice[devID] = indices
	}

	// Evaluate Rules 1, 2, 3 per device
	for devID, indices := range byDevice {
		dev := devMap[devID]
		maxDaily, hasMax := calculatePlausibleMax(dev, items, indices)

		for _, idx := range indices {
			it := &items[idx]

			// Rule 1: value_missing
			if it.reading.Value == nil {
				it.quality = domain.QualityInvalid
				it.reason = ReasonValueMissing
				continue
			}

			// Rule 2: negative_value
			if *it.reading.Value < 0 {
				it.quality = domain.QualityInvalid
				it.reason = ReasonNegativeValue
				continue
			}

			// Rule 3: implausible_high
			if hasMax && *it.reading.Value > maxDaily {
				it.quality = domain.QualityInvalid
				it.reason = ReasonImplausibleHigh
				continue
			}
		}
	}

	// Rule 4: stuck_value per device
	// Run of 3 or more consecutive days where the device has exactly the same non-zero raw value.
	// Every day in the run, including the first, is invalid.
	for _, indices := range byDevice {
		runStart := 0
		for i := 0; i < len(indices); i++ {
			if i == 0 {
				runStart = 0
				continue
			}
			currIdx := indices[i]
			prevIdx := indices[i-1]
			currR := &items[currIdx]
			prevR := &items[prevIdx]

			tPrev, _ := time.Parse(time.DateOnly, prevR.dateStr)
			tCurr, _ := time.Parse(time.DateOnly, currR.dateStr)
			isConsecutive := tPrev.AddDate(0, 0, 1).Equal(tCurr)
			isNonZero := currR.reading.Value != nil && *currR.reading.Value != 0 && currR.reading.RawValue != "" && currR.reading.RawValue != "0"
			isSameRaw := currR.reading.RawValue == prevR.reading.RawValue

			if isConsecutive && isNonZero && isSameRaw {
				// continue current run
			} else {
				// end run
				if i-runStart >= 3 {
					for k := runStart; k < i; k++ {
						rIdx := indices[k]
						// first match wins: apply rule 4 only if rules 1..3 didn't already match
						if items[rIdx].reason == "" {
							items[rIdx].quality = domain.QualityInvalid
							items[rIdx].reason = ReasonStuckValue
						}
					}
				}
				runStart = i
			}
		}
		// check final run
		if len(indices)-runStart >= 3 {
			for k := runStart; k < len(indices); k++ {
				rIdx := indices[k]
				if items[rIdx].reason == "" {
					items[rIdx].quality = domain.QualityInvalid
					items[rIdx].reason = ReasonStuckValue
				}
			}
		}
	}

	// zero_run: the device has 0 on 3 or more consecutive days.
	// Every day in the run is invalid. Runs are computed the same way as stuck_value,
	// and a date gap breaks the run.
	for _, indices := range byDevice {
		runStart := 0
		for i := 0; i < len(indices); i++ {
			if i == 0 {
				runStart = 0
				continue
			}
			currIdx := indices[i]
			prevIdx := indices[i-1]
			currR := &items[currIdx]
			prevR := &items[prevIdx]

			tPrev, _ := time.Parse(time.DateOnly, prevR.dateStr)
			tCurr, _ := time.Parse(time.DateOnly, currR.dateStr)
			isConsecutive := tPrev.AddDate(0, 0, 1).Equal(tCurr)
			isZero := currR.reading.Value != nil && *currR.reading.Value == 0 && prevR.reading.Value != nil && *prevR.reading.Value == 0

			if isConsecutive && isZero {
				// continue current zero run
			} else {
				// end run
				if i-runStart >= 3 {
					startR := &items[indices[runStart]]
					if startR.reading.Value != nil && *startR.reading.Value == 0 {
						for k := runStart; k < i; k++ {
							rIdx := indices[k]
							if items[rIdx].reason == "" {
								items[rIdx].quality = domain.QualityInvalid
								items[rIdx].reason = ReasonZeroRun
							}
						}
					}
				}
				runStart = i
			}
		}
		// check final run
		if len(indices)-runStart >= 3 {
			startR := &items[indices[runStart]]
			if startR.reading.Value != nil && *startR.reading.Value == 0 {
				for k := runStart; k < len(indices); k++ {
					rIdx := indices[k]
					if items[rIdx].reason == "" {
						items[rIdx].quality = domain.QualityInvalid
						items[rIdx].reason = ReasonZeroRun
					}
				}
			}
		}
	}

	// Group by local day to evaluate peer rules
	byDate := make(map[string][]int)
	for i, it := range items {
		byDate[it.dateStr] = append(byDate[it.dateStr], i)
	}

	isDefective := func(reason string) bool {
		switch reason {
		case ReasonValueMissing, ReasonNegativeValue, ReasonImplausibleHigh, ReasonStuckValue, ReasonZeroRun:
			return true
		default:
			return false
		}
	}

	for _, indices := range byDate {
		for _, idx := range indices {
			it := &items[idx]
			// First match wins: if already classified by rules 1..4 (or zero_run), leave untouched
			if isDefective(it.reason) {
				continue
			}

			val := *it.reading.Value
			if val == 0 {
				// zero_with_peers_generating: value 0 and at least 1 peer has an ok-so-far value above 1 kWh. Invalid.
				// zero_no_peer_evidence: every other zero, including all peers at zero and no peers at all. Suspect.
				// A peer with a defect from rules 1 to 4 (including zero_run) is not evidence.
				// Peers exclude devices with Kind "plant", because that is a plant aggregate and not a peer.
				var okPeersAbove1Count int

				for _, peerIdx := range indices {
					if peerIdx == idx {
						continue
					}
					peerIt := &items[peerIdx]
					peerDev := devMap[peerIt.reading.DeviceID]
					if peerDev.Kind == "plant" {
						continue
					}
					if isDefective(peerIt.reason) || peerIt.reading.Value == nil {
						continue
					}

					if *peerIt.reading.Value > 1.0 {
						okPeersAbove1Count++
					}
				}

				if okPeersAbove1Count >= 1 {
					it.quality = domain.QualityInvalid
					it.reason = ReasonZeroWithPeersGenerating
				} else {
					it.quality = domain.QualitySuspect
					it.reason = ReasonZeroNoPeerEvidence
				}
			} else {
				// Non-zero value and did not match rules 1..4 -> Rule: otherwise ok
				it.quality = domain.QualityOK
				it.reason = ReasonOK
			}
		}
	}

	// Prepare updates and summary
	summary := Summary{
		PlantID:         plantID,
		TotalReadings:   len(items),
		CountsByQuality: make(map[domain.Quality]int),
		CountsByReason:  make(map[string]int),
		ByDevice:        make(map[string]map[string]int),
	}

	updates := make([]store.QualityUpdate, len(items))
	for i, it := range items {
		summary.CountsByQuality[it.quality]++
		summary.CountsByReason[it.reason]++

		if summary.ByDevice[it.devName] == nil {
			summary.ByDevice[it.devName] = make(map[string]int)
		}
		summary.ByDevice[it.devName][it.reason]++

		dbReason := it.reason
		if it.quality == domain.QualityOK {
			dbReason = "" // no defect reason for OK
		}
		updates[i] = store.QualityUpdate{
			ReadingID:     it.reading.ID,
			Quality:       it.quality,
			QualityReason: dbReason,
		}
	}

	if err := st.UpdateReadingQualities(ctx, updates); err != nil {
		return Summary{}, fmt.Errorf("update reading qualities: %w", err)
	}

	return summary, nil
}

// calculatePlausibleMax determines the device's plausible daily maximum.
// In order of preference:
//  1. PanelAreaM2 * Efficiency * 10 kWh/m2/day * 1.2 when both are known;
//  2. CapacityKW * 14 hours * 1.2 when capacity is known;
//  3. 10 times the 95th percentile of all that device's non-negative values in the range
//     (and skip this rule when the device has fewer than 14 values).
func calculatePlausibleMax(dev domain.Device, items []classifiedReading, devIndices []int) (float64, bool) {
	if dev.PanelAreaM2 != nil && dev.Efficiency != nil {
		return (*dev.PanelAreaM2) * (*dev.Efficiency) * 10.0 * 1.2, true
	}
	if dev.CapacityKW != nil {
		return (*dev.CapacityKW) * 14.0 * 1.2, true
	}

	var nonNegative []float64
	for _, idx := range devIndices {
		v := items[idx].reading.Value
		if v != nil && *v >= 0 {
			nonNegative = append(nonNegative, *v)
		}
	}
	if len(nonNegative) < 14 {
		return 0, false
	}

	sort.Float64s(nonNegative)
	p95 := percentile(nonNegative, 0.95)
	return 10.0 * p95, true
}

// percentile computes the p-th percentile (0 <= p <= 1) with linear interpolation.
func percentile(vals []float64, p float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return vals[0]
	}
	idx := p * float64(n-1)
	lower := int(idx)
	upper := lower + 1
	if upper >= n {
		return vals[n-1]
	}
	frac := idx - float64(lower)
	return vals[lower] + frac*(vals[upper]-vals[lower])
}
