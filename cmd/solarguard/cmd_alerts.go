package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"time"

	"github.com/luigiazoreng/solarguard/internal/alerts"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/quality"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func init() {
	register("alerts-run", "avalia regras de alerta sobre as leituras de geração", runAlertsRun)
	register("alerts-list", "lista alertas gerados", runAlertsList)
}

func runAlertsRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("alerts-run", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "identificador interno da planta (omita para todas)")
	asOfStr := fs.String("as-of", "", "data de referência para replay (YYYY-MM-DD)")

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
		// 1. Run quality classifier on full history first
		if _, err := quality.Classify(ctx, st, p.ID, time.Time{}, time.Time{}); err != nil {
			return fmt.Errorf("classificar qualidade da planta %s (id=%d): %w", p.Name, p.ID, err)
		}

		// 2. Run alerts engine on full history (or replay with as-of)
		opts := alerts.Options{
			AsOf: *asOfStr,
		}
		summary, err := alerts.Run(ctx, st, p.ID, opts)
		if err != nil {
			return fmt.Errorf("executar alertas na planta %s (id=%d): %w", p.Name, p.ID, err)
		}

		fmt.Printf("Planta: %s (id=%d, externa=%s)\n", p.Name, p.ID, p.ExternalID)
		fmt.Printf("Novos: %d | Atualizados: %d | Resolvidos: %d\n", summary.New, summary.Updated, summary.Resolved)

		// Print counts per rule sorted
		ruleNames := make([]string, 0, len(summary.CountsByRule))
		for r := range summary.CountsByRule {
			ruleNames = append(ruleNames, r)
		}
		sort.Strings(ruleNames)

		fmt.Println("Alertas por regra:")
		if len(ruleNames) == 0 {
			fmt.Println("  (nenhum alerta gerado)")
		} else {
			for _, r := range ruleNames {
				fmt.Printf("  %s: %d\n", r, summary.CountsByRule[r])
			}
		}

		// Print counts per device sorted
		devNames := make([]string, 0, len(summary.CountsByDevice))
		for d := range summary.CountsByDevice {
			devNames = append(devNames, d)
		}
		sort.Strings(devNames)

		fmt.Println("Alertas por dispositivo:")
		if len(devNames) == 0 {
			fmt.Println("  (nenhum)")
		} else {
			for _, d := range devNames {
				fmt.Printf("  %s:\n", d)
				rules := make([]string, 0, len(summary.CountsByDevice[d]))
				for r := range summary.CountsByDevice[d] {
					rules = append(rules, r)
				}
				sort.Strings(rules)
				for _, r := range rules {
					fmt.Printf("    %s: %d\n", r, summary.CountsByDevice[d][r])
				}
			}
		}

		// Print skipped rules if any
		if len(summary.SkippedRules) > 0 {
			sort.Strings(summary.SkippedRules)
			fmt.Println("Regras ignoradas:")
			for _, r := range summary.SkippedRules {
				switch r {
				case "stale_data":
					fmt.Printf("  - %s (fonte de dados estática)\n", r)
				case "under_expected", "gradual_loss":
					fmt.Printf("  - %s (sem previsões ativas)\n", r)
				default:
					fmt.Printf("  - %s\n", r)
				}
			}
		}
		fmt.Println()
	}

	return nil
}

func runAlertsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("alerts-list", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "filtrar por identificador de planta")
	status := fs.String("status", "open", "filtrar por status (open, ack, resolved, ou vazio para todos)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer st.Close()

	var filter store.AlertFilter
	if *plantID > 0 {
		filter.PlantID = plantID
	}
	if *status != "" {
		filter.Status = *status
	}

	alertsList, err := st.ListAlerts(ctx, filter)
	if err != nil {
		return fmt.Errorf("listar alertas: %w", err)
	}

	if len(alertsList) == 0 {
		fmt.Println("Nenhum alerta encontrado.")
		return nil
	}

	// Fetch devices for name lookup
	devMap := make(map[int64]string)
	if filter.PlantID != nil {
		devs, _ := st.ListDevices(ctx, *filter.PlantID)
		for _, d := range devs {
			devMap[d.ID] = d.Name
		}
	} else {
		plants, _ := st.ListPlants(ctx)
		for _, p := range plants {
			devs, _ := st.ListDevices(ctx, p.ID)
			for _, d := range devs {
				devMap[d.ID] = d.Name
			}
		}
	}

	fmt.Printf("%-5s %-7s %-12s %-20s %-9s %-9s %-10s %-10s %s\n",
		"ID", "PLANTA", "DISPOSITIVO", "REGRA", "SEVERITY", "STATUS", "INÍCIO", "FIM", "TÍTULO")
	for _, a := range alertsList {
		devName := "Usina"
		if a.DeviceID != nil {
			if name, ok := devMap[*a.DeviceID]; ok {
				devName = name
			} else {
				devName = fmt.Sprintf("dev#%d", *a.DeviceID)
			}
		}
		fmt.Printf("%-5d %-7d %-12s %-20s %-9s %-9s %-10s %-10s %s\n",
			a.ID, a.PlantID, devName, a.Rule, a.Severity, a.Status, a.FirstDate, a.LastDate, a.Title)
	}

	return nil
}
