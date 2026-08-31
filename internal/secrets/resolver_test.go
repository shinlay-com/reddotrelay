package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestResolverEnvironmentReference(t *testing.T) {
	r := &Resolver{lookupEnv: func(name string) (string, bool) {
		if name != "RPC_URL" {
			t.Fatalf("unexpected name %q", name)
		}
		return "https://rpc.example/token", true
	}}
	got, err := r.Resolve(context.Background(), "env://RPC_URL")
	if err != nil || got != "https://rpc.example/token" {
		t.Fatalf("Resolve() = %q, %v", got, err)
	}
}

func TestResolverFileReferenceTrimsMountedSecretNewline(t *testing.T) {
	r := &Resolver{readFile: func(string) ([]byte, error) { return []byte("secret\r\n"), nil }}
	got, err := r.Resolve(context.Background(), "file:///run/secrets/webhook")
	if err != nil || got != "secret" {
		t.Fatalf("Resolve() = %q, %v", got, err)
	}
}

func TestResolverErrorsDoNotDiscloseReference(t *testing.T) {
	reference := "file:///very/sensitive/customer/path"
	r := &Resolver{readFile: func(string) ([]byte, error) { return nil, errors.New("permission denied") }}
	_, err := r.Resolve(context.Background(), reference)
	if err == nil || err.Error() != "file secret is unavailable" {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestValidateReference(t *testing.T) {
	for _, value := range []string{"env://", "env://BAD-NAME", "file://relative", "vault://key"} {
		if ValidateReference(value) == nil {
			t.Errorf("ValidateReference(%q) succeeded", value)
		}
	}
	for _, value := range []string{"env://VALID_NAME", "file:///run/secrets/key"} {
		if err := ValidateReference(value); err != nil {
			t.Errorf("ValidateReference(%q): %v", value, err)
		}
	}
}
