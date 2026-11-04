package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector/growatt"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// ResolveSourceConnector validates credentials and returns a configured Connector for the source.
func ResolveSourceConnector(src domain.Source) (*growatt.Connector, error) {
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

// StartAll starts one Runner and one Scheduler for every enabled growatt source.
// A missing token env var logs a clear error and skips that source without crashing the others.
func StartAll(ctx context.Context, st *store.Store, logger *log.Logger) error {
	sources, err := st.ListSources(ctx)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}

	for _, src := range sources {
		if !src.Enabled || src.Kind != "growatt" {
			continue
		}

		conn, err := ResolveSourceConnector(src)
		if err != nil {
			if logger != nil {
				logger.Printf("source %q (id %d): %v, skipping", src.Name, src.ID, err)
			}
			continue
		}

		runner := NewRunner(st, src.ID, conn, WithLogger(logger))
		scheduler := NewScheduler(st, src.ID, conn, WithSchedulerLogger(logger))

		go func(r *Runner, srcName string) {
			if err := r.Run(ctx); err != nil && logger != nil {
				logger.Printf("runner for source %q exited: %v", srcName, err)
			}
		}(runner, src.Name)

		go func(s *Scheduler, srcName string) {
			if err := s.Run(ctx); err != nil && logger != nil {
				logger.Printf("scheduler for source %q exited: %v", srcName, err)
			}
		}(scheduler, src.Name)
	}

	return nil
}
