package core

import "time"

type User struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	Role        APIKeyRole `json:"role"`
	Enabled     bool       `json:"enabled"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
}
