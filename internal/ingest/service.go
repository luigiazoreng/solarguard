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
	"github.com/luigiazoreng/solarguard/internal/store"
)

// StartAll starts one Runner for every enabled growatt source.
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

		token := os.Getenv(src.CredentialEnv)
		if token == "" {
			if logger != nil {
				logger.Printf("source %q (id %d): missing credential in env var %q, skipping", src.Name, src.ID, src.CredentialEnv)
			}
			continue
		}

		baseURL := "https://openapi.growatt.com"
		var cfg struct {
			BaseURL string `json:"base_url"`
		}
		if err := json.Unmarshal([]byte(src.ConfigJSON), &cfg); err == nil && cfg.BaseURL != "" {
			baseURL = cfg.BaseURL
		}

		conn := growatt.New(baseURL, token, &http.Client{Timeout: 30 * time.Second})
		runner := NewRunner(st, src.ID, conn, WithLogger(logger))

		go func(r *Runner, srcName string) {
			if err := r.Run(ctx); err != nil && logger != nil {
				logger.Printf("runner for source %q exited: %v", srcName, err)
			}
		}(runner, src.Name)
	}

	return nil
}
