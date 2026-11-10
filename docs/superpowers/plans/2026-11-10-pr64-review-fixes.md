# PR #64 Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the four confirmed PR #64 review findings without changing the gateway's public interfaces or accounting model.

**Architecture:** Use the persisted entitlement anchor inside the existing activation transaction, restrict Responses replay merging to tool-call conversion, add protocol-required defaults only at the public usage serializers, and apply the existing publish validator to parsed rollback targets. Historical snapshots remain readable; no migration or new dependency is needed.

**Tech Stack:** Go 1.25, PostgreSQL TIMESTAMPTZ, existing Go tests and race detector.

**Spec:** The four findings and the design decisions in this plan are the binding specification for this focused fix. The remainder of PR #64 is already reviewed and is outside this change.

## Global Constraints

- Work only in `/Users/jack-tian/Downloads/github/yunhou-users-model-gateway` on `fix/model-gateway-completion`; the base is `652e237d5d32c6fe42233547f298fbb1bf438cd9`.
- Implement through a secondary coder agent, one task at a time; the controller reviews every task and the complete focused diff.
- Test-driven: add each regression test, run it and capture the expected failure before editing production code, then make the smallest implementation change and rerun it.
- Use the real PostgreSQL supplied by the controller for database tests. Never use a developer or production database, and never accept a skipped database test as verification.
- Do not change exported signatures, database schemas, dependencies, internal nullable usage buckets, or unrelated PR behavior.
- Preserve the established rejection of lossy tool/history conversion. Do not concatenate adjacent assistant messages or reorder assistant text around tool calls.
- Anthropic `usage` MUST contain `input_tokens` and `output_tokens`; Responses `usage` MUST contain `input_tokens`, `output_tokens`, `total_tokens`, `input_tokens_details.cached_tokens`, and `output_tokens_details.reasoning_tokens`. A missing numeric value is rendered as zero at this public boundary only. Reported values, including a reported total and explicit zeroes, remain intact.
- Keep `addUsageDetails` and `derivedUsageTotal` unknown-preserving; do not convert missing internal counters into zero for settlement or native Chat relay.
- `ParseSnapshot` retains its read compatibility. Rollback must additionally run `validateCatalogForPublish` and return `invalid_input` without activating an unpublishable legacy revision.
- Do not commit, push, merge, deploy, modify GitHub, or dispatch further subagents. The user delegated implementation and review, not Git history mutation or publishing.
- Avoid new comments except where existing conventions make a boundary invariant useful. Update comments that describe the old behavior. Run `gofmt` on modified Go files and `git diff --check`.

---

### Task 1: Reuse the persisted quota anchor

**Files:**
- Modify: `internal/inference/postgres/quota_repo.go:199-272`
- Modify: `internal/inference/quota/service.go:265-271`
- Test: `internal/inference/postgres/quota_concurrency_test.go`
- Test: `internal/inference/quota/service_test.go`
- Modify stale comment: `internal/inference/quota/reservation.go:152-157`

**Interfaces:**
- Consumes: `quota.ActivateWindowsCommand` and the locked `inference_entitlements` row.
- Produces: unchanged `Store.ActivateWindowsTx` signature and quota window semantics.

**Design:** The command's in-memory anchor may retain nanoseconds, while PostgreSQL TIMESTAMPTZ stores microseconds. Read `anchor_at` together with the already locked entitlement's account and policy fields, and use that persisted value for `quota.ConsumptionWindow`. Explicitly truncate the consumption instant to microseconds in `Service.Admit` and at the direct store boundary before any window resolution, evaluation or insertion. Because the value inserted into a new five-hour window is already exactly representable, no database rounding occurs and no cross-language rounding rule is assumed. Do not broaden equality comparisons or introduce time tolerances. The stored timestamp is the authoritative original anchor.

- [ ] **Step 1: Write the failing real-database test**

Add `TestAdmitReusesStoredAnchorWithSubMicrosecondCallerTime` using existing `seedFixture`, `seedPrice`, `admitCmd`, and `newQuotaService` helpers. The essential sequence is:

```go
func TestAdmitReusesStoredAnchorWithSubMicrosecondCallerTime(t *testing.T) {
	db, s := testDB(t)
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	stored, err := s.GetEntitlement(context.Background(), f.entID)
	if err != nil { t.Fatal(err) }
	cmd := admitCmd(f, priceID, 100, 1_000_000, 10_000_000, 100_000_000, nil)
	cmd.At = stored.AnchorAt.Add(time.Hour)
	cmd.Entitlement.AnchorAt = stored.AnchorAt.Add(789 * time.Nanosecond)
	if _, err := newQuotaService(s).Admit(context.Background(), cmd); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	cmd.Request.ID = uuid.NewString()
	if _, err := newQuotaService(s).Admit(context.Background(), cmd); err != nil {
		t.Fatalf("second admission must reuse stored windows: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference_quota_windows WHERE entitlement_id=$1`, f.entID).Scan(&count); err != nil { t.Fatal(err) }
	if count != 3 { t.Fatalf("active windows = %d, want 3", count) }
}
```

The mutation caught is using the caller's sub-microsecond anchor instead of the stored anchor. Keep all three enabled windows so the test covers weekly and monthly reuse, and give both requests distinct IDs.

- [ ] **Step 2: Run the test against unmodified production code**

Run with the controller's `DATABASE_URL`:

```bash
go test -race -count=1 -run '^TestAdmitReusesStoredAnchorWithSubMicrosecondCallerTime$' -v ./internal/inference/postgres
```

Expected: FAIL on the second admission, normally an exclusion violation wrapped by `insert quota window` (`23P01`), not a skipped test or fixture error. Record the actual output.

- [ ] **Step 3: Add the same-precision five-hour regression and verify RED**

Add `TestAdmitReusesFiveHourWindowWithSubMicrosecondConsumptionTime` with the same fixture and two-admission structure as Step 1, but keep `cmd.Entitlement.AnchorAt = stored.AnchorAt` and set `cmd.At = stored.AnchorAt.Add(time.Hour).Add(789 * time.Nanosecond)`. Both admissions must succeed with exactly three windows, reusing the original five-hour row as well as the anchored rows.

Run it against the current implementation:

```bash
go test -race -count=1 -run '^TestAdmitReusesFiveHourWindowWithSubMicrosecondConsumptionTime$' -v ./internal/inference/postgres
```

Expected: FAIL on the second admission with `five_hour` and `23P01`. This is the same timestamp round-trip defect at the consumption-instant boundary, not a reason to add fuzzy interval comparisons.

- [ ] **Step 4: Unify the service and persistence instants**

First add `TestAdmitAlignsAdmissionTimeToPersistencePrecision` in `service_test.go`: use `serviceFixture`, set the service clock to `time.Date(2026, 9, 8, 4, 0, 0, 789, time.UTC)` via `domain.FixedClock`, admit the unchanged valid fixture, and require `res.AdmittedAt.Equal(time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC))`. Also require the persisted reserve command's `AdmittedAt` to equal the returned instant. Run it before editing `service.go` and record the missing-normalization failure. The already observed 789ns versus 1000ns failure belongs to the superseded Round design; Truncate must yield exactly zero nanoseconds, independent of database rounding.

Normalize after reading the caller's pinned instant or the service clock, before `admitOnce`:

```go
at := cmd.At.UTC()
if at.IsZero() {
	at = s.clock.Now()
}
at = at.UTC().Truncate(time.Microsecond)
```

This intentionally makes `AdmissionResult.AdmittedAt` the actual admission timestamp at persistence precision; the `AdmitCommand.At` caller field and all public signatures remain unchanged. Keep the store's entitlement lock query selecting `anchor_at` as its only additional column. At the store boundary use:

```go
var storedAnchor time.Time
// Scan accountID, policyID and storedAnchor using the existing locked query.
at := cmd.At.UTC().Truncate(time.Microsecond)
```

Use `storedAnchor` and `at` in `quota.ConsumptionWindow`, and use `at` in the later `quota.Interval.Contains` call. This ensures `EvaluateAdmission` receives windows containing the same normalized instant instead of failing `five_hour not resolved`. Truncation happens before a five-hour start is inserted, so the inserted timestamp is exactly representable and requires no PostgreSQL rounding. Do not use tolerances. Keep the account/policy checks, account lock, retry contract, and insertion behavior unchanged. Keep comments concise and limited to the persistence-precision invariant. Correct the stale `/chat` comment to match the shared persistent RPM/TPM admission enforcement described in `CLAUDE.md` and `reserveRateTx`; no behavior change belongs to this comment edit.

- [ ] **Step 5: Verify both regressions and neighboring quota behavior**

```bash
go test -race -count=1 -run '^(TestAdmitReusesStoredAnchorWithSubMicrosecondCallerTime|TestAdmitReusesFiveHourWindowWithSubMicrosecondConsumptionTime|TestRecover_KeyLeaseExpired_ThenSubsequentAdmissionSucceeds)$' -v ./internal/inference/postgres ./internal/inference/workers
go test -race -count=1 ./internal/inference/quota ./internal/inference/postgres
```

Expected: PASS with no skipped PostgreSQL tests. Record both the focused and package results.

- [ ] **Step 6: Self-review without Git mutation**

Run `gofmt` on the changed Go files and `git diff --check`. Inspect only this task's diff. Do not commit. Append the new RED/GREEN evidence to the report specified in the dispatch, including any remaining concerns.

---

### Task 2: Preserve assistant turns and required usage shapes

**Files:**
- Modify: `internal/inference/providers/responses_client.go:294-372` and `524-560`
- Modify: `internal/inference/providers/messages_client.go:553-584`
- Test: `internal/inference/providers/responses_client_test.go`
- Test: `internal/inference/providers/partial_usage_test.go`
- Test: `internal/inference/httpapi/partial_usage_test.go:16-110` (cross-package public contract coverage found during review)

**Interfaces:**
- Consumes: existing `model.ChatMessage`, `openAIUsage`, and native protocol conversion functions.
- Produces: unchanged exported signatures; distinct ordinary assistant Chat turns and SDK-compatible public usage maps.

**Design A:** A message item always appends its own Chat message. A function call appends its tool call to the immediately preceding assistant message when one exists, otherwise it creates an assistant message. This preserves text-before-tools and consecutive parallel calls without concatenating separate assistant message items. Keep the existing text-after-unresolved-call rejection.

```go
case "", "message":
	// Existing role/content validation stays unchanged.
	out = append(out, model.ChatMessage{Role: role, Content: text})
case "function_call":
	// Existing call validation stays unchanged.
	call := model.ToolCall{ID: it.CallID, Type: "function", Function: model.ToolCallFunction{
		Name: it.Name, Arguments: it.Arguments,
	}}
	if len(out) > 0 && out[len(out)-1].Role == "assistant" {
		out[len(out)-1].ToolCalls = append(out[len(out)-1].ToolCalls, call)
	} else {
		out = append(out, model.ChatMessage{Role: "assistant", ToolCalls: []model.ToolCall{call}})
	}
```

**Design B:** Continue to preserve unknown values internally and optional detail values. At the two public protocol serializers only, emit zero for an unknown required numeric value, and emit detail objects with zero counters for Responses. When Responses has no usable reported total, derive the public total from the public input/output counters after unknown values have become zero; overflow or invalid arithmetic falls back to zero. This preserves the Step 2 literals (`input_only` total 2, `output_only` total 7) without changing the internal unknown-preserving helpers.

- [ ] **Step 1: Add failing assistant-boundary tests**

In `responses_client_test.go`, exercise both `ParseResponsesRequest` and `ResponsesReplayToChat` with this input:

```go
input := `[{"role":"assistant","content":"Hello"},{"role":"assistant","content":"World"}]`
```

For each parsed/replayed result assert two messages, with literal contents `Hello` and `World`; the current one-message `HelloWorld` result must fail. Add a separate boundary case with `Hello` then an empty assistant message, asserting the empty turn is retained too. The existing `TestResponsesReplay_ParallelToolGroupingPreservesText` must remain unchanged as protection for text-before-tools and parallel grouping.

- [ ] **Step 2: Add failing public usage expectations**

Update the literal `messages` and `responses` expectations in `partialUsageCases` for all providers:

```text
messages input_only  => {"input_tokens":2,"output_tokens":0}
messages output_only => {"input_tokens":0,"output_tokens":7}
messages total_only  => {"input_tokens":0,"output_tokens":0}
responses input_only => {"input_tokens":2,"output_tokens":0,"total_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}
responses output_only=> {"input_tokens":0,"output_tokens":7,"total_tokens":7,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}
responses total_only => {"input_tokens":0,"output_tokens":0,"total_tokens":9,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}
```

Apply the same required-field rule to detail-only, explicit-zero, complete, cache, and arithmetic cases rather than leaving legacy omissions in table rows. For unknown input/output/total, emit zero. When input and output are both known and no reported total is usable, retain the existing arithmetic total. Retain known detail values and add the other required Responses detail at zero. Preserve the `chat` column exactly: native Chat relay still distinguishes unknown from zero. These literal tests must drive nonstream and stream translators through real adapters, not test a newly invented helper.

- [ ] **Step 3: Run focused tests to verify RED**

```bash
go test -race -count=1 -run '^(TestResponsesReplay_|TestPartialUsage_)' -v ./internal/inference/providers
```

Expected: new assistant test fails with merged `HelloWorld`; updated usage cases fail on missing required fields. Existing rejection cases may not be weakened to obtain a pass. Record representative output.

- [ ] **Step 4: Implement the two minimal serializer changes and replay fix**

Follow Design A and B above. In `anthropicUsageShape`, always populate the two required top-level counters after the existing cache arithmetic. In `ResponsesUsageShape`, preserve existing reported/derived calculations, then always populate the five required values using zero only when the relevant calculated pointer is nil. Continue to omit optional Anthropic cache fields when unknown. Do not modify `usage_wire.go`, `stream_usage.go`, accounting, or the openAI Chat serializer.

- [ ] **Step 5: Verify focused and full provider behavior**

```bash
go test -race -count=1 ./internal/inference/providers
```

Expected: all provider tests PASS, including assistant/tool ordering, partial usage and rejection cases.

The task review identified the old public expectations in `internal/inference/httpapi/partial_usage_test.go`. Before editing that test, run `DATABASE_URL=<disposable> go test -race -count=1 -p 1 -run '^TestV1PartialUsage_PublicFieldsMatchMetering$' ./internal/inference/httpapi` and record its missing-required-field failures. Replace the shared `public` expectation with explicit `publicMessages` and `publicResponses` strings matching Design B for `input_only` and `output_only`; keep `chat` and native inputs, database `sql.NullInt64` unknowns, charge and token expectations unchanged. Select the literal by `surface`, and update the assertion message so it no longer claims public required counters must be absent. Rerun the focused HTTP test and provider package. These are protocol-contract test corrections backed by the already verified required fields, not changes to production behavior or weakened assertions.

Run `gofmt`, `git diff --check`, self-review this task's diff, and append the requested report with RED/GREEN evidence. Do not commit.

---

### Task 3: Apply the publish gate to rollback

**Files:**
- Modify: `internal/inference/catalog/service.go:942-989`
- Test: `internal/inference/catalog/pool_strategy_test.go`

**Interfaces:**
- Consumes: `ParseSnapshot` and unexported `validateCatalogForPublish`.
- Produces: unchanged `Service.Rollback(ctx, toRevision, createdBy)` signature and immutable revision history.

**Design:** Keep the existing target status check and `ParseSnapshot` read gate. Collect the parsed snapshot's models, providers, deployments, and routes into slices in deterministic ID order, then call `validateCatalogForPublish` before `LatestRevision` and `InsertAndActivateRevision`. Sort IDs explicitly; `Snapshot.ModelIDs()` only covers models. Wrap validation errors with `domain.CodeInvalidInput` and `rollback target is not publishable`. Do not change `ValidateRouteForRead`, `ParseSnapshot`, `BuildCatalogPayload`, or the original target payload.

- [ ] **Step 1: Add a failing real-service rollback test**

Add `TestRollbackRejectsLegacyPoolStrategyWithoutActivatingRevision` in `pool_strategy_test.go`, using `testDB` and `seedProviderModelDeployment`. Publish a valid revision 1, read its raw payload, replace its one route's serialized `"pool_strategy":"round_robin"` with `"pool_strategy":"least_loaded"`, and store it as published active revision 2 using the existing Store methods. Assert the fixture replacement actually occurred. This represents a legacy snapshot which must remain parseable but cannot be republished.

```go
target, err := svc.Store().GetRevision(ctx, domain.ScopeCatalog, 1)
if err != nil { t.Fatal(err) }
raw := bytes.Replace(target.Payload.Raw,
	[]byte(`"pool_strategy":"round_robin"`),
	[]byte(`"pool_strategy":"least_loaded"`), 1)
if bytes.Equal(raw, target.Payload.Raw) { t.Fatal("legacy fixture did not change route strategy") }
legacy := &domain.ConfigRevision{Scope: domain.ScopeCatalog, Revision: 2,
	Status: domain.RevisionPublished, IsActive: true,
	Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: raw}, CreatedBy: "legacy"}
if err := svc.Store().InsertConfigRevision(ctx, legacy); err != nil { t.Fatal(err) }
if _, err := svc.Rollback(ctx, 2, "operator"); domain.CodeOf(err) != domain.CodeInvalidInput {
	t.Fatalf("rollback error = %v, want invalid_input", err)
}
```

Assert the error names `least_loaded` and the `round_robin` replacement, `LatestRevision` remains 2, and `LoadSnapshot` still loads revision 2 without dispatching the legacy route. Check `ParseSnapshot(target)` compatibility separately only if the fixture needs direct validation; never delete that existing compatibility test.

- [ ] **Step 2: Run the test against unmodified production code**

```bash
go test -race -count=1 -run '^TestRollbackRejectsLegacyPoolStrategyWithoutActivatingRevision$' -v ./internal/inference/catalog
```

Expected: FAIL because rollback currently succeeds and creates revision 3. It must not fail because the legacy fixture cannot be parsed.

- [ ] **Step 3: Implement deterministic snapshot validation in Rollback**

Retain the parsed snapshot in a variable. For each entity map, gather keys, sort them, and append values in that order; append routes by sorted model ID (their existing per-model order is already deterministic). Call the existing publish validator and return an `invalid_input` whose top-level `domain.Error.Message` includes the safe validation reason:

```go
if err := validateCatalogForPublish(models, providers, deployments, routes); err != nil {
	return 0, domain.NewError(domain.CodeInvalidInput,
		"rollback target is not publishable: "+routeRefusalMessage(err))
}
```

The HTTP error mapper deliberately exposes `domain.Error.Message` without the cause chain. Putting the existing validator's safe route ID, rejected value and replacement hint in that top-level message preserves the operator-facing guidance without changing generic error sanitization. Before this production edit, extend the rollback test to extract `*domain.Error` with `errors.As` and assert those details on `de.Message`, not only `err.Error()`; run it against the generic wrapped message and record the missing-detail RED.

Do not introduce a second validator or relax the legacy read path.

- [ ] **Step 4: Verify the regression and catalog suite**

```bash
go test -race -count=1 -run '^(TestRollbackRejectsLegacyPoolStrategyWithoutActivatingRevision|TestRollbackPublishesOldContentWithoutRewritingHistory|TestPublishedSnapshot_LegacyPoolStrategyStaysReadableButUnavailable)$' -v ./internal/inference/catalog
go test -race -count=1 ./internal/inference/catalog
```

Expected: invalid rollback is refused atomically, valid rollback still publishes a new revision, legacy snapshots remain readable but unavailable for dispatch, and the full catalog package passes. Run `gofmt`, `git diff --check`, self-review, and write the report. Do not commit.

---

## Final Review Fix Wave

The final focused-diff review found one required documentation gap and one narrow replay boundary regression. One secondary implementer addresses them together; no additional feature work is authorized.

**Files:**
- Modify: `docs/api-integration-guide.md:670-672`
- Modify: `docs/runbooks/kaya-coding-plan-operations.md` (add a catalog rollback section)
- Modify: `internal/inference/providers/responses_client.go`
- Test: `internal/inference/providers/responses_client_test.go`
- Modify comment: `internal/inference/quota/service.go:91-102`

- [ ] **Preserve unresolved-call rejection across empty assistant turns**

Add an `middle` case to the existing `TestResponsesReplay_RejectsTextAfterUnresolvedCall` table:

```go
`{"role":"assistant","content":""},
 {"role":"assistant","content":"after"}`,
```

Both `ParseResponsesRequest` and `ResponsesReplayToChat` must reject this sequence as `invalid_input` containing `order`, just like the existing nonempty-text case. Run that test first and record the current false-success RED.

Track whether the latest assistant/tool sequence has an unresolved function call. A function call sets the flag; a user, system or tool-output message, or an assistant message with nonempty text, resets it (the nonempty text after a call is rejected before reset). An empty assistant item remains a separate ChatMessage but does not clear the flag. Do not search beyond this local sequence or change tool-call grouping. The essential shape is:

```go
toolCallPending := false
// In a message item, after existing validation:
if role == "assistant" && text != "" && toolCallPending {
	return nil, invalidMsg("input", existingOrderRejection)
}
out = append(out, model.ChatMessage{Role: role, Content: text})
if role != "assistant" || text != "" {
	toolCallPending = false
}
// After a valid function_call:
toolCallPending = true
// After a valid function_call_output:
toolCallPending = false
```

Reuse the existing rejection text rather than duplicating a new message. Run the replay/rejection tests and full provider package.

- [ ] **Align the client usage documentation**

Replace only the `部分用量` paragraph in `docs/api-integration-guide.md`. State these exact public contracts: Chat omits unknown main/detail counters; Messages always has `input_tokens`/`output_tokens`; Responses always has `input_tokens`/`output_tokens`/`total_tokens` and both detail objects with numeric zero defaults; optional Messages cache counters remain omitted when unknown. Responses uses a usable reported total first, otherwise derives from the public counters after required unknown-to-zero conversion, with overflow/invalid arithmetic falling back to zero. Cache conversion failures remain unknown internally, with zero only at required public fields. Retain verbatim the facts that missing whole usage is not fabricated, unknown DB buckets remain NULL, and money/TPM use only known buckets. Do not claim an absent counter is measured zero.

- [ ] **Document rollback eligibility and the anchor authority**

Add a short `目录回滚（POST /admin/catalog/rollback）` section to the existing operations runbook: rollback creates a new revision after applying current publish validation; known-but-unsupported legacy routes (including `least_loaded`/generic `session_sticky`) stay readable but cannot be reactivated; the 400 names the route and supported replacement; failed rollback creates no revision and leaves active unchanged; operators fix the draft and publish instead of rewriting history.

Add a concise comment on `ActivateWindowsCommand.Anchor` explaining that it is retained for command compatibility while the PostgreSQL store uses the locked persisted `anchor_at` as authority. No production behavior changes for this comment. Optional error-message polishing, generic error-handling changes, additional test scenarios and OpenAPI edits stay out of this wave.

- [ ] **Verify the fix wave and report**

Run `go test -race -count=1 ./internal/inference/providers`, `gofmt` on changed Go files, `go build ./...`, `go vet ./internal/... ./cmd/...`, and `git diff --check`. Read both documentation edits against the actual behavior. Append RED/GREEN evidence, changed files, self-review and concerns to the final-wave report; do not commit or push.

---

## Controller Final Verification

After all task reviews and the focused-diff review, the controller runs the real CI-equivalent checks against a disposable PostgreSQL instance, with `DATABASE_URL` and `E2E_DATABASE_URL` pointing to it:

```bash
go build ./...
go vet ./internal/... ./cmd/...
go run ./cmd/migrate
go run ./cmd/migrate
go test -race -count=1 -p 1 -timeout=30m -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...
go tool cover -func=coverage.out
go test -race -count=1 -p 1 -timeout=30m ./tests/e2e/...
```

Require zero failed tests, no skipped database coverage, migrations `55/0` then `0/55`, and total statement coverage at least 80%. Run the focused quota recovery case repeatedly with explicit nanosecond coverage rather than relying on host clock precision. Stop and remove only the controller-created PostgreSQL instance and generated coverage file, then inspect `git status` before reporting. No push, GitHub review submission, merge, deployment, or real provider claim is implied by this plan.

---

## Verification Result

Implementation was performed by secondary coder agents in three task batches plus one final review-fix batch. The controller reviewed each production diff and dispatched independent task and final reviews; the final scoped re-review approved all fixes.

Verified on the delivered working tree:

- `go build ./...` and `go vet ./internal/... ./cmd/...`: PASS.
- Disposable PostgreSQL migrations: initial `applied=55 skipped=0`, repeated `applied=0 skipped=55`.
- `go test -race -count=1 -p 1 -timeout=30m -coverprofile=coverage.out -json ./internal/... ./cmd/... ./tests/integration/...`: **6,097 tests/subtests passed, 0 failed**, across 34 packages. Three expected skips: the worker-child entry and two opt-in `KAYA_LOAD` drills.
- Statement coverage: **80.9%**, above the 80% gate.
- Separate disposable E2E database: **186 tests/subtests passed, 0 failed**. One expected skip for the opt-in `USAGE_ACCEPTANCE` drill. No database test was skipped because PostgreSQL was unavailable.
- Each of `TestRecover_KeyLeaseExpired_ThenSubsequentAdmissionSucceeds`, `TestAdmitReusesStoredAnchorWithSubMicrosecondCallerTime`, and `TestAdmitReusesFiveHourWindowWithSubMicrosecondConsumptionTime` passed **10/10** focused repetitions.
- Changed Go files are `gofmt`-clean; `git diff --check` passes.
- Both disposable PostgreSQL instances, their data/logs and `coverage.out` were removed. No temporary probe tests or review overlays remain.

No commit, push, GitHub review submission, merge, deployment, schema migration, or dependency change was made. Real provider and real third-party client integration is not claimed. The low-priority Anthropic probe-selection and dry-run deployment-count observations from the initial PR review remain outside this focused repair.

## Integration

After the verification above, the user explicitly chose to push the feature branch and update the existing PR #64. The reviewed changes are committed and pushed only after that authorization; the earlier no-commit/no-push constraint applied to the implementation and local-review phase. No merge, automatic merge, deployment, or new duplicate PR is authorized by this choice.
