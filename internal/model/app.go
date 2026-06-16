package model

import (
	"time"

	"github.com/lib/pq"
)

type App struct {
	ID           string         `db:"id" json:"id"`
	Secret       string         `db:"secret" json:"-"`
	Name         string         `db:"name" json:"name"`
	RedirectURIs pq.StringArray `db:"redirect_uris" json:"redirect_uris"`
	Providers    pq.StringArray `db:"providers" json:"providers"`
	DefaultPlan  string         `db:"default_plan" json:"default_plan"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time      `db:"updated_at" json:"updated_at"`
}
