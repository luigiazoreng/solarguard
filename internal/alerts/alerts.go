package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// Options controls evaluation range and replay settings.
type Options struct {
	AsOf string           // YYYY-MM-DD (default: newest reading date)
	Now  func() time.Time // default: time.Now
}

// Summary aggregates counts and results of an alerts evaluation run.
type Summary struct {
	PlantID        int64
	CountsByRule   map[string]int
	CountsByDevice map[string]map[string]int // devName -> rule -> count
	SkippedRules   []string
	New            int
	Updated        int
	Resolved       int
	Alerts         []store.Alert
}

// DayValue records evidence for one local day.
type DayValue struct {
	Date       string   `json:"date"`
	Value      *float64 `json:"value,omitempty"`
	Expected   *float64 `json:"expected,omitempty"`
	PeerMedian *float64 `json:"peer_median,omitempty"`
	RawValue   string   `json:"raw_value,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// Evidence captures deterministic proof for an alert incident.
type Evidence struct {
	Rule        string         `json:"rule"`
	Params      map[string]any `json:"params"`
	FirstDate   string         `json:"first_date"`
	LastDate    string         `json:"last_date"`
	NumDays     int            `json:"num_days"`
	DetectedOn  string         `json:"detected_on"`
	Values      []DayValue     `json:"values"`
	TotalValues int            `json:"total_values"`
}

// Run evaluates one plant over a local-date range.
func Run(ctx context.Context, st *store.Store, plantID int64, opts Options) (Summary, error) {
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
		return Summary{}, fmt.Errorf("list devices for plant %d: %w", plantID, err)
	}
	devMap := make(map[int64]domain.Device)
	for _, d := range devices {
		devMap[d.ID] = d
	}

	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	todayStr := nowFn().In(loc).Format(time.DateOnly)

	var utcTo time.Time
	if opts.AsOf != "" {
		_, tEnd, err := domain.LocalDay(opts.AsOf, plant.Timezone)
		if err == nil {
			utcTo = tEnd
		}
	}

	rawReadings, err := st.ListReadingsByPlant(ctx, plantID, time.Time{}, utcTo)
	if err != nil {
		return Summary{}, fmt.Errorf("list readings: %w", err)
	}

	// Filter and organize readings by local date
	byDevice := make(map[int64][]readingItem)
	byDate := make(map[string]map[int64]domain.Reading)
	seenDates := make(map[string]bool)

	var plantEarliestDate, plantNewestDate string

	for _, r := range rawReadings {
		dev, ok := devMap[r.DeviceID]
		if !ok {
			continue
		}
		dStr := r.PeriodStart.In(loc).Format(time.DateOnly)
		if opts.AsOf != "" && dStr > opts.AsOf {
			continue
		}

		if plantEarliestDate == "" || dStr < plantEarliestDate {
			plantEarliestDate = dStr
		}
		if plantNewestDate == "" || dStr > plantNewestDate {
			plantNewestDate = dStr
		}

		item := readingItem{
			Reading: r,
			DevName: dev.Name,
			Date:    dStr,
		}
		byDevice[r.DeviceID] = append(byDevice[r.DeviceID], item)

		if byDate[dStr] == nil {
			byDate[dStr] = make(map[int64]domain.Reading)
		}
		byDate[dStr][r.DeviceID] = r
		seenDates[dStr] = true
	}

	// Sort each device's readings chronologically
	for devID := range byDevice {
		sort.Slice(byDevice[devID], func(i, j int) bool {
			return byDevice[devID][i].Date < byDevice[devID][j].Date
		})
	}

	var allDates []string
	for d := range seenDates {
		allDates = append(allDates, d)
	}
	sort.Strings(allDates)

	// Determine asOfDate for evaluation and auto-resolve
	asOfDate := opts.AsOf
	if asOfDate == "" {
		if plantNewestDate != "" {
			asOfDate = plantNewestDate
		} else {
			asOfDate = todayStr
		}
	}

	// Load rule configurations
	ruleConfigs, err := st.GetRuleConfigs(ctx, plantID)
	if err != nil {
		return Summary{}, fmt.Errorf("get rule configs: %w", err)
	}

	getRuleParams := func(ruleName string) (bool, map[string]any) {
		cfg, ok := ruleConfigs[ruleName]
		if !ok {
			return true, nil
		}
		if !cfg.Enabled {
			return false, nil
		}
		if cfg.ParamsJSON != "" && cfg.ParamsJSON != "{}" {
			var p map[string]any
			if err := json.Unmarshal([]byte(cfg.ParamsJSON), &p); err == nil {
				return true, p
			}
		}
		return true, nil
	}

	// Load predictions for active model run
	predsList, err := st.ListActivePredictions(ctx, plantID)
	if err != nil {
		return Summary{}, fmt.Errorf("list active predictions: %w", err)
	}
	hasPredictions := len(predsList) > 0
	preds := make(map[int64]map[string]float64)
	for _, p := range predsList {
		if preds[p.DeviceID] == nil {
			preds[p.DeviceID] = make(map[string]float64)
		}
		preds[p.DeviceID][p.Date] = p.KWhExpected
	}

	summary := Summary{
		PlantID:        plantID,
		CountsByRule:   make(map[string]int),
		CountsByDevice: make(map[string]map[string]int),
	}

	evaluatedRules := make(map[string]bool)
	var allIncidents []incident

	// 1. stuck_value
	if enabled, params := getRuleParams("stuck_value"); enabled {
		evaluatedRules["stuck_value"] = true
		for _, d := range devices {
			if d.Kind != "plant" {
				allIncidents = append(allIncidents, evaluateStuckValue(plantID, d, byDevice[d.ID], params)...)
			}
		}
	}

	// 2. zero_generation
	if enabled, params := getRuleParams("zero_generation"); enabled {
		evaluatedRules["zero_generation"] = true
		for _, d := range devices {
			if d.Kind != "plant" {
				allIncidents = append(allIncidents, evaluateZeroGeneration(plantID, d, byDevice[d.ID], params)...)
			}
		}
	}

	// 3. implausible_reading
	if enabled, params := getRuleParams("implausible_reading"); enabled {
		evaluatedRules["implausible_reading"] = true
		for _, d := range devices {
			if d.Kind != "plant" {
				allIncidents = append(allIncidents, evaluateImplausibleReading(plantID, d, byDevice[d.ID], params)...)
			}
		}
	}

	// 4. plant_no_generation
	if enabled, params := getRuleParams("plant_no_generation"); enabled {
		evaluatedRules["plant_no_generation"] = true
		allIncidents = append(allIncidents, evaluatePlantNoGeneration(plantID, devices, byDate, allDates, params)...)
	}

	// 5. missing_data
	if enabled, params := getRuleParams("missing_data"); enabled {
		evaluatedRules["missing_data"] = true
		cutoff := plantNewestDate
		if cutoff >= todayStr {
			tToday, _ := time.Parse(time.DateOnly, todayStr)
			cutoff = tToday.AddDate(0, 0, -1).Format(time.DateOnly)
		}
		allIncidents = append(allIncidents, evaluateMissingData(plantID, devices, byDevice, plantEarliestDate, cutoff, todayStr, params)...)
	}

	// 6. peer_divergence
	if enabled, params := getRuleParams("peer_divergence"); enabled {
		evaluatedRules["peer_divergence"] = true
		allIncidents = append(allIncidents, evaluatePeerDivergence(plantID, devices, byDate, allDates, params)...)
	}

	// 7. under_expected
	if enabled, params := getRuleParams("under_expected"); enabled {
		if !hasPredictions {
			summary.SkippedRules = append(summary.SkippedRules, "under_expected")
		} else {
			evaluatedRules["under_expected"] = true
			for _, d := range devices {
				if d.Kind != "plant" {
					allIncidents = append(allIncidents, evaluateUnderExpected(plantID, d, byDevice[d.ID], preds[d.ID], params)...)
				}
			}
		}
	}

	// 8. gradual_loss
	if enabled, params := getRuleParams("gradual_loss"); enabled {
		if !hasPredictions {
			summary.SkippedRules = append(summary.SkippedRules, "gradual_loss")
		} else {
			evaluatedRules["gradual_loss"] = true
			drops := make(map[int64][]gradualDay)
			dropByDate := make(map[int64]map[string]float64)
			for _, d := range devices {
				if d.Kind == "plant" {
					continue
				}
				drops[d.ID] = gradualDrops(byDevice[d.ID], preds[d.ID], params)
				dropByDate[d.ID] = make(map[string]float64, len(drops[d.ID]))
				for _, day := range drops[d.ID] {
					dropByDate[d.ID][day.Date] = day.Drop
				}
			}
			for _, d := range devices {
				if d.Kind == "plant" {
					continue
				}
				var peers []map[string]float64
				for id, m := range dropByDate {
					if id != d.ID {
						peers = append(peers, m)
					}
				}
				allIncidents = append(allIncidents, evaluateGradualLoss(plantID, d, drops[d.ID], peers, params)...)
			}
		}
	}

	// 9. stale_data
	// Evaluated only when AsOf is empty, and skipped for sources of kind "file".
	src, err := st.GetSource(ctx, plant.SourceID)
	if err == nil && src.Kind == "file" {
		summary.SkippedRules = append(summary.SkippedRules, "stale_data")
	} else if opts.AsOf == "" {
		if enabled, params := getRuleParams("stale_data"); enabled {
			evaluatedRules["stale_data"] = true
			localYesterday := nowFn().In(loc).AddDate(0, 0, -1).Format(time.DateOnly)
			allIncidents = append(allIncidents, evaluateStaleData(plantID, plantNewestDate, todayStr, localYesterday, params)...)
		}
	}

	// Load existing alerts for reconciliation
	existingAlerts, err := st.ListAlertsByPlant(ctx, plantID)
	if err != nil {
		return Summary{}, fmt.Errorf("list existing alerts: %w", err)
	}
	existingMap := make(map[string]*store.Alert)
	for i := range existingAlerts {
		existingMap[existingAlerts[i].DedupeKey] = &existingAlerts[i]
	}

	processedKeys := make(map[string]bool)

	// Process each incident
	for _, inc := range allIncidents {
		dedupeKey := inc.DedupeKey()
		processedKeys[dedupeKey] = true
		summary.CountsByRule[inc.Rule]++

		devLabel := inc.DevName
		if devLabel == "" {
			devLabel = "Usina"
		}
		if summary.CountsByDevice[devLabel] == nil {
			summary.CountsByDevice[devLabel] = make(map[string]int)
		}
		summary.CountsByDevice[devLabel][inc.Rule]++

		cappedValues := inc.Values
		if len(cappedValues) > 30 {
			cappedValues = cappedValues[:30]
		}
		ev := Evidence{
			Rule:        inc.Rule,
			Params:      inc.Params,
			FirstDate:   inc.FirstDate,
			LastDate:    inc.LastDate,
			NumDays:     inc.NumDays,
			DetectedOn:  inc.DetectedOn,
			Values:      cappedValues,
			TotalValues: len(inc.Values),
		}
		evJSONBytes, err := json.Marshal(ev)
		if err != nil {
			return Summary{}, fmt.Errorf("marshal evidence: %w", err)
		}
		evJSON := string(evJSONBytes)

		resolveAfterDays := getParamInt(inc.Params, "resolve_after_days", 3)

		if existing, ok := existingMap[dedupeKey]; ok {
			hasChanged := false
			if existing.LastDate != inc.LastDate {
				existing.LastDate = inc.LastDate
				hasChanged = true
			}
			if existing.Severity != inc.Severity {
				existing.Severity = inc.Severity
				hasChanged = true
			}
			if existing.Title != inc.Title {
				existing.Title = inc.Title
				hasChanged = true
			}
			if existing.EvidenceJSON != evJSON {
				existing.EvidenceJSON = evJSON
				hasChanged = true
			}

			// Auto-resolve check for existing open or ack alerts
			if existing.Status == "open" || existing.Status == "ack" {
				daysPast := daysBetween(existing.LastDate, asOfDate)
				if daysPast >= resolveAfterDays {
					existing.Status = "resolved"
					hasChanged = true
					summary.Resolved++
				}
			}

			if hasChanged {
				if err := st.SaveAlert(ctx, existing); err != nil {
					return Summary{}, fmt.Errorf("update alert: %w", err)
				}
				summary.Updated++
			}
			summary.Alerts = append(summary.Alerts, *existing)
		} else {
			// New alert
			status := "open"
			wasResolved := false
			daysPast := daysBetween(inc.LastDate, asOfDate)
			if daysPast >= resolveAfterDays {
				status = "resolved"
				wasResolved = true
			}

			newAlert := store.Alert{
				PlantID:      plantID,
				DeviceID:     inc.DeviceID,
				Rule:         inc.Rule,
				Severity:     inc.Severity,
				Status:       status,
				DedupeKey:    dedupeKey,
				Title:        inc.Title,
				EvidenceJSON: evJSON,
				FirstDate:    inc.FirstDate,
				LastDate:     inc.LastDate,
			}
			if err := st.SaveAlert(ctx, &newAlert); err != nil {
				return Summary{}, fmt.Errorf("save new alert: %w", err)
			}
			summary.New++
			if wasResolved {
				summary.Resolved++
			}
			summary.Alerts = append(summary.Alerts, newAlert)
		}
	}

	// Defect 5: Orphan alerts
	// After processing incidents, any open or ack alert of the plant, whose dedupe key did not appear
	// and whose rule was evaluated in this run, is set to resolved, keeping its note, and counted in Resolved.
	// Skipped rules must not resolve anything. In replay mode (AsOf set), do not apply this step.
	if opts.AsOf == "" {
		for i := range existingAlerts {
			existing := existingAlerts[i]
			if existing.Status == "open" || existing.Status == "ack" {
				if !processedKeys[existing.DedupeKey] && evaluatedRules[existing.Rule] {
					existing.Status = "resolved"
					if err := st.SaveAlert(ctx, &existing); err != nil {
						return Summary{}, fmt.Errorf("resolve orphan alert %d: %w", existing.ID, err)
					}
					summary.Resolved++
					summary.Alerts = append(summary.Alerts, existing)
				}
			}
		}
	}

	return summary, nil
}
