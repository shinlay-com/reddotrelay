package core

import "time"

type DeliveryRequeueAudit struct {
	ActorID   string
	ActorName string
	ActorRole APIKeyRole
}

type DeliveryRequeueAuditEntry struct {
	Sequence         uint64
	ID               string
	ActorID          string
	ActorName        string
	ActorRole        APIKeyRole
	DeliveryID       string
	EventID          string
	PreviousAttempts int
	CreatedAt        time.Time
}
