package secrets_test

import (
	"strings"
	"testing"

	"github.com/luigiazoreng/solarguard/internal/secrets"
)

const key = "chave-de-teste-com-mais-de-32-caracteres"

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("token-da-growatt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "token-da-growatt") {
		t.Fatal("sealed value contains the plaintext")
	}
	got, err := box.Open(sealed)
	if err != nil || got != "token-da-growatt" {
		t.Fatalf("Open = %q, %v", got, err)
	}

	again, err := box.Seal("token-da-growatt")
	if err != nil {
		t.Fatal(err)
	}
	if again == sealed {
		t.Error("two seals of the same token are identical: the nonce is being reused")
	}
}

func TestOpenRejectsWrongKeyAndTampering(t *testing.T) {
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("token")
	if err != nil {
		t.Fatal(err)
	}

	other, err := secrets.New("outra-chave-de-teste-com-mais-de-32-caracteres")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Error("opened with the wrong key")
	}

	tampered := "AAAA" + sealed[4:]
	if _, err := box.Open(tampered); err == nil {
		t.Error("opened a tampered value")
	}
	if _, err := box.Open("curto"); err == nil {
		t.Error("opened a truncated value")
	}
}

func TestNewRejectsShortKey(t *testing.T) {
	if _, err := secrets.New("curta"); err == nil {
		t.Error("accepted a short key")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(secrets.KeyEnv, "")
	if _, err := secrets.FromEnv(); err != secrets.ErrNoKey {
		t.Errorf("unset key: err = %v, want ErrNoKey", err)
	}
	t.Setenv(secrets.KeyEnv, key)
	if _, err := secrets.FromEnv(); err != nil {
		t.Errorf("valid key: %v", err)
	}
}
