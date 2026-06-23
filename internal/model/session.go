package model

import (
	"errors"
	"time"

	"github.com/lib/pq"
)

// ErrSessionAlreadyRevoked is returned by the repo layer when a
// refresh-token rotation is attempted on a session that was already revoked.
// It lives in the model package so both repo and service can import it
// without creating an import cycle.
var ErrSessionAlreadyRevoked = errors.New("session already revoked")

type Session struct {
	ID           string         `db:"id" json:"id"`
	UserID       string         `db:"user_id" json:"user_id"`
	AppID        string         `db:"app_id" json:"app_id"`
	SessionType  string         `db:"session_type" json:"-"`
	RefreshToken string         `db:"refresh_token" json:"-"`
	Scope        pq.StringArray `db:"scope" json:"scope"`
	Revoked      bool           `db:"revoked" json:"revoked"`
	ExpiresAt    time.Time      `db:"expires_at" json:"expires_at"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
}