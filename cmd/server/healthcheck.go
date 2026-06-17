package main

import (
	"context"
	"os"
	"time"

	"github.com/jmoiron/sqlx"
)

// runHealthcheck is invoked by the Docker HEALTHCHECK instruction via
// the binary's `-healthcheck` flag. Distroless has no wget/curl, so the
// binary itself does the DB ping and exits 0 (ok) or 1 (degraded).
func runHealthcheck(db *sqlx.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
