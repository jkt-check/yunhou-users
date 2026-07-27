package service

import (
	"context"
	"log"

	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/util"
)

// BackfillAppSecrets fills in secret_hash for any app row created before
// migration 007_app_secret.sql where the column was added NULL. Idempotent —
// rows that already have a non-empty hash are skipped. Returns the number of
// rows whose hash was newly written this call.
//
// The plaintext is NOT logged: per CLAUDE.md, server-side secrets
// (X-App-Secret, refresh tokens) are returned to the caller exactly once at
// create/rotate/login time and never appear in logs. After backfill the
// operator MUST rotate each app's secret via
// POST /admin/apps/:id/rotate-secret to obtain the plaintext — the rotate
// endpoint is the only path that surfaces a plaintext per the design
// invariant. The backfill secret is treated as already-compromised: a fresh
// rotate immediately after backfill is mandatory.
//
// Reason this lives in Go, not as a SQL migration block: bcrypt hashing needs
// util.HashSecret (DefaultCost) and the comparison side runs
// bcrypt.CompareHashAndPassword, which only accepts the Go bcrypt format.
// Postgres's pgcrypt crypt() uses an incompatible format we can't verify
// against from the auth middleware.
func BackfillAppSecrets(ctx context.Context, appRepo repo.AppRepo) (int, error) {
	// Only touch rows that need it (added NULL by migration 007 and not yet
	// backfilled). Once every row has a hash, this returns an empty slice
	// and the loop is a no-op — every restart after the first one pays only
	// the cost of the empty SELECT.
	apps, err := appRepo.ListUnhashed(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range apps {
		_, hash, err := util.GenerateSecret()
		if err != nil {
			return n, err
		}
		// BackfillSecretHash has a WHERE-secret_hash-empty guard so a manual
		// rotate that lands between ListUnhashed and this UPDATE won't be
		// silently overwritten. skipped=true means a concurrent rotate won;
		// the row is already covered and we don't touch it.
		skipped, err := appRepo.BackfillSecretHash(ctx, a.AppID, hash)
		if err != nil {
			return n, err
		}
		if skipped {
			continue
		}
		// Log only the app id (no plaintext). Operators discover the new
		// secret by hitting POST /admin/apps/:id/rotate-secret, which is the
		// single allowed plaintext-surfacing path per CLAUDE.md.
		log.Printf("app_secret_backfill: app_id=%q hash written; rotate via POST /admin/apps/%s/rotate-secret to obtain the plaintext", a.AppID, a.AppID)
		n++
	}
	return n, nil
}
