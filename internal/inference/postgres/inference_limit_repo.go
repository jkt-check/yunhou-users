package postgres

// inference_limit_repo.go — 表组 inference_rate_reservations
// (migration 052) + 业务速率闸门（RPM/TPM）的持久化。
//
// 语义（设计 §7.2 速率闸门；纯规则在 internal/inference/access）：
//
//   - RPM：滚动 60 秒内已成功提交准入的逻辑请求数（账户级 + Key 级两个
//     计数口径）。计数行在**准入事务内**写入，因此：准入事务回滚不计数、
//     一次成功准入计 1 次（内部 attempt 重试不重复计）、准入成功后上游
//     失败仍计数（释放只释放 TPM 预占，不动计数行）。
//   - TPM：账户级。窗口内已结算的归一化 token + 全部在途预占；在途预占与
//     年龄无关（长流跨 60 秒仍占额度），释放（确认零消费）才归还。
//   - 时间口径统一用**数据库时间**（now()）：多实例共享同一口径，跨实例
//     的窗口归属一致。
//
// 事务边界：速率预占与金额/并发预占共享**同一个 UnitOfWork**（准入事务），
// 结算与释放在各自的终态事务内更新速率行 —— 任一环节失败整个事务回滚，
// 不留部分速率效果（设计 §7.2）。
//
// 锁序：本文件的写入都在既有固定锁序的尾部（账户 → 请求行 → 窗口 → Key
// 预算 → 预占/租约行 → 速率行）；读取是普通 SELECT（不取行锁），因此
// 不会与任何写路径形成死锁环。

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
)

// rateWindowSeconds is the rolling RPM/TPM window (60 seconds).
const rateWindowSeconds = access.RateWindowSeconds

// ReserveRateTx evaluates the business rate limits and records the
// persistent rate reservation of the request INSIDE the caller's
// transaction (the admission transaction).
//
// The Key RPM limit is a DATABASE fact: it is read from
// inference_api_keys inside this transaction, so the limit that applies is
// always the one in force at admission time.
//
// A rejection is a typed *domain.RateLimitError (code=rate_limited) —
// never the monetary quota_exceeded (which is the signal for the wallet
// fallback: a rate rejection must not be buyable with money).
func (s *Store) ReserveRateTx(ctx context.Context, w domain.UnitOfWork, cmd domain.RateReserveCommand) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	limits := cmd.Limits
	usage, err := s.rateUsageLocked(ctx, tx, cmd)
	if err != nil {
		return err
	}
	// The Key RPM limit is a DATABASE fact (read in the same statement as the
	// aggregates — one snapshot, one round trip).
	limits.KeyRPM = usage.KeyRPMLimit
	rle := access.EvaluateRateLimits(limits, usage, cmd.ReservedTokens)
	if rle != nil {
		// Rejection: only now compute the recovery instants (the common
		// path — an admitted request — never pays for them).
		if err := s.fillRecoverySeconds(ctx, tx, cmd, limits, usage, rle); err != nil {
			return err
		}
		rle.RetryAfterSeconds = access.RecoverySeconds(rle.Blocks)
		return rle
	}
	// The account row carries the RPM count AND the account-level TPM
	// reservation; the key row carries the per-key RPM count only.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_rate_reservations
		 (request_id, billing_account_id, api_key_id, scope, counted_at, reserved_tokens, state)
		 VALUES ($1, $2, NULL, 'account', now(), $3, 'reserved')`,
		cmd.RequestID, cmd.BillingAccountID, cmd.ReservedTokens); err != nil {
		return rateStoreError("rate: insert account reservation", err)
	}
	if cmd.APIKeyID != nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO inference_rate_reservations
			 (request_id, billing_account_id, api_key_id, scope, counted_at, reserved_tokens, state)
			 VALUES ($1, $2, $3, 'key', now(), 0, 'reserved')`,
			cmd.RequestID, cmd.BillingAccountID, *cmd.APIKeyID); err != nil {
			return rateStoreError("rate: insert key reservation", err)
		}
	}
	return nil
}

// rateUsageLocked reads the rolling-window aggregates AND the customer
// Key's RPM limit in ONE statement (one snapshot, one round trip). The
// recovery instants are NOT read here — they are only needed on a
// rejection, and the common path (an admitted request) must not pay for
// them.
func (s *Store) rateUsageLocked(ctx context.Context, tx *sqlx.Tx, cmd domain.RateReserveCommand) (domain.RateUsage, error) {
	var usage domain.RateUsage
	var accountCount, keyCount, inflight, settled int64
	var keyRPM sql.NullInt64
	err := tx.QueryRowxContext(ctx,
		`SELECT
		   (SELECT COUNT(*) FROM inference_rate_reservations
		     WHERE billing_account_id = $1 AND scope = 'account'
		       AND counted_at > now() - make_interval(secs => $3)),
		   (SELECT COUNT(*) FROM inference_rate_reservations
		     WHERE api_key_id = $2 AND scope = 'key'
		       AND counted_at > now() - make_interval(secs => $3)),
		   (SELECT COALESCE(SUM(reserved_tokens), 0) FROM inference_rate_reservations
		     WHERE billing_account_id = $1 AND scope = 'account' AND state = 'reserved'),
		   (SELECT COALESCE(SUM(settled_tokens), 0) FROM inference_rate_reservations
		     WHERE billing_account_id = $1 AND scope = 'account' AND state = 'settled'
		       AND settled_at > now() - make_interval(secs => $3)),
		   (SELECT rpm_limit FROM inference_api_keys WHERE id = $2)`,
		cmd.BillingAccountID, cmd.APIKeyID, rateWindowSeconds).
		Scan(&accountCount, &keyCount, &inflight, &settled, &keyRPM)
	if err != nil {
		return usage, rateStoreError("rate: read window usage", err)
	}
	usage.AccountRPMCount = int(accountCount)
	usage.KeyRPMCount = int(keyCount)
	usage.InflightTokens = inflight
	usage.SettledTokens = settled
	usage.KeyRPMLimit = int(keyRPM.Int64)
	return usage, nil
}

// fillRecoverySeconds fills the determinable recovery instant of every
// blocking constraint (a rejection only — the admitted path never pays for
// these window scans). A constraint without a determinable instant keeps
// RecoverySeconds = 0, which makes the Retry-After disappear (no invented
// countdown).
func (s *Store) fillRecoverySeconds(ctx context.Context, tx *sqlx.Tx, cmd domain.RateReserveCommand,
	limits domain.RateLimits, usage domain.RateUsage, rle *domain.RateLimitError) error {
	for i := range rle.Blocks {
		b := &rle.Blocks[i]
		var secs int
		var err error
		switch b.Kind {
		case access.RateKindAccountRPM:
			secs, err = rpmRecoverySeconds(ctx, tx, cmd.BillingAccountID, nil, limits.AccountRPM)
		case access.RateKindPolicyRPM:
			secs, err = rpmRecoverySeconds(ctx, tx, cmd.BillingAccountID, nil, limits.PolicyRPM)
		case access.RateKindKeyRPM:
			secs, err = rpmRecoverySeconds(ctx, tx, cmd.BillingAccountID, cmd.APIKeyID, limits.KeyRPM)
		case access.RateKindPolicyTPM:
			// The TPM recovers deterministically only when NOTHING is in
			// flight: an in-flight hold's real consumption is unknown, so
			// no second may be promised (RecoverySeconds stays 0).
			if usage.InflightTokens == 0 {
				secs, err = tpmRecoverySeconds(ctx, tx, cmd.BillingAccountID, cmd.ReservedTokens, limits.PolicyTPM)
			}
		}
		if err != nil {
			return err
		}
		b.RecoverySeconds = secs
	}
	return nil
}

// rpmRecoverySeconds returns the instant (in seconds, from the database
// clock) at which ONE more request fits under the limit: the
// (count-limit+1)-th oldest counted row leaves the rolling window. It
// returns 0 when the moment is not determinable.
func rpmRecoverySeconds(ctx context.Context, tx *sqlx.Tx, accountID string, keyID *string, limit int) (int, error) {
	var secs sql.NullInt64
	var err error
	if keyID == nil {
		err = tx.QueryRowxContext(ctx,
			`WITH rows AS (
			   SELECT counted_at,
			          row_number() OVER (ORDER BY counted_at ASC) AS rn,
			          COUNT(*) OVER () AS n
			   FROM inference_rate_reservations
			   WHERE billing_account_id = $1 AND scope = 'account'
			     AND counted_at > now() - make_interval(secs => $3)
			 )
			 SELECT CEIL(EXTRACT(EPOCH FROM (counted_at + make_interval(secs => $3) - now())))::bigint
			 FROM rows WHERE rn = GREATEST(n - $2 + 1, 1) LIMIT 1`,
			accountID, limit, rateWindowSeconds).Scan(&secs)
	} else {
		err = tx.QueryRowxContext(ctx,
			`WITH rows AS (
			   SELECT counted_at,
			          row_number() OVER (ORDER BY counted_at ASC) AS rn,
			          COUNT(*) OVER () AS n
			   FROM inference_rate_reservations
			   WHERE api_key_id = $1 AND scope = 'key'
			     AND counted_at > now() - make_interval(secs => $3)
			 )
			 SELECT CEIL(EXTRACT(EPOCH FROM (counted_at + make_interval(secs => $3) - now())))::bigint
			 FROM rows WHERE rn = GREATEST(n - $2 + 1, 1) LIMIT 1`,
			*keyID, limit, rateWindowSeconds).Scan(&secs)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, rateStoreError("rate: rpm recovery", err)
	}
	if !secs.Valid {
		return 0, nil
	}
	return int(secs.Int64), nil
}

// tpmRecoverySeconds returns the instant (in seconds, from the database
// clock) at which the settled tokens leave the window and the TPM fits
// again. It is only meaningful when NOTHING is in flight (an in-flight
// hold's real consumption is unknown → no second may be promised).
func tpmRecoverySeconds(ctx context.Context, tx *sqlx.Tx, accountID string, reserved int64, limit int64) (int, error) {
	var secs sql.NullInt64
	err := tx.QueryRowxContext(ctx,
		`WITH rows AS (
		   SELECT settled_at, settled_tokens,
		          SUM(settled_tokens) OVER (ORDER BY settled_at ASC) AS cum,
		          SUM(settled_tokens) OVER () AS total
		   FROM inference_rate_reservations
		   WHERE billing_account_id = $1 AND scope = 'account' AND state = 'settled'
		     AND settled_at > now() - make_interval(secs => $4)
		 )
		 SELECT CEIL(EXTRACT(EPOCH FROM (settled_at + make_interval(secs => $4) - now())))::bigint
		 FROM rows WHERE total - cum + $2 <= $3
		 ORDER BY settled_at ASC LIMIT 1`,
		accountID, reserved, limit, rateWindowSeconds).Scan(&secs)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, rateStoreError("rate: tpm recovery", err)
	}
	if !secs.Valid {
		return 0, nil
	}
	return int(secs.Int64), nil
}

// settleRateTx converts the request's rate reservation from in-flight to
// settled: the TPM hold is replaced by the ACTUAL normalized token
// consumption (an over-hold is counted in full — never clamped to the
// hold). The RPM count is untouched: the request was admitted.
func (s *Store) settleRateTx(ctx context.Context, tx *sqlx.Tx, requestID string, tokens int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_rate_reservations
		 SET state = 'settled', settled_tokens = $2, settled_at = now()
		 WHERE request_id = $1 AND scope = 'account' AND state = 'reserved'`,
		requestID, tokens); err != nil {
		return rateStoreError("rate: settle account reservation", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_rate_reservations
		 SET state = 'settled', settled_at = now()
		 WHERE request_id = $1 AND scope = 'key' AND state = 'reserved'`,
		requestID); err != nil {
		return rateStoreError("rate: settle key reservation", err)
	}
	return nil
}

// releaseRateTx releases the TPM hold of a request whose upstream
// consumption is confirmed zero. The RPM count is NOT touched: an
// admitted request counts once even when the upstream refused it.
func (s *Store) releaseRateTx(ctx context.Context, tx *sqlx.Tx, requestID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_rate_reservations
		 SET state = 'released', reserved_tokens = 0, released_at = now()
		 WHERE request_id = $1 AND state = 'reserved'`,
		requestID); err != nil {
		return rateStoreError("rate: release reservation", err)
	}
	return nil
}

// CorrectRateTokensTx applies a settlement correction to the request's
// rate reservation:
//
//   - the token total is REPLACED by the corrected total — the net delta
//     of THIS request, never an accumulation across usage revisions;
//   - the ORIGINAL settlement instant is kept: a correction never resets
//     the 60-second window attribution, so correcting a request that
//     settled outside the window does not create consumption in the
//     current minute;
//   - idempotent: applying the same correction twice leaves the same
//     total.
//
// This is the rate-side counterpart of the money-side correction (the
// ledger correction itself stays in the reconciliation flow).
func (s *Store) CorrectRateTokensTx(ctx context.Context, w domain.UnitOfWork, requestID string, correctedTokens int64) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	if correctedTokens < 0 {
		return domain.WrapError(domain.CodeInvalidInput,
			"rate: corrected token total must be >= 0", domain.ErrNegativeValue)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_rate_reservations
		 SET settled_tokens = $2
		 WHERE request_id = $1 AND scope = 'account' AND state = 'settled'`,
		requestID, correctedTokens)
	if err != nil {
		return rateStoreError("rate: correct tokens", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NewError(domain.CodeConflict,
			"rate: no settled rate reservation to correct")
	}
	return nil
}

// PurgeRateReservations deletes the terminal rate reservations that are
// older than the retention cutoff. It NEVER deletes an unsettled
// reservation (state='reserved'): an in-flight hold still occupies the
// TPM budget and must not be dropped by a TTL (设计 §7.2: 禁止仅凭 TTL
// 释放全部预占).
func (s *Store) PurgeRateReservations(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM inference_rate_reservations
		 WHERE state <> 'reserved' AND counted_at < $1`, cutoff)
	if err != nil {
		return 0, mapError("rate: purge", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// rateStoreError classifies a rate-store failure: an idempotency/
// uniqueness conflict stays a conflict (the caller's identity problem);
// everything else is a service-side failure (the store is unreachable or
// the query failed) — a clear service error, NEVER a user quota
// exhaustion (设计 §7.2: DB 故障 fail closed with a service error).
func rateStoreError(op string, err error) error {
	if err == nil {
		return nil
	}
	mapped := mapError(op, err)
	switch domain.CodeOf(mapped) {
	case domain.CodeConflict, domain.CodeInvalidInput, domain.CodeNotFound:
		return mapped
	}
	return domain.WrapError(domain.CodeServiceUnavailable,
		"inference: rate limits unavailable", mapped)
}
