package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/luigiazoreng/solarguard/internal/reports"
	"github.com/luigiazoreng/solarguard/internal/store"
)

type ReportListItem struct {
	Report    store.Report
	PlantName string
}

func (s *Server) handleReportsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reps, err := s.store.ListReports(ctx, store.ReportFilter{Limit: 50})
	if err != nil {
		s.cfg.Logger.Error("list reports", "err", err)
		http.Error(w, "Erro ao listar relatórios", http.StatusInternalServerError)
		return
	}

	plants, _ := s.store.ListPlants(ctx)
	plantMap := make(map[int64]string)
	for _, p := range plants {
		plantMap[p.ID] = p.Name
	}

	var items []ReportListItem
	for _, rep := range reps {
		pName := "Todas as usinas"
		if rep.PlantID != nil {
			if name, ok := plantMap[*rep.PlantID]; ok {
				pName = name
			}
		}
		items = append(items, ReportListItem{
			Report:    rep,
			PlantName: pName,
		})
	}

	now := s.cfg.Now()
	defaultTo := now.AddDate(0, 0, -1).Format("2006-01-02")
	defaultFrom := now.AddDate(0, 0, -8).Format("2006-01-02")

	errMsg := r.URL.Query().Get("error")

	data := s.baseTemplateData(r, "Relatórios", "reports")
	data["Reports"] = items
	data["Plants"] = plants
	data["DefaultFrom"] = defaultFrom
	data["DefaultTo"] = defaultTo
	data["ErrorMessage"] = errMsg

	if err := s.render(w, "reports", data); err != nil {
		s.cfg.Logger.Error("render reports", "err", err)
	}
}

func (s *Server) handleReportDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	reportID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	rep, err := s.store.GetReport(ctx, reportID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Erro ao obter relatório", http.StatusInternalServerError)
		return
	}

	pName := "Todas as usinas"
	if rep.PlantID != nil {
		if plant, err := s.store.GetPlant(ctx, *rep.PlantID); err == nil {
			pName = plant.Name
		}
	}

	data := s.baseTemplateData(r, rep.Title, "reports")
	data["Report"] = rep
	data["PlantName"] = pName

	if err := s.render(w, "report_detail", data); err != nil {
		s.cfg.Logger.Error("render report detail", "err", err)
	}
}

func (s *Server) handleReportGenerate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	plantIDStr := r.PostFormValue("plant_id")
	plantID, err := strconv.ParseInt(plantIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Usina inválida", http.StatusBadRequest)
		return
	}

	kind := r.PostFormValue("kind")
	from := r.PostFormValue("from")
	to := r.PostFormValue("to")

	req := reports.Request{
		PlantID: plantID,
		Kind:    kind,
		From:    from,
		To:      to,
	}

	_, err = reports.Generate(ctx, s.store, req)
	if err != nil {
		// As documented, reports.Generate returns reports.ErrNotImplemented currently.
		// Display message cleanly without crashing.
		http.Redirect(w, r, "/reports?error="+err.Error(), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/reports", http.StatusSeeOther)
}

func (s *Server) handleReportFinalize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	reportID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := s.store.FinalizeReport(ctx, reportID); err != nil {
		s.cfg.Logger.Error("finalize report", "id", reportID, "err", err)
		http.Error(w, "Erro ao finalizar relatório", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/reports/"+idStr, http.StatusSeeOther)
}
