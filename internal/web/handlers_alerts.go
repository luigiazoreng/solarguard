package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

type AlertListItem struct {
	Alert      store.Alert
	PlantName  string
	DeviceName string
	DetectedOn string
}

type DayValueEvidence struct {
	Date       string   `json:"date"`
	Value      *float64 `json:"value"`
	Expected   *float64 `json:"expected"`
	PeerMedian *float64 `json:"peer_median"`
	Reason     string   `json:"reason"`
}

func parseDetectedOn(evidenceJSON string, fallback time.Time, loc *time.Location) string {
	if evidenceJSON != "" {
		var ev struct {
			DetectedOn string `json:"detected_on"`
		}
		if err := json.Unmarshal([]byte(evidenceJSON), &ev); err == nil && ev.DetectedOn != "" {
			if t, err := time.Parse(time.DateOnly, ev.DetectedOn); err == nil {
				return t.Format("02/01/2006")
			}
			return ev.DetectedOn
		}
	}
	if !fallback.IsZero() {
		if loc != nil {
			fallback = fallback.In(loc)
		}
		return fallback.Format("02/01/2006")
	}
	return "-"
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
	plantMap := make(map[int64]domain.Plant)
	for _, p := range plants {
		plantMap[p.ID] = p
	}

	var items []AlertListItem
	for _, a := range alerts {
		p := plantMap[a.PlantID]
		pName := p.Name
		dName := ""
		if a.DeviceID != nil {
			if dev, err := s.store.GetDevice(ctx, *a.DeviceID); err == nil {
				dName = dev.Name
			}
		}
		var loc *time.Location
		if p.Timezone != "" {
			loc, _ = time.LoadLocation(p.Timezone)
		}
		detOn := parseDetectedOn(a.EvidenceJSON, a.CreatedAt, loc)
		items = append(items, AlertListItem{
			Alert:      a,
			PlantName:  pName,
			DeviceName: dName,
			DetectedOn: detOn,
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

	loc, err := time.LoadLocation(plant.Timezone)
	if err != nil {
		loc = time.UTC
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
	var detectedOn string
	var totalValues int
	var firstDate string

	if alert.EvidenceJSON != "" {
		var ev struct {
			Params      map[string]any     `json:"params"`
			Values      []DayValueEvidence `json:"values"`
			DetectedOn  string             `json:"detected_on"`
			TotalValues int                `json:"total_values"`
			FirstDate   string             `json:"first_date"`
		}
		if err := json.Unmarshal([]byte(alert.EvidenceJSON), &ev); err == nil {
			evidenceValues = ev.Values
			evidenceParams = ev.Params
			detectedOn = ev.DetectedOn
			totalValues = ev.TotalValues
			firstDate = ev.FirstDate
		}
	}

	if detectedOn != "" {
		if t, err := time.Parse(time.DateOnly, detectedOn); err == nil {
			detectedOn = t.Format("02/01/2006")
		}
	} else if !alert.CreatedAt.IsZero() {
		detectedOn = alert.CreatedAt.In(loc).Format("02/01/2006")
	}

	// Fill the Esperado column from active model predictions when evidence has none
	if len(evidenceValues) > 0 {
		var minDate, maxDate string
		needsPreds := false
		for _, ev := range evidenceValues {
			if ev.Expected == nil {
				needsPreds = true
			}
			if minDate == "" || ev.Date < minDate {
				minDate = ev.Date
			}
			if maxDate == "" || ev.Date > maxDate {
				maxDate = ev.Date
			}
		}

		if needsPreds && minDate != "" && maxDate != "" {
			predsMap, err := s.store.GetActivePredictionsMap(ctx, alert.PlantID, minDate, maxDate)
			if err == nil {
				for i := range evidenceValues {
					if evidenceValues[i].Expected == nil {
						dStr := evidenceValues[i].Date
						if alert.DeviceID != nil {
							if exp, ok := predsMap[*alert.DeviceID][dStr]; ok {
								val := exp
								evidenceValues[i].Expected = &val
							}
						} else {
							var sumExp float64
							var found bool
							for _, devMap := range predsMap {
								if exp, ok := devMap[dStr]; ok {
									sumExp += exp
									found = true
								}
							}
							if found {
								val := sumExp
								evidenceValues[i].Expected = &val
							}
						}
					}
				}
			}
		}
	}

	isCapped := totalValues > len(evidenceValues) && len(evidenceValues) > 0

	// Details for device_offline
	isDeviceOffline := alert.Rule == "device_offline"
	lastContactStr := "-"
	// Whole days elapsed since the last contact. num_days in the evidence counts
	// calendar dates inclusively, which overstates the silence by up to a day.
	daysOffline := 0
	if isDeviceOffline {
		var lastContact time.Time
		if devPtr != nil && devPtr.LastSeenAt != nil {
			lastContact = *devPtr.LastSeenAt
			lastContactStr = lastContact.In(loc).Format("02/01/2006 15:04")
		} else if t, err := time.ParseInLocation(time.DateOnly, firstDate, loc); err == nil {
			lastContact = t
			lastContactStr = t.Format("02/01/2006")
		}
		if !lastContact.IsZero() {
			daysOffline = int(s.cfg.Now().Sub(lastContact).Hours() / 24)
		}
	}

	data := s.baseTemplateData(r, alert.Title, "alerts")
	data["Alert"] = alert
	data["Plant"] = plant
	data["Device"] = devPtr
	data["DetectedOn"] = detectedOn
	data["EvidenceValues"] = evidenceValues
	data["EvidenceParams"] = evidenceParams
	data["IsCapped"] = isCapped
	data["TotalValues"] = totalValues
	data["IsDeviceOffline"] = isDeviceOffline
	data["LastContactStr"] = lastContactStr
	data["DaysOffline"] = daysOffline
	data["OfflineDisclaimer"] = "Não indica a causa: o inversor pode estar funcionando com o coletor de dados desligado."

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

	http.Redirect(w, r, "/alerts/"+idStr, http.StatusSeeOther)
}
