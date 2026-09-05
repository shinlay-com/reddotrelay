package scanner

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

func (s *Scanner) canonicalHeadersByNumber(ctx context.Context, rpc canonicalBatchHeaderRPC, numbers []*big.Int) ([]CanonicalBatchHeader, error) {
	var headers []CanonicalBatchHeader
	err := s.retry(ctx, "block_headers", func() error {
		requestCtx, cancel := context.WithTimeout(ctx, s.options.RPCTimeout)
		defer cancel()
		var err error
		headers, err = rpc.CanonicalHeadersByNumber(requestCtx, numbers)
		if err != nil {
			if isUnsupportedBatchError(err) {
				headers, err = canonicalHeadersByNumberSequential(requestCtx, rpc, numbers)
				return err
			}
			return err
		}
		if len(headers) != len(numbers) {
			return fmt.Errorf("RPC returned %d block headers for %d requested blocks", len(headers), len(numbers))
		}
		seen := make(map[uint64]struct{}, len(headers))
		for i, header := range headers {
			if header.Hash == (common.Hash{}) {
				return errors.New("RPC returned an empty block hash")
			}
			if header.Number != numbers[i].Uint64() {
				return fmt.Errorf("RPC returned block %d for requested block %s", header.Number, numbers[i])
			}
			if _, ok := seen[header.Number]; ok {
				return fmt.Errorf("RPC returned duplicate block %d in batch", header.Number)
			}
			seen[header.Number] = struct{}{}
		}
		return nil
	})
	return headers, err
}

func canonicalHeadersByNumberSequential(ctx context.Context, rpc canonicalHeaderRPC, numbers []*big.Int) ([]CanonicalBatchHeader, error) {
	headers := make([]CanonicalBatchHeader, len(numbers))
	for i, number := range numbers {
		hash, parent, err := rpc.CanonicalHeaderByNumber(ctx, number)
		if err != nil {
			return nil, err
		}
		value := uint64(0)
		if number != nil {
			value = number.Uint64()
		}
		headers[i] = CanonicalBatchHeader{Number: value, Hash: hash, ParentHash: parent}
	}
	return headers, nil
}

func isUnsupportedBatchError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "batch") && (strings.Contains(message, "not supported") || strings.Contains(message, "unsupported"))
}
