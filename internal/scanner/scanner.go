package scanner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	"reddotrelay/internal/core"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const maxRPCBackoff = 30 * time.Second

type RPC interface {
	ChainID(context.Context) (*big.Int, error)
	HeaderByNumber(context.Context, *big.Int) (*types.Header, error)
	BlockHashByNumber(context.Context, *big.Int) (common.Hash, error)
	FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error)
}

type Checkpoints interface {
	Checkpoint(context.Context, uint64) (core.Checkpoint, error)
	ResetFrom(context.Context, uint64, uint64) error
}

// BatchProcessor must durably process all logs and commit the checkpoint in one
// transaction. A decoder/store adapter will implement this boundary later.
type BatchProcessor interface {
	ProcessBatch(context.Context, []core.RawLog, core.Checkpoint) error
}

type Options struct {
	ListenerID    string
	ChainID       uint64
	StartBlock    uint64
	BatchSize     uint64
	Confirmations uint64
	ReorgDepth    uint64
	PollInterval  time.Duration
	RetryAttempts int
	RetryBackoff  time.Duration
	RPCTimeout    time.Duration
	Addresses     []common.Address
	Topics        [][]common.Hash
}

type Observer interface {
	ScanCycle(listenerID string, chainID uint64, outcome string)
	RPCRequest(listenerID string, chainID uint64, operation, outcome string)
	Head(listenerID string, chainID, latest, confirmed uint64)
	BatchCommitted(listenerID string, chainID, checkpoint, confirmed uint64, events int)
	Reorg(listenerID string, chainID uint64)
}

// ErrorObserver is an optional extension used by management surfaces. Error
// details are sanitized before delivery so credential-bearing RPC URLs cannot
// escape through status APIs or logs.
type ErrorObserver interface {
	ScanError(listenerID string, chainID uint64, detail string, at time.Time)
}

type noopObserver struct{}

func (noopObserver) ScanCycle(string, uint64, string)                   {}
func (noopObserver) RPCRequest(string, uint64, string, string)          {}
func (noopObserver) Head(string, uint64, uint64, uint64)                {}
func (noopObserver) BatchCommitted(string, uint64, uint64, uint64, int) {}
func (noopObserver) Reorg(string, uint64)                               {}

type Scanner struct {
	rpc         RPC
	checkpoints Checkpoints
	processor   BatchProcessor
	options     Options
	logger      *slog.Logger
	sleep       func(context.Context, time.Duration) error
	observer    Observer
}

func New(rpc RPC, checkpoints Checkpoints, processor BatchProcessor, options Options, logger *slog.Logger) (*Scanner, error) {
	return NewWithObserver(rpc, checkpoints, processor, options, logger, noopObserver{})
}

func NewWithObserver(rpc RPC, checkpoints Checkpoints, processor BatchProcessor, options Options, logger *slog.Logger, observer Observer) (*Scanner, error) {
	if rpc == nil || checkpoints == nil || processor == nil {
		return nil, errors.New("RPC, checkpoint store, and batch processor are required")
	}
	if options.ChainID == 0 || options.BatchSize == 0 {
		return nil, errors.New("chain ID and batch size must be greater than zero")
	}
	if options.RetryAttempts <= 0 || options.RetryAttempts > 30 || options.RetryBackoff <= 0 || options.RPCTimeout <= 0 || options.PollInterval <= 0 {
		return nil, errors.New("retry attempts must be between 1 and 30; RPC timeout, retry backoff, and poll interval must be greater than zero")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if observer == nil {
		observer = noopObserver{}
	}
	return &Scanner{
		rpc: rpc, checkpoints: checkpoints, processor: processor,
		options: options, logger: logger, sleep: sleepContext, observer: observer,
	}, nil
}

// Run continuously scans confirmed blocks using polling. WebSockets are not
// involved in checkpointing or correctness.
func (s *Scanner) Run(ctx context.Context) error {
	for {
		err := s.ScanOnce(ctx)
		if err != nil {
			s.observer.ScanCycle(s.options.ListenerID, s.options.ChainID, "error")
			if observer, ok := s.observer.(ErrorObserver); ok {
				observer.ScanError(s.options.ListenerID, s.options.ChainID, safeScanError(err), time.Now().UTC())
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// RPC client errors can embed credential-bearing request URLs. Log the
			// same bounded classification exposed by the management API instead of
			// serializing the upstream error.
			s.logger.Error("scanner cycle failed", "chain_id", s.options.ChainID, "error_summary", safeScanError(err))
		} else {
			s.observer.ScanCycle(s.options.ListenerID, s.options.ChainID, "success")
		}
		if err := s.sleep(ctx, s.options.PollInterval); err != nil {
			return err
		}
	}
}

func safeScanError(err error) string {
	message := strings.ToLower(err.Error())
	operation := "RPC request"
	switch {
	case strings.Contains(message, "get logs"):
		operation = "fetching event logs"
	case strings.Contains(message, "latest block"):
		operation = "reading the latest block"
	case strings.Contains(message, "chain id"):
		operation = "verifying the chain ID"
	case strings.Contains(message, "checkpoint") || strings.Contains(message, "persist block batch"):
		operation = "reading or saving the scanner checkpoint"
	case strings.Contains(message, "snapshot block") || strings.Contains(message, "verify block"):
		operation = "verifying block data"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(message, "deadline exceeded") || strings.Contains(message, "timeout"):
		return "Timeout while " + operation
	case strings.Contains(message, "x509") || strings.Contains(message, "certificate") || strings.Contains(message, "tls handshake"):
		return "TLS certificate validation failed while " + operation
	case strings.Contains(message, "no such host") || strings.Contains(message, "server misbehaving"):
		return "RPC endpoint DNS resolution failed while " + operation
	case strings.Contains(message, "connection refused"):
		return "RPC endpoint refused the connection while " + operation
	case strings.Contains(message, "connection reset") || strings.Contains(message, "broken pipe") || strings.Contains(message, "unexpected eof"):
		return "RPC connection was interrupted while " + operation
	case strings.Contains(message, "429") || strings.Contains(message, "rate limit") || strings.Contains(message, "too many requests"):
		return "RPC provider rate limit while " + operation
	case strings.Contains(message, "401") || strings.Contains(message, "403") || strings.Contains(message, "unauthorized") || strings.Contains(message, "forbidden"):
		return "RPC authentication or authorization failed while " + operation
	case strings.Contains(message, "404") || strings.Contains(message, "not found"):
		return "RPC endpoint or method was not found while " + operation
	case strings.Contains(message, "400") || strings.Contains(message, "bad request") || strings.Contains(message, "invalid request"):
		return "RPC provider rejected the request while " + operation
	case strings.Contains(message, "500") || strings.Contains(message, "502") || strings.Contains(message, "503") || strings.Contains(message, "504"):
		return "RPC provider is unavailable while " + operation
	case strings.Contains(message, "invalid character") || strings.Contains(message, "unmarshal") || strings.Contains(message, "invalid json"):
		return "RPC provider returned an invalid response while " + operation
	case strings.Contains(message, "does not match configured chain id"):
		return "RPC endpoint chain ID does not match the configured chain ID"
	case strings.Contains(message, "range") || strings.Contains(message, "response size") || strings.Contains(message, "more than"):
		return "RPC provider rejected the block range while " + operation
	case strings.Contains(message, "persist") || strings.Contains(message, "checkpoint") || strings.Contains(message, "database") || strings.Contains(message, "sqlite"):
		return "SQLite persistence failed while " + operation
	default:
		return "RPC or persistence failure while " + operation
	}
}

// ScanOnce catches the durable checkpoint up to the current confirmed head.
func (s *Scanner) ScanOnce(ctx context.Context) error {
	if err := s.verifyChainID(ctx); err != nil {
		return err
	}
	latest, err := s.headerByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("get latest block: %w", err)
	}
	if latest.Number == nil || !latest.Number.IsUint64() {
		return errors.New("latest block number is invalid")
	}
	latestNumber := latest.Number.Uint64()
	if latestNumber < s.options.Confirmations {
		return nil
	}
	confirmedHead := latestNumber - s.options.Confirmations
	s.observer.Head(s.options.ListenerID, s.options.ChainID, latestNumber, confirmedHead)

	next, err := s.resumeBlock(ctx, latestNumber)
	if err != nil {
		return err
	}
	if next > confirmedHead {
		return nil
	}
	expectedParent := ""
	if next > s.options.StartBlock {
		checkpoint, err := s.checkpoints.Checkpoint(ctx, s.options.ChainID)
		if err != nil {
			return fmt.Errorf("reload checkpoint: %w", err)
		}
		expectedParent = checkpoint.BlockHash
	}

	for from := next; from <= confirmedHead; {
		to := from + s.options.BatchSize - 1
		if to < from || to > confirmedHead {
			to = confirmedHead
		}
		logs, checkpoint, err := s.fetchVerifiedBatch(ctx, from, to, expectedParent)
		if err != nil {
			return err
		}
		if err := s.processor.ProcessBatch(ctx, logs, checkpoint); err != nil {
			return fmt.Errorf("persist block batch %d-%d: %w", from, to, err)
		}
		s.observer.BatchCommitted(s.options.ListenerID, s.options.ChainID, checkpoint.BlockNumber, confirmedHead, len(logs))
		expectedParent = checkpoint.BlockHash
		if to == confirmedHead {
			break
		}
		from = to + 1
	}
	return nil
}

func (s *Scanner) resumeBlock(ctx context.Context, latestNumber uint64) (uint64, error) {
	checkpoint, err := s.checkpoints.Checkpoint(ctx, s.options.ChainID)
	if errors.Is(err, core.ErrCheckpointNotFound) {
		return s.options.StartBlock, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load checkpoint: %w", err)
	}
	// A restored endpoint can temporarily be behind our checkpoint. There is no
	// block at the checkpoint height to compare yet, so wait for the chain to
	// reach it rather than treating the expected condition as an RPC failure.
	// Once it exists, its hash is verified below and a fork resets conservatively.
	if checkpoint.BlockNumber > latestNumber {
		return latestNumber + 1, nil
	}
	number := new(big.Int).SetUint64(checkpoint.BlockNumber)
	_, err = s.headerByNumber(ctx, number)
	if err != nil {
		return 0, fmt.Errorf("verify checkpoint block %d: %w", checkpoint.BlockNumber, err)
	}
	hash, err := s.blockHashByNumber(ctx, number)
	if err != nil {
		return 0, fmt.Errorf("verify checkpoint block %d hash: %w", checkpoint.BlockNumber, err)
	}
	if hash.Hex() == checkpoint.BlockHash {
		if checkpoint.BlockNumber == ^uint64(0) {
			return 0, errors.New("checkpoint block overflows resume position")
		}
		return checkpoint.BlockNumber + 1, nil
	}

	// Only the latest checkpoint hash is durable, so there is no earlier hash
	// with which to prove a common ancestor. Resetting inclusively to the
	// configured start is the conservative recovery: a fixed-depth rewind can
	// silently skip replacement logs when a fork is deeper than expected.
	if err := s.checkpoints.ResetFrom(ctx, s.options.ChainID, s.options.StartBlock); err != nil {
		return 0, fmt.Errorf("reset after reorg: %w", err)
	}
	s.logger.Warn("chain reorganization detected", "chain_id", s.options.ChainID,
		"checkpoint", checkpoint.BlockNumber, "rescan_from", s.options.StartBlock,
		"configured_reorg_depth", s.options.ReorgDepth)
	s.observer.Reorg(s.options.ListenerID, s.options.ChainID)
	return s.options.StartBlock, nil
}

func (s *Scanner) fetchVerifiedBatch(ctx context.Context, from, to uint64, expectedParent string) ([]core.RawLog, core.Checkpoint, error) {
	endNumber := new(big.Int).SetUint64(to)
	_, err := s.headerByNumber(ctx, endNumber)
	if err != nil {
		return nil, core.Checkpoint{}, fmt.Errorf("snapshot block %d before log query: %w", to, err)
	}
	beforeEndHash, err := s.blockHashByNumber(ctx, endNumber)
	if err != nil {
		return nil, core.Checkpoint{}, fmt.Errorf("snapshot block %d hash before log query: %w", to, err)
	}
	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from), ToBlock: new(big.Int).SetUint64(to),
		Addresses: s.options.Addresses, Topics: s.options.Topics,
	}
	logs, err := s.filterLogs(ctx, query)
	if err != nil {
		return nil, core.Checkpoint{}, fmt.Errorf("get logs for blocks %d-%d: %w", from, to, err)
	}

	headers := make(map[uint64]*types.Header)
	hashes := make(map[uint64]common.Hash)
	for _, log := range logs {
		if log.Removed || log.BlockNumber < from || log.BlockNumber > to {
			return nil, core.Checkpoint{}, fmt.Errorf("RPC returned invalid log at block %d", log.BlockNumber)
		}
		if !matchesFilter(log, query) {
			return nil, core.Checkpoint{}, fmt.Errorf("RPC returned log outside the requested address/topic filter at block %d", log.BlockNumber)
		}
		headers[log.BlockNumber] = nil
	}
	headers[from] = nil
	headers[to] = nil
	for number := range headers {
		header, err := s.headerByNumber(ctx, new(big.Int).SetUint64(number))
		if err != nil {
			return nil, core.Checkpoint{}, fmt.Errorf("verify block %d: %w", number, err)
		}
		headers[number] = header
		hash, err := s.blockHashByNumber(ctx, new(big.Int).SetUint64(number))
		if err != nil {
			return nil, core.Checkpoint{}, fmt.Errorf("verify block %d hash: %w", number, err)
		}
		hashes[number] = hash
	}
	if hashes[to] != beforeEndHash {
		return nil, core.Checkpoint{}, fmt.Errorf("chain changed while querying logs for blocks %d-%d", from, to)
	}
	if expectedParent != "" && from > 0 && headers[from].ParentHash.Hex() != expectedParent {
		return nil, core.Checkpoint{}, fmt.Errorf("block %d does not descend from persisted checkpoint", from)
	}
	for _, log := range logs {
		if log.BlockHash != hashes[log.BlockNumber] {
			return nil, core.Checkpoint{}, fmt.Errorf("block hash changed while scanning block %d", log.BlockNumber)
		}
	}

	raw, err := deduplicate(s.options.ChainID, logs)
	if err != nil {
		return nil, core.Checkpoint{}, err
	}
	checkpoint := core.Checkpoint{
		ChainID: s.options.ChainID, BlockNumber: to, BlockHash: hashes[to].Hex(),
	}
	return raw, checkpoint, nil
}

func matchesFilter(log types.Log, query ethereum.FilterQuery) bool {
	if len(query.Addresses) > 0 && !slices.Contains(query.Addresses, log.Address) {
		return false
	}
	for position, accepted := range query.Topics {
		if len(accepted) == 0 {
			continue
		}
		if position >= len(log.Topics) || !slices.Contains(accepted, log.Topics[position]) {
			return false
		}
	}
	return true
}

func deduplicate(chainID uint64, logs []types.Log) ([]core.RawLog, error) {
	seen := make(map[core.EventID]types.Log, len(logs))
	result := make([]core.RawLog, 0, len(logs))
	for _, log := range logs {
		id := core.EventID{ChainID: chainID, TransactionHash: log.TxHash.Hex(), LogIndex: uint64(log.Index)}
		if previous, exists := seen[id]; exists {
			if !sameRPCLog(previous, log) {
				return nil, fmt.Errorf("RPC returned conflicting logs for event %v", id)
			}
			continue
		}
		seen[id] = log
		topics := make([]string, len(log.Topics))
		for i, topic := range log.Topics {
			topics[i] = topic.Hex()
		}
		result = append(result, core.RawLog{
			ChainID: chainID, BlockNumber: log.BlockNumber, BlockHash: log.BlockHash.Hex(),
			TransactionHash: log.TxHash.Hex(), LogIndex: uint64(log.Index),
			Address: log.Address.Hex(), Topics: topics, Data: append([]byte(nil), log.Data...),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].BlockNumber != result[j].BlockNumber {
			return result[i].BlockNumber < result[j].BlockNumber
		}
		return result[i].LogIndex < result[j].LogIndex
	})
	return result, nil
}

func sameRPCLog(first, second types.Log) bool {
	return first.Address == second.Address && slices.Equal(first.Topics, second.Topics) &&
		bytes.Equal(first.Data, second.Data) && first.BlockNumber == second.BlockNumber &&
		first.TxHash == second.TxHash && first.TxIndex == second.TxIndex &&
		first.BlockHash == second.BlockHash && first.Index == second.Index &&
		first.Removed == second.Removed
}

func (s *Scanner) headerByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	var header *types.Header
	err := s.retry(ctx, "header", func() error {
		requestCtx, cancel := context.WithTimeout(ctx, s.options.RPCTimeout)
		defer cancel()
		var err error
		header, err = s.rpc.HeaderByNumber(requestCtx, number)
		if err == nil && header == nil {
			err = errors.New("RPC returned a nil header")
		}
		if err == nil && number != nil && (header.Number == nil || header.Number.Cmp(number) != 0) {
			err = fmt.Errorf("RPC returned block %v for requested block %s", header.Number, number)
		}
		return err
	})
	return header, err
}

func (s *Scanner) blockHashByNumber(ctx context.Context, number *big.Int) (common.Hash, error) {
	var hash common.Hash
	err := s.retry(ctx, "block_hash", func() error {
		requestCtx, cancel := context.WithTimeout(ctx, s.options.RPCTimeout)
		defer cancel()
		var err error
		hash, err = s.rpc.BlockHashByNumber(requestCtx, number)
		if err == nil && hash == (common.Hash{}) {
			err = errors.New("RPC returned an empty block hash")
		}
		return err
	})
	return hash, err
}

func (s *Scanner) filterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	var logs []types.Log
	err := s.retry(ctx, "get_logs", func() error {
		requestCtx, cancel := context.WithTimeout(ctx, s.options.RPCTimeout)
		defer cancel()
		var err error
		logs, err = s.rpc.FilterLogs(requestCtx, query)
		return err
	})
	return logs, err
}

func (s *Scanner) verifyChainID(ctx context.Context) error {
	var chainID *big.Int
	err := s.retry(ctx, "chain_id", func() error {
		requestCtx, cancel := context.WithTimeout(ctx, s.options.RPCTimeout)
		defer cancel()
		var err error
		chainID, err = s.rpc.ChainID(requestCtx)
		if err == nil && (chainID == nil || !chainID.IsUint64()) {
			err = errors.New("RPC returned an invalid chain ID")
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("verify chain ID: %w", err)
	}
	if chainID.Uint64() != s.options.ChainID {
		return fmt.Errorf("RPC chain ID %d does not match configured chain ID %d", chainID.Uint64(), s.options.ChainID)
	}
	return nil
}

func (s *Scanner) retry(ctx context.Context, operationName string, operation func() error) error {
	var err error
	for attempt := 0; attempt < s.options.RetryAttempts; attempt++ {
		if err = operation(); err == nil {
			s.observer.RPCRequest(s.options.ListenerID, s.options.ChainID, operationName, "success")
			return nil
		}
		s.observer.RPCRequest(s.options.ListenerID, s.options.ChainID, operationName, "error")
		if attempt+1 == s.options.RetryAttempts {
			break
		}
		delay := rpcRetryDelay(s.options.RetryBackoff, attempt)
		if sleepErr := s.sleep(ctx, delay); sleepErr != nil {
			return sleepErr
		}
	}
	return err
}

func rpcRetryDelay(base time.Duration, attempt int) time.Duration {
	if base >= maxRPCBackoff {
		return maxRPCBackoff
	}
	delay := base
	for range attempt {
		if delay >= maxRPCBackoff/2 {
			return maxRPCBackoff
		}
		delay *= 2
	}
	return min(delay, maxRPCBackoff)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
