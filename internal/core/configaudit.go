package core

import "time"

const (
	AuditActionCreate = "create"
	AuditActionUpdate = "update"
	AuditActionDelete = "delete"
	AuditActionImport = "import"
	AuditActionPause  = "pause"
	AuditActionResume = "resume"

	AuditResourceRPCListener   = "rpc-listener"
	AuditResourceContract      = "contract-config"
	AuditResourceEvent         = "event-config"
	AuditResourceWebhook       = "webhook-config"
	AuditResourceConfiguration = "configuration"
)

// RPCListenerAudit describes one configuration mutation without retaining
// request bodies or configuration values.
type RPCListenerAudit struct {
	ActorID          string
	ActorName        string
	ActorRole        APIKeyRole
	Action           string
	ResourceKind     string
	ResourceID       string
	ParentListenerID string
}

type RPCListenerAuditEntry struct {
	Sequence         uint64
	ID               string
	ActorID          string
	ActorName        string
	ActorRole        APIKeyRole
	Action           string
	ResourceKind     string
	ResourceID       string
	ParentListenerID string
	PreviousRevision uint64
	NewRevision      uint64
	CreatedAt        time.Time
}
