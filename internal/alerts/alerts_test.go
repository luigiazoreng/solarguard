package alerts_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/alerts"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/quality"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func setupTestDB(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, ctx
}

func createTestPlant(t *testing.T, ctx context.Context, st *store.Store, name, tz string, devNames ...string) (domain.Plant, []domain.Device) {
	return createTestPlantWithKind(t, ctx, st, name, tz, "file", devNames...)
}

func insertDailyReadings(t *testing.T, ctx context.Context, st *store.Store, devID int64, tz string, dates []string, vals []float64, rawVals []string) {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	var readings []domain.Reading
	for i, dStr := range dates {
		tDate, err := time.ParseInLocation(time.DateOnly, dStr, loc)
		if err != nil {
			t.Fatalf("parse date: %v", err)
		}
		pStart := tDate.UTC()
		pEnd := tDate.AddDate(0, 0, 1).UTC()
		v := vals[i]
		raw := rawVals[i]
		readings = append(readings, domain.Reading{
			DeviceID:    devID,
			Metric:      domain.MetricEnergyKWh,
			PeriodStart: pStart,
			PeriodEnd:   pEnd,
			Value:       &v,
			RawValue:    raw,
		})
	}
	if _, err := st.InsertReadings(ctx, readings); err != nil {
		t.Fatalf("insert readings: %v", err)
	}
}

// 1. Each rule fires, and does not fire just below its threshold.
func TestRuleThresholds_FiresAndDoesNotFireJustBelow(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-Thresholds", tz, "D1", "D2", "D3")
	d1 := devs[0]
	d2 := devs[1]
	d3 := devs[2]

	// 1a. stuck_value:
	// D1 has 3 consecutive stuck days ("15.00") -> fires stuck_value
	// D2 has 2 consecutive stuck days ("15.00") then 1 different day ("16.00") -> does not fire stuck_value
	dates := []string{"2026-03-01", "2026-03-02", "2026-03-03", "2026-03-04"}
	insertDailyReadings(t, ctx, st, d1.ID, tz, dates[:3], []float64{15.0, 15.0, 15.0}, []string{"15.00", "15.00", "15.00"})
	insertDailyReadings(t, ctx, st, d2.ID, tz, dates[:3], []float64{15.0, 15.0, 16.0}, []string{"15.00", "15.00", "16.00"})
	insertDailyReadings(t, ctx, st, d3.ID, tz, dates[:3], []float64{20.0, 21.0, 22.0}, []string{"20.00", "21.00", "22.00"})

	if _, err := quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{}); err != nil {
		t.Fatalf("classify: %v", err)
	}
	sum, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run alerts: %v", err)
	}

	if sum.CountsByRule["stuck_value"] != 1 {
		t.Fatalf("expected 1 stuck_value alert, got %d", sum.CountsByRule["stuck_value"])
	}
	if sum.CountsByDevice["D1"]["stuck_value"] != 1 {
		t.Errorf("expected D1 to have 1 stuck_value, got %d", sum.CountsByDevice["D1"]["stuck_value"])
	}
	if sum.CountsByDevice["D2"]["stuck_value"] != 0 {
		t.Errorf("expected D2 to have 0 stuck_value, got %d", sum.CountsByDevice["D2"]["stuck_value"])
	}

	// 1b. zero_generation:
	// 1 or 2 days has severity warning; 3 days has severity critical.
	stZero, ctxZero := setupTestDB(t)
	pZero, devsZero := createTestPlant(t, ctxZero, stZero, "Plant-Zero", tz, "D1", "D2")
	dZero1 := devsZero[0]
	dZero2 := devsZero[1]
	// D2 generating ok > 1 on all days with varying values (not stuck)
	insertDailyReadings(t, ctxZero, stZero, dZero2.ID, tz, []string{"2026-04-01", "2026-04-02", "2026-04-03"},
		[]float64{10.0, 11.0, 12.0}, []string{"10.0", "11.0", "12.0"})
	// 1 day zero for D1
	insertDailyReadings(t, ctxZero, stZero, dZero1.ID, tz, []string{"2026-04-01"}, []float64{0.0}, []string{"0.0"})

	quality.Classify(ctxZero, stZero, pZero.ID, time.Time{}, time.Time{})
	sumZero1, err := alerts.Run(ctxZero, stZero, pZero.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run zero 1 day: %v", err)
	}
	if sumZero1.CountsByRule["zero_generation"] != 1 {
		t.Fatalf("expected 1 zero_generation alert for 1 day, got %d", sumZero1.CountsByRule["zero_generation"])
	}
	alertsList, _ := stZero.ListAlerts(ctxZero, store.AlertFilter{PlantID: &pZero.ID, Rule: "zero_generation"})
	if len(alertsList) != 1 || alertsList[0].Severity != "warning" {
		t.Fatalf("expected severity warning for 1 day zero, got %+v", alertsList)
	}

	// Now add 2 more consecutive zero days -> 3 days total -> critical
	insertDailyReadings(t, ctxZero, stZero, dZero1.ID, tz, []string{"2026-04-02", "2026-04-03"}, []float64{0.0, 0.0}, []string{"0.0", "0.0"})
	quality.Classify(ctxZero, stZero, pZero.ID, time.Time{}, time.Time{})
	sumZero3, err := alerts.Run(ctxZero, stZero, pZero.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run zero 3 days: %v", err)
	}
	alertsList3, _ := stZero.ListAlerts(ctxZero, store.AlertFilter{PlantID: &pZero.ID, Rule: "zero_generation"})
	if len(alertsList3) != 1 || alertsList3[0].Severity != "critical" {
		t.Fatalf("expected severity critical for 3 days zero, got %+v", alertsList3)
	}
	_ = sumZero3

	// 1c. implausible_reading:
	// A day with implausible_high (e.g. 5,000,000 kWh) fires, normal does not.
	stImp, ctxImp := setupTestDB(t)
	pImp, devsImp := createTestPlant(t, ctxImp, stImp, "Plant-Imp", tz, "D1")
	insertDailyReadings(t, ctxImp, stImp, devsImp[0].ID, tz, []string{"2026-05-01", "2026-05-02"},
		[]float64{5000000.0, 15.0}, []string{"5000000", "15.0"})
	quality.Classify(ctxImp, stImp, pImp.ID, time.Time{}, time.Time{})
	sumImp, err := alerts.Run(ctxImp, stImp, pImp.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run implausible: %v", err)
	}
	if sumImp.CountsByRule["implausible_reading"] != 1 {
		t.Fatalf("expected 1 implausible_reading alert, got %d", sumImp.CountsByRule["implausible_reading"])
	}

	// 1d. plant_no_generation:
	// All devices have zero on the same day -> fires with device_id NULL.
	// If one device generates ok, does not fire.
	stPlantNoGen, ctxPNG := setupTestDB(t)
	pPNG, devsPNG := createTestPlant(t, ctxPNG, stPlantNoGen, "Plant-PNG", tz, "D1", "D2")
	insertDailyReadings(t, ctxPNG, stPlantNoGen, devsPNG[0].ID, tz, []string{"2026-06-01"}, []float64{0.0}, []string{"0.0"})
	insertDailyReadings(t, ctxPNG, stPlantNoGen, devsPNG[1].ID, tz, []string{"2026-06-01"}, []float64{0.0}, []string{"0.0"})
	quality.Classify(ctxPNG, stPlantNoGen, pPNG.ID, time.Time{}, time.Time{})
	sumPNG, err := alerts.Run(ctxPNG, stPlantNoGen, pPNG.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run plant_no_generation: %v", err)
	}
	if sumPNG.CountsByRule["plant_no_generation"] != 1 {
		t.Fatalf("expected 1 plant_no_generation, got %d", sumPNG.CountsByRule["plant_no_generation"])
	}
	pngAlerts, _ := stPlantNoGen.ListAlerts(ctxPNG, store.AlertFilter{PlantID: &pPNG.ID, Rule: "plant_no_generation"})
	if len(pngAlerts) != 1 || pngAlerts[0].DeviceID != nil {
		t.Fatalf("expected plant_no_generation alert with device_id NULL, got %+v", pngAlerts)
	}

	// 1e. peer_divergence:
	// 3 consecutive days below 0.7 * median(peers), and median >= 5.
	// Test fires at < 0.7 * median, and does NOT fire at >= 0.7 or when median < 5 or only 2 days.
	stDiv, ctxDiv := setupTestDB(t)
	pDiv, devsDiv := createTestPlant(t, ctxDiv, stDiv, "Plant-Div", tz, "D1", "D2", "D3")
	// D2 and D3 generate 10.0 kWh (median = 10.0 >= 5.0)
	// Day 1: D1 is 6.0 (< 0.7*10 = 7.0)
	// Day 2: D1 is 6.0
	// Day 3: D1 is 6.0 -> 3 days! fires!
	pDivDates := []string{"2026-07-01", "2026-07-02", "2026-07-03"}
	insertDailyReadings(t, ctxDiv, stDiv, devsDiv[1].ID, tz, pDivDates, []float64{10.0, 10.1, 10.2}, []string{"10.0", "10.1", "10.2"})
	insertDailyReadings(t, ctxDiv, stDiv, devsDiv[2].ID, tz, pDivDates, []float64{10.0, 10.1, 10.2}, []string{"10.0", "10.1", "10.2"})
	insertDailyReadings(t, ctxDiv, stDiv, devsDiv[0].ID, tz, pDivDates, []float64{6.0, 6.1, 6.2}, []string{"6.0", "6.1", "6.2"})
	quality.Classify(ctxDiv, stDiv, pDiv.ID, time.Time{}, time.Time{})
	sumDiv, err := alerts.Run(ctxDiv, stDiv, pDiv.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run peer_divergence: %v", err)
	}
	if sumDiv.CountsByRule["peer_divergence"] != 1 {
		t.Fatalf("expected 1 peer_divergence alert, got %d", sumDiv.CountsByRule["peer_divergence"])
	}

	// Just below threshold: D1 is 7.5 (>= 0.7 * 10 = 7.0) -> does NOT fire
	stDivNo, ctxDivNo := setupTestDB(t)
	pDivNo, devsDivNo := createTestPlant(t, ctxDivNo, stDivNo, "Plant-DivNo", tz, "D1", "D2", "D3")
	insertDailyReadings(t, ctxDivNo, stDivNo, devsDivNo[1].ID, tz, pDivDates, []float64{10.0, 10.0, 10.0}, []string{"10", "10", "10"})
	insertDailyReadings(t, ctxDivNo, stDivNo, devsDivNo[2].ID, tz, pDivDates, []float64{10.0, 10.0, 10.0}, []string{"10", "10", "10"})
	insertDailyReadings(t, ctxDivNo, stDivNo, devsDivNo[0].ID, tz, pDivDates, []float64{7.5, 7.5, 7.5}, []string{"7.5", "7.5", "7.5"})
	quality.Classify(ctxDivNo, stDivNo, pDivNo.ID, time.Time{}, time.Time{})
	sumDivNo, err := alerts.Run(ctxDivNo, stDivNo, pDivNo.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run peer_divergence just below: %v", err)
	}
	if sumDivNo.CountsByRule["peer_divergence"] != 0 {
		t.Fatalf("expected 0 peer_divergence alerts for 7.5 kWh, got %d", sumDivNo.CountsByRule["peer_divergence"])
	}
}

// 2. detected_on for a stuck run is the 3rd day.
func TestDetectedOn_StuckRunIsThirdDay(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-DetectedOn", tz, "D1")
	d1 := devs[0]

	dates := []string{"2026-03-10", "2026-03-11", "2026-03-12", "2026-03-13", "2026-03-14"}
	vals := []float64{12.34, 12.34, 12.34, 12.34, 12.34}
	raws := []string{"12.34", "12.34", "12.34", "12.34", "12.34"}
	insertDailyReadings(t, ctx, st, d1.ID, tz, dates, vals, raws)

	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})
	sum, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run alerts: %v", err)
	}

	if sum.CountsByRule["stuck_value"] != 1 {
		t.Fatalf("expected 1 stuck_value alert, got %d", sum.CountsByRule["stuck_value"])
	}

	alertsList, err := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID, Rule: "stuck_value"})
	if err != nil || len(alertsList) != 1 {
		t.Fatalf("list alerts: %v, len=%d", err, len(alertsList))
	}
	a := alertsList[0]
	if a.FirstDate != "2026-03-10" {
		t.Errorf("expected FirstDate 2026-03-10, got %s", a.FirstDate)
	}
	if a.LastDate != "2026-03-14" {
		t.Errorf("expected LastDate 2026-03-14, got %s", a.LastDate)
	}

	var ev alerts.Evidence
	if err := json.Unmarshal([]byte(a.EvidenceJSON), &ev); err != nil {
		t.Fatalf("unmarshal evidence: %v", err)
	}
	if ev.DetectedOn != "2026-03-12" {
		t.Fatalf("expected detected_on to be 3rd day 2026-03-12, got %s", ev.DetectedOn)
	}
}

// 3. Two separate episodes on one device create two alerts; running twice creates no duplicates; the second run reports 0 new.
func TestTwoSeparateEpisodesAndIdempotency(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-Episodes", tz, "D1")
	d1 := devs[0]

	// Episode 1: 2026-03-01..2026-03-03 stuck at 10.0
	// Normal: 2026-03-04..2026-03-06 varying values
	// Episode 2: 2026-03-07..2026-03-09 stuck at 20.0
	dates := []string{
		"2026-03-01", "2026-03-02", "2026-03-03",
		"2026-03-04", "2026-03-05", "2026-03-06",
		"2026-03-07", "2026-03-08", "2026-03-09",
	}
	vals := []float64{
		10.0, 10.0, 10.0,
		15.0, 16.0, 17.0,
		20.0, 20.0, 20.0,
	}
	raws := []string{
		"10.0", "10.0", "10.0",
		"15.0", "16.0", "17.0",
		"20.0", "20.0", "20.0",
	}
	insertDailyReadings(t, ctx, st, d1.ID, tz, dates, vals, raws)

	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	// First run
	sum1, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if sum1.New != 2 {
		t.Fatalf("expected 2 new alerts on first run, got %d", sum1.New)
	}
	if sum1.CountsByRule["stuck_value"] != 2 {
		t.Fatalf("expected 2 stuck_value alerts, got %d", sum1.CountsByRule["stuck_value"])
	}

	allAlerts1, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID})
	if len(allAlerts1) != 2 {
		t.Fatalf("expected 2 total alerts stored, got %d", len(allAlerts1))
	}

	// Second run with identical data
	sum2, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if sum2.New != 0 {
		t.Fatalf("expected 0 new alerts on second run, got %d", sum2.New)
	}

	allAlerts2, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID})
	if len(allAlerts2) != 2 {
		t.Fatalf("expected still 2 total alerts stored, got %d", len(allAlerts2))
	}
}

// 4. Ack is preserved after a re-run; a resolved alert with a growing last_date updates but keeps its status.
func TestLifecycle_AckPreservedAndResolvedUpdates(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-Lifecycle", tz, "D1", "D2")
	d1 := devs[0]
	d2 := devs[1]

	// Create stuck on D1 and D2
	dates := []string{"2026-03-01", "2026-03-02", "2026-03-03"}
	insertDailyReadings(t, ctx, st, d1.ID, tz, dates, []float64{10.0, 10.0, 10.0}, []string{"10", "10", "10"})
	insertDailyReadings(t, ctx, st, d2.ID, tz, dates, []float64{20.0, 20.0, 20.0}, []string{"20", "20", "20"})

	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})
	sum1, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil || sum1.New != 2 {
		t.Fatalf("run 1: %v, new=%d", err, sum1.New)
	}

	alertsList, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID})
	var alertD1, alertD2 store.Alert
	for _, a := range alertsList {
		if *a.DeviceID == d1.ID {
			alertD1 = a
		} else if *a.DeviceID == d2.ID {
			alertD2 = a
		}
	}

	// Manually ack D1 with a note
	if err := st.AckAlert(ctx, alertD1.ID, "Investigando inversor D1"); err != nil {
		t.Fatalf("ack alert D1: %v", err)
	}

	// Manually resolve D2 with a note
	if err := st.ResolveAlert(ctx, alertD2.ID, "Resolvido manualmente"); err != nil {
		t.Fatalf("resolve alert D2: %v", err)
	}

	// Now add 2 more days of stuck data to both D1 and D2
	moreDates := []string{"2026-03-04", "2026-03-05"}
	insertDailyReadings(t, ctx, st, d1.ID, tz, moreDates, []float64{10.0, 10.0}, []string{"10", "10"})
	insertDailyReadings(t, ctx, st, d2.ID, tz, moreDates, []float64{20.0, 20.0}, []string{"20", "20"})

	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})
	sum2, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if sum2.New != 0 {
		t.Errorf("expected 0 new alerts on re-run, got %d", sum2.New)
	}

	gotD1, err := st.GetAlert(ctx, alertD1.ID)
	if err != nil {
		t.Fatalf("get D1: %v", err)
	}
	if gotD1.Status != "ack" {
		t.Fatalf("expected D1 status to remain ack, got %s", gotD1.Status)
	}
	if gotD1.Note != "Investigando inversor D1" {
		t.Fatalf("expected D1 note preserved, got %q", gotD1.Note)
	}
	if gotD1.LastDate != "2026-03-05" {
		t.Fatalf("expected D1 last_date updated to 2026-03-05, got %s", gotD1.LastDate)
	}

	gotD2, err := st.GetAlert(ctx, alertD2.ID)
	if err != nil {
		t.Fatalf("get D2: %v", err)
	}
	if gotD2.Status != "resolved" {
		t.Fatalf("expected D2 status to remain resolved despite growing last_date, got %s", gotD2.Status)
	}
	if gotD2.Note != "Resolvido manualmente" {
		t.Fatalf("expected D2 note preserved, got %q", gotD2.Note)
	}
	if gotD2.LastDate != "2026-03-05" {
		t.Fatalf("expected D2 last_date updated to 2026-03-05, got %s", gotD2.LastDate)
	}
}

// 5. Auto-resolve happens only after resolve_after_days; replay with AsOf in the middle of an episode leaves it open.
func TestAutoResolve_ResolveAfterDaysAndReplayAsOf(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-AutoResolve", tz, "D1")
	d1 := devs[0]

	// Episode: stuck from 2026-03-01 to 2026-03-05 (5 days).
	// Normal: 2026-03-06 to 2026-03-10 (different values).
	stuckDates := []string{"2026-03-01", "2026-03-02", "2026-03-03", "2026-03-04", "2026-03-05"}
	normalDates := []string{"2026-03-06", "2026-03-07", "2026-03-08", "2026-03-09", "2026-03-10"}
	insertDailyReadings(t, ctx, st, d1.ID, tz, stuckDates, []float64{10, 10, 10, 10, 10}, []string{"10", "10", "10", "10", "10"})
	insertDailyReadings(t, ctx, st, d1.ID, tz, normalDates, []float64{11, 12, 13, 14, 15}, []string{"11", "12", "13", "14", "15"})

	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	// Replay 1: AsOf in the middle of episode: AsOf = "2026-03-03". Alert must be open!
	sumReplay1, err := alerts.Run(ctx, st, plant.ID, alerts.Options{AsOf: "2026-03-03"})
	if err != nil {
		t.Fatalf("replay 1: %v", err)
	}
	alerts1, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID})
	if len(alerts1) != 1 || alerts1[0].Status != "open" {
		t.Fatalf("expected 1 open alert at AsOf 2026-03-03, got %+v", alerts1)
	}
	_ = sumReplay1

	// Replay 2: AsOf = "2026-03-06" (1 day after last_date 2026-03-05).
	// resolve_after_days = 3 -> 1 < 3 -> must still be open!
	sumReplay2, err := alerts.Run(ctx, st, plant.ID, alerts.Options{AsOf: "2026-03-06"})
	if err != nil {
		t.Fatalf("replay 2: %v", err)
	}
	alerts2, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID})
	if len(alerts2) != 1 || alerts2[0].Status != "open" {
		t.Fatalf("expected alert to remain open 1 day after episode, got %+v", alerts2)
	}
	_ = sumReplay2

	// Replay 3: AsOf = "2026-03-07" (2 days after last_date). Still open!
	_, err = alerts.Run(ctx, st, plant.ID, alerts.Options{AsOf: "2026-03-07"})
	if err != nil {
		t.Fatalf("replay 3: %v", err)
	}
	alerts3, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID})
	if len(alerts3) != 1 || alerts3[0].Status != "open" {
		t.Fatalf("expected alert to remain open 2 days after episode, got %+v", alerts3)
	}

	// Replay 4: AsOf = "2026-03-08" (3 days after last_date 2026-03-05).
	// Exactly resolve_after_days (3) -> auto-resolve!
	sumReplay4, err := alerts.Run(ctx, st, plant.ID, alerts.Options{AsOf: "2026-03-08"})
	if err != nil {
		t.Fatalf("replay 4: %v", err)
	}
	if sumReplay4.Resolved != 1 {
		t.Errorf("expected 1 resolved in summary, got %d", sumReplay4.Resolved)
	}
	alerts4, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID})
	if len(alerts4) != 1 || alerts4[0].Status != "resolved" {
		t.Fatalf("expected alert to be resolved at AsOf 2026-03-08, got %+v", alerts4)
	}
}

// 6. Two plants are isolated: same device names, different alerts. A 1-device plant and a 5-device plant work.
func TestTwoPlantsIsolation_OneDeviceAndFiveDevicePlants(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"

	// Plant A: 1 device named "D1"
	plantA, devsA := createTestPlant(t, ctx, st, "Plant-A", tz, "D1")
	// Plant B: 5 devices named "D1", "D2", "D3", "D4", "D5"
	plantB, devsB := createTestPlant(t, ctx, st, "Plant-B", tz, "D1", "D2", "D3", "D4", "D5")

	dates := []string{"2026-03-01", "2026-03-02", "2026-03-03"}

	// In Plant A: D1 has stuck value -> should fire stuck_value for Plant A.
	// 1-device plant skips peer rules cleanly without error.
	insertDailyReadings(t, ctx, st, devsA[0].ID, tz, dates, []float64{10, 10, 10}, []string{"10", "10", "10"})
	quality.Classify(ctx, st, plantA.ID, time.Time{}, time.Time{})

	sumA, err := alerts.Run(ctx, st, plantA.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run plant A (1 device): %v", err)
	}
	if sumA.CountsByRule["stuck_value"] != 1 {
		t.Errorf("expected 1 stuck_value on plant A, got %d", sumA.CountsByRule["stuck_value"])
	}
	if sumA.CountsByRule["peer_divergence"] != 0 {
		t.Errorf("expected 0 peer_divergence on 1-device plant A, got %d", sumA.CountsByRule["peer_divergence"])
	}

	// In Plant B: 5 devices.
	// D1 diverges from peers: D2..D5 generate 20.0 kWh (median = 20.0), D1 generates 5.0 kWh (< 0.7*20 = 14.0).
	// None are stuck (varying values: 5.0, 5.1, 5.2 etc.).
	insertDailyReadings(t, ctx, st, devsB[0].ID, tz, dates, []float64{5.0, 5.1, 5.2}, []string{"5.0", "5.1", "5.2"})
	insertDailyReadings(t, ctx, st, devsB[1].ID, tz, dates, []float64{20.0, 20.1, 20.2}, []string{"20.0", "20.1", "20.2"})
	insertDailyReadings(t, ctx, st, devsB[2].ID, tz, dates, []float64{20.0, 20.1, 20.2}, []string{"20.0", "20.1", "20.2"})
	insertDailyReadings(t, ctx, st, devsB[3].ID, tz, dates, []float64{20.0, 20.1, 20.2}, []string{"20.0", "20.1", "20.2"})
	insertDailyReadings(t, ctx, st, devsB[4].ID, tz, dates, []float64{20.0, 20.1, 20.2}, []string{"20.0", "20.1", "20.2"})

	quality.Classify(ctx, st, plantB.ID, time.Time{}, time.Time{})
	sumB, err := alerts.Run(ctx, st, plantB.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run plant B (5 devices): %v", err)
	}
	if sumB.CountsByRule["peer_divergence"] != 1 {
		t.Errorf("expected 1 peer_divergence on plant B, got %d", sumB.CountsByRule["peer_divergence"])
	}

	// Verify complete isolation of alerts between plants
	alertsA, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plantA.ID})
	alertsB, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plantB.ID})

	for _, a := range alertsA {
		if a.PlantID != plantA.ID {
			t.Errorf("leak: plant A alert has plant_id %d", a.PlantID)
		}
	}
	for _, b := range alertsB {
		if b.PlantID != plantB.ID {
			t.Errorf("leak: plant B alert has plant_id %d", b.PlantID)
		}
	}
}

// 7. Per-plant config override changes the threshold for that plant only.
func TestPerPlantConfigOverride(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"

	plant1, devs1 := createTestPlant(t, ctx, st, "Plant-Cfg1", tz, "D1", "D2", "D3")
	plant2, devs2 := createTestPlant(t, ctx, st, "Plant-Cfg2", tz, "D1", "D2", "D3")

	dates := []string{"2026-03-01", "2026-03-02", "2026-03-03"}

	// Peers generate 10.0 kWh (median = 10.0). D1 generates 6.0 kWh (ratio = 0.60).
	// Default threshold ratio = 0.70 (0.60 < 0.70 -> would fire).
	// For Plant 1 only, override ratio to 0.50 (0.60 >= 0.50 -> will NOT fire).
	if err := st.SetRuleConfig(ctx, store.RuleConfig{
		Rule:       "peer_divergence",
		PlantID:    &plant1.ID,
		Enabled:    true,
		ParamsJSON: `{"ratio": 0.50}`,
	}); err != nil {
		t.Fatalf("set rule config override: %v", err)
	}

	for _, p := range []struct {
		plant domain.Plant
		devs  []domain.Device
	}{
		{plant1, devs1},
		{plant2, devs2},
	} {
		insertDailyReadings(t, ctx, st, p.devs[1].ID, tz, dates, []float64{10.0, 10.1, 10.2}, []string{"10.0", "10.1", "10.2"})
		insertDailyReadings(t, ctx, st, p.devs[2].ID, tz, dates, []float64{10.0, 10.1, 10.2}, []string{"10.0", "10.1", "10.2"})
		insertDailyReadings(t, ctx, st, p.devs[0].ID, tz, dates, []float64{6.0, 6.1, 6.2}, []string{"6.0", "6.1", "6.2"})
		quality.Classify(ctx, st, p.plant.ID, time.Time{}, time.Time{})
	}

	sum1, err := alerts.Run(ctx, st, plant1.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run plant 1: %v", err)
	}
	if sum1.CountsByRule["peer_divergence"] != 0 {
		t.Fatalf("expected 0 peer_divergence on plant 1 due to ratio 0.50 override, got %d", sum1.CountsByRule["peer_divergence"])
	}

	sum2, err := alerts.Run(ctx, st, plant2.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run plant 2: %v", err)
	}
	if sum2.CountsByRule["peer_divergence"] != 1 {
		t.Fatalf("expected 1 peer_divergence on plant 2 with default ratio 0.70, got %d", sum2.CountsByRule["peer_divergence"])
	}
}

// 8. under_expected and gradual_loss are skipped without predictions, and fire with predictions you insert directly.
func TestPredictionsRules_SkippedWithoutPredictionsAndFireWithPredictions(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-Pred", tz, "D1")
	d1 := devs[0]

	// 8a. No predictions in DB (varying values so quality is OK, not stuck)
	dates := []string{"2026-03-01", "2026-03-02", "2026-03-03"}
	insertDailyReadings(t, ctx, st, d1.ID, tz, dates, []float64{3.0, 3.1, 3.2}, []string{"3.0", "3.1", "3.2"})
	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	sumNoPred, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run alerts without predictions: %v", err)
	}
	// Check that under_expected and gradual_loss are reported in SkippedRules
	hasSkippedUE := false
	hasSkippedGL := false
	for _, r := range sumNoPred.SkippedRules {
		if r == "under_expected" {
			hasSkippedUE = true
		}
		if r == "gradual_loss" {
			hasSkippedGL = true
		}
	}
	if !hasSkippedUE || !hasSkippedGL {
		t.Fatalf("expected under_expected and gradual_loss to be in SkippedRules, got %v", sumNoPred.SkippedRules)
	}
	if sumNoPred.CountsByRule["under_expected"] != 0 || sumNoPred.CountsByRule["gradual_loss"] != 0 {
		t.Fatalf("expected 0 alerts for prediction rules when skipped, got %+v", sumNoPred.CountsByRule)
	}

	// 8b. Insert active model run and predictions for under_expected
	var modelRunID int64
	err = st.DB().QueryRowContext(ctx, `INSERT INTO model_runs
		(name, version, plant_id, trained_at, active)
		VALUES ('test_model', 'v1', ?, '2026-03-01T00:00:00Z', 1)
		RETURNING id`, plant.ID).Scan(&modelRunID)
	if err != nil {
		t.Fatalf("insert model_run: %v", err)
	}

	for _, dStr := range dates {
		// Expected is 10.0 kWh (>= 5.0). Measured is 3.0 kWh (< 0.7 * 10 = 7.0).
		_, err := st.DB().ExecContext(ctx, `INSERT INTO predictions
			(device_id, date, model_run_id, kwh_expected, created_at)
			VALUES (?, ?, ?, 10.0, '2026-03-01T00:00:00Z')`, d1.ID, dStr, modelRunID)
		if err != nil {
			t.Fatalf("insert prediction: %v", err)
		}
	}

	sumWithPred, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run with predictions: %v", err)
	}
	if sumWithPred.CountsByRule["under_expected"] != 1 {
		t.Fatalf("expected 1 under_expected alert, got %d", sumWithPred.CountsByRule["under_expected"])
	}

	// 8c. Test gradual_loss:
	// Needs at least 30 baseline days, window of 14 days.
	// Suppose 40 baseline days with value = 10.0, expected = 10.0 (ratio = 1.0).
	// Followed by 14 window days with value = 8.5, expected = 10.0 (ratio = 0.85).
	// Drop is 1.0 - 0.85 = 0.15 > 0.10 -> fires gradual_loss!
	stGL, ctxGL := setupTestDB(t)
	pGL, devsGL := createTestPlant(t, ctxGL, stGL, "Plant-GL", tz, "D1")
	dGL := devsGL[0]

	var mrIDGL int64
	err = stGL.DB().QueryRowContext(ctxGL, `INSERT INTO model_runs
		(name, version, plant_id, trained_at, active)
		VALUES ('test_gl', 'v1', ?, '2026-01-01T00:00:00Z', 1)
		RETURNING id`, pGL.ID).Scan(&mrIDGL)
	if err != nil {
		t.Fatalf("insert model_run for GL: %v", err)
	}

	startDate, _ := time.Parse(time.DateOnly, "2026-01-01")
	var glDates []string
	var glVals []float64
	var glRaws []string

	for i := 0; i < 54; i++ { // 40 baseline + 14 window = 54 days
		dStr := startDate.AddDate(0, 0, i).Format(time.DateOnly)
		glDates = append(glDates, dStr)
		val := 10.0 + float64(i%5)*0.01
		if i >= 40 {
			val = 8.5 + float64(i%5)*0.01
		}
		glVals = append(glVals, val)
		glRaws = append(glRaws, fmt.Sprintf("%.2f", val))

		_, err := stGL.DB().ExecContext(ctxGL, `INSERT INTO predictions
			(device_id, date, model_run_id, kwh_expected, created_at)
			VALUES (?, ?, ?, 10.0, '2026-01-01T00:00:00Z')`, dGL.ID, dStr, mrIDGL)
		if err != nil {
			t.Fatalf("insert prediction for GL day %d: %v", i, err)
		}
	}
	insertDailyReadings(t, ctxGL, stGL, dGL.ID, tz, glDates, glVals, glRaws)
	quality.Classify(ctxGL, stGL, pGL.ID, time.Time{}, time.Time{})

	sumGL, err := alerts.Run(ctxGL, stGL, pGL.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run gradual loss: %v", err)
	}
	if sumGL.CountsByRule["gradual_loss"] != 1 {
		t.Fatalf("expected 1 gradual_loss alert, got %d", sumGL.CountsByRule["gradual_loss"])
	}
}

// 9. Today is never flagged as missing, in a plant's own time zone.
func TestTodayNeverFlaggedAsMissingInPlantTimezone(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-Today", tz, "D1")
	d1 := devs[0]

	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	todayStr := time.Now().In(loc).Format(time.DateOnly)
	yesterdayStr := time.Now().In(loc).AddDate(0, 0, -1).Format(time.DateOnly)
	twoDaysAgoStr := time.Now().In(loc).AddDate(0, 0, -2).Format(time.DateOnly)

	// Readings exist for twoDaysAgo and yesterday, but NOT today
	dates := []string{twoDaysAgoStr, yesterdayStr}
	insertDailyReadings(t, ctx, st, d1.ID, tz, dates, []float64{15.0, 16.0}, []string{"15.0", "16.0"})
	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	sum, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run alerts: %v", err)
	}

	// Missing data rule must NOT flag today!
	if sum.CountsByRule["missing_data"] != 0 {
		t.Fatalf("expected 0 missing_data alerts (today must not be flagged), got %d", sum.CountsByRule["missing_data"])
	}

	alertsList, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID, Rule: "missing_data"})
	for _, a := range alertsList {
		if a.FirstDate == todayStr || a.LastDate == todayStr {
			t.Fatalf("alert flagged today %s as missing: %+v", todayStr, a)
		}
	}
}

func TestStoreAlerts_StatusChangesAndNotFound(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-Store", tz, "D1")
	d1 := devs[0]

	// Non-existent ID checks
	if err := st.AckAlert(ctx, 99999, "note"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound for AckAlert(99999), got %v", err)
	}
	if err := st.ResolveAlert(ctx, 99999, "note"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound for ResolveAlert(99999), got %v", err)
	}
	if err := st.ReopenAlert(ctx, 99999, "note"); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound for ReopenAlert(99999), got %v", err)
	}
	if _, err := st.GetAlert(ctx, 99999); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound for GetAlert(99999), got %v", err)
	}

	// Create an alert
	devID := d1.ID
	a := store.Alert{
		PlantID:   plant.ID,
		DeviceID:  &devID,
		Rule:      "stuck_value",
		Severity:  "critical",
		Status:    "open",
		DedupeKey: "stuck_value|d1|2026-01-01",
		Title:     "Leitura travada em D1",
		FirstDate: "2026-01-01",
		LastDate:  "2026-01-05",
	}
	if err := st.SaveAlert(ctx, &a); err != nil {
		t.Fatalf("save alert: %v", err)
	}
	if a.ID == 0 {
		t.Fatalf("expected a.ID to be assigned")
	}

	// Ack alert
	if err := st.AckAlert(ctx, a.ID, "Em analise"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	got, err := st.GetAlert(ctx, a.ID)
	if err != nil {
		t.Fatalf("get alert: %v", err)
	}
	if got.Status != "ack" || got.Note != "Em analise" {
		t.Fatalf("unexpected alert after ack: %+v", got)
	}

	// Resolve alert
	if err := st.ResolveAlert(ctx, a.ID, "Resolvido"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, _ = st.GetAlert(ctx, a.ID)
	if got.Status != "resolved" || got.Note != "Resolvido" {
		t.Fatalf("unexpected alert after resolve: %+v", got)
	}

	// Reopen alert (note is empty, so existing note is preserved)
	if err := st.ReopenAlert(ctx, a.ID, ""); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, _ = st.GetAlert(ctx, a.ID)
	if got.Status != "open" || got.Note != "Resolvido" {
		t.Fatalf("unexpected alert after reopen: %+v", got)
	}

	// CountOpenAlertsByPlant
	counts, err := st.CountOpenAlertsByPlant(ctx)
	if err != nil {
		t.Fatalf("count open: %v", err)
	}
	if counts[plant.ID] != 1 {
		t.Fatalf("expected count 1 for plant %d, got %d", plant.ID, counts[plant.ID])
	}

	// ListAlerts filters
	list, err := st.ListAlerts(ctx, store.AlertFilter{
		PlantID:  &plant.ID,
		DeviceID: &devID,
		Status:   "open",
		Severity: "critical",
		Rule:     "stuck_value",
		FromDate: "2026-01-01",
		ToDate:   "2026-01-06",
		Limit:    10,
		Offset:   0,
	})
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 alert matching filters, got %d", len(list))
	}
}

func createTestPlantWithKind(t *testing.T, ctx context.Context, st *store.Store, name, tz, srcKind string, devNames ...string) (domain.Plant, []domain.Device) {
	t.Helper()
	src, err := st.EnsureSource(ctx, domain.Source{
		Kind: srcKind,
		Name: "test-src-" + name,
	})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	p, err := st.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "ext-" + name,
		Name:       name,
		Timezone:   tz,
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}
	var devs []domain.Device
	for _, dName := range devNames {
		area := 42.0
		eff := 0.18
		d, err := st.UpsertDevice(ctx, domain.Device{
			PlantID:     p.ID,
			ExternalID:  "ext-" + dName,
			Name:        dName,
			Kind:        "inverter",
			PanelAreaM2: &area,
			Efficiency:  &eff,
		})
		if err != nil {
			t.Fatalf("upsert device: %v", err)
		}
		devs = append(devs, d)
	}
	return p, devs
}

// 1. Defect 1: Silent stop is never flagged.
// missing_data only checks up to each device's own last reading, so a device that stops
// reporting while its peers continue raises nothing.
// TestDeviceThatStopsReportingIsFlagged: D1 stops 5 days before D2. The alert stays one growing incident.
func TestDeviceThatStopsReportingIsFlagged(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-SilentStop", tz, "D1", "D2")
	d1 := devs[0]
	d2 := devs[1]

	// Dates: 2026-03-01 to 2026-03-10 (10 days).
	// D2 reports on all 10 days.
	// D1 reports on 2026-03-01..2026-03-05 (5 days) and stops.
	var d2Dates []string
	var d2Vals []float64
	var d2Raws []string
	for i := 1; i <= 10; i++ {
		dStr := fmt.Sprintf("2026-03-%02d", i)
		d2Dates = append(d2Dates, dStr)
		val := 15.0 + float64(i)*0.1
		d2Vals = append(d2Vals, val)
		d2Raws = append(d2Raws, fmt.Sprintf("%.2f", val))
	}
	insertDailyReadings(t, ctx, st, d2.ID, tz, d2Dates, d2Vals, d2Raws)

	var d1Dates []string
	var d1Vals []float64
	var d1Raws []string
	for i := 1; i <= 5; i++ {
		dStr := fmt.Sprintf("2026-03-%02d", i)
		d1Dates = append(d1Dates, dStr)
		val := 15.0 + float64(i)*0.1
		d1Vals = append(d1Vals, val)
		d1Raws = append(d1Raws, fmt.Sprintf("%.2f", val))
	}
	insertDailyReadings(t, ctx, st, d1.ID, tz, d1Dates, d1Vals, d1Raws)

	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	// Fake now as 2026-03-15 (so none of 03-01..03-10 is "today")
	fixedNow, _ := time.Parse(time.RFC3339, "2026-03-15T12:00:00-03:00")
	opts := alerts.Options{
		Now: func() time.Time { return fixedNow },
	}
	sum, err := alerts.Run(ctx, st, plant.ID, opts)
	if err != nil {
		t.Fatalf("run alerts: %v", err)
	}

	if sum.CountsByRule["missing_data"] == 0 {
		t.Fatalf("expected missing_data alert for D1 that stopped reporting, got 0")
	}

	alertsList, err := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID, Rule: "missing_data"})
	if err != nil || len(alertsList) != 1 {
		t.Fatalf("expected 1 missing_data alert, got %d (err: %v)", len(alertsList), err)
	}
	a := alertsList[0]
	if a.DeviceID == nil || *a.DeviceID != d1.ID {
		t.Fatalf("expected alert for D1, got device_id %v", a.DeviceID)
	}
	if a.FirstDate != "2026-03-06" {
		t.Errorf("expected FirstDate 2026-03-06, got %s", a.FirstDate)
	}
	if a.LastDate != "2026-03-10" {
		t.Errorf("expected LastDate 2026-03-10, got %s", a.LastDate)
	}
}

// 2. Defect 2: Stale data rule.
func TestStaleDataFiresForLiveSource(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlantWithKind(t, ctx, st, "Plant-LiveStale", tz, "growatt", "D1")
	d1 := devs[0]

	// Last reading is 2026-03-05
	insertDailyReadings(t, ctx, st, d1.ID, tz, []string{"2026-03-05"}, []float64{10.0}, []string{"10.0"})
	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	// Now is 2026-03-10 (5 days after 2026-03-05 > max_age_days 2).
	fixedNow, _ := time.Parse(time.RFC3339, "2026-03-10T12:00:00-03:00")
	sum, err := alerts.Run(ctx, st, plant.ID, alerts.Options{
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("run alerts: %v", err)
	}

	if sum.CountsByRule["stale_data"] != 1 {
		t.Fatalf("expected 1 stale_data alert, got %d", sum.CountsByRule["stale_data"])
	}

	alertsList, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID, Rule: "stale_data"})
	if len(alertsList) != 1 {
		t.Fatalf("expected 1 stale_data alert in store, got %d", len(alertsList))
	}
	a := alertsList[0]
	if a.DeviceID != nil {
		t.Errorf("expected DeviceID NULL for stale_data, got %v", a.DeviceID)
	}
	if a.Severity != "critical" {
		t.Errorf("expected severity critical, got %s", a.Severity)
	}
	if a.FirstDate != "2026-03-06" {
		t.Errorf("expected first_date 2026-03-06, got %s", a.FirstDate)
	}
	if a.LastDate != "2026-03-09" { // local yesterday
		t.Errorf("expected last_date 2026-03-09, got %s", a.LastDate)
	}
	expectedKey := "stale_data|plant|2026-03-06"
	if a.DedupeKey != expectedKey {
		t.Errorf("expected dedupe_key %s, got %s", expectedKey, a.DedupeKey)
	}
}

func TestStaleDataSkippedForFileSource(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlantWithKind(t, ctx, st, "Plant-FileStale", tz, "file", "D1")
	d1 := devs[0]

	// Last reading is 2026-03-05
	insertDailyReadings(t, ctx, st, d1.ID, tz, []string{"2026-03-05"}, []float64{10.0}, []string{"10.0"})
	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	fixedNow, _ := time.Parse(time.RFC3339, "2026-03-10T12:00:00-03:00")
	sum, err := alerts.Run(ctx, st, plant.ID, alerts.Options{
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("run alerts: %v", err)
	}

	if sum.CountsByRule["stale_data"] != 0 {
		t.Fatalf("expected 0 stale_data alerts for file source, got %d", sum.CountsByRule["stale_data"])
	}
	hasSkipped := false
	for _, r := range sum.SkippedRules {
		if r == "stale_data" {
			hasSkipped = true
		}
	}
	if !hasSkipped {
		t.Errorf("expected stale_data in SkippedRules for file source, got %v", sum.SkippedRules)
	}
}

func TestStaleDataResolvesWhenDataReturns(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlantWithKind(t, ctx, st, "Plant-StaleReturns", tz, "growatt", "D1")
	d1 := devs[0]

	insertDailyReadings(t, ctx, st, d1.ID, tz, []string{"2026-03-05"}, []float64{10.0}, []string{"10.0"})
	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	fixedNow1, _ := time.Parse(time.RFC3339, "2026-03-10T12:00:00-03:00")
	sum1, err := alerts.Run(ctx, st, plant.ID, alerts.Options{
		Now: func() time.Time { return fixedNow1 },
	})
	if err != nil || sum1.CountsByRule["stale_data"] != 1 {
		t.Fatalf("run 1: %v, stale_data counts=%d", err, sum1.CountsByRule["stale_data"])
	}

	// New readings arrive for 2026-03-09 and 2026-03-10
	insertDailyReadings(t, ctx, st, d1.ID, tz, []string{"2026-03-09", "2026-03-10"}, []float64{11.0, 12.0}, []string{"11.0", "12.0"})
	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	// Now is 2026-03-11 (newest reading 2026-03-10 is age 1 <= 2).
	fixedNow2, _ := time.Parse(time.RFC3339, "2026-03-11T12:00:00-03:00")
	sum2, err := alerts.Run(ctx, st, plant.ID, alerts.Options{
		Now: func() time.Time { return fixedNow2 },
	})
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}

	alertsList, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID, Rule: "stale_data"})
	if len(alertsList) != 1 || alertsList[0].Status != "resolved" {
		t.Fatalf("expected stale_data alert to be resolved, got %+v (sum.Resolved=%d)", alertsList, sum2.Resolved)
	}
}

func TestStaleDataNotEvaluatedInReplay(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlantWithKind(t, ctx, st, "Plant-StaleReplay", tz, "growatt", "D1")
	d1 := devs[0]

	insertDailyReadings(t, ctx, st, d1.ID, tz, []string{"2026-03-01"}, []float64{10.0}, []string{"10.0"})
	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	fixedNow, _ := time.Parse(time.RFC3339, "2026-03-10T12:00:00-03:00")
	sum, err := alerts.Run(ctx, st, plant.ID, alerts.Options{
		AsOf: "2026-03-01",
		Now:  func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}

	if sum.CountsByRule["stale_data"] != 0 {
		t.Fatalf("expected 0 stale_data alerts during replay, got %d", sum.CountsByRule["stale_data"])
	}
}

// 3. Defect 3: Config overrides are ignored in two ways.
// resolve_after_days is read from inc.Params, but effectiveParams never contains it, so it is always 3.
// And implausible_reading, plant_no_generation and missing_data pass an empty effectiveParams.
// Test TestResolveAfterDaysOverride for at least two rules, one of them implausible_reading.
func TestResolveAfterDaysOverride(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-ResolveOverride", tz, "D1", "D2")
	d1 := devs[0]
	d2 := devs[1]

	// Configure resolve_after_days = 1 for implausible_reading and stuck_value
	if err := st.SetRuleConfig(ctx, store.RuleConfig{
		Rule:       "implausible_reading",
		PlantID:    &plant.ID,
		Enabled:    true,
		ParamsJSON: `{"resolve_after_days": 1}`,
	}); err != nil {
		t.Fatalf("config implausible_reading: %v", err)
	}
	if err := st.SetRuleConfig(ctx, store.RuleConfig{
		Rule:       "stuck_value",
		PlantID:    &plant.ID,
		Enabled:    true,
		ParamsJSON: `{"resolve_after_days": 1}`,
	}); err != nil {
		t.Fatalf("config stuck_value: %v", err)
	}

	// D1 has implausible reading on 2026-03-01, normal on 2026-03-02..2026-03-03
	insertDailyReadings(t, ctx, st, d1.ID, tz, []string{"2026-03-01", "2026-03-02", "2026-03-03"},
		[]float64{9000000.0, 10.0, 11.0}, []string{"9000000", "10.0", "11.0"})

	// D2 has stuck reading on 2026-03-01..2026-03-03, normal on 2026-03-04
	insertDailyReadings(t, ctx, st, d2.ID, tz, []string{"2026-03-01", "2026-03-02", "2026-03-03", "2026-03-04"},
		[]float64{20.0, 20.0, 20.0, 25.0}, []string{"20.0", "20.0", "20.0", "25.0"})

	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	// Replay as of 2026-03-02 (for D1, last_date is 2026-03-01, 1 day later -> with override 1, auto-resolves!)
	sum1, err := alerts.Run(ctx, st, plant.ID, alerts.Options{AsOf: "2026-03-02"})
	if err != nil {
		t.Fatalf("run AsOf 2026-03-02: %v", err)
	}

	alertsList1, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID, Rule: "implausible_reading"})
	if len(alertsList1) != 1 {
		t.Fatalf("expected 1 implausible_reading alert, got %d", len(alertsList1))
	}
	var ev1 alerts.Evidence
	json.Unmarshal([]byte(alertsList1[0].EvidenceJSON), &ev1)
	if ev1.Params["resolve_after_days"] != float64(1) && ev1.Params["resolve_after_days"] != 1 {
		t.Fatalf("expected resolve_after_days: 1 in evidence params, got %v", ev1.Params["resolve_after_days"])
	}
	if alertsList1[0].Status != "resolved" {
		t.Fatalf("expected implausible_reading to be resolved after 1 day with override 1, got status %s", alertsList1[0].Status)
	}
	_ = sum1

	// Full run up to 2026-03-04 (for D2 stuck_value, last_date is 2026-03-03; on 2026-03-04 it is 1 day later -> auto-resolves!)
	sum2, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run full: %v", err)
	}
	alertsList2, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID, Rule: "stuck_value"})
	if len(alertsList2) != 1 {
		t.Fatalf("expected 1 stuck_value alert, got %d", len(alertsList2))
	}
	var ev2 alerts.Evidence
	json.Unmarshal([]byte(alertsList2[0].EvidenceJSON), &ev2)
	if ev2.Params["resolve_after_days"] != float64(1) && ev2.Params["resolve_after_days"] != 1 {
		t.Fatalf("expected resolve_after_days: 1 in evidence params for stuck_value, got %v", ev2.Params["resolve_after_days"])
	}
	if alertsList2[0].Status != "resolved" {
		t.Fatalf("expected stuck_value to be resolved after 1 day with override 1, got status %s", alertsList2[0].Status)
	}
	_ = sum2
}

// 4. Defect 4: Episode cut changes first_date creating duplicate alert.
// Test TestRerunWithAsOfThenFullDoesNotDuplicate: replay as-of mid-episode, then a full run;
// the episode stays one alert with the final last_date.
func TestRerunWithAsOfThenFullDoesNotDuplicate(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-NoDup", tz, "D1")
	d1 := devs[0]

	// 6 days of stuck values: 2026-03-01 to 2026-03-06
	dates := []string{"2026-03-01", "2026-03-02", "2026-03-03", "2026-03-04", "2026-03-05", "2026-03-06"}
	vals := []float64{10, 10, 10, 10, 10, 10}
	raws := []string{"10", "10", "10", "10", "10", "10"}
	insertDailyReadings(t, ctx, st, d1.ID, tz, dates, vals, raws)

	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	// 1. Replay as-of mid-episode: 2026-03-03
	sum1, err := alerts.Run(ctx, st, plant.ID, alerts.Options{AsOf: "2026-03-03"})
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if sum1.New != 1 {
		t.Fatalf("expected 1 new alert in replay, got %d", sum1.New)
	}

	alertsAfter1, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID})
	if len(alertsAfter1) != 1 {
		t.Fatalf("expected 1 alert stored, got %d", len(alertsAfter1))
	}
	if alertsAfter1[0].LastDate != "2026-03-03" {
		t.Fatalf("expected LastDate 2026-03-03, got %s", alertsAfter1[0].LastDate)
	}

	// 2. Full run without AsOf
	sum2, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	if sum2.New != 0 {
		t.Fatalf("expected 0 new alerts on full run, got %d", sum2.New)
	}

	alertsAfter2, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID})
	if len(alertsAfter2) != 1 {
		t.Fatalf("expected still exactly 1 alert stored, got %d", len(alertsAfter2))
	}
	if alertsAfter2[0].FirstDate != "2026-03-01" {
		t.Errorf("expected FirstDate 2026-03-01, got %s", alertsAfter2[0].FirstDate)
	}
	if alertsAfter2[0].LastDate != "2026-03-06" {
		t.Errorf("expected LastDate 2026-03-06, got %s", alertsAfter2[0].LastDate)
	}
}

// 5. Defect 5: Orphan alerts stay open forever.
// An open or ack alert with no incident in this run stays open forever (rule disabled, or thresholds changed).
// After processing incidents, any open or ack alert of the plant, whose dedupe key did not appear and
// whose rule was evaluated in this run, is set to resolved, keeping its note, and counted in Resolved.
// Skipped rules must not resolve anything. In replay mode (AsOf set), do not apply this step.
// Test TestOrphanAlertResolves and TestReplayDoesNotResolveLaterAlerts.
func TestOrphanAlertResolves(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-Orphan", tz, "D1", "D2", "D3")

	dates := []string{"2026-03-01", "2026-03-02", "2026-03-03"}
	// D1 is 6.0 (< 0.7 * 10 = 7.0), peers are 10.0
	insertDailyReadings(t, ctx, st, devs[1].ID, tz, dates, []float64{10.0, 10.1, 10.2}, []string{"10.0", "10.1", "10.2"})
	insertDailyReadings(t, ctx, st, devs[2].ID, tz, dates, []float64{10.0, 10.1, 10.2}, []string{"10.0", "10.1", "10.2"})
	insertDailyReadings(t, ctx, st, devs[0].ID, tz, dates, []float64{6.0, 6.1, 6.2}, []string{"6.0", "6.1", "6.2"})

	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	// Run 1: fires peer_divergence alert
	sum1, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil || sum1.CountsByRule["peer_divergence"] != 1 {
		t.Fatalf("run 1: %v, counts=%+v", err, sum1.CountsByRule)
	}

	alertsList1, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID, Rule: "peer_divergence"})
	if len(alertsList1) != 1 || alertsList1[0].Status != "open" {
		t.Fatalf("expected 1 open alert, got %+v", alertsList1)
	}

	// Add a note to the alert
	if err := st.AckAlert(ctx, alertsList1[0].ID, "Em investigacao"); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// Change config: ratio override to 0.40 (6.0 >= 0.40 * 10 = 4.0 -> condition no longer holds!)
	if err := st.SetRuleConfig(ctx, store.RuleConfig{
		Rule:       "peer_divergence",
		PlantID:    &plant.ID,
		Enabled:    true,
		ParamsJSON: `{"ratio": 0.40}`,
	}); err != nil {
		t.Fatalf("set config: %v", err)
	}

	// Run 2: peer_divergence is evaluated, but no incidents produced.
	// The existing ack alert should be automatically resolved, keeping its note, and counted in Resolved!
	sum2, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if sum2.Resolved != 1 {
		t.Errorf("expected 1 resolved in sum2, got %d", sum2.Resolved)
	}

	alertsList2, _ := st.ListAlerts(ctx, store.AlertFilter{PlantID: &plant.ID, Rule: "peer_divergence"})
	if len(alertsList2) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alertsList2))
	}
	if alertsList2[0].Status != "resolved" {
		t.Errorf("expected orphan alert to be resolved, got %s", alertsList2[0].Status)
	}
	if alertsList2[0].Note != "Em investigacao" {
		t.Errorf("expected note preserved, got %q", alertsList2[0].Note)
	}
}

func TestReplayDoesNotResolveLaterAlerts(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-ReplayLater", tz, "D1")
	d1 := devs[0]

	// Create an open alert for a later period (2026-03-10..2026-03-15) directly in DB
	devID := d1.ID
	laterAlert := store.Alert{
		PlantID:   plant.ID,
		DeviceID:  &devID,
		Rule:      "stuck_value",
		Severity:  "critical",
		Status:    "open",
		DedupeKey: "stuck_value|d1|2026-03-10",
		Title:     "Leitura travada em D1",
		FirstDate: "2026-03-10",
		LastDate:  "2026-03-15",
		Note:      "Alerta futuro",
	}
	if err := st.SaveAlert(ctx, &laterAlert); err != nil {
		t.Fatalf("save alert: %v", err)
	}

	// Insert readings only for early period 2026-03-01..2026-03-03
	insertDailyReadings(t, ctx, st, d1.ID, tz, []string{"2026-03-01", "2026-03-02", "2026-03-03"},
		[]float64{10.0, 11.0, 12.0}, []string{"10.0", "11.0", "12.0"})
	quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{})

	// Replay as of 2026-03-03
	sum, err := alerts.Run(ctx, st, plant.ID, alerts.Options{AsOf: "2026-03-03"})
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if sum.Resolved != 0 {
		t.Errorf("expected 0 resolved in replay, got %d", sum.Resolved)
	}

	// The later alert MUST still be open!
	got, err := st.GetAlert(ctx, laterAlert.ID)
	if err != nil {
		t.Fatalf("get alert: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("expected later alert to remain open during early replay, got status %s", got.Status)
	}
}

// gradualFixture builds a plant whose devices follow expected for 40 days and
// then produce the given share of expected for `lowDays` days.
func gradualFixture(t *testing.T, shares map[string]float64, lowDays int) (*store.Store, context.Context, int64) {
	t.Helper()
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	names := make([]string, 0, len(shares))
	for name := range shares {
		names = append(names, name)
	}
	sort.Strings(names)
	plant, devs := createTestPlant(t, ctx, st, "Plant-GL", tz, names...)
	var runID int64
	if err := st.DB().QueryRowContext(ctx, `INSERT INTO model_runs (name, version, plant_id, trained_at, active)
		VALUES ('m', 'v1', ?, '2026-01-01T00:00:00Z', 1) RETURNING id`, plant.ID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	start, _ := time.Parse(time.DateOnly, "2026-01-01")
	for _, d := range devs {
		var dates, raws []string
		var vals []float64
		for i := range 40 + lowDays {
			date := start.AddDate(0, 0, i).Format(time.DateOnly)
			share := 1.0
			if i >= 40 {
				share = shares[d.Name]
			}
			v := 40*share + float64(i%5)*0.01
			dates, vals, raws = append(dates, date), append(vals, v), append(raws, fmt.Sprintf("%.2f", v))
			if _, err := st.DB().ExecContext(ctx, `INSERT INTO predictions (device_id, date, model_run_id, kwh_expected, created_at)
				VALUES (?, ?, ?, 40, '2026-01-01T00:00:00Z')`, d.ID, date, runID); err != nil {
				t.Fatal(err)
			}
		}
		insertDailyReadings(t, ctx, st, d.ID, tz, dates, vals, raws)
	}
	if _, err := quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	return st, ctx, plant.ID
}

func TestGradualLossIgnoresPlantWideDrop(t *testing.T) {
	st, ctx, plantID := gradualFixture(t, map[string]float64{"D1": 0.8, "D2": 0.8, "D3": 0.8}, 30)
	sum, err := alerts.Run(ctx, st, plantID, alerts.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := sum.CountsByRule["gradual_loss"]; n != 0 {
		t.Fatalf("a drop shared by every device is not a device loss, got %d alerts", n)
	}
}

func TestGradualLossFiresForOneDeviceAgainstPeers(t *testing.T) {
	st, ctx, plantID := gradualFixture(t, map[string]float64{"D1": 0.8, "D2": 1, "D3": 1}, 30)
	sum, err := alerts.Run(ctx, st, plantID, alerts.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sum.CountsByDevice["D1"]["gradual_loss"]; got != 1 {
		t.Fatalf("D1 got %d alerts, want one incident", got)
	}
	if sum.CountsByRule["gradual_loss"] != 1 {
		t.Fatalf("only D1 should alert, got %+v", sum.CountsByDevice)
	}
}

func TestGradualLossRequiresPersistence(t *testing.T) {
	// 10 low days leave only 3 days with the low value holding the 14-day median.
	st, ctx, plantID := gradualFixture(t, map[string]float64{"D1": 0.8, "D2": 1}, 10)
	sum, err := alerts.Run(ctx, st, plantID, alerts.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := sum.CountsByRule["gradual_loss"]; n != 0 {
		t.Fatalf("a short dip must not alert, got %d", n)
	}
}

func TestDeviceOfflineFiresAndResolvesWhenBackOnline(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-Off", tz, "D1", "D2")
	offline, online := false, true
	lastSeen := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	devs[0].Online, devs[0].LastSeenAt = &offline, &lastSeen
	if _, err := st.UpsertDevice(ctx, devs[0]); err != nil {
		t.Fatal(err)
	}
	devs[1].Online = &online
	if _, err := st.UpsertDevice(ctx, devs[1]); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC) }

	sum, err := alerts.Run(ctx, st, plant.ID, alerts.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if sum.CountsByDevice["D1"]["device_offline"] != 1 || sum.CountsByRule["device_offline"] != 1 {
		t.Fatalf("counts = %+v", sum.CountsByDevice)
	}
	var found store.Alert
	for _, a := range sum.Alerts {
		if a.Rule == "device_offline" {
			found = a
		}
	}
	if found.FirstDate != "2026-06-25" || found.LastDate != "2026-11-04" || found.Severity != "critical" || found.Status != "open" {
		t.Fatalf("alert = %+v", found)
	}

	replay, err := alerts.Run(ctx, st, plant.ID, alerts.Options{Now: now, AsOf: "2026-07-01"})
	if err != nil {
		t.Fatal(err)
	}
	if replay.CountsByRule["device_offline"] != 0 {
		t.Fatalf("replay evaluated the present state: %+v", replay.CountsByRule)
	}

	devs[0].Online = &online
	if _, err := st.UpsertDevice(ctx, devs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := alerts.Run(ctx, st, plant.ID, alerts.Options{Now: now}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetAlert(ctx, found.ID)
	if err != nil || got.Status != "resolved" {
		t.Fatalf("alert = %+v err=%v", got, err)
	}
}

func TestPlantNoGenerationNeedsMoreThanOneDevice(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createTestPlant(t, ctx, st, "Plant-One", tz, "D1")
	insertDailyReadings(t, ctx, st, devs[0].ID, tz,
		[]string{"2026-03-01", "2026-03-02", "2026-03-03", "2026-03-04"},
		[]float64{0, 0, 0, 0}, []string{"0", "0", "0", "0"})
	if _, err := quality.Classify(ctx, st, plant.ID, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	sum, err := alerts.Run(ctx, st, plant.ID, alerts.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.CountsByRule["zero_generation"] != 1 || sum.CountsByRule["plant_no_generation"] != 0 {
		t.Fatalf("a single device must report only zero_generation: %+v", sum.CountsByRule)
	}
}
