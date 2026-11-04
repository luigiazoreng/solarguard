// Package labels maps internal domain codes to friendly Brazilian Portuguese labels.
package labels

import (
	"html"
	"strings"
)

var ruleLabels = map[string]string{
	"stuck_value":         "Leitura travada",
	"zero_generation":     "Geração zerada",
	"implausible_reading": "Leitura implausível",
	"plant_no_generation": "Usina sem geração",
	"missing_data":        "Dados ausentes",
	"peer_divergence":     "Divergência entre dispositivos",
	"under_expected":      "Abaixo do esperado",
	"gradual_loss":        "Perda gradual",
	"stale_data":          "Sem dados recentes",
	"device_offline":      "Sem comunicação",
}

var reasonLabels = map[string]string{
	"stuck_value":                "leitura travada",
	"leitura_travada":            "leitura travada",
	"zero_sequence":              "sequência de zeros",
	"zero_run":                   "sequência de zeros",
	"zero_with_peers":            "zero com pares gerando",
	"zero_with_peers_generating": "zero com pares gerando",
	"zero_without_evidence":      "zero sem evidência",
	"zero_no_peer_evidence":      "zero sem evidência",
	"implausible_value":          "valor implausível",
	"implausible_high":           "valor implausível",
	"implausible_negative":       "negativo",
	"missing_value":              "dados ausentes",
	"negative":                   "negativo",
	"missing_data":               "dados ausentes",
	"dados_ausentes":             "dados ausentes",
	"zero_generation":            "geração zerada",
}

var severityLabels = map[string]string{
	"warning":     "aviso",
	"critical":    "crítico",
	"info":        "informativo",
	"aviso":       "aviso",
	"crítico":     "crítico",
	"informativo": "informativo",
}

var statusLabels = map[string]string{
	"open":         "aberto",
	"ack":          "reconhecido",
	"acknowledged": "reconhecido",
	"resolved":     "resolvido",
	"aberto":       "aberto",
	"reconhecido":  "reconhecido",
	"resolvido":    "resolvido",
}

var pipelineStepLabels = map[string]string{
	"observed_weather": "clima observado",
	"forecast_weather": "previsão do tempo",
	"quality":          "qualidade",
	"forecast":         "modelo",
	"alerts":           "alertas",
}

var paramLabels = map[string]string{
	"baseline":           "dias da referência",
	"critical_days":      "dias até ficar crítico",
	"drop":               "queda mínima",
	"max_gap_days":       "maior intervalo sem dados (dias)",
	"min_baseline_days":  "mínimo de dias na referência",
	"min_days":           "mínimo de dias",
	"min_expected_kwh":   "mínimo esperado (kWh)",
	"ratio":              "razão mínima em relação ao esperado",
	"resolve_after_days": "dias normais para resolver",
	"window":             "dias da janela",
}

// Param returns the friendly Portuguese label for an alert rule parameter.
func Param(name string) string {
	if label, ok := paramLabels[name]; ok {
		return label
	}
	return escape(name)
}

func escape(s string) string {
	return html.EscapeString(strings.TrimSpace(s))
}

// Rule returns the friendly Portuguese label for an alert rule code.
func Rule(code string) string {
	if label, ok := ruleLabels[code]; ok {
		return label
	}
	return escape(code)
}

// Reason returns the friendly Portuguese label for a quality or failure reason code.
func Reason(code string) string {
	if label, ok := reasonLabels[code]; ok {
		return label
	}
	return escape(code)
}

// Severity returns the friendly Portuguese label for an alert severity code.
func Severity(code string) string {
	if label, ok := severityLabels[code]; ok {
		return label
	}
	return escape(code)
}

// Status returns the friendly Portuguese label for an alert status code.
func Status(code string) string {
	if label, ok := statusLabels[code]; ok {
		return label
	}
	return escape(code)
}

// PipelineStep returns the friendly Portuguese label for a pipeline step name.
func PipelineStep(name string) string {
	if label, ok := pipelineStepLabels[name]; ok {
		return label
	}
	return escape(name)
}
