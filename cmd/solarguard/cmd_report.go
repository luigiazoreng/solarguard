package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/luigiazoreng/solarguard/internal/reports"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func init() {
	register("report", "gera relatório determinístico de geração de uma usina", runReport)
}

func runReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	plantID := fs.Int64("plant-id", 0, "identificador da usina (obrigatório)")
	fromStr := fs.String("from", "", "data inicial (YYYY-MM-DD, obrigatório)")
	toStr := fs.String("to", "", "data final (YYYY-MM-DD, obrigatório)")
	kind := fs.String("kind", "period", "tipo do relatório (weekly, monthly, period)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *plantID <= 0 {
		return errors.New("a opção --plant-id é obrigatória e deve ser maior que zero")
	}
	if *fromStr == "" || *toStr == "" {
		return errors.New("as opções --from e --to são obrigatórias (formato YYYY-MM-DD)")
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer st.Close()

	rep, err := reports.Generate(ctx, st, reports.Request{
		PlantID: *plantID,
		Kind:    *kind,
		From:    *fromStr,
		To:      *toStr,
	})
	if err != nil {
		return fmt.Errorf("gerar relatório: %w", err)
	}

	fmt.Printf("ID: %d\n\n%s\n", rep.ID, rep.BodyMD)
	return nil
}
