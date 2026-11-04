package main

import (
	"context"
	"fmt"

	"github.com/luigiazoreng/solarguard/internal/mcpserver"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func init() {
	register("mcp", "inicia o servidor MCP sobre entrada/saída padrão (stdio)", runMCP)
}

func runMCP(ctx context.Context, args []string) error {
	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco: %w", err)
	}
	defer st.Close()

	return mcpserver.ServeStdio(ctx, st)
}
