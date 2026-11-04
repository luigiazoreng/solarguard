package openmeteo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/connector/weather/openmeteo"
)

func TestMJToKWhConversion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"latitude": -23.5,
			"longitude": -46.6,
			"timezone": "America/Sao_Paulo",
			"daily_units": {
				"time": "iso8601",
				"shortwave_radiation_sum": "MJ/m²"
			},
			"daily": {
				"time": ["2026-09-20", "2026-09-21"],
				"shortwave_radiation_sum": [3.6, 7.2]
			}
		}`))
	}))
	defer ts.Close()

	p := openmeteo.New(
		openmeteo.WithClient(ts.Client()),
		openmeteo.WithArchiveURL(ts.URL),
	)

	q := connector.WeatherQuery{
		Latitude:  -23.5,
		Longitude: -46.6,
		Timezone:  "America/Sao_Paulo",
	}

	days, err := p.Observed(context.Background(), q, "2026-09-20", "2026-09-21")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(days) != 2 {
		t.Fatalf("expected 2 days, got %d", len(days))
	}

	// 3.6 MJ/m² / 3.6 = 1.0 kWh/m²
	if days[0].IrradiationKWhM2 == nil || *days[0].IrradiationKWhM2 != 1.0 {
		t.Errorf("expected 1.0 kWh/m², got %v", days[0].IrradiationKWhM2)
	}
	// 7.2 MJ/m² / 3.6 = 2.0 kWh/m²
	if days[1].IrradiationKWhM2 == nil || *days[1].IrradiationKWhM2 != 2.0 {
		t.Errorf("expected 2.0 kWh/m², got %v", days[1].IrradiationKWhM2)
	}
}

func TestNullBecomesNil(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"latitude": -23.5,
			"longitude": -46.6,
			"timezone": "America/Sao_Paulo",
			"daily_units": {
				"time": "iso8601",
				"shortwave_radiation_sum": "MJ/m²",
				"temperature_2m_mean": "°C",
				"temperature_2m_max": "°C",
				"precipitation_sum": "mm",
				"relative_humidity_2m_mean": "%"
			},
			"daily": {
				"time": ["2026-09-20", "2026-09-21"],
				"shortwave_radiation_sum": [null, 18.0],
				"temperature_2m_mean": [null, 20.5],
				"temperature_2m_max": [25.0, null],
				"precipitation_sum": [null, 0.0],
				"relative_humidity_2m_mean": [null, 75.0]
			}
		}`))
	}))
	defer ts.Close()

	p := openmeteo.New(
		openmeteo.WithClient(ts.Client()),
		openmeteo.WithArchiveURL(ts.URL),
	)

	q := connector.WeatherQuery{
		Latitude:  -23.5,
		Longitude: -46.6,
		Timezone:  "America/Sao_Paulo",
	}

	days, err := p.Observed(context.Background(), q, "2026-09-20", "2026-09-21")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if days[0].IrradiationKWhM2 != nil {
		t.Errorf("expected nil irradiation, got %v", *days[0].IrradiationKWhM2)
	}
	if days[0].TemperatureMeanC != nil {
		t.Errorf("expected nil temperature mean, got %v", *days[0].TemperatureMeanC)
	}
	if days[0].TemperatureMaxC == nil || *days[0].TemperatureMaxC != 25.0 {
		t.Errorf("expected 25.0 max temp, got %v", days[0].TemperatureMaxC)
	}
	if days[0].PrecipitationMM != nil {
		t.Errorf("expected nil precipitation, got %v", *days[0].PrecipitationMM)
	}
	if days[0].HumidityMeanPct != nil {
		t.Errorf("expected nil humidity, got %v", *days[0].HumidityMeanPct)
	}

	// Day 1
	if days[1].IrradiationKWhM2 == nil || *days[1].IrradiationKWhM2 != 5.0 {
		t.Errorf("expected 5.0 kWh/m², got %v", days[1].IrradiationKWhM2)
	}
	if days[1].TemperatureMaxC != nil {
		t.Errorf("expected nil max temp on day 1, got %v", *days[1].TemperatureMaxC)
	}
}

func TestWrongDailyUnitsReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"latitude": -23.5,
			"longitude": -46.6,
			"daily_units": {
				"shortwave_radiation_sum": "kWh/m²"
			},
			"daily": {
				"time": ["2026-09-20"],
				"shortwave_radiation_sum": [5.0]
			}
		}`))
	}))
	defer ts.Close()

	p := openmeteo.New(
		openmeteo.WithClient(ts.Client()),
		openmeteo.WithArchiveURL(ts.URL),
	)

	q := connector.WeatherQuery{Latitude: -23.5, Longitude: -46.6, Timezone: "America/Sao_Paulo"}
	_, err := p.Observed(context.Background(), q, "2026-09-20", "2026-09-20")
	if err == nil {
		t.Fatal("expected error due to unexpected daily_units, got nil")
	}
	if !strings.Contains(err.Error(), "MJ/m2") && !strings.Contains(err.Error(), "MJ/m²") {
		t.Errorf("expected error message mentioning MJ/m2, got %q", err.Error())
	}
}

func TestProviderErrorMessageIncludesStatusAndReason(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": true, "reason": "Latitude must be in range of -90 to 90"}`))
	}))
	defer ts.Close()

	p := openmeteo.New(
		openmeteo.WithClient(ts.Client()),
		openmeteo.WithArchiveURL(ts.URL),
	)

	q := connector.WeatherQuery{Latitude: 150.0, Longitude: -46.6, Timezone: "America/Sao_Paulo"}
	_, err := p.Observed(context.Background(), q, "2026-09-20", "2026-09-20")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "400") {
		t.Errorf("expected error to include status code 400, got %q", errMsg)
	}
	if !strings.Contains(errMsg, "Latitude must be in range of -90 to 90") {
		t.Errorf("expected error to include provider reason, got %q", errMsg)
	}
}

func TestForecastAndUserAgent(t *testing.T) {
	var capturedUserAgent string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"latitude": -23.5,
			"longitude": -46.6,
			"timezone": "America/Sao_Paulo",
			"daily_units": {
				"time": "iso8601",
				"shortwave_radiation_sum": "MJ/m²"
			},
			"daily": {
				"time": ["2026-11-05"],
				"shortwave_radiation_sum": [14.4]
			}
		}`))
	}))
	defer ts.Close()

	p := openmeteo.New(
		openmeteo.WithClient(ts.Client()),
		openmeteo.WithForecastURL(ts.URL),
	)

	q := connector.WeatherQuery{Latitude: -23.5, Longitude: -46.6, Timezone: "America/Sao_Paulo"}
	days, err := p.Forecast(context.Background(), q, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedUserAgent != "solarguard" {
		t.Errorf("expected User-Agent 'solarguard', got %q", capturedUserAgent)
	}
	if len(days) != 1 {
		t.Fatalf("expected 1 day, got %d", len(days))
	}
	if days[0].IrradiationKWhM2 == nil || *days[0].IrradiationKWhM2 != 4.0 {
		t.Errorf("expected 4.0 kWh/m², got %v", days[0].IrradiationKWhM2)
	}
}

func TestMissingRadiationUnitIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"daily":{"time":["2026-01-01"],"shortwave_radiation_sum":[3.6]}}`))
	}))
	defer srv.Close()
	p := openmeteo.New(openmeteo.WithArchiveURL(srv.URL))
	_, err := p.Observed(context.Background(), connector.WeatherQuery{Timezone: "UTC"}, "2026-01-01", "2026-01-01")
	if err == nil {
		t.Fatal("a response with no radiation unit must be rejected")
	}
}
