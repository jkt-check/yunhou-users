package model

import (
	"time"

	"github.com/lib/pq"
)

type Plan struct {
	ID           string         `db:"id" json:"id"` // free/monthly/quarterly/yearly
	Name         string         `db:"name" json:"name"`
	Price        float64        `db:"price" json:"price"`
	IntervalDays int            `db:"interval_days" json:"interval_days"`
	Apps         pq.StringArray `db:"apps" json:"apps"`
	IsActive     bool           `db:"is_active" json:"is_active"`
	IsDefault    bool           `db:"is_default" json:"is_default"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
}
