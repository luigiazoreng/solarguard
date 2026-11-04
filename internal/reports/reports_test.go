package reports_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/reports"
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

func createPlant(t *testing.T, ctx context.Context, st *store.Store, name, tz string, devNames ...string) (domain.Plant, []domain.Device) {
	t.Helper()
	src, err := st.EnsureSource(ctx, domain.Source{Kind: "test", Name: "src-" + name})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	p, err := st.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "p-" + name,
		Name:       name,
		Timezone:   tz,
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}
	var devs []domain.Device
	for _, dName := range devNames {
		d, err := st.UpsertDevice(ctx, domain.Device{
			PlantID:    p.ID,
			ExternalID: "dev-" + name + "-" + dName,
			Name:       dName,
			Kind:       "inverter",
		})
		if err != nil {
			t.Fatalf("upsert device %s: %v", dName, err)
		}
		devs = append(devs, d)
	}
	return p, devs
}

func insertReading(t *testing.T, ctx context.Context, st *store.Store, devID int64, tz, dateStr string, val float64, q domain.Quality, reason string) {
	t.Helper()
	pStart, pEnd, err := domain.LocalDay(dateStr, tz)
	if err != nil {
		t.Fatalf("local day %s: %v", dateStr, err)
	}
	_, err = st.InsertReadings(ctx, []domain.Reading{
		{
			DeviceID:      devID,
			Metric:        domain.MetricEnergyKWh,
			PeriodStart:   pStart,
			PeriodEnd:     pEnd,
			Value:         &val,
			RawValue:      "val",
			Quality:       q,
			QualityReason: reason,
		},
	})
	if err != nil {
		t.Fatalf("insert reading: %v", err)
	}
}

// 1. Deterministic report: same input produces same output with fixed injected time.
func TestReport_Deterministic(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createPlant(t, ctx, st, "Plant1", tz, "D1", "D2")

	// Model run with metrics
	mrun, err := st.UpsertModelRun(ctx, store.ModelRun{
		Name:        "ridge",
		Version:     "v1",
		PlantID:     plant.ID,
		TrainedAt:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		MetricsJSON: `{"walk_forward":{"mae":4.45,"rmse":6.10,"r2":0.85,"wape":0.137}}`,
		Active:      true,
	})
	if err != nil {
		t.Fatalf("upsert model run: %v", err)
	}

	_ = st.UpsertPredictions(ctx, []store.Prediction{
		{DeviceID: devs[0].ID, Date: "2026-03-01", ModelRunID: mrun.ID, KWhExpected: 10.0},
		{DeviceID: devs[1].ID, Date: "2026-03-01", ModelRunID: mrun.ID, KWhExpected: 20.0},
	})

	insertReading(t, ctx, st, devs[0].ID, tz, "2026-03-01", 10.5, domain.QualityOK, "")
	insertReading(t, ctx, st, devs[1].ID, tz, "2026-03-01", 19.8, domain.QualityOK, "")

	// Save an alert in the period
	alert := store.Alert{
		PlantID:      plant.ID,
		DeviceID:     &devs[0].ID,
		Rule:         "under_expected",
		Severity:     "warning",
		Status:       "open",
		DedupeKey:    "test|alert",
		Title:        "Geração abaixo do esperado",
		FirstDate:    "2026-03-01",
		LastDate:     "2026-03-01",
		EvidenceJSON: "{}",
	}
	if err := st.SaveAlert(ctx, &alert); err != nil {
		t.Fatalf("save alert: %v", err)
	}

	req := reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2026-03-01",
		To:      "2026-03-01",
	}

	fixedNow := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	rep1, err := reports.GenerateWithTime(ctx, st, req, fixedNow)
	if err != nil {
		t.Fatalf("GenerateWithTime 1: %v", err)
	}

	rep2, err := reports.GenerateWithTime(ctx, st, req, fixedNow)
	if err != nil {
		t.Fatalf("GenerateWithTime 2: %v", err)
	}

	if rep1.BodyMD != rep2.BodyMD {
		t.Errorf("markdown output is not deterministic!\nREP1:\n%s\nREP2:\n%s", rep1.BodyMD, rep2.BodyMD)
	}
	if rep1.DataJSON != rep2.DataJSON {
		t.Errorf("data_json output is not deterministic!\nREP1:\n%s\nREP2:\n%s", rep1.DataJSON, rep2.DataJSON)
	}

	if rep1.Author != "system" {
		t.Errorf("author = %q, want 'system'", rep1.Author)
	}
	if rep1.Status != "final" {
		t.Errorf("status = %q, want 'final'", rep1.Status)
	}
}

// 2. Escaping: provider text in Markdown is escaped, so a plant named `# x | <b>` cannot break the structure.
func TestReport_Escaping(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createPlant(t, ctx, st, "# x | <b>", tz, "*D1* | <i>")

	insertReading(t, ctx, st, devs[0].ID, tz, "2026-03-01", 10.0, domain.QualityOK, "")

	alert := store.Alert{
		PlantID:   plant.ID,
		DeviceID:  &devs[0].ID,
		Rule:      "stuck_value | <h2>",
		Severity:  "warning",
		Status:    "open",
		DedupeKey: "esc|alert",
		Title:     "Título | <script>alert(1)</script>",
		FirstDate: "2026-03-01",
		LastDate:  "2026-03-01",
	}
	_ = st.SaveAlert(ctx, &alert)

	req := reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2026-03-01",
		To:      "2026-03-01",
	}

	rep, err := reports.Generate(ctx, st, req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Raw unescaped HTML tags should not exist in the Markdown
	if strings.Contains(rep.BodyMD, "<b>") || strings.Contains(rep.BodyMD, "<script>") || strings.Contains(rep.BodyMD, "<h2>") {
		t.Errorf("found unescaped HTML tag in body:\n%s", rep.BodyMD)
	}
	// Raw unescaped pipe inside table text should not corrupt table structure
	// Let's check that pipe character is escaped (\ | or &#124;)
	lines := strings.Split(rep.BodyMD, "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "|") && strings.Contains(l, "# x") {
			// Inside this table row, the plant name pipe must be escaped
			if !strings.Contains(l, `\|`) && !strings.Contains(l, "&#124;") {
				t.Errorf("plant name pipe not escaped in table line: %s", l)
			}
		}
	}
}

// 3. Empty sections say so; it never shows zero as if it were a measurement.
func TestReport_EmptySectionsSaySo(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, _ := createPlant(t, ctx, st, "EmptyPlant", tz, "D1")

	// No readings, no model run, no forecast, no alerts!
	req := reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2026-01-01",
		To:      "2026-01-07",
	}

	rep, err := reports.Generate(ctx, st, req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	md := rep.BodyMD

	// Should state that there is no data, and NOT show "0,00 kWh" as measured generation
	if strings.Contains(md, "0,00 kWh") {
		t.Errorf("empty report showed '0,00 kWh' as measurement!\n%s", md)
	}

	// Must have explanations for empty sections
	if !strings.Contains(md, "Nenhum dado") && !strings.Contains(md, "Sem dados") {
		t.Errorf("expected empty data message in markdown:\n%s", md)
	}
	if !strings.Contains(md, "Nenhum alerta") {
		t.Errorf("expected no-alerts message in markdown:\n%s", md)
	}
}

// 4. Note present in "modelo em uso" section:
// "Métricas calculadas com o clima observado do dia. Elas medem a geração esperada, não uma previsão feita com antecedência."
func TestReport_NotePresent(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createPlant(t, ctx, st, "PlantWithModel", tz, "D1")

	_, err := st.UpsertModelRun(ctx, store.ModelRun{
		Name:        "ridge",
		Version:     "v1",
		PlantID:     plant.ID,
		TrainedAt:   time.Now(),
		MetricsJSON: `{"walk_forward":{"mae":4.45,"rmse":6.10,"r2":0.85,"wape":0.137}}`,
		Active:      true,
	})
	if err != nil {
		t.Fatalf("upsert model run: %v", err)
	}

	insertReading(t, ctx, st, devs[0].ID, tz, "2026-03-01", 10.0, domain.QualityOK, "")

	req := reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2026-03-01",
		To:      "2026-03-01",
	}

	rep, err := reports.Generate(ctx, st, req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	expectedNote := "Métricas calculadas com o clima observado do dia. Elas medem a geração esperada, não uma previsão feita com antecedência."
	if !strings.Contains(rep.BodyMD, expectedNote) {
		t.Errorf("mandatory note missing in markdown:\nwant substring: %s\ngot:\n%s", expectedNote, rep.BodyMD)
	}

	// Also check the mandatory alert line:
	expectedAlertNote := "Um alerta é um indício para análise, não um diagnóstico."
	if !strings.Contains(rep.BodyMD, expectedAlertNote) {
		t.Errorf("mandatory alert note missing in markdown:\nwant substring: %s\ngot:\n%s", expectedAlertNote, rep.BodyMD)
	}
}

// 5. Invalid range rejected with clear Portuguese errors.
func TestReport_InvalidRangeRejected(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, _ := createPlant(t, ctx, st, "Plant1", tz, "D1")

	// From > To
	_, err := reports.Generate(ctx, st, reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2026-03-10",
		To:      "2026-03-01",
	})
	if err == nil {
		t.Fatal("expected error for from > to")
	}

	// Invalid date format
	_, err = reports.Generate(ctx, st, reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "10/03/2026",
		To:      "2026-03-15",
	})
	if err == nil {
		t.Fatal("expected error for invalid date format")
	}

	// Range > 400 days
	_, err = reports.Generate(ctx, st, reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2024-01-01",
		To:      "2026-01-01",
	})
	if err == nil {
		t.Fatal("expected error for range > 400 days")
	}

	// Unknown plant
	_, err = reports.Generate(ctx, st, reports.Request{
		PlantID: 99999,
		Kind:    "period",
		From:    "2026-03-01",
		To:      "2026-03-05",
	})
	if err == nil {
		t.Fatal("expected error for unknown plant ID")
	}
}

// 6. Previsão dos próximos dias: shown if predictions exist AND period ends within 3 days of today.
func TestReport_PrevisaoCondition(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createPlant(t, ctx, st, "PlantPrevisao", tz, "D1")

	mrun, _ := st.UpsertModelRun(ctx, store.ModelRun{
		Name:      "ridge",
		Version:   "v1",
		PlantID:   plant.ID,
		TrainedAt: time.Now(),
		Active:    true,
	})

	// Predictions for 2026-03-02, 2026-03-03
	_ = st.UpsertPredictions(ctx, []store.Prediction{
		{DeviceID: devs[0].ID, Date: "2026-03-02", ModelRunID: mrun.ID, KWhExpected: 15.0},
		{DeviceID: devs[0].ID, Date: "2026-03-03", ModelRunID: mrun.ID, KWhExpected: 16.0},
	})

	// Case A: period ends on 2026-03-01, today is 2026-03-02 (diff = 1 day <= 3 days) -> PREVISÃO SHOWN!
	repA, err := reports.GenerateWithTime(ctx, st, reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2026-02-20",
		To:      "2026-03-01",
	}, time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GenerateWithTime A: %v", err)
	}
	if !strings.Contains(repA.BodyMD, "Previsão dos próximos dias") {
		t.Errorf("expected Previsão section in RepA:\n%s", repA.BodyMD)
	}

	// Case B: period ends on 2025-10-01, today is 2026-03-02 (diff > 3 days) -> PREVISÃO NOT SHOWN (or says not applicable)
	repB, err := reports.GenerateWithTime(ctx, st, reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2025-09-01",
		To:      "2025-10-01",
	}, time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GenerateWithTime B: %v", err)
	}
	if strings.Contains(repB.BodyMD, "2026-03-02") {
		t.Errorf("RepB should not contain upcoming forecast for 2026-03-02:\n%s", repB.BodyMD)
	}
}

// 7. TestReportShowsDetectedOnFromEvidence: "Detectado em" must show detected_on from evidence_json, formatted DD/MM/AAAA.
func TestReportShowsDetectedOnFromEvidence(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createPlant(t, ctx, st, "PlantDetectedOn", tz, "D1")

	alert := store.Alert{
		PlantID:      plant.ID,
		DeviceID:     &devs[0].ID,
		Rule:         "stuck_value",
		Severity:     "critical",
		Status:       "open",
		DedupeKey:    "test|detected_on",
		Title:        "Leitura travada em D1",
		EvidenceJSON: `{"detected_on":"2026-02-18"}`,
		FirstDate:    "2026-02-16",
		LastDate:     "2026-02-20",
		CreatedAt:    time.Date(2026, 11, 4, 15, 10, 0, 0, time.UTC),
	}
	if err := st.SaveAlert(ctx, &alert); err != nil {
		t.Fatalf("save alert: %v", err)
	}

	rep, err := reports.Generate(ctx, st, reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2026-02-01",
		To:      "2026-02-28",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Must show 18/02/2026 in the detected on column
	if !strings.Contains(rep.BodyMD, "18/02/2026") {
		t.Errorf("expected detected_on '18/02/2026' in markdown:\n%s", rep.BodyMD)
	}
	// Must NOT show CreatedAt date in detected on
	if strings.Contains(rep.BodyMD, "04/11/2026") || strings.Contains(rep.BodyMD, "2026-11-04") {
		t.Errorf("markdown contains CreatedAt instead of detected_on:\n%s", rep.BodyMD)
	}
}

// 8. TestReportLabelsAreReadable: Brazilian readability checks:
// - Friendly Portuguese labels for rules, reasons, severities, status
// - No raw rule code appears
// - All dates DD/MM/AAAA
// - Ratios in pt-BR: 0,99 and never 0.99x
// - Title "Relatório de geração: <planta>"
// - No `> [!NOTE]` or `> [!IMPORTANT]`
// - Section numbers sequential, forecast section included with explanation if old.
func TestReportLabelsAreReadable(t *testing.T) {
	st, ctx := setupTestDB(t)
	tz := "America/Sao_Paulo"
	plant, devs := createPlant(t, ctx, st, "PlantReadable", tz, "D1", "D2")

	mrun, _ := st.UpsertModelRun(ctx, store.ModelRun{
		Name:        "ridge",
		Version:     "v1",
		PlantID:     plant.ID,
		TrainedAt:   time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		MetricsJSON: `{"walk_forward":{"pooled":{"mae":4.45,"rmse":6.10,"r2":0.85,"wape":0.137}}}`,
		Active:      true,
	})

	_ = st.UpsertPredictions(ctx, []store.Prediction{
		{DeviceID: devs[0].ID, Date: "2026-03-01", ModelRunID: mrun.ID, KWhExpected: 10.0},
		{DeviceID: devs[1].ID, Date: "2026-03-01", ModelRunID: mrun.ID, KWhExpected: 20.0},
	})

	insertReading(t, ctx, st, devs[0].ID, tz, "2026-03-01", 10.0, domain.QualityOK, "")
	insertReading(t, ctx, st, devs[1].ID, tz, "2026-03-01", 19.8, domain.QualityOK, "")

	// Alert with rule code
	alert := store.Alert{
		PlantID:      plant.ID,
		DeviceID:     &devs[0].ID,
		Rule:         "under_expected",
		Severity:     "warning",
		Status:       "open",
		DedupeKey:    "test|readable",
		Title:        "Geração abaixo do esperado em D1",
		EvidenceJSON: `{"detected_on":"2026-03-01"}`,
		FirstDate:    "2026-03-01",
		LastDate:     "2026-03-01",
	}
	_ = st.SaveAlert(ctx, &alert)

	fixedNow := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC) // > 3 days after 2026-03-01
	rep, err := reports.GenerateWithTime(ctx, st, reports.Request{
		PlantID: plant.ID,
		Kind:    "period",
		From:    "2026-03-01",
		To:      "2026-03-01",
	}, fixedNow)
	if err != nil {
		t.Fatalf("GenerateWithTime: %v", err)
	}

	md := rep.BodyMD

	// Title format: "Relatório de geração: <planta>"
	if !strings.Contains(md, "# Relatório de geração: PlantReadable") {
		t.Errorf("title does not match 'Relatório de geração: PlantReadable':\n%s", md)
	}

	// Must NOT contain raw rule code under_expected
	if strings.Contains(md, "`under_expected`") || strings.Contains(md, "under_expected") {
		t.Errorf("raw rule code 'under_expected' found in markdown:\n%s", md)
	}
	// Must contain friendly rule label "Abaixo do esperado"
	if !strings.Contains(md, "Abaixo do esperado") {
		t.Errorf("expected friendly rule label 'Abaixo do esperado' in markdown:\n%s", md)
	}

	// Severity / status translated
	if !strings.Contains(md, "aviso") {
		t.Errorf("expected translated severity 'aviso' in markdown:\n%s", md)
	}
	if !strings.Contains(md, "aberto") {
		t.Errorf("expected translated status 'aberto' in markdown:\n%s", md)
	}

	// Dates in DD/MM/AAAA
	if !strings.Contains(md, "01/03/2026") {
		t.Errorf("expected date '01/03/2026' in markdown:\n%s", md)
	}
	if strings.Contains(md, "2026-03-01") {
		t.Errorf("raw ISO date '2026-03-01' found in markdown:\n%s", md)
	}

	// Ratio in pt-BR: 0,99 and NEVER 0.99x
	if strings.Contains(md, "x") && strings.Contains(md, "0.") {
		t.Errorf("ratio found with 'x' or dot format:\n%s", md)
	}

	// No github callout syntax > [!NOTE] or > [!IMPORTANT]
	if strings.Contains(md, "> [!NOTE]") || strings.Contains(md, "> [!IMPORTANT]") {
		t.Errorf("markdown contains unsupported callout syntax:\n%s", md)
	}

	// Sequential section numbers: 1, 2, 3, 4, 5, 6
	sections := []string{"## 1.", "## 2.", "## 3.", "## 4.", "## 5.", "## 6."}
	for _, sec := range sections {
		if !strings.Contains(md, sec) {
			t.Errorf("missing section heading %s in markdown:\n%s", sec, md)
		}
	}
}
