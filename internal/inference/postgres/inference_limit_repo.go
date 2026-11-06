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

// rateStateAnomaly is the disposition of a rate reservation whose request
// was PARKED for manual handling because of an accounting anomaly
// (missing_reservation / reservation_drift / insufficient_evidence).
//
// 语义（终审 Finding 1）：
//
//   - 它**不再占账户 TPM 额度**：TPM 的在途求和只算 state='reserved'，一个
//     永远不会被结算的异常预占不得永久吃掉账户的 TPM（否则一笔异常就把
//     账户永久 429 拦住，而且没有任何路径能把它取回来）；
//   - 它**不是** 'released'（那等于宣称"确认零消费"），也**不是**
//     'settled'（那等于宣称"已知实际用量"）：消费仍然未知 —— 未知绝不记成 0。
//     异常证据留在行上（anomaly_reason / anomaly_at）与核对任务里；
//   - **RPM 事实保留**：行还在、counted_at 不变，一次逻辑请求仍然计 1 次
//     （RPM 计数不看 state）；
//   - 它是**终态**：保留期清理像删除其它终态行一样删除它（滞留行不再无限
//     增长）。
const rateStateAnomaly = "anomaly"

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
//
// The TPM aggregates have two halves with DIFFERENT lifetimes:
//
//   - the in-flight sum counts only state='reserved' rows (an in-flight
//     hold occupies the budget regardless of its age — a long stream still
//     holds it); a row disposed to 'anomaly' (a request parked for manual
//     handling) is NOT in flight any more: it no longer occupies the
//     budget, so one anomaly can never permanently block the account;
//   - the settled sum counts the tokens actually consumed inside the
//     rolling window.
//
// The RPM counts look at the rows' age only (counted_at), NOT at their
// state: a parked anomaly still counts once — one logical request, one
// count.
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
//
// A row previously disposed to 'anomaly' (the request was parked for
// manual handling and the hold was taken out of the live TPM budget) is
// promoted to 'settled' here: the request really settled, so its real
// consumption must enter the TPM budget — at the settlement instant. The
// anomaly trace (anomaly_reason / anomaly_at) is kept, never erased.
func (s *Store) settleRateTx(ctx context.Context, tx *sqlx.Tx, requestID string, tokens int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_rate_reservations
		 SET state = 'settled', settled_tokens = $2, settled_at = now()
		 WHERE request_id = $1 AND scope = 'account' AND state IN ('reserved','anomaly')`,
		requestID, tokens); err != nil {
		return rateStoreError("rate: settle account reservation", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_rate_reservations
		 SET state = 'settled', settled_at = now()
		 WHERE request_id = $1 AND scope = 'key' AND state IN ('reserved','anomaly')`,
		requestID); err != nil {
		return rateStoreError("rate: settle key reservation", err)
	}
	return nil
}

// releaseRateTx releases the TPM hold of a request whose upstream
// consumption is confirmed zero. The RPM count is NOT touched: an
// admitted request counts once even when the upstream refused it.
//
// A row previously disposed to 'anomaly' is promoted to 'released' here:
// the hold was already out of the live TPM budget, and the release path
// is exactly the "confirmed zero consumption" fact this state means. The
// anomaly trace is kept.
func (s *Store) releaseRateTx(ctx context.Context, tx *sqlx.Tx, requestID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_rate_reservations
		 SET state = 'released', reserved_tokens = 0, released_at = now()
		 WHERE request_id = $1 AND state IN ('reserved','anomaly')`,
		requestID); err != nil {
		return rateStoreError("rate: release reservation", err)
	}
	return nil
}

// parkRateReservationTx disposes of the request's rate reservation when
// the request is PARKED for manual handling because of an accounting
// anomaly (终审 Finding 1).
//
// The disposition moves the row out of the live TPM budget (state='anomaly')
// WITHOUT claiming anything about the consumption: it is neither
// 'released' (confirmed zero) nor 'settled' (known actual usage). The RPM
// count is untouched (the request was admitted), the monetary holds are
// untouched (no money is settled, no hold is released), and the anomaly
// evidence stays on the row (anomaly_reason / anomaly_at) and in the
// reconciliation job.
//
// It is idempotent: only a row that is still in flight is disposed, so a
// repeated park neither re-disposes nor re-drives anything.
func (s *Store) parkRateReservationTx(ctx context.Context, tx *sqlx.Tx, requestID, reason string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_rate_reservations
		 SET state = $3, anomaly_at = now(), anomaly_reason = $2
		 WHERE request_id = $1 AND state = 'reserved'`,
		requestID, reason, rateStateAnomaly); err != nil {
		return rateStoreError("rate: park anomaly reservation", err)
	}
	return nil
}

// CorrectRateTokensTx corrects ONLY the token budget of an already settled
// request (the rate-side half of a settlement correction). It takes the
// ABSOLUTE corrected token total; the storage effect is a NET DELTA — the
// row keeps ONE authoritative total (never an accumulation across usage
// revisions) and the ORIGINAL settlement instant is kept, so a correction
// never resets the 60-second window attribution.
//
// A MISSING rate row is a BENIGN absence, not an anomaly: the request may
// predate migration 052, or its terminal row may have been purged by the
// retention sweep. The money correction still applies.
//
// The money correction and this one belong in the SAME transaction — see
// Store.CorrectSettlement, which calls the shared core below.
func (s *Store) CorrectRateTokensTx(ctx context.Context, w domain.UnitOfWork, requestID string, correctedTokens int64) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	return s.correctRateTokensTx(ctx, tx, requestID, correctedTokens)
}

// correctRateTokensTx is the shared core of the token-budget correction
// (the exported method and the money correction use the same transaction).
//
//   - the token total is REPLACED by the corrected absolute total — the net
//     delta of THIS request, never an accumulation across usage revisions;
//   - the ORIGINAL settlement instant is kept: a correction never resets the
//     60-second window attribution, so correcting a request that settled
//     outside the window does not create consumption in the current minute;
//   - idempotent: applying the same correction twice leaves the same total;
//   - a MISSING rate row is a benign absence (pre-052 request / retention
//     purge) and leaves the money correction untouched;
//   - a row that IS present but malformed (not settled, or a key row
//     without its account row) is an anomaly and fails loudly — it must
//     never be mistaken for a benign absence;
//   - a row disposed to 'anomaly' (the request was parked for manual
//     handling, so its hold left the live TPM budget) has NO settled token
//     total to correct: the correction fails LOUDLY with an explicit
//     message instead of silently keeping a total that was never booked.
//     The row becomes correctable the moment the request really settles —
//     settleRateTx then promotes it to 'settled' with the actual tokens.
func (s *Store) correctRateTokensTx(ctx context.Context, tx *sqlx.Tx, requestID string, correctedTokens int64) error {
	if correctedTokens < 0 {
		return domain.WrapError(domain.CodeInvalidInput,
			"correction: the corrected token total must be >= 0", domain.ErrNegativeValue)
	}
	var state string
	var anomalyReason sql.NullString
	err := tx.QueryRowxContext(ctx,
		`SELECT state, anomaly_reason FROM inference_rate_reservations
		 WHERE request_id = $1 AND scope = 'account' FOR UPDATE`, requestID).Scan(&state, &anomalyReason)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		// Benign absence — but a key row without its account row is drift
		// (the two rows are written together): that must NOT be mistaken
		// for a benign absence.
		var keyRows int
		if qerr := tx.QueryRowxContext(ctx,
			`SELECT COUNT(*) FROM inference_rate_reservations
			 WHERE request_id = $1 AND scope = 'key'`, requestID).Scan(&keyRows); qerr != nil {
			return mapError("correction: rate key rows", qerr)
		}
		if keyRows > 0 {
			return domain.NewError(domain.CodeConflict,
				"correction: the request has a key rate reservation but no account one (rate row drift)")
		}
		return nil
	default:
		return mapError("correction: rate row", err)
	}
	if state == rateStateAnomaly {
		reason := "unknown"
		if anomalyReason.Valid {
			reason = anomalyReason.String
		}
		return domain.NewError(domain.CodeConflict,
			"correction: the rate reservation was disposed to the anomaly bucket (reason="+reason+
				") — it holds no settled token total; settle the request first (未知用量不得记成 0)")
	}
	if state != "settled" {
		return domain.NewError(domain.CodeConflict,
			"correction: the rate reservation is not settled ("+state+
				") — the token total cannot be corrected")
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_rate_reservations
		 SET settled_tokens = $2
		 WHERE request_id = $1 AND scope = 'account' AND state = 'settled'`,
		requestID, correctedTokens); err != nil {
		return mapError("correction: rate tokens", err)
	}
	return nil
}

// PurgeRateReservations deletes the terminal rate reservations that are
// older than the retention cutoff. It NEVER deletes an unsettled
// reservation (state='reserved'): an in-flight hold still occupies the
// TPM budget and must not be dropped by a TTL (设计 §7.2: 禁止仅凭 TTL
// 释放全部预占).
//
// A row disposed to 'anomaly' IS terminal: its hold left the live TPM
// budget when the request was parked for manual handling, so deleting it
// frees nothing that is still in flight — it only reclaims table volume.
// The anomaly evidence stays in the reconciliation job and in the
// request's last_error.
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
