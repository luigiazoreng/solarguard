package store

import (
	"context"
	"strings"
	"testing"
)

func TestPlantNameOverrideSurvivesDiscovery(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	p, _ := seed(t, s, "A")
	p.Name = "Usina Exempl�"
	if _, err := s.UpsertPlant(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePlantDetails(ctx, p.ID, "Usina Exemplo", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Discovery must still refresh the provider name, without changing the correction.
	for _, providerName := range []string{"Usina Exempl�", "Usina Renomeada"} {
		p.Name = providerName
		upserted, err := s.UpsertPlant(ctx, p)
		if err != nil || upserted.Name != "Usina Exemplo" {
			t.Fatalf("upsert name=%q err=%v", upserted.Name, err)
		}
		var rawName string
		if err := s.db.QueryRowContext(ctx, `SELECT name FROM plants WHERE id = ?`, p.ID).Scan(&rawName); err != nil || rawName != providerName {
			t.Fatalf("provider name=%q err=%v", rawName, err)
		}
		got, err := s.GetPlant(ctx, p.ID)
		if err != nil || got.Name != "Usina Exemplo" {
			t.Fatalf("get name=%q err=%v", got.Name, err)
		}
		bySource, err := s.GetPlantByExternalID(ctx, p.SourceID, p.ExternalID)
		if err != nil || bySource.Name != "Usina Exemplo" {
			t.Fatalf("external lookup name=%q err=%v", bySource.Name, err)
		}
	}
	// Empty edits keep the correction; ordering must use the effective name.
	if err := s.UpdatePlantDetails(ctx, p.ID, "", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	other, _ := seed(t, s, "B")
	if err := s.UpdatePlantDetails(ctx, other.ID, "Usina Z", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, sourceOnly := range []bool{false, true} {
		plants, err := s.ListPlants(ctx)
		if sourceOnly {
			plants, err = s.ListPlantsBySource(ctx, p.SourceID)
		}
		if err != nil || len(plants) != 2 || plants[0].ID != p.ID || plants[0].Name != "Usina Exemplo" || plants[1].Name != "Usina Z" {
			t.Fatalf("sourceOnly=%v plants=%v err=%v", sourceOnly, plants, err)
		}
	}
}

func TestPlantNameOverrideLength(t *testing.T) {
	s := newStore(t)
	p, _ := seed(t, s, "A")
	ctx := context.Background()
	valid := strings.Repeat("á", 120)
	if err := s.UpdatePlantDetails(ctx, p.ID, valid, "", nil, nil); err != nil {
		t.Fatalf("120 characters must be accepted: %v", err)
	}
	if err := s.UpdatePlantDetails(ctx, p.ID, valid+"á", "", nil, nil); err == nil {
		t.Fatal("121 characters must be refused")
	}
	got, err := s.GetPlant(ctx, p.ID)
	if err != nil || got.Name != valid {
		t.Fatalf("refused edit changed name: %q err=%v", got.Name, err)
	}
}
