package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

func init() {
	register("source-add", "cadastra uma nova fonte de dados", runSourceAdd)
}

func runSourceAdd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("uso: solarguard source-add <tipo> --name NAME --base-url URL --token-env ENV_NAME")
	}

	kind := args[0]
	flagArgs := args[1:]
	if strings.HasPrefix(kind, "-") {
		kind = "growatt"
		flagArgs = args
	}

	fs := flag.NewFlagSet("source-add", flag.ContinueOnError)
	var name, baseURL, tokenEnv string
	fs.StringVar(&name, "name", "", "nome único da fonte (obrigatório)")
	fs.StringVar(&baseURL, "base-url", "https://openapi.growatt.com", "URL base da API do provedor")
	fs.StringVar(&tokenEnv, "token-env", "", "nome da variável de ambiente com o token (obrigatório)")

	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	if name == "" {
		return fmt.Errorf("parâmetro --name é obrigatório")
	}
	if tokenEnv == "" {
		return fmt.Errorf("parâmetro --token-env é obrigatório")
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer st.Close()

	cfg := map[string]string{"base_url": baseURL}
	cfgBytes, _ := json.Marshal(cfg)

	src := domain.Source{
		Kind:          kind,
		Name:          name,
		ConfigJSON:    string(cfgBytes),
		CredentialEnv: tokenEnv, // Only the env var's NAME is stored, never the secret
		Enabled:       true,
	}

	created, err := st.EnsureSource(ctx, src)
	if err != nil {
		return fmt.Errorf("salvar fonte: %w", err)
	}

	fmt.Printf("fonte %q (id %d, tipo %s) cadastrada com sucesso\n", created.Name, created.ID, created.Kind)
	return nil
}
