package web

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ReportView is a report body split into sections for display. The Markdown
// itself is untouched: the split happens only when rendering.
type ReportView struct {
	Preamble template.HTML
	Sections []ReportSection
}

// ReportSection is one "## " block of the body.
type ReportSection struct {
	ID        string
	Number    int // leading "N." of the heading; 0 when it has none
	TitleHTML template.HTML
	KPIs      []ReportKPI
	HTML      template.HTML
	// Forecast is set only on the section titled "N. Previsão ..." (N = 5)
	// and only when the report has forecast data to draw.
	Forecast *ForecastChart
}

// ReportKPI is a row of the section 1 "Métrica | Valor" table shown as a card.
type ReportKPI struct {
	Label template.HTML
	Value string
	Unit  string
	Sub   template.HTML
	Small bool // text value rather than a number
}

// ForecastChart is the daily plant forecast stored in a report's data_json.
// Lower and Upper hold nil on days where not every device has a 90% band.
type ForecastChart struct {
	Dates    []string   `json:"dates"`
	Expected []float64  `json:"expected"`
	Lower    []*float64 `json:"lower"`
	Upper    []*float64 `json:"upper"`
	HasBand  bool       `json:"has_band"`
}

var (
	sectionNumberRegex = regexp.MustCompile(`^(\d+)\.\s*(.*)$`)
	pairedValueRegex   = regexp.MustCompile(`^(.+) medidos vs (.+) esperados \(desvio: (.+), (.+)\)$`)
	valueUnitRegex     = regexp.MustCompile(`^(.+?)\s+(kWh|MWh|Wh|kW)$`)
)

// forecastSectionNumber is where the report generator puts the forecast.
const forecastSectionNumber = 5

// BuildReportView splits a Markdown report body into sections. Bodies that do
// not follow the numbered structure still render: everything before the first
// "## " heading, or the whole body when there is none, goes in Preamble.
func BuildReportView(body string, forecast *ForecastChart) ReportView {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")

	var preamble []string
	type rawSection struct {
		heading string
		lines   []string
	}
	var raws []rawSection
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "## ") {
			raws = append(raws, rawSection{heading: strings.TrimSpace(t[3:])})
			continue
		}
		if len(raws) == 0 {
			preamble = append(preamble, line)
		} else {
			raws[len(raws)-1].lines = append(raws[len(raws)-1].lines, line)
		}
	}

	view := ReportView{Preamble: RenderMarkdown(strings.Join(preamble, "\n"))}
	for idx, raw := range raws {
		sec := ReportSection{ID: fmt.Sprintf("secao-%d", idx+1)}
		title := raw.heading
		if m := sectionNumberRegex.FindStringSubmatch(title); m != nil {
			sec.Number, _ = strconv.Atoi(m[1])
			title = m[2]
		}
		sec.TitleHTML = template.HTML(renderInlines(html.EscapeString(title)))

		bodyLines := raw.lines
		if sec.Number == 1 {
			var kpis []ReportKPI
			kpis, bodyLines = extractKPIs(bodyLines)
			sec.KPIs = kpis
		}
		sec.HTML = RenderMarkdown(strings.Join(bodyLines, "\n"))

		if forecast != nil && sec.Number == forecastSectionNumber &&
			strings.Contains(strings.ToLower(title), "previs") {
			sec.Forecast = forecast
		}
		view.Sections = append(view.Sections, sec)
	}
	return view
}

// extractKPIs removes a "Métrica | Valor" table from the lines and returns its
// rows as cards. Without such a table the lines come back as they were.
func extractKPIs(lines []string) ([]ReportKPI, []string) {
	for i := 0; i+1 < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.Contains(t, "|") || !isTableSeparator(lines[i+1]) {
			continue
		}
		headers := splitTableCells(t)
		if len(headers) != 2 || !strings.EqualFold(headers[0], "Métrica") {
			return nil, lines
		}
		j := i + 2
		var kpis []ReportKPI
		for j < len(lines) && strings.Contains(lines[j], "|") && strings.TrimSpace(lines[j]) != "" {
			cells := splitTableCells(lines[j])
			if len(cells) == 2 {
				kpis = append(kpis, newKPI(cells[0], cells[1]))
			}
			j++
		}
		if len(kpis) == 0 {
			return nil, lines
		}
		rest := append(append([]string{}, lines[:i]...), lines[j:]...)
		return kpis, rest
	}
	return nil, lines
}

func newKPI(label, value string) ReportKPI {
	k := ReportKPI{Label: template.HTML(renderInlines(html.EscapeString(label)))}
	if m := pairedValueRegex.FindStringSubmatch(value); m != nil {
		k.Value = m[4]
		k.Sub = template.HTML(html.EscapeString(fmt.Sprintf("%s medidos vs %s esperados (desvio: %s)", m[1], m[2], m[3])))
		return k
	}
	if m := valueUnitRegex.FindStringSubmatch(value); m != nil {
		k.Value, k.Unit = m[1], m[2]
		return k
	}
	k.Value = value
	k.Small = !numericCellRegex.MatchString(value)
	return k
}

// ForecastFromDataJSON builds the section 5 chart from a report's stored
// data_json, so the chart is the snapshot taken when the report was made.
// It returns nil for empty, malformed or old data_json, and when there are no
// valid forecast days.
//
// The "previsao" entries are store.Prediction values marshaled without JSON
// tags, hence the Go field names. Per-device values are summed per day, and the
// 90% band is combined the same way as on the plant page (AggregatePredictions).
// A day gets a band only when every device prediction of that day has both
// limits.
func ForecastFromDataJSON(dataJSON string) *ForecastChart {
	if strings.TrimSpace(dataJSON) == "" {
		return nil
	}
	var parsed struct {
		Previsao []struct {
			Date        string
			KWhExpected *float64
			KWhLower    *float64
			KWhUpper    *float64
		} `json:"previsao"`
	}
	if err := json.Unmarshal([]byte(dataJSON), &parsed); err != nil {
		return nil
	}

	byDate := make(map[string][]PredictionInput)
	complete := make(map[string]bool)
	for _, p := range parsed.Previsao {
		if p.KWhExpected == nil {
			continue
		}
		if _, err := time.Parse(time.DateOnly, p.Date); err != nil {
			continue
		}
		if _, seen := complete[p.Date]; !seen {
			complete[p.Date] = true
		}
		if p.KWhLower == nil || p.KWhUpper == nil {
			complete[p.Date] = false
		}
		byDate[p.Date] = append(byDate[p.Date], PredictionInput{Expected: *p.KWhExpected, Lower: p.KWhLower, Upper: p.KWhUpper})
	}
	if len(byDate) == 0 {
		return nil
	}

	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	fc := &ForecastChart{}
	for _, d := range dates {
		total, lo, up := AggregatePredictions(byDate[d])
		fc.Dates = append(fc.Dates, d)
		fc.Expected = append(fc.Expected, total)
		if complete[d] {
			fc.Lower = append(fc.Lower, &lo)
			fc.Upper = append(fc.Upper, &up)
			fc.HasBand = true
		} else {
			fc.Lower = append(fc.Lower, nil)
			fc.Upper = append(fc.Upper, nil)
		}
	}
	return fc
}
