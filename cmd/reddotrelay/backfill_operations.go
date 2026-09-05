package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"reddotrelay/internal/config"
	"reddotrelay/internal/core"
	"reddotrelay/internal/decoder"
	"reddotrelay/internal/observability"
	"reddotrelay/internal/scanner"
	"reddotrelay/internal/secrets"
	"reddotrelay/internal/store/sqlite"

	"github.com/ethereum/go-ethereum/common"
)

type backfillRequest struct {
	RPCListenerID string   `json:"rpcListenerId"`
	FromBlock     uint64   `json:"fromBlock"`
	ToBlock       uint64   `json:"toBlock"`
	Mode          string   `json:"mode"`
	ContractIDs   []string `json:"contractIds"`
	EventIDs      []string `json:"eventIds"`
	Confirm       bool     `json:"confirm"`
}
type backfillPreview struct {
	RPCListenerID         string   `json:"rpcListenerId"`
	ChainID               uint64   `json:"chainId"`
	FromBlock             uint64   `json:"fromBlock"`
	ToBlock               uint64   `json:"toBlock"`
	Blocks                uint64   `json:"blocks"`
	Mode                  string   `json:"mode"`
	ContractIDs           []string `json:"contractIds"`
	EventIDs              []string `json:"eventIds"`
	ConfigurationRevision uint64   `json:"configurationRevision"`
	Destinations          []string `json:"destinations"`
}

func prepareBackfill(ctx context.Context, store *sqlite.Store, in backfillRequest, maxRange uint64) (backfillPreview, []byte, error) {
	snap, err := store.RPCListenerSnapshot(ctx)
	if err != nil {
		return backfillPreview{}, nil, err
	}
	var listener *core.RPCListener
	for i := range snap.Listeners {
		if snap.Listeners[i].ID == in.RPCListenerID {
			listener = &snap.Listeners[i]
			break
		}
	}
	if listener == nil {
		return backfillPreview{}, nil, sqlite.ErrNotFound
	}
	if in.Mode != "" && in.Mode != "backfill-missing" {
		return backfillPreview{}, nil, errors.New("mode must be backfill-missing")
	}
	if in.FromBlock < listener.StartBlock || in.ToBlock < in.FromBlock || in.ToBlock-in.FromBlock+1 > maxRange {
		return backfillPreview{}, nil, errors.New("block range is invalid")
	}
	contracts := map[string]bool{}
	events := map[string]bool{}
	dest := map[string]bool{}
	for _, id := range in.ContractIDs {
		contracts[id] = false
	}
	for _, id := range in.EventIDs {
		events[id] = false
	}
	for _, c := range listener.Contracts {
		includeContract := len(contracts) == 0 || slices.Contains(in.ContractIDs, c.ID)
		if slices.Contains(in.ContractIDs, c.ID) {
			contracts[c.ID] = true
		}
		for _, e := range c.Events {
			if slices.Contains(in.EventIDs, e.ID) {
				events[e.ID] = true
			}
			if includeContract && (len(events) == 0 || slices.Contains(in.EventIDs, e.ID)) {
				for _, d := range effectiveWebhookURLs(snap.GlobalWebhooks, listener.Webhooks, c.Webhooks, e.Webhooks) {
					dest[d.Locator] = true
				}
			}
		}
	}
	for _, v := range contracts {
		if !v {
			return backfillPreview{}, nil, errors.New("contractIds contains a stale or cross-listener identifier")
		}
	}
	for _, v := range events {
		if !v {
			return backfillPreview{}, nil, errors.New("eventIds contains a stale or cross-listener identifier")
		}
	}
	destinations := []string{}
	for d := range dest {
		if strings.HasPrefix(d, "env://") || strings.HasPrefix(d, "file:///") {
			destinations = append(destinations, d)
		} else {
			destinations = append(destinations, redactOptionalURL(d))
		}
	}
	slices.Sort(destinations)
	encoded, err := json.Marshal(snap)
	if err == nil {
		encoded, err = store.SealBackfillSnapshot(encoded)
	}
	return backfillPreview{listener.ID, listener.ChainID, in.FromBlock, in.ToBlock, in.ToBlock - in.FromBlock + 1, "backfill-missing", in.ContractIDs, in.EventIDs, snap.Revision, destinations}, encoded, err
}

func handleBackfills(store *sqlite.Store, maxRange uint64, w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.URL.Path == "/api/v1/backfills/preview" {
		if r.Method != http.MethodPost {
			writeAPIError(w, 405, "method not allowed")
			return
		}
		var in backfillRequest
		if !decodeCreateRequest(w, r, &in) {
			return
		}
		p, _, err := prepareBackfill(r.Context(), store, in, maxRange)
		if err != nil {
			writeAPIError(w, 422, err.Error())
			return
		}
		writeJSON(w, 200, p)
		return
	}
	if r.URL.Path == "/api/v1/backfills" {
		if r.Method == http.MethodGet {
			for key := range r.URL.Query() {
				if key != "limit" && key != "before" {
					writeAPIError(w, 400, "unknown query parameter")
					return
				}
			}
			if before := r.URL.Query().Get("before"); before != "" && !canonicalUUID(before) {
				writeAPIError(w, 400, "before must be a valid job cursor")
				return
			}
			limit, ok := diagnosticsLimit(w, r.URL.Query().Get("limit"))
			if !ok {
				return
			}
			jobs, err := store.ListBackfills(r.Context(), limit+1, r.URL.Query().Get("before"))
			if err != nil {
				writeAPIError(w, 500, "internal server error")
				return
			}
			more := len(jobs) > limit
			if more {
				jobs = jobs[:limit]
			}
			if jobs == nil {
				jobs = []core.BackfillJob{}
			}
			out := map[string]any{"entries": jobs}
			if more {
				out["nextBefore"] = jobs[len(jobs)-1].ID
			}
			writeJSON(w, 200, out)
			return
		}
		if r.Method == http.MethodPost {
			var in backfillRequest
			if !decodeCreateRequest(w, r, &in) {
				return
			}
			if !in.Confirm {
				writeAPIError(w, 422, "confirm must be true")
				return
			}
			p, snapshot, err := prepareBackfill(r.Context(), store, in, maxRange)
			if err != nil {
				writeAPIError(w, 422, err.Error())
				return
			}
			job := core.BackfillJob{ID: core.NewConfigID(), ListenerID: p.RPCListenerID, ChainID: p.ChainID, Mode: p.Mode, FromBlock: p.FromBlock, ToBlock: p.ToBlock, ContractIDs: p.ContractIDs, EventIDs: p.EventIDs, ConfigRevision: p.ConfigurationRevision, Snapshot: snapshot, Destinations: p.Destinations}
			actor := r.Context().Value(apiKeyPrincipalContextKey{}).(core.APIKeyPrincipal)
			if err = store.CreateBackfill(r.Context(), job, actor, time.Now().UTC()); err != nil {
				if errors.Is(err, sqlite.ErrActiveBackfill) {
					writeAPIError(w, 409, err.Error())
				} else {
					writeAPIError(w, 500, "internal server error")
				}
				return
			}
			job, _ = store.Backfill(r.Context(), job.ID)
			w.Header().Set("Location", "/api/v1/backfills/"+job.ID)
			writeJSON(w, 201, job)
			return
		}
		writeAPIError(w, 405, "method not allowed")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backfills/"), "/")
	if len(parts) < 1 || !canonicalUUID(parts[0]) {
		writeAPIError(w, 404, "resource not found")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		job, err := store.Backfill(r.Context(), parts[0])
		if err != nil {
			writeAPIError(w, 404, "backfill not found")
			return
		}
		writeJSON(w, 200, job)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost {
		var input struct {
			Confirm bool `json:"confirm"`
		}
		if !decodeCreateRequest(w, r, &input) {
			return
		}
		if !input.Confirm {
			writeAPIError(w, http.StatusUnprocessableEntity, "confirm must be true")
			return
		}
		var state core.BackfillState
		switch parts[1] {
		case "pause":
			state = core.BackfillPaused
		case "resume":
			state = core.BackfillQueued
		case "cancel":
			state = core.BackfillCancelled
		default:
			writeAPIError(w, 404, "resource not found")
			return
		}
		actor := r.Context().Value(apiKeyPrincipalContextKey{}).(core.APIKeyPrincipal)
		if err := store.TransitionBackfill(r.Context(), parts[0], state, actor, time.Now().UTC()); err != nil {
			writeAPIError(w, 409, err.Error())
			return
		}
		job, _ := store.Backfill(r.Context(), parts[0])
		writeJSON(w, 200, job)
		return
	}
	writeAPIError(w, 405, "method not allowed")
}

func handleBackfillAudit(store *sqlite.Store, w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "method not allowed")
		return
	}
	for key := range r.URL.Query() {
		if key != "limit" && key != "before" {
			writeAPIError(w, 400, "unknown query parameter")
			return
		}
	}
	limit, ok := diagnosticsLimit(w, r.URL.Query().Get("limit"))
	if !ok {
		return
	}
	var before uint64
	if raw := r.URL.Query().Get("before"); raw != "" {
		before, ok = diagnosticUint(w, "before", raw)
		if !ok {
			return
		}
	}
	entries, err := store.BackfillAudit(r.Context(), limit+1, before)
	if err != nil {
		writeAPIError(w, 500, "internal server error")
		return
	}
	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}
	out := map[string]any{"entries": entries}
	if more {
		out["nextBefore"] = entries[len(entries)-1].Sequence
	}
	writeJSON(w, 200, out)
}

type backfillWorker struct {
	store                   *sqlite.Store
	cfg                     config.BackfillConfig
	logger                  *slog.Logger
	metrics                 *observability.Metrics
	verificationConcurrency int
	verificationLimiter     chan struct{}
}

func (b *backfillWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			b.runOne(ctx)
		}
	}
}
func (b *backfillWorker) runOne(ctx context.Context) {
	job, err := b.store.ClaimBackfill(ctx, time.Now().UTC())
	if errors.Is(err, sqlite.ErrNotFound) {
		return
	}
	if err != nil {
		return
	}
	if b.metrics != nil {
		b.metrics.BackfillStarted()
		defer b.metrics.BackfillBatch(0, 0)
	}
	var snap core.RPCListenerSnapshot
	opened, openErr := b.store.OpenBackfillSnapshot(job.Snapshot)
	if openErr != nil || json.Unmarshal(opened, &snap) != nil {
		_ = b.store.FailBackfill(ctx, job.ID, "Invalid immutable configuration snapshot", time.Now().UTC())
		return
	}
	var listener core.RPCListener
	for _, l := range snap.Listeners {
		if l.ID == job.ListenerID {
			listener = l
		}
	}
	specs := []decoder.ContractSpec{}
	for _, c := range listener.Contracts {
		if len(job.ContractIDs) > 0 && !slices.Contains(job.ContractIDs, c.ID) {
			continue
		}
		es := []decoder.EventSpec{}
		for _, e := range c.Events {
			if len(job.EventIDs) > 0 && !slices.Contains(job.EventIDs, e.ID) {
				continue
			}
			es = append(es, decoder.EventSpec{Selector: e.Selector, Destinations: effectiveWebhookURLs(snap.GlobalWebhooks, listener.Webhooks, c.Webhooks, e.Webhooks)})
		}
		if len(es) > 0 {
			specs = append(specs, decoder.ContractSpec{Address: c.Address, ABIJSON: c.ABI, Events: es})
		}
	}
	decoded, err := decoder.Load("", specs)
	if err != nil {
		_ = b.store.FailBackfill(ctx, job.ID, "Invalid captured decoder configuration", time.Now().UTC())
		return
	}
	rpcURL := listener.RPCURL
	if listener.RPCURLRef != "" {
		rpcURL, err = secrets.New().Resolve(ctx, listener.RPCURLRef)
	}
	if err != nil {
		_ = b.store.FailBackfill(ctx, job.ID, "RPC configuration is unavailable", time.Now().UTC())
		return
	}
	client, err := dialListenerRPC(ctx, rpcURL, listener.TLS, listener.RPCAuthentication)
	if err != nil {
		_ = b.store.FailBackfill(ctx, job.ID, "RPC connection failed", time.Now().UTC())
		return
	}
	defer client.Close()
	concurrency := b.verificationConcurrency
	if concurrency <= 0 {
		concurrency = 8
	}
	scanned, err := scanner.New(&canonicalRPC{Client: client}, b.store, decoder.NewProcessor(decoded, b.store), scanner.Options{ListenerID: listener.ID, ChainID: listener.ChainID, StartBlock: listener.StartBlock, BatchSize: listener.BatchSize, Confirmations: listener.Confirmations, ReorgDepth: listener.ReorgDepth, PollInterval: listener.PollInterval, RetryAttempts: listener.RPCRetryAttempts, RetryBackoff: listener.RPCRetryBackoff, RPCTimeout: listener.RPCTimeout, VerificationConcurrency: concurrency, VerificationLimiter: b.verificationLimiter, Addresses: decoded.Addresses(), Topics: [][]common.Hash{decoded.Topic0()}}, b.logger)
	if err != nil {
		return
	}
	to := job.NextBlock + listener.BatchSize - 1
	if to < job.NextBlock || to > job.ToBlock {
		to = job.ToBlock
	}
	logs, err := scanned.FetchRange(ctx, job.NextBlock, to)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		_ = b.store.FailBackfill(ctx, job.ID, backfillRPCFailureSummary(err), time.Now().UTC())
		return
	}
	events := []core.Event{}
	deliveries := []core.Delivery{}
	for _, raw := range logs {
		event, e := decoded.Decode(ctx, raw)
		if errors.Is(e, decoder.ErrUnconfiguredEvent) {
			continue
		}
		if e != nil {
			_ = b.store.FailBackfill(ctx, job.ID, "Event decoding failed", time.Now().UTC())
			return
		}
		events = append(events, event)
		ds, _ := decoded.Destinations(event)
		for _, d := range ds {
			deliveries = append(deliveries, core.Delivery{EventID: event.ID, Destination: d.Locator, Authentication: d.Authentication, NextAttempt: time.Now().UTC()})
		}
	}
	next := to + 1
	if to == job.ToBlock {
		next = 0
	}
	_, _, _, err = b.store.SaveBackfillBatch(ctx, job.ID, events, deliveries, next, to-job.NextBlock+1, uint64(len(events)), time.Now().UTC())
	if err != nil {
		_ = b.store.FailBackfill(ctx, job.ID, "SQLite persistence failed; resume to retry", time.Now().UTC())
		if b.metrics != nil {
			b.metrics.BackfillFinished("failed")
		}
	} else if b.metrics != nil {
		b.metrics.BackfillBatch(to-job.NextBlock+1, uint64(len(events)))
		if next == 0 {
			b.metrics.BackfillFinished("completed")
		}
	}
}

func backfillRPCFailureSummary(err error) string {
	message := strings.ToLower(err.Error())
	summary := "RPC query failed; resume to retry"
	switch {
	case strings.Contains(message, "chain id"):
		summary = "RPC chain verification failed; check the listener and resume to retry"
	case strings.Contains(message, "get logs"):
		summary = "RPC log query failed; check provider range limits and resume to retry"
	case strings.Contains(message, "snapshot block") || strings.Contains(message, "verify block") || strings.Contains(message, "block hash"):
		summary = "RPC historical block verification failed; check archive access and resume to retry"
	case strings.Contains(message, "does not descend") || strings.Contains(message, "chain changed"):
		summary = "RPC returned inconsistent chain data; verify the provider and resume to retry"
	case strings.Contains(message, "timeout") || strings.Contains(message, "deadline exceeded"):
		summary = "RPC request timed out; check provider capacity and resume to retry"
	case strings.Contains(message, "unauthorized") || strings.Contains(message, "forbidden") || strings.Contains(message, "authentication"):
		summary = "RPC authentication failed; check listener credentials and resume to retry"
	}
	return summary
}
