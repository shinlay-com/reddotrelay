package core

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

func NewConfigID() string { return uuid.NewString() }

// RPCListenerSnapshot is the complete durable configuration used to build
// scanner runtimes. Revision changes after every successful mutation.
type RPCListenerSnapshot struct {
	Revision       uint64
	UpdatedAt      time.Time
	GlobalWebhooks []WebhookConfig
	Listeners      []RPCListener
}

type RPCListener struct {
	ID                string
	Name              string
	Paused            bool
	ChainID           uint64
	RPCURL            string
	RPCURLRef         string
	RPCAuthentication RPCAuthentication
	StartBlock        uint64
	BatchSize         uint64
	PollInterval      time.Duration
	Confirmations     uint64
	ReorgDepth        uint64
	RPCRetryAttempts  int
	RPCRetryBackoff   time.Duration
	RPCTimeout        time.Duration
	TLS               ListenerTLSConfig
	Webhooks          []WebhookConfig
	Contracts         []ContractConfig
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RPCAuthentication describes outbound authentication. Secret is only present
// in process memory after SQLite decryption; API response types must omit it.
type RPCAuthentication struct {
	Type        string
	Username    string
	HeaderName  string
	Secret      string
	TokenURL    string
	TokenAPIKey string
}

type ListenerTLSConfig struct {
	CAPEM              string
	ServerName         string
	InsecureSkipVerify bool
}

type ContractConfig struct {
	ID        string
	Address   string
	ABI       json.RawMessage
	Webhooks  []WebhookConfig
	Events    []EventConfig
	CreatedAt time.Time
	UpdatedAt time.Time
}

type EventConfig struct {
	ID        string
	Selector  string
	Webhooks  []WebhookConfig
	CreatedAt time.Time
	UpdatedAt time.Time
}

type WebhookConfig struct {
	ID             string
	URL            string
	URLRef         string
	Authentication WebhookAuthentication
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type WebhookAuthentication struct {
	Type      string
	SecretRef string
	KeyID     string
}

type WebhookDestination struct {
	Locator        string
	Authentication WebhookAuthentication
}
