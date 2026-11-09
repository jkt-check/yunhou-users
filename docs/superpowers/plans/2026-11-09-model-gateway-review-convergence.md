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
