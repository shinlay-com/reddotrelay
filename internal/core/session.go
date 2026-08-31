package core

import "time"

type UISessionRecord struct {
	TokenHash   []byte
	PrincipalID string
	CSRFToken   string
	CreatedAt   time.Time
	LastSeen    time.Time
}
