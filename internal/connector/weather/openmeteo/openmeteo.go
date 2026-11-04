// Package openmeteo implements the connector.WeatherProvider interface using Open-Meteo's API.
package openmeteo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
)

const (
	// DefaultArchiveURL is the default Open-Meteo historical archive API.
	DefaultArchiveURL = "https://archive-api.open-meteo.com/v1/archive"
	// DefaultForecastURL is the default Open-Meteo forecast API.
	DefaultForecastURL = "https://api.open-meteo.com/v1/forecast"
	// DefaultDailyVars are the requested daily weather variables.
	DefaultDailyVars = "shortwave_radiation_sum,temperature_2m_mean,temperature_2m_max,precipitation_sum,relative_humidity_2m_mean"
	// UserAgent is the HTTP User-Agent sent with requests.
	UserAgent = "solarguard"
	// ProviderName is the name of this weather provider.
	ProviderName = "open-meteo"
)

// Provider queries Open-Meteo for observed and forecast weather.
type Provider struct {
	client      *http.Client
	archiveURL  string
	forecastURL string
}

// Option configures a Provider.
type Option func(*Provider)

// WithClient sets the HTTP client.
func WithClient(client *http.Client) Option {
	return func(p *Provider) {
		if client != nil {
			p.client = client
		}
	}
}

// WithArchiveURL overrides the default historical archive API endpoint.
func WithArchiveURL(endpoint string) Option {
	return func(p *Provider) {
		p.archiveURL = endpoint
	}
}

// WithForecastURL overrides the default forecast API endpoint.
func WithForecastURL(endpoint string) Option {
	return func(p *Provider) {
		p.forecastURL = endpoint
	}
}

// New creates a new Open-Meteo weather provider.
func New(opts ...Option) *Provider {
	p := &Provider{
		client:      &http.Client{Timeout: 30 * time.Second},
		archiveURL:  DefaultArchiveURL,
		forecastURL: DefaultForecastURL,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name returns the provider identifier.
func (p *Provider) Name() string {
	return ProviderName
}

type apiResponse struct {
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	Timezone   string  `json:"timezone"`
	DailyUnits struct {
		Time                   string `json:"time"`
		ShortwaveRadiationSum  string `json:"shortwave_radiation_sum"`
		Temperature2mMean      string `json:"temperature_2m_mean"`
		Temperature2mMax       string `json:"temperature_2m_max"`
		PrecipitationSum       string `json:"precipitation_sum"`
		RelativeHumidity2mMean string `json:"relative_humidity_2m_mean"`
	} `json:"daily_units"`
	Daily struct {
		Time                   []string   `json:"time"`
		ShortwaveRadiationSum  []*float64 `json:"shortwave_radiation_sum"`
		Temperature2mMean      []*float64 `json:"temperature_2m_mean"`
		Temperature2mMax       []*float64 `json:"temperature_2m_max"`
		PrecipitationSum       []*float64 `json:"precipitation_sum"`
		RelativeHumidity2mMean []*float64 `json:"relative_humidity_2m_mean"`
	} `json:"daily"`
}

type errorResponse struct {
	Error  bool   `json:"error"`
	Reason string `json:"reason"`
}

// Observed fetches historical daily weather from the archive endpoint.
func (p *Provider) Observed(ctx context.Context, q connector.WeatherQuery, from, to string) ([]connector.WeatherDay, error) {
	params := url.Values{}
	params.Set("latitude", strconv.FormatFloat(q.Latitude, 'f', -1, 64))
	params.Set("longitude", strconv.FormatFloat(q.Longitude, 'f', -1, 64))
	params.Set("start_date", from)
	params.Set("end_date", to)
	params.Set("timezone", q.Timezone)
	params.Set("daily", DefaultDailyVars)

	reqURL := p.archiveURL + "?" + params.Encode()
	return p.fetchDays(ctx, reqURL)
}

// Forecast fetches forecast daily weather from the forecast endpoint.
func (p *Provider) Forecast(ctx context.Context, q connector.WeatherQuery, days int) ([]connector.WeatherDay, error) {
	if days <= 0 {
		days = 7
	}
	params := url.Values{}
	params.Set("latitude", strconv.FormatFloat(q.Latitude, 'f', -1, 64))
	params.Set("longitude", strconv.FormatFloat(q.Longitude, 'f', -1, 64))
	params.Set("timezone", q.Timezone)
	params.Set("forecast_days", strconv.Itoa(days))
	params.Set("daily", DefaultDailyVars)

	reqURL := p.forecastURL + "?" + params.Encode()
	return p.fetchDays(ctx, reqURL)
}

func (p *Provider) fetchDays(ctx context.Context, reqURL string) ([]connector.WeatherDay, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp errorResponse
		if err := json.Unmarshal(body, &errResp); err == nil && errResp.Reason != "" {
			return nil, fmt.Errorf("open-meteo request failed with status %d: %s", resp.StatusCode, errResp.Reason)
		}
		return nil, fmt.Errorf("open-meteo request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var data apiResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decode json response: %w", err)
	}

	// Verify shortwave radiation unit if present
	radUnit := data.DailyUnits.ShortwaveRadiationSum
	if radUnit != "MJ/m²" && radUnit != "MJ/m2" {
		return nil, fmt.Errorf("unexpected shortwave_radiation_sum unit %q: expected MJ/m2", radUnit)
	}

	out := make([]connector.WeatherDay, len(data.Daily.Time))
	for i, date := range data.Daily.Time {
		d := connector.WeatherDay{Date: date}

		if i < len(data.Daily.ShortwaveRadiationSum) && data.Daily.ShortwaveRadiationSum[i] != nil {
			v := *data.Daily.ShortwaveRadiationSum[i] / 3.6
			d.IrradiationKWhM2 = &v
		}
		if i < len(data.Daily.Temperature2mMean) && data.Daily.Temperature2mMean[i] != nil {
			v := *data.Daily.Temperature2mMean[i]
			d.TemperatureMeanC = &v
		}
		if i < len(data.Daily.Temperature2mMax) && data.Daily.Temperature2mMax[i] != nil {
			v := *data.Daily.Temperature2mMax[i]
			d.TemperatureMaxC = &v
		}
		if i < len(data.Daily.PrecipitationSum) && data.Daily.PrecipitationSum[i] != nil {
			v := *data.Daily.PrecipitationSum[i]
			d.PrecipitationMM = &v
		}
		if i < len(data.Daily.RelativeHumidity2mMean) && data.Daily.RelativeHumidity2mMean[i] != nil {
			v := *data.Daily.RelativeHumidity2mMean[i]
			d.HumidityMeanPct = &v
		}

		out[i] = d
	}

	return out, nil
}
