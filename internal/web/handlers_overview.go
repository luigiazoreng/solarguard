package web

import (
	"net/http"
	"sort"

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
	var totalMeasured, totalExpected float64
	var totalDevices, totalCrit, totalWarn, totalInfo int

	for _, p := range plants {
		devs, _ := s.store.ListDevices(ctx, p.ID)
		totalDevices += len(devs)

		stats, _ := s.store.GetPlantYesterdayStats(ctx, p, now)

		var devPct float64
		hasDev := false
		if stats.HasReadings && stats.HasExpected && stats.ExpectedKWh > 0 {
			devPct = ((stats.MeasuredKWh - stats.ExpectedKWh) / stats.ExpectedKWh) * 100.0
			hasDev = true
		}

		if stats.HasReadings {
			totalMeasured += stats.MeasuredKWh
		}
		if stats.HasExpected {
			totalExpected += stats.ExpectedKWh
		}

		pAlerts := alertsByPlant[p.ID]
		var critCount, warnCount, infoCount int
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
		}

		badge := "Normal"
		score := 0
		if critCount > 0 {
			badge = "Crítico"
			score = 1000 + critCount*10
		} else if warnCount > 0 {
			badge = "Atenção"
			score = 500 + warnCount*5
		} else if hasDev && devPct <= -15.0 {
			badge = "Atenção"
			score = 200 - int(devPct)
		} else if !stats.HasReadings {
			badge = "Sem dados"
			score = 10
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

	// Sort: problems first (highest severity score), then alphabetical
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SeverityScore != rows[j].SeverityScore {
			return rows[i].SeverityScore > rows[j].SeverityScore
		}
		return rows[i].Plant.Name < rows[j].Plant.Name
	})

	var totalDevPct float64
	hasTotalDev := false
	if totalExpected > 0 && totalMeasured > 0 {
		totalDevPct = ((totalMeasured - totalExpected) / totalExpected) * 100.0
		hasTotalDev = true
	}

	data := s.baseTemplateData(r, "Visão Geral", "overview")
	data["Plants"] = rows
	data["TotalPlants"] = len(plants)
	data["TotalDevices"] = totalDevices
	data["TotalYesterdayMeasured"] = totalMeasured
	data["TotalYesterdayExpected"] = totalExpected
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
