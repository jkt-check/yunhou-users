# Migrations

This directory holds the SQL files that the `cmd/migrate` binary applies
in lexicographic filename order. Each successfully applied file is
recorded in the `_migrations` ledger table inside the same transaction
as its SQL — re-running `make migrate` on a clean DB is a no-op.

## Naming

- Format: `NNN_short_description.sql` where `NNN` is a zero-padded
  sequence number. Example: `001_init.sql`, `009_add_user_locale.sql`.
- Numbers don't need to be contiguous — leave gaps so a hot-fix can
  slip in between planned ones without renumbering.
- Once a migration file has been deployed to **any** environment, **do
  not edit it**. Fix forward with a new file. The ledger keeps the
  original's id recorded, so an in-place edit would diverge from what
  every running database actually has.

## Idempotent DDL — required

The cmd/migrate binary wraps each file in a single transaction, but it
**does not** retry a failed migration. If `001_init.sql` ever runs twice
on the same database the non-idempotent DDL will fail on the second
run. The fix is the same one you'd use for any hand-applied migration:

| Operation | Idempotent form |
|---|---|
| Create table | `CREATE TABLE IF NOT EXISTS ...` |
| Add column | `ALTER TABLE ... ADD COLUMN IF NOT EXISTS ...` |
| Drop constraint | `ALTER TABLE ... DROP CONSTRAINT IF EXISTS ...` |
| Add constraint | wrap in `DO $$ ... EXCEPTION WHEN duplicate_object THEN NULL; END $$;` |
| Create index | `CREATE INDEX IF NOT EXISTS ...` |
| Insert seed | `INSERT ... ON CONFLICT DO NOTHING` |

The integration test `TestApply_RealMigrationsFromRepo` (in
`internal/migrate/migrate_test.go`) loads this directory and runs every
file against a fresh database; a non-idempotent DDL anywhere here will
break that test, which is the safety net the deploy script depends on.

## Transaction control statements

Don't put `BEGIN` / `COMMIT` / `ROLLBACK` at the top level of a
migration file. The cmd/migrate binary already wraps each file in its
own transaction — adding your own breaks the nested-tx model (PG has
no real nested transactions; the inner commit silently ends the outer
tx and the ledger INSERT then fails). The `DO $$ BEGIN ... EXCEPTION
... END $$;` blocks used in 005_paypal_channel.sql are PL/pgSQL block
syntax, **not** transaction control — those are fine.

## Files

| File | Purpose |
|---|---|
| `001_init.sql` | Core users / identities / apps / plans / subscriptions tables |
| `002_simplify_plans.sql` | Plan has apps[]; seeded free/monthly/quarterly/yearly rows (default-plan concept later dropped by 014) |
| `003_payments.sql` | orders / payments / refunds / webhook_events / audit_log |
| `004_ls_channel.sql` | (historical) added lemonsqueezy to channel CHECK — kept for installs that ran it before 008 |
| `005_paypal_channel.sql` | extends channel CHECK to include 'paypal' |
| `006_paypal_sub_mapping.sql` | subscriptions.external_subscription_id for PayPal renewals |
| `007_app_secret.sql` | apps.secret_hash column (backfilled by server startup) |
| `008_drop_lemonsqueezy.sql` | removes lemonsqueezy from channel CHECK (LS code was removed in d8f333d) |
| `009_wechat_pay_intent.sql` | adds orders.provider_intent JSONB for wechat_pay pre-auth metadata |
| `010_provider_intent_nullable.sql` | change provider_intent default from `'{}'` to NULL so omitempty works |
| `011_order_reconcile.sql` | adds orders.last_reconciled_at (defaulted to `now()`, indexed) so GetOrder's wechat_pay QueryOrder reconcile path can throttle re-polls |
| `012_plan_commercial_fields.sql` | commercializes the plans surface: `is_listed`, `accepting_new_subscriptions`, `currency`, `trial_days`, `description`, `display_order`, `updated_at` (trigger-maintained) + new `plan_change_log` audit table |
| `013_plan_change_log_fk_set_null.sql` | relaxes `plan_change_log.plan_id` FK to `ON DELETE SET NULL` so hard-deleted plans keep their audit history; pairs with `PlanService.DeletePlan` reorder to INSERT-then-DELETE |
| `014_remove_default_plan.sql` | retires the default-plan concept: marks `free` inactive, drops `plans_one_default` partial unique index, drops `plans.is_default` column (gated on no active `free` subscriptions) |
| `015_plan_change_log_nullable_snapshots.sql` | plan_change_log.before / .after become nullable so CreatePlan (`before=NULL`) and DeletePlan (`after=NULL`) can write audit rows (spec §6.1) |
| `016_plan_pricing_and_hide.sql` | re-prices `monthly` (¥19.9/mo) and `yearly` (¥199.9/yr) to match the yunhou-website frontend promo; fully retires `quarterly` (`is_listed=false`, `is_active=false`) and hides `free` from the public catalog |
| `017_sub_expiry_does_not_backfill.sql` | no-op marker documenting the decision NOT to backfill `subscriptions.expires_at = NULL` rows from the pre-2026-07-27 WeChat NATIVE v3 bug (see `docs/superpowers/plans/2026-07-27-subscription-expiry-fallback.md` for rationale) |
| `018_trial_plan.sql` | adds the `trial` plan row (active, not purchasable, not listed, trial_days=7) backing `AuthService.grantTrialSubscription` — 7-day free trial granted at first login |