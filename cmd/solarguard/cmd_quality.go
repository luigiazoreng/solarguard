package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/quality"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func init() {
	register("quality", "classifica a qualidade das leituras de geração", runQuality)
}

func runQuality(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quality", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "identificador interno da planta (omita para todas)")
	fromStr := fs.String("from", "", "data inicial (YYYY-MM-DD)")
	toStr := fs.String("to", "", "data final (YYYY-MM-DD)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer st.Close()

	var plants []domain.Plant
	if *plantID > 0 {
		p, err := st.GetPlant(ctx, *plantID)
		if err != nil {
			return fmt.Errorf("buscar planta %d: %w", *plantID, err)
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
		fmt.Println("Nenhuma planta cadastrada.")
		return nil
	}

	for _, p := range plants {
		var from, to time.Time
		if *fromStr != "" {
			fStart, _, err := domain.LocalDay(*fromStr, p.Timezone)
			if err != nil {
				return fmt.Errorf("data inicial inválida %q: %w", *fromStr, err)
			}
			from = fStart
		}
		if *toStr != "" {
			_, tEnd, err := domain.LocalDay(*toStr, p.Timezone)
			if err != nil {
				return fmt.Errorf("data final inválida %q: %w", *toStr, err)
			}
			to = tEnd
		}

		summary, err := quality.Classify(ctx, st, p.ID, from, to)
		if err != nil {
			return fmt.Errorf("classificar planta %s (id=%d): %w", p.Name, p.ID, err)
		}

		fmt.Printf("Planta: %s (id=%d, externa=%s)\n", p.Name, p.ID, p.ExternalID)
		fmt.Printf("Total de leituras: %d\n", summary.TotalReadings)

		// Print per device per reason sorted
		devNames := make([]string, 0, len(summary.ByDevice))
		for d := range summary.ByDevice {
			devNames = append(devNames, d)
		}
		sort.Strings(devNames)

		for _, d := range devNames {
			fmt.Printf("  Dispositivo %s:\n", d)
			reasons := make([]string, 0, len(summary.ByDevice[d]))
			for r := range summary.ByDevice[d] {
				reasons = append(reasons, r)
			}
			sort.Strings(reasons)
			for _, r := range reasons {
				fmt.Printf("    %s: %d\n", r, summary.ByDevice[d][r])
			}
		}
	}

	return nil
}
