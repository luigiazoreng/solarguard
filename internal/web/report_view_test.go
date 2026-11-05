package web_test

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/web"
)

// Same shape the report generator stores: store.Prediction marshaled with Go
// field names, one entry per device per day.
const forecastDataJSON = `{"previsao":[
 {"ID":1,"DeviceID":1,"Date":"2026-03-03","ModelRunID":1,"KWhExpected":10,"KWhLower":8,"KWhUpper":13},
 {"ID":2,"DeviceID":2,"Date":"2026-03-03","ModelRunID":1,"KWhExpected":20,"KWhLower":16,"KWhUpper":24},
 {"ID":3,"DeviceID":1,"Date":"2026-03-02","ModelRunID":1,"KWhExpected":9,"KWhLower":7,"KWhUpper":12},
 {"ID":4,"DeviceID":2,"Date":"2026-03-02","ModelRunID":1,"KWhExpected":18,"KWhLower":null,"KWhUpper":null}
]}`

func TestForecastFromDataJSON_AggregatesPerDay(t *testing.T) {
	fc := web.ForecastFromDataJSON(forecastDataJSON)
	if fc == nil {
		t.Fatal("expected a forecast")
	}
	if got := strings.Join(fc.Dates, ","); got != "2026-03-02,2026-03-03" {
		t.Fatalf("dates sorted and unique: %s", got)
	}
	if fc.Expected[0] != 27 || fc.Expected[1] != 30 {
		t.Errorf("expected totals 27 and 30, got %v", fc.Expected)
	}
	// 03-02: one device has no band, so no band is invented for the day.
	if fc.Lower[0] != nil || fc.Upper[0] != nil {
		t.Errorf("day with an incomplete band must have none, got %v %v", fc.Lower[0], fc.Upper[0])
	}
	// 03-03: sqrt(3^2+4^2)=5 around 30.
	if fc.Lower[1] == nil || fc.Upper[1] == nil || math.Abs(*fc.Lower[1]-25) > 1e-9 || math.Abs(*fc.Upper[1]-35) > 1e-9 {
		t.Errorf("band for 03-03 should be 25..35")
	}
	if !fc.HasBand {
		t.Error("HasBand should be true")
	}
}

func TestForecastFromDataJSON_NoBoundsMeansNoBand(t *testing.T) {
	fc := web.ForecastFromDataJSON(`{"previsao":[{"Date":"2026-03-02","KWhExpected":5}]}`)
	if fc == nil || fc.HasBand || fc.Lower[0] != nil {
		t.Fatalf("expected forecast without band, got %+v", fc)
	}
}

func TestForecastFromDataJSON_AbsentOrBroken(t *testing.T) {
	for name, in := range map[string]string{
		"empty":            "",
		"malformed":        `{"previsao": [`,
		"not an object":    `[1,2,3]`,
		"old, no field":    `{"resumo":{"medida_kwh":1}}`,
		"null forecast":    `{"previsao":null}`,
		"empty forecast":   `{"previsao":[]}`,
		"wrong type":       `{"previsao":"x"}`,
		"bad dates":        `{"previsao":[{"Date":"amanhã","KWhExpected":5}]}`,
		"missing expected": `{"previsao":[{"Date":"2026-03-02"}]}`,
	} {
		if fc := web.ForecastFromDataJSON(in); fc != nil {
			t.Errorf("%s: expected nil, got %+v", name, fc)
		}
	}
}

const sampleBody = `# Relatório

- **Planta:** A

## 1. Resumo Executivo

| Métrica | Valor |
|---|---|
| Geração medida | 1.234,50 kWh |
| Medida x esperada (3 dias-dispositivo com ambos) | 10,00 kWh medidos vs 12,00 kWh esperados (desvio: -2,00 kWh, -16,7%) |

> Nota de estimativa.

## 3. Alertas do Período

| Título | Primeira data | Severidade | Status |
|---|---|---|---|
| Queda | 01/03/2026 | crítico | aberto |
| Aviso | 01/03/2026 | aviso | resolvido |

## 5. Previsão dos próximos dias

| Data | Geração esperada prevista |
|---|---|
| 02/03/2026 | 27,00 kWh |
`

func TestBuildReportView_Structure(t *testing.T) {
	fc := web.ForecastFromDataJSON(forecastDataJSON)
	v := web.BuildReportView(sampleBody, fc)
	if len(v.Sections) != 3 {
		t.Fatalf("want 3 sections, got %d", len(v.Sections))
	}
	if len(v.Sections[0].KPIs) != 2 {
		t.Fatalf("section 1 should yield 2 KPI cards, got %d", len(v.Sections[0].KPIs))
	}
	k := v.Sections[0].KPIs[0]
	if k.Value != "1.234,50" || k.Unit != "kWh" {
		t.Errorf("KPI value/unit: %q %q", k.Value, k.Unit)
	}
	if v.Sections[0].KPIs[1].Value != "-16,7%" || !strings.Contains(string(v.Sections[0].KPIs[1].Sub), "10,00 kWh medidos vs 12,00 kWh esperados") {
		t.Errorf("paired KPI: %+v", v.Sections[0].KPIs[1])
	}
	if strings.Contains(string(v.Sections[0].HTML), "<table") {
		t.Error("KPI table should not be rendered twice")
	}
	if !strings.Contains(string(v.Sections[0].HTML), "Nota de estimativa") {
		t.Error("text after the KPI table must be kept")
	}
	if v.Sections[0].Forecast != nil || v.Sections[1].Forecast != nil {
		t.Error("only section 5 gets the chart")
	}
	if v.Sections[2].Forecast == nil {
		t.Error("section 5 should carry the forecast")
	}
	alerts := string(v.Sections[1].HTML)
	for _, want := range []string{`badge badge-critical">crítico`, `badge badge-warning">aviso`, `badge badge-warning">aberto`, `badge badge-success">resolvido`, `class="row-critical"`} {
		if !strings.Contains(alerts, want) {
			t.Errorf("alerts table missing %q:\n%s", want, alerts)
		}
	}
	forecastTable := string(v.Sections[2].HTML)
	if !strings.Contains(forecastTable, `<th class="num">Geração esperada prevista</th>`) || !strings.Contains(forecastTable, `<td class="num">27,00 kWh</td>`) {
		t.Errorf("numbers should be right-aligned:\n%s", forecastTable)
	}
	if !strings.Contains(forecastTable, "<td>02/03/2026</td>") {
		t.Errorf("dates stay left-aligned:\n%s", forecastTable)
	}
}

func TestBuildReportView_ForecastOnlyOnForecastSection(t *testing.T) {
	fc := web.ForecastFromDataJSON(forecastDataJSON)
	v := web.BuildReportView("## 5. Recomendações\n\ntexto\n", fc)
	if len(v.Sections) != 1 || v.Sections[0].Forecast != nil {
		t.Errorf("a non-forecast section 5 must not get the chart: %+v", v.Sections)
	}
	v = web.BuildReportView(strings.Replace(sampleBody, "## 5.", "## 4.", 1), fc)
	if v.Sections[2].Forecast != nil {
		t.Error("forecast heading numbered 4 must not get the chart")
	}
	v = web.BuildReportView(sampleBody, nil)
	if v.Sections[2].Forecast != nil {
		t.Error("no forecast data, no chart")
	}
}

func TestBuildReportView_FreeFormBodiesAndEscaping(t *testing.T) {
	v := web.BuildReportView("Texto livre <script>alert(1)</script>\n\n### Sub\n", nil)
	if len(v.Sections) != 0 || !strings.Contains(string(v.Preamble), "&lt;script&gt;") || strings.Contains(string(v.Preamble), "<script>") {
		t.Errorf("free-form body: %+v", v)
	}
	v = web.BuildReportView("## Título <b>x</b>\n\n| Métrica | Valor |\n|---|---|\n| <i>a</i> | <u>b</u> |\n", nil)
	if len(v.Sections) != 1 {
		t.Fatal("want one section")
	}
	s := v.Sections[0]
	if s.Number != 0 {
		t.Errorf("unnumbered heading, got %d", s.Number)
	}
	all := string(s.TitleHTML) + string(s.HTML)
	if strings.Contains(all, "<b>") || strings.Contains(all, "<i>") || strings.Contains(all, "<u>") {
		t.Errorf("raw HTML leaked: %s", all)
	}
	if len(s.KPIs) != 0 {
		t.Error("KPI cards are only for section 1")
	}
	// Empty body must not panic.
	web.BuildReportView("", nil)
}

func fetchReportPage(t *testing.T, st *store.Store, rep store.Report) (int, string) {
	t.Helper()
	srv, err := web.NewServer(web.Config{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	ts := ownerServer(t, st, srv)
	defer ts.Close()
	saved, err := st.InsertReport(context.Background(), rep)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(fmt.Sprintf("%s/reports/%d", ts.URL, saved.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestReportDetail_ChartData(t *testing.T) {
	st := newWebTestStore(t)
	plant, _, _ := seedPlantWithDevices(t, st, "CH_A", "Usina A")
	base := store.Report{
		PlantID: &plant.ID, Kind: "period", PeriodStart: "2026-02-20", PeriodEnd: "2026-03-01",
		Status: "final", Author: "system", Title: "Relatório A", BodyMD: sampleBody,
	}

	t.Run("with forecast", func(t *testing.T) {
		rep := base
		rep.DataJSON = forecastDataJSON
		code, body := fetchReportPage(t, st, rep)
		if code != 200 {
			t.Fatalf("status %d", code)
		}
		for _, want := range []string{`id="report-forecast-chart"`, `<script type="application/json" id="report-forecast-data">`, `"has_band":true`, `class="report-section card" id="secao-3"`, `class="card kpi-card"`} {
			if !strings.Contains(body, want) {
				t.Errorf("page missing %q", want)
			}
		}
		// The chart sits in section 5, after its heading and before its table.
		sec := body[strings.Index(body, `id="secao-3"`):]
		if strings.Index(sec, "report-forecast-chart") > strings.Index(sec, "<table") {
			t.Error("chart should come before the forecast table")
		}
	})

	for name, dj := range map[string]string{
		"no forecast":  `{"previsao":null}`,
		"old data":     `{"resumo":{}}`,
		"malformed":    `{oops`,
		"empty":        ``,
		"empty before": `{"previsao":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			rep := base
			rep.DataJSON = dj
			code, body := fetchReportPage(t, st, rep)
			if code != 200 {
				t.Fatalf("status %d", code)
			}
			if strings.Contains(body, "report-forecast-chart") || strings.Contains(body, "report-forecast-data") {
				t.Error("no chart expected")
			}
			if !strings.Contains(body, "Previsão dos próximos dias") && !strings.Contains(body, "Previsão") {
				t.Error("section 5 should still render")
			}
		})
	}

	t.Run("ai body without structure", func(t *testing.T) {
		rep := base
		rep.Author = "ai"
		rep.Status = "draft"
		rep.BodyMD = "Análise livre <script>alert(1)</script>"
		rep.DataJSON = forecastDataJSON
		code, body := fetchReportPage(t, st, rep)
		if code != 200 || strings.Contains(body, "report-forecast-chart") || strings.Contains(body, "<script>alert(1)") {
			t.Errorf("status %d, body unsafe or chart present", code)
		}
		if !strings.Contains(body, "Análise livre") {
			t.Error("body text missing")
		}
	})
}

func TestReportsList_ShowsKindAndPlant(t *testing.T) {
	st := newWebTestStore(t)
	plant, _, _ := seedPlantWithDevices(t, st, "LS_A", "Usina A")
	if _, err := st.InsertReport(context.Background(), store.Report{
		PlantID: &plant.ID, Kind: "weekly", PeriodStart: "2026-02-20", PeriodEnd: "2026-02-27",
		Status: "draft", Author: "ai", Title: "Semana", BodyMD: "x",
	}); err != nil {
		t.Fatal(err)
	}
	srv, _ := web.NewServer(web.Config{Store: st})
	ts := ownerServer(t, st, srv)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/reports")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	for _, want := range []string{"Semanal", `class="cell-sub">Usina A`, `class="row-draft"`, "20/02/2026 a 27/02/2026"} {
		if !strings.Contains(body, want) {
			t.Errorf("list missing %q", want)
		}
	}
}
