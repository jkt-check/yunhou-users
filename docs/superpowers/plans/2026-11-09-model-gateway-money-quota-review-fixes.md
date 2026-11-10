# Money And Quota Review Fixes Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans for inline implementation.

**Goal:** Fix independent review findings 1 and 2 without changing findings 3-5.

**Architecture:** Rate retention must respect the settlement-based live TPM window.
Corrections append usage and a durable, uniquely keyed audit receipt even when the
monetary delta is zero; financial adjustments remain strictly positive.

**Tech Stack:** Go, PostgreSQL, existing transaction and test helpers.

**Spec:** The user's November 9, 2026 independent-review handoff in this thread.
This document records its binding requirements for execution.

## Global Constraints

- Use only this worktree; do not modify adjacent repositories.
- Use owned disposable PostgreSQL instances and explicit test DSNs, with `-p 1`.
- Do not touch production databases, secrets, `.env`, `keys/`, or real providers.
- Do not terminate or restart Kaya.
- No push, merge, deployment, or new commit in this task.
- Fix only findings 1 and 2; record findings 3-5 in the verification runbook.
- Run the full race suite and E2E; report coverage and remaining risks.

## Review Focus

- Short retention cutoffs must not remove live RPM or settled TPM usage.
- Zero-priced requests must accept estimated-to-reported corrections.
- Metering-only replay and concurrent mixed corrections must apply once.
- Failed token correction must roll back usage and the correction receipt.
- Equal evidence is a no-op, but source-only or bucket-only evidence changes count.

### Task 1: Retain Live Rate Consumption

**Files:** `internal/inference/postgres/inference_limit_repo.go`,
`internal/inference/postgres/inference_limit_test.go`.

**Interfaces:** Existing `PurgeRateReservations`, admission and settlement helpers.

- [x] Add regression: admit over 24 hours ago, settle 90 tokens now, purge, and
  reject a new 20-token request at TPM 100. Also cover a short retention cutoff.
- [x] Run the regression in a disposable database; expect the old code to fail.
- [x] Protect live settlement timestamps and live admission timestamps during purge.
  Age both timestamps in the existing expired-terminal-row test.
- [x] Re-run focused race tests; expect PASS.

### Task 2: Permit Metering-Only Corrections

**Files:** `internal/inference/postgres/reconciliation_repo.go`,
`internal/inference/postgres/correction_rate_test.go`,
`internal/inference/postgres/metering_correction_test.go`,
`internal/inference/postgres/recovery_test.go`,
`migrations/055_inference_usage_corrections.sql`.

**Interfaces:** Existing `CorrectSettlement`, unchanged command shape. All new
corrections share a unique audit receipt; legacy financial keys remain recognized.

- [x] Add regressions for nonzero and zero equal amounts, changed tokens, source-only
  changes, rollback, replay, and concurrent mixed corrections.
- [x] Run before production changes; expect equal-money regressions to fail.
- [x] Append usage and receipt atomically; create financial entries only for nonzero
  deltas. Retain genuine no-op rejection and the original settlement timestamp.
- [x] Re-run all focused correction/rate race tests; expect PASS.

### Task 3: Verify And Record

**Files:** `docs/runbooks/2026-11-05-model-gateway-verification.md`.

- [x] Obtain one independent diff review and address important findings with TDD.
- [x] Run disposable-database build, vet, full race + coverage, and race E2E.
- [x] Inspect package/test/skip counts and owned cluster shutdown evidence.
- [x] Record findings 3-5 as deferred and append final verification evidence.
- [x] Report changed files, tests, coverage, and limitations without publishing.
