-- 2026-07-28: 7-day free trial grant (docs/superpowers/specs/2026-07-28-trial-grant-design.md).
--
-- Registers the 'trial' plan row consumed by AuthService.grantTrialSubscription
-- (internal/service/auth.go) on a user's first-ever login. Properties:
--   is_active=true                    — has_access requires plan.is_active
--                                       (resolvePlanForTokenIssuanceWithPlan)
--   accepting_new_subscriptions=false — CreateOrder rejects it (409); trial
--                                       can only be granted, never bought
--   is_listed=false                   — stays out of the public catalog
--   trial_days=7                      — grant reads this; ops can tune via the
--                                       plan admin API without a deploy
--   interval_days=0                   — smallest interval, so repurchaseAllowed
--                                       lets a trial user buy any paid plan and
--                                       the activation-time downgrade guard
--                                       never fires from a trial row
--   apps={yundian,yundash}            — trial = full functionality, same app
--                                       set as the paid plans
INSERT INTO plans (id, name, price, interval_days, apps, is_active,
                   accepting_new_subscriptions, is_listed, trial_days)
VALUES ('trial', 'Free Trial', 0, 0, '{yundian,yundash}', true,
        false, false, 7)
ON CONFLICT (id) DO NOTHING;
