package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/luigiazoreng/solarguard/internal/ingest"
	"github.com/luigiazoreng/solarguard/internal/pipeline"
	"github.com/luigiazoreng/solarguard/internal/secrets"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/web"
)

func init() {
	register("serve", "inicia o servidor web, ingestão e pipeline de agendamento", runServe)
}

// allowSignupFromEnv reads SOLARGUARD_ALLOW_SIGNUP. Signup is open by default,
// because a new installation needs its first account. A typo must not leave it
// open by accident, so an invalid value stops the server.
func allowSignupFromEnv() (bool, error) {
	value := os.Getenv("SOLARGUARD_ALLOW_SIGNUP")
	if value == "" {
		return true, nil
	}
	allow, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("SOLARGUARD_ALLOW_SIGNUP deve ser true ou false, recebido %q", value)
	}
	return allow, nil
}

func runServe(ctx context.Context, args []string) error {
	defaultAddr := os.Getenv("SOLARGUARD_ADDR")
	if defaultAddr == "" {
		defaultAddr = "127.0.0.1:8080"
	}

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "endereço de escuta do servidor HTTP (ex: 127.0.0.1:8080)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	mcpToken := os.Getenv("SOLARGUARD_MCP_TOKEN")

	allowSignup, err := allowSignupFromEnv()
	if err != nil {
		return err
	}

	// Without the key, no account could save or read a token.
	box, err := secrets.FromEnv()
	if err != nil {
		return fmt.Errorf("%w; gere uma com: openssl rand -base64 32", err)
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer st.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	stdLogger := log.New(os.Stderr, "[ingest] ", log.LstdFlags)
	collectors := ingest.NewManager(ctx, st, box, stdLogger, ingest.WithGrowattBaseURL(os.Getenv("SOLARGUARD_GROWATT_BASE_URL")))
	defer collectors.Wait()

	cfg := web.Config{
		MCPToken:     mcpToken,
		TrustProxy:   os.Getenv("SOLARGUARD_TRUST_PROXY") == "true",
		SignupClosed: !allowSignup,
		Store:        st,
		Tokens:       collectors,
		Logger:       logger,
	}

	srv, err := web.NewServer(cfg)
	if err != nil {
		return fmt.Errorf("iniciar servidor web: %w", err)
	}

	// 1. Start ingest service
	if err := collectors.StartAll(); err != nil {
		logger.Warn("ingest StartAll avisos", "err", err)
	}

	// 2. Start pipeline background scheduler
	pipelineDeps := pipeline.Deps{
		Logger: logger,
	}
	sched := pipeline.NewScheduler(st, pipelineDeps)
	go sched.Start(ctx)

	// 3. Start HTTP server with timeouts
	httpSrv := &http.Server{
		Addr:         *addr,
		Handler:      srv.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("SolarGuard web server iniciado", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-ctx.Done():
		logger.Info("desligando servidor SolarGuard...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-serverErr:
		return err
	}
}
