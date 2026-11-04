package forecast

import (
	"fmt"
	"sort"
	"strings"
)

// FormatMetricsTable formats a map of modelName -> Metrics into a clean ascii table.
func FormatMetricsTable(title string, metricsMap map[string]Metrics) string {
	var sb strings.Builder
	sb.WriteString(title + "\n")
	sb.WriteString(fmt.Sprintf("%-14s %6s %10s %10s %8s %9s %20s %11s\n",
		"Modelo", "N", "MAE (kWh)", "RMSE (kWh)", "R²", "WAPE", "MAPE (>5kWh)", "Bias (kWh)"))
	sb.WriteString(strings.Repeat("-", 96) + "\n")

	// Order models deterministically: physical, calibrated, ridge, persistence, then others
	order := []string{"calibrated", "ridge", "physical", "persistence"}
	seen := make(map[string]bool)
	var sortedNames []string

	for _, name := range order {
		if _, ok := metricsMap[name]; ok {
			sortedNames = append(sortedNames, name)
			seen[name] = true
		}
	}
	var remaining []string
	for name := range metricsMap {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	sortedNames = append(sortedNames, remaining...)

	for _, name := range sortedNames {
		m := metricsMap[name]
		mapeStr := "-"
		if m.MAPE != nil {
			mapeStr = fmt.Sprintf("%6.2f%% (n=%d)", *m.MAPE, m.MAPEN)
		}
		sb.WriteString(fmt.Sprintf("%-14s %6d %10.3f %10.3f %8.4f %8.2f%% %20s %+11.3f\n",
			name,
			m.N,
			m.MAE,
			m.RMSE,
			m.R2,
			m.WAPE*100.0,
			mapeStr,
			m.Bias,
		))
	}
	sb.WriteString(strings.Repeat("-", 96) + "\n")
	return sb.String()
}

// FormatBacktestReport formats a complete BacktestResult for CLI output.
func FormatBacktestReport(res *BacktestResult) string {
	var sb strings.Builder
	sb.WriteString("\n" + strings.Repeat("=", 96) + "\n")
	sb.WriteString(fmt.Sprintf("Planta %d: %s\n", res.PlantID, res.PlantName))
	sb.WriteString(strings.Repeat("=", 96) + "\n")

	if res.Message != "" {
		sb.WriteString(fmt.Sprintf("Aviso: %s\n\n", res.Message))
	}

	if len(res.WalkForwardPooled) == 0 {
		sb.WriteString("Nenhum dado avaliado no backtest.\n")
		return sb.String()
	}

	sb.WriteString(FormatMetricsTable("[Walk-Forward (janela expansível)] - Agrupado (Pooled)", res.WalkForwardPooled))

	if res.ActiveModel != "" {
		activeM := res.WalkForwardPooled[res.ActiveModel]
		persistM := res.WalkForwardPooled["persistence"]
		sb.WriteString(fmt.Sprintf("Modelo ativo selecionado: %s (MAE walk-forward: %.3f kWh vs persistência: %.3f kWh)\n\n",
			res.ActiveModel, activeM.MAE, persistM.MAE))
	} else {
		sb.WriteString("Nenhum modelo ativo selecionado (não superou a persistência ou sem clima).\n\n")
	}

	// Walk-forward per device
	var devNames []string
	for devName := range res.WalkForwardDevices {
		devNames = append(devNames, devName)
	}
	sort.Strings(devNames)

	for _, devName := range devNames {
		tableTitle := fmt.Sprintf("[Walk-Forward] - Dispositivo: %s", devName)
		sb.WriteString(FormatMetricsTable(tableTitle, res.WalkForwardDevices[devName]))
		sb.WriteString("\n")
	}

	// 80/20 Chronological split
	if len(res.Split8020Pooled) > 0 {
		sb.WriteString(FormatMetricsTable("[Divisão 80/20 Cronológica] - Agrupado (Pooled)", res.Split8020Pooled))
		sb.WriteString("\n")

		var dev8020Names []string
		for devName := range res.Split8020Devices {
			dev8020Names = append(dev8020Names, devName)
		}
		sort.Strings(dev8020Names)

		for _, devName := range dev8020Names {
			tableTitle := fmt.Sprintf("[Divisão 80/20] - Dispositivo: %s", devName)
			sb.WriteString(FormatMetricsTable(tableTitle, res.Split8020Devices[devName]))
			sb.WriteString("\n")
		}
	}

	return sb.String()
}
