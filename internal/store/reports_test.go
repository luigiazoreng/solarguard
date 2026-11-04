package store

import (
	"context"
	"testing"
)

func TestReportsRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	p, _ := seed(t, s, "A")
	r, err := s.InsertReport(ctx, Report{PlantID: &p.ID, Kind: "weekly", PeriodStart: "2026-01-01", PeriodEnd: "2026-01-07", Author: "ai", Title: "T", BodyMD: "# T"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "draft" {
		t.Fatalf("status %q", r.Status)
	}
	if err := s.FinalizeReport(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetReport(ctx, r.ID)
	if err != nil || got.Status != "final" || *got.PlantID != p.ID {
		t.Fatalf("got %+v err %v", got, err)
	}
	list, err := s.ListReports(ctx, ReportFilter{PlantID: &p.ID, Author: "ai", Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("list %d err %v", len(list), err)
	}
	if _, err := s.GetReport(ctx, 999); err != ErrNotFound {
		t.Fatalf("got %v", err)
	}
	if err := s.FinalizeReport(ctx, 999); err != ErrNotFound {
		t.Fatalf("got %v", err)
	}
}
