package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reddotrelay/internal/config"
	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

func TestDeadLetterListAndRequeueCLI(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "reddotrelay.db")
	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(configPath, []byte("storage:\n  path: "+databasePath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := core.EventID{ChainID: 1, TransactionHash: "0xabc", LogIndex: 7}
	event := core.Event{ID: id, BlockNumber: 10, BlockHash: "0xblock", Address: "0xcontract", Name: "Transfer", DecodedPayload: []byte(`{"value":"1"}`), ObservedAt: now}
	delivery := core.Delivery{EventID: id, Destination: "https://example.test/hook", NextAttempt: now}
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, core.Checkpoint{ChainID: 1, BlockNumber: 10, BlockHash: "0xblock"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if err := store.MarkDeliveryDead(ctx, id, delivery.Destination, claimed[0].Delivery.LeaseToken, "test failure", 500); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runDeadLetter(ctx, []string{"list", "-config", configPath}, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(core.EventGUID(id))) || !bytes.Contains(output.Bytes(), []byte("test failure")) {
		t.Fatalf("dead-letter list = %s", output.String())
	}
	output.Reset()
	if err := runDeadLetter(ctx, []string{"requeue", "-config", configPath, "-event-id", core.EventGUID(id)}, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("requeued 1")) {
		t.Fatalf("requeue output = %s", output.String())
	}
	store, err = sqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	due, err := store.DueDeliveries(ctx, time.Now().Add(time.Second), 1)
	if err != nil || len(due) != 1 || due[0].Delivery.Attempts != 0 {
		t.Fatalf("requeued delivery = %#v, %v", due, err)
	}
}

func TestAPIKeyCLI_CreateListAndRevoke(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "reddotrelay.db")
	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(configPath, []byte("storage:\n  path: "+databasePath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runAPIKey(ctx, []string{"create", "-config", configPath, "-name", "deployment-console", "-role", "admin"}, &output); err != nil {
		t.Fatal(err)
	}
	createdOutput := output.String()
	if !bytes.Contains(output.Bytes(), []byte("Key ID:")) || !bytes.Contains(output.Bytes(), []byte("Secret: api_key_")) {
		t.Fatalf("create output = %s", createdOutput)
	}
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("create output lines = %q", lines)
	}
	keyID := string(bytes.TrimPrefix(lines[0], []byte("Key ID: ")))
	secret := string(bytes.TrimPrefix(lines[1], []byte("Secret: ")))

	output.Reset()
	if err := runAPIKey(ctx, []string{"list", "-config", configPath}, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(keyID)) || !bytes.Contains(output.Bytes(), []byte(`"role": "admin"`)) {
		t.Fatalf("list output = %s", output.String())
	}
	if bytes.Contains(output.Bytes(), []byte(secret)) {
		t.Fatal("api-key list exposed the plaintext secret")
	}

	output.Reset()
	if err := runAPIKey(ctx, []string{"revoke", "-config", configPath, "-id", keyID}, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("revoked API key "+keyID)) {
		t.Fatalf("revoke output = %s", output.String())
	}
}

func TestDatabaseBackupCLI(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "reddotrelay.db")
	backupPath := filepath.Join(directory, "reddotrelay.backup.db")
	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(configPath, []byte("storage:\n  path: "+databasePath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runDatabase(ctx, []string{"backup", "-config", configPath, "-output", backupPath}, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("created verified SQLite backup")) {
		t.Fatalf("database backup output = %s", output.String())
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatal(err)
	}
	if err := runDatabase(ctx, []string{"backup", "-config", configPath, "-output", backupPath}, &output); err == nil {
		t.Fatal("second database backup error = nil, want overwrite refusal")
	}
	if err := runDatabase(ctx, []string{"restore", "-config", configPath, "-input", backupPath}, &output); err == nil {
		t.Fatal("database restore error = nil, want confirmation requirement")
	}
	if err := runDatabase(ctx, []string{"restore", "-config", configPath, "-input", backupPath, "-confirm-service-stopped"}, &output); err != nil {
		t.Fatal(err)
	}
}

func TestRunServiceStartsWithNoRPCListeners(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	databasePath := filepath.Join(directory, "reddotrelay.db")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenAddress := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	contents := "server:\n  listen_address: " + listenAddress + "\nstorage:\n  path: " + databasePath + "\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runService(ctx, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		response, requestErr := http.Get("http://" + listenAddress + "/readyz")
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case serviceErr := <-result:
			t.Fatalf("runService() exited before readiness: %v", serviceErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not become ready within 10 seconds")
		}
		time.Sleep(25 * time.Millisecond)
	}

	cancel()
	select {
	case serviceErr := <-result:
		if serviceErr != nil {
			t.Fatalf("runService() error = %v", serviceErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("service did not shut down within 10 seconds")
	}
}

func TestRetentionCLIRequiresPreviewAndConfirmation(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "reddotrelay.db")
	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(configPath, []byte("storage:\n  path: "+databasePath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := core.EventID{ChainID: 1, TransactionHash: "0xretained", LogIndex: 1}
	event := core.Event{ID: id, BlockNumber: 10, BlockHash: "0xblock", Address: "0xcontract", Name: "Transfer", ObservedAt: now}
	delivery := core.Delivery{EventID: id, Destination: "https://example.test/hook", NextAttempt: now}
	if err := store.SaveEventsAndCheckpoint(ctx, []core.Event{event}, []core.Delivery{delivery}, core.Checkpoint{ChainID: 1, BlockNumber: 10, BlockHash: "0xblock"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueDeliveries(ctx, now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if err := store.MarkDeliveryDelivered(ctx, id, delivery.Destination, claimed[0].Delivery.LeaseToken, now.Add(-100*24*time.Hour), 204); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	args := []string{"prune", "-config", configPath, "-older-than", "2160h"}
	if err := runRetention(ctx, args, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("1 delivered event(s) eligible")) {
		t.Fatalf("retention preview = %s", output.String())
	}
	store, err = sqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := store.CountDeliveredBefore(ctx, time.Now().UTC().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if eligible != 1 {
		t.Fatalf("eligible events after preview = %d, want 1", eligible)
	}

	output.Reset()
	if err := runRetention(ctx, append(args, "-confirm"), &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("pruned 1 delivered event(s)")) {
		t.Fatalf("retention prune = %s", output.String())
	}
}
