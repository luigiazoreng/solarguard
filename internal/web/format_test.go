package web_test

import (
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/web"
)

func TestFormatFloatPtBR(t *testing.T) {
	tests := []struct {
		val      float64
		decimals int
		expected string
	}{
		{1234.56, 2, "1.234,56"},
		{0.0, 2, "0,00"},
		{1000000.5, 1, "1.000.000,5"},
		{-5432.1, 2, "-5.432,10"},
		{42, 0, "42"},
	}

	for _, tc := range tests {
		got := web.FormatFloat(tc.val, tc.decimals)
		if got != tc.expected {
			t.Errorf("FormatFloat(%f, %d) = %q, expected %q", tc.val, tc.decimals, got, tc.expected)
		}
	}
}

func TestFormatDatePtBR(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"2026-10-23", "23/10/2026"},
		{"2026-01-01", "01/01/2026"},
		{"invalid", "invalid"},
	}

	for _, tc := range tests {
		got := web.FormatDate(tc.input)
		if got != tc.expected {
			t.Errorf("FormatDate(%q) = %q, expected %q", tc.input, got, tc.expected)
		}
	}
}

func TestFormatTimePtBR(t *testing.T) {
	tm := time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC)
	// In America/Sao_Paulo (UTC-3), it is 11:30
	got := web.FormatTimeInZone(&tm, "America/Sao_Paulo")
	expected := "23/10/2026 11:30"
	if got != expected {
		t.Errorf("FormatTimeInZone() = %q, expected %q", got, expected)
	}

	if web.FormatTimeInZone(nil, "America/Sao_Paulo") != "-" {
		t.Errorf("expected '-' for nil time")
	}
}
