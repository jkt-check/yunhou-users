package model

import "time"

type SocialIdentity struct {
	ID          string    `db:"id" json:"id"`
	UserID      string    `db:"user_id" json:"user_id"`
	Provider    string    `db:"provider" json:"provider"`
	ProviderUID string    `db:"provider_uid" json:"provider_uid"`
	Email       *string   `db:"email" json:"email"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}
