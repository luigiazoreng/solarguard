package domain

import (
	"testing"
	"time"
)

func TestLocalDay(t *testing.T) {
	start, end, err := LocalDay("2026-09-22", "America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	wantStart := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 9, 23, 3, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Fatalf("got %s to %s, want %s to %s", start, end, wantStart, wantEnd)
	}
}

func TestLocalDayRejectsBadInput(t *testing.T) {
	if _, _, err := LocalDay("22/09/2026", "America/Sao_Paulo"); err == nil {
		t.Error("expected error for bad date")
	}
	if _, _, err := LocalDay("2026-09-22", "Not/AZone"); err == nil {
		t.Error("expected error for bad time zone")
	}
}
