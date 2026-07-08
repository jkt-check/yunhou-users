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
// Each newly-generated plaintext is logged once via stdout. Operators running
// the migration in a deploy pipeline MUST capture the deploy log (or grep for
// "app_secret_backfill") to record the plaintexts, then immediately rotate
// each app's secret via POST /admin/apps/:id/rotate-secret so the
// backfill-secret never lives long enough to be considered "production".
//
// Caveat: log lines persist in the deploy log aggregator (CloudWatch, journald,
// etc.) well beyond the backfill moment. Anyone with log-read access can read
// the plaintext. If your log retention is multi-day or longer, the rotate step
// is not optional — treat backfill secrets as already compromised.
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
		plaintext, hash, err := util.GenerateSecret()
		if err != nil {
			return n, err
		}
		// BackfillSecretHash has a WHERE-secret_hash-empty guard so a manual
		// rotate that lands between ListUnhashed and this UPDATE won't be
		// silently overwritten. skipped=true means a concurrent rotate won;
		// we do NOT log the plaintext in that case — it doesn't reflect what
		// the DB holds, and operators capturing it would lose auth on the
		// manual rotate.
		skipped, err := appRepo.BackfillSecretHash(ctx, a.AppID, hash)
		if err != nil {
			return n, err
		}
		if skipped {
			continue
		}
		// Plaintext surfaces here exactly once. Operators MUST scrape this
		// from the deploy log and rotate immediately after — see caveat above.
		log.Printf("app_secret_backfill: app_id=%q plaintext=%q (rotate via POST /admin/apps/%s/rotate-secret IMMEDIATELY after capture)", a.AppID, plaintext, a.AppID)
		n++
	}
	return n, nil
}