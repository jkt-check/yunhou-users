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
