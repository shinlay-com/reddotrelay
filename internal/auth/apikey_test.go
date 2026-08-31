package auth

import (
	"strings"
	"testing"
)

func TestGenerateAPIKeySecret(t *testing.T) {
	first, err := GenerateAPIKeySecret()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateAPIKeySecret()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "api_key_") {
		t.Fatalf("generated secrets = %q and %q", first, second)
	}
	if _, err := HashAPIKeySecret(first); err != nil {
		t.Fatalf("HashAPIKeySecret() error = %v", err)
	}
	if prefix := APIKeyPrefix(first); prefix == first || !strings.HasPrefix(prefix, "api_key_") {
		t.Fatalf("display prefix = %q", prefix)
	}
}

func TestHashAPIKeySecretRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"", "wrong_prefix", "api_key_short", "api_key_!!!!!!!!!!!!!!!!"} {
		if _, err := HashAPIKeySecret(value); err == nil {
			t.Fatalf("HashAPIKeySecret(%q) error = nil", value)
		}
	}
}
