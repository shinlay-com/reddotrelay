package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAppliesStaticDefaultsAndAllowsNoRPCListeners(t *testing.T) {
	path := writeConfig(t, `
storage:
  path: static.db
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.ListenAddress != ":8080" || cfg.Server.UIDirectory != "/ui" {
		t.Errorf("server defaults = %#v", cfg.Server)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Errorf("log defaults = %#v", cfg.Log)
	}
	if cfg.Delivery.Workers != 4 || cfg.Delivery.BatchSize != 32 || cfg.Delivery.HTTPTimeout != 10*time.Second ||
		cfg.Delivery.LeaseDuration != 30*time.Second || cfg.Delivery.RetryBackoff != time.Second ||
		cfg.Delivery.MaxBackoff != 5*time.Minute || cfg.Delivery.MaxAttempts != 20 || cfg.Delivery.PollInterval != time.Second {
		t.Errorf("delivery defaults = %#v", cfg.Delivery)
	}
	if cfg.Retention != (RetentionConfig{}) {
		t.Errorf("retention should default disabled: %#v", cfg.Retention)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `
server:
  typo: true
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "field typo not found") {
		t.Fatalf("Load() error = %v, want unknown-field error", err)
	}
}

func TestLoadStaticSettings(t *testing.T) {
	path := writeConfig(t, `
server:
  listen_address: "127.0.0.1:9090"
  ui_directory: ./custom-ui
log:
  level: debug
  format: text
storage:
  path: ./data/static.db
delivery:
  workers: 8
  batch_size: 64
  http_timeout: 5s
  lease_duration: 20s
  retry_backoff: 250ms
  max_backoff: 30s
  max_attempts: 12
  poll_interval: 100ms
retention:
  delivered_for: 720h
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ListenAddress != "127.0.0.1:9090" || cfg.Server.UIDirectory != "./custom-ui" || cfg.Log.Level != "debug" || cfg.Log.Format != "text" || cfg.Storage.Path != "./data/static.db" {
		t.Fatalf("static config = %#v", cfg)
	}
	if cfg.Delivery.Workers != 8 || cfg.Delivery.BatchSize != 64 || cfg.Delivery.HTTPTimeout != 5*time.Second ||
		cfg.Delivery.LeaseDuration != 20*time.Second || cfg.Delivery.RetryBackoff != 250*time.Millisecond ||
		cfg.Delivery.MaxBackoff != 30*time.Second || cfg.Delivery.MaxAttempts != 12 || cfg.Delivery.PollInterval != 100*time.Millisecond {
		t.Fatalf("delivery config = %#v", cfg.Delivery)
	}
	if cfg.Retention.DeliveredFor != 720*time.Hour || cfg.Retention.PollInterval != time.Hour || cfg.Retention.BatchSize != 1000 {
		t.Fatalf("retention config = %#v", cfg.Retention)
	}
}

func TestLoadRejectsLegacyRPCListenerFields(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		field    string
	}{
		{"chains", "chains: []\n", "chains"},
		{"global webhook route", "delivery:\n  webhook_urls: [https://example.test/hook]\n", "webhook_urls"},
		{"RPC at top level", "rpc_url: https://rpc.example.test\n", "rpc_url"},
		{"contract at top level", "contracts: []\n", "contracts"},
		{"ABI at top level", "abi: contract.json\n", "abi"},
		{"events at top level", "events: []\n", "events"},
		{"webhooks at top level", "webhooks: []\n", "webhooks"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.contents))
			if err == nil || !strings.Contains(err.Error(), "field "+test.field+" not found") {
				t.Fatalf("Load() error = %v, want removed-field error for %s", err, test.field)
			}
		})
	}
}

func TestLoadStoragePathExpandsEnvironment(t *testing.T) {
	t.Setenv("TEST_DB_PATH", filepath.Join(t.TempDir(), "operations.db"))
	path := writeConfig(t, `
storage:
  path: ${TEST_DB_PATH}
`)
	got, err := LoadStoragePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != os.Getenv("TEST_DB_PATH") {
		t.Fatalf("LoadStoragePath() = %q, want %q", got, os.Getenv("TEST_DB_PATH"))
	}
}

func TestLoadStoragePathRejectsLegacyRPCListenerFields(t *testing.T) {
	path := writeConfig(t, "storage:\n  path: operations.db\nchains: []\n")
	_, err := LoadStoragePath(path)
	if err == nil || !strings.Contains(err.Error(), "field chains not found") {
		t.Fatalf("LoadStoragePath() error = %v, want removed-field error", err)
	}
}

func TestLoadRejectsInvalidStaticSettings(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{"empty listener", "server:\n  listen_address: '  '\n", "server.listen_address is required"},
		{"empty storage path", "storage:\n  path: '  '\n", "storage.path is required"},
		{"invalid delivery", "delivery:\n  workers: -1\n", "delivery settings are invalid"},
		{"short lease", "delivery:\n  http_timeout: 10s\n  lease_duration: 10s\n", "delivery settings are invalid"},
		{"retention options while disabled", "retention:\n  batch_size: 10\n", "retention settings are invalid"},
		{"negative retention", "retention:\n  delivered_for: -1h\n", "retention settings are invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.contents))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCheckedInConfigurationsUseStaticSchema(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "config*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no checked-in configuration files found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := Load(path); err != nil {
				t.Fatalf("Load(%s) error = %v", path, err)
			}
		})
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
