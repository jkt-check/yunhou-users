package model

import (
	"time"

	"github.com/lib/pq"
)

type Session struct {
	ID           string         `db:"id" json:"id"`
	UserID       string         `db:"user_id" json:"user_id"`
	AppID        string         `db:"app_id" json:"app_id"`
	RefreshToken string         `db:"refresh_token" json:"-"`
	Scope        pq.StringArray `db:"scope" json:"scope"`
	Revoked      bool           `db:"revoked" json:"revoked"`
	ExpiresAt    time.Time      `db:"expires_at" json:"expires_at"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
}
