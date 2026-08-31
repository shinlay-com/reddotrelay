package core

import "time"

type APIKeyRole string

const (
	APIKeyAdmin    APIKeyRole = "admin"
	APIKeyReadOnly APIKeyRole = "read-only"
)

type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       APIKeyRole `json:"role"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

type APIKeyPrincipal struct {
	ID   string
	Name string
	Role APIKeyRole
}
