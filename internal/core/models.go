package core

import "time"

type EventID struct {
	ChainID         uint64
	TransactionHash string
	LogIndex        uint64
}

type Event struct {
	ID             EventID
	BlockNumber    uint64
	BlockHash      string
	Address        string
	Name           string
	Signature      string
	RawTopics      []string
	RawData        []byte
	DecodedPayload []byte
	ObservedAt     time.Time
}

type Checkpoint struct {
	ChainID     uint64
	BlockNumber uint64
	BlockHash   string
}

type Delivery struct {
	ID             string
	EventID        EventID
	Destination    string
	Authentication WebhookAuthentication
	Status         DeliveryStatus
	Attempts       int
	TotalAttempts  int
	LeaseToken     string
	NextAttempt    time.Time
	LastAttemptAt  *time.Time
	LastStatusCode int
	LastError      string
	DeliveredAt    *time.Time
}

type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliveryDelivered DeliveryStatus = "delivered"
	DeliveryDead      DeliveryStatus = "dead"
)

type OutboxItem struct {
	Delivery Delivery
	Event    Event
}

type EventHistoryCursor struct {
	ObservedAt      time.Time
	ChainID         uint64
	TransactionHash string
	LogIndex        uint64
}

type EventHistoryFilter struct {
	ChainID         *uint64
	TransactionHash string
	BlockNumber     *uint64
	Address         string
	Signature       string
	DeliveryStatus  DeliveryStatus
	Before          *EventHistoryCursor
}

type EventHistoryEntry struct {
	EventGUID string
	Event     Event
	Pending   int
	Delivered int
	Dead      int
}
