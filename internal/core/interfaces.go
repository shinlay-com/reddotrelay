package core

import (
	"context"
	"errors"
	"time"
)

var ErrCheckpointNotFound = errors.New("checkpoint not found")

// Store defines the durability boundary. SaveEventsAndCheckpoint must atomically
// persist events, their initial deliveries, and the new checkpoint.
type Store interface {
	SaveEventsAndCheckpoint(context.Context, []Event, []Delivery, Checkpoint) error
	Checkpoint(context.Context, uint64) (Checkpoint, error)
	Rewind(context.Context, Checkpoint) error
	ResetFrom(context.Context, uint64, uint64) error
	DueDeliveries(context.Context, time.Time, int) ([]OutboxItem, error)
	ClaimDueDeliveries(context.Context, time.Time, time.Duration, int) ([]OutboxItem, error)
	MarkDeliveryDelivered(context.Context, EventID, string, string, time.Time, int) error
	ScheduleDeliveryRetry(context.Context, EventID, string, string, time.Time, string, int) error
	MarkDeliveryDead(context.Context, EventID, string, string, string, int) error
}

type LogSource interface {
	LatestBlock(context.Context) (uint64, error)
	Logs(context.Context, uint64, uint64) ([]RawLog, error)
}

type RawLog struct {
	ChainID         uint64
	BlockNumber     uint64
	BlockHash       string
	TransactionHash string
	LogIndex        uint64
	Address         string
	Topics          []string
	Data            []byte
}

type Decoder interface {
	Decode(context.Context, RawLog) (Event, error)
}

type Deliverer interface {
	Deliver(context.Context, Delivery, Event) error
}
