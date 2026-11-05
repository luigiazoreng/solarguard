// Package domain holds the types shared across SolarGuard packages.
package domain

import (
	"fmt"
	"time"
)

// Metric names a measured quantity.
type Metric string

const (
	// MetricEnergyKWh is energy generated during the reading's period.
	MetricEnergyKWh Metric = "energy_kwh"
	// MetricPowerKW is average power during the reading's period.
	MetricPowerKW Metric = "power_kw"
)

// Quality classifies a reading. It never changes the measured value.
type Quality string

const (
	QualityUnchecked Quality = "unchecked"
	QualityOK        Quality = "ok"
	QualitySuspect   Quality = "suspect"
	QualityInvalid   Quality = "invalid"
)

// Source is a configured data origin. Its secret is either CredentialEnc, the
// token encrypted by an account, or the environment variable named by
// CredentialEnv, used by the command line. It is never stored in clear text.
type Source struct {
	ID            int64
	Kind          string
	Name          string
	ConfigJSON    string
	CredentialEnv string
	CredentialEnc string
	Enabled       bool
	UserID        *int64 // nil: no account owns it, so the panel hides it
}

// Plant is one generation site. Optional fields are nil when unknown.
type Plant struct {
	ID                int64
	SourceID          int64
	ExternalID        string
	Name              string
	Timezone          string
	Latitude          *float64
	Longitude         *float64
	PeakPowerKW       *float64
	WeatherLocationID *int64
}

// Device is one generating unit in a plant, such as an inverter or a tower.
// PanelAreaM2 and Efficiency feed the physical model when known.
type Device struct {
	ID          int64
	PlantID     int64
	ExternalID  string
	Name        string
	Kind        string
	CapacityKW  *float64
	PanelAreaM2 *float64
	Efficiency  *float64
	Online      *bool
	LastSeenAt  *time.Time
}

// Reading is one immutable measurement over [PeriodStart, PeriodEnd).
// RawValue preserves the provider's value exactly as received.
type Reading struct {
	ID            int64
	DeviceID      int64
	Metric        Metric
	PeriodStart   time.Time
	PeriodEnd     time.Time
	Value         *float64
	RawValue      string
	Quality       Quality
	QualityReason string
	RawPayloadID  *int64
}

// LocalDay returns the UTC bounds of a calendar date in the given time zone.
// date must be in YYYY-MM-DD format.
func LocalDay(date, timezone string) (start, end time.Time, err error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("load time zone %q: %w", timezone, err)
	}
	d, err := time.ParseInLocation(time.DateOnly, date, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse date %q: %w", date, err)
	}
	return d.UTC(), d.AddDate(0, 0, 1).UTC(), nil
}
