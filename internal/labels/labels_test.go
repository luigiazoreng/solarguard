package labels_test

import (
	"testing"

	"github.com/luigiazoreng/solarguard/internal/labels"
)

func TestLabels(t *testing.T) {
	if got := labels.Rule("stuck_value"); got != "Leitura travada" {
		t.Errorf("Rule(stuck_value) = %q, want %q", got, "Leitura travada")
	}
	if got := labels.Reason("zero_sequence"); got != "sequência de zeros" {
		t.Errorf("Reason(zero_sequence) = %q, want %q", got, "sequência de zeros")
	}
	if got := labels.Severity("critical"); got != "crítico" {
		t.Errorf("Severity(critical) = %q, want %q", got, "crítico")
	}
	if got := labels.Status("open"); got != "aberto" {
		t.Errorf("Status(open) = %q, want %q", got, "aberto")
	}
	if got := labels.PipelineStep("observed_weather"); got != "clima observado" {
		t.Errorf("PipelineStep(observed_weather) = %q, want %q", got, "clima observado")
	}
	if got := labels.Rule("<script>"); got != "&lt;script&gt;" {
		t.Errorf("Rule(<script>) = %q, want %q", got, "&lt;script&gt;")
	}
}
