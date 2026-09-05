package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"reddotrelay/internal/core"
	"reddotrelay/internal/secrets"
	"reddotrelay/internal/store/sqlite"
)

type scannerSkipService struct {
	store   *sqlite.Store
	key     [32]byte
	stopped func(string, uint64) bool
	fetch   func(context.Context, core.RPCListener, *uint64) (core.CanonicalBlock, error)
	now     func() time.Time
}
type scannerSkipClaims struct {
	ListenerID string              `json:"rpcListenerId"`
	ActorID    string              `json:"actorId"`
	Revision   uint64              `json:"revision"`
	Previous   *core.Checkpoint    `json:"previous"`
	Target     core.CanonicalBlock `json:"target"`
	ExpiresAt  time.Time           `json:"expiresAt"`
}
type scannerSkipPreview struct {
	Token         string    `json:"token"`
	ListenerID    string    `json:"rpcListenerId"`
	ChainID       uint64    `json:"chainId"`
	Revision      uint64    `json:"configurationRevision"`
	PreviousBlock *uint64   `json:"previousBlock"`
	FromBlock     uint64    `json:"fromBlock"`
	ToBlock       uint64    `json:"toBlock"`
	Blocks        uint64    `json:"blocks"`
	BlockHash     string    `json:"blockHash"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Confirmation  string    `json:"confirmation"`
}

func newScannerSkipService(store *sqlite.Store, manager *runtimeManager) *scannerSkipService {
	s := &scannerSkipService{store: store, now: time.Now}
	// A process-local signing key means previews cannot survive a restart.
	if _, err := rand.Read(s.key[:]); err != nil {
		panic("cannot initialize skip preview signing key")
	}
	s.stopped = func(id string, revision uint64) bool {
		if manager == nil {
			return false
		}
		status := manager.Status()
		if !status.InitialReconcileComplete || status.DesiredRevision != revision {
			return false
		}
		for _, listener := range status.RPCListeners {
			if listener.ID == id {
				return listener.State == "paused"
			}
		}
		return false
	}
	return s
}

func (s *scannerSkipService) sign(claims scannerSkipClaims) string {
	body, _ := json.Marshal(claims)
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *scannerSkipService) verify(token string) (scannerSkipClaims, error) {
	var claims scannerSkipClaims
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return claims, errors.New("invalid preview")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, err
	}
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return claims, errors.New("invalid preview")
	}
	if err = json.Unmarshal(body, &claims); err != nil {
		return claims, err
	}
	if !s.now().Before(claims.ExpiresAt) {
		return claims, errors.New("expired preview")
	}
	return claims, nil
}

func (s *scannerSkipService) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireAdmin(w, r) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/rpc-listeners/"), "/")
	if len(parts) < 2 || !canonicalUUID(parts[0]) {
		writeAPIError(w, 404, "resource not found")
		return
	}
	if len(parts) == 2 && parts[1] == "skip-audit" {
		s.audit(w, r, parts[0])
		return
	}
	preview := len(parts) == 3 && parts[1] == "skip-to-head" && parts[2] == "preview"
	if !preview && !(len(parts) == 2 && parts[1] == "skip-to-head") {
		writeAPIError(w, 404, "resource not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, 405, "method not allowed")
		return
	}
	if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		writeAPIError(w, 415, "Content-Type must be application/json")
		return
	}
	var input *struct {
		Token        string `json:"token"`
		Confirmation string `json:"confirmation"`
	}
	reader := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	reader.DisallowUnknownFields()
	if err := reader.Decode(&input); err != nil || input == nil {
		writeAPIError(w, 400, "invalid request body")
		return
	}
	if err := reader.Decode(new(any)); err != io.EOF {
		writeAPIError(w, 400, "invalid request body")
		return
	}
	revision, _, listener, ok := loadListenerMutation(s.store, parts[0], w, r)
	if !ok {
		return
	}
	if !listener.Paused || !s.stopped(listener.ID, revision) {
		writeAPIError(w, 409, "Pause this listener and wait for its runtime to stop, then preview again.")
		return
	}
	actor := r.Context().Value(apiKeyPrincipalContextKey{}).(core.APIKeyPrincipal)
	// Only this bounded administrative operation may exceed the normal server
	// write timeout. Leave time to return an error after the operation expires.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(65 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeAPIError(w, 500, "could not establish skip response deadline")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	// A request-local copy shares one client/token cache between head and old
	// checkpoint reads without sharing mutable state between administrators.
	operation := *s
	s = &operation
	if s.fetch == nil {
		fetch, closeClient, err := openSkipReader(ctx, *listener)
		if err != nil {
			writeSkipRPCError(w, ctx, "opening the listener RPC", err)
			return
		}
		defer closeClient()
		s.fetch = fetch
	}
	if preview {
		if input.Token != "" || input.Confirmation != "" {
			writeAPIError(w, 400, "preview requires an empty object")
			return
		}
		target, err := s.fetch(ctx, *listener, nil)
		if err != nil {
			writeSkipRPCError(w, ctx, "verifying the confirmed head", err)
			return
		}
		var previous *core.Checkpoint
		cp, err := s.store.Checkpoint(ctx, listener.ChainID)
		if err == nil {
			previous = &cp
		} else if !errors.Is(err, core.ErrCheckpointNotFound) {
			writeAPIError(w, 500, "could not read checkpoint")
			return
		}
		from := listener.StartBlock
		if previous != nil {
			from = previous.BlockNumber + 1
		}
		if target.Number < from || target.Number < listener.StartBlock || target.Number >= math.MaxInt64 {
			writeAPIError(w, 409, "There are no eligible confirmed blocks to skip.")
			return
		}
		if !s.verifyPrevious(ctx, *listener, previous, w) {
			return
		}
		claims := scannerSkipClaims{ListenerID: listener.ID, ActorID: actor.ID, Revision: revision, Previous: previous, Target: target, ExpiresAt: s.now().UTC().Add(5 * time.Minute)}
		out := scannerSkipPreview{Token: s.sign(claims), ListenerID: listener.ID, ChainID: listener.ChainID, Revision: revision, FromBlock: from, ToBlock: target.Number, Blocks: target.Number - from + 1, BlockHash: target.Hash, ExpiresAt: claims.ExpiresAt, Confirmation: fmt.Sprintf("SKIP %d", target.Number)}
		if previous != nil {
			out.PreviousBlock = &previous.BlockNumber
		}
		w.Header().Set("ETag", revisionETag(revision))
		writeJSON(w, 200, out)
		return
	}
	claims, err := s.verify(input.Token)
	if err != nil || claims.ListenerID != listener.ID || claims.ActorID != actor.ID || claims.Revision != revision || claims.Target.ChainID != listener.ChainID {
		writeAPIError(w, 409, "Preview is invalid, expired, or stale; preview again.")
		return
	}
	if input.Confirmation != fmt.Sprintf("SKIP %d", claims.Target.Number) {
		writeAPIError(w, 422, "Type the exact preview confirmation phrase.")
		return
	}
	// Recheck exactly the previewed block; never silently expand the skipped range.
	target, err := s.fetch(ctx, *listener, &claims.Target.Number)
	if err != nil {
		writeSkipRPCError(w, ctx, "reverifying the previewed confirmed block", err)
		return
	}
	if target != claims.Target {
		writeAPIError(w, 409, "The confirmed chain changed; preview again.")
		return
	}
	if !s.verifyPrevious(ctx, *listener, claims.Previous, w) {
		return
	}
	result, err := s.store.SkipScannerToHead(ctx, listener.ID, revision, claims.Previous, target, actor, s.now())
	switch {
	case errors.Is(err, sqlite.ErrRevisionConflict):
		writeAPIError(w, 412, "configuration changed; preview again")
		return
	case errors.Is(err, sqlite.ErrScannerSkipConflict):
		writeAPIError(w, 409, err.Error())
		return
	case errors.Is(err, sqlite.ErrActiveBackfill):
		writeAPIError(w, 409, "Finish or cancel active backfills on this chain before skipping.")
		return
	case err != nil:
		writeAPIError(w, 500, "skip could not be persisted; no checkpoint was changed")
		return
	}
	w.Header().Set("ETag", revisionETag(result.Revision))
	writeJSON(w, 200, result)
}

// A forward skip must not conceal an already-orphaned checkpoint. Let normal
// reorg recovery repair that branch before permitting a new forward anchor.
func (s *scannerSkipService) verifyPrevious(ctx context.Context, listener core.RPCListener, previous *core.Checkpoint, w http.ResponseWriter) bool {
	if previous == nil {
		return true
	}
	block, err := s.fetch(ctx, listener, &previous.BlockNumber)
	if err != nil {
		writeSkipRPCError(w, ctx, fmt.Sprintf("verifying existing checkpoint block %d", previous.BlockNumber), err)
		return false
	}
	if block.Number != previous.BlockNumber || block.Hash != previous.BlockHash || block.ChainID != previous.ChainID {
		writeAPIError(w, 409, "The existing checkpoint is no longer canonical. Resume normal scanning for reorg recovery before skipping.")
		return false
	}
	return true
}

func (s *scannerSkipService) audit(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
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
	if before > math.MaxInt64 {
		writeAPIError(w, 400, "before is out of range")
		return
	}
	entries, err := s.store.ScannerSkipAudit(r.Context(), id, limit+1, before)
	if err != nil {
		writeAPIError(w, 500, "could not read skip audit")
		return
	}
	out := map[string]any{}
	if len(entries) > limit {
		entries = entries[:limit]
		out["nextBefore"] = entries[len(entries)-1].Sequence
	}
	out["entries"] = entries
	writeJSON(w, 200, out)
}

func fetchSkipHead(ctx context.Context, listener core.RPCListener, target *uint64) (core.CanonicalBlock, error) {
	fetch, closeClient, err := openSkipReader(ctx, listener)
	if err != nil {
		return core.CanonicalBlock{}, err
	}
	defer closeClient()
	return fetch(ctx, listener, target)
}

func openSkipReader(ctx context.Context, listener core.RPCListener) (func(context.Context, core.RPCListener, *uint64) (core.CanonicalBlock, error), func(), error) {
	url := listener.RPCURL
	if listener.RPCURLRef != "" {
		var err error
		url, err = secrets.New().Resolve(ctx, listener.RPCURLRef)
		if err != nil {
			return nil, nil, err
		}
	}
	if !validAbsoluteHTTPURL(url) {
		return nil, nil, errors.New("invalid RPC URL")
	}
	client, err := dialListenerRPC(ctx, url, listener.TLS, listener.RPCAuthentication)
	if err != nil {
		return nil, nil, err
	}
	timeout := listener.RPCTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	timeout = min(timeout, 30*time.Second)
	var initialized bool
	var confirmed uint64
	read := func(ctx context.Context, tag string) (core.CanonicalBlock, error) {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		block, err := readSkipBlock(callCtx, client, listener.ChainID, tag)
		if callCtx.Err() != nil {
			return block, callCtx.Err()
		}
		return block, err
	}
	fetch := func(ctx context.Context, _ core.RPCListener, target *uint64) (core.CanonicalBlock, error) {
		if !initialized {
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			chain, err := client.ChainID(callCtx)
			if callCtx.Err() != nil {
				err = callCtx.Err()
			}
			cancel()
			if err != nil {
				return core.CanonicalBlock{}, err
			}
			if chain == nil || !chain.IsUint64() || chain.Uint64() != listener.ChainID {
				return core.CanonicalBlock{}, errors.New("chain ID mismatch")
			}
			latest, err := read(ctx, "latest")
			if err != nil {
				return latest, err
			}
			if latest.Number < listener.Confirmations {
				return latest, errors.New("no confirmed head")
			}
			confirmed = latest.Number - listener.Confirmations
			initialized = true
		}
		number := confirmed
		if target != nil {
			if *target > confirmed {
				return core.CanonicalBlock{}, errors.New("target is no longer confirmed")
			}
			number = *target
		}
		// Read the numeric block even at zero confirmations to bind a stable number.
		return read(ctx, hexutil.EncodeUint64(number))
	}
	return fetch, client.Close, nil
}

// Never include provider error text, response bodies, or credential-bearing URLs.
func writeSkipRPCError(w http.ResponseWriter, ctx context.Context, stage string, err error) {
	reason := "RPC request failed"
	var timeout net.Error
	var httpError rpc.HTTPError
	var syntax *json.SyntaxError
	var invalidType *json.UnmarshalTypeError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		reason = "RPC verification timed out; check endpoint latency and the listener RPC timeout"
	case errors.Is(err, context.Canceled):
		reason = "RPC verification was cancelled"
	case errors.As(err, &httpError):
		switch httpError.StatusCode {
		case 401, 403:
			reason = "RPC authentication was rejected; check the listener credentials"
		case 429:
			reason = "RPC provider rate limit reached; wait before retrying"
		default:
			reason = fmt.Sprintf("RPC endpoint returned HTTP %d", httpError.StatusCode)
		}
	case strings.Contains(err.Error(), "RPC provider JWT"), strings.Contains(err.Error(), "RPC provider token"):
		reason = "RPC provider JWT authentication failed; check the token endpoint and credentials"
	case errors.As(err, &syntax), errors.As(err, &invalidType):
		reason = "RPC returned malformed block data"
	default:
		switch err.Error() {
		case "incomplete canonical block", "missing parent hash", "wrong block number":
			reason = "RPC returned missing or invalid block number, hash, or parent hash"
		case "chain ID mismatch":
			reason = "RPC chain ID does not match the listener"
		case "no confirmed head", "target is no longer confirmed":
			reason = "The requested block is not currently confirmed"
		}
	}
	writeAPIError(w, 502, reason+" while "+stage+"; no checkpoint was changed.")
}

func readSkipBlock(ctx context.Context, client *ethclient.Client, chain uint64, tag string) (core.CanonicalBlock, error) {
	var value *struct {
		Number     *hexutil.Big `json:"number"`
		Hash       *common.Hash `json:"hash"`
		ParentHash *common.Hash `json:"parentHash"`
	}
	if err := client.Client().CallContext(ctx, &value, "eth_getBlockByNumber", tag, false); err != nil {
		return core.CanonicalBlock{}, err
	}
	if value == nil || value.Number == nil || !value.Number.ToInt().IsUint64() || value.Hash == nil || *value.Hash == (common.Hash{}) || value.ParentHash == nil {
		return core.CanonicalBlock{}, errors.New("incomplete canonical block")
	}
	number := value.Number.ToInt().Uint64()
	if number > 0 && *value.ParentHash == (common.Hash{}) {
		return core.CanonicalBlock{}, errors.New("missing parent hash")
	}
	if tag != "latest" && tag != hexutil.EncodeUint64(number) {
		return core.CanonicalBlock{}, errors.New("wrong block number")
	}
	return core.CanonicalBlock{ChainID: chain, Number: number, Hash: value.Hash.Hex(), ParentHash: value.ParentHash.Hex()}, nil
}
