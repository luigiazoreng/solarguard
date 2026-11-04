package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

type AlertListItem struct {
	Alert      store.Alert
	PlantName  string
	DeviceName string
}

type DayValueEvidence struct {
	Date       string   `json:"date"`
	Value      *float64 `json:"value"`
	Expected   *float64 `json:"expected"`
	PeerMedian *float64 `json:"peer_median"`
	Reason     string   `json:"reason"`
}

func (s *Server) handleAlertsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	var filter store.AlertFilter
	filterPlantID := q.Get("plant_id")
	if filterPlantID != "" {
		if id, err := strconv.ParseInt(filterPlantID, 10, 64); err == nil {
			filter.PlantID = &id
		}
	}

	filterStatus := q.Get("status")
	filter.Status = filterStatus

	filterSeverity := q.Get("severity")
	filter.Severity = filterSeverity

	filterRule := q.Get("rule")
	filter.Rule = filterRule

	page := 1
	if pStr := q.Get("page"); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			page = p
		}
	}

	pageSize := 20
	filter.Limit = pageSize + 1
	filter.Offset = (page - 1) * pageSize

	alerts, err := s.store.ListAlerts(ctx, filter)
	if err != nil {
		s.cfg.Logger.Error("list alerts", "err", err)
		http.Error(w, "Erro ao listar alertas", http.StatusInternalServerError)
		return
	}

	hasNextPage := false
	if len(alerts) > pageSize {
		hasNextPage = true
		alerts = alerts[:pageSize]
	}

	plants, _ := s.store.ListPlants(ctx)
	plantMap := make(map[int64]string)
	for _, p := range plants {
		plantMap[p.ID] = p.Name
	}

	var items []AlertListItem
	for _, a := range alerts {
		pName := plantMap[a.PlantID]
		dName := ""
		if a.DeviceID != nil {
			if dev, err := s.store.GetDevice(ctx, *a.DeviceID); err == nil {
				dName = dev.Name
			}
		}
		items = append(items, AlertListItem{
			Alert:      a,
			PlantName:  pName,
			DeviceName: dName,
		})
	}

	data := s.baseTemplateData(r, "Alertas e Incidentes", "alerts")
	data["Alerts"] = items
	data["Plants"] = plants
	data["FilterPlantID"] = filterPlantID
	data["FilterStatus"] = filterStatus
	data["FilterSeverity"] = filterSeverity
	data["FilterRule"] = filterRule
	data["Page"] = page
	data["HasNextPage"] = hasNextPage

	if err := s.render(w, "alerts", data); err != nil {
		s.cfg.Logger.Error("render alerts", "err", err)
	}
}

func (s *Server) handleAlertDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	alertID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	alert, err := s.store.GetAlert(ctx, alertID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Erro ao obter alerta", http.StatusInternalServerError)
		return
	}

	plant, err := s.store.GetPlant(ctx, alert.PlantID)
	if err != nil {
		http.Error(w, "Erro ao obter usina do alerta", http.StatusInternalServerError)
		return
	}

	var devPtr *domain.Device
	if alert.DeviceID != nil {
		if dev, err := s.store.GetDevice(ctx, *alert.DeviceID); err == nil {
			devPtr = &dev
		}
	}

	// Parse evidence JSON
	var evidenceValues []DayValueEvidence
	var evidenceParams map[string]any

	if alert.EvidenceJSON != "" {
		var ev struct {
			Params map[string]any     `json:"params"`
			Values []DayValueEvidence `json:"values"`
		}
		if err := json.Unmarshal([]byte(alert.EvidenceJSON), &ev); err == nil {
			evidenceValues = ev.Values
			evidenceParams = ev.Params
		}
	}

	data := s.baseTemplateData(r, alert.Title, "alerts")
	data["Alert"] = alert
	data["Plant"] = plant
	data["Device"] = devPtr
	data["EvidenceValues"] = evidenceValues
	data["EvidenceParams"] = evidenceParams

	if err := s.render(w, "alert_detail", data); err != nil {
		s.cfg.Logger.Error("render alert detail", "err", err)
	}
}

func (s *Server) handleAlertAck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	alertID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	note := r.PostFormValue("note")
	if err := s.store.AckAlert(ctx, alertID, note); err != nil {
		s.cfg.Logger.Error("ack alert", "id", alertID, "err", err)
		http.Error(w, "Erro ao reconhecer alerta", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		http.Redirect(w, r, "/alerts/"+idStr, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/alerts/"+idStr, http.StatusSeeOther)
}

func (s *Server) handleAlertResolve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	alertID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	note := r.PostFormValue("note")
	if err := s.store.ResolveAlert(ctx, alertID, note); err != nil {
		s.cfg.Logger.Error("resolve alert", "id", alertID, "err", err)
		http.Error(w, "Erro ao resolver alerta", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		http.Redirect(w, r, "/alerts/"+idStr, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/alerts/"+idStr, http.StatusSeeOther)
}
