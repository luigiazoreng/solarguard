// Package connector defines how SolarGuard reads from external providers.
// Each provider lives in a subpackage. Connectors are read-only.
package connector

import (
	"context"
	"errors"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
)

// ErrRateLimited reports that the provider refused a call because of its rate
// limit. Return it wrapped in a RateLimitError when the wait time is known.
var ErrRateLimited = errors.New("provider rate limit")

// RateLimitError carries how long to wait before retrying.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// Task is exactly one provider call. The scheduler persists tasks, so every
// field must survive a JSON round trip.
type Task struct {
	// Endpoint is the provider operation, for example "device/inverter/day_energy".
	Endpoint string `json:"endpoint"`
	// Params holds the call parameters.
	Params map[string]string `json:"params"`
	// LimitKey identifies what the provider's limit applies to.
	LimitKey string `json:"limit_key"`
	// MinInterval is the shortest allowed gap between calls with this LimitKey.
	MinInterval time.Duration `json:"min_interval"`
	// DailyBudgetKey and DailyBudget cap calls per UTC day. Budget 0 means no cap.
	DailyBudgetKey string `json:"daily_budget_key,omitempty"`
	DailyBudget    int    `json:"daily_budget,omitempty"`
	// Priority orders due tasks. Higher runs first.
	Priority int `json:"priority"`
	// Refreshable marks tasks that should refresh existing records instead of skipping them.
	Refreshable bool `json:"refreshable,omitempty"`
}

// PlantInfo describes a discovered plant.
type PlantInfo struct {
	ExternalID  string
	Name        string
	Timezone    string
	Latitude    *float64
	Longitude   *float64
	PeakPowerKW *float64
}

// DeviceInfo describes a discovered device.
type DeviceInfo struct {
	PlantExternalID string
	ExternalID      string
	Name            string
	Kind            string
	Online          *bool
}

// ReadingInfo is a measurement before it is linked to stored device IDs.
type ReadingInfo struct {
	PlantExternalID  string
	DeviceExternalID string
	Metric           domain.Metric
	PeriodStart      time.Time
	PeriodEnd        time.Time
	Value            *float64
	RawValue         string
}

// Result is what one call produced. Raw must hold the provider's exact response.
type Result struct {
	Plants   []PlantInfo
	Devices  []DeviceInfo
	Readings []ReadingInfo
	// Followups are further calls this result revealed, such as the next page
	// or the devices of a new plant.
	Followups []Task
	Raw       []byte
}

// Connector is implemented once per provider.
type Connector interface {
	// Kind returns the provider name stored in sources.kind.
	Kind() string
	// Discovery returns the tasks that list plants and devices.
	Discovery() []Task
	// DailyTasks returns the tasks that collect one local calendar date.
	DailyTasks(plant domain.Plant, devices []domain.Device, date string) []Task
	// Execute performs one task. It returns an error wrapping ErrRateLimited
	// when the provider refuses the call for rate reasons.
	Execute(ctx context.Context, task Task) (Result, error)
}

// WeatherDay is one local day of weather for a location.
type WeatherDay struct {
	Date             string
	IrradiationKWhM2 *float64
	TemperatureMeanC *float64
	TemperatureMaxC  *float64
	PrecipitationMM  *float64
	HumidityMeanPct  *float64
}

// WeatherQuery identifies a location. Providers use the fields they support.
type WeatherQuery struct {
	Latitude    float64
	Longitude   float64
	Timezone    string
	StationCode string
}

// WeatherProvider reads observed and forecast weather.
type WeatherProvider interface {
	Name() string
	Observed(ctx context.Context, q WeatherQuery, from, to string) ([]WeatherDay, error)
	Forecast(ctx context.Context, q WeatherQuery, days int) ([]WeatherDay, error)
}
