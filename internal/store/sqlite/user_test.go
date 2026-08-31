package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestInitialAdminAndPasswordAuthentication(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	has, err := store.HasUsers(ctx)
	if err != nil || has {
		t.Fatalf("initial users = %v, %v", has, err)
	}
	created, err := store.CreateInitialAdmin(ctx, "local-admin", "correct horse battery", now)
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "local-admin" || created.Role != "admin" {
		t.Fatalf("created = %#v", created)
	}
	if _, err := store.CreateInitialAdmin(ctx, "other-admin", "another secure password", now); !errors.Is(err, ErrUserSetupComplete) {
		t.Fatalf("second setup = %v", err)
	}
	if _, err := store.AuthenticateUser(ctx, "local-admin", "wrong password", now); !errors.Is(err, ErrInvalidUserCredentials) {
		t.Fatalf("wrong password = %v", err)
	}
	principal, err := store.AuthenticateUser(ctx, "LOCAL-ADMIN", "correct horse battery", now)
	if err != nil || principal.ID != created.ID {
		t.Fatalf("authenticate = %#v, %v", principal, err)
	}
}
