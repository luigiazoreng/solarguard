// Package mcpserver implements the Model Context Protocol (MCP) server for SolarGuard.
package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/luigiazoreng/solarguard/internal/store"
)

// New creates and configures an MCP server with all tools and prompts registered.
func New(st *store.Store) *mcp.Server {
	// Protocol traffic goes to stdout in stdio mode; logs must only go to stderr.
	stderrLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "solarguard",
		Version: "1.0.0",
		Title:   "SolarGuard Photovoltaic Monitoring MCP Server",
	}, &mcp.ServerOptions{
		Logger: stderrLogger,
	})

	RegisterAllTools(server, st)
	RegisterAllPrompts(server, st)

	return server
}

// ServeStdio runs the MCP server over standard input/output.
// stdout carries only protocol traffic, and logs are directed to stderr.
func ServeStdio(ctx context.Context, st *store.Store) error {
	server := New(st)
	return server.Run(ctx, &mcp.StdioTransport{})
}

// HTTPHandler returns an http.Handler that serves the MCP protocol via the streamable HTTP transport.
// Requests must carry a valid "Authorization: Bearer <token>" header, checked in constant time.
// An empty token causes the constructor to return an error immediately.
func HTTPHandler(st *store.Store, token string) (http.Handler, error) {
	trimmedToken := strings.TrimSpace(token)
	if trimmedToken == "" {
		return nil, errors.New("o token de autenticação não pode ser vazio")
	}

	server := New(st)
	streamable := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless: true,
	})

	expectedBearer := "Bearer " + trimmedToken
	expectedBearerBytes := []byte(expectedBearer)

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authHeader := req.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Unauthorized: missing Authorization header", http.StatusUnauthorized)
			return
		}

		if subtle.ConstantTimeCompare([]byte(authHeader), expectedBearerBytes) != 1 {
			http.Error(w, "Unauthorized: invalid bearer token", http.StatusUnauthorized)
			return
		}

		streamable.ServeHTTP(w, req)
	})

	return handler, nil
}
