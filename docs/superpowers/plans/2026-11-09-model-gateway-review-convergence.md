# Model Gateway Review Convergence

> **For agentic workers:** Use superpowers:executing-plans for inline execution.

**Goal:** Continue the requested review until a complete scoped pass has no
confirmed unresolved defects.

**Architecture:** Keep the internal Chat representation and reject history
it cannot represent faithfully. Bound SSE lines at 1 MiB and fail explicitly
above that limit instead of discarding content or usage. Assemble Responses
output by the index assigned when each item opened, not its closing order.

**Tech Stack:** Go, existing SSE adapters and race tests, disposable PostgreSQL.

**Spec:** `2026-11-05-users-model-gateway-completion.md`, user review goal,
and the November 9 no-push/no-merge/no-deploy constraints.

## Global Constraints

- Current linked worktree only; no neighboring repository or kaya changes.
- No production configuration, credentials, providers or database access.
- Database tests use owned disposable databases and explicit DSNs, `-p 1`.
- Reproduce each confirmed defect before production edits.

## Review Focus

- Assistant text after a tool call must not move before that call.
- Large text/tool deltas may carry usage in the same SSE line.
- Oversized lines split across reads must not resume from a suffix or DONE.
- Terminal Responses output must agree with emitted output indexes.
- Reasoning belongs in response output, but not in the replay transcript.

## Task 1: Incoming Messages Order

**Files:** `messages_client.go`, `messages_client_order_test.go` in
`internal/inference/providers`.

- [x] Reproduce before/call/after and call/text/call with invalid-input tests.
- [x] Reject nonempty assistant text after tool_use; preserve supported cases.
- [x] Run focused race tests, then the providers suite.

## Task 2: Responses Output Order

**Files:** `responses_client.go`, `responses_stream_order_test.go` in
`internal/inference/providers`.

- [x] Reproduce tool/text/reasoning/tool closing out of opening order.
- [x] Store final items by output index; filter reasoning only for replay.
- [x] Verify completed/incomplete terminal output and transcript order.

## Task 3: SSE Line Limits

**Files:** `stream_usage.go`, `client_surface.go`,
`openai_responses_stream.go`, `stream_usage_test.go`, `sse_limits_test.go`
in `internal/inference/providers`.

**Interfaces:** `UsageTap.Feed([]byte) error`; no-op native taps return nil.
`chunkPump.Feed([]byte) ([]pumpEvent, error)` reports framing overflow.

- [x] Reproduce 70 KiB text/tool deltas with inline usage losing data.
- [x] Reproduce over-limit lines falsely reaching a successful terminal.
- [x] Support bounded large lines and propagate sticky errors through the tee.
- [x] Emit native errors on translation overflow, never normal completion.
- [x] Verify whole-read and fragmented input, then run providers race tests.

## Task 4: Independent Review And Delivery

- [x] Evaluate read-only accounting and gateway reviewer findings.
- [x] Fix confirmed important defects with RED/GREEN tests.
- [x] Review the resulting branch independently, including the focus list.
- [x] Run build, vet, full race coverage and E2E with disposable databases.
- [x] Record evidence, file manifest, compatibility costs and remaining risks.
- [ ] Commit the final converged review locally, without push, merge or deployment.

## Task 5: Refund Versus Stale Entitlement Sync

**Files:** `workers/entitlement_sync.go`, its regression tests, and the
PostgreSQL outbox, billing-account and entitlement repositories.

- [x] Reproduce a paused paid-message/direct sync that resumes after refund
  and after another worker has noop-delivered both pending messages.
- [x] Read the subscription, paid order and existing entitlement on the
  applying transaction; hold shared locks on the source rows until commit.
- [x] Include the empty-plan delivery path and account creation in the
  transaction; retain revision/unique race handling.
- [x] Run all worker and PostgreSQL race tests with disposable databases.

## Task 6: Model Capability Error Classification

**Files:** `gateway/service.go`, gateway/facade/HTTP regression tests.

- [x] Reproduce unsupported model tools/reasoning returning `/chat` 502.
- [x] Return the existing typed capability error before admission.
- [x] Verify capability 400 with no dispatch/reservation, while actual
  upstream invalid-input errors retain their structured 502 mapping.

## Task 7: Sticky Failed Streams

**Files:** Chat usage tracker, Messages/Responses renderers, native stream
adapters and their tests in `internal/inference/providers`, plus the
database-backed regression in `internal/inference/httpapi`.

- [x] Reproduce error followed by DONE/completion becoming success.
- [x] Latch failures without losing known content or usage; never synthesize
  a successful terminal or persist a failed Responses chain.
- [x] Cover same-read, split-read and EOF across all supported protocols.

Raw OpenAI Chat remains byte passthrough, including an upstream error
followed by DONE. Its authoritative metering/settlement remains failed;
the native adapters suppress subsequent content and synthetic success.
Filtering the raw Chat wire would change its existing passthrough contract
and is not part of this checkpoint.

## Task 8: Native Nonstream Output Order

**Files:** native provider decoders/dispatch, gateway nonstream finalizer,
domain error mapping, HTTP handlers, regression tests and API documentation.

- [x] Reproduce text/tool/text and tool/text/tool folding into reordered
  Chat output.
- [x] Reject unrepresentable nonstream output explicitly while preserving
  supported arrangements; never persist reordered replay history.

## Task 9: Interrupted Anthropic Start Usage

**Files:** `anthropic_stream.go` and provider/gateway stream tests.

- [x] Reproduce message_start input/output usage followed by error or EOF
  losing known output usage.
- [x] Preserve the initial counters; later cumulative message_delta
  counters replace, rather than add to, the initial values.

## Task 10: Nonstream Finalization Lease Order

**Files:** `gateway/service.go`, `gateway/nonstream_finish_order_test.go`.

- [x] Reproduce upstream lease release before nonstream finalization, including
  recovery near a legal deployment timeout just below the grace boundary.
- [x] Keep every lease renewed until finalization completes; release with a
  fresh cleanup budget on success, timeout and panic.
- [x] Verify reported settlement wins once, preserves TPM, and leaves no leases.

RED checkpoint (2026-11-09): focused race run
`go test -race -count=1 -p 1 ./internal/inference/gateway -run '^TestGateway_NonstreamFinal' -timeout=3m`
against an owned disposable PostgreSQL failed as expected: finalization held 0/1
or 2/3 live leases, and recovery won the upstream-only case with a conservative
charge of 16413 instead of reported usage's 20. The panic case also observed
only 2/3 leases before cleanup. This checkpoint contains tests, not the fix;
Task 10 remains open. The temporary database was stopped after verification.

GREEN checkpoint: the same keeper now renews all leases through nonstream
finalization, then stops with a fresh cleanup deadline. Focused race run
`run.NYjMkE` passed 20 tests/subtests, including timeout and panic cleanup.
Full gateway race `run.2Pn4qs` passed 142 tests/subtests (42.169s), no
failures or skips; reported charge 20, settled TPM 15, one charge, no
reconciliation jobs or held leases. Both owned clusters stopped normally.
The obsolete early-release lease-ID split was removed. The translation
rejection regression now checks the actual `held` lease state.

## Task 11: Cross-Process Rate And Concurrency Acceptance

**Files:** `postgres/quota_concurrency_test.go`,
`postgres/rate_process_test.go`, original plan and verification runbook.

- [x] Reuse the quota worker with two independent OS processes and pools,
  synchronized before admission on the same disposable PostgreSQL database.
- [x] Exercise global account RPM, policy RPM, Key RPM, account TPM, account
  concurrency and Key concurrency as independent bottlenecks.
- [x] Assert exactly three admissions across both processes, typed
  rejections, and no partial request, monetary, rate or lease effects.
- [x] Run PostgreSQL race tests.
- [x] Include the cases in a fresh whole-branch race/coverage and E2E gate.

Commit checkpoint: focused race `run.MhRoxm` passed 8 tests/subtests;
full PostgreSQL race `run.16SlWX` passed 213 (24.805s), with package
statement coverage 77.5%. Both skip only the parent invocation of the
child-only `TestQuotaWorkerProcess`; the re-executed children ran and
reported distinct PIDs. Each of the six rate/concurrency modes admitted
3 of 16 requests and rejected 13 with the expected code. Monetary
reservations include all four targets (three windows plus Key budget).
Both owned disposable PostgreSQL clusters stopped normally. This closes
the cross-process evidence gap, not cross-host or whole-branch acceptance.

## Task 12: Correction Evidence Integrity

**Files:** `postgres/reconciliation_repo.go`, correction identity/evidence
regressions, correction-rate/recovery fixtures and verification runbook.

- [x] Reproduce a correction using another account's request attempt.
- [x] Reject a foreign attempt before writing usage, money, TPM or receipts.
- [x] Reproduce normalized usage of 90 tokens correcting TPM to zero.
- [x] Reproduce int64-overflow evidence passing with a wrapped total of 1.
- [x] Require the declared total to match normalized buckets without
  unchecked addition; reasoning remains included in output exactly once.
- [x] Verify rejection has no side effects and the corrected command can
  reuse the same idempotency key; preserve unknown-total behavior.
- [x] Run all settlement-correction regressions with race detection.
- [x] Finish a fresh whole-branch race/coverage and E2E gate.
- [x] Record the gate and narrow independent review for the local checkpoint.

RED evidence is retained in `run.rAVmUu` (foreign attempt) and
`run.BMonGO` (inconsistent and overflowing totals). Focused GREEN
`run.kBbcB3` passed 35 tests/subtests without skips. All runs use the
owned disposable PostgreSQL runner under
`/private/tmp/yunhou-review-20261109.3NuMWy`; cleanup stopped each
completed run's owned server.

Full `run.ZWo4XY` exited 0: build/vet passed; full race passed 4,449
tests/subtests across 34 packages, with 80.6% total statement coverage;
E2E race passed 186 tests/subtests (237.008s). The three full-race skips
and one E2E skip are the same explicit child-entry/opt-in scenarios as
the previous checkpoint, not unavailable databases. Both databases
applied all 55 migrations and skipped all 55 on rerun; cleanup confirmed
the owned PostgreSQL server stopped.

McClintock's bounded read-only review found no confirmed Critical or
Important defects in this five-Go-file delta. Minor test gaps and
unchanged wallet/production-trigger limitations remain documented.
See runbook 4.72 for the manifest, exact evidence and scope boundaries.
This is a local commit checkpoint, not whole-repository convergence.

## Task 13: Initial Settlement Identity

**Files:** `postgres/ledger_repo.go`, settlement identity regressions and
verification runbook.

- [x] Reproduce usage booked against another request, a foreign usage
  attempt, and a foreign cost attempt on both plan and wallet settlement.
- [x] Reject mismatched or missing attempt identities before any write,
  including when classifying a duplicate delivery.
- [x] Inspect the still-open transaction for unchanged usage, ledger,
  monetary holds, rate reservations, window/Key aggregates and attempt cost.
- [x] Retry the valid command on the same transaction and verify a single
  settlement; preserve valid duplicate classification.
- [x] Run focused regressions, whole-branch race/coverage and E2E on owned
  disposable PostgreSQL; obtain an independent read-only review.

Ruling: this enforces the existing request/attempt accounting boundary;
it adds no correction trigger or wallet product policy. Standalone usage
insertion is not itself a settlement and remains a separate review surface.

RED checkpoint (2026-11-09): the focused race run of
`TestSettle_RejectsForeignEvidenceBeforeWriting` failed all 10 leaf cases
(plan/wallet, five identity mismatches each). Foreign evidence was accepted
and changed accounting state; a missing usage attempt aborted the transaction
instead of leaving it usable for a valid retry. Evidence:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.RYh2s8/focused.jsonl`.
The runner used its own disposable PostgreSQL cluster and explicit test DSNs.
That RED checkpoint contains regression tests and this plan only, not a
production fix.

GREEN checkpoint: `Settle` now checks the usage request and both attempt
identities before any write, wallet dispatch or duplicate classification.
Empty/malformed attempt UUIDs are rejected before SQL, keeping the transaction
usable for a valid retry. Distinct usage/cost attempts belonging to the same
request remain supported.

Fresh focused race `run.jvq0Tg` passed 21 tests/subtests, with no failures or
skips (4.890s). It covers nine invalid-identity cases on both plan and wallet,
inspects the open transaction before retry, verifies one valid settlement and
retains typed duplicate classification.

The first whole-branch attempt, `run.6Txzg7`, passed build/vet but failed
the TPM and zero-charge duplicate tests: both fixtures used random,
nonexistent attempts and now correctly received `invalid_input`. They
now replay the persisted original usage without weakening their duplicate,
single-charge or unchanged-TPM assertions. Focused `run.NfcvZF` passed all
23 tests/subtests (7.258s), with no failures or skips. Completed runs' owned
PostgreSQL servers stopped normally.

Fresh `run.N9MTPk` exited 0 after all Go edits: build/vet passed; full
race passed 4,470 tests/subtests across 34 packages, zero failures, with
80.6% total statement coverage. E2E race passed 186 tests/subtests
(236.625s), zero failures. The three full-race skips and one E2E skip
were the explicit child-entry/opt-in scenarios; no database acceptance
was skipped. Both owned databases applied 55 migrations and skipped all
55 on rerun; cleanup confirmed the owned PostgreSQL server stopped.

Euler's bounded read-only review and its follow-up on both duplicate
fixtures found no confirmed Critical or Important defects. Nonblocking
test gaps remain recorded in runbook 4.73. This is a local Task 13 commit
checkpoint, not whole-repository convergence or delivery of the still-open
wallet-correction/production-trigger requirements.

## Task 14: Token Arithmetic Boundaries

**Files:** `access/inference_limits.go`, its rule tests,
`postgres/ledger_repo.go`, new settlement/rate overflow regressions,
original interface inventory and verification runbook.

**Interfaces:** `NormalizedTokens(domain.UsageBuckets) (int64, error)`;
invalid or overflowing evidence returns `invalid_input` before writes.
`EvaluateRateLimits` keeps its typed rate rejection; an unrepresentable
diagnostic total saturates at MaxInt64 without changing the decision.

- [x] Reproduce positive wrapped settlement totals on plan and wallet,
  and inspect the still-open transaction.
- [x] Reproduce combined TPM overflow in the pure rule and PostgreSQL
  admission, including a MaxInt64 policy and a wrapped-positive sum.
- [x] Validate token sums before writes or terminal classification and
  reject over-limit admission without unchecked arithmetic.
- [x] Verify a valid settlement retry on the same transaction after
  rejecting invalid token evidence.
- [x] Verify exact-Max totals, unknown buckets and reasoning counted once;
  preserve disabled limits and determinable-only Retry-After.
- [x] Run focused race, full race/coverage and E2E in owned disposable
  databases, record evidence and review the delta before local commit.

Ruling: use the existing nonnegative int64 accounting contract rather than
clamping persisted evidence. Only the customer-facing diagnostic aggregate
may saturate; a true sum above MaxInt64 must still reject at limit MaxInt64.
The original wallet-correction/production-trigger gap remains open.

RED commit checkpoint: the user's latest `commit` request checkpoints only
the three new regression files and this plan, without a production fix.
Fresh focused race run `run.5pBbzf` exited 1: 12 leaf cases failed, two
boundary controls passed, and no tests skipped. Including parent tests,
the JSON log records 17 failures. TPM overflow admitted all three blocked
cases in both the pure rule and PostgreSQL, changing admission state.
Wrapped-positive settlement totals were accepted on both plan and wallet,
with usage, ledger, rate and monetary state changed inside the transaction.
Negative buckets instead aborted the transaction before a valid retry.
The retry assertions remain unverified until the missing validation is fixed.

Evidence:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.5pBbzf/focused.jsonl`.
Command: `go test -race -count=1 -p 1 -json ./internal/inference/access ./internal/inference/postgres -run '^(TestEvaluateRateLimits_TPMCannotWrapIntoAdmission|TestReserveRateTx_TPMOverflowCannotBypassLimit|TestSettle_RejectsInvalidTokenTotalsBeforeWriting)$' -timeout=5m`.
Both owned disposable databases applied all 55 migrations and skipped all
55 on rerun; `cleanup.log` confirms the owned PostgreSQL server stopped.
No production code changed. Full race, E2E and coverage were not rerun for
this intentionally non-green checkpoint; the previous 80.6% coverage and
passing gate below describe the earlier commit, not this tree. Task 14
remains open. No push, merge, deployment or kaya process operation.

GREEN implementation checkpoint: `NormalizedTokens` now rejects negative
bucket/detail values and unrepresentable totals before settlement writes,
wallet dispatch or terminal classification. Both settlement paths share
the validated total. TPM admission uses checked addition; an overflow
blocks even a MaxInt64 policy while saturating only the diagnostic total.
The original T4 interface inventory now matches the two-result helper.

Fresh focused race `run.1fvhmM` exited 0 with 19 test/subtest passes across
access and PostgreSQL, no skips or failures. This covers exact-Max and
disabled-policy controls, three overflow admission cases, and plan/wallet
invalid evidence followed by a valid retry on the same transaction.
Both owned disposable databases applied all 55 migrations and skipped
all 55 on rerun; cleanup confirms the owned PostgreSQL server stopped.
Fresh full gate `run.hmsZjn` exited 0 on 2026-11-10 (Asia/Shanghai):
build/vet passed, full race passed 4,489 tests/subtests across 34 packages,
total statement coverage was 80.6%, and E2E passed 186 tests/subtests
(241.174s). The three full-race skips and one E2E skip are the expected
child-entry and opt-in cases; no database acceptance was skipped.
Both independent disposable databases applied/reran all 55 migrations;
cleanup confirms the owned PostgreSQL server stopped.

Euler's bounded read-only review found no confirmed Critical or Important
defects. The minor negative-bucket coverage gap and unchanged review
surfaces are recorded in runbook 4.74. Task 14 is verified for this local
commit checkpoint, not a claim that the wider review or the original
wallet-correction/production-trigger requirement is complete. No push,
merge, deployment, adjacent repository edit or kaya process operation.

## Task 15: Correction Validation Before SQL Effects

**Files:** `postgres/reconciliation_repo.go`, correction validation regressions
and verification runbook.

- [x] Reproduce empty/malformed attempt identities and negative reasoning
  evidence reaching SQL constraints instead of safe input rejection.
- [x] Reproduce a negative declared total with unknown normalized buckets.
- [x] Reject invalid correction evidence before usage, receipt or financial
  writes; reuse the shared normalized-token validator.
- [x] Inspect the open transaction after rejection, then apply a valid
  estimated-to-reported correction with the same transaction and key.
- [x] Preserve reasoning-only unknown totals and exact-Max valid totals.
- [x] Run focused and full race/coverage/E2E on owned disposable databases.

Ruling: extend the existing correction evidence boundary, not the wallet
product contract. Validation must not defer to a database error that aborts
the caller's transaction. Invalid evidence remains `invalid_input`; unknown
totals retain their previous TPM value. Wallet correction and its production
trigger remain genuine unfinished original requirements.

RED commit checkpoint: the latest user `commit` request checkpoints the new
regression and this plan only; no production fix is included. Fresh focused
race run `run.s5nzpC` exited 1 on 2026-11-10 (Asia/Shanghai). All five leaf
cases failed with PostgreSQL `25P02` when inspecting the still-open
transaction after rejection; no tests skipped. The valid same-transaction
retry remains unverified because the transaction has already been aborted.
Task 15 remains open.

Evidence:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.s5nzpC/focused.jsonl`.
Command: `go test -race -count=1 -p 1 -json ./internal/inference/postgres -run '^TestCorrectSettlement_RejectsInvalidEvidenceBeforeWriting$' -timeout=5m`.
Both owned disposable databases applied all 55 migrations and skipped them
on rerun; `cleanup.log` confirms the owned PostgreSQL server stopped.
Formatting and diff whitespace checks passed. Full race, E2E and coverage
were not rerun for this intentionally failing reproduction checkpoint;
the previous 80.6% coverage does not describe the current tree.
No push, merge, deployment, adjacent repository edit or kaya operation.

GREEN implementation checkpoint: parse and canonicalize the corrected
attempt UUID before querying it, validate all buckets with the shared
normalized-token helper, and reject a negative declared total even when
the additive buckets are unknown. Existing idempotency, wallet and status
rejection precedence is unchanged.

The regressions are committed in `ef84a93` and `639bb22`. Fresh pre-fix
`run.8HU0A5` reproduced the five transaction-aborting leaf cases; overflow
rejection and both valid boundary controls already passed. Post-fix
`run.bGAnnh` passed all 45 correction tests/subtests with race detection,
no failures or skips. The same transaction and idempotency key accept a
valid reported correction after rejection, with unchanged settlement time,
TPM 90, and the expected single monetary adjustment. Exact-Max, canonical
uppercase UUID input and reasoning-only unknown totals also pass.
Both completed runs used owned disposable databases and stopped their
owned PostgreSQL servers.

Fresh whole-branch `run.qkqGUu` exited 0 on 2026-11-10 (Asia/Shanghai):
build/vet passed; full race passed 4,499 tests/subtests across 34 packages,
zero failures, with total statement coverage 80.7%; E2E race passed 186
tests/subtests (215.356s), zero failures. The three full-race skips and
one E2E skip were the expected child-entry/opt-in scenarios, not missing
databases. Both independent disposable databases applied 55 migrations
and skipped all 55 on rerun; cleanup confirmed the owned server stopped.

Euler's bounded read-only review found no new Critical or Important
Task 15 defect; it did not independently execute tests. It confirmed
the original wallet-correction and production-trigger requirements remain
unfulfilled. Runbook 4.75 records the manifest, scope and evidence.
Task 15 is verified for this local commit checkpoint, not a declaration
that the original plan or wider until-clean review is complete.

## Task 16: Missing Usage Is Not An Idempotent Replay

**Files:** `postgres/ledger_repo.go`, new PostgreSQL and gateway missing-usage
regressions, and the verification runbook.

**Interfaces:** keep `usageFactConsistent(ctx, tx, usage) (bool, error)` and
the existing `SettleOutcomeChargeMismatch` / `settlement_mismatch` reaction.
Only an existing, matching attempt revision proves metering consistency.

- [x] Reproduce missing usage, absent revision and a different valid attempt
  of the same request being silently classified as duplicate.
- [x] Cover plan/wallet and zero/positive amounts, with both identical and
  changed token evidence; inspect the still-open transaction for no writes.
- [x] Preserve exact original replays with default revision 0 and explicit 1,
  and keep real usage-query errors distinguishable from a mismatch.
- [x] Reproduce gateway replay after deleting usage: one alarm and a pending
  settlement_mismatch job, no second charge or rewritten terminal/TPM.
- [x] Reject absent comparable usage without adding evidence, financial
  entries, rate changes or a new public outcome kind.
- [x] Run focused race, full race/coverage and E2E on owned disposable
  databases; record evidence and checkpoint locally.

Ruling: the original T2 and domain contract require consistent charge AND
usage. Retaining an amount-only compatibility fallback contradicts that
contract, especially for zero-priced models. Use the existing mismatch
classification instead of silently repairing missing evidence or inventing
a new financial policy. Missing evidence now requires operator attention;
ordinary exact replays remain silent. Wallet correction and its production
trigger are still genuine unfinished original requirements.

RED commit checkpoint (2026-11-10): the latest user request checkpoints the
two new regression files and this plan only, without a production fix.
The focused race run
`/private/tmp/yunhou-review-20261109.3NuMWy/run.1QzRSV/focused.jsonl`
exited 1, with 22 failed test/subtest events and no skips. All 12
plan/wallet, zero/positive, missing-usage/revision/attempt combinations
returned duplicate instead of charge_mismatch for both unchanged and
changed input tokens. The original revision 0/1 replay controls and
open-transaction no-write assertions did not fail. Both gateway cases
reproduced zero alarms and no reconciliation job after usage deletion,
while their single charge and terminal/TPM invariants remained unchanged.

The separate focused race control run
`/private/tmp/yunhou-review-20261109.3NuMWy/run.lURFCX/focused.jsonl`
exited 0 with two passes and no failures/skips:
`TestSettle_DuplicateDelivery_UsageLookupFailure_Propagates` and
`TestGateway_ConsistentDuplicateSettleIsIdempotent`. It used a fresh owned
cluster, also stopped on cleanup.

The runner created its own PostgreSQL cluster on loopback port 55491,
using explicit disposable review_app/review_e2e DSNs. Both databases
applied all 55 migrations and skipped all 55 on rerun; cleanup.log
confirms the owned server stopped. No kaya process was operated on.
Full race/coverage and E2E were not rerun for this RED-only checkpoint;
earlier green coverage does not describe this tree. Task 16 remains open.

GREEN implementation checkpoint: `usageFactConsistent` now returns false
when the matching attempt revision is absent, and no longer has an
empty-attempt compatibility branch (public settlement validates UUIDs
before classification). The existing charge-mismatch outcome triggers
the gateway's alarm and reconciliation path. It does not create usage,
append money entries or change the terminal request or rate reservation.
The regression tests are already committed in `60cdc45`.

Fresh pre-fix `run.7sjNlN/focused.jsonl` reproduced all 12 repository
leaves and both gateway cases, with 22 failed test/subtest events.
The post-fix `run.bpWMlS/focused.jsonl` passed all 24 tests/subtests,
zero failures or skips, including exact-replay and storage-failure
controls. Both runs used owned disposable databases, applied all 55
migrations to each and skipped them on rerun; their cleanup logs confirm
the owned PostgreSQL servers stopped.

Fresh whole-branch `run.eDLUYE` exited 0 on 2026-11-10 (Asia/Shanghai):
build/vet passed; full race passed 4,521 tests/subtests across 34 packages,
zero failures, with total statement coverage 80.7%; E2E race passed 186
tests/subtests (177.130s), zero failures. The three full-race skips and
one E2E skip were the expected child-entry/opt-in scenarios, not missing
databases. Both independent disposable databases applied 55 migrations
and skipped all 55 on rerun; cleanup confirmed the owned server stopped.
Formatting and diff checks passed. Runbook 4.76 records the manifest,
commands and remaining scope. Task 16 is verified for this local commit
checkpoint; the broader review and original wallet-correction/production
trigger requirements remain unfinished.

## Task 17: Settlement Replay Evidence Equality

**Files:** `postgres/ledger_repo.go`, new
`postgres/settlement_replay_evidence_test.go`, gateway replay regressions,
and the verification runbook.

**Interfaces:** preserve `usageFactConsistent(ctx, tx, usage) (bool, error)`.
An exact replay must match source and semantic JSON as well as all nullable
token buckets and the attempt revision. Empty raw documents canonicalize to
`{"schema_version":1}`, matching insertion; revision 0 remains revision 1.

- [x] Reproduce source-only and raw-only changes being absorbed as duplicates,
  on plan/wallet and zero/positive amounts.
- [x] Preserve exact replays, JSON key-order/whitespace/numeric equivalence,
  and implicit/explicit default raw documents; inspect unchanged accounting
  inside the open replay transaction.
- [x] Compare stored source and JSONB equality without changing financial,
  rate, request or usage state; keep storage failures loud.
- [x] Verify the gateway alarms and queues mismatching evidence, but absorbs
  actual exact replays (including their original raw evidence).
- [x] Run focused race and the full disposable-database verification gate.

Ruling: metering source and provider evidence are part of the persisted
usage fact, not just the token total. A late reported delivery that agrees
with an estimate's amount and buckets is still different evidence and must
reach the existing reconciliation path, not be silently discarded. This
does not implement the still-missing production correction channel.

RED commit checkpoint (2026-11-10): the latest user request checkpoints the
new repository regression and this plan without a production fix. Fresh
focused race run
`/private/tmp/yunhou-review-20261109.3NuMWy/run.3toLXZ/focused.jsonl`
exited 1: 20 of 36 leaf cases reproduced source/raw changes being returned
as duplicate instead of charge_mismatch, for both revisions 0 and 1.
The 16 exact/semantic/default replay controls passed; no open-transaction
accounting snapshot assertion failed. Including parent suites, there were
27 failed and 16 passed test/subtest events, with no skips.

The runner used its own disposable PostgreSQL cluster on loopback port
55491 with explicit review_app/review_e2e DSNs. Both databases applied all
55 migrations and skipped all 55 on rerun; cleanup.log confirms the owned
server stopped. No kaya process was operated on. Formatting and diff
checks passed. Full race/coverage and E2E were not rerun for this RED-only
checkpoint; prior green coverage does not describe this tree. The
production fix, gateway regression and remaining Task 17 work stay open.

Implementation checkpoint (2026-11-10): repository and gateway RED
regressions are committed in `60d68b9` and `1dce935`. The production
comparison now includes source and semantic JSONB evidence, using the same
empty-raw default as insertion. Focused GREEN `run.NusFdg` passed 57
tests/subtests across PostgreSQL and gateway with no failures or skips.
Dewey's bounded read-only review found no Critical/Important issue; its
nonblocking suggestion is to add isolated mutations for all five nullable
token buckets, including NULL versus zero. The current implementation
retains all five comparisons. Full gate `run.VdSEsP` exited 0: build/vet
passed; race passed 4,576 tests/subtests across 34 packages with 80.6%
statement coverage; separate E2E race passed 186 tests/subtests (149.735s).
There were no failures; only the three expected full-race and one E2E
opt-in/child-entry skips. Both disposable databases applied all 55
migrations and skipped all 55 on rerun; cleanup confirmed the owned server
stopped. Formatting and diff checks passed. Runbook 4.77 records the
commands, manifest and limits. Task 17 is verified for this local
checkpoint; Task 18, other pending findings and the original
wallet-correction/production evidence-trigger requirements remain open.

## Task 18: Late Dispatch Markers Preserve Terminal State

**Files:** `postgres/request_repo.go`, new
`postgres/request_terminal_marker_test.go`,
`gateway/terminal_marker_test.go`, and the verification runbook.

**Interfaces:** retain `UpdateRequestStatus` for dispatch markers and its
not-found behavior. Transactional settlement/release keep owning terminal
transitions; a late marker against settled/released is a successful no-op.

- [x] Reproduce dispatching/streaming/non_streaming overwriting settled or
  released requests, including plan/wallet and zero-priced settlement.
- [x] Reproduce a real nonstream finalizer resuming after another transaction
  has settled or released the request; assert typed duplicate, mismatch or
  released handling rather than missing_reservation.
- [x] Guard only the public dispatch-marker path atomically; preserve the
  transactional release helper and existing nonterminal marker behavior.
- [x] Preserve terminal status, timestamps, usage, charge, money holds and
  original TPM settlement time; no double charge or additional usage.
- [x] Run focused race, independent review and full race/coverage plus E2E
  against owned disposable databases.

Task 18 focused race passed 54 tests/subtests with no failures/skips in
`run.UU9TZx`, after `run.ixZmDC` reproduced 32 failing leaves. The public
marker now uses a guarded single-statement UPDATE/EXISTS CTE, while the
private transactional release helper is unchanged. Terminal markers do
not clear anomaly details or move completion/TPM timestamps. Nonterminal
markers and missing-request errors retain their behavior.

Full gate `run.DRZo2C` completed build/vet, full race (4,630 tests/subtests
across 34 packages, no failures, 80.7% total statement coverage) and
separate E2E race (186 tests/subtests, 148.197s, no failures). All 54 new
tests/subtests passed again in the full run. The three full-race skips
and one E2E skip are the expected child-entry/opt-in load scenarios,
not unavailable databases. Both owned disposable databases applied all
55 migrations and skipped all 55 on rerun; cleanup confirmed the owned
PostgreSQL server stopped.

Boole's bounded read-only Task 18 diff review against `0cff06a` found no
confirmed actionable findings; it did not independently run database
tests. The regressions cover committed-before-resume ordering, not
overlapping uncommitted SQL lock waits. Formatting and diff checks passed.
Task 18 is verified for this local checkpoint. Other pending findings,
wallet corrections and the production evidence-to-correction trigger
remain open; see runbook 4.78 for the manifest, commands and evidence
boundaries. This is not a whole-branch until-clean conclusion.

## Task 19: Refresh Live Window Limits On Policy Revision

**Files:** `quota/service.go`, `postgres/quota_repo.go`, new
`postgres/quota_upgrade_test.go`, and the verification runbook.

**Interfaces:** add the selected entitlement's `PolicyVersionID` to
`ActivateWindowsCommand`. After locking the account, read the entitlement
FOR SHARE on the same transaction and reject a mismatching account or
policy with `conflict`. Hold that lock through admission commit.

- [x] Reproduce each live window retaining limit 100 after upgrade to 200,
  with used 90 and reserved 5; a hold of 20 must admit.
- [x] Preserve IDs, boundaries, creation time, used and existing reserved;
  change only the live enabled row's limit, including effective decreases.
- [x] Preserve expired/voided/disabled windows; newly enabled windows
  activate with the selected policy's limit.
- [x] Reject stale-policy snapshots before creating or rewriting windows;
  verify rejection leaves request, holds, rate and lease state unchanged.
- [x] Reproduce overlapping revision/admission in both lock orders and
  prove no stale rewrite can commit after a policy revision.
- [x] Run focused race, full race/coverage and E2E using owned disposable
  databases; record evidence and commit this fix locally.

Ruling: refresh lazily inside admission, not every entitlement revision
path. The read model already uses the current policy's limit; admission
must bring only its live enabled rows into agreement without rewriting
historical consumption. A rejected admission rolls back the refresh too.
A stale snapshot fails closed with the existing conflict code rather than
retrying internally with the same stale limits. Renewal under the same
policy remains valid; this does not remove the existing bounded cache.

RED commit checkpoint (2026-11-10): the latest user `commit` request
checkpoints `postgres/quota_upgrade_test.go` and this evidence without a
production fix. Fresh focused race run
`/private/tmp/yunhou-review-20261109.3NuMWy/run.3DxbOU/focused.jsonl`
exited 1: all 12 leaf cases failed on the intended behavior assertions
(16 failed test/subtest events including parents, no skips). The cases
cover all three upgrade bottlenecks, effective decreases and rollback,
history/disabled-kind preservation, newly enabled monthly windows, stale
policy snapshots, account ownership, and both revision/admission lock
orders. Policy-only updates demonstrate that incidental FK KEY SHARE
does not protect admission from a concurrent policy change.

The runner used its own disposable PostgreSQL cluster on loopback port
55491 with explicit review_app/review_e2e DSNs. Both databases applied all
55 migrations and skipped all 55 on rerun; cleanup.log confirms the owned
server stopped. No kaya process was operated on. Formatting and diff
checks passed. Full race/coverage and E2E were not rerun for that RED-only
checkpoint; prior green coverage did not describe that tree.

Implementation checkpoint (2026-11-10): admission passes the selected
policy ID to activation, which validates entitlement account/policy under
FOR SHARE after locking the account. Reused enabled live windows update
only limit_micros, including decreases. Rejected admission rolls back the
refresh, and stale policy snapshots fail with a non-retriable conflict.
The SHARE lock, unlike KEY SHARE, also blocks policy-only updates until
the admission commits. No migration or historical-counter rewrite.

Focused GREEN `run.m697te/focused.jsonl` passed all 16 tests/subtests
(PostgreSQL 4.543s), with no failures or skips. The runner used new owned
disposable databases, applied/reran all 55 migrations and stopped its
owned PostgreSQL server.

Full gate `run.RnaqMo` exited 0: build/vet passed; full race passed 4,646
tests/subtests across 34 packages, including all 16 Task 19 cases, with
80.7% statement coverage. E2E race passed 186 tests/subtests (146.804s).
The three full-race and one E2E skips were expected child-entry/opt-in
cases; no database acceptance skipped. Both new owned databases applied
55 migrations and skipped all 55 on rerun; cleanup confirms their owned
PostgreSQL server stopped.

The preceding full attempt `run.MDIhns` failed from filesystem exhaustion,
not a passing gate. Its logs were preserved; only stopped generated
temporary database data under the owned runner root were reclaimed before
retrying. See runbook 4.79 for that failure, the scoped cleanup and final
evidence. This local implementation checkpoint does not resolve other
review findings or the original wallet/production-trigger requirements.

## Task 20: Fair Bounded Upstream Health Scans

**Files:** `workers/upstream_health.go`, new
`workers/upstream_health_scan_test.go`, `postgres/oauth_repo.go`,
`postgres/oauth_operators_outbox_repo_test.go`, and the verification runbook.

**Interfaces:** change `ListUpstreamAccountsForHealth` to
`(ctx context.Context, cooldownDueBefore time.Time, afterAccountID string,
limit int) ([]domain.UpstreamAccount, error)`. Empty cursor starts at the
first eligible UUID; nonempty cursor returns only greater UUIDs.

- [x] Reproduce batch 2 repeatedly skipping a third, due-cooldown account
  behind two unchanged healthy/no-quota, static-skip or connector-error
  accounts. After pass 2 the third account must be active; the prefix
  status and updated_at must remain unchanged.
- [x] Cover exact-page wrap, an ineligible tail and newly due lower UUIDs;
  every pass visits at most its configured batch.
- [x] Preserve unvisited accounts on cancellation and cursor progress
  after a guarded probe panic; concurrent passes must not race or lose
  the next page.
- [x] Add the UUID predicate without changing eligibility. Keep per-worker
  cursor state, serialize passes, advance before each attempted probe,
  wrap once on an empty tail, and reset after a fully visited short page.
  Normalize batch limits outside 1..500 to 100, matching the repository.
- [x] Run focused worker race tests, full race/coverage and E2E on owned
  disposable databases, record evidence and commit locally.

Ruling: use per-worker keyset rotation, not updated_at ordering, offsets,
or a timestamp write on every skipped/error account. Those alternatives
either retain starvation paths, skip rows as eligibility changes, or
change cooldown/status semantics. No migration or cross-instance probe
ownership is added. Cost: a restart starts a new scan cycle; workers may
probe the same account independently, and scan latency grows with the
eligible population and per-account network timeout.

RED evidence: pre-fix `run.5dgAaD` reproduced eight failing behavioral
leaves, plus a panic fixture error that is not behavioral evidence.
The corrected panic regression failed at the cursor-progress assertion
in mutation run `run.M5fEje` when cursor advancement moved after probe.
After restoring advance-before-probe, focused race `run.E23NcF` passed
all 13 tests/subtests (1.851s), with no failures or skips. Each run used
owned disposable PostgreSQL and stopped its owned server.

Full gate `run.hqyAz0` exited 0: build/vet passed, full race passed 4,659
tests/subtests across 34 packages with no failures, total statement
coverage was 80.7%, and separate E2E race passed 186 tests/subtests
(141.800s). Only the expected child-entry and opt-in drills skipped;
no database acceptance skipped. Both owned databases applied all 55
migrations and skipped all 55 on rerun. Cleanup confirmed the owned
server stopped. See runbook 4.80 for the file manifest, commands,
evidence and remaining scope. This is a local commit checkpoint,
not a whole-branch until-clean review conclusion.

## Task 21: Include Anthropic Cache Creation In Responses Usage

**Files:** `providers/responses_client.go`, new provider and HTTP cache-usage
regressions, and the verification runbook.

**Interfaces:** preserve `ResponsesUsageShape(*openAIUsage, bool)`; the
existing false flag denotes Anthropic-origin exclusive input buckets.

- [x] Reproduce Anthropic input 2, cache read 3, cache creation 5, output 7
  rendering Responses input 5/total 12 instead of input 10/total 17.
  Exercise the real native decoder and stream adapter before rendering.
- [x] Cover missing/zero cache buckets and OpenAI-origin inclusive input,
  with stream fragmentation and both completed/incomplete terminals.
- [x] Add cache creation alongside cache reads only for exclusive input;
  preserve cached_tokens as the cache-read detail, not cache-write.
- [x] Verify real HTTP stream/nonstream Responses usage agrees with
  reported persisted buckets and TPM 17, with one charge/attempt and no
  held reservations or leases.
- [x] Run focused race, full race/coverage and separate E2E against owned
  disposable databases; record evidence and commit locally.

Ruling: fix the Responses aggregate at its existing inclusion boundary,
not the canonical accounting buckets or the Anthropic adapter's wire.
Counting both cache buckets for all origins would double count inclusive
OpenAI input. Renaming the shared inclusion flag is unnecessary churn.
No new response fields, migration, price or correction policy.

Evidence: pre-fix provider race reproduced the missing cache-write
totals, and HTTP `run.dwCrJQ` reproduced both stream/nonstream failures
while accounting and shared-fixture consumer controls passed.
Post-fix providers race passed (2.443s), and focused HTTP `run.cL9GIP`
passed all 25 tests/subtests (4.272s), without failures or skips.

The first full run, `run.GqtBCB`, exited 1 after the filesystem filled;
it is not a passing gate. After removing only confirmed-stopped
disposable database data and retaining logs, fresh full gate
`run.Y5btrk` exited 0: build/vet passed, full race passed 4,716
tests/subtests across 34 packages with no failures, statement coverage
was 80.7%, and separate E2E race passed 186 tests/subtests (147.656s).
Only the expected child-entry and opt-in drills skipped; no database
acceptance skipped. Both owned databases applied all 55 migrations and
skipped all 55 on rerun. Cleanup confirmed the owned server stopped.
See runbook 4.81 for the file manifest, commands and remaining scope.
This is a local commit checkpoint, not whole-branch review convergence.

## Task 22: Persist Only Replayable Responses Transcripts

**Files:** `httpapi/responses.go`, new
`httpapi/responses_chain_replay_test.go`, shared
`httpapi/protocol_surfaces_test.go` fixture, API guide/OpenAPI and runbook.

**Interfaces:** keep `ResponsesReplayToChat([]json.RawMessage)` as the
canonical replay validator; validate the combined input/output transcript
at `ResponsesHandler.persistChain` before any row or session binding.

- [x] Add real HTTP regressions for tool/text and before/tool/after stream
  output, with both clean completed and incomplete terminals. Assert
  output order, reported 7/5 usage, single settlement and no leaked holds.
- [x] Reproduce saved but unreplayable chains: expect no chain/binding and
  subsequent reference 404 without a new admission or upstream call.
- [x] Cover replayable text/tool output, an unrepresentable child that
  leaves its parent usable, and input/output adjacency that is only
  unrepresentable in the combined transcript.
- [x] Validate replayability centrally before persistence; log and skip
  only the new chain. Keep successful stream output/accounting unchanged.
- [x] Run focused race, full race/coverage and separate E2E on owned
  disposable databases, document evidence and commit locally.

Ruling: successful ordered streams remain successful; completion does
not guarantee that their history fits the existing Chat replay model.
Skip unusable chain state just as oversized transcripts are skipped,
rather than persist a response ID that deterministically returns 400.
Do not reorder output or loosen the explicit history rejection policy.
Cost: such response IDs are not referenceable (404), now documented.

RED-only local commit checkpoint (2026-11-10): the current commit request
records regressions and the plan, not the production persistence fix.
Initial focused run `run.OCLfgw` also exposed missing session-binder
wiring in the shared matrix fixture. Configure the real binder as in
`newProtoFixture`; do not weaken the chain or binding assertions.

Fresh package race command:
`GOMAXPROCS=2 zsh /private/tmp/yunhou-review-20261109.3NuMWy/run.zsh ./internal/inference/httpapi -timeout=10m`.
Artifacts are under
`/private/tmp/yunhou-review-20261109.3NuMWy/run.O5mXJY/`.
The run exited 1 in 58.972s: 364 tests/subtests passed, nine failure
events (seven cases and two parent tests), no skips. All failures are in
the new replayability regressions: unrepresentable transcripts are
persisted, and their later references return 400 instead of the intended
404. Both replayable before/tool controls passed, as did parent reuse,
ordered terminal output, reported usage and settlement/hold assertions.
No shared-fixture consumer outside the new tests failed.

Both owned disposable databases applied all 55 migrations and skipped
all 55 on rerun; `cleanup.log` confirms the owned server stopped.
`go vet ./internal/inference/httpapi`, formatting and diff whitespace
checks passed. Full-repository race, E2E and coverage were not rerun for
this intentionally non-green checkpoint; Task 21's 80.7% is not coverage
of this tree. Task 22's production fix, API documentation and full gate
remain open. No push, merge, deployment or kaya process operation.

Implementation checkpoint (2026-11-10): `persistChain` now validates
the combined transcript after the existing 4 MiB cap and before any
chain insert or session binding. API guide/OpenAPI document the explicit
non-persistence contract and correct the obsolete 512 KiB documentation.
Fresh pre-fix focused race `run.yy5rWY` reproduced the seven failing
cases; both valid controls passed. Post-fix complete HTTP package race
`run.se4rH9` passed all 373 tests/subtests in 58.719s, without failures
or skips. Its owned PostgreSQL stopped.

Fresh full gate `run.l5HHVZ` exited 0: build/vet passed; full race passed
4,727 tests/subtests across 34 packages, with zero failures and 80.7%
statement coverage. Separate E2E race passed 186 tests/subtests in
154.854s. Only the expected child entry and opt-in load/usage drills
skipped; no database acceptance skipped. Each owned database applied all
55 migrations and skipped all 55 on rerun. Cleanup confirmed the owned
server stopped and `postmaster.pid` is absent. Formatting, API YAML
parsing and diff checks passed. Lovelace's bounded read-only review found
no new important issue in this task diff; it did not independently run
tests or review the whole branch. See runbook 4.82 for the file manifest,
commands and compatibility limitation. This local checkpoint does not
complete the remaining review findings.

## Task 23: Preserve Explicit Reasoning Disable

**Files:** `providers/openai_chat.go`, `providers/anthropic.go`,
`providers/adapter.go`, provider and HTTP regression tests, API guide,
OpenAPI and verification runbook.

**Interfaces:** retain `ThinkingEnabled *bool`: nil leaves the upstream
default unchanged, false explicitly disables, true enables. Keep the
existing explicit `ReasoningEffort` precedence and effort vocabulary.
Chat uses its existing DeepSeek-style `thinking` dialect; Messages uses
`thinking.type`, Responses uses `reasoning.effort`.

- [x] Reproduce false disappearing on Chat and Messages egress, using all
  registered adapters and nil/true controls, streamed and nonstreamed.
- [x] Reproduce Responses `effort=none`, Messages `thinking=disabled` and
  both Chat toggle forms through real authenticated HTTP and PostgreSQL.
- [x] Emit `thinking: {type: disabled}` for an explicit false in the two
  affected adapters, with no budget and no change to an omitted toggle.
- [x] Preserve effort precedence and enabled-budget/output-cap behavior.
- [x] Run providers race, HTTP race and full build/vet/race/coverage/E2E
  with separate owned disposable databases; record commands and skips.
- [x] Update the API contract and commit this bounded correction locally.

Ruling: preserve the existing normalized toggle rather than promoting
`none` into the effort vocabulary. This fixes the lost disable intent
without treating disabling as a reasoning capability requirement or
changing effort routing. Cost: Chat retains its documented vendor
`thinking` dialect, not a claim that every OpenAI-compatible endpoint
supports that extension. Provider compatibility still needs live
acceptance; no real-provider call is part of this review.

Local commit checkpoint (2026-11-10): provider race and the full HTTP
package race passed, including the 48 authenticated ingress/egress
combinations. Full gate `run.4gNB3I` exited 0: build/vet passed, full race
passed 4,798 tests/subtests across 34 packages, statement coverage was
80.7%, and separate E2E race passed 186 tests/subtests (175.875s).
Only the expected child-entry and opt-in load/usage drills skipped;
no database acceptance skipped. Both owned disposable databases applied
55 migrations and skipped all 55 on rerun. Cleanup confirmed the owned
PostgreSQL stopped and `postmaster.pid` is absent. Runbook 4.83 records
the manifest, RED/GREEN evidence, interrupted disk-full gate, successful
rerun and bounded review. This checkpoint does not complete the original
wallet-correction/production-trigger scope or the until-clean review.
No push, merge, deployment or kaya process operation is included.

## Task 24: Preserve Rejected Responses Evidence And Validate Thinking

**Files:** `providers/openai_responses.go`,
`providers/openai_responses_stream.go`, focused provider regressions,
`httpapi/chat_completions.go`, parser and authenticated HTTP regressions,
API guide, OpenAPI, verification runbook and this plan.

**Interfaces:** keep the existing nonnil metering result plus error contract
for fully decoded executions. Refusals have no faithful internal Chat field,
so reject explicitly rather than convert them into empty successful answers.
Streaming rejection is sticky, but continues reading late metering evidence.

- [x] Reproduce failed HTTP-200 Responses losing identity and usage 10/3.
- [x] Reproduce nonstream refusals and streaming refusal delta/done, content
  part, output item and terminal-only evidence becoming empty successes.
- [x] Retain reported usage or refusal bytes for estimation without counting
  the same refusal again at each cumulative event; never serve success/DONE
  or create a stored Responses chain after the rejection.
- [x] Reproduce unsupported/missing Chat thinking types becoming disabled,
  then reject invalid types before admission, including with another knob.
- [x] Preserve valid enabled/disabled, omitted/null and valid boolean/effort
  precedence; update the obsolete unknown-type acceptance regression.
- [x] Run actual RED/GREEN tests, providers/parser race, authenticated HTTP
  race and fresh build/vet/full race/coverage/E2E on disposable databases.
- [x] Record evidence and remaining requirements, review and commit locally.

Ruling: use the established explicit-rejection policy instead of adding a
refusal field across every client surface and stored transcript. Cost:
Responses refusal output returns a translation error rather than a native
refusal answer. Failed nonstream responses retain metering without a success
payload. Validate every supplied thinking object before precedence selection;
valid booleans and explicit efforts retain their existing priority. Cost:
previously ignored invalid objects now return 400 even with another knob.

Tests-only checkpoint (2026-11-10): focused provider and parser race tests
both exit 1, reproducing the findings. Valid thinking precedence controls
pass. No production fix, database regression, full-suite coverage or E2E
verification is included in this checkpoint; the remaining Task 24 steps
are still open.

Local fix checkpoint (2026-11-10): `run.mUq0Vv` passed focused
providers, parser, authenticated HTTP and gateway race regressions on an
owned disposable database. Complete rejected streams without usage now
retain `RejectedComplete` evidence and estimate without client success;
bare refusal EOF without usage keeps all four fixture holds (three
windows plus Key budget) and a pending reconciliation job. The preceding
`run.SumfTL` failed only that new fixture's incorrect one-hold assertion;
the production behavior and the assertion that holds remain were unchanged.
`run.Czd9yy` passed build/vet, 4,930 race tests/subtests, 80.7% coverage
and 186 E2E tests/subtests. Bounded review then found mixed refusal
snapshots omitted non-refusal bytes. Eight new completed/incomplete
snapshot regressions reproduced the undercount; kind/index-aware
snapshot accounting fixed it and full providers race passed.
The follow-up bounded review closed that finding with no further confirmed
delta defect. `run.98p2aM` then failed one HTTP fixture wipe (leaf and
parent) with PostgreSQL 53100, No space left on device; 4,937 tests/subtests
passed, E2E was not reached, and the owned PostgreSQL stopped. No code
changed in response. Removed only verified-stopped owned temporary
database data while preserving logs. Fresh final `run.r6xaWn` exited 0:
build/vet passed, full race passed 4,939 tests/subtests across 34 packages
with zero failures, coverage was 80.8%, and separate E2E race passed 186
tests/subtests (214.535s), zero failures. Only the expected child entry and
opt-in drills skipped; no database acceptance skipped. Both independent
owned databases applied 55 migrations and skipped all 55 on rerun.
The previously disk-failed fixture passed. Cleanup confirmed PostgreSQL
stopped and `postmaster.pid` is absent. Runbook 4.84 records the manifest,
RED/GREEN and final-gate evidence, bounded review and remaining risks.
Original wallet correction and its production evidence caller remain
unfinished and outside this local commit checkpoint; the until-clean
review is not declared complete. No push, merge, deployment, adjacent
repository edit or kaya process operation is included.

## Task 25: Preserve Wallet Gates At Integer Boundaries

**Files:** `accounting/overage.go`, `accounting/wallet.go`,
`accounting/wallet_overflow_test.go`, `postgres/wallet_repo.go`,
`postgres/wallet_overflow_test.go`, this plan and the verification runbook.

**Interfaces:** preserve wallet micromoney units and source isolation.
`CheckWalletSpend` must count settled spend, each in-flight source and the
new hold without an unchecked intermediate sum. Representable negative
balances remain visible; unrepresentable balances return `ErrOverflow`.

- [x] Reproduce a monthly cap bypass with `MaxInt64-5` spent and a new
  10-micro hold, including a real bonus-funded settlement and cash admission.
- [x] Reproduce pre-summed in-flight holds returning invalid input instead
  of quota exhaustion after an over-hold settlement.
- [x] Reproduce two positive source balances falsely rejecting a freeze,
  two extreme deficits admitting it, and derived availability wrapping positive.
- [x] Fix the comparisons without floats or converting money to microcredits;
  retain exact-cap admission, bonus-first splitting and minimum signed balance.
- [x] Run focused RED/GREEN, accounting/PostgreSQL race, then fresh full
  build/vet/race/coverage/E2E with separate owned disposable databases.
- [x] Record independent review, evidence and remaining requirements for
  this bounded local checkpoint without push, merge or deployment.

**Focused evidence (2026-11-10):** `run.F0bnUQ/focused.jsonl` reproduces
the overflow failures against the pre-fix production code, including both
real PostgreSQL admission cases. `run.zpCDQD/focused.jsonl` passes all 20
tests/subtests across accounting and PostgreSQL under
`-race -count=1 -p 1`, with zero failures or skips. Both runs use owned
disposable databases; the GREEN run's cleanup confirms its server stopped.
Fermat's read-only review found no confirmed Critical or Important issue
in the five Go files, their callers and adjacent aggregation/test helpers.
Minor follow-ups are direct held-cash/held-bonus boundary cases and an
explicit zero-count assertion on `inference_ledger_entries`; the new
database test already checks wallet entries. No tests were run by that
reviewer.

**Final verification:** fresh `run.Qp5T7t` exited 0 on 2026-11-10:
build/vet passed; full race passed 4,959 tests/subtests across 34 packages
with zero failures and 80.7% statement coverage; separate E2E race passed
186 tests/subtests (147.238s), zero failures. The four skips are the
child-process entry and three opt-in drills, not unavailable-database
acceptance. Each independent disposable database applied all 55
migrations, then skipped all 55 on rerun. Owned-server cleanup succeeded
and `postmaster.pid` is absent. See runbook 4.85 for the manifest,
commands, evidence and remaining risks. Other pending findings and the
until-clean review remain unfinished.

## Task 26: Close Addressed Escalated Correction Jobs

**Files:** `postgres/reconciliation_repo.go`,
`postgres/correction_jobs_test.go`, this plan and the verification runbook.

**Interfaces:** retain `CorrectSettlement` and its one-transaction contract.
Escalation pages a human; it does not make an evidence/overage job
ineligible for resolution after a successful authorized correction.

- [x] Reproduce a successful correction leaving escalated `unknown_usage`,
  `crash_recovery` and addressed `settlement_overage` tasks unresolved.
- [x] Test pending/running controls, equal-money evidence, exact-hold
  corrections, still-over-hold anomalies and unrelated manual reasons.
- [x] Include `escalated` in both existing correction resolution predicates,
  without broadening the reason or hold guards.
- [x] Run focused RED/GREEN, full build/vet/race/coverage and E2E with
  independent owned disposable databases; review the bounded delta.
- [x] Record the evidence and commit locally, without claiming the pending
  evidence-retention, wallet-correction or production-trigger work complete.

Ruling: follow the existing `ResolveReconciliationJob` open-state contract
and reason/hold guards rather than automatically closing every anomaly.
An escalation remains visible whenever the corrected charge is still over
its hold or the job needs a different manual action.

**Focused RED/GREEN:** `run.r53niI` reproduced five escalated leaf failures:
accounting and token corrections succeeded, but addressed jobs stayed
escalated without a resolution. After the two predicate changes,
`run.7sIqxD` passed all 25 tests/subtests with zero failures or skips.
Both runs used owned disposable PostgreSQL; cleanup confirmed each
server stopped.

**Final verification:** fresh `run.2VvYtW` exited 0 on 2026-11-10.
Build/vet passed; full race passed 4,984 tests/subtests across 34 packages,
with zero failures and 80.8% total statement coverage. Separate E2E race
passed 186 tests/subtests in 168.228s with zero failures. Only the expected
child entry and opt-in load/usage drills skipped; no database acceptance
skipped. Both independent disposable databases applied all 55 migrations
then skipped all 55 on rerun. Cleanup confirmed the owned PostgreSQL
stopped and `postmaster.pid` is absent. Singer's bounded read-only review
found no confirmed local-commit blocker; nonblocking test gaps and the
separate already-resolved-overage question are recorded in runbook 4.86.
Other pending findings and the until-clean review remain unfinished.

## Task 27: Retain Conflicting Finalizer Evidence

**Files:** `gateway/service.go`, `gateway/settlement_evidence.go`,
`gateway/terminal_marker_test.go`, `gateway/terminal_evidence_test.go`,
`postgres/reconciliation_repo.go`, the operations runbook, this plan and
the verification runbook.

**Interfaces:** terminal anomaly reporting consumes the incoming normalized
`SettleCommand` and pinned price. Its versioned `settlement_evidence` object
retains distinct content-addressed observations, not new authoritative usage
revisions. Request-job upsert unions that object atomically while preserving
existing top-level detail, reason, status and deadline rules.

- [ ] Extend `TestGateway_NonstreamFinalizerPreservesCompetingTerminal`:
  mismatched/released finalizers retain the incoming 10/5 reported usage,
  attempt, raw usage and pinned price; exact duplicates still create no job.
- [ ] Add gateway regressions for distinct evidence, exact-retry dedup,
  normalized cache/reasoning buckets, nullable counters, missing/malformed
  raw usage, credit/wallet units, and unrelated job-detail refresh.
  Terminal, usage, ledger, rate and wallet snapshots must stay unchanged.
- [ ] Run focused race tests with owned disposable databases. Expected RED:
  missing incoming evidence, not a fixture or connection failure.
- [ ] Pass the normalized command and pinned price to terminal reporting;
  retain usage-only raw JSON (malformed input as explicit text evidence),
  source, nullable buckets, attempt, price identity and charge denomination.
  Union nested evidence on every request-job refresh, without selecting an
  authoritative candidate or automatically rewriting accounting.
- [ ] Run the focused race tests again. Expected GREEN: all new regressions
  and existing terminal/idempotency controls pass with no database skips.
- [ ] Run fresh build/vet/full race/coverage/E2E using independent owned
  disposable databases; record evidence and remaining scope, then commit
  locally as `fix(inference): retain conflicting settlement evidence`.

Ruling: retain observations in reconciliation detail rather than appending
usage revisions, because latest-revision reads are accounting authority.
Exact content retries deduplicate; distinct observations survive unrelated
refreshes. This is evidence retention, not the production correction caller
or wallet correction required by the original plan.

## Pending Review Findings (2026-11-10)

This list tracks findings and verified closures; unfinished items are
not waived scope:

- Responses stream output with a tool followed by text no longer creates
  an unusable stored chain; fixed and verified in Task 22.
- Explicit reasoning disable is now retained on Chat and Messages egress,
  including Responses `reasoning.effort=none`; fixed and verified in Task 23.
- Anthropic cache-write inclusion was fixed and verified in Task 21.
- Failed nonstream Responses retain reported usage and response identity;
  fixed and verified in Task 24.
- Responses refusals now explicitly reject while retaining metering and
  suppressing successful terminals and chains; fixed and verified in Task 24.
- Unknown or missing Chat thinking types now reject before admission;
  Task 24 valid-precedence controls and fresh full verification pass.
- The original wallet-correction and production evidence-to-correction
  requirements remain incomplete and need their own implementation audit.
- Ohm's read-only review of `bb80c3e` reports five provider defects:
  Chat refusal translation loses refusal text and produces empty success;
  incomplete function arguments are replaced with `{}` and made replayable;
  incomplete responses without a reason become completed; partial public
  usage fabricates missing zero/total fields; Anthropic thinking rejects the
  valid 1025-output/1024-budget boundary. The reviewer reproduced these in
  owned temporary provider overlays; checkout regressions and fixes remain open.
- Boole's finding of late real finalizer evidence lost on terminal conflict
  remains source-confirmed and needs dynamic reproduction and a fix.
- Addressed escalated evidence/overage jobs remaining open after correction
  are reproduced, fixed and verified in Task 26. Re-enqueue policy for a
  subsequent changed correction to an already-resolved overage remains a
  separate open investigation, not behavior prescribed by Task 26.
- Wallet arithmetic overflow is reproduced, fixed and verified in Task 25
  with focused RED/GREEN, bounded review and a fresh full gate.

### Previous Verification Checkpoint

At `814d120`, Noether and Tesla's bounded independent reviews, including
Task 10/11 increments, report no confirmed unresolved important defects
in their respective scopes. They did not independently execute the
whole-branch gate.

Fresh `run.3fEfYH` passed build, vet and full race: 4,442 tests/subtests,
34 packages, zero failures, 80.6% total statement coverage. Its three
skips are the expected child-entry and opt-in load drills; no database
acceptance skipped. E2E passed 186 tests/subtests (189.043s), with one
expected `USAGE_ACCEPTANCE=1` opt-in skip. Both databases applied 55
migrations and skipped all 55 on rerun. Cleanup confirmed the owned
PostgreSQL server stopped; the runner exited 0. See runbook 4.71.

Original-plan section 1.1.6 item 1 remains a genuine unfulfilled
requirement: wallet settlement correction and the production evidence
trigger are not delivered. The named follow-up remains open, not a
plan exclusion. This commit checkpoint is not a claim that the full
original T4 or real-provider/client acceptance is complete.

## Execution Notes

Ruling: use the existing explicit-rejection policy for unrepresentable
history rather than inventing provider-dependent assistant-turn splitting.
Cost: callers must move subsequent text to a turn after tool results.

Ruling: 1 MiB is the supported SSE line bound, matching the existing native
translators. Larger lines now fail visibly rather than silently losing data.
Cost: unusually large single-event providers need smaller deltas.

RED/GREEN evidence: before the fixes, Messages before/call/after returned
no error, Responses terminal/transcript items followed closing order, and
70 KiB deltas lost content and usage while 2 MiB lines reached DONE. The
new regressions passed after the changes. The complete providers suite
passed `go test -race -count=1 ./internal/inference/providers` (2.480s).

Checkpoint ruling: the latest user request is to commit the completed
changes locally. Do not fold newly reported defects into this checkpoint
without reproduction and verification. The review goal remains unfinished.

Pending reviewer findings at this checkpoint:

- P1: a stale entitlement-sync grant plan can outlive a refund and two
  noop-delivered messages; reproduce with a PostgreSQL barrier test.
- P2: model-level tools/reasoning capability refusal reaches the facade
  as untyped invalid_input and becomes 502 rather than capability 400.
- P1: in-band error followed by DONE can emit a successful terminal and
  mark Responses persistable; native adapters have the same failed-state gap.
- P1: native non-stream Anthropic/Responses output can reorder text and
  tool calls while folding through Chat.
- P2: interrupted Anthropic message_start output usage is not retained
  when no subsequent message_delta arrives.

All five remain unresolved and require dynamic reproduction. The first
two were identified in baseline code. See runbook section 4.63 for
evidence boundaries and compatibility costs. The providers reviewer found
no confirmed new issue in the three checkpoint fixes but did not complete
every native item-boundary/refusal/multiturn combination.

Checkpoint verification completed with exit 0 in
`/private/tmp/yunhou-review-20261109.3NuMWy/run.dY8W6i`: build/vet passed;
full race had 4,306 test/subtest passes, E2E had 181, coverage was 80.6%.
The four expected child-entry/opt-in skips are listed in runbook 4.63.4.
Both disposable databases passed 55 migrations and their idempotent rerun.
The owned PostgreSQL cluster was stopped by its runner; kaya was untouched.

### Refund Race Checkpoint

The latest commit request checkpoints Task 5 only; Tasks 6-9 and the
until-clean review goal remain unfinished. The five-finding list above
describes the preceding checkpoint, not the current resolution status.

RED evidence: both outbox and direct variants of
`TestEntitlementSync_RefundBeforeApplyCannotLeaveStaleGrant` left an active,
open-ended entitlement after refund and after both messages were delivered
as noops. The failing run was
`/private/tmp/yunhou-review-20261109.3NuMWy/run.wI6xQY`.

The fix reads and applies on one transaction, holding shared subscription
and paid-order locks until convergence and delivery commit. Account
creation also uses this transaction, avoiding a second pool connection
while source locks are held. Direct optimistic conflicts now return a
retryable conflict instead of a successful action count.

GREEN focused evidence:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.4poINd` completed with exit 0:
314 test/subtest passes across workers and PostgreSQL, including both
refund regressions. Only the child-process entry point was skipped.
Its owned PostgreSQL server was stopped by the runner.

Full checkpoint verification completed with exit 0 in
`/private/tmp/yunhou-review-20261109.3NuMWy/run.wgodFl`: build/vet passed;
full race had 4,309 test/subtest passes across 34 packages, E2E had 181,
and coverage was 80.5%, above the 80% floor. The only skips were the four
expected child-entry/opt-in cases listed in runbook section 4.64.4.
Both disposable databases passed all 55 migrations and their idempotent
rerun. Cleanup confirmed the owned PostgreSQL server stopped; kaya was
untouched. The file manifest and remaining risks are in runbook 4.64.

### Model Capability Checkpoint

The latest commit request checkpoints Task 6 only. Tasks 7-9 and the
until-clean review goal remain unfinished; this is not a final clean
review claim.

RED evidence:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.CU2MzY`.
All five gateway cases returned an untyped capability error; `/chat`
tools, thinking and protocol refusals returned 502 instead of 400.
The real upstream invalid-input control already returned structured 502.

The four model-level protocol, image, tools and reasoning checks now
return the existing typed capability error, before any admission.
Generic invalid-input and actual upstream rejection mappings are unchanged.

Focused GREEN evidence:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.8l1Q7Q` completed with exit 0:
11 test/subtest passes across gateway and E2E, no skips or failures.
The regressions assert zero requests, attempts, money/rate reservations,
leases, ledger entries and upstream dispatch on capability refusal.
The upstream-invalid-input control still returns HTTP 502 with its
original upstream status and code. The runner stopped its owned
PostgreSQL server.

Full checkpoint verification completed with exit 0 in
`/private/tmp/yunhou-review-20261109.3NuMWy/run.aqkdPZ`: build/vet passed;
full race had 4,315 test/subtest passes across 34 packages, E2E had 186,
and coverage was 80.5%, above the 80% floor. Only the four expected
child-entry/opt-in cases listed in runbook section 4.65.4 were skipped.
Both disposable databases passed all 55 migrations and their idempotent
rerun. Cleanup confirmed the owned PostgreSQL server stopped; kaya was
untouched. The file manifest and remaining risks are in runbook 4.65.

### Sticky Failed Stream Checkpoint

The latest commit request checkpoints Task 7 only. Tasks 8-9 and the
until-clean review goal remain unfinished.

Provider RED logs:
`.superpowers/sdd/2026-11-09-model-gateway-review-convergence/task-7-red.jsonl`.
Error followed by DONE/completion was accepted as a successful terminal,
Messages/Responses emitted success, and Responses became persistable.
The database-backed RED run
`/private/tmp/yunhou-review-20261109.3NuMWy/run.I0n7d5` reproduced all
three surfaces settling as reported with a completed attempt; Messages
emitted message_stop and Responses emitted response.completed and saved
one response chain.

Failure is now sticky in the usage tracker and both native upstream
adapters. Messages/Responses client renderers stop rendering after the
first error. Later cumulative usage still updates metering without
restoring successful completion or response-chain persistence.

Focused GREEN evidence: all providers passed
`go test -race -count=1 ./internal/inference/providers` (2.660s).
The disposable-database run
`/private/tmp/yunhou-review-20261109.3NuMWy/run.t5Kh2u` passed all four
HTTP test/subtest cases, with no skips. It verified failed
stream_interrupted attempts, estimated 12/9 usage and no response chains
on all three surfaces. Cleanup stopped the runner's own PostgreSQL
server. The raw Chat passthrough boundary above remains explicit.

Full checkpoint verification completed with exit 0 in
`/private/tmp/yunhou-review-20261109.3NuMWy/run.kBQlZC`: build/vet passed;
full race had 4,358 test/subtest passes across 34 packages, E2E had 186,
and total statement coverage was 80.6%, above the 80% floor. Only the four
expected child-entry/opt-in cases listed in runbook 4.66.4 were skipped.
Both disposable databases applied 55 migrations and skipped all 55 on
their idempotent rerun. Cleanup confirmed the owned PostgreSQL server
stopped; kaya was untouched. The file manifest and remaining risks are
in runbook 4.66. Tasks 8-9 and the until-clean review remain unfinished.

### Native Nonstream Output Checkpoint

The latest commit request checkpoints Task 8 only. Fully decoded native
output with nonempty text after a tool call now returns a 502 translation
error without a reordered payload or Responses chain. Complete metering
survives the rejection, settles once, and prevents gateway replay or
failover; malformed responses remain unknown execution.

Provider regressions cover text/tool/text, tool/text/tool, reported and
absent usage, supported arrangements and dispatch metadata. The focused
disposable-database run `run.gVp6cA` passed 24 gateway/HTTP tests and
subtests, including single settlement, no fallback and no response chain.
The complete providers race suite passed again before commit (2.349s).

Full verification in `run.yj5MJY` is non-green: build/vet passed, 4,404
tests/subtests passed, but `TestWSSlowConsumer` and
`TestWSConnectionCaps` failed in unchanged `internal/relay`. A separate
relay recheck still failed the slow-consumer test. Total statement
coverage was 80.6% from this non-green run, not a passing gate.

The first separate E2E run reached the default package timeout. The
retry `run.jJtp92`, using `GOMAXPROCS=2` and `-timeout=30m`, exited 0
with 186 test/subtest passes and one expected opt-in skip. It used new
independent disposable databases; both passed all 55 migrations and the
idempotent rerun, and cleanup stopped the owned PostgreSQL server.
No test assertions or individual deadlines changed.

The file manifest, exact evidence paths and remaining risks are in
runbook 4.67. Task 9, unresolved relay failures and the until-clean
review remain open. No push, merge, deployment or kaya process operation
is part of this checkpoint.

### Interrupted Anthropic Usage Checkpoint

The latest user request is a local Task 9 commit checkpoint, not a new
whole-branch review. Preserve output usage from `message_start`, including
reported zero, without changing the existing cumulative replacement rule
for later `message_delta` usage.

Provider RED reproduced missing output in the tap, raw usage and translated
wire on EOF, read error, protocol error and a delta without output.
The disposable-database RED run `run.RwICUb` reproduced a charge of 25
instead of 59 for known input/output 25/17. The full providers race suite
passed after the assignment fix (5.438s).

The gateway regression follows the existing relay contract: without a
terminal marker it calls `Finish(EndUpstreamBroke)`, then verifies that a
duplicate `Finish(EndCompleted)` cannot change settlement or attempt state.
The first post-fix test attempt incorrectly passed `EndCompleted` for the
interrupted stream; that fixture error was corrected, not production
finalization behavior.

The unfinished `internal/relay/conn_test.go` changes are excluded from this
checkpoint. Their owner confirmed that `TestWSConnectionCaps` still
contains a deliberately delayed hello reproducing its timeout; no relay
production fix has been made. They remain in the worktree, without being
reverted or presented as completed work.

The corrected database-backed gateway regression passed both interrupted
endings (three test/subtest passes, 0.33s) in `run.q2GTJf`. Independent
E2E `run.U9pgJY` exited 0 with 186 test/subtest passes and one expected
`USAGE_ACCEPTANCE=1` opt-in skip. It used its own disposable PostgreSQL
cluster, whose cleanup confirmed server stopped.

Full `run.q2GTJf` exited 1: build/vet passed, 4,424 test/subtest passes,
seven failure events, 32 passing packages and two failing packages.
Coverage was 80.6% from this non-green run. The three expected skips were
the quota worker child entry and two opt-in load drills, not unavailable
databases. Both disposable databases passed all 55 migrations and their
idempotent rerun, and the owned PostgreSQL cluster stopped.

Besides the excluded relay RED, three Messages integration fixtures
reported initial output 1 while asserting NULL. Correct the EOF fixture
to truly omit output, preserving unknown-bucket coverage, and make client
interruption/protocol-error assertions retain 1. These two integration
test files are part of the checkpoint; no additional production change.

The complete integration race recheck `run.KdvIFK` exited 0 after those
corrections: 82 test/subtest passes, two expected opt-in load-drill skips,
no failures or database-unavailable skips (143.433s). A new owned
disposable PostgreSQL cluster applied all 55 migrations to both databases,
skipped all 55 on rerun, and stopped on cleanup. Task 9 implementation
and its seven-file local checkpoint are verified; the earlier full gate
is still non-green, relay work remains excluded, and review convergence
is not complete. No push, merge, deployment or kaya process operation.
