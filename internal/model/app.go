package model

import (
	"encoding/json"
	"time"
)

type App struct {
	AppID       string          `db:"app_id" json:"app_id"`
	Name        string          `db:"name" json:"name"`
	Description string          `db:"description" json:"description"`
	Config      json.RawMessage `db:"config" json:"config"`
	IsActive    bool            `db:"is_active" json:"is_active"`
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time       `db:"updated_at" json:"updated_at"`
}
