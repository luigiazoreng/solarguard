package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/luigiazoreng/solarguard/internal/connector/weather/openmeteo"
	"github.com/luigiazoreng/solarguard/internal/pipeline"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func init() {
	register("pipeline", "executa a pipeline completa (clima, qualidade, previsão, alertas)", runPipeline)
}

func runPipeline(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pipeline", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "identificador da planta (opcional, padrão: todas)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer st.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	deps := pipeline.Deps{
		WeatherProvider: openmeteo.New(),
		Logger:          logger,
	}

	if *plantID > 0 {
		run, err := pipeline.RunPlant(ctx, st, *plantID, deps)
		if err != nil {
			return fmt.Errorf("executar pipeline na planta %d: %w", *plantID, err)
		}
		fmt.Printf("Planta %d: pipeline concluída com status %q (data %s)\n", *plantID, run.Status, run.LocalDate)
		return nil
	}

	runs, err := pipeline.RunAll(ctx, st, deps)
	if err != nil {
		return fmt.Errorf("executar pipeline em todas as plantas: %w", err)
	}

	fmt.Printf("Pipeline concluída para %d usinas.\n", len(runs))
	for _, r := range runs {
		fmt.Printf("  Planta %d (%s): %s\n", r.PlantID, r.LocalDate, r.Status)
	}
	return nil
}
