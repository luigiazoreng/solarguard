package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/luigiazoreng/solarguard/internal/connector/weather/openmeteo"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/forecast"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/weather"
)

func init() {
	register("backtest", "executa validação temporal e seleção de modelos por planta", runBacktest)
	register("forecast", "gera previsões de geração esperada e futuras", runForecast)
}

func runBacktest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backtest", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "identificador da planta (opcional, padrão: todas)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer st.Close()

	var plants []domain.Plant
	if *plantID > 0 {
		p, err := st.GetPlant(ctx, *plantID)
		if err != nil {
			return fmt.Errorf("obter planta %d: %w", *plantID, err)
		}
		plants = append(plants, p)
	} else {
		all, err := st.ListPlants(ctx)
		if err != nil {
			return fmt.Errorf("listar plantas: %w", err)
		}
		plants = all
	}

	if len(plants) == 0 {
		fmt.Println("nenhuma planta cadastrada")
		return nil
	}

	var failures []string

	for _, p := range plants {
		res, err := forecast.Backtest(ctx, st, p.ID, forecast.BacktestOpts{
			MinTrainDays: 60,
			RefitDays:    7,
		})
		if err != nil {
			msg := fmt.Sprintf("planta %d (%s): %v", p.ID, p.Name, err)
			fmt.Fprintf(os.Stderr, "erro no backtest da %s\n", msg)
			failures = append(failures, msg)
			continue
		}

		fmt.Print(forecast.FormatBacktestReport(res))
	}

	if len(failures) > 0 {
		return fmt.Errorf("falhas durante o backtest:\n  %s", strings.Join(failures, "\n  "))
	}

	return nil
}

func runForecast(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forecast", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "identificador da planta (opcional, padrão: todas)")
	days := fs.Int("days", 7, "quantidade de dias de previsão futura")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer st.Close()

	var plants []domain.Plant
	if *plantID > 0 {
		p, err := st.GetPlant(ctx, *plantID)
		if err != nil {
			return fmt.Errorf("obter planta %d: %w", *plantID, err)
		}
		plants = append(plants, p)
	} else {
		all, err := st.ListPlants(ctx)
		if err != nil {
			return fmt.Errorf("listar plantas: %w", err)
		}
		plants = all
	}

	if len(plants) == 0 {
		fmt.Println("nenhuma planta cadastrada")
		return nil
	}

	weatherProvider := openmeteo.New()
	weatherSvc := weather.NewService(st, weatherProvider)

	var failures []string

	for _, p := range plants {
		// 1. Sync weather forecast if plant has coordinates or weather location
		if p.WeatherLocationID != nil || (p.Latitude != nil && p.Longitude != nil) {
			if err := weatherSvc.SyncForecast(ctx, p.ID, *days); err != nil {
				fmt.Fprintf(os.Stderr, "aviso: falha ao sincronizar previsão do tempo para planta %d (%s): %v\n", p.ID, p.Name, err)
			}
		}

		// 2. Predict
		res, err := forecast.Predict(ctx, st, p.ID, forecast.PredictOpts{Days: *days})
		if err != nil {
			msg := fmt.Sprintf("planta %d (%s): %v", p.ID, p.Name, err)
			fmt.Fprintf(os.Stderr, "erro ao gerar previsão para %s\n", msg)
			failures = append(failures, msg)
			continue
		}

		fmt.Printf("\nPlanta %d (%s) - Modelo ativo: %s\n", p.ID, p.Name, res.ActiveModel)
		if res.Message != "" {
			fmt.Printf("Aviso: %s\n", res.Message)
		}
		fmt.Printf("Total de previsões geradas (histórico + futuro): %d\n", len(res.Predictions))

		if len(res.FutureDays) > 0 {
			fmt.Printf("\nPrevisões futuras (%d registros):\n", len(res.FutureDays))
			fmt.Printf("%-12s %-10s %12s %14s %14s\n", "Data", "Dispositivo", "Esperado(kWh)", "Inferior(90%)", "Superior(90%)")
			fmt.Println(strings.Repeat("-", 66))

			// Resolve device names
			devs, _ := st.ListDevices(ctx, p.ID)
			devNameMap := make(map[int64]string)
			for _, d := range devs {
				devNameMap[d.ID] = d.Name
			}

			for _, f := range res.FutureDays {
				devName := devNameMap[f.DeviceID]
				if devName == "" {
					devName = fmt.Sprintf("dev_%d", f.DeviceID)
				}
				lowStr, upStr := "-", "-"
				if f.KWhLower != nil {
					lowStr = fmt.Sprintf("%.2f", *f.KWhLower)
				}
				if f.KWhUpper != nil {
					upStr = fmt.Sprintf("%.2f", *f.KWhUpper)
				}
				fmt.Printf("%-12s %-10s %12.2f %14s %14s\n", f.Date, devName, f.KWhExpected, lowStr, upStr)
			}
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("falhas durante previsão:\n  %s", strings.Join(failures, "\n  "))
	}

	return nil
}
