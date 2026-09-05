package decoder

import (
	"context"
	"errors"
	"fmt"
	"time"

	"reddotrelay/internal/core"
)

type EventStore interface {
	SaveEventsAndCheckpoint(context.Context, []core.Event, []core.Delivery, core.Checkpoint) error
}

type canonicalEventStore interface {
	SaveCanonicalBatch(context.Context, []core.Event, []core.Delivery, []core.CanonicalBlock, core.Checkpoint, uint64) error
}

type RoutedDecoder interface {
	core.Decoder
	Destinations(core.Event) ([]core.WebhookDestination, error)
}

type Processor struct {
	decoder RoutedDecoder
	store   EventStore
	now     func() time.Time
}

func NewProcessor(decoder RoutedDecoder, store EventStore) *Processor {
	return &Processor{decoder: decoder, store: store, now: time.Now}
}

func (p *Processor) ProcessBatch(ctx context.Context, logs []core.RawLog, checkpoint core.Checkpoint) error {
	return p.process(ctx, logs, nil, checkpoint, 0)
}

func (p *Processor) ProcessCanonicalBatch(ctx context.Context, logs []core.RawLog, blocks []core.CanonicalBlock, checkpoint core.Checkpoint, retain uint64) error {
	return p.process(ctx, logs, blocks, checkpoint, retain)
}

func (p *Processor) process(ctx context.Context, logs []core.RawLog, blocks []core.CanonicalBlock, checkpoint core.Checkpoint, retain uint64) error {
	events := make([]core.Event, 0, len(logs))
	deliveries := make([]core.Delivery, 0, len(logs))
	for _, raw := range logs {
		event, err := p.decoder.Decode(ctx, raw)
		if errors.Is(err, ErrUnconfiguredEvent) {
			continue
		}
		if err != nil {
			return fmt.Errorf("decode log %s/%d: %w", raw.TransactionHash, raw.LogIndex, err)
		}
		events = append(events, event)
		destinations, err := p.decoder.Destinations(event)
		if err != nil {
			return fmt.Errorf("route event %s/%d: %w", raw.TransactionHash, raw.LogIndex, err)
		}
		for _, destination := range destinations {
			deliveries = append(deliveries, core.Delivery{
				EventID: event.ID, Destination: destination.Locator, Authentication: destination.Authentication, NextAttempt: p.now().UTC(),
			})
		}
	}
	var err error
	if store, ok := p.store.(canonicalEventStore); ok && blocks != nil {
		err = store.SaveCanonicalBatch(ctx, events, deliveries, blocks, checkpoint, retain)
	} else {
		err = p.store.SaveEventsAndCheckpoint(ctx, events, deliveries, checkpoint)
	}
	if err != nil {
		return fmt.Errorf("persist decoded batch: %w", err)
	}
	return nil
}
