package main

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestCanonicalRPCHeaderAcceptsMinimalHistoricalBlock(t *testing.T) {
	parent := common.HexToHash("0x1234")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var call struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"jsonrpc": "2.0", "id": call.ID,
			"result": map[string]any{"number": "0x2", "parentHash": parent.Hex()},
		})
	}))
	defer server.Close()

	client, err := rpc.DialContext(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	canonical := &canonicalRPC{Client: ethclient.NewClient(client)}
	header, err := canonical.HeaderByNumber(context.Background(), big.NewInt(2))
	if err != nil || header.Number.Uint64() != 2 || header.ParentHash != parent {
		t.Fatalf("header = %#v, err = %v", header, err)
	}
}
