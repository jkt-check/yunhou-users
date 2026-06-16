package model

import "time"

type User struct {
	ID        string    `db:"id" json:"id"`
	Nickname  *string   `db:"nickname" json:"nickname"`
	AvatarURL *string   `db:"avatar_url" json:"avatar_url"`
	Status    string    `db:"status" json:"status"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}
