package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector/growatt"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/ingest"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func init() {
	register("discover", "enfileira tarefas de descoberta para uma fonte", runDiscover)
	register("backfill", "enfileira coleta histórica para uma planta", runBackfill)
	register("collect", "executa a fila de coleta e agendamento diário", runCollect)
	register("queue", "exibe o status e estatísticas da fila de coleta", runQueue)
}

func loadGrowattConnector(src domain.Source) (*growatt.Connector, error) {
	if src.CredentialEnv == "" {
		return nil, fmt.Errorf("fonte %q não tem variável de credencial configurada", src.Name)
	}
	token := os.Getenv(src.CredentialEnv)
	if token == "" {
		return nil, fmt.Errorf("variável de ambiente %q não definida", src.CredentialEnv)
	}

	baseURL := "https://openapi.growatt.com"
	var cfg struct {
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal([]byte(src.ConfigJSON), &cfg); err == nil && cfg.BaseURL != "" {
		baseURL = cfg.BaseURL
	}

	return growatt.New(baseURL, token, &http.Client{Timeout: 30 * time.Second}), nil
}

func runDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	var sourceName string
	fs.StringVar(&sourceName, "source", "", "nome da fonte cadastrada (obrigatório)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if sourceName == "" {
		return fmt.Errorf("parâmetro --source é obrigatório")
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer st.Close()

	src, err := st.GetSourceByName(ctx, sourceName)
	if err != nil {
		return fmt.Errorf("buscar fonte %q: %w", sourceName, err)
	}

	conn, err := loadGrowattConnector(src)
	if err != nil {
		return err
	}

	if err := ingest.Discover(ctx, st, src.ID, conn, time.Now()); err != nil {
		return fmt.Errorf("enfileirar descoberta: %w", err)
	}

	fmt.Printf("descoberta enfileirada com sucesso para a fonte %q\n", src.Name)
	return nil
}

func runBackfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	var sourceName, from, to string
	var plantID int64
	fs.StringVar(&sourceName, "source", "", "nome da fonte cadastrada (obrigatório)")
	fs.Int64Var(&plantID, "plant-id", 0, "ID interno da planta (obrigatório)")
	fs.StringVar(&from, "from", "", "data de início YYYY-MM-DD (obrigatório)")
	fs.StringVar(&to, "to", "", "data de fim YYYY-MM-DD (obrigatório)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if sourceName == "" || plantID == 0 || from == "" || to == "" {
		return fmt.Errorf("parâmetros --source, --plant-id, --from e --to são obrigatórios")
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer st.Close()

	src, err := st.GetSourceByName(ctx, sourceName)
	if err != nil {
		return fmt.Errorf("buscar fonte %q: %w", sourceName, err)
	}

	conn, err := loadGrowattConnector(src)
	if err != nil {
		return err
	}

	if err := ingest.Backfill(ctx, st, src.ID, conn, plantID, from, to); err != nil {
		return fmt.Errorf("enfileirar backfill: %w", err)
	}

	fmt.Printf("backfill enfileirado para a planta %d (%s até %s)\n", plantID, from, to)
	return nil
}

func runCollect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	var sourceName string
	fs.StringVar(&sourceName, "source", "", "nome da fonte cadastrada (obrigatório)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if sourceName == "" {
		return fmt.Errorf("parâmetro --source é obrigatório")
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer st.Close()

	src, err := st.GetSourceByName(ctx, sourceName)
	if err != nil {
		return fmt.Errorf("buscar fonte %q: %w", sourceName, err)
	}

	conn, err := loadGrowattConnector(src)
	if err != nil {
		return err
	}

	logger := log.New(os.Stdout, fmt.Sprintf("[%s] ", src.Name), log.LstdFlags)
	runner := ingest.NewRunner(st, src.ID, conn, ingest.WithLogger(logger))

	// Daily scheduling goroutine
	go func() {
		// Run once on startup
		_ = ingest.ScheduleCompletedDays(ctx, st, src.ID, conn, time.Now)

		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = ingest.ScheduleCompletedDays(ctx, st, src.ID, conn, time.Now)
			}
		}
	}()

	logger.Printf("iniciando coletor para fonte %q...", src.Name)
	return runner.Run(ctx)
}

func runQueue(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	var sourceName string
	fs.StringVar(&sourceName, "source", "", "nome da fonte cadastrada (obrigatório)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if sourceName == "" {
		return fmt.Errorf("parâmetro --source é obrigatório")
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer st.Close()

	src, err := st.GetSourceByName(ctx, sourceName)
	if err != nil {
		return fmt.Errorf("buscar fonte %q: %w", sourceName, err)
	}

	stats, err := st.GetQueueStats(ctx, src.ID)
	if err != nil {
		return fmt.Errorf("obter estatísticas da fila: %w", err)
	}

	fmt.Printf("Fila da fonte: %s (id: %d)\n", src.Name, src.ID)
	fmt.Printf("  Pendentes:  %d\n", stats.Pending)
	fmt.Printf("  Concluídas: %d\n", stats.Done)
	fmt.Printf("  Falhas:     %d\n", stats.Failed)

	if stats.NextDue != nil {
		fmt.Printf("  Próxima tarefa devida em: %s\n", stats.NextDue.Format(time.RFC3339))
	} else {
		fmt.Println("  Próxima tarefa devida em: (nenhuma pendente)")
	}

	if len(stats.FailedTasks) > 0 {
		fmt.Println("\nTarefas que falharam permanentemente:")
		for _, f := range stats.FailedTasks {
			fmt.Printf("  - [%d] %s (tentativas: %d): %s\n", f.ID, f.TaskKey, f.Attempts, f.LastError)
		}
	}

	return nil
}
