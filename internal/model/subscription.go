package model

import "time"

type Subscription struct {
	ID        string     `db:"id" json:"id"`
	UserID    string     `db:"user_id" json:"user_id"`
	AppID     string     `db:"app_id" json:"app_id"`
	Plan      string     `db:"plan" json:"plan"`
	Status    string     `db:"status" json:"status"`
	ExpiresAt *time.Time `db:"expires_at" json:"expires_at"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt time.Time  `db:"updated_at" json:"updated_at"`
}
