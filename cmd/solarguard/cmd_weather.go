package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/luigiazoreng/solarguard/internal/connector/weather/inmet"
	"github.com/luigiazoreng/solarguard/internal/connector/weather/openmeteo"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/weather"
)

func init() {
	register("weather-import-inmet", "importa arquivo CSV anual de estação do INMET", runWeatherImportInmet)
	register("weather-sync", "sincroniza histórico de clima observado via Open-Meteo", runWeatherSync)
	register("weather-forecast", "sincroniza previsão do tempo via Open-Meteo", runWeatherForecast)
	register("plant-set-location", "define as coordenadas geográficas de uma planta", runPlantSetLocation)
}

func runWeatherImportInmet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("weather-import-inmet", flag.ContinueOnError)
	filePath := fs.String("file", "", "caminho para o arquivo CSV do INMET")
	locationName := fs.String("location", "", "nome do local de clima")
	timezone := fs.String("timezone", "", "fuso horário (ex: America/Sao_Paulo)")
	plantID := fs.Int64("plant-id", 0, "identificador da planta a vincular (opcional)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *filePath == "" || *locationName == "" || *timezone == "" {
		return errors.New("uso: solarguard weather-import-inmet --file PATH --location NAME --timezone ZONE [--plant-id ID]")
	}

	f, err := os.Open(*filePath)
	if err != nil {
		return fmt.Errorf("abrir arquivo %q: %w", *filePath, err)
	}
	defer f.Close()

	station, days, err := inmet.ParseFile(f, *timezone)
	if err != nil {
		return fmt.Errorf("processar arquivo INMET: %w", err)
	}

	s, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer s.Close()

	loc := store.WeatherLocation{
		Name:        *locationName,
		Latitude:    &station.Latitude,
		Longitude:   &station.Longitude,
		Timezone:    *timezone,
		StationCode: station.Code,
	}

	savedLoc, err := s.EnsureWeatherLocation(ctx, loc)
	if err != nil {
		return fmt.Errorf("salvar local de clima: %w", err)
	}

	if *plantID > 0 {
		if err := s.LinkPlantWeather(ctx, *plantID, savedLoc.ID); err != nil {
			return fmt.Errorf("vincular planta %d ao local %d: %w", *plantID, savedLoc.ID, err)
		}
	}

	if err := s.UpsertWeatherDays(ctx, savedLoc.ID, "observed", "inmet", days); err != nil {
		return fmt.Errorf("salvar dias de clima: %w", err)
	}

	fmt.Printf("importados %d dias para o local %q (estação %s)\n", len(days), savedLoc.Name, station.Code)
	return nil
}

func runWeatherSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("weather-sync", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "identificador da planta (opcional)")
	from := fs.String("from", "", "data inicial (YYYY-MM-DD)")
	to := fs.String("to", "", "data final (YYYY-MM-DD)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return errors.New("uso: solarguard weather-sync [--plant-id ID] --from D --to D")
	}

	s, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer s.Close()

	provider := openmeteo.New()
	svc := weather.NewService(s, provider)

	if *plantID > 0 {
		if err := svc.SyncObserved(ctx, *plantID, *from, *to); err != nil {
			return fmt.Errorf("sincronizar clima da planta %d: %w", *plantID, err)
		}
		fmt.Printf("planta %d: clima observado sincronizado (%s a %s)\n", *plantID, *from, *to)
		return nil
	}

	plants, err := s.ListPlants(ctx)
	if err != nil {
		return fmt.Errorf("listar plantas: %w", err)
	}
	if len(plants) == 0 {
		fmt.Println("nenhuma planta cadastrada")
		return nil
	}

	for _, p := range plants {
		if p.WeatherLocationID == nil && (p.Latitude == nil || p.Longitude == nil) {
			fmt.Printf("planta %d (%s) ignorada: sem coordenadas\n", p.ID, p.Name)
			continue
		}
		if err := svc.SyncObserved(ctx, p.ID, *from, *to); err != nil {
			fmt.Fprintf(os.Stderr, "erro ao sincronizar planta %d (%s): %v\n", p.ID, p.Name, err)
			continue
		}
		fmt.Printf("planta %d (%s): clima observado sincronizado (%s a %s)\n", p.ID, p.Name, *from, *to)
	}
	return nil
}

func runWeatherForecast(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("weather-forecast", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "identificador da planta (opcional)")
	days := fs.Int("days", 7, "quantidade de dias de previsão")

	if err := fs.Parse(args); err != nil {
		return err
	}

	s, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer s.Close()

	provider := openmeteo.New()
	svc := weather.NewService(s, provider)

	if *plantID > 0 {
		if err := svc.SyncForecast(ctx, *plantID, *days); err != nil {
			return fmt.Errorf("sincronizar previsão da planta %d: %w", *plantID, err)
		}
		fmt.Printf("planta %d: previsão sincronizada (%d dias)\n", *plantID, *days)
		return nil
	}

	plants, err := s.ListPlants(ctx)
	if err != nil {
		return fmt.Errorf("listar plantas: %w", err)
	}
	if len(plants) == 0 {
		fmt.Println("nenhuma planta cadastrada")
		return nil
	}

	for _, p := range plants {
		if p.WeatherLocationID == nil && (p.Latitude == nil || p.Longitude == nil) {
			fmt.Printf("planta %d (%s) ignorada: sem coordenadas\n", p.ID, p.Name)
			continue
		}
		if err := svc.SyncForecast(ctx, p.ID, *days); err != nil {
			fmt.Fprintf(os.Stderr, "erro ao sincronizar previsão da planta %d (%s): %v\n", p.ID, p.Name, err)
			continue
		}
		fmt.Printf("planta %d (%s): previsão sincronizada (%d dias)\n", p.ID, p.Name, *days)
	}
	return nil
}

func runPlantSetLocation(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plant-set-location", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "identificador da planta")
	lat := fs.Float64("lat", 0, "latitude")
	lon := fs.Float64("lon", 0, "longitude")

	if err := fs.Parse(args); err != nil {
		return err
	}
	hasLat, hasLon := false, false
	fs.Visit(func(f *flag.Flag) {
		hasLat = hasLat || f.Name == "lat"
		hasLon = hasLon || f.Name == "lon"
	})
	if *plantID <= 0 || !hasLat || !hasLon {
		return errors.New("uso: solarguard plant-set-location --plant-id ID --lat X --lon Y")
	}

	s, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer s.Close()

	if err := s.SetPlantLocation(ctx, *plantID, *lat, *lon); err != nil {
		return fmt.Errorf("atualizar coordenadas da planta %d: %w", *plantID, err)
	}

	fmt.Printf("planta %d: coordenadas atualizadas para (%.4f, %.4f)\n", *plantID, *lat, *lon)
	return nil
}
