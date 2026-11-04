package web

import (
	"net/http"
	"sort"

	"github.com/luigiazoreng/solarguard/internal/analytics"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

type PlantOverviewRow struct {
	Plant               domain.Plant
	YesterdayStats      store.PlantYesterdayStats
	DeviationPct        float64
	HasDeviation        bool
	OpenAlertsCount     int
	CriticalAlertsCount int
	WarningAlertsCount  int
	InfoAlertsCount     int
	StatusBadge         string
	SeverityScore       int
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	plants, err := s.store.ListPlants(ctx)
	if err != nil {
		s.cfg.Logger.Error("list plants", "err", err)
		http.Error(w, "Erro ao listar usinas", http.StatusInternalServerError)
		return
	}

	alerts, err := s.store.ListAlerts(ctx, store.AlertFilter{Status: "open"})
	if err != nil {
		s.cfg.Logger.Error("list alerts", "err", err)
		http.Error(w, "Erro ao listar alertas", http.StatusInternalServerError)
		return
	}

	alertsByPlant := make(map[int64][]store.Alert)
	for _, a := range alerts {
		alertsByPlant[a.PlantID] = append(alertsByPlant[a.PlantID], a)
	}

	now := s.cfg.Now()
	var rows []PlantOverviewRow
	var totalMeasured, pairedMeasured, pairedExpected float64
	var totalDevices, totalCrit, totalWarn, totalInfo int
	var plantsWithReadingsCount, plantsCompared int

	for _, p := range plants {
		devs, err := s.store.ListDevices(ctx, p.ID)
		if err != nil {
			s.cfg.Logger.Error("list devices", "plant_id", p.ID, "err", err)
			http.Error(w, "Erro ao listar dispositivos", http.StatusInternalServerError)
			return
		}
		totalDevices += len(devs)

		stats, err := s.store.GetPlantYesterdayStats(ctx, p, now)
		if err != nil {
			s.cfg.Logger.Error("yesterday stats", "plant_id", p.ID, "err", err)
			http.Error(w, "Erro ao calcular geração de ontem", http.StatusInternalServerError)
			return
		}

		// The deviation only compares device-days that have both a valid reading
		// and an expected value, through the same analytics used everywhere else.
		eva, err := analytics.ExpectedVsActual(ctx, s.store, p.ID, nil, stats.YesterdayDate, stats.YesterdayDate)
		if err != nil {
			s.cfg.Logger.Error("expected vs actual", "plant_id", p.ID, "err", err)
			http.Error(w, "Erro ao comparar geração de ontem", http.StatusInternalServerError)
			return
		}

		var devPct float64
		hasDev := false
		if eva.PairedDeviceDays > 0 && eva.PairedExpectedKWh > 0 {
			devPct = ((eva.PairedMeasuredKWh - eva.PairedExpectedKWh) / eva.PairedExpectedKWh) * 100.0
			hasDev = true
			pairedMeasured += eva.PairedMeasuredKWh
			pairedExpected += eva.PairedExpectedKWh
			plantsCompared++
		}

		if stats.HasReadings {
			totalMeasured += stats.MeasuredKWh
			plantsWithReadingsCount++
		}

		pAlerts := alertsByPlant[p.ID]
		var critCount, warnCount, infoCount int
		hasOfflineDevice := false
		hasStaleAlert := false

		for _, d := range devs {
			if d.Kind != "plant" && d.Online != nil && !*d.Online {
				hasOfflineDevice = true
				break
			}
		}

		for _, a := range pAlerts {
			switch a.Severity {
			case "critical":
				critCount++
				totalCrit++
			case "warning":
				warnCount++
				totalWarn++
			case "info":
				infoCount++
				totalInfo++
			}
			if a.Rule == "device_offline" {
				hasOfflineDevice = true
			}
			if a.Rule == "stale_data" {
				hasStaleAlert = true
			}
		}

		// Priority order: "Sem comunicação" -> "Crítico" -> "Atenção" -> "Sem dados recentes" -> "OK"
		var badge string
		var score int

		if hasOfflineDevice {
			badge = "Sem comunicação"
			score = 2000
		} else if critCount > 0 {
			badge = "Crítico"
			score = 1000 + critCount*10
		} else if warnCount > 0 || (hasDev && devPct <= -15.0) {
			badge = "Atenção"
			score = 500 + warnCount*5
		} else if stats.HasStaleData || hasStaleAlert || !stats.HasReadings {
			badge = "Sem dados recentes"
			score = 100
		} else {
			badge = "OK"
			score = 0
		}

		rows = append(rows, PlantOverviewRow{
			Plant:               p,
			YesterdayStats:      stats,
			DeviationPct:        devPct,
			HasDeviation:        hasDev,
			OpenAlertsCount:     len(pAlerts),
			CriticalAlertsCount: critCount,
			WarningAlertsCount:  warnCount,
			InfoAlertsCount:     infoCount,
			StatusBadge:         badge,
			SeverityScore:       score,
		})
	}

	// Sort: highest severity score first, then alphabetical by name
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SeverityScore != rows[j].SeverityScore {
			return rows[i].SeverityScore > rows[j].SeverityScore
		}
		return rows[i].Plant.Name < rows[j].Plant.Name
	})

	hasAnyYesterdayReadings := plantsWithReadingsCount > 0
	var totalDevPct float64
	hasTotalDev := false
	if pairedExpected > 0 {
		totalDevPct = ((pairedMeasured - pairedExpected) / pairedExpected) * 100.0
		hasTotalDev = true
	}

	data := s.baseTemplateData(r, "Visão Geral", "overview")
	data["Plants"] = rows
	data["TotalPlants"] = len(plants)
	data["TotalDevices"] = totalDevices
	data["HasAnyYesterdayReadings"] = hasAnyYesterdayReadings
	data["TotalYesterdayMeasured"] = totalMeasured
	data["PlantsWithReadings"] = plantsWithReadingsCount
	data["PlantsCompared"] = plantsCompared
	data["TotalDeviationPct"] = totalDevPct
	data["HasDeviation"] = hasTotalDev
	data["TotalOpenAlerts"] = len(alerts)
	data["TotalCriticalAlerts"] = totalCrit
	data["TotalWarningAlerts"] = totalWarn
	data["TotalInfoAlerts"] = totalInfo

	if err := s.render(w, "overview", data); err != nil {
		s.cfg.Logger.Error("render overview", "err", err)
	}
}
