package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// The caller holds account and request locks. Original financial facts stay
// immutable; each changed source gets a paired adjustment in both ledgers.
func correctWalletSettlementTx(ctx context.Context, tx *sqlx.Tx, accountID string, cmd CorrectionCommand, base int64) (int64, error) {
	var holdID, walletID, state, currency, owner string
	var hold, cash, bonus int64
	var settledAt sql.NullTime
	var priceID, requestPrice sql.NullString
	err := tx.QueryRowxContext(ctx, `SELECT h.id, h.wallet_id, h.state, w.currency,
	    w.billing_account_id, h.amount_micros, h.cash_micros, h.bonus_micros,
	    h.settled_at, h.price_version_id, r.price_version_id
	    FROM inference_wallet_holds h
	    JOIN inference_wallets w ON w.id=h.wallet_id
	    JOIN inference_requests r ON r.id=h.request_id
	    WHERE h.request_id=$1 FOR UPDATE OF h`, cmd.RequestID).
		Scan(&holdID, &walletID, &state, &currency, &owner, &hold, &cash, &bonus,
			&settledAt, &priceID, &requestPrice)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, domain.NewError(domain.CodeConflict, "correction: wallet hold missing")
	}
	if err != nil {
		return 0, mapError("correction: wallet hold", err)
	}
	if owner != accountID || state != "settled" || !settledAt.Valid ||
		!priceID.Valid || priceID != requestPrice {
		return 0, domain.NewError(domain.CodeConflict, "correction: wallet hold drift")
	}
	if _, err := lockWalletTx(ctx, tx, walletID); err != nil {
		return 0, err
	}
	var chargeAccount, unit string
	var chargeCurrency, chargePrice sql.NullString
	var originalAmount int64
	if err := tx.QueryRowxContext(ctx, `SELECT billing_account_id, unit,
	    currency, price_version_id, amount_micros FROM inference_ledger_entries
	    WHERE request_id=$1 AND entry_type='charge'`, cmd.RequestID).
		Scan(&chargeAccount, &unit, &chargeCurrency, &chargePrice, &originalAmount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, domain.NewError(domain.CodeConflict, "correction: wallet charge missing")
		}
		return 0, mapError("correction: wallet charge", err)
	}
	if chargeAccount != accountID || unit != "micromoney" ||
		chargeCurrency.String != currency || chargePrice != priceID {
		return 0, domain.NewError(domain.CodeConflict, "correction: wallet charge unit/currency drift")
	}
	// Check all three financial facts, including missing or duplicate pairs.
	// Equal net totals alone would hide offsetting historical corruption.
	var ledgerNet, invalidPairs int64
	if err := tx.QueryRowxContext(ctx, `WITH pairs AS (
	    SELECT count(*) OVER (PARTITION BY a.id) AS pair_count,
	        a.id IS NOT NULL AND e.id IS NOT NULL AND l.id IS NOT NULL
	        AND a.billing_account_id=$2 AND a.request_id=$1
	        AND a.unit='micromoney' AND a.currency=$5
	        AND e.entry_type='adjustment' AND e.request_id=$1
	        AND e.wallet_id=$3 AND e.hold_id=$4 AND e.currency=$5
	        AND e.source=a.source AND e.direction=a.direction
	        AND e.amount_micros=a.amount_micros AND e.reverses_entry_id IS NULL
	        AND l.entry_type='adjustment' AND l.request_id=$1
	        AND l.billing_account_id=$2 AND l.unit='micromoney' AND l.currency=$5
	        AND l.price_version_id=$6 AND l.amount_micros=a.amount_micros
	        AND l.reverses_entry_id IS NULL AS valid
	    FROM inference_adjustments a
	    FULL JOIN inference_wallet_entries e ON e.adjustment_id=a.id
	    FULL JOIN inference_ledger_entries l ON l.adjustment_id=a.id
	    WHERE (a.request_id=$1 OR e.request_id=$1 OR l.request_id=$1)
	        AND (a.id IS NOT NULL OR e.entry_type='adjustment' OR l.entry_type='adjustment')
	)
	SELECT COALESCE(SUM(CASE
	    WHEN l.entry_type='charge' OR (l.entry_type='adjustment' AND a.direction='debit')
	        THEN l.amount_micros
	    WHEN l.entry_type='adjustment' AND a.direction='credit' THEN -l.amount_micros
	    ELSE 0 END),0),
	    count(*) FILTER (WHERE l.billing_account_id<>$2 OR l.unit<>'micromoney'
	        OR l.currency IS DISTINCT FROM $5::text OR l.price_version_id IS DISTINCT FROM $6::uuid
	        OR l.entry_type NOT IN ('charge','adjustment'))
	    + (SELECT count(*) FROM pairs WHERE pair_count<>1 OR valid IS NOT TRUE)
	FROM inference_ledger_entries l
	LEFT JOIN inference_adjustments a ON a.id=l.adjustment_id
	WHERE l.request_id=$1`, cmd.RequestID, accountID, walletID, holdID, currency, priceID.String).
		Scan(&ledgerNet, &invalidPairs); err != nil {
		return 0, mapError("correction: wallet ledger pairs", err)
	}
	if invalidPairs != 0 || ledgerNet != base {
		return 0, domain.NewError(domain.CodeConflict, "correction: wallet ledger pairing/totals drift")
	}
	var netCash, netBonus, originalCash, originalBonus, invalid int64
	if err := tx.QueryRowxContext(ctx, `SELECT
	    COALESCE(SUM(CASE WHEN e.source='cash' THEN
	        CASE WHEN e.direction='debit' THEN e.amount_micros ELSE -e.amount_micros END ELSE 0 END),0),
	    COALESCE(SUM(CASE WHEN e.source='bonus' THEN
	        CASE WHEN e.direction='debit' THEN e.amount_micros ELSE -e.amount_micros END ELSE 0 END),0),
	    COALESCE(SUM(e.amount_micros) FILTER (WHERE e.entry_type='consume' AND e.source='cash'),0),
	    COALESCE(SUM(e.amount_micros) FILTER (WHERE e.entry_type='consume' AND e.source='bonus'),0),
	    count(*) FILTER (WHERE e.wallet_id<>$2 OR e.currency<>$3
	        OR (e.entry_type<>'reversal' AND e.hold_id IS DISTINCT FROM $4::uuid)
	        OR e.entry_type NOT IN ('consume','adjustment','reversal'))
	    FROM inference_wallet_entries e
	    LEFT JOIN inference_wallet_entries o ON o.id=e.reverses_entry_id
	    WHERE e.request_id=$1 OR o.request_id=$1`, cmd.RequestID, walletID, currency, holdID).
		Scan(&netCash, &netBonus, &originalCash, &originalBonus, &invalid); err != nil {
		return 0, mapError("correction: wallet source totals", err)
	}
	oldCash, oldBonus, oldExtra, err := accounting.SplitConsume(cash, bonus, base)
	if err != nil {
		return 0, err
	}
	firstCash, firstBonus, firstExtra, err := accounting.SplitConsume(cash, bonus, originalAmount)
	if err != nil {
		return 0, err
	}
	if invalid != 0 || netCash != oldCash+oldExtra || netBonus != oldBonus ||
		originalCash != firstCash+firstExtra || originalBonus != firstBonus {
		return 0, domain.NewError(domain.CodeConflict, "correction: wallet source totals drift")
	}
	newCash, newBonus, extra, err := accounting.SplitConsume(cash, bonus, int64(cmd.CorrectedMicros))
	if err != nil {
		return 0, err
	}
	for _, source := range []struct {
		name  string
		delta int64
	}{{"bonus", newBonus - netBonus}, {"cash", newCash + extra - netCash}} {
		if source.delta == 0 {
			continue
		}
		amount, direction := source.delta, "debit"
		if amount < 0 {
			amount, direction = -amount, "credit"
		}
		adjID := uuid.NewString()
		internalKey := "correction-source:" + adjID
		if _, err := tx.ExecContext(ctx, `INSERT INTO inference_adjustments
		    (id,billing_account_id,request_id,reason,amount_micros,direction,unit,currency,
		     source,operator_subject,service_subject,idempotency_key)
		    VALUES ($1,$2,$3,$4,$5,$6,'micromoney',$7,$8,$9,'inference-reconciliation',$10)`,
			adjID, accountID, cmd.RequestID, cmd.Reason, amount, direction, currency,
			source.name, cmd.OperatorSubject, internalKey); err != nil {
			return 0, mapError("correction: wallet adjustment", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO inference_wallet_entries
		    (wallet_id,entry_type,direction,source,amount_micros,currency,request_id,
		     hold_id,adjustment_id,business_key,created_by)
		    VALUES ($1,'adjustment',$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			walletID, direction, source.name, amount, currency, cmd.RequestID, holdID,
			adjID, accounting.WalletAdjustmentKey(internalKey), cmd.OperatorSubject); err != nil {
			return 0, mapError("correction: wallet entry", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO inference_ledger_entries
		    (billing_account_id,request_id,entry_type,amount_micros,unit,currency,
		     price_version_id,adjustment_id,created_by)
		    VALUES ($1,$2,'adjustment',$3,'micromoney',$4,$5,$6,$7)`,
			accountID, cmd.RequestID, amount, currency, priceID.String, adjID, cmd.OperatorSubject); err != nil {
			return 0, mapError("correction: wallet ledger", err)
		}
	}
	return hold, nil
}
