package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"reddotrelay/internal/core"
	"reddotrelay/internal/decoder"
	"reddotrelay/internal/rpcauth"
	"reddotrelay/internal/scanner"
	"reddotrelay/internal/secrets"
	"reddotrelay/internal/store/sqlite"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
)

func buildScannerRuntime(store *sqlite.Store, logger *slog.Logger) scannerRuntimeBuilder {
	return buildScannerRuntimeWithObservers(store, logger, secrets.New(), nil, 8, nil)
}

type secretValueResolver interface {
	Resolve(context.Context, string) (string, error)
}

func buildScannerRuntimeWithResolver(store *sqlite.Store, logger *slog.Logger, resolver secretValueResolver) scannerRuntimeBuilder {
	return buildScannerRuntimeWithObservers(store, logger, resolver, nil, 8, nil)
}

func buildScannerRuntimeWithObservers(store *sqlite.Store, logger *slog.Logger, resolver secretValueResolver, observer scanner.Observer, verificationConcurrency int, verificationLimiter chan struct{}) scannerRuntimeBuilder {
	return func(ctx context.Context, snapshot core.RPCListenerSnapshot, listener core.RPCListener) (*scannerRuntime, error) {
		specs := make([]decoder.ContractSpec, 0, len(listener.Contracts))
		for _, contract := range listener.Contracts {
			if len(contract.Events) == 0 {
				continue
			}
			events := make([]decoder.EventSpec, len(contract.Events))
			for j, event := range contract.Events {
				events[j] = decoder.EventSpec{
					Selector: event.Selector,
					Destinations: effectiveWebhookURLs(
						snapshot.GlobalWebhooks, listener.Webhooks, contract.Webhooks, event.Webhooks,
					),
				}
			}
			specs = append(specs, decoder.ContractSpec{
				Address: contract.Address, ABIJSON: append([]byte(nil), contract.ABI...), Events: events,
			})
		}
		if len(specs) == 0 {
			logger.Info("RPC listener has no event subscriptions; scanner is idle", "rpc_listener_id", listener.ID, "chain_id", listener.ChainID)
			return &scannerRuntime{runner: idleScannerRuntime{}, idle: true}, nil
		}
		decoded, err := decoder.Load("", specs)
		if err != nil {
			return nil, fmt.Errorf("load ABI for chain %d: %w", listener.ChainID, err)
		}
		if listener.TLS.InsecureSkipVerify {
			logger.Warn("RPC TLS certificate verification is disabled", "rpc_listener_id", listener.ID, "chain_id", listener.ChainID)
		}
		rpcURL := listener.RPCURL
		if listener.RPCURLRef != "" {
			rpcURL, err = resolver.Resolve(ctx, listener.RPCURLRef)
			if err != nil {
				return nil, errors.New("resolve RPC URL secret reference")
			}
			if !validAbsoluteHTTPURL(rpcURL) {
				return nil, errors.New("resolved RPC URL is not an absolute HTTP(S) URL")
			}
		}
		dialCtx, cancel := context.WithTimeout(ctx, listener.RPCTimeout)
		client, err := dialListenerRPC(dialCtx, rpcURL, listener.TLS, listener.RPCAuthentication)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("connect RPC for chain %d: %w", listener.ChainID, err)
		}
		processor := decoder.NewProcessor(decoded, store)
		scanned, err := scanner.NewWithObserver(&canonicalRPC{Client: client}, store, processor, scanner.Options{
			ListenerID: listener.ID, ChainID: listener.ChainID, StartBlock: listener.StartBlock, BatchSize: listener.BatchSize,
			Confirmations: listener.Confirmations, ReorgDepth: listener.ReorgDepth, PollInterval: listener.PollInterval,
			RetryAttempts: listener.RPCRetryAttempts, RetryBackoff: listener.RPCRetryBackoff, RPCTimeout: listener.RPCTimeout,
			VerificationConcurrency: verificationConcurrency, VerificationLimiter: verificationLimiter,
			Addresses: decoded.Addresses(), Topics: [][]common.Hash{decoded.Topic0()},
		}, logger, observer)
		if err != nil {
			client.Close()
			return nil, fmt.Errorf("configure scanner for chain %d: %w", listener.ChainID, err)
		}
		return &scannerRuntime{runner: scanned, close: client.Close}, nil
	}
}

type idleScannerRuntime struct{}

func (idleScannerRuntime) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func effectiveWebhookURLs(levels ...[]core.WebhookConfig) []core.WebhookDestination {
	for i := len(levels) - 1; i >= 0; i-- {
		if len(levels[i]) == 0 {
			continue
		}
		urls := make([]core.WebhookDestination, len(levels[i]))
		for j, webhook := range levels[i] {
			urls[j] = core.WebhookDestination{Locator: configuredWebhookLocator(webhook), Authentication: webhook.Authentication}
		}
		return urls
	}
	return nil
}

func configuredWebhookLocator(webhook core.WebhookConfig) string {
	if webhook.URLRef != "" {
		return webhook.URLRef
	}
	return webhook.URL
}

func dialListenerRPC(ctx context.Context, rawURL string, settings core.ListenerTLSConfig, authentications ...core.RPCAuthentication) (*ethclient.Client, error) {
	authentication := core.RPCAuthentication{}
	if len(authentications) != 0 {
		authentication = authentications[0]
	}
	if settings.CAPEM == "" && settings.ServerName == "" && !settings.InsecureSkipVerify && authentication.Type == "" {
		return ethclient.DialContext(ctx, rawURL)
	}
	httpClient, err := listenerRPCHTTPClient(settings, authentication)
	if err != nil {
		return nil, err
	}
	client, err := gethrpc.DialOptions(ctx, rawURL, gethrpc.WithHTTPClient(httpClient))
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(client), nil
}

func listenerRPCHTTPClient(settings core.ListenerTLSConfig, authentications ...core.RPCAuthentication) (*http.Client, error) {
	var roots *x509.CertPool
	if settings.CAPEM != "" {
		var err error
		roots, err = x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system certificate pool: %w", err)
		}
		if roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM([]byte(settings.CAPEM)) {
			return nil, errors.New("RPC CA PEM contains no valid certificates")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: settings.ServerName,
		InsecureSkipVerify: settings.InsecureSkipVerify,
	}
	var roundTripper http.RoundTripper = transport
	if len(authentications) != 0 && authentications[0].Type != "" {
		auth := authentications[0]
		wrapped, err := rpcauth.NewTransport(roundTripper, rpcauth.Config{Type: auth.Type, Username: auth.Username, HeaderName: auth.HeaderName, Secret: auth.Secret, TokenURL: auth.TokenURL, TokenAPIKey: auth.TokenAPIKey})
		if err != nil {
			return nil, err
		}
		roundTripper = wrapped
	}
	return &http.Client{Transport: roundTripper}, nil
}
