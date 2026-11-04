// Package reports builds deterministic reports from stored data, with no LLM.
package reports

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/luigiazoreng/solarguard/internal/analytics"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// Request selects what to report on. From and To are local dates, inclusive.
type Request struct {
	PlantID int64
	Kind    string // weekly, monthly or period
	From    string
	To      string
}

// Generate builds a report using the current wall clock and stores it with author system.
func Generate(ctx context.Context, st *store.Store, req Request) (store.Report, error) {
	return GenerateWithTime(ctx, st, req, time.Now())
}

// GenerateWithTime builds a deterministic report with an injected reference time and stores it.
func GenerateWithTime(ctx context.Context, st *store.Store, req Request, now time.Time) (store.Report, error) {
	if req.PlantID <= 0 {
		return store.Report{}, fmt.Errorf("identificador de planta inválido: %d", req.PlantID)
	}
	plant, err := st.GetPlant(ctx, req.PlantID)
	if err != nil {
		return store.Report{}, fmt.Errorf("buscar planta %d: %w", req.PlantID, err)
	}

	tFrom, err := time.Parse(time.DateOnly, req.From)
	if err != nil {
		return store.Report{}, fmt.Errorf("data inicial inválida %q: use o formato YYYY-MM-DD", req.From)
	}
	tTo, err := time.Parse(time.DateOnly, req.To)
	if err != nil {
		return store.Report{}, fmt.Errorf("data final inválida %q: use o formato YYYY-MM-DD", req.To)
	}
	if tFrom.After(tTo) {
		return store.Report{}, fmt.Errorf("intervalo inválido: data inicial (%s) posterior à data final (%s)", req.From, req.To)
	}

	days := int(tTo.Sub(tFrom).Hours()/24) + 1
	if days > 400 {
		return store.Report{}, fmt.Errorf("intervalo de %d dias excede o limite máximo de 400 dias", days)
	}

	kind := req.Kind
	if kind == "" {
		kind = "period"
	}
	if kind != "weekly" && kind != "monthly" && kind != "period" {
		return store.Report{}, fmt.Errorf("tipo de relatório inválido %q: deve ser 'weekly', 'monthly' ou 'period'", kind)
	}

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		return store.Report{}, fmt.Errorf("carregar fuso horário %q: %w", plant.Timezone, err)
	}
	todayStr := now.In(loc).Format(time.DateOnly)

	// 1. Fetch Analytics
	expVsAct, err := analytics.ExpectedVsActual(ctx, st, plant.ID, nil, req.From, req.To)
	if err != nil {
		return store.Report{}, fmt.Errorf("calcular esperado vs realizado: %w", err)
	}

	lostSummary, err := analytics.LostEnergy(ctx, st, plant.ID, req.From, req.To)
	if err != nil {
		return store.Report{}, fmt.Errorf("calcular energia perdida: %w", err)
	}

	dqSummary, err := analytics.DataQuality(ctx, st, plant.ID, req.From, req.To)
	if err != nil {
		return store.Report{}, fmt.Errorf("calcular qualidade de dados: %w", err)
	}

	cmpSummary, err := analytics.CompareDevices(ctx, st, plant.ID, req.From, req.To)
	if err != nil {
		return store.Report{}, fmt.Errorf("comparar dispositivos: %w", err)
	}

	// 2. Fetch Alerts in Period
	periodAlerts, err := st.ListAlerts(ctx, store.AlertFilter{
		PlantID:  &plant.ID,
		FromDate: req.From,
		ToDate:   req.To,
	})
	if err != nil {
		return store.Report{}, fmt.Errorf("listar alertas do período: %w", err)
	}

	// 3. Fetch Active Model Run
	activeModel, _ := st.GetActiveModelRun(ctx, plant.ID)

	// 4. Fetch Forecast if period ends within 3 days of today
	var forecastPreds []store.Prediction
	todayT, _ := time.Parse(time.DateOnly, todayStr)
	diffDays := math.Abs(tTo.Sub(todayT).Hours() / 24)
	if diffDays <= 3 {
		fStart := todayStr
		fEnd := todayT.AddDate(0, 0, 7).Format(time.DateOnly)
		forecastPreds, _ = st.ListActivePredictionsInRange(ctx, plant.ID, fStart, fEnd)
	}

	// 5. Build data_json
	dataMap := map[string]any{
		"plant": map[string]any{
			"id":       plant.ID,
			"name":     plant.Name,
			"timezone": plant.Timezone,
		},
		"period": map[string]any{
			"kind": kind,
			"from": req.From,
			"to":   req.To,
		},
		"resumo": map[string]any{
			"medida_kwh":           expVsAct.TotalMeasuredKWh,
			"esperada_validos_kwh": expVsAct.TotalExpectedKWh,
			"ratio":                expVsAct.Ratio,
			"desvio_kwh":           expVsAct.TotalMeasuredKWh - expVsAct.TotalExpectedKWh,
			"energia_perdida_kwh":  lostSummary.LostEnergyKWh,
		},
		"qualidade_dados": dqSummary,
		"comparacao":      cmpSummary,
		"alertas":         periodAlerts,
		"modelo":          activeModel,
		"previsao":        forecastPreds,
	}
	dataBytes, _ := json.Marshal(dataMap)
	dataJSON := string(dataBytes)

	// 6. Build Markdown in Brazilian Portuguese
	var sb strings.Builder
	escapedPlantName := escapeMD(plant.Name)

	titleKind := "Período"
	switch kind {
	case "weekly":
		titleKind = "Semanal"
	case "monthly":
		titleKind = "Mensal"
	}

	title := fmt.Sprintf("Relatório %s de Geração — %s (%s a %s)", titleKind, plant.Name, req.From, req.To)

	hasValidDays := false
	for _, d := range expVsAct.Days {
		if d.Quality == domain.QualityOK {
			hasValidDays = true
			break
		}
	}

	renderMarkdown(&sb, titleKind, escapedPlantName, plant, req, days, expVsAct, hasValidDays, lostSummary, dqSummary, periodAlerts, loc, cmpSummary, forecastPreds, diffDays, activeModel)

	reportRow := store.Report{
		PlantID:     &plant.ID,
		Kind:        kind,
		PeriodStart: req.From,
		PeriodEnd:   req.To,
		Status:      "final",
		Author:      "system",
		Title:       title,
		BodyMD:      sb.String(),
		DataJSON:    dataJSON,
	}

	saved, err := st.InsertReport(ctx, reportRow)
	if err != nil {
		return store.Report{}, fmt.Errorf("salvar relatório: %w", err)
	}
	return saved, nil
}

func renderMarkdown(sb *strings.Builder, titleKind, escapedPlantName string, plant domain.Plant, req Request, days int,
	expVsAct analytics.ExpectedVsActualSummary, hasValidDays bool, lostSummary analytics.LostEnergySummary,
	dqSummary analytics.DataQualitySummary, periodAlerts []store.Alert, loc *time.Location,
	cmpSummary analytics.CompareDevicesSummary, forecastPreds []store.Prediction, diffDays float64,
	activeModel store.ModelRun) {

	sb.WriteString(fmt.Sprintf("# Relatório %s de Geração — %s\n\n", titleKind, escapedPlantName))
	sb.WriteString(fmt.Sprintf("- **Planta:** %s (ID: %d)\n", escapedPlantName, plant.ID))
	sb.WriteString(fmt.Sprintf("- **Período avaliado:** %s a %s (%d dias)\n", req.From, req.To, days))
	sb.WriteString(fmt.Sprintf("- **Fuso horário:** %s\n\n", plant.Timezone))

	// Section 1: Resumo
	sb.WriteString("## 1. Resumo Executivo\n\n")
	if !hasValidDays {
		sb.WriteString("Nenhum dado de medição válida disponível no período avaliado.\n\n")
		if lostSummary.LostEnergyKWh > 0 {
			sb.WriteString(fmt.Sprintf("- **Energia estimada perdida:** %s (calculada sobre %d dia(s) com dados ausentes ou inválidos)\n\n",
				formatKWh(lostSummary.LostEnergyKWh), lostSummary.DeviceDaysAffected))
		}
	} else {
		desvioKWh := expVsAct.TotalMeasuredKWh - expVsAct.TotalExpectedKWh
		desvioPctStr := "N/A"
		if expVsAct.TotalExpectedKWh > 0 {
			pct := (desvioKWh / expVsAct.TotalExpectedKWh) * 100.0
			desvioPctStr = formatPercent(pct)
		}

		sb.WriteString("| Métrica | Valor |\n")
		sb.WriteString("|---|---|\n")
		sb.WriteString(fmt.Sprintf("| Geração medida | %s |\n", formatKWh(expVsAct.TotalMeasuredKWh)))
		sb.WriteString(fmt.Sprintf("| Esperada nos dias válidos | %s |\n", formatKWh(expVsAct.TotalExpectedKWh)))
		sb.WriteString(fmt.Sprintf("| Desvio | %s (%s) |\n", formatKWh(desvioKWh), desvioPctStr))
		sb.WriteString(fmt.Sprintf("| Energia estimada perdida | %s |\n\n", formatKWh(lostSummary.LostEnergyKWh)))

		if lostSummary.LostEnergyKWh > 0 {
			sb.WriteString(fmt.Sprintf("> [!NOTE]\n> A energia estimada perdida é uma estimativa calculada sobre %d dia(s)-dispositivo com medições ausentes ou inválidas.\n\n", lostSummary.DeviceDaysAffected))
		}
	}

	// Section 2: Qualidade dos dados por dispositivo
	sb.WriteString("## 2. Qualidade dos Dados por Dispositivo\n\n")
	if len(dqSummary.Devices) == 0 {
		sb.WriteString("Nenhum dispositivo cadastrado para avaliação de qualidade.\n\n")
	} else {
		sb.WriteString("| Dispositivo | % Válido | Dias OK | Suspeitos | Inválidos | Ausentes | Motivos frequentes |\n")
		sb.WriteString("|---|---|---|---|---|---|---|\n")
		for _, devQ := range dqSummary.Devices {
			reasonsList := make([]string, 0, len(devQ.ReasonsCount))
			for rName, count := range devQ.ReasonsCount {
				reasonsList = append(reasonsList, fmt.Sprintf("%s (%d)", escapeMD(rName), count))
			}
			sort.Strings(reasonsList)
			reasonsStr := strings.Join(reasonsList, ", ")
			if reasonsStr == "" {
				reasonsStr = "Nenhum"
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %d | %d | %d | %d | %s |\n",
				escapeMD(devQ.DeviceName),
				formatPercent(devQ.PercentOK),
				devQ.OKDays,
				devQ.SuspectDays,
				devQ.InvalidDays,
				devQ.MissingDays,
				reasonsStr,
			))
		}
		sb.WriteString("\n")
	}

	// Section 3: Alertas do período
	sb.WriteString("## 3. Alertas do Período\n\n")
	sb.WriteString("Um alerta é um indício para análise, não um diagnóstico.\n\n")
	if len(periodAlerts) == 0 {
		sb.WriteString("Nenhum alerta registrado no período avaliado.\n\n")
	} else {
		byRule := make(map[string][]store.Alert)
		var rulesOrder []string
		for _, a := range periodAlerts {
			if _, exists := byRule[a.Rule]; !exists {
				rulesOrder = append(rulesOrder, a.Rule)
			}
			byRule[a.Rule] = append(byRule[a.Rule], a)
		}
		sort.Strings(rulesOrder)

		for _, rule := range rulesOrder {
			alertsList := byRule[rule]
			sb.WriteString(fmt.Sprintf("### Regra: `%s` (%d ocorrência(s))\n\n", escapeMD(rule), len(alertsList)))
			sb.WriteString("| Título | Primeira data | Última data | Detectado em | Severidade | Status |\n")
			sb.WriteString("|---|---|---|---|---|---|\n")
			for _, a := range alertsList {
				detectedAtStr := a.CreatedAt.In(loc).Format("2006-01-02 15:04")
				sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s |\n",
					escapeMD(a.Title),
					a.FirstDate,
					a.LastDate,
					detectedAtStr,
					escapeMD(a.Severity),
					escapeMD(a.Status),
				))
			}
			sb.WriteString("\n")
		}
	}

	// Section 4: Comparação entre dispositivos
	sb.WriteString("## 4. Comparação entre Dispositivos\n\n")
	if len(cmpSummary.Devices) <= 1 {
		sb.WriteString("Comparação não aplicável: a planta possui apenas um dispositivo cadastrado ou nenhum dispositivo comparável.\n\n")
	} else if cmpSummary.CommonOKDays == 0 {
		sb.WriteString("Não há dias com medições simultaneamente válidas em todos os dispositivos para uma comparação justa no período.\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("Comparação justa calculada sobre **%d dia(s)** em que todos os dispositivos estavam com operação e medição válidas simultaneamente:\n\n", cmpSummary.CommonOKDays))
		sb.WriteString("| Dispositivo | Geração medida | Geração esperada | Razão / Mediana dos pares |\n")
		sb.WriteString("|---|---|---|---|\n")
		for _, devC := range cmpSummary.Devices {
			ratioStr := "N/A"
			if devC.RatioToPeerMedian != nil {
				ratioStr = fmt.Sprintf("%.2fx", *devC.RatioToPeerMedian)
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				escapeMD(devC.DeviceName),
				formatKWh(devC.MeasuredKWh),
				formatKWh(devC.ExpectedKWh),
				ratioStr,
			))
		}
		sb.WriteString("\n")
	}

	// Section 5: Previsão dos próximos dias (only when period ends within 3 days of today)
	if diffDays <= 3 {
		sb.WriteString("## 5. Previsão dos próximos dias\n\n")
		if len(forecastPreds) == 0 {
			sb.WriteString("Nenhuma previsão disponível para os próximos dias.\n\n")
		} else {
			byDate := make(map[string]float64)
			var datesOrder []string
			for _, p := range forecastPreds {
				if _, exists := byDate[p.Date]; !exists {
					datesOrder = append(datesOrder, p.Date)
				}
				byDate[p.Date] += p.KWhExpected
			}
			sort.Strings(datesOrder)

			sb.WriteString("| Data | Geração esperada prevista |\n")
			sb.WriteString("|---|---|\n")
			for _, dStr := range datesOrder {
				sb.WriteString(fmt.Sprintf("| %s | %s |\n", dStr, formatKWh(byDate[dStr])))
			}
			sb.WriteString("\n")
		}
	}

	// Section 6: Modelo em uso
	sb.WriteString("## 6. Modelo em Uso\n\n")
	if activeModel.ID == 0 {
		sb.WriteString("Nenhum modelo ativo configurado para esta planta.\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("- **Modelo ativo:** %s (versão %s)\n", escapeMD(activeModel.Name), escapeMD(activeModel.Version)))
		sb.WriteString(fmt.Sprintf("- **Ajustado em:** %s\n\n", activeModel.TrainedAt.In(loc).Format("2006-01-02 15:04")))

		var metricsPayload map[string]any
		_ = json.Unmarshal([]byte(activeModel.MetricsJSON), &metricsPayload)

		if wf, ok := metricsPayload["walk_forward"].(map[string]any); ok {
			sb.WriteString("### Métricas de Validação Temporal (Walk-Forward)\n\n")
			sb.WriteString("| Métrica | Valor |\n")
			sb.WriteString("|---|---|\n")

			targetWF := wf
			if pooled, ok := wf["pooled"].(map[string]any); ok {
				targetWF = pooled
			}

			if mae, ok := targetWF["mae"].(float64); ok {
				sb.WriteString(fmt.Sprintf("| MAE | %s kWh |\n", formatNumberPtBR(mae, 2)))
			}
			if rmse, ok := targetWF["rmse"].(float64); ok {
				sb.WriteString(fmt.Sprintf("| RMSE | %s kWh |\n", formatNumberPtBR(rmse, 2)))
			}
			if r2, ok := targetWF["r2"].(float64); ok {
				sb.WriteString(fmt.Sprintf("| R² | %s |\n", formatNumberPtBR(r2, 2)))
			}
			if wape, ok := targetWF["wape"].(float64); ok {
				sb.WriteString(fmt.Sprintf("| WAPE | %s |\n", formatPercent(wape*100.0)))
			}
			sb.WriteString("\n")
		}

		sb.WriteString("> [!IMPORTANT]\n")
		sb.WriteString("> Métricas calculadas com o clima observado do dia. Elas medem a geração esperada, não uma previsão feita com antecedência.\n\n")
	}
}

// escapeMD escapes Markdown and HTML special characters in provider text.
func escapeMD(text string) string {
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")
	text = strings.ReplaceAll(text, "|", `\|`)
	text = strings.ReplaceAll(text, "*", `\*`)
	text = strings.ReplaceAll(text, "_", `\_`)
	text = strings.ReplaceAll(text, "`", "\\`")
	if strings.HasPrefix(text, "#") {
		text = `\#` + text[1:]
	}
	text = strings.ReplaceAll(text, "\n#", "\n\\#")
	return text
}

// formatKWh formats a kWh measurement in pt-BR (e.g. "1.234,56 kWh").
func formatKWh(v float64) string {
	return formatNumberPtBR(v, 2) + " kWh"
}

// formatPercent formats a percentage in pt-BR (e.g. "98,5%").
func formatPercent(v float64) string {
	return formatNumberPtBR(v, 1) + "%"
}

// formatNumberPtBR formats a float with dot thousands separator and comma decimal separator.
func formatNumberPtBR(v float64, decimals int) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	fmtStr := fmt.Sprintf("%%.%df", decimals)
	formatted := fmt.Sprintf(fmtStr, v)

	parts := strings.Split(formatted, ".")
	intPart := parts[0]
	decPart := ""
	if len(parts) > 1 {
		decPart = parts[1]
	}

	var intWithDots strings.Builder
	l := len(intPart)
	for i, c := range intPart {
		if i > 0 && (l-i)%3 == 0 {
			intWithDots.WriteByte('.')
		}
		intWithDots.WriteRune(c)
	}

	if decimals > 0 && decPart != "" {
		return sign + intWithDots.String() + "," + decPart
	}
	return sign + intWithDots.String()
}
