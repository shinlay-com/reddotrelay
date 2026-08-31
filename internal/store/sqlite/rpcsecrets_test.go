package sqlite

import (
	"bytes"
	"testing"
)

func TestRPCSecretsRequireKeyAndRoundTripEncrypted(t *testing.T) {
	store := &Store{}
	if _, err := store.sealRPCSecret("signature"); err == nil {
		t.Fatal("seal without an operator key succeeded")
	}
	if err := store.ConfigureRPCSecrets([]byte("operator-key-one")); err != nil {
		t.Fatal(err)
	}
	sealed, err := store.sealRPCSecret("signature")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("signature")) {
		t.Fatalf("sealed credential contains plaintext: %q", sealed)
	}
	plain, err := store.openRPCSecret(sealed)
	if err != nil || plain != "signature" {
		t.Fatalf("open = %q, %v", plain, err)
	}

	wrongKey := &Store{}
	if err := wrongKey.ConfigureRPCSecrets([]byte("operator-key-two")); err != nil {
		t.Fatal(err)
	}
	if _, err := wrongKey.openRPCSecret(sealed); err == nil {
		t.Fatal("credential decrypted with the wrong operator key")
	}
}
