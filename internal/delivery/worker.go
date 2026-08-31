package delivery

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/secrets"
)

const (
	idempotencyHeader = "Idempotency-Key"
	timestampHeader   = "RedDotRelay-Timestamp"
	signatureHeader   = "RedDotRelay-Signature"
	keyIDHeader       = "RedDotRelay-Key-Id"
)

type Outbox interface {
	ClaimDueDeliveries(context.Context, time.Time, time.Duration, int) ([]core.OutboxItem, error)
	MarkDeliveryDelivered(context.Context, core.EventID, string, string, time.Time, int) error
	ScheduleDeliveryRetry(context.Context, core.EventID, string, string, time.Time, string, int) error
	MarkDeliveryDead(context.Context, core.EventID, string, string, string, int) error
}

type Options struct {
	Workers       int
	BatchSize     int
	HTTPTimeout   time.Duration
	LeaseDuration time.Duration
	RetryBackoff  time.Duration
	MaxBackoff    time.Duration
	MaxAttempts   int
	PollInterval  time.Duration
}

type Worker struct {
	outbox   Outbox
	client   *http.Client
	options  Options
	logger   *slog.Logger
	now      func() time.Time
	resolver secretResolver
	observer Observer
}

type Observer interface{ DeliveryAttempt(outcome string) }
type noopObserver struct{}

func (noopObserver) DeliveryAttempt(string) {}

type secretResolver interface {
	Resolve(context.Context, string) (string, error)
}

func New(outbox Outbox, options Options, logger *slog.Logger) (*Worker, error) {
	return NewWithResolver(outbox, options, logger, secrets.New())
}

func NewWithResolver(outbox Outbox, options Options, logger *slog.Logger, resolver secretResolver) (*Worker, error) {
	return NewWithObserver(outbox, options, logger, resolver, noopObserver{})
}

func NewWithObserver(outbox Outbox, options Options, logger *slog.Logger, resolver secretResolver, observer Observer) (*Worker, error) {
	if outbox == nil {
		return nil, errors.New("outbox is required")
	}
	if options.Workers <= 0 || options.BatchSize <= 0 || options.HTTPTimeout <= 0 ||
		options.LeaseDuration <= options.HTTPTimeout || options.RetryBackoff <= 0 ||
		options.MaxBackoff < options.RetryBackoff || options.MaxAttempts <= 0 || options.PollInterval <= 0 {
		return nil, errors.New("invalid delivery worker options")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if resolver == nil {
		return nil, errors.New("secret resolver is required")
	}
	if observer == nil {
		observer = noopObserver{}
	}
	return &Worker{
		outbox: outbox,
		client: &http.Client{
			Timeout:       options.HTTPTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
		options: options, logger: logger, now: time.Now, resolver: resolver, observer: observer,
	}, nil
}

// Run continuously drains independently durable outbox rows. Errors are logged
// and retried on subsequent polls; they never propagate into scanner work.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.options.PollInterval)
	defer ticker.Stop()
	for {
		if err := w.DrainOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("outbox delivery cycle failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) DrainOnce(ctx context.Context) error {
	now := w.now().UTC()
	// Do not lease work that cannot start immediately. A queued item could
	// otherwise expire before its HTTP request begins and be claimed twice.
	claimLimit := min(w.options.BatchSize, w.options.Workers)
	items, err := w.outbox.ClaimDueDeliveries(ctx, now, w.options.LeaseDuration, claimLimit)
	if err != nil {
		return fmt.Errorf("claim due deliveries: %w", err)
	}
	if len(items) == 0 {
		return nil
	}

	jobs := make(chan core.OutboxItem)
	var group sync.WaitGroup
	var errorsMu sync.Mutex
	var deliveryErrors []error
	workerCount := min(w.options.Workers, len(items))
	for range workerCount {
		group.Add(1)
		go func() {
			defer group.Done()
			for item := range jobs {
				if err := w.deliver(ctx, item); err != nil {
					errorsMu.Lock()
					deliveryErrors = append(deliveryErrors, err)
					errorsMu.Unlock()
				}
			}
		}()
	}
	for _, item := range items {
		select {
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			return ctx.Err()
		case jobs <- item:
		}
	}
	close(jobs)
	group.Wait()
	return errors.Join(deliveryErrors...)
}

func (w *Worker) deliver(parent context.Context, item core.OutboxItem) error {
	payload, err := marshalPayload(item.Event)
	if err != nil {
		return w.fail(parent, item, fmt.Errorf("marshal webhook payload: %w", err), 0)
	}
	ctx, cancel := context.WithTimeout(parent, w.options.HTTPTimeout)
	defer cancel()
	destination := item.Delivery.Destination
	if secrets.IsReference(destination) {
		destination, err = w.resolver.Resolve(ctx, destination)
		if err != nil {
			return w.fail(parent, item, errors.New("webhook destination secret is unavailable"), 0)
		}
		parsed, parseErr := url.ParseRequestURI(destination)
		if parseErr != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Fragment != "" {
			return w.fail(parent, item, errors.New("resolved webhook destination is invalid"), 0)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, destination, bytes.NewReader(payload))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(idempotencyHeader, eventID(item.Event.ID))
		if item.Delivery.Authentication.Type != "" {
			if item.Delivery.Authentication.Type != "hmac-sha256" {
				return w.fail(parent, item, errors.New("unsupported webhook authentication"), 0)
			}
			secret, resolveErr := w.resolver.Resolve(ctx, item.Delivery.Authentication.SecretRef)
			if resolveErr != nil {
				return w.fail(parent, item, errors.New("webhook signing secret is unavailable"), 0)
			}
			timestamp := strconv.FormatInt(w.now().UTC().Unix(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			_, _ = mac.Write([]byte(timestamp))
			_, _ = mac.Write([]byte("."))
			_, _ = mac.Write(payload)
			req.Header.Set(timestampHeader, timestamp)
			req.Header.Set(signatureHeader, "v1="+hex.EncodeToString(mac.Sum(nil)))
			if item.Delivery.Authentication.KeyID != "" {
				req.Header.Set(keyIDHeader, item.Delivery.Authentication.KeyID)
			}
		}
		resp, requestErr := w.client.Do(req)
		if requestErr == nil {
			defer resp.Body.Close()
			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				if err := w.outbox.MarkDeliveryDelivered(parent, item.Event.ID, item.Delivery.Destination, item.Delivery.LeaseToken, w.now().UTC(), resp.StatusCode); err != nil {
					return fmt.Errorf("mark successful delivery %s: %w", eventID(item.Event.ID), err)
				}
				w.observer.DeliveryAttempt("delivered")
				return nil
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			err = httpStatusError{status: resp.StatusCode}
		} else {
			err = requestErr
		}
	}
	statusCode := 0
	var statusError httpStatusError
	if errors.As(err, &statusError) {
		statusCode = statusError.status
	}
	return w.fail(parent, item, err, statusCode)
}

func (w *Worker) fail(ctx context.Context, item core.OutboxItem, cause error, statusCode int) error {
	message := deliveryErrorMessage(cause)
	if item.Delivery.Attempts >= w.options.MaxAttempts {
		if err := w.outbox.MarkDeliveryDead(ctx, item.Event.ID, item.Delivery.Destination, item.Delivery.LeaseToken, message, statusCode); err != nil {
			return fmt.Errorf("mark dead delivery %s: %w", eventID(item.Event.ID), err)
		}
		w.logger.Warn("delivery moved to dead letter", "event_id", eventID(item.Event.ID), "error", message)
		w.observer.DeliveryAttempt("dead")
		return nil
	}
	next := w.now().UTC().Add(w.backoff(item.Delivery.Attempts))
	if err := w.outbox.ScheduleDeliveryRetry(ctx, item.Event.ID, item.Delivery.Destination, item.Delivery.LeaseToken, next, message, statusCode); err != nil {
		return fmt.Errorf("schedule delivery retry %s: %w", eventID(item.Event.ID), err)
	}
	w.logger.Warn("delivery failed; retry scheduled", "event_id", eventID(item.Event.ID), "next_attempt", next, "error", message)
	w.observer.DeliveryAttempt("retry")
	return nil
}

type httpStatusError struct {
	status int
}

func (e httpStatusError) Error() string {
	return fmt.Sprintf("webhook returned HTTP %d", e.status)
}

func deliveryErrorMessage(cause error) string {
	var statusError httpStatusError
	if errors.As(cause, &statusError) {
		return statusError.Error()
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return "webhook request timed out"
	}
	return "webhook request failed"
}

func (w *Worker) backoff(attempts int) time.Duration {
	delay := w.options.RetryBackoff
	for range max(attempts-1, 0) {
		if delay >= w.options.MaxBackoff/2 {
			return w.options.MaxBackoff
		}
		delay *= 2
	}
	return min(delay, w.options.MaxBackoff)
}

func eventID(id core.EventID) string {
	return core.EventGUID(id)
}

func marshalPayload(event core.Event) ([]byte, error) {
	decoded := json.RawMessage(event.DecodedPayload)
	if len(decoded) == 0 {
		decoded = json.RawMessage("null")
	}
	if !json.Valid(decoded) {
		return nil, errors.New("decoded event payload is not valid JSON")
	}
	return json.Marshal(struct {
		EventID         string          `json:"eventId"`
		ChainID         uint64          `json:"chainId"`
		EventName       string          `json:"eventName"`
		EventSignature  string          `json:"eventSignature"`
		Params          json.RawMessage `json:"params"`
		TransactionHash string          `json:"transactionHash"`
		BlockNumber     uint64          `json:"blockNumber"`
		BlockHash       string          `json:"blockHash"`
		LogIndex        uint64          `json:"logIndex"`
		ContractAddress string          `json:"contractAddress"`
	}{
		EventID: eventID(event.ID), ChainID: event.ID.ChainID, TransactionHash: event.ID.TransactionHash,
		LogIndex: event.ID.LogIndex, BlockNumber: event.BlockNumber, BlockHash: event.BlockHash,
		ContractAddress: event.Address, EventName: event.Name, EventSignature: event.Signature, Params: decoded,
	})
}
