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
	if err := p.store.SaveEventsAndCheckpoint(ctx, events, deliveries, checkpoint); err != nil {
		return fmt.Errorf("persist decoded batch: %w", err)
	}
	return nil
}
