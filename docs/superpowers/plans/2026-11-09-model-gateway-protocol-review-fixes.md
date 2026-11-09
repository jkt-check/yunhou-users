# Protocol Review Fixes Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans for inline execution.

**Goal:** Fix review findings 3-5 after committing findings 1-2.

**Architecture:** Keep representable Responses history ordered and reject text
after a function call before tool results, since the internal Chat turn places
content before all calls. Explicit reasoning effort takes precedence over the
thinking toggle, matching Chat and the gateway's capability routing. Carry
known rate recovery seconds through the facade to the HTTP response.

**Tech Stack:** Go, existing adapters, Gin, disposable PostgreSQL test runner.

**Spec:** The user's November 9, 2026 two-commit execution request.

## Constraints And Decisions

- Current worktree only; no push, merge, deployment or Kaya process changes.
- Explicit disposable database DSNs only; no secrets or production providers.
- Commit findings 1-2 first, then findings 3-5 together after verification.
- Finding 3: splitting an unresolved tool turn into consecutive Chat assistant
  messages cannot guarantee provider acceptance. Reject nonempty trailing text
  rather than silently moving it. Text before calls and parallel calls remain
  supported; parsing and stored transcript replay share the same conversion.
- Finding 4: prefer the existing explicit-effort precedence to a new ingress
  rejection. Cost: callers passing contradictory flags get the explicit effort.
- Finding 5: only positive known recovery seconds produce Retry-After; do not
  invent a delay for in-flight TPM, concurrency, quota or upstream errors.

## Review Focus

- Text between two calls must not be moved before the first call.
- Parallel calls, text before calls and post-result text must still work.
- Explicit reasoning levels must survive false, true and unset toggles.
- Wrapped typed rate errors must retain recovery metadata.
- Unknown recovery must omit Retry-After and retain the existing 429 envelope.

### Task 1: Responses History

**Files:** `internal/inference/providers/responses_client.go`,
`internal/inference/providers/responses_client_test.go`.

**Interface:** `responsesItemsToChat` shared by parse and replay.

- [x] Add failing before/call/after and interleaved-call rejection regressions;
  change the existing test that asserts reordered text to assert faithful cases.
- [x] Run focused race tests and observe RED.
- [x] Reject unrepresentable text-after-call combinations with invalid_input.
- [x] Run providers race suite and observe GREEN.

### Task 2: Reasoning Precedence

**Files:** `internal/inference/providers/openai_responses.go`,
`internal/inference/providers/openai_responses_test.go`,
`internal/inference/httpapi/reasoning_precedence_test.go`,
`internal/inference/providers/adapter.go`.

**Interface:** Existing ChatRequest fields and adapter BuildPayload.

- [x] Add failing false/high regression and cross-adapter precedence matrix.
- [x] Run focused race tests and observe RED.
- [x] Prefer explicit effort before the thinking toggle in Responses.
- [x] Run provider and ingress regressions and observe GREEN.

### Task 3: Chat Retry Metadata

**Files:** `internal/service/errors.go`, `internal/service/chat_facade.go`,
`internal/service/chat_facade_test.go`, `internal/handler/chat.go`,
`internal/handler/chat_test.go`, `tests/e2e/chat_retry_after_test.go`.

**Interface:** Add `RetryAfterSeconds int` to ChatLimitError; keep sentinels
and response envelope unchanged.

- [x] Add facade and handler regressions for known, unknown and wrapped errors.
- [x] Observe RED, then retain metadata and emit positive seconds only.
- [x] Run focused service, handler and real-admission E2E race regressions
  and observe GREEN; full service/handler suites run in Task 4.

### Task 4: Final Verification And Commit

**Files:** `docs/runbooks/2026-11-05-model-gateway-verification.md`.

- [x] Run build, vet, full race coverage and E2E with owned temporary databases.
- [x] Review the complete second-commit diff independently; address important
  findings with RED/GREEN and rerun the full suite if code changes.
- [x] Record test counts, skips, coverage, migration replay and cluster cleanup.
- [x] Commit scoped code, tests and documents as the second commit.

**Verification:** `run.ObuVVi` exited 0: build/vet passed; full race suite
4,291 test/subtest passes, three expected skips; race E2E 181 passes, one
expected skip. All 35 packages passed. Statement coverage 80.5% (80% floor).
Both disposable databases applied 55 migrations, repeat migration skipped
all 55, and the owned PostgreSQL cluster stopped. Independent review found
no confirmed bugs. Detailed evidence, deferred test opportunities and file
manifest: verification runbook section 4.62.

**Delivery:** first commit `5f74ee8`; second commit message
`fix(inference): preserve protocol intent and retry metadata`. Worktree and
branch are retained; no push, merge or deployment.
