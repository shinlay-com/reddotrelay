package scanner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"reflect"
	"testing"
	"time"

	"reddotrelay/internal/core"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestScanOnceBatchesOnlyConfirmedBlocks(t *testing.T) {
	rpc := newMockRPC(12)
	checkpoints := &mockCheckpoints{err: core.ErrCheckpointNotFound}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanner := newScanner(t, rpc, checkpoints, processor, Options{
		ChainID: 1, StartBlock: 1, BatchSize: 4, Confirmations: 2,
	})

	if err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantRanges := [][2]uint64{{1, 4}, {5, 8}, {9, 10}}
	if !reflect.DeepEqual(rpc.ranges, wantRanges) {
		t.Fatalf("eth_getLogs ranges = %v, want %v", rpc.ranges, wantRanges)
	}
	if got := processor.checkpointNumbers(); !reflect.DeepEqual(got, []uint64{4, 8, 10}) {
		t.Fatalf("persisted checkpoints = %v", got)
	}
}

func TestScanOnceResumesAndDeduplicatesRPCLogs(t *testing.T) {
	rpc := newMockRPC(6)
	blockFive := rpc.headers[5].Hash()
	log := types.Log{
		BlockNumber: 5, BlockHash: blockFive, TxHash: common.HexToHash("0xabc"),
		Index: 2, Address: common.HexToAddress("0x123"), Data: []byte{1, 2},
	}
	rpc.logs = []types.Log{log, log}
	checkpoints := &mockCheckpoints{checkpoint: core.Checkpoint{
		ChainID: 1, BlockNumber: 4, BlockHash: rpc.headers[4].Hash().Hex(),
	}}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanner := newScanner(t, rpc, checkpoints, processor, Options{
		ChainID: 1, StartBlock: 1, BatchSize: 10,
	})

	if err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rpc.ranges, [][2]uint64{{5, 6}}) {
		t.Fatalf("eth_getLogs ranges = %v", rpc.ranges)
	}
	if len(processor.batches) != 1 || len(processor.batches[0]) != 1 {
		t.Fatalf("processed batches = %#v, want one unique log", processor.batches)
	}
}

func TestScanOnceUsesCanonicalRPCHashInsteadOfLocallyComputedHeaderHash(t *testing.T) {
	rpc := newMockRPC(2)
	canonicalOne := common.HexToHash("0x1111")
	canonicalTwo := common.HexToHash("0x2222")
	rpc.canonicalHashes = map[uint64]common.Hash{1: canonicalOne, 2: canonicalTwo}
	rpc.headers[2].ParentHash = canonicalOne
	rpc.logs = []types.Log{{
		BlockNumber: 2, BlockHash: canonicalTwo, TxHash: common.HexToHash("0xabc"),
		Index: 1, Address: common.HexToAddress("0x123"),
	}}
	checkpoints := &mockCheckpoints{checkpoint: core.Checkpoint{
		ChainID: 1, BlockNumber: 1, BlockHash: canonicalOne.Hex(),
	}}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanned := newScanner(t, rpc, checkpoints, processor, Options{ChainID: 1, StartBlock: 1, BatchSize: 1})

	if err := scanned.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := processor.commits[0].BlockHash; got != canonicalTwo.Hex() {
		t.Fatalf("checkpoint hash = %s, want canonical RPC hash %s", got, canonicalTwo.Hex())
	}
}

func TestScanOnceRejectsConflictingDuplicateRPCLogs(t *testing.T) {
	rpc := newMockRPC(5)
	log := types.Log{
		BlockNumber: 5, BlockHash: rpc.headers[5].Hash(), TxHash: common.HexToHash("0xabc"),
		Index: 2, Address: common.HexToAddress("0x123"), Data: []byte{1},
	}
	conflicting := log
	conflicting.Data = []byte{2}
	rpc.logs = []types.Log{log, conflicting}
	checkpoints := &mockCheckpoints{err: core.ErrCheckpointNotFound}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanned := newScanner(t, rpc, checkpoints, processor, Options{ChainID: 1, StartBlock: 5, BatchSize: 1})

	if err := scanned.ScanOnce(context.Background()); err == nil {
		t.Fatal("ScanOnce() error = nil, want conflicting duplicate error")
	}
	if len(processor.commits) != 0 {
		t.Fatalf("persisted checkpoints = %#v, want none", processor.commits)
	}
}

func TestScanOnceRejectsLogOutsideRPCFilter(t *testing.T) {
	rpc := newMockRPC(5)
	requested := common.HexToAddress("0x123")
	rpc.logs = []types.Log{{
		BlockNumber: 5, BlockHash: rpc.headers[5].Hash(), TxHash: common.HexToHash("0xabc"),
		Index: 2, Address: common.HexToAddress("0x456"),
	}}
	checkpoints := &mockCheckpoints{err: core.ErrCheckpointNotFound}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanned := newScanner(t, rpc, checkpoints, processor, Options{
		ChainID: 1, StartBlock: 5, BatchSize: 1, Addresses: []common.Address{requested},
	})

	if err := scanned.ScanOnce(context.Background()); err == nil {
		t.Fatal("ScanOnce() error = nil, want unexpected-filter-result error")
	}
	if len(processor.commits) != 0 {
		t.Fatalf("persisted checkpoints = %#v, want none", processor.commits)
	}
}

func TestScanOnceRetriesRPCWithExponentialBackoff(t *testing.T) {
	rpc := newMockRPC(2)
	rpc.latestFailures = 2
	checkpoints := &mockCheckpoints{err: core.ErrCheckpointNotFound}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanner := newScanner(t, rpc, checkpoints, processor, Options{
		ChainID: 1, StartBlock: 2, BatchSize: 1, RetryAttempts: 3, RetryBackoff: time.Second,
	})
	var delays []time.Duration
	scanner.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	if err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(delays, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("retry delays = %v", delays)
	}
}

func TestScanOnceRPCOutageDoesNotAdvanceCheckpoint(t *testing.T) {
	rpc := newMockRPC(2)
	rpc.latestFailures = 3
	checkpoints := &mockCheckpoints{err: core.ErrCheckpointNotFound}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanner := newScanner(t, rpc, checkpoints, processor, Options{
		ChainID: 1, StartBlock: 1, BatchSize: 2, RetryAttempts: 3, RetryBackoff: time.Millisecond,
	})
	scanner.sleep = func(context.Context, time.Duration) error { return nil }

	if err := scanner.ScanOnce(context.Background()); err == nil {
		t.Fatal("ScanOnce() error = nil, want exhausted RPC outage error")
	}
	if len(processor.commits) != 0 || checkpoints.err == nil {
		t.Fatalf("RPC outage advanced durable state: commits=%#v checkpoint=%#v", processor.commits, checkpoints.checkpoint)
	}
}

func TestRPCRetryDelayIsCappedWithoutOverflow(t *testing.T) {
	for _, test := range []struct {
		base    time.Duration
		attempt int
	}{
		{base: time.Second, attempt: 29},
		{base: time.Duration(1<<63 - 1), attempt: 29},
	} {
		if got := rpcRetryDelay(test.base, test.attempt); got != maxRPCBackoff {
			t.Fatalf("rpcRetryDelay(%s, %d) = %s, want %s", test.base, test.attempt, got, maxRPCBackoff)
		}
	}
}

func TestScanOnceRescansFromStartOnCheckpointHashMismatch(t *testing.T) {
	rpc := newMockRPC(10)
	checkpoints := &mockCheckpoints{checkpoint: core.Checkpoint{
		ChainID: 1, BlockNumber: 10, BlockHash: common.HexToHash("0xold").Hex(),
	}}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanner := newScanner(t, rpc, checkpoints, processor, Options{
		ChainID: 1, StartBlock: 1, BatchSize: 10, ReorgDepth: 3,
	})

	if err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(checkpoints.resets, []uint64{1}) {
		t.Fatalf("reset positions = %#v, want [1]", checkpoints.resets)
	}
	if !reflect.DeepEqual(rpc.ranges, [][2]uint64{{1, 10}}) {
		t.Fatalf("eth_getLogs ranges after reset = %v", rpc.ranges)
	}
}

func TestScanOnceWaitsForRestoredChainToReachCheckpoint(t *testing.T) {
	rpc := newMockRPC(9)
	checkpoints := &mockCheckpoints{checkpoint: core.Checkpoint{
		ChainID: 1, BlockNumber: 10, BlockHash: common.HexToHash("0xold").Hex(),
	}}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanned := newScanner(t, rpc, checkpoints, processor, Options{ChainID: 1, StartBlock: 1, BatchSize: 10})

	if err := scanned.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(checkpoints.resets) != 0 || len(processor.commits) != 0 || len(rpc.ranges) != 0 {
		t.Fatalf("restored chain before checkpoint was processed: resets=%v commits=%v ranges=%v", checkpoints.resets, processor.commits, rpc.ranges)
	}
}

func TestScanOnceRejectsLogWhoseBlockHashChanged(t *testing.T) {
	rpc := newMockRPC(3)
	rpc.logs = []types.Log{{
		BlockNumber: 3, BlockHash: common.HexToHash("0xstale"),
		TxHash: common.HexToHash("0xabc"), Index: 1,
	}}
	checkpoints := &mockCheckpoints{err: core.ErrCheckpointNotFound}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanner := newScanner(t, rpc, checkpoints, processor, Options{
		ChainID: 1, StartBlock: 3, BatchSize: 1,
	})

	if err := scanner.ScanOnce(context.Background()); err == nil {
		t.Fatal("ScanOnce() error = nil, want block-hash verification error")
	}
	if len(processor.batches) != 0 {
		t.Fatal("stale logs were processed")
	}
}

func TestScanOnceRejectsWrongRPCChain(t *testing.T) {
	rpc := newMockRPC(1)
	rpc.chainID = 2
	checkpoints := &mockCheckpoints{err: core.ErrCheckpointNotFound}
	scanned := newScanner(t, rpc, checkpoints, &mockProcessor{checkpoints: checkpoints}, Options{ChainID: 1, StartBlock: 1, BatchSize: 1})
	if err := scanned.ScanOnce(context.Background()); err == nil {
		t.Fatal("ScanOnce() error = nil, want chain ID mismatch")
	}
}

func TestReorgBetweenBatchesCannotHideReplacementRange(t *testing.T) {
	rpc := newMockRPC(6)
	checkpoints := &mockCheckpoints{err: core.ErrCheckpointNotFound}
	processor := &mockProcessor{checkpoints: checkpoints}
	processor.afterProcess = func(count int) {
		if count != 1 {
			return
		}
		for number := uint64(3); number <= 6; number++ {
			header := &types.Header{Number: new(big.Int).SetUint64(number), Time: number + 100, Extra: []byte("replacement")}
			if number == 3 {
				header.ParentHash = rpc.headers[2].Hash()
			} else {
				header.ParentHash = rpc.headers[number-1].Hash()
			}
			rpc.headers[number] = header
		}
	}
	scanned := newScanner(t, rpc, checkpoints, processor, Options{ChainID: 1, StartBlock: 1, BatchSize: 3})
	if err := scanned.ScanOnce(context.Background()); err == nil {
		t.Fatal("ScanOnce() error = nil, want cross-batch reorg error")
	}
	if got := processor.checkpointNumbers(); !reflect.DeepEqual(got, []uint64{3}) {
		t.Fatalf("persisted checkpoints = %v, want only pre-reorg batch", got)
	}
}

func TestReorgDuringEmptyLogQueryDoesNotAdvanceCheckpoint(t *testing.T) {
	rpc := newMockRPC(3)
	rpc.afterFilter = func() {
		for number := uint64(1); number <= 3; number++ {
			header := &types.Header{Number: new(big.Int).SetUint64(number), Time: number + 100, Extra: []byte("replacement")}
			if number == 1 {
				header.ParentHash = rpc.headers[0].Hash()
			} else {
				header.ParentHash = rpc.headers[number-1].Hash()
			}
			rpc.headers[number] = header
		}
		rpc.afterFilter = nil
	}
	checkpoints := &mockCheckpoints{err: core.ErrCheckpointNotFound}
	processor := &mockProcessor{checkpoints: checkpoints}
	scanned := newScanner(t, rpc, checkpoints, processor, Options{ChainID: 1, StartBlock: 1, BatchSize: 3})

	if err := scanned.ScanOnce(context.Background()); err == nil {
		t.Fatal("ScanOnce() error = nil, want in-query reorg error")
	}
	if len(processor.commits) != 0 {
		t.Fatalf("persisted checkpoints = %#v, want none", processor.commits)
	}
}

type mockRPC struct {
	latest          uint64
	headers         map[uint64]*types.Header
	canonicalHashes map[uint64]common.Hash
	logs            []types.Log
	ranges          [][2]uint64
	latestFailures  int
	chainID         int64
	afterFilter     func()
}

func newMockRPC(latest uint64) *mockRPC {
	headers := make(map[uint64]*types.Header, latest+1)
	for number := uint64(0); number <= latest; number++ {
		header := &types.Header{
			Number: new(big.Int).SetUint64(number),
			Time:   number, Extra: []byte{byte(number)},
		}
		if number > 0 {
			header.ParentHash = headers[number-1].Hash()
		}
		headers[number] = header
	}
	return &mockRPC{latest: latest, headers: headers, chainID: 1}
}

func (m *mockRPC) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	if number == nil {
		if m.latestFailures > 0 {
			m.latestFailures--
			return nil, errors.New("temporary RPC failure")
		}
		return m.headers[m.latest], nil
	}
	return m.headers[number.Uint64()], nil
}

func (m *mockRPC) BlockHashByNumber(_ context.Context, number *big.Int) (common.Hash, error) {
	blockNumber := m.latest
	if number != nil {
		blockNumber = number.Uint64()
	}
	if hash, ok := m.canonicalHashes[blockNumber]; ok {
		return hash, nil
	}
	return m.headers[blockNumber].Hash(), nil
}

func (m *mockRPC) ChainID(context.Context) (*big.Int, error) { return big.NewInt(m.chainID), nil }

func (m *mockRPC) FilterLogs(_ context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	from, to := query.FromBlock.Uint64(), query.ToBlock.Uint64()
	m.ranges = append(m.ranges, [2]uint64{from, to})
	var logs []types.Log
	for _, log := range m.logs {
		if log.BlockNumber >= from && log.BlockNumber <= to {
			logs = append(logs, log)
		}
	}
	if m.afterFilter != nil {
		m.afterFilter()
	}
	return logs, nil
}

type mockCheckpoints struct {
	checkpoint core.Checkpoint
	err        error
	resets     []uint64
}

func (m *mockCheckpoints) Checkpoint(context.Context, uint64) (core.Checkpoint, error) {
	return m.checkpoint, m.err
}

func (m *mockCheckpoints) ResetFrom(_ context.Context, _ uint64, block uint64) error {
	m.resets = append(m.resets, block)
	m.checkpoint = core.Checkpoint{}
	m.err = core.ErrCheckpointNotFound
	return nil
}

type mockProcessor struct {
	checkpoints  *mockCheckpoints
	batches      [][]core.RawLog
	commits      []core.Checkpoint
	afterProcess func(int)
}

func (m *mockProcessor) ProcessBatch(_ context.Context, logs []core.RawLog, checkpoint core.Checkpoint) error {
	m.batches = append(m.batches, append([]core.RawLog(nil), logs...))
	m.commits = append(m.commits, checkpoint)
	m.checkpoints.checkpoint = checkpoint
	m.checkpoints.err = nil
	if m.afterProcess != nil {
		m.afterProcess(len(m.commits))
	}
	return nil
}

func (m *mockProcessor) checkpointNumbers() []uint64 {
	result := make([]uint64, len(m.commits))
	for i, checkpoint := range m.commits {
		result[i] = checkpoint.BlockNumber
	}
	return result
}

func newScanner(t *testing.T, rpc RPC, checkpoints Checkpoints, processor BatchProcessor, options Options) *Scanner {
	t.Helper()
	if options.PollInterval == 0 {
		options.PollInterval = time.Second
	}
	if options.RetryAttempts == 0 {
		options.RetryAttempts = 1
	}
	if options.RetryBackoff == 0 {
		options.RetryBackoff = time.Millisecond
	}
	if options.RPCTimeout == 0 {
		options.RPCTimeout = time.Second
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scanner, err := New(rpc, checkpoints, processor, options, logger)
	if err != nil {
		t.Fatal(err)
	}
	return scanner
}
