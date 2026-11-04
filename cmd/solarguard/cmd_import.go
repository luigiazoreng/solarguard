package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/luigiazoreng/solarguard/internal/fileimport"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func init() {
	register("import-xlsx", "importa leituras de planilha Excel", runImport)
}

func runImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import-xlsx", flag.ContinueOnError)
	file := fs.String("file", "", "caminho para a planilha (.xlsx)")
	source := fs.String("source", "", "nome da fonte de dados")
	plantID := fs.String("plant-id", "", "identificador externo da planta")
	plantName := fs.String("plant-name", "", "nome da planta")
	timezone := fs.String("timezone", "America/Sao_Paulo", "fuso horário da planta")
	semantics := fs.String("stamp-semantics", "previous_day", "semântica do carimbo: previous_day ou same_day")
	areaM2 := fs.Float64("area-m2", 0, "área de painéis por torre em m²")
	eff := fs.Float64("efficiency", 0, "eficiência das placas (ex: 0.18)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *file == "" {
		return fmt.Errorf("parâmetro obrigatório ausente: --file")
	}
	if *source == "" {
		return fmt.Errorf("parâmetro obrigatório ausente: --source")
	}
	if *plantID == "" {
		return fmt.Errorf("parâmetro obrigatório ausente: --plant-id")
	}
	if *plantName == "" {
		return fmt.Errorf("parâmetro obrigatório ausente: --plant-name")
	}

	var areaPtr *float64
	var effPtr *float64
	// Check if flags were explicitly provided on the command line
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "area-m2" {
			v := *areaM2
			areaPtr = &v
		}
		if f.Name == "efficiency" {
			v := *eff
			effPtr = &v
		}
	})

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer st.Close()

	summary, err := fileimport.ImportXLSX(ctx, st, fileimport.Options{
		File:           *file,
		Source:         *source,
		PlantID:        *plantID,
		PlantName:      *plantName,
		Timezone:       *timezone,
		StampSemantics: fileimport.StampSemantics(*semantics),
		AreaM2:         areaPtr,
		Efficiency:     effPtr,
	})
	if err != nil {
		return fmt.Errorf("importação: %w", err)
	}

	fmt.Printf("Linhas lidas: %d\n", summary.Rows)
	fmt.Printf("Linhas inseridas: %d\n", summary.Inserted)
	fmt.Printf("Células de texto: %d\n", summary.TextCells)
	fmt.Printf("Células não analisáveis: %d\n", summary.UnparsableCells)
	fmt.Printf("Dispositivos: %d\n", summary.Devices)
	fmt.Printf("Primeira data: %s\n", summary.FirstDate)
	fmt.Printf("Última data: %s\n", summary.LastDate)

	return nil
}
