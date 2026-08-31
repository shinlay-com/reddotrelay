package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"reddotrelay/internal/auth"
	"reddotrelay/internal/core"
)

func TestAPIKeyLifecycleAndAuthentication(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	createdAt := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	usedAt := createdAt.Add(time.Minute)
	secret, err := auth.GenerateAPIKeySecret()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashAPIKeySecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	key := core.APIKey{
		ID: core.NewConfigID(), Name: "operations", Role: core.APIKeyAdmin,
		Prefix: auth.APIKeyPrefix(secret), CreatedAt: createdAt,
	}
	if err := store.CreateAPIKey(ctx, key, hash); err != nil {
		t.Fatal(err)
	}

	principal, err := store.AuthenticateAPIKey(ctx, secret, usedAt)
	if err != nil {
		t.Fatal(err)
	}
	if principal.ID != key.ID || principal.Name != key.Name || principal.Role != key.Role {
		t.Fatalf("principal = %#v", principal)
	}
	keys, err := store.APIKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].LastUsedAt == nil || !keys[0].LastUsedAt.Equal(usedAt) {
		t.Fatalf("listed keys = %#v, %v", keys, err)
	}
	if keys[0].Prefix != key.Prefix {
		t.Fatalf("listed prefix = %q, want %q", keys[0].Prefix, key.Prefix)
	}

	revokedAt := usedAt.Add(time.Minute)
	if err := store.RevokeAPIKey(ctx, key.ID, revokedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateAPIKey(ctx, secret, revokedAt.Add(time.Minute)); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("authentication after revoke error = %v", err)
	}
	keys, err = store.APIKeys(ctx)
	if err != nil || keys[0].RevokedAt == nil || !keys[0].RevokedAt.Equal(revokedAt) {
		t.Fatalf("revoked key listing = %#v, %v", keys, err)
	}
}

func TestAuthenticateAPIKeyRejectsUnknownAndMalformedSecrets(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	unknown, err := auth.GenerateAPIKeySecret()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"malformed", unknown} {
		if _, err := store.AuthenticateAPIKey(ctx, secret, time.Now()); !errors.Is(err, ErrInvalidAPIKey) {
			t.Fatalf("AuthenticateAPIKey(%q) error = %v", secret, err)
		}
	}
}

func TestCreateAPIKeyRejectsDuplicateNameWithoutExposingSecret(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "reddotrelay.db"))
	t.Cleanup(func() { _ = store.Close() })
	secret, _ := auth.GenerateAPIKeySecret()
	hash, _ := auth.HashAPIKeySecret(secret)
	key := core.APIKey{ID: core.NewConfigID(), Name: "same-name", Role: core.APIKeyReadOnly, Prefix: auth.APIKeyPrefix(secret), CreatedAt: time.Now().UTC()}
	if err := store.CreateAPIKey(ctx, key, hash); err != nil {
		t.Fatal(err)
	}
	key.ID = core.NewConfigID()
	if err := store.CreateAPIKey(ctx, key, hash); err == nil {
		t.Fatal("duplicate CreateAPIKey() error = nil")
	}
	keys, err := store.APIKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys after duplicate = %#v, %v", keys, err)
	}
}
