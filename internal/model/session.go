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
	// RevokedAt records when the session was revoked (migration 020). NULL
	// for rows revoked before the column existed — the refresh grace-window
	// logic treats a NULL RevokedAt as "outside the window".
	RevokedAt *time.Time `db:"revoked_at" json:"revoked_at,omitempty"`
	// RotatedTo links a rotated-out refresh session to the successor session
	// minted by the same rotation (migration 020). The grace-window logic in
	// AuthService.RefreshToken walks this chain to distinguish a legitimate
	// lost-response retry from a token replay. NULL for sessions revoked by
	// anything other than a rotation (logout, family revoke).
	RotatedTo  *string   `db:"rotated_to" json:"-"`
	ExpiresAt  time.Time `db:"expires_at" json:"expires_at"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}
