package inmet_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"

	"github.com/luigiazoreng/solarguard/internal/connector/weather/inmet"
)

// encodeLatin1 converts a UTF-8 string into Latin-1 (ISO-8859-1) bytes.
func encodeLatin1(t *testing.T, s string) []byte {
	t.Helper()
	enc := charmap.ISO8859_1.NewEncoder()
	b, err := enc.Bytes([]byte(s))
	if err != nil {
		t.Fatalf("failed to encode Latin-1: %v", err)
	}
	return b
}

func TestINMETLatin1AndCommaDecimals(t *testing.T) {
	csvData := `REGIAO:;SE
UF:;SP
ESTACAO:;SÃO PAULO - MIRANTE
CODIGO (WMO):;A701
LATITUDE:;-23,4962888
LONGITUDE:;-46,6200666
ALTITUDE:;785,64
DATA DE FUNDACAO:;25/07/06
Data;Hora UTC;PRECIPITAÇÃO TOTAL, HORÁRIO (mm);RADIACAO GLOBAL (Kj/m²);TEMPERATURA DO AR - BULBO SECO, HORARIA (°C);UMIDADE RELATIVA DO AR, HORARIA (%);
2026/09/20;1200 UTC;,4;3600;25,5;60;
2026/09/20;1300 UTC;0;7200;26,5;50;
`
	b := encodeLatin1(t, csvData)
	station, days, err := inmet.ParseFile(bytes.NewReader(b), "America/Sao_Paulo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if station.Code != "A701" {
		t.Errorf("expected station code A701, got %q", station.Code)
	}
	if station.Name != "SÃO PAULO - MIRANTE" {
		t.Errorf("expected station name 'SÃO PAULO - MIRANTE', got %q", station.Name)
	}
	if station.State != "SP" {
		t.Errorf("expected state SP, got %q", station.State)
	}
	if station.Latitude != -23.4962888 {
		t.Errorf("expected latitude -23.4962888, got %v", station.Latitude)
	}
	if station.Longitude != -46.6200666 {
		t.Errorf("expected longitude -46.6200666, got %v", station.Longitude)
	}

	if len(days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(days))
	}
	d := days[0]
	if d.Date != "2026-09-20" {
		t.Errorf("expected date 2026-09-20, got %q", d.Date)
	}
	// Precipitation: 0.4 + 0 = 0.4
	if d.PrecipitationMM == nil || *d.PrecipitationMM != 0.4 {
		t.Errorf("expected precipitation 0.4, got %v", d.PrecipitationMM)
	}
	// Temperature mean: (25.5 + 26.5) / 2 = 26.0, max: 26.5
	if d.TemperatureMeanC == nil || *d.TemperatureMeanC != 26.0 {
		t.Errorf("expected mean temp 26.0, got %v", d.TemperatureMeanC)
	}
	if d.TemperatureMaxC == nil || *d.TemperatureMaxC != 26.5 {
		t.Errorf("expected max temp 26.5, got %v", d.TemperatureMaxC)
	}
	// Humidity mean: (60 + 50) / 2 = 55.0
	if d.HumidityMeanPct == nil || *d.HumidityMeanPct != 55.0 {
		t.Errorf("expected mean humidity 55.0, got %v", d.HumidityMeanPct)
	}
}

func TestINMETMissingValuesStayMissing(t *testing.T) {
	csvData := `REGIAO;SE
UF;SP
ESTACAO;TEST
CODIGO (WMO);A000
LATITUDE;-20,0
LONGITUDE;-40,0
ALTITUDE;100
DATA DE FUNDACAO;01/01/00
DATA;HORA (UTC);PRECIPITACAO TOTAL, HORARIO (mm);RADIACAO GLOBAL (Kj/m2);TEMPERATURA DO AR - BULBO SECO, HORARIA (C);UMIDADE RELATIVA DO AR, HORARIA (%)
2026-09-20;12:00;-9999;-9999;-9999;-9999
2026-09-20;13:00;;;;
`
	b := encodeLatin1(t, csvData)
	_, days, err := inmet.ParseFile(bytes.NewReader(b), "UTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(days))
	}
	d := days[0]
	if d.PrecipitationMM != nil {
		t.Errorf("expected nil precipitation, got %v", *d.PrecipitationMM)
	}
	if d.TemperatureMeanC != nil {
		t.Errorf("expected nil mean temp, got %v", *d.TemperatureMeanC)
	}
	if d.TemperatureMaxC != nil {
		t.Errorf("expected nil max temp, got %v", *d.TemperatureMaxC)
	}
	if d.HumidityMeanPct != nil {
		t.Errorf("expected nil humidity, got %v", *d.HumidityMeanPct)
	}
	if d.IrradiationKWhM2 != nil {
		t.Errorf("expected nil irradiation, got %v", *d.IrradiationKWhM2)
	}
}

func TestINMETColumnSpellingsAndFormats(t *testing.T) {
	// Tests DATA vs Data, HORA (UTC) vs Hora UTC, Kj/m2 vs Kj/m², C vs °C, YYYY/MM/DD vs YYYY-MM-DD, 1200 UTC vs 12:00, trailing empty column
	csvData := `REGIAO:;SE
UF:;SP
ESTACAO:;TEST
CODIGO (WMO):;A000
LATITUDE:;-20,0
LONGITUDE:;-40,0
ALTITUDE:;100
DATA DE FUNDACAO:;01/01/00
Data;Hora UTC;PRECIPITACAO TOTAL, HORARIO (mm);RADIACAO GLOBAL (Kj/m2);TEMPERATURA DO AR - BULBO SECO, HORARIA (C);UMIDADE RELATIVA DO AR, HORARIA (%);
2026/09/20;1200 UTC;1,5;3600;20,0;70;
2026-09-20;13:00;0,5;7200;22,0;60;
`
	b := encodeLatin1(t, csvData)
	_, days, err := inmet.ParseFile(bytes.NewReader(b), "UTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(days))
	}
	d := days[0]
	if d.Date != "2026-09-20" {
		t.Errorf("expected 2026-09-20, got %s", d.Date)
	}
	if d.PrecipitationMM == nil || *d.PrecipitationMM != 2.0 {
		t.Errorf("expected precipitation 2.0, got %v", d.PrecipitationMM)
	}
}

func TestINMETMissingRequiredColumn(t *testing.T) {
	csvData := `REGIAO:;SE
UF:;SP
ESTACAO:;TEST
CODIGO (WMO):;A000
LATITUDE:;-20,0
LONGITUDE:;-40,0
ALTITUDE:;100
DATA DE FUNDACAO:;01/01/00
DATA;HORA (UTC);PRECIPITACAO TOTAL, HORARIO (mm);TEMPERATURA DO AR - BULBO SECO, HORARIA (C);UMIDADE RELATIVA DO AR, HORARIA (%)
2026-09-20;12:00;1,0;20,0;70
`
	b := encodeLatin1(t, csvData)
	_, _, err := inmet.ParseFile(bytes.NewReader(b), "UTC")
	if err == nil {
		t.Fatal("expected error for missing radiation column, got nil")
	}
	if !strings.Contains(err.Error(), "RADIACAO GLOBAL") {
		t.Errorf("expected error to name RADIACAO GLOBAL, got %q", err.Error())
	}
}

func TestINMETLocalDayGrouping(t *testing.T) {
	// "INMET local-day grouping: with America/Sao_Paulo (UTC-3), the hour 01:00 UTC on the 2nd belongs to local day the 1st. Test this explicitly."
	// 01:00 UTC on 2026-09-02 in America/Sao_Paulo (UTC-3) is 22:00 on 2026-09-01.
	// 04:00 UTC on 2026-09-02 in America/Sao_Paulo (UTC-3) is 01:00 on 2026-09-02.
	csvData := `REGIAO:;SE
UF:;SP
ESTACAO:;TEST
CODIGO (WMO):;A000
LATITUDE:;-23,5
LONGITUDE:;-46,6
ALTITUDE:;700
DATA DE FUNDACAO:;01/01/00
DATA;HORA (UTC);PRECIPITACAO TOTAL, HORARIO (mm);RADIACAO GLOBAL (Kj/m2);TEMPERATURA DO AR - BULBO SECO, HORARIA (C);UMIDADE RELATIVA DO AR, HORARIA (%)
2026-09-01;2300 UTC;1,0;;20,0;80
2026-09-02;0100 UTC;2,0;;21,0;82
2026-09-02;0400 UTC;5,0;;19,0;85
`
	b := encodeLatin1(t, csvData)
	_, days, err := inmet.ParseFile(bytes.NewReader(b), "America/Sao_Paulo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(days) != 2 {
		t.Fatalf("expected 2 local days, got %d", len(days))
	}

	// First local day should be 2026-09-01 (containing 23:00 UTC and 01:00 UTC)
	d1 := days[0]
	if d1.Date != "2026-09-01" {
		t.Errorf("expected first day 2026-09-01, got %q", d1.Date)
	}
	// Precipitation for 2026-09-01 should be 1.0 + 2.0 = 3.0
	if d1.PrecipitationMM == nil || *d1.PrecipitationMM != 3.0 {
		t.Errorf("expected day 1 precipitation 3.0, got %v", d1.PrecipitationMM)
	}
	// Temperature mean for 2026-09-01 should be (20.0 + 21.0)/2 = 20.5
	if d1.TemperatureMeanC == nil || *d1.TemperatureMeanC != 20.5 {
		t.Errorf("expected day 1 mean temp 20.5, got %v", d1.TemperatureMeanC)
	}

	// Second local day should be 2026-09-02 (containing 04:00 UTC)
	d2 := days[1]
	if d2.Date != "2026-09-02" {
		t.Errorf("expected second day 2026-09-02, got %q", d2.Date)
	}
	if d2.PrecipitationMM == nil || *d2.PrecipitationMM != 5.0 {
		t.Errorf("expected day 2 precipitation 5.0, got %v", d2.PrecipitationMM)
	}
}

func TestINMETRadiationSumAndMissingDaytimeHours(t *testing.T) {
	// Radiation sum and division by 3600 (kJ/m² to kWh/m²).
	// Daytime hours are local 07:00 to 17:00 (11 hours).
	// If missing daytime hours <= 2, irradiation is calculated.
	// If missing daytime hours > 2, irradiation is nil.
	// Missing night hours do not make irradiation nil.

	var sb strings.Builder
	sb.WriteString(`REGIAO;SE
UF;SP
ESTACAO;TEST
CODIGO (WMO);A000
LATITUDE;-23,5
LONGITUDE;-46,6
ALTITUDE;700
DATA DE FUNDACAO;01/01/00
DATA;HORA (UTC);PRECIPITACAO TOTAL, HORARIO (mm);RADIACAO GLOBAL (Kj/m2);TEMPERATURA DO AR - BULBO SECO, HORARIA (C);UMIDADE RELATIVA DO AR, HORARIA (%)
`)

	// Day 1: 2026-09-10 (UTC timezone for simplicity of hour mapping)
	// All 11 daytime hours present (07:00 to 17:00), each 3600 kJ/m².
	// Total = 11 * 3600 kJ/m² = 39600 kJ/m² -> 11.0 kWh/m².
	for h := 0; h < 24; h++ {
		rad := ""
		if h >= 7 && h <= 17 {
			rad = "3600"
		}
		sb.WriteString(fmt.Sprintf("2026-09-10;%02d:00;0;%s;20;60\n", h, rad))
	}

	// Day 2: 2026-09-11
	// 2 daytime hours missing (07:00 and 08:00 are -9999). 9 daytime hours present (3600 each).
	// Total = 9 * 3600 = 32400 kJ/m² -> 9.0 kWh/m².
	for h := 0; h < 24; h++ {
		rad := ""
		if h >= 7 && h <= 17 {
			if h == 7 || h == 8 {
				rad = "-9999"
			} else {
				rad = "3600"
			}
		}
		sb.WriteString(fmt.Sprintf("2026-09-11;%02d:00;0;%s;20;60\n", h, rad))
	}

	// Day 3: 2026-09-12
	// 3 daytime hours missing (07:00, 08:00, 09:00 missing). More than 2 daytime hours missing -> nil!
	for h := 0; h < 24; h++ {
		rad := ""
		if h >= 7 && h <= 17 {
			if h == 7 || h == 8 || h == 9 {
				rad = ""
			} else {
				rad = "3600"
			}
		}
		sb.WriteString(fmt.Sprintf("2026-09-12;%02d:00;0;%s;20;60\n", h, rad))
	}

	b := encodeLatin1(t, sb.String())
	_, days, err := inmet.ParseFile(bytes.NewReader(b), "UTC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(days) != 3 {
		t.Fatalf("expected 3 days, got %d", len(days))
	}

	// Day 1
	if days[0].IrradiationKWhM2 == nil || *days[0].IrradiationKWhM2 != 11.0 {
		t.Errorf("day 1: expected 11.0 kWh/m², got %v", days[0].IrradiationKWhM2)
	}

	// Day 2 (2 daytime missing <= 2)
	if days[1].IrradiationKWhM2 == nil || *days[1].IrradiationKWhM2 != 9.0 {
		t.Errorf("day 2: expected 9.0 kWh/m², got %v", days[1].IrradiationKWhM2)
	}

	// Day 3 (3 daytime missing > 2)
	if days[2].IrradiationKWhM2 != nil {
		t.Errorf("day 3: expected nil irradiation for >2 missing daytime hours, got %v", *days[2].IrradiationKWhM2)
	}
}
