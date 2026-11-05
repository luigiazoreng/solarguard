package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/luigiazoreng/solarguard/internal/secrets"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/web"
)

func init() {
	register("user-reset-password", "troca a senha de uma conta (a nova senha é lida da entrada padrão)", runUserResetPassword)
	register("source-assign", "entrega uma fonte sem dono, como uma planilha importada, a uma conta", runSourceAssign)
}

// optionalBox returns the token cipher when the key is configured. Sources that
// keep their token in the environment work without it.
func optionalBox() *secrets.Box {
	box, err := secrets.FromEnv()
	if err != nil {
		if !errors.Is(err, secrets.ErrNoKey) {
			fmt.Fprintf(os.Stderr, "aviso: %v\n", err)
		}
		return nil
	}
	return box
}

func runUserResetPassword(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("user-reset-password", flag.ContinueOnError)
	email := fs.String("email", "", "e-mail da conta (obrigatório)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("parâmetro --email é obrigatório")
	}

	fmt.Fprint(os.Stderr, "Nova senha: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("ler senha: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if err := web.ValidatePassword(password); err != nil {
		return err
	}
	hash, err := web.HashPassword(password)
	if err != nil {
		return fmt.Errorf("gerar hash: %w", err)
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer st.Close()

	normalized := web.NormalizeEmail(*email)
	if err := st.SetUserPassword(ctx, normalized, hash); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("não há conta com o e-mail %q", normalized)
		}
		return err
	}
	fmt.Printf("senha da conta %q alterada. Sessões abertas continuam válidas até reiniciar o serviço\n", normalized)
	return nil
}

func runSourceAssign(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("source-assign", flag.ContinueOnError)
	source := fs.String("source", "", "nome da fonte (obrigatório)")
	email := fs.String("email", "", "e-mail da conta (obrigatório)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" || *email == "" {
		return errors.New("parâmetros --source e --email são obrigatórios")
	}

	st, err := store.Open(ctx, dbPath())
	if err != nil {
		return fmt.Errorf("abrir banco de dados: %w", err)
	}
	defer st.Close()

	src, err := st.GetSourceByName(ctx, *source)
	if err != nil {
		return fmt.Errorf("buscar fonte %q: %w", *source, err)
	}
	user, err := st.GetUserByEmail(ctx, web.NormalizeEmail(*email))
	if err != nil {
		return fmt.Errorf("buscar conta %q: %w", *email, err)
	}
	if err := st.ClaimSource(ctx, src.ID, user.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("a fonte %q já tem dono", *source)
		}
		return err
	}
	fmt.Printf("fonte %q entregue à conta %q\n", src.Name, user.Email)
	return nil
}
