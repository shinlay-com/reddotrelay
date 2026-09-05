package core

import "time"

type BackfillState string

const (
	BackfillQueued    BackfillState = "queued"
	BackfillRunning   BackfillState = "running"
	BackfillPaused    BackfillState = "paused"
	BackfillCompleted BackfillState = "completed"
	BackfillFailed    BackfillState = "failed"
	BackfillCancelled BackfillState = "cancelled"
)

type BackfillJob struct {
	ID                string        `json:"id"`
	ListenerID        string        `json:"rpcListenerId"`
	ChainID           uint64        `json:"chainId"`
	Mode              string        `json:"mode"`
	FromBlock         uint64        `json:"fromBlock"`
	ToBlock           uint64        `json:"toBlock"`
	NextBlock         uint64        `json:"nextBlock"`
	ContractIDs       []string      `json:"contractIds"`
	EventIDs          []string      `json:"eventIds"`
	ConfigRevision    uint64        `json:"configurationRevision"`
	Snapshot          []byte        `json:"-"`
	Destinations      []string      `json:"destinations"`
	State             BackfillState `json:"state"`
	ProcessedBlocks   uint64        `json:"processedBlocks"`
	DiscoveredEvents  uint64        `json:"discoveredEvents"`
	CreatedEvents     uint64        `json:"createdEvents"`
	CreatedDeliveries uint64        `json:"createdDeliveries"`
	Duplicates        uint64        `json:"duplicates"`
	FailureSummary    string        `json:"failureSummary,omitempty"`
	CreatedAt         time.Time     `json:"createdAt"`
	UpdatedAt         time.Time     `json:"updatedAt"`
	StartedAt         *time.Time    `json:"startedAt,omitempty"`
	CompletedAt       *time.Time    `json:"completedAt,omitempty"`
}

type BackfillAudit struct {
	Sequence  uint64     `json:"sequence"`
	ID        string     `json:"id"`
	JobID     string     `json:"jobId"`
	ActorID   string     `json:"actorId"`
	ActorName string     `json:"actorName"`
	ActorRole APIKeyRole `json:"actorRole"`
	Action    string     `json:"action"`
	CreatedAt time.Time  `json:"createdAt"`
}
