// Command solarguard runs the SolarGuard service and its maintenance tasks.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	_ "time/tzdata" // plants have their own time zones; servers may not ship the database

	"github.com/luigiazoreng/solarguard/internal/store"
)

type command struct {
	summary string
	run     func(ctx context.Context, args []string) error
}

var commands = map[string]command{}

// register adds a subcommand. Each subcommand lives in its own cmd_<name>.go.
func register(name, summary string, run func(ctx context.Context, args []string) error) {
	commands[name] = command{summary: summary, run: run}
}

// dbPath returns the configured database path.
func dbPath() string {
	if v := os.Getenv("SOLARGUARD_DB"); v != "" {
		return v
	}
	return "./data/solarguard.db"
}

func init() {
	register("migrate", "cria ou atualiza o banco de dados", func(ctx context.Context, _ []string) error {
		s, err := store.Open(ctx, dbPath())
		if err != nil {
			return err
		}
		defer s.Close()
		fmt.Println("banco pronto:", dbPath())
		return nil
	})
}

func usage() {
	fmt.Fprintln(os.Stderr, "uso: solarguard <comando> [opções]")
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(os.Stderr, "  %-12s %s\n", name, commands[name].summary)
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.run(ctx, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}
