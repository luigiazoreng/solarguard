package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/pipeline"
	"github.com/luigiazoreng/solarguard/internal/store"
)

type SourceStatusItem struct {
	Source          domain.Source
	IsEnvConfigured bool
	Stats           store.QueueStats
}

type DeviceManageRow struct {
	Device    domain.Device
	PlantName string
}

type RuleConfigRow struct {
	Rule       string
	PlantID    int64
	PlantName  string
	Enabled    bool
	ParamsJSON string
}

type PipelineRunRow struct {
	Run       store.PipelineRun
	PlantName string
	Steps     []pipeline.StepOutcome
}

func (s *Server) handleManage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	plants, err := s.store.ListPlants(ctx)
	if err != nil {
		http.Error(w, "Erro ao listar usinas", http.StatusInternalServerError)
		return
	}
	plantMap := make(map[int64]string)
	for _, p := range plants {
		plantMap[p.ID] = p.Name
	}

	// 1. Sources with queue stats & credential status (NO VALUES EXPOSED)
	sources, _ := s.store.ListSources(ctx)
	var sourceStats []SourceStatusItem
	for _, src := range sources {
		stats, _ := s.store.GetQueueStats(ctx, src.ID)
		isConfigured := false
		if src.CredentialEnv != "" && os.Getenv(src.CredentialEnv) != "" {
			isConfigured = true
		}
		sourceStats = append(sourceStats, SourceStatusItem{
			Source:          src,
			IsEnvConfigured: isConfigured,
			Stats:           stats,
		})
	}

	// 2. All devices
	var allDevices []DeviceManageRow
	for _, p := range plants {
		devs, _ := s.store.ListDevices(ctx, p.ID)
		for _, d := range devs {
			allDevices = append(allDevices, DeviceManageRow{
				Device:    d,
				PlantName: p.Name,
			})
		}
	}

	// 3. Rule configs
	knownRules := []string{
		"zero_generation",
		"under_expected",
		"implausible_reading",
		"missing_data",
		"stuck_value",
		"plant_no_generation",
		"gradual_loss",
		"peer_divergence",
	}

	var ruleRows []RuleConfigRow
	for _, p := range plants {
		existingCfgs, _ := s.store.GetRuleConfigs(ctx, p.ID)
		for _, rule := range knownRules {
			cfg, ok := existingCfgs[rule]
			enabled := true
			params := "{}"
			if ok {
				enabled = cfg.Enabled
				params = cfg.ParamsJSON
			}
			ruleRows = append(ruleRows, RuleConfigRow{
				Rule:       rule,
				PlantID:    p.ID,
				PlantName:  p.Name,
				Enabled:    enabled,
				ParamsJSON: params,
			})
		}
	}

	// 4. Latest pipeline runs
	rawRuns, _ := s.store.ListPipelineRuns(ctx, nil, 10)
	var runRows []PipelineRunRow
	for _, r := range rawRuns {
		var steps []pipeline.StepOutcome
		if r.StepsJSON != "" {
			_ = json.Unmarshal([]byte(r.StepsJSON), &steps)
		}
		runRows = append(runRows, PipelineRunRow{
			Run:       r,
			PlantName: plantMap[r.PlantID],
			Steps:     steps,
		})
	}

	successMsg := r.URL.Query().Get("success")
	errorMsg := r.URL.Query().Get("error")

	data := s.baseTemplateData(r, "Gestão e Configurações", "manage")
	data["Plants"] = plants
	data["SourceStats"] = sourceStats
	data["AllDevices"] = allDevices
	data["RuleRows"] = ruleRows
	data["PipelineRuns"] = runRows
	data["SuccessMessage"] = successMsg
	data["ErrorMessage"] = errorMsg

	if err := s.render(w, "manage", data); err != nil {
		s.cfg.Logger.Error("render manage", "err", err)
	}
}

func parseNumberInput(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	s = strings.ReplaceAll(s, ",", ".")
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	return &val, nil
}

func (s *Server) handleManagePlantUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	plantID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	timezone := strings.TrimSpace(r.PostFormValue("timezone"))
	if timezone == "" {
		http.Redirect(w, r, "/manage?error=Fuso+horário+é+obrigatório", http.StatusSeeOther)
		return
	}

	var latPtr, lonPtr *float64
	latStr := r.PostFormValue("latitude")
	lonStr := r.PostFormValue("longitude")

	if strings.TrimSpace(latStr) != "" {
		val, err := parseNumberInput(latStr)
		if err != nil {
			http.Redirect(w, r, "/manage?error=Valor+inválido+para+latitude", http.StatusSeeOther)
			return
		}
		latPtr = val
	}
	if strings.TrimSpace(lonStr) != "" {
		val, err := parseNumberInput(lonStr)
		if err != nil {
			http.Redirect(w, r, "/manage?error=Valor+inválido+para+longitude", http.StatusSeeOther)
			return
		}
		lonPtr = val
	}

	if err := s.store.UpdatePlantDetails(ctx, plantID, timezone, latPtr, lonPtr); err != nil {
		s.cfg.Logger.Error("update plant details", "err", err)
		http.Redirect(w, r, "/manage?error=Erro+ao+atualizar+usina", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/manage?success=Configurações+da+usina+atualizadas", http.StatusSeeOther)
}

func (s *Server) handleManageDeviceUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	deviceID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var capPtr, areaPtr, effPtr *float64

	capStr := r.PostFormValue("capacity_kw")
	if strings.TrimSpace(capStr) != "" {
		val, err := parseNumberInput(capStr)
		if err != nil {
			http.Redirect(w, r, "/manage?error=Valor+inválido+para+capacidade", http.StatusSeeOther)
			return
		}
		capPtr = val
	}

	areaStr := r.PostFormValue("panel_area_m2")
	if strings.TrimSpace(areaStr) != "" {
		val, err := parseNumberInput(areaStr)
		if err != nil {
			http.Redirect(w, r, "/manage?error=Valor+inválido+para+área+do+painel", http.StatusSeeOther)
			return
		}
		areaPtr = val
	}

	effStr := r.PostFormValue("efficiency")
	if strings.TrimSpace(effStr) != "" {
		val, err := parseNumberInput(effStr)
		if err != nil {
			http.Redirect(w, r, "/manage?error=Valor+inválido+para+eficiência", http.StatusSeeOther)
			return
		}
		effPtr = val
	}

	if err := s.store.UpdateDevice(ctx, deviceID, capPtr, areaPtr, effPtr); err != nil {
		s.cfg.Logger.Error("update device", "err", err)
		http.Redirect(w, r, "/manage?error=Erro+ao+atualizar+dispositivo", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/manage?success=Dispositivo+atualizado+com+sucesso", http.StatusSeeOther)
}

func (s *Server) handleManageRuleUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rule := strings.TrimSpace(r.PostFormValue("rule"))
	plantIDStr := strings.TrimSpace(r.PostFormValue("plant_id"))
	plantID, err := strconv.ParseInt(plantIDStr, 10, 64)
	if err != nil || rule == "" {
		http.Redirect(w, r, "/manage?error=Parâmetros+inválidos", http.StatusSeeOther)
		return
	}

	enabled := r.PostFormValue("enabled") == "true"
	paramsJSON := strings.TrimSpace(r.PostFormValue("params_json"))
	if paramsJSON == "" {
		paramsJSON = "{}"
	}

	// Validate JSON params as JSON object
	var obj map[string]any
	if err := json.Unmarshal([]byte(paramsJSON), &obj); err != nil {
		http.Redirect(w, r, fmt.Sprintf("/manage?error=JSON+inválido+para+a+regra+%s:+%s", rule, err.Error()), http.StatusSeeOther)
		return
	}

	cfg := store.RuleConfig{
		Rule:       rule,
		PlantID:    &plantID,
		Enabled:    enabled,
		ParamsJSON: paramsJSON,
	}

	if err := s.store.SetRuleConfig(ctx, cfg); err != nil {
		s.cfg.Logger.Error("set rule config", "err", err)
		http.Redirect(w, r, "/manage?error=Erro+ao+salvar+regra", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/manage?success=Regra+atualizada+com+sucesso", http.StatusSeeOther)
}

func (s *Server) handleManagePipelineRun(w http.ResponseWriter, r *http.Request) {
	plantIDStr := r.PostFormValue("plant_id")

	go func() {
		bgCtx := context.Background()
		deps := pipeline.Deps{
			Logger: s.cfg.Logger,
		}

		if plantIDStr != "" {
			if id, err := strconv.ParseInt(plantIDStr, 10, 64); err == nil && id > 0 {
				_, _ = pipeline.RunPlant(bgCtx, s.store, id, deps)
				return
			}
		}
		_, _ = pipeline.RunAll(bgCtx, s.store, deps)
	}()

	http.Redirect(w, r, "/manage?success=Pipeline+iniciada+em+segundo+plano", http.StatusSeeOther)
}
