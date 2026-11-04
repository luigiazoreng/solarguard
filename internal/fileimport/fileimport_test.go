package fileimport_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/fileimport"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestCellTypesUnitRule verifies:
// - numeric cell becomes value/1000
// - text cell stays with no division
// - comma decimal separator is accepted
// - unparsable text stores Value nil with raw text kept and counted in summary.
func TestCellTypesUnitRule(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	f := excelize.NewFile()
	sheet := "Sheet1"
	f.SetSheetName("Sheet1", sheet)

	// Headers
	headers := []string{"Torre", "Endpoint", "UTCDateTime", "ActualValue (KW)"}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheet, cell, h)
	}

	// Row 2: numeric 54465 -> Wh, should become 54.465 kWh
	f.SetCellValue(sheet, "A2", "INV-1")
	f.SetCellValue(sheet, "B2", "ep1")
	f.SetCellValue(sheet, "C2", "2026-03-01 03:00:00.000")
	f.SetCellValue(sheet, "D2", 54465) // numeric

	// Row 3: text "42.32" -> already kWh, should become 42.32 kWh
	f.SetCellValue(sheet, "A3", "INV-1")
	f.SetCellValue(sheet, "B3", "ep1")
	f.SetCellValue(sheet, "C3", "2026-03-02 03:00:00.000")
	f.SetCellStr(sheet, "D3", "42.32") // text dot

	// Row 4: text "35,50" -> comma decimal, should become 35.50 kWh
	f.SetCellValue(sheet, "A4", "INV-1")
	f.SetCellValue(sheet, "B4", "ep1")
	f.SetCellValue(sheet, "C4", "2026-03-03 03:00:00.000")
	f.SetCellStr(sheet, "D4", "35,50") // text comma

	// Row 5: unparsable text "N/A" -> Value nil, RawValue "N/A"
	f.SetCellValue(sheet, "A5", "INV-1")
	f.SetCellValue(sheet, "B5", "ep1")
	f.SetCellValue(sheet, "C5", "2026-03-04 03:00:00.000")
	f.SetCellStr(sheet, "D5", "N/A") // unparsable text

	filePath := filepath.Join(t.TempDir(), "unit_rules.xlsx")
	if err := f.SaveAs(filePath); err != nil {
		t.Fatalf("save xlsx: %v", err)
	}

	area := 50.0
	eff := 0.20
	summary, err := fileimport.ImportXLSX(ctx, st, fileimport.Options{
		File:           filePath,
		Source:         "test_source",
		PlantID:        "PLANT_TEST",
		PlantName:      "Plant Test",
		Timezone:       "America/Sao_Paulo",
		StampSemantics: fileimport.SemanticsPreviousDay,
		AreaM2:         &area,
		Efficiency:     &eff,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if summary.Rows != 4 {
		t.Errorf("expected 4 rows, got %d", summary.Rows)
	}
	if summary.Inserted != 4 {
		t.Errorf("expected 4 inserted, got %d", summary.Inserted)
	}
	if summary.TextCells != 3 {
		t.Errorf("expected 3 text cells, got %d", summary.TextCells)
	}
	if summary.UnparsableCells != 1 {
		t.Errorf("expected 1 unparsable cell, got %d", summary.UnparsableCells)
	}
	if summary.Devices != 1 {
		t.Errorf("expected 1 device, got %d", summary.Devices)
	}

	plants, err := st.ListPlants(ctx)
	if err != nil || len(plants) != 1 {
		t.Fatalf("expected 1 plant, got %v (err: %v)", plants, err)
	}
	devs, err := st.ListDevices(ctx, plants[0].ID)
	if err != nil || len(devs) != 1 {
		t.Fatalf("expected 1 device, got %v (err: %v)", devs, err)
	}
	if devs[0].PanelAreaM2 == nil || *devs[0].PanelAreaM2 != 50.0 {
		t.Errorf("expected PanelAreaM2 50.0, got %v", devs[0].PanelAreaM2)
	}
	if devs[0].Efficiency == nil || *devs[0].Efficiency != 0.20 {
		t.Errorf("expected Efficiency 0.20, got %v", devs[0].Efficiency)
	}

	readings, err := st.ListReadings(ctx, store.ReadingFilter{
		DeviceID: devs[0].ID,
		Metric:   domain.MetricEnergyKWh,
		From:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("list readings: %v", err)
	}
	if len(readings) != 4 {
		t.Fatalf("expected 4 readings, got %d", len(readings))
	}

	// Reading 1: 54465 -> 54.465
	if readings[0].RawValue != "54465" {
		t.Errorf("reading 0 raw: expected %q, got %q", "54465", readings[0].RawValue)
	}
	if readings[0].Value == nil || *readings[0].Value != 54.465 {
		t.Errorf("reading 0 value: expected 54.465, got %v", readings[0].Value)
	}

	// Reading 2: "42.32" -> 42.32
	if readings[1].RawValue != "42.32" {
		t.Errorf("reading 1 raw: expected %q, got %q", "42.32", readings[1].RawValue)
	}
	if readings[1].Value == nil || *readings[1].Value != 42.32 {
		t.Errorf("reading 1 value: expected 42.32, got %v", readings[1].Value)
	}

	// Reading 3: "35,50" -> 35.5
	if readings[2].RawValue != "35,50" {
		t.Errorf("reading 2 raw: expected %q, got %q", "35,50", readings[2].RawValue)
	}
	if readings[2].Value == nil || *readings[2].Value != 35.5 {
		t.Errorf("reading 2 value: expected 35.5, got %v", readings[2].Value)
	}

	// Reading 4: "N/A" -> Value nil, RawValue "N/A"
	if readings[3].RawValue != "N/A" {
		t.Errorf("reading 3 raw: expected %q, got %q", "N/A", readings[3].RawValue)
	}
	if readings[3].Value != nil {
		t.Errorf("reading 3 value: expected nil, got %v", *readings[3].Value)
	}
}

// TestSmallNumericCellIsAlreadyKWh verifies that:
// - numeric cell 44 (below 1000) is already kWh and gives 44
// - numeric cell 2001 (at or above 1000) is Wh and gives 2.001
func TestSmallNumericCellIsAlreadyKWh(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	f := excelize.NewFile()
	sheet := "Sheet1"
	f.SetCellValue(sheet, "A1", "Torre")
	f.SetCellValue(sheet, "B1", "UTCDateTime")
	f.SetCellValue(sheet, "C1", "ActualValue (KW)")

	// Row 2: numeric 44 (already kWh)
	f.SetCellValue(sheet, "A2", "INV-1")
	f.SetCellValue(sheet, "B2", "2026-05-06 03:00:00.000")
	f.SetCellValue(sheet, "C2", 44)

	// Row 3: numeric 2001 (Wh -> 2.001 kWh)
	f.SetCellValue(sheet, "A3", "INV-1")
	f.SetCellValue(sheet, "B3", "2026-05-07 03:00:00.000")
	f.SetCellValue(sheet, "C3", 2001)

	path := filepath.Join(t.TempDir(), "small_numeric.xlsx")
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	summary, err := fileimport.ImportXLSX(ctx, st, fileimport.Options{
		File:           path,
		Source:         "test_src",
		PlantID:        "PLANT_SMALL",
		PlantName:      "Plant Small",
		Timezone:       "America/Sao_Paulo",
		StampSemantics: fileimport.SemanticsPreviousDay,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	devs, _ := st.ListDevices(ctx, 1)
	readings, _ := st.ListReadings(ctx, store.ReadingFilter{
		DeviceID: devs[0].ID,
		Metric:   domain.MetricEnergyKWh,
		From:     time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
	})
	if len(readings) != 2 {
		t.Fatalf("expected 2 readings, got %d", len(readings))
	}

	// 44 must be 44 kWh (no division)
	if readings[0].Value == nil || *readings[0].Value != 44.0 {
		t.Errorf("expected 44.0 for cell 44, got %v", readings[0].Value)
	}
	if readings[0].RawValue != "44" {
		t.Errorf("expected RawValue 44, got %q", readings[0].RawValue)
	}

	// 2001 must be 2.001 kWh (divided by 1000)
	if readings[1].Value == nil || *readings[1].Value != 2.001 {
		t.Errorf("expected 2.001 for cell 2001, got %v", readings[1].Value)
	}
	if readings[1].RawValue != "2001" {
		t.Errorf("expected RawValue 2001, got %q", readings[1].RawValue)
	}

	if summary.Rows != 2 {
		t.Errorf("expected 2 rows, got %d", summary.Rows)
	}
}

// TestStampSemanticsBounds verifies that previous_day and same_day give correct UTC bounds.
func TestStampSemanticsBounds(t *testing.T) {
	ctx := context.Background()

	makeSpreadsheet := func(t *testing.T, filename string) string {
		f := excelize.NewFile()
		sheet := "Sheet1"
		f.SetCellValue(sheet, "A1", "Torre")
		f.SetCellValue(sheet, "B1", "UTCDateTime")
		f.SetCellValue(sheet, "C1", "ActualValue (KW)")
		// "2026-01-02 03:00:00.000" in America/Sao_Paulo (UTC-3) is 2026-01-02 00:00:00 local
		f.SetCellValue(sheet, "A2", "T1")
		f.SetCellValue(sheet, "B2", "2026-01-02 03:00:00.000")
		f.SetCellValue(sheet, "C2", 10000)
		path := filepath.Join(t.TempDir(), filename)
		if err := f.SaveAs(path); err != nil {
			t.Fatalf("save: %v", err)
		}
		return path
	}

	// 1. previous_day covers local day D-1 (2026-01-01 local day -> 2026-01-01 03:00 to 2026-01-02 03:00 UTC)
	st1 := newTestStore(t)
	path1 := makeSpreadsheet(t, "prev_day.xlsx")
	_, err := fileimport.ImportXLSX(ctx, st1, fileimport.Options{
		File:           path1,
		Source:         "src1",
		PlantID:        "P1",
		PlantName:      "Plant 1",
		Timezone:       "America/Sao_Paulo",
		StampSemantics: fileimport.SemanticsPreviousDay,
	})
	if err != nil {
		t.Fatalf("import previous_day: %v", err)
	}
	devs1, _ := st1.ListDevices(ctx, 1)
	readings1, _ := st1.ListReadings(ctx, store.ReadingFilter{
		DeviceID: devs1[0].ID,
		Metric:   domain.MetricEnergyKWh,
		From:     time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	})
	if len(readings1) != 1 {
		t.Fatalf("expected 1 reading, got %d", len(readings1))
	}
	expectedStartPrev := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	expectedEndPrev := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	if !readings1[0].PeriodStart.Equal(expectedStartPrev) {
		t.Errorf("previous_day PeriodStart: expected %v, got %v", expectedStartPrev, readings1[0].PeriodStart)
	}
	if !readings1[0].PeriodEnd.Equal(expectedEndPrev) {
		t.Errorf("previous_day PeriodEnd: expected %v, got %v", expectedEndPrev, readings1[0].PeriodEnd)
	}

	// 2. same_day covers local day D (2026-01-02 local day -> 2026-01-02 03:00 to 2026-01-03 03:00 UTC)
	st2 := newTestStore(t)
	path2 := makeSpreadsheet(t, "same_day.xlsx")
	_, err = fileimport.ImportXLSX(ctx, st2, fileimport.Options{
		File:           path2,
		Source:         "src2",
		PlantID:        "P2",
		PlantName:      "Plant 2",
		Timezone:       "America/Sao_Paulo",
		StampSemantics: fileimport.SemanticsSameDay,
	})
	if err != nil {
		t.Fatalf("import same_day: %v", err)
	}
	devs2, _ := st2.ListDevices(ctx, 1)
	readings2, _ := st2.ListReadings(ctx, store.ReadingFilter{
		DeviceID: devs2[0].ID,
		Metric:   domain.MetricEnergyKWh,
		From:     time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	})
	if len(readings2) != 1 {
		t.Fatalf("expected 1 reading, got %d", len(readings2))
	}
	expectedStartSame := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	expectedEndSame := time.Date(2026, 1, 3, 3, 0, 0, 0, time.UTC)
	if !readings2[0].PeriodStart.Equal(expectedStartSame) {
		t.Errorf("same_day PeriodStart: expected %v, got %v", expectedStartSame, readings2[0].PeriodStart)
	}
	if !readings2[0].PeriodEnd.Equal(expectedEndSame) {
		t.Errorf("same_day PeriodEnd: expected %v, got %v", expectedEndSame, readings2[0].PeriodEnd)
	}
}

// TestReimportSafe verifies that re-importing inserts 0 rows and leaves values intact.
func TestReimportSafe(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	f := excelize.NewFile()
	sheet := "Sheet1"
	f.SetCellValue(sheet, "A1", "Torre")
	f.SetCellValue(sheet, "B1", "UTCDateTime")
	f.SetCellValue(sheet, "C1", "ActualValue (KW)")
	f.SetCellValue(sheet, "A2", "DEV-1")
	f.SetCellValue(sheet, "B2", "2026-05-10 03:00:00.000")
	f.SetCellValue(sheet, "C2", 20000)

	path := filepath.Join(t.TempDir(), "reimport.xlsx")
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	opts := fileimport.Options{
		File:           path,
		Source:         "src",
		PlantID:        "P1",
		PlantName:      "Plant 1",
		Timezone:       "America/Sao_Paulo",
		StampSemantics: fileimport.SemanticsPreviousDay,
	}

	s1, err := fileimport.ImportXLSX(ctx, st, opts)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if s1.Inserted != 1 {
		t.Errorf("first import: expected 1 inserted, got %d", s1.Inserted)
	}

	s2, err := fileimport.ImportXLSX(ctx, st, opts)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if s2.Rows != 1 {
		t.Errorf("second import: expected 1 row, got %d", s2.Rows)
	}
	if s2.Inserted != 0 {
		t.Errorf("second import: expected 0 inserted, got %d", s2.Inserted)
	}

	devs, _ := st.ListDevices(ctx, 1)
	readings, _ := st.ListReadings(ctx, store.ReadingFilter{
		DeviceID: devs[0].ID,
		Metric:   domain.MetricEnergyKWh,
		From:     time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
	})
	if len(readings) != 1 {
		t.Fatalf("expected 1 reading, got %d", len(readings))
	}
	if readings[0].Value == nil || *readings[0].Value != 20.0 {
		t.Errorf("expected value 20.0, got %v", readings[0].Value)
	}
}

// TestMultiPlantAndDeviceCount verifies:
// - two plants imported from two files stay separate
// - a plant with 2 devices or 5 devices works properly.
func TestMultiPlantAndDeviceCount(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// Plant A: 2 devices
	fA := excelize.NewFile()
	fA.SetCellValue("Sheet1", "A1", "Torre")
	fA.SetCellValue("Sheet1", "B1", "UTCDateTime")
	fA.SetCellValue("Sheet1", "C1", "ActualValue (KW)")
	fA.SetCellValue("Sheet1", "A2", "PA-D1")
	fA.SetCellValue("Sheet1", "B2", "2026-06-01 03:00:00.000")
	fA.SetCellValue("Sheet1", "C2", 10000)
	fA.SetCellValue("Sheet1", "A3", "PA-D2")
	fA.SetCellValue("Sheet1", "B3", "2026-06-01 03:00:00.000")
	fA.SetCellValue("Sheet1", "C3", 12000)

	pathA := filepath.Join(t.TempDir(), "plantA.xlsx")
	fA.SaveAs(pathA)

	// Plant B: 5 devices
	fB := excelize.NewFile()
	fB.SetCellValue("Sheet1", "A1", "Torre")
	fB.SetCellValue("Sheet1", "B1", "UTCDateTime")
	fB.SetCellValue("Sheet1", "C1", "ActualValue (KW)")
	for i := 1; i <= 5; i++ {
		row := i + 1
		cellA, _ := excelize.CoordinatesToCellName(1, row)
		cellB, _ := excelize.CoordinatesToCellName(2, row)
		cellC, _ := excelize.CoordinatesToCellName(3, row)
		fB.SetCellValue("Sheet1", cellA, "PB-D"+string(rune('0'+i)))
		fB.SetCellValue("Sheet1", cellB, "2026-06-01 03:00:00.000")
		fB.SetCellValue("Sheet1", cellC, float64(i*5000))
	}
	pathB := filepath.Join(t.TempDir(), "plantB.xlsx")
	fB.SaveAs(pathB)

	sumA, err := fileimport.ImportXLSX(ctx, st, fileimport.Options{
		File:           pathA,
		Source:         "source_a",
		PlantID:        "EXT-A",
		PlantName:      "Plant Alpha",
		Timezone:       "America/Sao_Paulo",
		StampSemantics: fileimport.SemanticsPreviousDay,
	})
	if err != nil {
		t.Fatalf("import plant A: %v", err)
	}
	if sumA.Devices != 2 || sumA.Rows != 2 {
		t.Errorf("plant A: expected 2 devices, 2 rows; got devices=%d rows=%d", sumA.Devices, sumA.Rows)
	}

	sumB, err := fileimport.ImportXLSX(ctx, st, fileimport.Options{
		File:           pathB,
		Source:         "source_b",
		PlantID:        "EXT-B",
		PlantName:      "Plant Beta",
		Timezone:       "America/Sao_Paulo",
		StampSemantics: fileimport.SemanticsPreviousDay,
	})
	if err != nil {
		t.Fatalf("import plant B: %v", err)
	}
	if sumB.Devices != 5 || sumB.Rows != 5 {
		t.Errorf("plant B: expected 5 devices, 5 rows; got devices=%d rows=%d", sumB.Devices, sumB.Rows)
	}

	plants, err := st.ListPlants(ctx)
	if err != nil {
		t.Fatalf("list plants: %v", err)
	}
	if len(plants) != 2 {
		t.Fatalf("expected 2 plants, got %d", len(plants))
	}
	if plants[0].ExternalID == plants[1].ExternalID {
		t.Errorf("plants should have distinct external IDs: %s vs %s", plants[0].ExternalID, plants[1].ExternalID)
	}

	devsA, err := st.ListDevices(ctx, plants[0].ID)
	if err != nil || len(devsA) != 2 {
		t.Errorf("expected 2 devices for plant A, got %d", len(devsA))
	}
	devsB, err := st.ListDevices(ctx, plants[1].ID)
	if err != nil || len(devsB) != 5 {
		t.Errorf("expected 5 devices for plant B, got %d", len(devsB))
	}
}

// TestMissingColumnFails verifies that missing required column returns clear error naming it.
func TestMissingColumnFails(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	f := excelize.NewFile()
	// Missing "ActualValue (KW)"
	f.SetCellValue("Sheet1", "A1", "Torre")
	f.SetCellValue("Sheet1", "B1", "UTCDateTime")

	path := filepath.Join(t.TempDir(), "missing_col.xlsx")
	f.SaveAs(path)

	_, err := fileimport.ImportXLSX(ctx, st, fileimport.Options{
		File:           path,
		Source:         "src",
		PlantID:        "P_ERR",
		PlantName:      "Plant Err",
		Timezone:       "America/Sao_Paulo",
		StampSemantics: fileimport.SemanticsPreviousDay,
	})
	if err == nil {
		t.Fatal("expected error for missing column, got nil")
	}
	if expected := `missing required column "ActualValue (KW)"`; err.Error() != expected && !contains(err.Error(), "ActualValue (KW)") {
		t.Errorf("expected error message to name missing column, got: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) > 0 && searchSubstr(s, substr))
}

func searchSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
