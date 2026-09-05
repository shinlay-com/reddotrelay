package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Environment             EnvironmentConfig `yaml:"environment"`
	Server                  ServerConfig      `yaml:"server"`
	Log                     LogConfig         `yaml:"log"`
	Storage                 StorageConfig     `yaml:"storage"`
	Delivery                DeliveryConfig    `yaml:"delivery"`
	Retention               RetentionConfig   `yaml:"retention"`
	Backfill                BackfillConfig    `yaml:"backfill"`
	VerificationConcurrency int               `yaml:"verification_concurrency"`
	Security                SecurityConfig    `yaml:"security"`
}

type BackfillConfig struct {
	MaxRange     uint64        `yaml:"max_range"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

type SecurityConfig struct {
	// RPCCredentialsKeyRef is an env:// or file:// reference whose resolved
	// bytes derive the local AES key for admin-managed RPC credentials.
	RPCCredentialsKeyRef string `yaml:"rpc_credentials_key_ref"`
}

type EnvironmentConfig struct {
	Name string `yaml:"name"`
}

type ServerConfig struct {
	ListenAddress   string `yaml:"listen_address"`
	UIDirectory     string `yaml:"ui_directory"`
	UISecureCookies bool   `yaml:"ui_secure_cookies"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type StorageConfig struct {
	Path string `yaml:"path"`
}

type DeliveryConfig struct {
	Workers       int           `yaml:"workers"`
	BatchSize     int           `yaml:"batch_size"`
	HTTPTimeout   time.Duration `yaml:"http_timeout"`
	LeaseDuration time.Duration `yaml:"lease_duration"`
	RetryBackoff  time.Duration `yaml:"retry_backoff"`
	MaxBackoff    time.Duration `yaml:"max_backoff"`
	MaxAttempts   int           `yaml:"max_attempts"`
	PollInterval  time.Duration `yaml:"poll_interval"`
}

type RetentionConfig struct {
	DeliveredFor time.Duration `yaml:"delivered_for"`
	PollInterval time.Duration `yaml:"poll_interval"`
	BatchSize    int           `yaml:"batch_size"`
}

func Load(path string) (Config, error) {
	cfg, err := decode(path)
	if err != nil {
		return Config{}, err
	}
	applyDeliveryDefaults(&cfg.Delivery)
	applyRetentionDefaults(&cfg.Retention)
	if cfg.Backfill.MaxRange == 0 {
		cfg.Backfill.MaxRange = 100000
	}
	if cfg.Backfill.PollInterval == 0 {
		cfg.Backfill.PollInterval = 500 * time.Millisecond
	}
	if cfg.VerificationConcurrency == 0 {
		cfg.VerificationConcurrency = 8
	}
	cfg.Storage.Path = os.ExpandEnv(cfg.Storage.Path)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadStoragePath reads the SQLite path for offline operational commands while
// enforcing the same static-only YAML schema as service startup.
func LoadStoragePath(path string) (string, error) {
	cfg, err := decode(path)
	if err != nil {
		return "", err
	}
	storagePath := os.ExpandEnv(cfg.Storage.Path)
	if strings.TrimSpace(storagePath) == "" {
		return "", errors.New("storage.path is required")
	}
	return storagePath, nil
}

func decode(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode YAML: %w", err)
	}
	return cfg, nil
}

func defaults() Config {
	return Config{
		Environment: EnvironmentConfig{Name: "Local Engine"},
		Server:      ServerConfig{ListenAddress: ":8080", UIDirectory: "/ui"},
		Log:         LogConfig{Level: "info", Format: "json"},
		Storage:     StorageConfig{Path: "reddotrelay.db"},
	}
}

func applyDeliveryDefaults(delivery *DeliveryConfig) {
	if delivery.Workers == 0 {
		delivery.Workers = 4
	}
	if delivery.BatchSize == 0 {
		delivery.BatchSize = 32
	}
	if delivery.HTTPTimeout == 0 {
		delivery.HTTPTimeout = 10 * time.Second
	}
	if delivery.LeaseDuration == 0 {
		delivery.LeaseDuration = 30 * time.Second
	}
	if delivery.RetryBackoff == 0 {
		delivery.RetryBackoff = time.Second
	}
	if delivery.MaxBackoff == 0 {
		delivery.MaxBackoff = 5 * time.Minute
	}
	if delivery.MaxAttempts == 0 {
		delivery.MaxAttempts = 20
	}
	if delivery.PollInterval == 0 {
		delivery.PollInterval = time.Second
	}
}

func applyRetentionDefaults(retention *RetentionConfig) {
	if retention.DeliveredFor <= 0 {
		return
	}
	if retention.PollInterval == 0 {
		retention.PollInterval = time.Hour
	}
	if retention.BatchSize == 0 {
		retention.BatchSize = 1000
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.ListenAddress) == "" {
		return errors.New("server.listen_address is required")
	}
	if strings.TrimSpace(c.Storage.Path) == "" {
		return errors.New("storage.path is required")
	}
	if c.Delivery.Workers <= 0 || c.Delivery.BatchSize <= 0 || c.Delivery.HTTPTimeout <= 0 ||
		c.Delivery.LeaseDuration <= c.Delivery.HTTPTimeout || c.Delivery.RetryBackoff <= 0 ||
		c.Delivery.MaxBackoff < c.Delivery.RetryBackoff || c.Delivery.MaxAttempts <= 0 || c.Delivery.PollInterval <= 0 {
		return errors.New("delivery settings are invalid")
	}
	if c.Retention.DeliveredFor < 0 || c.Retention.PollInterval < 0 || c.Retention.BatchSize < 0 ||
		(c.Retention.DeliveredFor == 0 && (c.Retention.PollInterval != 0 || c.Retention.BatchSize != 0)) ||
		(c.Retention.DeliveredFor > 0 && (c.Retention.PollInterval <= 0 || c.Retention.BatchSize <= 0)) {
		return errors.New("retention settings are invalid")
	}
	if c.Backfill.MaxRange == 0 || c.Backfill.MaxRange > 10000000 || c.Backfill.PollInterval <= 0 {
		return errors.New("backfill settings are invalid")
	}
	if c.VerificationConcurrency <= 0 || c.VerificationConcurrency > 128 {
		return errors.New("verification_concurrency must be between 1 and 128")
	}
	if ref := strings.TrimSpace(c.Security.RPCCredentialsKeyRef); ref != "" && !(strings.HasPrefix(ref, "env://") || strings.HasPrefix(ref, "file:///")) {
		return errors.New("security.rpc_credentials_key_ref must be an env:// or file:/// reference")
	}
	return nil
}
