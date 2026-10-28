# Usage Query Rate Limit and Cache Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow 10 Usage pages per user without sustained 429s while collapsing identical dashboard queries into one PostgreSQL read per 5-second bucket.

**Architecture:** Keep the existing per-user token bucket in front of the handlers. Add a Usage-only Gin response cache with per-key in-flight deduplication and generation-based invalidation after successful writes; split coverage into its existing-rate independent bucket.

**Tech Stack:** Go 1.25, Gin, `sync`, `time`, existing `middleware.RateLimitWithKey`, existing hand-written Go tests.

**Spec:** `docs/superpowers/specs/2026-10-28-usage-query-rate-limit-cache-design.md`

## Global Constraints

- Work only in the `feat/rate-limit-optimization` worktree.
- Query dashboard/device-read bucket: exactly `20` requests/second and burst `100`, keyed by JWT `user_id`.
- Coverage: independent bucket at exactly `1` request/second and burst `30`; never cached.
- Batch `2/20`, backfill `0.167/2`, export `0.0834/2`, delete `0.1/3`, device write `0.167/5` remain byte-for-byte unchanged.
- Do not modify `internal/middleware/ratelimit.go`, Website, API response shapes, or 429 behavior.
- Use TDD: each behavior test must be observed failing before its implementation.
- Do not add dependencies and do not create git commits unless the user explicitly asks.
- High machine load makes tests slow; wait for actual results and do not label slowness a test failure.

---

### Task 1: Usage response cache with deduplication and invalidation

**Files:**
- Create: `internal/middleware/usage_query_cache.go`
- Create: `internal/middleware/usage_query_cache_test.go`

**Interfaces:**
- Consumes: Gin handler chain and a key function returning JWT `user_id`.
- Produces:
  - `type UsageQueryCache`
  - `func NewUsageQueryCache(ttl time.Duration, now func() time.Time) *UsageQueryCache`
  - `func (q *UsageQueryCache) Handle(userKey func(*gin.Context) string) gin.HandlerFunc`
  - `func (q *UsageQueryCache) InvalidateOnSuccess(userKey func(*gin.Context) string) gin.HandlerFunc`

- [ ] **Step 1: Write failing cache behavior tests**

Use a fake clock and an atomic downstream call counter. Cover these test names and assertions:

```go
func TestUsageQueryCache_CachesOnlySuccessfulGETInSameBucket(t *testing.T)
// Two identical requests: 200 bodies equal, downstream calls == 1.
// Advance fake clock past the 5s bucket: downstream calls == 2.
// A 500 response followed by a 200: downstream calls == 2 and no error body is cached.

func TestUsageQueryCache_CanonicalizesAndIsolatesKeys(t *testing.T)
// ?agent=b&agent=a and ?agent=a&agent=b share one entry.
// Different users, paths, or semantically different params each have independent entries.

func TestUsageQueryCache_ConcurrentMissRunsOneLeader(t *testing.T)
// Block the leader on a channel, start 80 identical requests, release the channel.
// Assert all 80 return the same 200 body and downstream calls == 1.

func TestUsageQueryCache_InvalidateOnSuccessAndInflightRace(t *testing.T)
// Prime a cache entry; a 2xx write makes the next query execute downstream again.
// A 500 write leaves the entry valid.
// Start query A, successfully invalidate while A is blocked, then finish A.
// The next query must execute downstream rather than reuse A's pre-write result.
```

- [ ] **Step 2: Run the tests and confirm RED**

Run: `go test ./internal/middleware -run TestUsageQueryCache -count=1`
Expected: compilation failure because `NewUsageQueryCache` is undefined.

- [ ] **Step 3: Implement the minimal cache**

Use one mutex protecting entries, per-key in-flight calls, and per-user generations. A cache key contains user, `c.FullPath()`, canonical `url.Values`, time bucket, and generation. Store status, body, and content type only for completed 200 responses. The leader runs the downstream chain through a Gin-compatible response recorder; followers wait on the in-flight done channel and replay the leader result. Delete the in-flight entry and close its channel under the same lock. On invalidation, increment the user's generation only when the write status is 2xx. Sweep expired entries lazily; do not start a goroutine.

The response recorder must preserve Gin's writer behavior for these JSON handlers and restore the original writer before returning. Followers must not call `c.Next()`.

- [ ] **Step 4: Run cache tests and race detector**

Run: `go test -race ./internal/middleware -run TestUsageQueryCache -count=1`
Expected: all four tests PASS, including the 80-request deduplication and invalidation race tests.

- [ ] **Step 5: Review checkpoint**

Inspect the diff for response-contract changes, lock-held downstream execution, goroutine creation, or shared mutable response buffers. Fix any occurrence before router wiring.

### Task 2: Wire 20/100 query limiting, independent coverage, caching, and write invalidation

**Files:**
- Modify: `internal/router/router.go:150-179`
- Modify: `internal/router/router_test.go`
- Modify: `CLAUDE.md:136-140`

**Interfaces:**
- Consumes: `middleware.NewUsageQueryCache`, `Handle`, and `InvalidateOnSuccess` from Task 1.
- Produces: Usage routes with limiter → cache → handler order and successful-write invalidation.

- [ ] **Step 1: Write failing router/configuration tests**

Add tests that exercise the actual Usage middleware configuration, not a re-declared test-only limiter. Assert:

```go
func TestSetup_UsageTokenQueryRateLimitAllowsDesignedBurst(t *testing.T)
// A fresh user's 80 simultaneous legal dashboard queries all pass the limiter.
// Exhaust the bucket and assert a later request returns 429 with non-empty Retry-After.

func TestSetup_UsageTokenCoverageHasIndependentBucket(t *testing.T)
// Exhausting dashboard queries does not consume coverage's bucket and vice versa.

func TestSetup_UsageTokenUnchangedWriteLimiters(t *testing.T)
// Assert the configured batch/backfill/export/delete/device-write values remain
// 2/20, 0.167/2, 0.0834/2, 0.1/3, and 0.167/5 respectively.
```

If direct setup makes limiter inspection impractical, extract the existing construction into an unexported `usageTokenLimiters` helper whose fields are the actual handlers mounted by `Setup`; tests must call that same helper. Do not test duplicated constants as a substitute for behavior.

- [ ] **Step 2: Run router tests and confirm RED**

Run: `go test ./internal/router -run 'TestSetup_UsageToken(QueryRateLimit|Coverage|Unchanged)' -count=1`
Expected: the 80-request/coverage-independence tests fail against the old shared `1/30` bucket.

- [ ] **Step 3: Wire the cache and limiters**

In `Setup`, construct one `UsageQueryCache` with `5*time.Second` and a nil clock. Keep this exact limiter order and values:

```go
tokenBatchLimiter := middleware.RateLimitWithKey(ctx, 2, 20, userKey)
tokenBackfillLimiter := middleware.RateLimitWithKey(ctx, 0.167, 2, userKey)
tokenQueryLimiter := middleware.RateLimitWithKey(ctx, 20, 100, userKey)
tokenCoverageLimiter := middleware.RateLimitWithKey(ctx, 1, 30, userKey)
tokenExportLimiter := middleware.RateLimitWithKey(ctx, 0.0834, 2, userKey)
tokenDeleteLimiter := middleware.RateLimitWithKey(ctx, 0.1, 3, userKey)
tokenDeviceWriteLimiter := middleware.RateLimitWithKey(ctx, 0.167, 5, userKey)
```

Mount `queryCache.Handle(userKey)` after `tokenQueryLimiter` on summary, breakdown, timeseries, and GET devices. Mount coverage with only `tokenCoverageLimiter`. Mount `queryCache.InvalidateOnSuccess(userKey)` after the existing limiter on batch, backfill, DELETE tokens, and all device writes.

- [ ] **Step 4: Update the route documentation**

Change the Usage query description in `CLAUDE.md` to 20 QPS/burst 100 with 5-second response caching and deduplication; document coverage's independent 1 QPS/burst 30 bucket and keep every unchanged limiter value accurate.

- [ ] **Step 5: Run focused tests**

Run: `go test -race ./internal/middleware ./internal/router -count=1`
Expected: PASS with no data races.

### Task 3: End-to-end acceptance and regression verification

**Files:**
- Modify if an acceptance test belongs in the existing suite: `tests/e2e/usage_tokens_test.go` or the existing Usage E2E file, following current harness conventions.
- Review: all files changed by Tasks 1–2.

**Interfaces:**
- Consumes: fully wired Usage routes.
- Produces: evidence for the approved load and regression criteria.

- [ ] **Step 1: Add a failing two-minute acceptance test in the existing E2E style**

Use one real test user and the real Usage routes. Simulate 10 pages; each page issues the same 8 legal queries every 5 seconds for at least 2 minutes. Record status codes and downstream query count where the harness supports it. Also run a fresh-user synchronized 80-request burst and an over-limit drain test. Keep the long test behind the repository's existing load/E2E gating convention so normal unit tests do not take two minutes.

- [ ] **Step 2: Run the focused E2E and upload regression tests**

Run the repository's targeted Usage E2E command with the existing database gate. Expected: no 429/5xx for the 10-page drill or 80 burst; over-limit traffic eventually returns 429 with a reasonable positive `Retry-After`; batch/backfill behavior is unchanged. If PostgreSQL or credentials are unavailable, report the exact blocked command and do not claim this acceptance item passed.

- [ ] **Step 3: Run the standard quality gates**

Run, allowing extra time under high load:

```bash
make test
make lint
make build
```

Expected: all PASS. Do not replace failing standard checks with narrower tests.

- [ ] **Step 4: Report evidence and residual limits**

Report test commands/results, changed files, DB query deduplication evidence, and whether CPU/PostgreSQL observations were available. State explicitly that the cache is process-local and cross-instance freshness is bounded by the 5-second TTL. Do not commit or push without explicit user instruction.
