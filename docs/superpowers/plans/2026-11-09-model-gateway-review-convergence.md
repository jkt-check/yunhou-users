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
- [ ] Fix confirmed important defects with RED/GREEN tests.
- [ ] Review the resulting branch independently, including the focus list.
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

**Files:** `anthropic.go`, `openai_responses.go` and provider tests.

- [ ] Reproduce text/tool/text and tool/text/tool folding into reordered
  Chat output.
- [ ] Reject unrepresentable nonstream output explicitly while preserving
  supported arrangements; never persist reordered replay history.

## Task 9: Interrupted Anthropic Start Usage

**Files:** `anthropic_stream.go` and provider stream tests.

- [ ] Reproduce message_start input/output usage followed by error or EOF
  losing known output usage.
- [ ] Preserve the initial counters; later cumulative message_delta
  counters replace, rather than add to, the initial values.

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
