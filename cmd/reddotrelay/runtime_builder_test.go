package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"reddotrelay/internal/core"
)

func TestEffectiveWebhookURLsUsesMostSpecificConfiguredLevel(t *testing.T) {
	global := []core.WebhookConfig{{URL: "https://global.example.test"}}
	chain := []core.WebhookConfig{{URL: "https://chain.example.test"}}
	contract := []core.WebhookConfig{{URL: "https://contract.example.test"}}
	event := []core.WebhookConfig{{URL: "https://event.example.test"}}

	for name, test := range map[string]struct {
		levels [][]core.WebhookConfig
		want   string
	}{
		"event":    {[][]core.WebhookConfig{global, chain, contract, event}, event[0].URL},
		"contract": {[][]core.WebhookConfig{global, chain, contract, nil}, contract[0].URL},
		"chain":    {[][]core.WebhookConfig{global, chain, nil, nil}, chain[0].URL},
		"global":   {[][]core.WebhookConfig{global, nil, nil, nil}, global[0].URL},
	} {
		t.Run(name, func(t *testing.T) {
			got := effectiveWebhookURLs(test.levels...)
			if len(got) != 1 || got[0].Locator != test.want {
				t.Fatalf("effective URLs = %#v, want %s", got, test.want)
			}
		})
	}
}

func TestListenerRPCHTTPClientRejectsInvalidCAPEM(t *testing.T) {
	if _, err := listenerRPCHTTPClient(core.ListenerTLSConfig{CAPEM: "not a certificate"}); err == nil {
		t.Fatal("listenerRPCHTTPClient() error = nil, want invalid CA PEM error")
	}
}

func TestBuildScannerRuntimeIsIdleWithoutEventSubscriptions(t *testing.T) {
	listener := runtimeListenerFixture()
	listener.Contracts = []core.ContractConfig{{
		ID: core.NewConfigID(), Address: "0x0000000000000000000000000000000000000001",
	}}
	builder := buildScannerRuntime(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runtime, err := builder(context.Background(), core.RPCListenerSnapshot{}, listener)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- runtime.runner.Run(ctx) }()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("idle runtime error = %v, want context canceled", err)
	}
}
