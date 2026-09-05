package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"reddotrelay/internal/auth"
	"reddotrelay/internal/config"
	"reddotrelay/internal/core"
	"reddotrelay/internal/delivery"
	"reddotrelay/internal/logging"
	"reddotrelay/internal/observability"
	retentionservice "reddotrelay/internal/retention"
	"reddotrelay/internal/scanner"
	"reddotrelay/internal/secrets"
	"reddotrelay/internal/store/sqlite"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
)

var version = "dev"
var commit = "unknown"
var buildDate = "unknown"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "reddotrelay:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "api-key" {
		return runAPIKey(ctx, args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "user" {
		return runUser(ctx, args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "dead-letter" {
		return runDeadLetter(ctx, args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "retention" {
		return runRetention(ctx, args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "database" {
		return runDatabase(ctx, args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "backfill" {
		return runBackfill(ctx, args[1:], os.Stdout)
	}
	flags := flag.NewFlagSet("reddotrelay", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "config.yaml", "path to the YAML configuration file")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *showVersion {
		fmt.Fprintln(os.Stdout, version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger, err := logging.New(cfg.Log)
	if err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	operationalEvents := newOperationalEventBuffer(operationalEventCapacity)
	logger = slog.New(teeLogHandler{primary: logger.Handler(), events: &operationalEventHandler{buffer: operationalEvents}})
	slog.SetDefault(logger)
	return runService(ctx, cfg, logger, operationalEvents)
}

func runDatabase(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("database action is required: backup, restore, status, or optimize")
	}
	action := args[0]
	flags := flag.NewFlagSet("database "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "config.yaml", "path to the YAML configuration file")
	backupPath := flags.String("output", "", "path for a new SQLite backup")
	inputPath := flags.String("input", "", "path to a verified SQLite backup")
	confirm := flags.Bool("confirm-service-stopped", false, "confirm the RedDotRelay service using this database is stopped")
	confirmOptimize := flags.Bool("confirm", false, "confirm database optimization")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected database arguments")
	}
	storagePath, err := config.LoadStoragePath(*configPath)
	if err != nil {
		return fmt.Errorf("load storage configuration: %w", err)
	}
	switch action {
	case "status":
		store, openErr := sqlite.Open(ctx, storagePath)
		if openErr != nil {
			return openErr
		}
		defer store.Close()
		status, statusErr := store.StorageStatus(ctx)
		if statusErr != nil {
			return statusErr
		}
		return json.NewEncoder(output).Encode(status)
	case "optimize":
		if !*confirmOptimize {
			return errors.New("database optimize requires --confirm")
		}
		store, openErr := sqlite.Open(ctx, storagePath)
		if openErr != nil {
			return openErr
		}
		defer store.Close()
		if optimizeErr := store.Optimize(ctx); optimizeErr != nil {
			return optimizeErr
		}
		_, err = fmt.Fprintln(output, "SQLite WAL checkpoint and optimize completed")
		return err
	case "backup":
		if strings.TrimSpace(*backupPath) == "" {
			return errors.New("output is required")
		}
		if strings.TrimSpace(*inputPath) != "" || *confirm {
			return errors.New("input and confirm-service-stopped are not accepted for database backup")
		}
		if err := sqlite.Backup(ctx, storagePath, *backupPath); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "created verified SQLite backup %s\n", *backupPath)
		return err
	case "restore":
		if strings.TrimSpace(*inputPath) == "" {
			return errors.New("input is required")
		}
		if strings.TrimSpace(*backupPath) != "" {
			return errors.New("output is not accepted for database restore")
		}
		if !*confirm {
			return errors.New("database restore requires -confirm-service-stopped")
		}
		if err := sqlite.Restore(ctx, *inputPath, storagePath); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "restored verified SQLite backup %s\n", *inputPath)
		return err
	default:
		return fmt.Errorf("unknown database action %q: use backup or restore", action)
	}
}

type apiKeyRecord struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Role       string  `json:"role"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
	RevokedAt  *string `json:"revokedAt"`
}

func runAPIKey(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("api-key action is required: create, list, or revoke")
	}
	action := args[0]
	flags := flag.NewFlagSet("api-key "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "config.yaml", "path to the YAML configuration file")
	name := flags.String("name", "", "human-readable API key name")
	role := flags.String("role", string(core.APIKeyReadOnly), "API key role: admin or read-only")
	id := flags.String("id", "", "API key UUID")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected api-key arguments")
	}
	storagePath, err := config.LoadStoragePath(*configPath)
	if err != nil {
		return fmt.Errorf("load storage configuration: %w", err)
	}
	store, err := sqlite.Open(ctx, storagePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	switch action {
	case "create":
		if *name == "" {
			return errors.New("name is required")
		}
		if *id != "" {
			return errors.New("id is not accepted for api-key create")
		}
		keyRole := core.APIKeyRole(*role)
		if keyRole != core.APIKeyAdmin && keyRole != core.APIKeyReadOnly {
			return errors.New("role must be admin or read-only")
		}
		secret, err := auth.GenerateAPIKeySecret()
		if err != nil {
			return fmt.Errorf("generate API key secret: %w", err)
		}
		hash, err := auth.HashAPIKeySecret(secret)
		if err != nil {
			return fmt.Errorf("hash API key secret: %w", err)
		}
		key := core.APIKey{
			ID: core.NewConfigID(), Name: *name, Role: keyRole,
			Prefix: auth.APIKeyPrefix(secret), CreatedAt: time.Now().UTC(),
		}
		if err := store.CreateAPIKey(ctx, key, hash); err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "Key ID: %s\nSecret: %s\n", key.ID, secret)
		return err
	case "list":
		if *name != "" || *id != "" || *role != string(core.APIKeyReadOnly) {
			return errors.New("name, role, and id are not accepted for api-key list")
		}
		keys, err := store.APIKeys(ctx)
		if err != nil {
			return err
		}
		records := make([]apiKeyRecord, len(keys))
		for i, key := range keys {
			records[i] = apiKeyRecord{
				ID: key.ID, Name: key.Name, Role: string(key.Role), Prefix: key.Prefix,
				CreatedAt:  key.CreatedAt.Format(time.RFC3339Nano),
				LastUsedAt: formattedTime(key.LastUsedAt), RevokedAt: formattedTime(key.RevokedAt),
			}
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(records)
	case "revoke":
		if *id == "" {
			return errors.New("id is required")
		}
		if *name != "" || *role != string(core.APIKeyReadOnly) {
			return errors.New("name and role are not accepted for api-key revoke")
		}
		if err := store.RevokeAPIKey(ctx, *id, time.Now().UTC()); err != nil {
			return err
		}
		_, err := fmt.Fprintf(output, "revoked API key %s\n", *id)
		return err
	default:
		return fmt.Errorf("unknown api-key action %q: use create, list, or revoke", action)
	}
}

func formattedTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func runRetention(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 || (args[0] != "prune" && args[0] != "config") {
		return errors.New("retention action is required: prune or config")
	}
	if args[0] == "config" {
		return runRetentionConfig(ctx, args[1:], output)
	}
	flags := flag.NewFlagSet("retention prune", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "config.yaml", "path to the YAML configuration file")
	olderThan := flags.Duration("older-than", 0, "minimum age of delivered records to prune")
	batchSize := flags.Int("batch-size", 500, "events to delete per short transaction")
	maximum := flags.Int64("max-events", 0, "maximum events to delete; zero is unlimited")
	confirm := flags.Bool("confirm", false, "confirm permanent deletion")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected retention arguments")
	}
	if *olderThan <= 0 {
		return errors.New("older-than must be greater than zero")
	}
	if *batchSize <= 0 {
		return errors.New("batch-size must be greater than zero")
	}
	if *maximum < 0 {
		return errors.New("max-events must not be negative")
	}
	storagePath, err := config.LoadStoragePath(*configPath)
	if err != nil {
		return fmt.Errorf("load storage configuration: %w", err)
	}
	store, err := sqlite.Open(ctx, storagePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()
	cutoff := time.Now().UTC().Add(-*olderThan)
	service := retentionservice.New(store)
	if !*confirm {
		preview, previewErr := service.Preview(ctx, cutoff)
		if previewErr != nil {
			return previewErr
		}
		_, err = fmt.Fprintf(output, "%d delivered event(s) eligible before %s; rerun with --confirm to delete\n", preview.Eligible, cutoff.Format(time.RFC3339))
		return err
	}
	result, pruneErr := service.Prune(ctx, cutoff, *batchSize, *maximum, 25*time.Millisecond, func(total int64) {
		if total > 0 && total%int64(*batchSize*10) == 0 {
			_, _ = fmt.Fprintf(output, "pruned %d event(s)…\n", total)
		}
	})
	if pruneErr != nil {
		return pruneErr
	}
	_, err = fmt.Fprintf(output, "pruned %d delivered event(s) before %s in %s\n", result.Deleted, cutoff.Format(time.RFC3339), result.Duration.Round(time.Millisecond))
	return err
}

func runRetentionConfig(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("retention config", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "config.yaml", "path to the YAML configuration file")
	enabled := flags.Bool("enabled", false, "enable automatic retention")
	disabled := flags.Bool("disabled", false, "disable automatic retention")
	delivered := flags.Duration("delivered-for", 0, "retain delivered events for this duration")
	poll := flags.Duration("poll-interval", time.Hour, "automatic cleanup interval")
	batch := flags.Int("batch-size", 1000, "events per cleanup batch")
	confirm := flags.Bool("confirm", false, "confirm changing retention configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected retention arguments")
	}
	if *enabled == *disabled {
		return errors.New("specify exactly one of --enabled or --disabled")
	}
	if !*confirm {
		return errors.New("retention config requires --confirm")
	}
	path, err := config.LoadStoragePath(*configPath)
	if err != nil {
		return err
	}
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	settings := config.RetentionConfig{}
	if *enabled {
		if *delivered <= 0 || *poll <= 0 || *batch <= 0 {
			return errors.New("enabled retention requires positive delivered-for, poll-interval, and batch-size")
		}
		settings = config.RetentionConfig{DeliveredFor: *delivered, PollInterval: *poll, BatchSize: *batch}
	}
	if err := store.SaveRetentionSettings(ctx, settings); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "automatic retention %s\n", map[bool]string{true: "enabled", false: "disabled"}[settings.DeliveredFor > 0])
	return err
}

type deadLetterRecord struct {
	EventID         string `json:"eventId"`
	ChainID         uint64 `json:"chainId"`
	TransactionHash string `json:"transactionHash"`
	LogIndex        uint64 `json:"logIndex"`
	BlockNumber     uint64 `json:"blockNumber"`
	EventName       string `json:"eventName"`
	Destination     string `json:"destination"`
	Attempts        int    `json:"attempts"`
	LastError       string `json:"lastError"`
}

func runDeadLetter(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("dead-letter action is required: list or requeue")
	}
	action := args[0]
	flags := flag.NewFlagSet("dead-letter "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "config.yaml", "path to the YAML configuration file")
	limit := flags.Int("limit", 100, "maximum dead letters to list")
	eventGUID := flags.String("event-id", "", "event GUID to requeue")
	all := flags.Bool("all", false, "requeue every dead letter")
	confirm := flags.Bool("confirm", false, "confirm a bulk requeue")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected dead-letter arguments")
	}
	storagePath, err := config.LoadStoragePath(*configPath)
	if err != nil {
		return fmt.Errorf("load storage configuration: %w", err)
	}
	store, err := sqlite.Open(ctx, storagePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()
	switch action {
	case "list":
		if *limit <= 0 {
			return errors.New("limit must be greater than zero")
		}
		items, err := store.DeadDeliveries(ctx, *limit)
		if err != nil {
			return err
		}
		records := make([]deadLetterRecord, len(items))
		for i, item := range items {
			records[i] = deadLetterRecord{
				EventID: core.EventGUID(item.Event.ID), ChainID: item.Event.ID.ChainID,
				TransactionHash: item.Event.ID.TransactionHash, LogIndex: item.Event.ID.LogIndex,
				BlockNumber: item.Event.BlockNumber, EventName: item.Event.Name,
				Destination: item.Delivery.Destination, Attempts: item.Delivery.Attempts,
				LastError: item.Delivery.LastError,
			}
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(records)
	case "requeue":
		if *all {
			if *eventGUID != "" {
				return errors.New("event-id and all are mutually exclusive")
			}
			if !*confirm {
				return errors.New("bulk requeue requires --confirm")
			}
			count, err := store.RequeueAllDead(ctx, time.Now().UTC())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(output, "requeued %d dead-letter delivery(s)\n", count)
			return err
		}
		if *eventGUID == "" {
			return errors.New("event-id is required unless --all is used")
		}
		count, err := store.RequeueDeadByGUID(ctx, *eventGUID, time.Now().UTC())
		if err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("no dead-letter delivery found for event ID %s", *eventGUID)
		}
		_, err = fmt.Fprintf(output, "requeued %d dead-letter delivery(s) for %s\n", count, *eventGUID)
		return err
	default:
		return fmt.Errorf("unknown dead-letter action %q: use list or requeue", action)
	}
}

func runService(ctx context.Context, cfg config.Config, logger *slog.Logger, eventBuffers ...*operationalEventBuffer) error {
	store, err := sqlite.Open(ctx, cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	secretResolver := secrets.New()
	if reference := strings.TrimSpace(cfg.Security.RPCCredentialsKeyRef); reference != "" {
		key, resolveErr := secretResolver.Resolve(ctx, reference)
		if resolveErr != nil {
			return fmt.Errorf("resolve RPC credential encryption key: %w", resolveErr)
		}
		if configureErr := store.ConfigureRPCSecrets([]byte(key)); configureErr != nil {
			return fmt.Errorf("configure RPC credential encryption: %w", configureErr)
		}
	}
	metrics := observability.New(store, version)
	progress := newScannerProgressTracker()
	operations := newEngineOperations(store, cfg.Retention, logger)
	verificationLimiter := make(chan struct{}, cfg.VerificationConcurrency)
	backfills := &backfillWorker{store: store, cfg: cfg.Backfill, logger: logger, metrics: metrics, verificationConcurrency: cfg.VerificationConcurrency, verificationLimiter: verificationLimiter}
	worker, err := delivery.NewWithObserver(store, delivery.Options{
		Workers: cfg.Delivery.Workers, BatchSize: cfg.Delivery.BatchSize, HTTPTimeout: cfg.Delivery.HTTPTimeout,
		LeaseDuration: cfg.Delivery.LeaseDuration, RetryBackoff: cfg.Delivery.RetryBackoff,
		MaxBackoff: cfg.Delivery.MaxBackoff, MaxAttempts: cfg.Delivery.MaxAttempts, PollInterval: cfg.Delivery.PollInterval,
	}, logger, secretResolver, metrics)
	if err != nil {
		return fmt.Errorf("configure delivery: %w", err)
	}
	manager, err := newRuntimeManagerWithObserver(store, buildScannerRuntimeWithObservers(store, logger, secretResolver, scannerObserverGroup{metrics, progress}, cfg.VerificationConcurrency, verificationLimiter), logger, metrics)
	if err != nil {
		return fmt.Errorf("configure scanner runtime manager: %w", err)
	}

	uiDirectory := strings.TrimSpace(cfg.Server.UIDirectory)
	if uiDirectory != "" {
		if info, statErr := os.Stat(uiDirectory); statErr != nil || !info.IsDir() {
			logger.Warn("management UI disabled; directory unavailable", "ui_directory", uiDirectory)
			uiDirectory = ""
		}
	}
	uiSessions := newUISessionManager(store, cfg.Server.UISecureCookies)
	operationalEvents := newOperationalEventBuffer(operationalEventCapacity)
	if len(eventBuffers) != 0 && eventBuffers[0] != nil {
		operationalEvents = eventBuffers[0]
	}
	server := &http.Server{
		Handler: healthHandlerWithRuntimeOperations(store, manager, metrics.Handler(), uiDirectory, uiSessions, operationalEvents, progress, operations, cfg.Environment, cfg.Backfill, metrics), ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	listener, err := net.Listen("tcp", cfg.Server.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Server.ListenAddress, err)
	}
	defer listener.Close()

	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		if err := manager.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("scanner runtime manager stopped", "error", err)
		}
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		if err := backfills.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("backfill worker stopped")
		}
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("delivery worker stopped", "error", err)
		}
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		_ = runDynamicRetentionWorker(ctx, operations.dynamic, logger, operations.retention)
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server stopped", "error", err)
		}
	}()

	logger.Info("RedDotRelay started", "rpc_listener_source", "sqlite", "listen_address", cfg.Server.ListenAddress)
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown health server", "error", err)
	}
	group.Wait()
	return nil
}

// canonicalRPC preserves the block hash returned by the node. Some
// EVM-compatible networks use a header hashing scheme that differs from
// go-ethereum's local types.Header.Hash calculation.
type canonicalRPC struct {
	*ethclient.Client
}

// HeaderByNumber reads only the canonical fields used by the scanner. Some
// EVM-compatible providers omit optional go-ethereum header fields, while
// number and parent hash remain sufficient for the scanner's chain checks.
func (client *canonicalRPC) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	tag := "latest"
	if number != nil {
		tag = hexutil.EncodeBig(number)
	}
	var block *struct {
		Number     *hexutil.Big `json:"number"`
		ParentHash common.Hash  `json:"parentHash"`
	}
	if err := client.Client.Client().CallContext(ctx, &block, "eth_getBlockByNumber", tag, false); err != nil {
		return nil, err
	}
	if block == nil || block.Number == nil {
		return nil, errors.New("RPC returned an incomplete block header")
	}
	if block.Number.ToInt().Sign() != 0 && block.ParentHash == (common.Hash{}) {
		return nil, errors.New("RPC returned an incomplete block header")
	}
	return &types.Header{Number: block.Number.ToInt(), ParentHash: block.ParentHash}, nil
}

func (client *canonicalRPC) CanonicalHeaderByNumber(ctx context.Context, number *big.Int) (common.Hash, common.Hash, error) {
	tag := "latest"
	if number != nil {
		tag = hexutil.EncodeBig(number)
	}
	var block *struct {
		Number     *hexutil.Big `json:"number"`
		Hash       common.Hash  `json:"hash"`
		ParentHash common.Hash  `json:"parentHash"`
	}
	if err := client.Client.Client().CallContext(ctx, &block, "eth_getBlockByNumber", tag, false); err != nil {
		return common.Hash{}, common.Hash{}, err
	}
	if block == nil || block.Number == nil || block.Hash == (common.Hash{}) {
		return common.Hash{}, common.Hash{}, errors.New("RPC returned an incomplete block header")
	}
	if block.Number.ToInt().Sign() != 0 && block.ParentHash == (common.Hash{}) {
		return common.Hash{}, common.Hash{}, errors.New("RPC returned an incomplete block header")
	}
	return block.Hash, block.ParentHash, nil
}

func (client *canonicalRPC) CanonicalHeadersByNumber(ctx context.Context, numbers []*big.Int) ([]scanner.CanonicalBatchHeader, error) {
	type result struct {
		Number     *hexutil.Big `json:"number"`
		Hash       common.Hash  `json:"hash"`
		ParentHash common.Hash  `json:"parentHash"`
	}
	elements := make([]gethrpc.BatchElem, len(numbers))
	results := make([]result, len(numbers))
	for i, number := range numbers {
		tag := "latest"
		if number != nil {
			tag = hexutil.EncodeBig(number)
		}
		elements[i] = gethrpc.BatchElem{Method: "eth_getBlockByNumber", Args: []any{tag, false}, Result: &results[i]}
	}
	if err := client.Client.Client().BatchCallContext(ctx, elements); err != nil {
		if isUnsupportedBatchError(err) {
			return client.canonicalHeadersByNumberSequential(ctx, numbers)
		}
		return nil, err
	}
	headers := make([]scanner.CanonicalBatchHeader, len(results))
	for i, block := range results {
		if block.Number == nil || block.Hash == (common.Hash{}) {
			return nil, errors.New("RPC returned an incomplete block header")
		}
		if block.Number.ToInt().Sign() != 0 && block.ParentHash == (common.Hash{}) {
			return nil, errors.New("RPC returned an incomplete block header")
		}
		headers[i] = scanner.CanonicalBatchHeader{Number: block.Number.ToInt().Uint64(), Hash: block.Hash, ParentHash: block.ParentHash}
	}
	return headers, nil
}

func (client *canonicalRPC) canonicalHeadersByNumberSequential(ctx context.Context, numbers []*big.Int) ([]scanner.CanonicalBatchHeader, error) {
	headers := make([]scanner.CanonicalBatchHeader, len(numbers))
	for i, number := range numbers {
		hash, parent, err := client.CanonicalHeaderByNumber(ctx, number)
		if err != nil {
			return nil, err
		}
		value := uint64(0)
		if number != nil {
			value = number.Uint64()
		}
		headers[i] = scanner.CanonicalBatchHeader{Number: value, Hash: hash, ParentHash: parent}
	}
	return headers, nil
}

func isUnsupportedBatchError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "batch") && (strings.Contains(message, "not supported") || strings.Contains(message, "unsupported"))
}

func (client *canonicalRPC) BlockHashByNumber(ctx context.Context, number *big.Int) (common.Hash, error) {
	tag := "latest"
	if number != nil {
		tag = hexutil.EncodeBig(number)
	}
	var block *struct {
		Hash common.Hash `json:"hash"`
	}
	if err := client.Client.Client().CallContext(ctx, &block, "eth_getBlockByNumber", tag, false); err != nil {
		return common.Hash{}, err
	}
	if block == nil {
		return common.Hash{}, errors.New("RPC returned a nil block")
	}
	return block.Hash, nil
}

func healthHandler(store *sqlite.Store, managers ...*runtimeManager) http.Handler {
	var manager *runtimeManager
	if len(managers) != 0 {
		manager = managers[0]
	}
	return healthHandlerWithMetrics(store, manager, nil)
}

func healthHandlerWithMetrics(store *sqlite.Store, manager *runtimeManager, metrics http.Handler, uiDirectories ...string) http.Handler {
	uiDirectory := ""
	if len(uiDirectories) != 0 {
		uiDirectory = uiDirectories[0]
	}
	return healthHandlerWithSessions(store, manager, metrics, uiDirectory, newUISessionManager(store, false))
}

func healthHandlerWithSessions(store *sqlite.Store, manager *runtimeManager, metrics http.Handler, uiDirectory string, uiSessions *uiSessionManager, eventBuffers ...*operationalEventBuffer) http.Handler {
	operationalEvents := newOperationalEventBuffer(operationalEventCapacity)
	if len(eventBuffers) != 0 && eventBuffers[0] != nil {
		operationalEvents = eventBuffers[0]
	}
	return healthHandlerWithRuntimeOperations(store, manager, metrics, uiDirectory, uiSessions, operationalEvents, nil)
}

func healthHandlerWithRuntimeOperations(store *sqlite.Store, manager *runtimeManager, metrics http.Handler, uiDirectory string, uiSessions *uiSessionManager, operationalEvents *operationalEventBuffer, progress *scannerProgressTracker, operationSets ...any) http.Handler {
	operations := newEngineOperations(store, config.RetentionConfig{})
	environment := config.EnvironmentConfig{}
	backfillConfig := config.BackfillConfig{MaxRange: 100000}
	var operationalMetrics *observability.Metrics
	for _, option := range operationSets {
		switch value := option.(type) {
		case *engineOperations:
			if value != nil {
				operations = value
			}
		case config.EnvironmentConfig:
			environment = value
		case *observability.Metrics:
			operationalMetrics = value
		case config.BackfillConfig:
			backfillConfig = value
		}
	}
	mux := http.NewServeMux()
	if strings.TrimSpace(uiDirectory) != "" {
		mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/" {
				http.NotFound(writer, request)
				return
			}
			if request.Method != http.MethodGet && request.Method != http.MethodHead {
				writer.Header().Set("Allow", "GET, HEAD")
				writer.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			http.Redirect(writer, request, "/ui/", http.StatusMovedPermanently)
		})
		mux.Handle("/ui/", newUIHandler(uiDirectory))
		mux.HandleFunc("/ui", func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodGet && request.Method != http.MethodHead {
				writer.Header().Set("Allow", "GET, HEAD")
				writer.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			http.Redirect(writer, request, "/ui/", http.StatusMovedPermanently)
		})
	}
	if metrics != nil {
		mux.Handle("/metrics", metrics)
	}
	mux.HandleFunc("/api/v1/ui-session", func(writer http.ResponseWriter, request *http.Request) {
		handleUISession(uiSessions, writer, request)
	})
	mux.HandleFunc("/api/v1/ui-setup", func(writer http.ResponseWriter, request *http.Request) {
		handleUserSetup(store, uiSessions, writer, request)
	})
	mux.Handle("/api/v1/users", authenticateAPIKey(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleUsers(store, w, r) }), uiSessions))
	mux.Handle("/api/v1/users/", authenticateAPIKey(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleUserResource(store, w, r) }), uiSessions))
	mux.Handle("/api/v1/api-keys", authenticateAPIKey(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleAPIKeys(store, w, r) }), uiSessions))
	mux.Handle("/api/v1/api-keys/", authenticateAPIKey(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleAPIKeyResource(store, w, r) }), uiSessions))
	mux.Handle("/api/v1/build-info", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleBuildInfo(environment, writer, request)
	}), uiSessions))
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		writer.Header().Set("Content-Type", "application/json")
		if err := store.Ping(ctx); err != nil {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"status":"unhealthy"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		if store.Ping(ctx) != nil || manager == nil || !manager.Ready() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"status":"not_ready"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"status":"ready"}`))
	})
	mux.Handle("/api/v1/rpc-listeners", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleRPCListenersCollection(store, writer, request)
	}), uiSessions))
	skips := newScannerSkipService(store, manager)
	mux.Handle("/api/v1/rpc-listeners/", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/rpc-listeners/"), "/")
		if len(parts) > 1 && (parts[1] == "skip-to-head" || parts[1] == "skip-audit") {
			skips.handle(writer, request)
			return
		}
		handleRPCListenersResource(store, writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/rpc-listener-audit", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleRPCListenerAudit(store, writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/operational-events", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleOperationalEvents(operationalEvents, writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/events", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { handleEventHistory(store, writer, request) }), uiSessions))
	mux.Handle("/api/v1/events/", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { handleEventDeliveries(store, writer, request) }), uiSessions))
	mux.Handle("/api/v1/deliveries/", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { handleDeliveryRequeue(store, writer, request) }), uiSessions))
	mux.Handle("/api/v1/delivery-requeue-audit", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleDeliveryRequeueAudit(store, writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/backfills", authenticateAPIKey(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleBackfills(store, backfillConfig.MaxRange, w, r) }), uiSessions))
	mux.Handle("/api/v1/backfills/", authenticateAPIKey(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleBackfills(store, backfillConfig.MaxRange, w, r) }), uiSessions))
	mux.Handle("/api/v1/backfill-audit", authenticateAPIKey(store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handleBackfillAudit(store, w, r) }), uiSessions))
	mux.Handle("/api/v1/scanner-progress", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleScannerProgress(progress, writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/operational-summary", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleOperationalSummary(store, operationalMetrics, writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/storage/status", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { handleStorageStatus(store, writer, request) }), uiSessions))
	mux.Handle("/api/v1/storage/optimize", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleStorageOptimize(store, writer, request, operations.logger)
	}), uiSessions))
	mux.Handle("/api/v1/retention/status", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { handleRetention(operations, writer, request) }), uiSessions))
	mux.Handle("/api/v1/retention/preview", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { handleRetention(operations, writer, request) }), uiSessions))
	mux.Handle("/api/v1/retention/prune", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { handleRetention(operations, writer, request) }), uiSessions))
	mux.Handle("/api/v1/retention/config", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { handleRetention(operations, writer, request) }), uiSessions))
	mux.Handle("/api/v1/connection-tests/rpc", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleRPCConnectionTest(secrets.New(), writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/connection-tests/webhook", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleWebhookConnectionTest(secrets.New(), connectionTestHTTPClient(), time.Now, writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/rpc-listener-status", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleRPCListenerStatus(manager, writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/rpc-listener-export", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleRPCListenerExport(store, writer, request)
	}), uiSessions))
	mux.Handle("/api/v1/rpc-listener-import", authenticateAPIKey(store, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handleRPCListenerImport(store, writer, request)
	}), uiSessions))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		writer.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(writer, request)
	})
}

func authenticateAPIKey(store *sqlite.Store, next http.Handler, uiSessions ...*uiSessionManager) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization := strings.TrimSpace(request.Header.Get("Authorization"))
		if authorization != "" {
			parts := strings.Fields(authorization)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				unauthorized(writer)
				return
			}
			principal, err := store.AuthenticateAPIKey(request.Context(), parts[1], time.Now().UTC())
			if err != nil {
				if !errors.Is(err, sqlite.ErrInvalidAPIKey) {
					writeAPIError(writer, http.StatusInternalServerError, "internal server error")
					return
				}
				unauthorized(writer)
				return
			}
			next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), apiKeyPrincipalContextKey{}, principal)))
			return
		}

		if len(uiSessions) == 0 || uiSessions[0] == nil {
			unauthorized(writer)
			return
		}
		_, session, ok := requestUISession(uiSessions[0], writer, request)
		if !ok {
			return
		}
		if request.Method == http.MethodPost || request.Method == http.MethodPut || request.Method == http.MethodPatch || request.Method == http.MethodDelete {
			if !requireSameOrigin(writer, request) {
				return
			}
			if !validCSRFToken(request.Header.Get("X-CSRF-Token"), session.CSRFToken) {
				writeAPIError(writer, http.StatusForbidden, "same-origin CSRF validation failed")
				return
			}
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), apiKeyPrincipalContextKey{}, session.Principal)))
	})
}

func unauthorized(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="reddotrelay-management"`)
	writeAPIError(writer, http.StatusUnauthorized, "unauthorized")
}

func writeAPIError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func revisionETag(revision uint64) string { return fmt.Sprintf(`"revision-%d"`, revision) }

type webhookAPIResponse struct {
	ID             string                            `json:"id"`
	URL            string                            `json:"url,omitempty"`
	URLRef         string                            `json:"urlRef,omitempty"`
	Authentication *webhookAuthenticationAPIResponse `json:"authentication,omitempty"`
	CreatedAt      time.Time                         `json:"createdAt"`
	UpdatedAt      time.Time                         `json:"updatedAt"`
}

type webhookAuthenticationAPIResponse struct {
	Type      string `json:"type"`
	SecretRef string `json:"secretRef"`
	KeyID     string `json:"keyId,omitempty"`
}

type eventAPIResponse struct {
	ID                string               `json:"id"`
	Selector          string               `json:"selector"`
	Webhooks          []webhookAPIResponse `json:"webhooks"`
	EffectiveWebhooks []webhookAPIResponse `json:"effectiveWebhooks"`
	WebhookSource     string               `json:"webhookSource"`
	CreatedAt         time.Time            `json:"createdAt"`
	UpdatedAt         time.Time            `json:"updatedAt"`
}

type contractAPIResponse struct {
	ID        string               `json:"id"`
	Address   string               `json:"address"`
	ABI       json.RawMessage      `json:"abi"`
	Webhooks  []webhookAPIResponse `json:"webhooks"`
	Events    []eventAPIResponse   `json:"events"`
	CreatedAt time.Time            `json:"createdAt"`
	UpdatedAt time.Time            `json:"updatedAt"`
}

type rpcListenerAPIResponse struct {
	ID                string                        `json:"id"`
	Name              string                        `json:"name"`
	Paused            bool                          `json:"paused"`
	ChainID           uint64                        `json:"chainId"`
	RPCURL            string                        `json:"rpcUrl,omitempty"`
	RPCURLRef         string                        `json:"rpcUrlRef,omitempty"`
	RPCAuthentication *rpcAuthenticationAPIResponse `json:"rpcAuthentication,omitempty"`
	StartBlock        uint64                        `json:"startBlock"`
	BatchSize         uint64                        `json:"batchSize"`
	PollInterval      string                        `json:"pollInterval"`
	Confirmations     uint64                        `json:"confirmations"`
	ReorgDepth        uint64                        `json:"reorgDepth"`
	RPCRetryAttempts  int                           `json:"rpcRetryAttempts"`
	RPCRetryBackoff   string                        `json:"rpcRetryBackoff"`
	RPCTimeout        string                        `json:"rpcTimeout"`
	TLS               listenerTLSAPIResponse        `json:"tls"`
	Webhooks          []webhookAPIResponse          `json:"webhooks"`
	Contracts         []contractAPIResponse         `json:"contracts"`
	CreatedAt         time.Time                     `json:"createdAt"`
	UpdatedAt         time.Time                     `json:"updatedAt"`
}

type rpcAuthenticationAPIResponse struct {
	Type                  string `json:"type"`
	Username              string `json:"username,omitempty"`
	HeaderName            string `json:"headerName,omitempty"`
	SecretConfigured      bool   `json:"secretConfigured"`
	TokenURL              string `json:"tokenUrl,omitempty"`
	TokenAPIKeyConfigured bool   `json:"tokenApiKeyConfigured,omitempty"`
}

type listenerTLSAPIResponse struct {
	CAPEM              string `json:"caPem,omitempty"`
	ServerName         string `json:"serverName,omitempty"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify"`
}

func rpcListenerListResponse(snapshot core.RPCListenerSnapshot) map[string]any {
	configs := make([]rpcListenerAPIResponse, 0, len(snapshot.Listeners))
	for _, config := range snapshot.Listeners {
		configs = append(configs, rpcListenerResponse(config, snapshot.GlobalWebhooks))
	}
	return map[string]any{
		"revision":       snapshot.Revision,
		"updatedAt":      snapshot.UpdatedAt,
		"globalWebhooks": webhookResponses(snapshot.GlobalWebhooks),
		"rpcListeners":   configs,
	}
}

func rpcListenerResponse(config core.RPCListener, global []core.WebhookConfig) rpcListenerAPIResponse {
	contracts := make([]contractAPIResponse, 0, len(config.Contracts))
	for _, contract := range config.Contracts {
		events := make([]eventAPIResponse, 0, len(contract.Events))
		for _, event := range contract.Events {
			effective, source := event.Webhooks, "event"
			if len(effective) == 0 {
				effective, source = contract.Webhooks, "contract"
			}
			if len(effective) == 0 {
				effective, source = config.Webhooks, "chain"
			}
			if len(effective) == 0 {
				effective, source = global, "global"
			}
			if len(effective) == 0 {
				source = "none"
			}
			events = append(events, eventAPIResponse{ID: event.ID, Selector: event.Selector, Webhooks: webhookResponses(event.Webhooks), EffectiveWebhooks: webhookResponses(effective), WebhookSource: source, CreatedAt: event.CreatedAt, UpdatedAt: event.UpdatedAt})
		}
		contracts = append(contracts, contractAPIResponse{ID: contract.ID, Address: contract.Address, ABI: contract.ABI, Webhooks: webhookResponses(contract.Webhooks), Events: events, CreatedAt: contract.CreatedAt, UpdatedAt: contract.UpdatedAt})
	}
	response := rpcListenerAPIResponse{
		ID: config.ID, Name: config.Name, Paused: config.Paused, ChainID: config.ChainID, RPCURL: redactRPCURL(config.RPCURL), RPCURLRef: config.RPCURLRef,
		StartBlock: config.StartBlock, BatchSize: config.BatchSize, PollInterval: config.PollInterval.String(),
		Confirmations: config.Confirmations, ReorgDepth: config.ReorgDepth, RPCRetryAttempts: config.RPCRetryAttempts,
		RPCRetryBackoff: config.RPCRetryBackoff.String(), RPCTimeout: config.RPCTimeout.String(),
		TLS:      listenerTLSAPIResponse{CAPEM: config.TLS.CAPEM, ServerName: config.TLS.ServerName, InsecureSkipVerify: config.TLS.InsecureSkipVerify},
		Webhooks: webhookResponses(config.Webhooks), Contracts: contracts, CreatedAt: config.CreatedAt, UpdatedAt: config.UpdatedAt,
	}
	if config.RPCAuthentication.Type != "" {
		auth := &rpcAuthenticationAPIResponse{Type: config.RPCAuthentication.Type, Username: config.RPCAuthentication.Username, HeaderName: config.RPCAuthentication.HeaderName, SecretConfigured: config.RPCAuthentication.Secret != "", TokenURL: config.RPCAuthentication.TokenURL, TokenAPIKeyConfigured: config.RPCAuthentication.TokenAPIKey != ""}
		response.RPCAuthentication = auth
	}
	return response
}

func webhookResponses(webhooks []core.WebhookConfig) []webhookAPIResponse {
	result := make([]webhookAPIResponse, 0, len(webhooks))
	for _, webhook := range webhooks {
		response := webhookAPIResponse{ID: webhook.ID, URL: redactOptionalURL(webhook.URL), URLRef: webhook.URLRef, CreatedAt: webhook.CreatedAt, UpdatedAt: webhook.UpdatedAt}
		if webhook.Authentication.Type != "" {
			response.Authentication = &webhookAuthenticationAPIResponse{Type: webhook.Authentication.Type, SecretRef: webhook.Authentication.SecretRef, KeyID: webhook.Authentication.KeyID}
		}
		result = append(result, response)
	}
	return result
}

func redactRPCURL(raw string) string {
	return redactOptionalURL(raw)
}

func redactOptionalURL(raw string) string {
	if raw == "" {
		return ""
	}
	return redactConfiguredURL(raw)
}

func redactConfiguredURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "[redacted]"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed.String()
}
