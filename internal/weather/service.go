// Package weather provides orchestration for fetching, updating, and storing weather data for plants.
package weather

import (
	"context"
	"fmt"
	"math"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// Service coordinates weather retrieval from providers and storage in the database.
type Service struct {
	store    *store.Store
	provider connector.WeatherProvider
}

// NewService creates a new weather service with the given store and provider.
func NewService(s *store.Store, p connector.WeatherProvider) *Service {
	return &Service{
		store:    s,
		provider: p,
	}
}

// resolveLocation ensures the plant has a linked WeatherLocation and returns it.
// If the plant already has a weather location, it is retrieved.
// If not, but the plant has coordinates, a location rounded to 2 decimals is ensured and linked.
// If the plant has neither, an error is returned.
func (s *Service) resolveLocation(ctx context.Context, plant domain.Plant) (store.WeatherLocation, error) {
	if plant.WeatherLocationID != nil {
		loc, err := s.store.GetWeatherLocation(ctx, *plant.WeatherLocationID)
		if err != nil {
			return store.WeatherLocation{}, fmt.Errorf("get plant weather location: %w", err)
		}
		return loc, nil
	}

	if plant.Latitude == nil || plant.Longitude == nil {
		return store.WeatherLocation{}, fmt.Errorf("plant %d has no coordinates", plant.ID)
	}

	lat := math.Round(*plant.Latitude*100) / 100
	lon := math.Round(*plant.Longitude*100) / 100
	name := fmt.Sprintf("loc_%.2f_%.2f", lat, lon)

	// A wrong zone would shift every weather day, so an unknown zone is an error.
	if plant.Timezone == "" {
		return store.WeatherLocation{}, fmt.Errorf("plant %d has no time zone", plant.ID)
	}

	loc, err := s.store.EnsureWeatherLocation(ctx, store.WeatherLocation{
		Name:      name,
		Latitude:  &lat,
		Longitude: &lon,
		Timezone:  plant.Timezone,
	})
	if err != nil {
		return store.WeatherLocation{}, fmt.Errorf("ensure weather location: %w", err)
	}

	if err := s.store.LinkPlantWeather(ctx, plant.ID, loc.ID); err != nil {
		return store.WeatherLocation{}, fmt.Errorf("link plant weather: %w", err)
	}

	return loc, nil
}

// SyncObserved fetches observed weather for plantID in [from, to] and stores it with kind "observed".
func (s *Service) SyncObserved(ctx context.Context, plantID int64, from, to string) error {
	plant, err := s.store.GetPlant(ctx, plantID)
	if err != nil {
		return fmt.Errorf("get plant %d: %w", plantID, err)
	}

	loc, err := s.resolveLocation(ctx, plant)
	if err != nil {
		return err
	}

	var lat, lon float64
	if loc.Latitude != nil {
		lat = *loc.Latitude
	}
	if loc.Longitude != nil {
		lon = *loc.Longitude
	}

	query := connector.WeatherQuery{
		Latitude:    lat,
		Longitude:   lon,
		Timezone:    loc.Timezone,
		StationCode: loc.StationCode,
	}

	days, err := s.provider.Observed(ctx, query, from, to)
	if err != nil {
		return fmt.Errorf("fetch observed weather: %w", err)
	}

	if err := s.store.UpsertWeatherDays(ctx, loc.ID, "observed", s.provider.Name(), days); err != nil {
		return fmt.Errorf("store observed weather: %w", err)
	}

	return nil
}

// SyncForecast fetches forecast weather for plantID for the given days count and stores it with kind "forecast".
func (s *Service) SyncForecast(ctx context.Context, plantID int64, days int) error {
	if days <= 0 {
		days = 7
	}

	plant, err := s.store.GetPlant(ctx, plantID)
	if err != nil {
		return fmt.Errorf("get plant %d: %w", plantID, err)
	}

	loc, err := s.resolveLocation(ctx, plant)
	if err != nil {
		return err
	}

	var lat, lon float64
	if loc.Latitude != nil {
		lat = *loc.Latitude
	}
	if loc.Longitude != nil {
		lon = *loc.Longitude
	}

	query := connector.WeatherQuery{
		Latitude:    lat,
		Longitude:   lon,
		Timezone:    loc.Timezone,
		StationCode: loc.StationCode,
	}

	forecastDays, err := s.provider.Forecast(ctx, query, days)
	if err != nil {
		return fmt.Errorf("fetch forecast weather: %w", err)
	}

	if err := s.store.UpsertWeatherDays(ctx, loc.ID, "forecast", s.provider.Name(), forecastDays); err != nil {
		return fmt.Errorf("store forecast weather: %w", err)
	}

	return nil
}
