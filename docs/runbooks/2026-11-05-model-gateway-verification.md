# Users Model Gateway — Verification Runbook (2026-11-05)

Companion runbook for the **Users model gateway completion** plan
(`.superpowers/sdd/2026-11-05-users-model-gateway-completion/`).

This file is the single place where every verification command, its exit code,
and its skip/fail inventory is recorded. Each task T1–T10 appends its own
evidence to the matching section. **A skipped database test is not a pass.**

Scope: Users model gateway only. No adjacent repositories, no product sales,
no payments/refunds, no deployment, no merge, no push.

---

## 1. Baseline environment

| Item | Value |
|---|---|
| Worktree | `/Users/jack-tian/Downloads/github/yunhou-users-model-gateway` (isolated) |
| Branch | `fix/model-gateway-completion` |
| T0 base SHA | `35cf836274e870d5e0cd9604c264a57f28ca8f8b` |
| Plan merge base | `299fc49` |
| Go | `go1.27.1 darwin/arm64` |
| PostgreSQL | 16.15 (Homebrew `/opt/homebrew/opt/postgresql@16`), disposable instance |
| OS | macOS (Apple Silicon) |

### 1.1 Module cache / network

`go mod download` fails against `proxy.golang.org` (i/o timeouts) in this
environment. The **existing module cache is sufficient**: every command below
runs with the proxy disabled and the module files locked read-only.

```bash
export GOPROXY=off
export GOFLAGS=-mod=readonly
```

`-mod=readonly` is the Go default for modules, but it is set explicitly here
so the intent is unambiguous. It **fails closed**: if a command would need to
add or change a requirement, the build errors out instead of rewriting
`go.mod` / `go.sum`. Do **not** use `-mod=mod` here — that mode is allowed to
mutate the dependency files, which contradicts the rule below.

`go build ./...` completes with exit code 0 under `GOPROXY=off
-mod=readonly`, which proves all dependencies are already cached. `go.mod` /
`go.sum` are **never** modified — verified after every command with
`git diff --stat go.mod go.sum` → empty.

If a task genuinely needs a new external dependency, the cache cannot satisfy
it and the command fails with a "module lookup disabled by GOPROXY=off" error.
That is the **blocker to report** — never widen the verification to hide it.

### 1.2 Disposable PostgreSQL recipe

The baseline **must never** use the default local business database. Several
`TestMain` functions fall back to `postgres://postgres@localhost/yunhou_users`
when `DATABASE_URL` is unset, and their fixtures `TRUNCATE` shared tables — so
an unset DSN silently points at (and wipes) whatever is on 5432.

Create one instance per verification session, in a fresh temporary directory,
on a port confirmed free **before** the instance exists. The recipe is a
single script so that a **`trap` can guarantee teardown**: if any step fails —
`initdb`, `pg_ctl start`, `createdb`, or a test — the instance is stopped and
the temporary directories are removed, instead of leaving a live trust-auth
PostgreSQL and an orphaned PGDATA behind.

```bash
#!/usr/bin/env bash
set -euo pipefail
PGBIN=/opt/homebrew/opt/postgresql@16/bin
export GOPROXY=off
export GOFLAGS=-mod=readonly

# 1. pick a port and PROVE it is free BEFORE the instance exists.
#    (the baseline run used 55432; any free high port is equivalent)
PGPORT=55432
while lsof -nP -iTCP:"$PGPORT" -sTCP:LISTEN >/dev/null 2>&1 \
   || nc -z 127.0.0.1 "$PGPORT" >/dev/null 2>&1; do
  PGPORT=$((PGPORT + 1))
done
echo "[setup] using free port $PGPORT"

# 2. fresh temp dirs; initdb refuses a non-empty PGDATA, so keep logs outside it
RUN_DIR=$(mktemp -d /tmp/t0-pgrun-XXXXXXXX)
SOCK_DIR=$(mktemp -d /tmp/t0-pgsock-XXXXXXXX)
PGDATA="$RUN_DIR/pgdata"; mkdir -p "$PGDATA"
chmod 700 "$RUN_DIR" "$SOCK_DIR" "$PGDATA"

# 3. THE TRAP — registered BEFORE initdb, so a failure at ANY later step
#    still tears the instance down. It only ever touches $PGDATA, $RUN_DIR
#    and $SOCK_DIR, all three created by this script via mktemp -d.
#    It never runs `pkill postgres` and never touches another instance.
t0_cleanup() {
  rc=$?
  trap - EXIT INT TERM
  echo "[cleanup] trap fired (exit code $rc)"
  if [ -n "${PGDATA:-}" ] && [ -d "$PGDATA" ]; then
    echo "[cleanup] stopping ONLY this instance: pg_ctl -D $PGDATA -m fast stop"
    "$PGBIN/pg_ctl" -D "$PGDATA" -m fast stop \
      && echo "[cleanup] pg_ctl stop ok" \
      || echo "[cleanup] pg_ctl stop non-zero (server may never have started)"
  fi
  if [ -n "${RUN_DIR:-}" ]; then rm -rf "$RUN_DIR" && echo "[cleanup] removed $RUN_DIR"; fi
  if [ -n "${SOCK_DIR:-}" ]; then rm -rf "$SOCK_DIR" && echo "[cleanup] removed $SOCK_DIR"; fi
  exit $rc
}
trap t0_cleanup EXIT INT TERM

# 4. initdb
"$PGBIN/initdb" -D "$PGDATA" -U t0 --auth=trust --encoding=UTF8 --locale=C \
  >"$RUN_DIR/initdb.log" 2>&1

# 5. start: loopback only, unix socket inside our own temp dir
"$PGBIN/pg_ctl" -D "$PGDATA" -l "$RUN_DIR/server.log" -w -t 60 \
  -o "-p $PGPORT -k $SOCK_DIR -c listen_addresses=127.0.0.1 -c unix_socket_directories=$SOCK_DIR" \
  start

# 6. two disposable databases, so the app-side and e2e-side suites cannot
#    contaminate each other
"$PGBIN/createdb" -h 127.0.0.1 -p "$PGPORT" -U t0 t0_app
"$PGBIN/createdb" -h 127.0.0.1 -p "$PGPORT" -U t0 t0_e2e

# 7. export the DSNs at runtime — the DSN string is NEVER committed
export PGHOST=127.0.0.1 PGPORT="$PGPORT" PGUSER=t0
export DATABASE_URL="postgres://${PGUSER}@${PGHOST}:${PGPORT}/t0_app?sslmode=disable"
export E2E_DATABASE_URL="postgres://${PGUSER}@${PGHOST}:${PGPORT}/t0_e2e?sslmode=disable"

# 8. run the verification commands in THIS shell (§2), then let the trap
#    tear the instance down on exit.
```

The teardown is **not** a separate manual step: it is the trap above, and it
runs on normal exit, on `INT`, and on `TERM`. If you deliberately need the
instance to outlive one script (for example to drive an IDE or a manual
`psql` session), disarm the trap explicitly and restore it by hand — this is
the *only* sanctioned way to keep it alive, and the manual equivalent is:

```bash
"$PGBIN/pg_ctl" -D "$PGDATA" -m fast stop   # stops ONLY this PGDATA
rm -rf "$RUN_DIR" "$SOCK_DIR"               # removes ONLY this script's temp dirs
```

Never use `pkill postgres` and never point `pg_ctl -D` at another instance's
data directory.

Both DSNs are always exported **explicitly** for every test command. The
snippet above is the only DSN material in this repository: it is a runtime
template built from `PGHOST` / `PGPORT` / `PGUSER`, it points at a throwaway
instance living in a temp directory, and no DSN of any real or persistent
system is committed.

If `DATABASE_URL` is empty the `TestMain` helpers in
`internal/inference/{catalog,httpapi,postgres,gateway}` silently fall back to
`postgres://postgres@localhost/yunhou_users`; when that connect fails they set
`testDSN = ""` and every DB-backed test calls `t.Skip(...)`. `tests/e2e` has
the same shape — `tests/e2e/testhelpers.go` reads `E2E_DATABASE_URL` and
falls back to `postgres://postgres@localhost/yunhou_users`. That silent skip
is the main hazard this runbook exists to prevent.

### 1.3 Merge gates enforced by CI

Source of truth: `.github/workflows/pr-ci.yml` (read it before changing this
section). A change is only mergeable when **both** jobs pass.

**Job `go-ci`** — one `postgres:16-alpine` service, `DATABASE_URL` set to it:

| # | Gate | Command |
|---|---|---|
| 1 | lint (vet) | `go vet ./internal/... ./cmd/...` |
| 2 | build | `go build ./...` |
| 3 | apply migrations | `go run ./cmd/migrate` |
| 4 | migrate idempotency smoke | `go run ./cmd/migrate` (2nd run: all skipped, exit 0) |
| 5 | unit + integration tests | `go test -race -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` |
| 6 | **coverage floor ≥ 80%** | see the snippet below |
| 7 | upload coverage | uploads `coverage.out` as an artefact |

**Job `go-e2e`** — a *separate* Postgres service, with **both**
`DATABASE_URL` and `E2E_DATABASE_URL` pointed at it:

| # | Gate | Command |
|---|---|---|
| 1 | apply migrations | `go run ./cmd/migrate` |
| 2 | e2e tests | `go test -race -count=1 ./tests/e2e/...` |

Coverage floor, verbatim from the workflow:

```bash
set -euo pipefail
total=$(go tool cover -func=coverage.out | awk '/^total:/ {gsub("%","",$3); print $3}')
echo "total coverage: ${total}%"
awk -v t="$total" 'BEGIN { exit !(t+0 >= 80) }' || {
  echo "::error::coverage ${total}% is below the 80% floor"
  exit 1
}
```

Notes that matter when reproducing a gate locally:

- **`-p 1` is mandatory** in gate 5: `internal/repo`, `internal/service`,
  `internal/migrate` and `tests/integration` share one Postgres and `TRUNCATE`
  each other's tables, so parallel packages corrupt each other.
- **Gate 5 includes `./tests/integration/...`.** The Makefile target
  `make ci-test` does **not** — it runs `./internal/... ./cmd/...` only, so
  `make ci-test` alone under-covers relative to CI. Use the workflow command,
  not `make ci-test`, when you need the real gate.
- **The `go-e2e` job must not share a database with `go-ci`.** Each e2e setup
  wipes and re-seeds the whole database, so the job gets its own Postgres
  service. Keep `t0_app` and `t0_e2e` separate for the same reason.
- `make ci-local` (`ci-test` + `ci-e2e`) is the local pre-push approximation
  of both jobs — but it inherits `ci-test`'s missing `./tests/integration/...`.
- The CI runner uses Go **1.25**; the local toolchain is **1.27.1**. The
  `go.mod` `go` directive is the authority — do not bump it as a side effect.

---

## 2. T0 — isolated test environment and baseline

Recorded on 2026-11-06 at base SHA `35cf836`.

This section records **three runs of the same code at the same base SHA**:

| Run | When | Procedure | Purpose |
|---|---|---|---|
| **R1 — original** | 2026-11-06 00:29–00:41 | manual stop (no trap), `GOFLAGS=-mod=mod` | the original baseline |
| **R2 — trap re-validation** | 2026-11-06 00:47–00:50 | trap-protected recipe, `-mod=readonly` | confirms the corrected procedure produces the same results |
| **R3 — CI gates** | 2026-11-06 00:51– | trap-protected recipe, `-mod=readonly` | runs the real `pr-ci.yml` merge gates (§2.8, §2.9) |

R2 and R3 are authoritative for procedure; R1 is retained because it produced
the original per-package timings and the first verbose skip inventory. **All
three agree on every result.** `go.mod` / `go.sum` were unchanged by every
run (`git diff --stat go.mod go.sum` → empty).

### 2.1 Workspace baseline

| Command | Exit | Result |
|---|---|---|
| `pwd` | 0 | `/Users/jack-tian/Downloads/github/yunhou-users-model-gateway` |
| `git branch --show-current` | 0 | `fix/model-gateway-completion` |
| `git status --short` | 0 | *(empty — clean worktree)* |
| `git rev-parse HEAD` | 0 | `35cf836274e870d5e0cd9604c264a57f28ca8f8b` |
| `go build ./...` (`GOPROXY=off -mod=readonly`) | 0 | module cache sufficient |
| `git diff --stat go.mod go.sum` | 0 | *(empty — dependency files untouched)* |

### 2.2 Pure-unit baseline

```bash
go test -count=1 ./internal/inference/access ./internal/inference/accounting \
  ./internal/inference/quota ./internal/inference/management \
  ./internal/inference/domain ./internal/inference/providers \
  ./internal/inference/providers/connector
```

**Exit code: 0.** All 7 packages `ok`.

| Package | Result |
|---|---|
| `internal/inference/access` | ok |
| `internal/inference/accounting` | ok |
| `internal/inference/quota` | ok |
| `internal/inference/management` | ok |
| `internal/inference/domain` | ok |
| `internal/inference/providers` | ok |
| `internal/inference/providers/connector` | ok |

Verbose re-run for the skip inventory: **448 PASS, 0 SKIP, 0 FAIL**, exit 0.
These seven packages contain no `TestMain` and no `sqlx.Connect` /
`sql.Open` / `migrate.Apply` call — they are genuinely DB-free.

### 2.3 Migration idempotency

Run against the disposable `t0_app` database (`DATABASE_URL` explicit).
49 migration files present in `migrations/`.

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go run ./cmd/migrate` | **0** | `[migrate] applied=49 skipped=0` |
| 2 | `go run ./cmd/migrate` | **0** | `[migrate] applied=0 skipped=49` (49 × `skip … already applied`) |
| 3 | `go run ./cmd/migrate -status` | **0** | 49 × `✅` |

The e2e-side disposable database (`t0_e2e`, via `E2E_DATABASE_URL`) was migrated
the same way: run 1 `applied=49 skipped=0` (exit 0), run 2 `applied=0 skipped=49`
(exit 0). `_migrations` ledger holds **49** rows in each database; `t0_app`
holds **56** tables in `public`.

### 2.4 Serial inference test baseline

```bash
DATABASE_URL=<disposable> go test -race -count=1 -p 1 ./internal/inference/...
```

**Exit code: 0.** All 14 packages `ok` under `-race`:

`access`, `accounting`, `catalog`, `credentials`, `domain`, `gateway`,
`httpapi`, `management`, `postgres`, `providers`, `providers/connector`,
`quota`, `routing`, `workers`.

Verbose re-run for the skip inventory: **1088 PASS, 0 FAIL, 1 SKIP**, exit 0.

### 2.5 Existing gateway integration tests

```bash
DATABASE_URL=<disposable> go test -race -count=1 -v -run 'TestProtocols_' ./tests/integration/...
```

**Exit code: 0.** 5 top-level tests / 13 subtests `PASS`, 0 skips:

| Test | Result |
|---|---|
| `TestProtocols_MultiTurnToolCallIDPreserved` | PASS (3 subtests) |
| `TestProtocols_BrokenStreamSequence` | PASS (3 subtests) |
| `TestProtocols_LedgerConsistencyCrossProtocol` | PASS |
| `TestProtocols_ExplicitRejectionMatrix` | PASS (5 subtests) |
| `TestProtocols_SessionBindingAttributionAndIsolation` | PASS |

Full package (all files in `tests/integration/...`):

```bash
DATABASE_URL=<disposable> go test -race -count=1 -v ./tests/integration/...
```

**Exit code: 0** — 52 PASS, 0 FAIL, 2 SKIP.

`tests/integration` cannot skip on a missing database: `setupDB` calls
`dbURL()` (which reads `DATABASE_URL`) and `t.Fatalf`s on connect failure —
the file header states this explicitly ("skip 不算通过").

### 2.6 Skip inventory (T0)

Every skip observed, with the reason it is **not** a lost database check:

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; skips unless re-executed with `KAYA_QUOTA_WORKER=1` (`internal/inference/postgres/quota_concurrency_test.go:773`). The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and **PASSED** with real contention output (`proc A {Admitted:1 QuotaExceeded:7 Other:0}, proc B {Admitted:2 QuotaExceeded:6 Other:0}`), so the two-process scenario really executed. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1` (`tests/integration/kaya_load_drill_test.go:35`). Load drill, not a correctness test. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |

**Database-test skips: 0.** No inference or integration test skipped for
"no postgres available".

Re-measured through the trap-protected path in §2.10 — same three skips,
same zero database-test skips.

### 2.7 Proof that the disposable DB was actually exercised

Post-run row counts in the disposable `t0_app` database — **identical in R1
and R2**:

```
inference_requests=2  inference_ledger_entries=1  inference_api_keys=1
inference_billing_accounts=1  users=1  plans=3
```

The `_migrations` ledger held **49** rows in both `t0_app` and `t0_e2e`.

And the host had exactly **one** PostgreSQL listener during each run — the
disposable instance that run created:

| Run | Listener |
|---|---|
| R1 | `postgres 66667 jack-tian 5u IPv4 ... TCP 127.0.0.1:55432 (LISTEN)` |
| R2 | `postgres 81907 jack-tian 5u IPv4 ... TCP 127.0.0.1:55432 (LISTEN)` |

`lsof -nP -iTCP -sTCP:LISTEN | grep -i postgres` returned that single line
only. Databases in the disposable instance: `postgres`, `t0_app`, `t0_e2e`,
`template0`, `template1` — no business database was reachable or touched.

### 2.8 The `go-ci` test gate, run verbatim

The CI `go-ci` job's test step is:

```bash
go test -race -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...
```

Note that it **includes `./tests/integration/...`**. The Makefile target
`make ci-test` runs `./internal/... ./cmd/...` only, so `make ci-test` is
**not** the CI gate — it under-covers by one package tree. This section
therefore reproduces the **workflow** command, not `make ci-test`.

T0 ran it with `-count=1` added so the run really executes instead of
serving a cached result (CI runners have a cold cache):

```bash
DATABASE_URL=<disposable> go test -race -count=1 -p 1 \
  -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...
```

**Exit code: 0** — **33 packages `ok`, 0 `FAIL`**, including `tests/integration`:

`internal/analytics`, `internal/billing/{paddle,paypal,wechat}`,
`internal/config`, `internal/handler`, all 14 `internal/inference/*`
packages, `internal/llm`, `internal/middleware`, `internal/migrate`,
`internal/model`, `internal/relay`, `internal/repo`, `internal/router`,
`internal/service`, `internal/util`, `cmd/admin-bootstrap`,
`cmd/inference-entitlement-backfill`, `cmd/server` and **`tests/integration`**
(`cmd/migrate` has no test files).

The coverage floor, run verbatim from `pr-ci.yml`:

```
total coverage: 80.4%
```

80.4% ≥ the 80% floor → **exit code 0**. The workflow's comment records an
83.7% baseline measured on 2026-08-17; the local figure is lower because this
profile also covers `./tests/integration/...`. The margin above the floor is
thin — a task that adds uncovered code can drop below 80% and fail CI, so
re-check the floor after adding packages.

**The whole repository baseline is green against the disposable PostgreSQL**
(and green against the real CI gate, coverage included) — a later task that
turns any of these red has introduced the regression itself.

Runtime note: the sweep takes ~5 min, dominated by `internal/inference/httpapi`
(52 s), `internal/service` (17 s), `internal/relay` (18 s) and
`internal/inference/postgres` (11 s).

### 2.9 Which CI gates T0 actually ran

Honest accounting, so no later task mistakes "documented" for "executed":

| CI gate | Ran in T0? | Evidence |
|---|---|---|
| `go vet ./internal/... ./cmd/...` | **Yes** | §2.10 |
| `go build ./...` | **Yes** | §2.1 |
| `go run ./cmd/migrate` (apply) | **Yes** | §2.3 |
| `go run ./cmd/migrate` (idempotency smoke) | **Yes** | §2.3 |
| `go test -race -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **Yes** | §2.8 |
| coverage floor ≥ 80% | **Yes** | §2.8 |
| `go test -race -count=1 ./tests/e2e/...` (the separate `go-e2e` job) | **Yes** | §2.10 |
| `make ci-local` / `make ci-test` / `make ci-e2e` | **No** | the Makefile targets are approximations of the workflow and `ci-test` omits `./tests/integration/...`; the workflow commands were run directly instead |
| coverage artefact upload (`upload-artifact`) | **No** | CI-only step; the local `coverage.out` was deleted after the floor check (§2.10) |
| `go test ./tests/e2e-ui/...` (Playwright) | **No** | not part of `pr-ci.yml`; needs a browser stack |

### 2.10 T0 re-validation through the trap-protected path

The original T0 run used a manual stop instead of a `trap`, and ran with
`-mod=mod`. The runbook was corrected on both counts (§1.1, §1.2) and the
whole baseline was **re-executed** through the corrected procedure on
2026-11-06. All three runs used their own disposable PostgreSQL instance and
the same base SHA `35cf836`; no existing database was touched at any point.

##### R2 — trap-protected baseline

One script created the instance, ran every T0 command, and was torn down by
the `EXIT` trap. Every step below exited **0**:

| Step | Command | Exit |
|---|---|---|
| migrate run 1 (`t0_app`) | `go run ./cmd/migrate` | 0 |
| migrate run 2 (`t0_app`) | `go run ./cmd/migrate` | 0 |
| migrate status (`t0_app`) | `go run ./cmd/migrate -status` | 0 |
| migrate run 1 (`t0_e2e`) | `DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate` | 0 |
| migrate run 2 (`t0_e2e`) | `DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate` | 0 |
| pure unit | `go test -count=1 ./internal/inference/{access,accounting,quota,management,domain,providers,providers/connector}` | 0 |
| inference serial | `go test -race -count=1 -p 1 ./internal/inference/...` | 0 |
| gateway integration | `go test -race -count=1 -v -run 'TestProtocols_' ./tests/integration/...` | 0 |
| full integration package | `go test -race -count=1 -v ./tests/integration/...` | 0 |

Script exit code **0**. Migration idempotency reproduced exactly:
run 1 `applied=49 skipped=0`, run 2 `applied=0 skipped=49`; ledger 49 rows in
both disposable databases.

##### R3 — CI merge gates

All eight gate steps exited **0** (§2.8, §2.9): vet, build, migrate, migrate
idempotency smoke, the exact `go-ci` test command with coverage, the coverage
floor (`total coverage: 80.4%`), the `go-e2e` job's migrate, and the `go-e2e`
job's test run (`ok github.com/yunhou/users/tests/e2e 159.264s`). The
generated `coverage.out` was deleted by the script, leaving the worktree
clean.

##### Trap cleanup evidence

The trap fired on **every** run and removed only its own artefacts:

```
[cleanup] trap fired (exit code 0)
[cleanup] stopping ONLY this instance: pg_ctl -D /tmp/t0-pgrun-<id>/pgdata -m fast stop
waiting for server to shut down......... done
server stopped
[cleanup] pg_ctl stop ok
[cleanup] removed /tmp/t0-pgrun-<id>
[cleanup] removed /tmp/t0-pgsock-<id>
```

| Run | PGDATA stopped | Temp dirs removed |
|---|---|---|
| R2 | `/tmp/t0-pgrun-Ucoe7Q5r/pgdata` | `/tmp/t0-pgrun-Ucoe7Q5r`, `/tmp/t0-pgsock-LznYMASO` |
| R3 | `/tmp/t0-pgrun-6dOziCOY/pgdata` | `/tmp/t0-pgrun-6dOziCOY`, `/tmp/t0-pgsock-bAGv7BZI` |
| skip inventory | `/tmp/t0-pgrun-rjvFAK5l/pgdata` | `/tmp/t0-pgrun-rjvFAK5l`, `/tmp/t0-pgsock-4PfzEVes` |

After each run `lsof -nP -iTCP -sTCP:LISTEN | grep -i postgres` returned
**nothing** — no stray instance was left behind.

##### Skip inventory through the trap path

A non-verbose `go test` cannot tell "passed" from "skipped", so the skip
inventory was re-measured with `-v` inside the trap-protected session:

| Suite | PASS | SKIP | FAIL |
|---|---|---|---|
| `go test -race -count=1 -p 1 -v ./internal/inference/...` | 1088 | 1 | 0 |
| `go test -race -count=1 -v ./tests/integration/...` | 52 | 2 | 0 |

The three skips are exactly the three in §2.6 — `TestQuotaWorkerProcess`
(child-process entry point, `KAYA_QUOTA_WORKER=1`), `TestLoadDrill_Throughput`
and `TestLoadDrill_LongConnections` (`KAYA_LOAD=1`). Grepping the skip lines
for `postgres` / `no database` / `database unavailable` returned **nothing**:

```
[inventory] ==== any skip mentioning postgres / no database ====
  (NONE — no database-backed test was skipped)
```

**No database test was skipped in any run.** The counts in §2.6 are
reproduced byte-for-byte by the re-validation.

##### `go.mod` / `go.sum` under `-mod=readonly`

Every run used `GOPROXY=off` with `GOFLAGS=-mod=readonly`. After each run:

```
$ git diff --stat go.mod go.sum
(empty)
```

The dependency files were never modified. With `-mod=readonly` a command that
would need to change a requirement now fails instead of silently rewriting
them.

---

## 3. Standing rules for every task

1. **Always** export `DATABASE_URL` and `E2E_DATABASE_URL` explicitly, pointing
   at a disposable instance. Never rely on the default fallback DSN.
2. **Always** pass `-p 1` to `go test` when the suite touches the database:
   `internal/repo`, `internal/service`, `internal/migrate`,
   `internal/inference/*` and `tests/integration` share one Postgres and
   `TRUNCATE` each other's tables.
3. **Always** use `-race` for the inference and integration suites.
4. A **skipped** DB test is not a pass. Record every skip and its reason.
5. `GOPROXY=off` with `GOFLAGS=-mod=readonly` (the Go default). The module
   files must never be modified; `-mod=readonly` fails closed instead of
   rewriting `go.mod` / `go.sum`. **Never** use `-mod=mod`.
6. Create the disposable PostgreSQL with the **trap-protected** recipe in
   §1.2 and let the trap tear it down. Stop only the instance you created
   (`pg_ctl -D <your PGDATA> stop`); never `pkill postgres`.
7. Never commit a DSN, a key, a refresh token, image base64 or a user
   conversation into the repo, the logs, or this runbook.
8. Stage only the files belonging to the current task. Never `git add .`.
   No push, no merge, no deploy.

### 3.1 The merge gates a change must clear

Reproduced from `.github/workflows/pr-ci.yml` — see §1.3 for the full table.
A task is not done until it has run, or explicitly deferred with a reason:

| Gate | Command | Notes |
|---|---|---|
| lint | `go vet ./internal/... ./cmd/...` | also `make lint` |
| build | `go build ./...` | |
| migrate | `go run ./cmd/migrate` | against the disposable instance |
| migrate idempotency | `go run ./cmd/migrate` again | 2nd run must apply 0 and exit 0 |
| unit + integration | `go test -race -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **includes `./tests/integration/...`** — this is the CI command, not `make ci-test` |
| coverage floor | `go tool cover -func=coverage.out` → total ≥ 80% | CI fails the build below the floor; the workflow's stated baseline was 83.7% (measured 2026-08-17) |
| e2e (separate job) | `go test -race -count=1 ./tests/e2e/...` | needs `E2E_DATABASE_URL`; its own Postgres service in CI — never share one with the unit/integration run |

`go vet` and the coverage floor are **merge gates that T0 did not run** — see
§2.9 for exactly what T0 did and did not execute.

---

## 4. Per-task evidence (T1–T10)

Each task appends: the exact command, the exit code, the skip inventory, the
migration result if it added a migration, and the files changed.

### T1 — OAuth hot-path authentication

Recorded on 2026-11-06, base SHA `b24a328` (T1 branch tip: see §4.1).

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the
instance down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`, and
every database-backed command used `-p 1`. `go.mod` / `go.sum` were never
modified (`git diff --stat go.mod go.sum` → empty after every run).

#### 4.1 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1, `t1_app`) | **0** | `applied=49 skipped=0` |
| 4 | `go run ./cmd/migrate` (run 2, `t1_app`) | **0** | `applied=0 skipped=49` (idempotent) |
| 5 | `go run ./cmd/migrate` (`t1_e2e`) | **0** | e2e DB migrated before the e2e job |
| 6 | **RED** `go test -run 'TestGateway_OAuth\|TestGateway_DisabledCredential\|TestGateway_RotatedOAuth\|TestGateway_StaticCredential' -count=1 -v ./internal/inference/gateway` | **1** | the leak reproduced — see §4.2 |
| 7 | **GREEN** same command as #6 | **0** | all T1 gateway cases pass |
| 8 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/credentials ./internal/inference/providers/... ./internal/inference/gateway` | **0** | 4 packages `ok` |
| 9 | `go test -race -count=1 -p 1 -v ./internal/inference/...` | **0** | 14 packages `ok`, 1109 PASS / 1 SKIP / 0 FAIL |
| 10 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | 52 PASS / 2 SKIP / 0 FAIL |
| 11 | `go test -race -count=1 ./tests/e2e/...` (`E2E_DATABASE_URL` → `t1_e2e`) | **0** | `ok github.com/yunhou/users/tests/e2e 159.213s` |
| 12 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 13 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.5%` ≥ the 80% floor (T0 measured 80.4%) |

#### 4.2 RED evidence (before any production edit)

Command #6 ran against the **unmodified** production code at `b24a328`; the
new tests were written against the *existing* API surface only
(`credentials.MarshalBundle` + `credentials.Service.Create(..., "oauth", …)`),
so the failure is a behavioural assertion failure, never a compile error.

```
=== RUN   TestGateway_OAuthUsesAccessTokenOnly
=== RUN   TestGateway_OAuthUsesAccessTokenOnly/non-stream
    gateway_test.go:1737: outbound header "Authorization" leaked the refresh token
    gateway_test.go:1737: Authorization is not the bare access token (bytes=135 json=true)
=== RUN   TestGateway_OAuthUsesAccessTokenOnly/stream
    gateway_test.go:1758: outbound header "Authorization" leaked the refresh token
    gateway_test.go:1758: Authorization is not the bare access token (bytes=135 json=true)
--- FAIL: TestGateway_OAuthUsesAccessTokenOnly (0.31s)
    --- FAIL: TestGateway_OAuthUsesAccessTokenOnly/non-stream (0.20s)
    --- FAIL: TestGateway_OAuthUsesAccessTokenOnly/stream (0.11s)
=== RUN   TestGateway_OAuthAnthropicUsesAccessTokenOnly
    gateway_test.go:1805: outbound header "X-Api-Key" leaked the refresh token
    gateway_test.go:1814: anthropic oauth must use the bearer method (bytes=0 json=false)
    gateway_test.go:1818: an oauth access token must not occupy the api-key slot (bytes=148)
--- FAIL: TestGateway_OAuthAnthropicUsesAccessTokenOnly (0.12s)
=== RUN   TestGateway_OAuthUnusableCredential_NeverDispatches
    gateway_test.go:1849: an unusable oauth credential must not be dispatched
    gateway_test.go:1849: an unusable oauth credential must not be dispatched
--- FAIL: TestGateway_OAuthUnusableCredential_NeverDispatches (0.25s)
    --- FAIL: TestGateway_OAuthUnusableCredential_NeverDispatches/empty_access_token (0.12s)
    --- FAIL: TestGateway_OAuthUnusableCredential_NeverDispatches/unparsable_bundle (0.13s)
--- FAIL: TestGateway_RotatedOAuthCredential_DispatchesOnlyCurrentToken (0.15s)
    gateway_test.go:1914: Authorization must carry the current access token (bytes=143 json=true)
    gateway_test.go:1926: outbound header "Authorization" leaked the refresh token
    gateway_test.go:1929: outbound header "Authorization" leaked the sealed oauth bundle
--- PASS: TestGateway_DisabledCredential_NeverDispatches (0.11s)
--- PASS: TestGateway_StaticCredentialHeadersUnchanged (0.13s)
FAIL	github.com/yunhou/users/internal/inference/gateway	2.141s
```

**Why this failure is the expected one.** The gateway resolved the upstream
credential with `SecretResolver.ResolveSecret`, which returns the *whole
decrypted plaintext*. For an `auth_type='oauth'` row that plaintext is the
sealed `OAuthBundle` JSON (`{"access_token":…,"refresh_token":…}`), and
`Adapter.AuthHeaders` copied it verbatim into the upstream header — 135 bytes
of JSON on `openai_chat`, 148 bytes on `anthropic_messages` (`x-api-key`).
The refresh token therefore left the credentials boundary on every dispatch.
The two "unusable credential" cases failed for the same root cause: an empty
access token / an unparsable bundle was still dispatched instead of being
refused before the outbound call. The two cases that already passed at
baseline (disabled credential, static `api_key`/`service` headers) are
regression guards, not new behaviour.

The failure messages deliberately print **no** credential material (only the
header name and a byte count) — the test log itself stays free of secrets.

#### 4.3 GREEN evidence

Command #7 (same tests, production code fixed) — exit code **0**:

```
--- PASS: TestGateway_OAuthUsesAccessTokenOnly (0.12s)
    --- PASS: TestGateway_OAuthUsesAccessTokenOnly/non-stream (0.06s)
    --- PASS: TestGateway_OAuthUsesAccessTokenOnly/stream (0.06s)
--- PASS: TestGateway_OAuthAnthropicUsesAccessTokenOnly (0.06s)
--- PASS: TestGateway_OAuthUnusableCredential_NeverDispatches (0.12s)
    --- PASS: TestGateway_OAuthUnusableCredential_NeverDispatches/empty_access_token (0.06s)
    --- PASS: TestGateway_OAuthUnusableCredential_NeverDispatches/unparsable_bundle (0.06s)
--- PASS: TestGateway_DisabledCredential_NeverDispatches (0.06s)
--- PASS: TestGateway_RotatedOAuthCredential_DispatchesOnlyCurrentToken (0.06s)
--- PASS: TestGateway_StaticCredentialHeadersUnchanged (0.13s)
    --- PASS: TestGateway_StaticCredentialHeadersUnchanged/api_key (0.06s)
    --- PASS: TestGateway_StaticCredentialHeadersUnchanged/service (0.09s)
ok  	github.com/yunhou/users/internal/inference/gateway	3.495s
```

Acceptance command #8 — exit code **0**:

```
ok  	github.com/yunhou/users/internal/inference/credentials	1.977s
ok  	github.com/yunhou/users/internal/inference/providers	1.729s
ok  	github.com/yunhou/users/internal/inference/providers/connector	1.665s
ok  	github.com/yunhou/users/internal/inference/gateway	6.727s
```

#### 4.4 Skips

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; needs `KAYA_QUOTA_WORKER=1` (`quota_concurrency_test.go:773`). The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and passes with real contention output. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1`. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |

**Database-test skips: 0** — identical to the T0 inventory (§2.6); grepping the
skip lines for `postgres` / `no database` / `database unavailable` returns
nothing.

#### 4.5 Migration

**None.** T1 adds no migration: the fix lives entirely in the
credentials/gateway/provider boundaries. The disposable databases were
migrated with `cmd/migrate` exactly as in §2.3 (49 files, idempotent on the
second run).

#### 4.6 Files changed

| File | Change |
|---|---|
| `internal/inference/credentials/service.go` | new `OutboundAuth` + `ResolveOutboundAuth` (auth-type-carrying outbound resolver; oauth → access token only) |
| `internal/inference/providers/adapter.go` | new `Auth` type (`Type`+`Token`), `Call.Secret []byte` → `Call.Auth`, `Adapter.AuthHeaders(Auth) (map, error)` |
| `internal/inference/providers/openai_chat.go` | bearer for `api_key`/`service`/`oauth`; unknown auth type refused |
| `internal/inference/providers/openai_responses.go` | same |
| `internal/inference/providers/anthropic.go` | `api_key`/`service` → `x-api-key` (unchanged); `oauth` → `Authorization: Bearer` and **never** the API-key slot; unknown auth type refused |
| `internal/inference/gateway/service.go` | `SecretResolver` now resolves `credentials.OutboundAuth`; the dispatch builds `providers.Auth` from it |
| `internal/inference/gateway/isolation_test.go` | the hand-rolled `failSecretFor` double follows the new resolver signature |
| `internal/inference/gateway/gateway_test.go` | T1 gateway cases (real gateway → httptest, streaming + non-streaming) |
| `internal/inference/credentials/service_test.go` | `ResolveOutboundAuth` unit cases |
| `internal/inference/providers/adapter_dispatch_test.go` | per-protocol auth-header cases |
| `internal/inference/providers/anthropic_payload_test.go` | `Call.Secret` → `Call.Auth` (mechanical) |

`cmd/server/main.go` needed **no** change: the wiring already passes
`*credentials.Service`, which satisfies the new `SecretResolver`.

#### 4.7 Notes (compatibility, boundaries, honesty)

- **The bundle no longer leaves the credentials package.** `ResolveSecret`
  stays for internal code that really needs the raw material (the refresh
  worker); the hot path now calls `ResolveOutboundAuth`, which yields the
  access token alone for `oauth` rows and the untouched plaintext for
  `api_key`/`service` rows.
- **Static credentials are byte-identical to the old hot path**
  (`api_key`/`service` → `Authorization: Bearer <plaintext>` on the
  OpenAI-compatible protocols, `x-api-key` on Anthropic) — pinned by
  `TestGateway_StaticCredentialHeadersUnchanged` and
  `TestAuthHeaders_StaticCredentialsUnchanged`.
- **Anthropic + OAuth — supported set is explicit (see §4.8).** The
  Anthropic adapter keeps the API-key slot (`x-api-key`) for static keys and
  **refuses** `auth_type='oauth'` outright: the protocol's documented support
  set contains no OAuth vendor (the repo's own OAuth connectors speak
  OpenAI-compatible protocols, and Anthropic's official claude.ai OAuth needs
  an `anthropic-beta: oauth-2025-04-20` flag this deployment does not send).
  An OAuth credential aimed at an `anthropic_messages` deployment is therefore
  refused **before** any upstream call — never mis-slotted into `x-api-key`,
  never sent as a bearer that only a 401 would disprove.
- **Unusable credentials fail closed before the outbound call**: an empty
  access token, an unparsable bundle, a revoked credential and a stale
  generation all refuse the candidate (`NoCooldown`, no upstream request). The
  failure messages carry no part of the sealed material — the bundle parse
  cause is dropped on purpose (a JSON syntax error can echo one character of
  the plaintext).
- **Preserved:** credential status (`active`/`rotating`/`revoked`), the
  generation CAS, rotation, the SSRF/egress re-validation and the
  cross-origin redirect refusal are untouched; the dispatch order (attempt
  row → lease → egress → credential → dispatch) is unchanged.
- **No new dependency, no migration, no API-surface change**: no new enum,
  response field or client-visible error — the OpenAPI spec and the
  integration guide need no update for T1.

#### 4.8 T1 fix round — supported-set refusal + single-sourced bundle decode

Recorded on 2026-11-06, after the T1 commit `52b7864`. Two **Important**
findings from the T1 review were fixed with TDD; the fix is one extra commit
(§4.9). Nothing else was touched — deferred minors stayed deferred.

**Finding 1 — `anthropic_messages` accepted `oauth` although its documented
support set contains no OAuth vendor.** `AuthHeaders` returned
`Authorization: Bearer` for `AuthTypeOAuth`, while §4.7 declared that
Anthropic-official OAuth is *not* supported and the repo's OAuth connectors
speak OpenAI-compatible protocols. Code and docs disagreed, and a vendor
mismatch surfaced only as an upstream 401 — which is not the fail-closed
behaviour the explicit supported-set rule asks for.

**Finding 2 — the bundle-decode invariant lived in two divergent places.**
`ResolveBearerToken` (probe surface) and `ResolveOutboundAuth` (hot path)
each unmarshalled the bundle and rejected an empty access token, but the
probe path attached the raw JSON/unmarshal cause (a JSON syntax error can
echo one character of the sealed plaintext) and used a different message than
the hot path.

##### 4.8.1 RED (production code still at `52b7864`)

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go test -count=1 -run 'TestResolveOAuthBundle_SharedDecodePath\|TestResolveStaticCredentialBehaviourPreserved' -v ./internal/inference/credentials` | **1** | the two public paths diverge; the probe path attaches the raw parse cause |
| 2 | `go test -count=1 -run 'TestAuthHeaders' -v ./internal/inference/providers` | **1** | `anthropic_messages` returns headers for an oauth credential instead of refusing |
| 3 | `DATABASE_URL=<disposable> go test -run 'TestGateway_OAuth' -count=1 -v ./internal/inference/gateway` | **1** | the oauth credential **was** dispatched upstream to the anthropic deployment |

```
=== RUN   TestResolveOAuthBundle_SharedDecodePath/corrupt_bundle
    service_test.go:446: the two public paths must reject identically (bearer message="oauth bundle unparsable", outbound message="oauth credential bundle is unreadable")
    service_test.go:452: the failure attached a raw json parse cause
--- FAIL: TestResolveOAuthBundle_SharedDecodePath (0.00s)
    --- FAIL: TestResolveOAuthBundle_SharedDecodePath/corrupt_bundle (0.00s)
=== RUN   TestAuthHeaders_AnthropicRefusesOAuth
    adapter_dispatch_test.go:377: anthropic_messages must refuse oauth credentials (header count=2)
--- FAIL: TestAuthHeaders_AnthropicRefusesOAuth (0.00s)
=== RUN   TestGateway_OAuthAnthropicIsRefusedBeforeDispatch
    gateway_test.go:1792: an oauth credential must not be dispatched to the anthropic protocol
--- FAIL: TestGateway_OAuthAnthropicIsRefusedBeforeDispatch (0.05s)
```

The failure messages print header counts, codes and domain messages only —
never a token, a bundle or a parse cause.

##### 4.8.2 The fix

* `internal/inference/providers/anthropic.go` — `AuthTypeOAuth` is now
  **refused** (`domain.CodeInvalidInput`, "providers: anthropic_messages does
  not support oauth credentials"). `Dispatch` turns that into a
  `DispatchError{NoCooldown: true}`: **nothing is sent**, the account is not
  cooled, and the gateway fails over to the next candidate. No beta header was
  added and no Anthropic-official OAuth support is claimed. Static
  `api_key`/`service` behaviour (`x-api-key` + `anthropic-version`) is
  unchanged.
* `internal/inference/credentials/service.go` — one internal helper
  `decodeOAuthAccessToken(plain []byte) (string, error)` is now the single
  place where a sealed bundle is decoded for an outbound bearer token. Both
  public resolvers call it, so a corrupt bundle and an empty access token are
  rejected **identically** (same code, same message) and the returned error
  never carries the raw JSON/unmarshal cause nor any part of the sealed
  material. The static `api_key`/`service` behaviour of each public method is
  untouched — the probe path still trims (`ResolveBearerToken`), the hot path
  still returns the plaintext byte-identical (`ResolveOutboundAuth`).

##### 4.8.3 GREEN and re-verification

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go test -count=1 -run 'TestResolveOAuthBundle_SharedDecodePath\|TestResolveStaticCredentialBehaviourPreserved\|TestResolveOutboundAuth' -v ./internal/inference/credentials` | **0** | 3 test funcs / 7 cases pass |
| 2 | `go test -count=1 -run 'TestAuthHeaders' -v ./internal/inference/providers` | **0** | 4 test funcs pass |
| 3 | `DATABASE_URL=<disposable> go test -race -count=1 -p 1 -run 'TestGateway_OAuth\|TestGateway_StaticCredential\|TestGateway_DisabledCredential\|TestGateway_RotatedOAuth' -v ./internal/inference/gateway` | **0** | all T1 gateway cases pass |
| 4 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/credentials ./internal/inference/providers/... ./internal/inference/gateway` | **0** | 4 packages `ok` |
| 5 | `go vet ./internal/... ./cmd/...` + `go build ./...` | **0** / **0** | lint + build green |
| 6 | `go run ./cmd/migrate` (x2, idempotency) | **0** / **0** | `applied=49 skipped=0` → `applied=0 skipped=49` |
| 7 | `DATABASE_URL=<disposable> go test -race -count=1 -p 1 -v ./internal/inference/... ./tests/integration/...` | **0** | 1166 PASS / 3 SKIP / 0 FAIL |
| 8 | `E2E_DATABASE_URL=<disposable> go test -race -count=1 ./tests/e2e/...` | **0** | `ok github.com/yunhou/users/tests/e2e 166.253s` |
| 9 | CI `go-ci` gate + coverage floor | **0** | 33 packages `ok`, `total coverage: 80.5%` ≥ 80% |

Acceptance command output:

```
ok  	github.com/yunhou/users/internal/inference/credentials	2.050s
ok  	github.com/yunhou/users/internal/inference/providers	1.721s
ok  	github.com/yunhou/users/internal/inference/providers/connector	1.672s
ok  	github.com/yunhou/users/internal/inference/gateway	5.965s
```

**Skip inventory (fix round)** — unchanged from §4.4 and the T0 baseline:
`TestQuotaWorkerProcess` (`KAYA_QUOTA_WORKER=1` child-process entry point),
`TestLoadDrill_Throughput` and `TestLoadDrill_LongConnections`
(`KAYA_LOAD=1`). **Database-test skips: 0.**

**Files changed in the fix round**

| File | Change |
|---|---|
| `internal/inference/providers/anthropic.go` | refuse `AuthTypeOAuth` before dispatch |
| `internal/inference/credentials/service.go` | one shared `decodeOAuthAccessToken` helper for both public resolvers |
| `internal/inference/providers/adapter_dispatch_test.go` | oauth bearer now asserted for the OpenAI-compatible adapters only + a new anthropic-refusal case |
| `internal/inference/gateway/gateway_test.go` | the anthropic case is now a zero-dispatch / secret-free-error refusal test |
| `internal/inference/credentials/service_test.go` | shared-decode-path identity test + a static-behaviour-preserved test |

**Compatibility boundary (restated for the fix).** `anthropic_messages` +
`auth_type='oauth'` is an explicitly unsupported combination: the request is
refused before the outbound call, the client sees the ordinary
`upstream_unavailable` shape of a failed dispatch, and no credential material
appears in the error, the attempt record or the logs. `api_key`/`service` on
`anthropic_messages` are unchanged (`x-api-key` + `anthropic-version`).
OAuth credentials remain dispatchable on the OpenAI-compatible protocols
(`openai_chat`, `openai_responses`) via `Authorization: Bearer <access token>`.

### T2 — Window-less settlement and conflict classification

Recorded on 2026-11-06, base SHA `fc0674e` (T1 branch tip; T0 base `35cf836`).
Task report: `.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-2-report.md`.

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the instance
down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`; every
database-backed command used `-p 1`; `go.mod` / `go.sum` were never modified
(`git diff --stat go.mod go.sum` → empty after every run).

#### 4.9 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1, `t2_app`) | **0** | `applied=50 skipped=0` (49 baseline + 050) |
| 4 | `go run ./cmd/migrate` (run 2, `t2_app`) | **0** | `applied=0 skipped=50` (idempotent) |
| 5 | `go run ./cmd/migrate -status` | **0** | 50 × `✅` |
| 6 | **RED** `go test -count=1 -run 'TestGateway_NoWindowPolicySettles\|TestGateway_MissingExpectedReservationRequiresReconciliation\|TestGateway_DuplicateSettleWithInconsistentChargeIsAnomaly\|TestGateway_LateUsageAfterReleaseIsAnomaly' -v ./internal/inference/gateway` | **1** | the leak reproduced — see §4.10 |
| 7 | **RED** `go test -count=1 -run 'TestSettleWithoutMonetaryHoldSettlesWindowlessRequest\|TestSettleWithDeletedReservationsDoesNotSettle\|TestConcurrentFinalizeSettlesOnce' -v ./internal/inference/postgres` | **1** | the store-level guard reproduced — see §4.10 |
| 8 | **RED** `go test -count=1 -run 'TestRecover_WindowlessRequest_SettlesAtDurableEstimate\|TestRecover_LegacyWindowlessRequest_ParksForReconciliation' -v ./internal/inference/workers` | **1** | the recovery path reproduced — see §4.10 |
| 9 | **RED (release path)** the same two release cases against the pre-fix production code (§4.10.5) | **1** | the release path had the same "no holds ⇒ conflict" guard |
| 10 | **GREEN** commands #6/#7/#8 re-run after the fix | **0** | all T2 cases pass |
| 11 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/quota ./internal/inference/postgres ./internal/inference/gateway ./internal/inference/workers` | **0** | 4 packages `ok` (1.657s / 16.898s / 9.879s / 7.909s) |
| 12 | `go test -race -count=1 -p 1 -v ./internal/inference/...` | **0** | 14 packages `ok`, 1135 PASS / 1 SKIP / 0 FAIL |
| 13 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | 52 PASS / 2 SKIP / 0 FAIL |
| 14 | `go test -race -count=1 ./tests/e2e/...` (`E2E_DATABASE_URL` → `t2_e2e`) | **0** | `ok github.com/yunhou/users/tests/e2e 178.306s` |
| 15 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 16 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.3%` ≥ the 80% floor (T0 80.4%, T1 80.5%) |

The whole table above was re-run end to end in **one** trap-protected
session on a fresh disposable instance (the CI gate was re-run separately
because the combined script exceeded the 600 s background budget; the gate
itself is the same command with the same disposable-PostgreSQL recipe).

#### 4.10 RED evidence (before any production edit)

Command #6 ran against the **unmodified** production code at `fc0674e`; the
new tests were written against the *existing* API surface only (the gateway's
`ChatCompletions`/`settle`, the store's `Reserve`/`Settle`/`Release`, the
recovery worker's `RunPass`), so every failure is a behavioural assertion
failure, never a compile error.

##### 4.10.1 Gateway — no-window settlement (reported + estimated)

```
=== RUN   TestGateway_NoWindowPolicySettles/nokey_positive_reported
    gateway_test.go:2179: request status = "non_streaming", want settled (无窗口策略也必须终态结算)
    gateway_test.go:2182: usage_status = "pending", want "reported"
    gateway_test.go:2185: ledger charge rows = 0, want exactly 1 (一逻辑请求一次 charge)
    gateway_test.go:2188: charge = -1, want 20
    gateway_test.go:2191: settled_micros = 0, want 20
    gateway_test.go:2195: usage records = 0, want 1 (必须有计量事实)
=== RUN   TestGateway_NoWindowPolicySettles/nokey_positive_estimated
    gateway_test.go:2179: request status = "non_streaming", want settled (无窗口策略也必须终态结算)
    ...
    gateway_test.go:2195: usage records = 0, want 1 (必须有计量事实)
=== RUN   TestGateway_NoWindowPolicySettles/nokey_zero_reported
    gateway_test.go:2179: request status = "non_streaming", want settled (无窗口策略也必须终态结算)
=== RUN   TestGateway_NoWindowPolicySettles/nokey_zero_estimated
    gateway_test.go:2179: request status = "non_streaming", want settled (无窗口策略也必须终态结算)
--- FAIL: TestGateway_NoWindowPolicySettles (0.55s)
    --- FAIL: TestGateway_NoWindowPolicySettles/nokey_positive_reported (0.07s)
    --- FAIL: TestGateway_NoWindowPolicySettles/nokey_positive_estimated (0.06s)
    --- FAIL: TestGateway_NoWindowPolicySettles/nokey_zero_reported (0.06s)
    --- FAIL: TestGateway_NoWindowPolicySettles/nokey_zero_estimated (0.07s)
    --- PASS: TestGateway_NoWindowPolicySettles/key_positive_reported (0.08s)
    --- PASS: TestGateway_NoWindowPolicySettles/key_positive_estimated (0.07s)
    --- PASS: TestGateway_NoWindowPolicySettles/key_zero_reported (0.07s)
    --- PASS: TestGateway_NoWindowPolicySettles/key_zero_estimated (0.07s)
```

The four **with-Key** cases already passed at baseline: a Key budget hold is
a monetary hold, so the request had a `held` reservation row and the store
settled it. They are recorded as **protection evidence**, not as new
behaviour. The four **no-Key** cases are the leak: the request stayed in
`non_streaming` with `usage_status='pending'`, **no usage fact, no ledger
charge, no terminal state** — the request was consumed upstream and nothing
was ever billed or recorded.

##### 4.10.2 Gateway — missing expected reservation / duplicate / released

```
=== RUN   TestGateway_MissingExpectedReservationRequiresReconciliation
    gateway_test.go:2268: request status = "non_streaming", want reconciliation_required (缺预占必须进入待核对)
    gateway_test.go:2278: no reconciliation job: a missing expected reservation must enter the reconciliation queue
--- FAIL: TestGateway_MissingExpectedReservationRequiresReconciliation (0.07s)
=== RUN   TestGateway_DuplicateSettleWithInconsistentChargeIsAnomaly
    gateway_test.go:2318: settlement alarms = 0, want 1 (不一致的重复结算必须告警)
    gateway_test.go:2322: no reconciliation job: an inconsistent duplicate settlement must enter the reconciliation queue
--- FAIL: TestGateway_DuplicateSettleWithInconsistentChargeIsAnomaly (0.08s)
=== RUN   TestGateway_LateUsageAfterReleaseIsAnomaly
    gateway_test.go:2370: settlement alarms = 0, want 1 (released 后的晚到用量必须告警)
    gateway_test.go:2374: no reconciliation job: late usage after release must enter the reconciliation queue
--- FAIL: TestGateway_LateUsageAfterReleaseIsAnomaly (0.07s)
```

All three were swallowed by the same line: the gateway treated **every**
`CodeConflict` as "already settled". A deleted reservation, an inconsistent
duplicate and a late usage after release were all silently dropped — no
alarm, no reconciliation job, no terminal state.

##### 4.10.3 Store level — the exact guard, and the two guards that already held

```
=== RUN   TestSettleWithoutMonetaryHoldSettlesWindowlessRequest
    quota_settlement_test.go:659: settle a window-less request: inference/conflict: settle: no held reservations (already released or settled?)
--- FAIL: TestSettleWithoutMonetaryHoldSettlesWindowlessRequest (0.08s)
=== RUN   TestSettleWithDeletedReservationsDoesNotSettle
--- PASS: TestSettleWithDeletedReservationsDoesNotSettle (0.08s)
=== RUN   TestConcurrentFinalizeSettlesOnce
--- PASS: TestConcurrentFinalizeSettlesOnce (0.09s)
```

`TestSettleWithDeletedReservationsDoesNotSettle` (the settle must not
succeed) and `TestConcurrentFinalizeSettlesOnce` (exactly one charge, the
loser is a conflict) already passed at baseline — protection evidence for the
"never double charge" and "never settle without holds" invariants.

##### 4.10.4 Recovery — the free-request leak

```
=== RUN   TestRecover_WindowlessRequest_SettlesAtDurableEstimate
    settlement_recovery_test.go:1171: stats = {Scanned:1 ReleasedNotSent:0 SettledConservative:0 ... RacingSkipped:1 ...}, want one conservative settle (无窗口请求也必须结算)
    settlement_recovery_test.go:1175: request = dispatching/pending, want settled/estimated
    settlement_recovery_test.go:1178: settled = 0, want 8312 (按可恢复估算上界入账，不得静默免费)
    settlement_recovery_test.go:1185: ledger = 0 charges/0, want 1/8312
--- FAIL: TestRecover_WindowlessRequest_SettlesAtDurableEstimate (0.13s)
=== RUN   TestRecover_LegacyWindowlessRequest_ParksForReconciliation
    settlement_recovery_test.go:1232: request status = "dispatching", want reconciliation_required (保留待核对)
    settlement_recovery_test.go:1243: no reconciliation job: an evidence-less request must stay in the reconciliation queue
--- FAIL: TestRecover_LegacyWindowlessRequest_ParksForReconciliation (0.10s)
```

The recovery worker could not settle a window-less request at all: the store
rejected it ("no held reservations"), the worker counted it as a benign
`racing_skipped`, and the request stayed open forever. Worse, the only
amount the row carried was `reserved_micros = 0`, so the "conservative
estimate" the worker *would* have booked was **zero** — a consumed request
given away for free.

##### 4.10.5 The release path (found during the fix, reproduced on demand)

The same `len(rows) == 0 ⇒ conflict` guard existed in the **release** path:
a window-less request that the upstream refused could never be released, so
it hung as "in flight" and the recovery sweep later settled it. The two
regression cases for it were therefore run against the **pre-fix** production
code as well — the production files were reverted with
`git stash push -- <production files>` while the test files stayed in place
(so the new tests ran against the original code), then restored with
`git stash pop`:

```
=== RUN   TestGateway_NoWindowPolicyRejectedRequestReleases
    gateway_test.go:2406: request status = "dispatching", want released (确认零消费 → 释放并终态化)
--- FAIL: TestGateway_NoWindowPolicyRejectedRequestReleases (0.12s)
FAIL	github.com/yunhou/users/internal/inference/gateway	0.840s

=== RUN   TestReleaseWithoutMonetaryHoldFinalizesWindowlessRequest
    quota_settlement_test.go:878: release a window-less request: inference/conflict: release: no held reservations (already released or settled?)
--- FAIL: TestReleaseWithoutMonetaryHoldFinalizesWindowlessRequest (0.09s)
FAIL	github.com/yunhou/users/internal/inference/postgres	0.778s
```

After `git stash pop` the same two cases pass (the fixed tree is verified
again below).

#### 4.11 GREEN evidence

Commands #6/#7/#8 re-run after the implementation — exit code **0**:

```
--- PASS: TestGateway_NoWindowPolicySettles (0.46s)
    --- PASS: TestGateway_NoWindowPolicySettles/nokey_positive_reported (0.08s)
    --- PASS: TestGateway_NoWindowPolicySettles/nokey_positive_estimated (0.05s)
    --- PASS: TestGateway_NoWindowPolicySettles/nokey_zero_reported (0.05s)
    --- PASS: TestGateway_NoWindowPolicySettles/nokey_zero_estimated (0.05s)
    --- PASS: TestGateway_NoWindowPolicySettles/key_positive_reported (0.06s)
    --- PASS: TestGateway_NoWindowPolicySettles/key_positive_estimated (0.06s)
    --- PASS: TestGateway_NoWindowPolicySettles/key_zero_reported (0.06s)
    --- PASS: TestGateway_NoWindowPolicySettles/key_zero_estimated (0.06s)
--- PASS: TestGateway_MissingExpectedReservationRequiresReconciliation (0.06s)
--- PASS: TestGateway_DuplicateSettleWithInconsistentChargeIsAnomaly (0.06s)
--- PASS: TestGateway_LateUsageAfterReleaseIsAnomaly (0.06s)
--- PASS: TestGateway_NoWindowPolicyRejectedRequestReleases (0.12s)
--- PASS: TestGateway_ConsistentDuplicateSettleIsIdempotent (0.06s)
ok  	github.com/yunhou/users/internal/inference/gateway	1.297s
```

```
--- PASS: TestSettleWithoutMonetaryHoldSettlesWindowlessRequest (0.06s)
--- PASS: TestSettleWithDeletedReservationsDoesNotSettle (0.05s)
--- PASS: TestConcurrentFinalizeSettlesOnce (0.05s)
--- PASS: TestReleaseWithoutMonetaryHoldFinalizesWindowlessRequest (0.11s)
ok  	github.com/yunhou/users/internal/inference/postgres	0.804s
```

```
--- PASS: TestRecover_WindowlessRequest_SettlesAtDurableEstimate (0.06s)
--- PASS: TestRecover_LegacyWindowlessRequest_ParksForReconciliation (0.05s)
--- PASS: TestRecover_MissingReservationIsAnomaly (0.12s)
ok  	github.com/yunhou/users/internal/inference/workers	0.771s
```

Acceptance command #11 — exit code **0** (final trap-protected run):

```
ok  	github.com/yunhou/users/internal/inference/quota	1.657s
ok  	github.com/yunhou/users/internal/inference/postgres	16.898s
ok  	github.com/yunhou/users/internal/inference/gateway	9.879s
ok  	github.com/yunhou/users/internal/inference/workers	7.909s
```

#### 4.12 Skips

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; needs `KAYA_QUOTA_WORKER=1` (`quota_concurrency_test.go:773`). The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and passes with real contention output. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1`. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |

**Database-test skips: 0** — identical to the T0 inventory (§2.6) and to T1
(§4.4); grepping the skip lines for `postgres` / `no database` / `database
unavailable` returns nothing.

#### 4.13 Migration

**One appended migration: `migrations/050_inference_request_expected_hold.sql`**
(50 = the re-checked latest number; 049 was the baseline, nothing was
rewritten and no historical ledger row was touched).

1. `inference_requests.expected_hold_micros BIGINT` — the admission-priced
   monetary upper bound (the **recoverable estimate upper bound**). It
   equals the mirrored hold amount for a request that has monetary windows;
   for a **window-less** policy it is the *only* place the amount can live
   (there is no hold row at all). `NULL` = a pre-migration row: the recovery
   keeps it pending reconciliation — no free settlement, no guessed
   back-charge, no recomputation of history.
2. `inference_reconciliation_jobs.reason` — four new dedicated reasons:
   `missing_reservation`, `late_usage_after_release`, `settlement_mismatch`,
   `insufficient_evidence`.

Evidence: `go run ./cmd/migrate` (run 1) → `applied=50 skipped=0`;
(run 2) → `applied=0 skipped=50`; `go run ./cmd/migrate -status` → 50 × `✅`.
The e2e-side disposable database was migrated the same way (`applied=50
skipped=0`).

#### 4.14 Files changed

| File | Change |
|---|---|
| `migrations/050_inference_request_expected_hold.sql` | **new** — the durable estimate upper bound column + the dedicated reconciliation reasons |
| `internal/inference/domain/settlement_outcome.go` | **new** — `SettleOutcomeKind` + the typed `SettleError` (duplicate / released / mismatch / missing_reservation) |
| `internal/inference/domain/usage.go` | `Request.ExpectedHoldMicros`; `ExpectedMonetaryTargets`, `HoldUpperBound`, `RecoverableEstimate` |
| `internal/inference/domain/errors.go` | `CodeOf` recognises the typed settlement result |
| `internal/inference/postgres/ledger_repo.go` | `Settle` classifies an already-terminal request; the window-less request settles without holds; a missing expected reservation is an anomaly; `ReportTerminalAnomaly` (terminal state preserved, job filed) |
| `internal/inference/postgres/reservation_repo.go` | `Release` finalizes a window-less request (no holds ⇒ nothing to release) and refuses loudly when the expected holds are missing |
| `internal/inference/postgres/request_repo.go` | persists / reads `expected_hold_micros` |
| `internal/inference/postgres/reconciliation_repo.go` | the overage baseline is the request's monetary upper bound (not `reserved_micros=0`) |
| `internal/inference/quota/service.go` | the admission persists the priced upper bound on the request row |
| `internal/inference/accounting/settlement.go` | the conservative-estimate basis is the recoverable estimate |
| `internal/inference/gateway/service.go` | the settle outcome is classified (duplicate ⇒ silent; released/mismatch/missing ⇒ alarm + dedicated reconciliation); `parkSettleFailure` carries a reason |
| `internal/inference/workers/settlement_recovery.go` | the recovery settles on the recoverable estimate, parks evidence-less rows, classifies the typed outcomes and files the dedicated jobs |
| `internal/inference/gateway/gateway_test.go` | T2 gateway cases (no-window matrix, missing reservation, inconsistent duplicate, late usage after release, window-less release) |
| `internal/inference/postgres/quota_settlement_test.go` | T2 store cases (window-less settle, deleted reservations, concurrent finalize, window-less release) |
| `internal/inference/workers/settlement_recovery_test.go` | T2 recovery cases (durable estimate, evidence-less row) |

#### 4.16 T2 fix round — terminal-outcome classification, typed release outcome, reason persistence

Recorded on 2026-11-06, base SHA `b8439fe` (the T2 commit). Three review
findings were fixed with TDD; the fix round adds **one** appended migration
(`051`) and touches the domain, the store, the gateway, the recovery worker
and the reconciliation vocabulary.

**Environment.** Same recipe as §4.9: a fresh trap-cleaned disposable
PostgreSQL per run, explicit `DATABASE_URL` / `E2E_DATABASE_URL`, `-p 1`,
`-race`, `GOPROXY=off` + `GOFLAGS=-mod=readonly`, `go.mod` / `go.sum`
untouched.

##### 4.16.1 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | **RED** `go test -count=1 -run 'TestSettle_SettledWithoutChargeRow_ClassifiesLedgerDrift\|TestSettle_SettledWithDifferentCharge_ClassifiesChargeMismatch\|TestSettle_MoreHeldThanExpected_ClassifiesReservationDrift\|TestRelease_MissingReservation_ReturnsTypedOutcome\|TestReconciliationJob_ManualOnlyReasonTakesOver' -v ./internal/inference/postgres` | **1** | the three terminal classes were indistinguishable, the release path was untyped, the reason was not taken over — see §4.16.2 |
| 2 | **RED** `go test -count=1 -run 'TestRecover_TerminalOutcomeClassification\|TestRecover_MissingReservationRelease_ParksWithoutReplay\|TestRecover_ManualOnlyJobIsNotDriven\|TestRecover_InsufficientEvidence_DoesNotReDriveEveryPass' -v ./internal/inference/workers` | **1** | the drift classes were swallowed as a benign race; the release anomaly was invisible; the manual-only job was driven — see §4.16.3 |
| 3 | `go run ./cmd/migrate` (run 1) | **0** | `applied=51 skipped=0` (50 + 051) |
| 4 | `go run ./cmd/migrate` (run 2) | **0** | `applied=0 skipped=51` (idempotent) |
| 5 | **GREEN** the same command as #1 | **0** | 5/5 pass |
| 6 | **GREEN** the same command as #2 | **0** | 4/4 pass (10 subtests) |
| 7 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/quota ./internal/inference/postgres ./internal/inference/gateway ./internal/inference/workers` | **0** | 4 packages `ok` (1.553s / 14.057s / 9.674s / 8.401s) |
| 8 | `go test -race -count=1 -p 1 -v ./internal/inference/...` | **0** | 14 packages `ok`, 1154 PASS / 1 SKIP / 0 FAIL |
| 9 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | 52 PASS / 2 SKIP / 0 FAIL |
| 10 | `go test -race -count=1 ./tests/e2e/...` | **0** | `ok github.com/yunhou/users/tests/e2e 210.998s` |
| 11 | `go vet ./internal/... ./cmd/...` + `go build ./...` | **0** / **0** | lint + build green |
| 12 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` + coverage floor | **0** | 33 packages `ok`, 0 `FAIL`, `total coverage: 80.4%` ≥ the 80% floor (T2 80.3%) |

##### 4.16.2 RED — the store classification (command #1)

```
=== RUN   TestSettle_SettledWithoutChargeRow_ClassifiesLedgerDrift
    quota_settlement_test.go:997: outcome = "mismatch", want ledger_drift (账本无 charge 行 ≠ 金额不一致)
--- FAIL: TestSettle_SettledWithoutChargeRow_ClassifiesLedgerDrift (0.10s)
=== RUN   TestSettle_SettledWithDifferentCharge_ClassifiesChargeMismatch
    quota_settlement_test.go:1015: outcome = "mismatch", want charge_mismatch (金额不一致的重复结算)
--- FAIL: TestSettle_SettledWithDifferentCharge_ClassifiesChargeMismatch (0.11s)
=== RUN   TestSettle_MoreHeldThanExpected_ClassifiesReservationDrift
    quota_settlement_test.go:1069: outcome = "mismatch", want reservation_drift (held 数多于准入创建)
--- FAIL: TestSettle_MoreHeldThanExpected_ClassifiesReservationDrift (0.11s)
=== RUN   TestRelease_MissingReservation_ReturnsTypedOutcome
    quota_settlement_test.go:1105: err = inference/conflict: release: the expected monetary reservation is missing (accounting anomaly), want a typed settlement outcome (缺预占必须可识别)
--- FAIL: TestRelease_MissingReservation_ReturnsTypedOutcome (0.12s)
=== RUN   TestReconciliationJob_ManualOnlyReasonTakesOver
    recovery_test.go:1087: job reason = "unknown_usage", want missing_reservation (人工原因必须接管)
--- FAIL: TestReconciliationJob_ManualOnlyReasonTakesOver (0.10s)
```

All three "already terminal" cases collapsed into one `mismatch` kind, the
release path returned an **untyped** conflict, and an open job kept its
evidence-driven reason.

##### 4.16.3 RED — the recovery classification (command #2)

```
=== RUN   TestRecover_TerminalOutcomeClassification/ledger_drift
    settlement_recovery_outcomes_test.go:157: stats = {… RacingSkipped:1 … Errors:0}, want an alarm (账务异常必须响亮)
    settlement_recovery_outcomes_test.go:160: stats = {… RacingSkipped:1 …}, want no benign skip (账务异常不得当竞态)
    settlement_recovery_outcomes_test.go:164: reconciliation reason = [], want [ledger_drift]
--- FAIL: TestRecover_TerminalOutcomeClassification/ledger_drift (0.00s)
=== RUN   TestRecover_TerminalOutcomeClassification/reservation_drift
    … want an alarm (账务异常必须响亮) …
--- FAIL: TestRecover_TerminalOutcomeClassification/reservation_drift (0.00s)
    --- PASS: …/duplicate, …/charge_mismatch, …/missing_reservation, …/released
--- FAIL: TestRecover_TerminalOutcomeClassification (0.00s)
=== RUN   TestRecover_MissingReservationRelease_ParksWithoutReplay
    settlement_recovery_outcomes_test.go:188: stats = {… RacingSkipped:1 …}, want an alarm (缺预占必须响亮)
    settlement_recovery_outcomes_test.go:201: request status = "reserved", want reconciliation_required (停放进专门核对)
    settlement_recovery_outcomes_test.go:205: no reconciliation job: 缺预占必须进入专门核对
--- FAIL: TestRecover_MissingReservationRelease_ParksWithoutReplay (0.07s)
=== RUN   TestRecover_ManualOnlyJobIsNotDriven/insufficient_evidence
    settlement_recovery_outcomes_test.go:271: stats = {… SettledConservative:1 …}, want no automatic settlement (人工原因不得被驱动)
    settlement_recovery_outcomes_test.go:278: request status = "settled", want reconciliation_required (保持停放)
    settlement_recovery_outcomes_test.go:281: settled = 50000, want 0 (人工原因不得自动入账)
--- FAIL: TestRecover_ManualOnlyJobIsNotDriven/insufficient_evidence (0.08s)
    --- PASS: …/missing_reservation, …/late_usage_after_release, …/settlement_mismatch
--- FAIL: TestRecover_ManualOnlyJobIsNotDriven (0.31s)
=== RUN   TestRecover_InsufficientEvidence_DoesNotReDriveEveryPass
    settlement_recovery_outcomes_test.go:347: pass 2 stats = {… ParkedNoEvidence:1 …}, want no re-drive (已停放的任务不得每轮重放)
    settlement_recovery_outcomes_test.go:347: pass 3 stats = {… ParkedNoEvidence:1 …}, want no re-drive
--- FAIL: TestRecover_InsufficientEvidence_DoesNotReDriveEveryPass (0.09s)
```

The four subtests that already passed (`duplicate`, `charge_mismatch`,
`missing_reservation`, `released`) are **protection evidence** for the
behaviour that must not change.

##### 4.16.4 GREEN

Command #5 (exit 0): 5/5 store cases pass. Command #6 (exit 0): 4/4
recovery cases pass, all 10 subtests green — including the two drift classes
that now alarm and file their dedicated job, the release-path anomaly that
now parks the request without a terminal success, and the manual-only job
that is no longer driven.

##### 4.16.5 Migration

**One appended migration: `migrations/051_inference_reconciliation_reasons.sql`**
(51 = the re-checked latest number; 050 was the T2 baseline). It only widens
the `inference_reconciliation_jobs.reason` CHECK with the two new dedicated
classes `ledger_drift` and `reservation_drift` — the reason vocabulary is
enforced by a DB CHECK, and Finding 1 requires the two genuine drift classes
to be persisted and queryable **distinctly** (the previous single `mismatch`
reason is what let the recovery treat them as a benign race). No historical
migration was rewritten and no historical ledger row was touched; the
pre-existing reasons keep their meaning.

##### 4.16.6 What the fix round changed

| Area | Change |
|---|---|
| `internal/inference/domain/settlement_outcome.go` | the single `mismatch` kind is split into `charge_mismatch` (an inconsistent duplicate — the gateway alarms, the recovery's estimate yields), `ledger_drift` (settled but no charge row) and `reservation_drift` (held count ≠ admission) |
| `internal/inference/postgres/ledger_repo.go` | the settled classification returns the three distinct kinds; the reservation-count check returns `missing_reservation` (fewer) or `reservation_drift` (more) |
| `internal/inference/postgres/reservation_repo.go` | the **release** path returns the same typed outcomes (`missing_reservation` / `reservation_drift`) instead of an untyped conflict |
| `internal/inference/postgres/reconciliation_reasons.go` | **new** — the single reason vocabulary and the `ManualOnlyReconciliationReasons` set (the evidence-driven vs manual-only split), shared by the upsert and the recovery skip list |
| `internal/inference/postgres/reconciliation_repo.go` | the upsert takes over an open job's reason when the incoming reason is manual-only (and never downgrades a manual-only reason); the detail merge keeps the prior audit content and a `reason_history` array records the replaced reason |
| `internal/inference/gateway/service.go` | the classification maps each kind to its own reconciliation reason (`settlement_mismatch`, `ledger_drift`, `reservation_drift`, `missing_reservation`, `late_usage_after_release`); the inconsistent-duplicate anomaly behaviour is unchanged |
| `internal/inference/workers/settlement_recovery.go` | only `duplicate` and `charge_mismatch` are benign skips; every other kind alarms and is parked or filed; the skip list is the shared manual-only vocabulary (so `insufficient_evidence` is skipped too); `parkForManual` no longer returns early without correcting the reason |
| `migrations/051_inference_reconciliation_reasons.sql` | **new** — the two new reasons |

**Compatibility.** No client-visible contract changes (no new enum, field or
error shape on the wire); the OpenAPI spec and the integration guide need no
update. The store's transaction boundaries, the fixed lock order, the
one-charge-per-request uniqueness, the legacy no-backfill behaviour and the
T1 credential behaviour are untouched. The only behavioural change is that
the two genuine drift classes and the release-path anomaly are now **loud**
(alarm + dedicated reconciliation job) instead of being swallowed as a
benign race.

##### 4.16.7 Skips

The skip inventory is unchanged: `TestQuotaWorkerProcess`
(`KAYA_QUOTA_WORKER=1`), `TestLoadDrill_Throughput` and
`TestLoadDrill_LongConnections` (`KAYA_LOAD=1`). **Database-test skips: 0.**

#### 4.15 Notes (root cause, compatibility, honesty)

- **Root cause (three defects, one symptom).** (1) `postgres.Store.Settle`
  used `len(rows) == 0` — "no held reservation" — as a proxy for "already
  settled or released". A policy with no monetary window and no Key budget
  has **no hold row at all**, so its settlement was rejected as a conflict.
  (2) `gateway.Service.settle` treated **every** `CodeConflict` as "already
  settled — idempotent delivery, not an error", so the rejection was
  swallowed: no usage fact, no ledger charge, no terminal state, no alarm.
  (3) The admission priced a monetary upper bound but persisted it only
  *through* the hold rows; with no hold row the amount was lost, so the
  crash-recovery estimate degenerated to `reserved_micros = 0` — a consumed
  request given away for free.
- **The fix keeps the transaction boundary.** `Store.Settle(ctx, uow, cmd)`
  is still one transaction (usage fact + ledger charge + reserved→used
  conversion + terminal state); the classification happens inside the same
  locked transaction, before anything is written. No transaction is held
  across an upstream call; the settlement context is still detached and
  deadline-bounded; finalize still precedes the lease release; the panic
  cleanup in `StreamBody.Finish` is untouched.
- **One charge per logical request.** The ledger's partial unique index is
  unchanged; a second delivery never appends a second charge. What changed
  is *what the second delivery means*: a consistent duplicate is absorbed
  silently, anything else is an alarm.
- **No fake windows, no forced zero charges.** A window-less policy keeps
  zero windows (`quota windows = 0` is asserted); its requests still carry
  usage, a ledger row and a terminal state. The zero-price model still gets
  a zero charge **with** a ledger row (migration 031 semantics).
- **Unknown vs estimated vs reported are preserved.** The estimated path is
  unchanged (`accounting.Decide` still prices the estimate and records
  `source='estimated'`); the recovery path still records
  `source='estimated'` with an auditable raw basis. Nothing was collapsed
  into zero.
- **Money is still fixed-point.** No float was introduced; the new column is
  a `BIGINT` micro amount and every amount still comes from the existing
  rational pricing (`domain.Rate`, `accounting.PriceVersion`).
- **Compatibility.** No client-visible contract changed: no new enum, no new
  response field, no new error shape on the wire. The OpenAPI spec and the
  integration guide need **no** update for T2. The new reconciliation
  reasons and the new column are internal audit facts.
- **Behaviour change for operators:** a request whose expected monetary
  reservation is missing is now parked in reconciliation instead of being
  silently dropped, and a released request that receives a late nonzero
  usage now raises an alarm and a dedicated job. The window's
  `reserved_micros` of a request with deleted holds stays frozen until a
  human resolves it — that is the point of the dedicated flow, not a silent
  leak.
- **The recovery's job reasons are not re-driven.** An anomaly job
  (`missing_reservation` / `late_usage_after_release` /
  `settlement_mismatch`) waits for a human instead of retrying a settlement
  that can never succeed (which would re-alarm every pass).
- **The recovery yields to the live settlement.** When the recovery's
  conservative estimate meets a request the live path already settled with a
  *different* amount, the estimate yields (nothing is booked, no alarm):
  the recorded charge is authoritative and the estimate is explicitly
  correctable. The opposite order — the estimate booked first, the real
  usage arriving later — is the **gateway's** anomaly and is filed there
  (`settlement_mismatch`), because only the live path can see that the
  booked amount is no longer the real one.

### T3 — Provider disable, model state and sticky consistency

Recorded on 2026-11-06, base SHA `4439f92` (the T2 branch tip; T0 base
`35cf836`). Task report:
`.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-3-report.md`.

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the
instance down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`;
every database-backed command used `-p 1`; `go.mod` / `go.sum` were never
modified (`git diff --stat go.mod go.sum` → empty after every run).

#### 4.17 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1, `t3_app`) | **0** | `applied=51 skipped=0` (T2 baseline 51; T3 adds no migration) |
| 4 | `go run ./cmd/migrate` (run 2, `t3_app`) | **0** | `applied=0 skipped=51` (idempotent) |
| 5 | `go run ./cmd/migrate -status` | **0** | 51 × `✅` |
| 6 | `DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate` | **0** | e2e-side DB migrated before the e2e job |
| 7 | **RED** the 14 new T3 cases against the pristine `4439f92` production code | **1** | the leak reproduced — see §4.18 |
| 8 | **GREEN** the same 14 cases after the fix | **0** | all T3 cases pass |
| 9 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/catalog ./internal/inference/routing ./internal/inference/gateway ./internal/inference/httpapi` | **0** | 4 packages `ok` (5.707s / 1.875s / 9.031s / 62.150s) |
| 10 | `go test -race -count=1 -p 1 -v ./internal/inference/...` | **0** | 14 packages `ok`, 1168 PASS / 1 SKIP / 0 FAIL |
| 11 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | 52 PASS / 2 SKIP / 0 FAIL |
| 12 | `go test -race -count=1 ./tests/e2e/...` (`E2E_DATABASE_URL` → `t3_e2e`) | **0** | `ok github.com/yunhou/users/tests/e2e 170.105s` |
| 13 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 14 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.4%` ≥ the 80% floor (T0 80.4%, T1 80.5%, T2 80.3%) |

#### 4.18 RED evidence (before any production edit)

The 14 new cases were run against the **pristine `4439f92` production tree**
(a pristine copy of the base commit with only the T3 *test* files copied
over it — the same "tests in, production out" shape the T2 fix round used
via `git stash push -- <production files>`). Every failure is a behavioural
assertion failure, never a compile error.

```
--- FAIL: TestCandidates_DisabledProviderExcluded (0.00s)
    service_test.go:476: candidates = [prov-off/acct-off,prov-on/acct-on], want only acct-on (停用的 provider 不得出站)
--- FAIL: TestCandidates_DisabledProviderOnlyMeansNoCandidates (0.00s)
    service_test.go:517: candidates = [prov-off/acct-off], want 0 (只有 disabled provider 的模型不可调度)
--- FAIL: TestCandidates_ProviderAbsentFromSnapshotIsNotSchedulable (0.00s)
    service_test.go:542: candidates = [prov-gone/acct-gone], want 0 (provider 不在已发布快照里 → 不可调度)
--- FAIL: TestSnapshot_DisabledProviderRemovesDeploymentAndSellability (0.00s)
    revision_test.go:275: ActiveDeployments = [dep-off prov-off …, dep-on prov-on …], want only dep-on（停用 provider 的部署不算可用部署）
--- FAIL: TestParseSnapshot_RejectsDeploymentWithUnknownProvider (0.00s)
    revision_test.go:300: a deployment referencing an unknown provider must fail the whole parse
--- FAIL: TestProviderDisableTakesEffectOnlyOnPublish (0.06s)
    service_test.go:662: a model served only by a disabled provider must not be sellable
    service_test.go:665: ActiveDeployments = [dep … prov …], want 0（停用 provider 的部署不再是可用部署）
--- FAIL: TestGateway_DisabledProviderNeverDispatches (0.07s)
    sticky_candidates_test.go:233: disabled provider received 1 upstream calls, want 0
    sticky_candidates_test.go:236: active provider received 0 upstream calls, want 1
--- FAIL: TestGateway_StickyCandidates_DisabledProviderNotDispatched (0.07s)
    sticky_candidates_test.go:260: a sticky session bound to a disabled provider must not be served
--- FAIL: TestGateway_StickyCandidates_NewSessionBindsActiveProvider (0.07s)
    sticky_candidates_test.go:288: disabled provider received 1 upstream calls, want 0
    sticky_candidates_test.go:291: active provider received 0 upstream calls, want 1
    sticky_candidates_test.go:298: session bound to <disabled-provider account>, want the active provider's account <other>
--- FAIL: TestGateway_AliasToDisabledProviderNotDispatched (0.07s)
    sticky_candidates_test.go:323: disabled provider received 1 upstream calls via alias, want 0
    sticky_candidates_test.go:326: active provider received 0 upstream calls, want 1
--- FAIL: TestGateway_DisabledProviderKeepsInFlightRequestAlive (0.11s)
    gateway_test.go:2559: code = , want upstream_unavailable (停用后的新请求不得出站): <nil>
--- FAIL: TestAdminCatalogFlow_ProviderDisableRequiresRepublish (0.05s)
    catalog_test.go:503: a model served only by a disabled provider must not be sellable
--- FAIL: TestKayaModels_DisabledProviderHidesModel (0.12s)
    kaya_models_test.go:435: picker = [map[default:true … id:deepseek-chat … provider:deepseek]], want no model（provider 停用后模型没有可用部署）
```

**Root cause (one defect, four surfaces).** The published snapshot carried
the provider's `status`, but **nothing ever read it**: `routing.Candidates`
filtered on `route.Enabled` + `deployment.Status` only, `Snapshot.ActiveDeployments`
(the model-list projection) did the same, and the sticky branch simply
narrowed the candidate set the normal path had built. So a provider that
the operator disabled (`PATCH /admin/providers/:id` → `status=disabled` →
publish) kept receiving traffic through **every** entry point — normal
candidates, aliases, sticky sessions and the published model list.

**Protection evidence (already-correct behaviour, recorded instead of a
forced change).** Two T3 cases passed at the pre-fix baseline and are
guards, not new behaviour:

| Case | Guard that already held |
|---|---|
| `TestLifecycleGate_SnapshotCacheErrorIsNotPermissionDenial` | the gateway pins the snapshot **before** the lifecycle gate (`gateway/service.go` step 1) and maps a snapshot failure to `CodeInternal` — never `CodeModelNotAllowed` / `CodeNotFound`; zero upstream calls, zero request rows, zero ledger rows |
| `TestLifecycleGate_DefaultModeIsObserve` | `Service.LifecycleGate` still defaults to `LifecycleGateObserve` (zero value); the enforce mode stays opt-in — the T3 change did not touch the lifecycle defaults |

The lifecycle contract itself was already fully pinned by the pre-existing
cases (`TestLifecycleGateDecision`, `…_Observe_RetiredAllowedAndReported`,
`…_Observe_DraftAndDeprecatedAllowed`, `…_Enforce_RetiredBlocked`,
`…_Enforce_DraftBlocked`, `…_Enforce_DeprecatedAllowed`,
`…_DefaultReporterFallback`, `…_Enforce_ActiveUnaffected`) — all of them
still pass.

#### 4.19 GREEN evidence

Command #8 (the same 14 cases, production code fixed) — exit code **0**:

```
--- PASS: TestCandidates_DisabledProviderExcluded (0.00s)
--- PASS: TestCandidates_DisabledProviderOnlyMeansNoCandidates (0.00s)
--- PASS: TestCandidates_ProviderAbsentFromSnapshotIsNotSchedulable (0.00s)
--- PASS: TestSnapshot_DisabledProviderRemovesDeploymentAndSellability (0.00s)
--- PASS: TestParseSnapshot_RejectsDeploymentWithUnknownProvider (0.00s)
--- PASS: TestProviderDisableTakesEffectOnlyOnPublish (0.06s)
--- PASS: TestGateway_DisabledProviderNeverDispatches (0.07s)
--- PASS: TestGateway_StickyCandidates_DisabledProviderNotDispatched (0.07s)
--- PASS: TestGateway_StickyCandidates_NewSessionBindsActiveProvider (0.07s)
--- PASS: TestGateway_AliasToDisabledProviderNotDispatched (0.09s)
--- PASS: TestGateway_DisabledProviderKeepsInFlightRequestAlive (0.08s)
--- PASS: TestLifecycleGate_SnapshotCacheErrorIsNotPermissionDenial (0.06s)
--- PASS: TestLifecycleGate_DefaultModeIsObserve (0.06s)
--- PASS: TestAdminCatalogFlow_ProviderDisableRequiresRepublish (0.06s)
--- PASS: TestKayaModels_DisabledProviderHidesModel (0.19s)
```

Acceptance command #9 — exit code **0**:

```
ok  	github.com/yunhou/users/internal/inference/catalog	5.707s
ok  	github.com/yunhou/users/internal/inference/routing	1.875s
ok  	github.com/yunhou/users/internal/inference/gateway	9.031s
ok  	github.com/yunhou/users/internal/inference/httpapi	62.150s
```

CI `go-ci` gate (command #13) — exit code **0**, 33 packages `ok`, 0 `FAIL`,
including `tests/integration`; coverage floor `total coverage: 80.4%` ≥ 80%.

#### 4.20 Skips

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; needs `KAYA_QUOTA_WORKER=1` (`quota_concurrency_test.go:773`). The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and passes with real contention output. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1`. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |

**Database-test skips: 0** — identical to the T0 (§2.6), T1 (§4.4) and T2
(§4.12) inventories; grepping the skip lines for `postgres` / `no database`
/ `database unavailable` returns nothing.

#### 4.21 Migration

**None.** T3 adds no migration: the fix lives entirely in the published
snapshot's projection (`catalog/revision.go`) and in the candidate selection
(`routing/service.go`). The disposable databases were migrated with
`cmd/migrate` exactly as in §2.3 (51 files, idempotent on the second run).

#### 4.22 Files changed

| File | Change |
|---|---|
| `internal/inference/catalog/revision.go` | the shared published-state decision: `Snapshot.ProviderSchedulable` (a provider absent from the snapshot, or whose published status is not `active`, is out of rotation) and `Snapshot.DeploymentSchedulable` (deployment active **and** provider schedulable); `ActiveDeployments` uses it, so the model-list projection and the dispatch path agree; `ParseSnapshot` now rejects a deployment that references a provider missing from the revision |
| `internal/inference/routing/service.go` | `Candidates` filters on `snap.DeploymentSchedulable(d)` instead of `d.Status != domain.DeploymentActive` — the one shared rule for the normal **and** the sticky candidate set |
| `internal/inference/gateway/service.go` | comments only: the sticky branch narrows the candidate set the normal path built and never re-derives schedulability (no back door) |
| `internal/inference/routing/service_test.go` | T3 cases: `TestCandidates_DisabledProviderExcluded`, `…_DisabledProviderOnlyMeansNoCandidates`, `…_ProviderAbsentFromSnapshotIsNotSchedulable`; the shared `testSnapshot` helper now registers each deployment's provider as **active** (a published snapshot always carries it) and the fake store counts account-pool queries per provider |
| `internal/inference/catalog/revision_test.go` | T3 cases: `TestSnapshot_DisabledProviderRemovesDeploymentAndSellability`, `TestParseSnapshot_RejectsDeploymentWithUnknownProvider` |
| `internal/inference/catalog/service_test.go` | T3 case: `TestProviderDisableTakesEffectOnlyOnPublish` (draft disable ≠ published change) |
| `internal/inference/gateway/sticky_candidates_test.go` | T3 cases: `TestGateway_DisabledProviderNeverDispatches`, `…_StickyCandidates_DisabledProviderNotDispatched`, `…_StickyCandidates_NewSessionBindsActiveProvider`, `…_AliasToDisabledProviderNotDispatched` + the `addSecondProvider` helper |
| `internal/inference/gateway/gateway_test.go` | the `withSnapshotSource` fixture option and `TestGateway_DisabledProviderKeepsInFlightRequestAlive` (in-flight request keeps its pinned snapshot) |
| `internal/inference/gateway/lifecycle_gate_test.go` | T3 cases: `TestLifecycleGate_SnapshotCacheErrorIsNotPermissionDenial`, `TestLifecycleGate_DefaultModeIsObserve` + the `failingSnapshot` stub |
| `internal/inference/httpapi/kaya_models_test.go` | T3 case: `TestKayaModels_DisabledProviderHidesModel` |
| `internal/inference/httpapi/catalog_test.go` | T3 case: `TestAdminCatalogFlow_ProviderDisableRequiresRepublish` + the `publishedCatalogSvc` helper |
| `internal/inference/httpapi/protocol_surfaces_test.go` | **fixture repair** (see §4.23): the hand-built snapshot loaded its provider with a raw `SELECT *` into the untagged `domain.Provider`, which silently failed — the provider was never in the snapshot. It now uses the store's typed row mapper. |

#### 4.23 Notes (root cause, compatibility, honesty)

- **Root cause: the published provider status was carried but never read.**
  `routing.Candidates` and `Snapshot.ActiveDeployments` both filtered on the
  route and the deployment only. One shared decision
  (`Snapshot.DeploymentSchedulable`) now covers **route + provider +
  deployment**, and both consumers call it — the normal candidate path and
  the sticky path (which only narrows the candidate set the normal path
  built, so it inherits the decision by construction).
- **A disabled provider takes its deployments with it.** An *enabled* route
  to a disabled provider's deployment is not a dispatchable route: the
  provider is out of rotation, so every deployment behind it is out too.
  The other active provider of the same model stays usable — pinned by
  `TestGateway_DisabledProviderNeverDispatches` and
  `…_StickyCandidates_NewSessionBindsActiveProvider`.
- **Model list ⇔ dispatch now agree.** `Sellable` and `ActiveDeployments`
  use the same rule, so a model whose only provider is disabled is not
  presented as an available deployment (`/v1/models`, `/chat/models`,
  `/chat` facade). The list reads the **active revision** directly
  (`catalog.Service.LoadSnapshot`) while the dispatch path reads the
  **1s-TTL snapshot cache** — the list is therefore never staler than the
  dispatch path, and both apply the same rule. The catalogue snapshot
  cache convention (1s head probe, refresh-failure keeps the last verified
  snapshot) is **unchanged**.
- **In-flight requests keep their admission snapshot.** The provider
  disable is a catalogue-state change, not a live revocation: a request
  already in flight continues on its pinned snapshot and is **not**
  force-stopped; only new requests (after the cache refreshes per the
  existing convention) see the disable. Pinned by
  `TestGateway_DisabledProviderKeepsInFlightRequestAlive`.
- **Sticky sessions do not silently switch accounts.** A binding whose
  account belongs to a disabled provider fails the call with
  `upstream_unavailable` — the sticky contract still forbids a silent
  account switch, and a disabled provider is not a transient capacity gap.
- **Draft edits never touch the running snapshot.** Disabling a provider is
  a draft edit (`PATCH /admin/providers/:id`); the published revision — and
  therefore every pinned snapshot — changes only on `publish`. Pinned by
  `TestProviderDisableTakesEffectOnlyOnPublish` and
  `TestAdminCatalogFlow_ProviderDisableRequiresRepublish`.
- **Lifecycle defaults unchanged.** `observe` stays the default,
  `enforce` stays opt-in, `deprecated` still passes existing traffic, and
  a cache failure is still an internal failure — never a permission
  denial. The lifecycle gate itself was **not** modified.
- **No new dependency, no migration, no client-visible contract change.**
  No new enum, response field or error shape: the disable surfaces through
  the **existing** `upstream_unavailable` (HTTP 503) error, which the
  OpenAPI spec already documents
  (`docs/api/kaya-coding-plan.openapi.yaml:917`/`:1006`). The OpenAPI
  spec and the integration guide therefore need **no** update for T3.
- **One fixture repair, not a behaviour change.** The httpapi
  protocol-surfaces fixture built a hand-made snapshot whose provider was
  loaded with a raw `SELECT *` into the untagged `domain.Provider` — sqlx
  cannot map `display_name`/`access_type` into it, so the read silently
  failed and the snapshot never carried a provider. That was invisible
  while nothing consulted the provider; with the shared decision the
  fixture now loads it through the store's typed row mapper
  (`store.GetProvider`). No production behaviour depends on it.
- **Preserved:** the 1s account-pool cache and the 1s entitlement cache
  (a disabled provider is rejected **before** the account pool is even
  consulted — the routing test asserts zero account-pool round trips for
  it), the T1 credential behaviour, the T2 settlement/recovery behaviour,
  and the settlement transaction boundaries (untouched).

#### 4.24 T3 fix round — the two 503 messages must not misdescribe a
#### provider-disable rejection

Recorded on 2026-11-06, after the T3 commit `d9a5ad3`. One **Important**
review finding was fixed with TDD; the fix round is one extra commit
(§4.25). Nothing else was touched — the dry-run/probe finding stays with
T9 (explicitly owned there), and no unrelated/minor change was made.

**The finding (verbatim).** "Both 503 paths misdescribe a provider-disable
rejection as transient — `internal/inference/gateway/service.go:442-443`
says *no compatible deployment with capacity for model …* and
`:566-569` says *session-bound account is temporarily unavailable; retry
shortly*. A disabled provider is neither a capacity gap nor temporary. The
sticky contract (no silent rebind) and existing `upstream_unavailable`
code/503 shape are correct, but the text invites retry loops against a
condition that clears only after re-enable or a new session key."

**Environment.** Same recipe as §4.17: a fresh trap-cleaned disposable
PostgreSQL per run, explicit `DATABASE_URL` / `E2E_DATABASE_URL`, `-p 1`,
`-race`, `GOPROXY=off` + `GOFLAGS=-mod=readonly`, `go.mod` / `go.sum`
untouched.

##### 4.24.1 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | **RED** `go test -count=1 -v -run 'TestGateway_DisabledProvider_ErrorMessageIsNotCapacityOrTransient\|TestGateway_StickyDisabledProvider_ErrorMessageIsNotTemporaryOrTransient' ./internal/inference/gateway` | **1** | both messages misdescribed the condition — see §4.24.2 |
| 2 | **GREEN** the same command after the wording change | **0** | both cases pass |
| 3 | **GREEN** the whole T3 case set (16 cases) with `-p 1` | **0** | 4 packages `ok` |
| 4 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/catalog ./internal/inference/routing ./internal/inference/gateway ./internal/inference/httpapi` | **0** | 4 packages `ok` (3.614s / 1.915s / 11.170s / 64.534s) |
| 5 | `go test -race -count=1 -p 1 -v ./internal/inference/...` | **0** | 14 packages `ok`, 1171 PASS / 1 SKIP / 0 FAIL |
| 6 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | 52 PASS / 2 SKIP / 0 FAIL |
| 7 | `go vet ./internal/... ./cmd/...` + `go build ./...` | **0** / **0** | lint + build green |
| 8 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 9 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.5%` ≥ the 80% floor (T3 first round 80.4%) |
| 10 | `go test -race -count=1 ./tests/e2e/...` (`E2E_DATABASE_URL` → `t3_e2e`) | **0** | `ok github.com/yunhou/users/tests/e2e 162.762s` |

##### 4.24.2 RED (production code still at `d9a5ad3`)

```
=== RUN   TestGateway_DisabledProvider_ErrorMessageIsNotCapacityOrTransient
    sticky_candidates_test.go:388: message "no compatible deployment with capacity for model glm-4.6" must not mention "capacity"（停用不是容量缺口，也不是瞬时故障）
    sticky_candidates_test.go:388: message "no compatible deployment with capacity for model glm-4.6" must state that no upstream is dispatchable
--- FAIL: TestGateway_DisabledProvider_ErrorMessageIsNotCapacityOrTransient (0.09s)
=== RUN   TestGateway_StickyDisabledProvider_ErrorMessageIsNotTemporaryOrTransient
    sticky_candidates_test.go:419: message "session-bound account is temporarily unavailable; retry shortly" must not mention "retry"（停用不是容量缺口，也不是瞬时故障）
    sticky_candidates_test.go:419: message "session-bound account is temporarily unavailable; retry shortly" must not mention "temporar"（停用不是容量缺口，也不是瞬时故障）
    sticky_candidates_test.go:419: message "session-bound account is temporarily unavailable; retry shortly" must still name the model "glm-4.6"
    sticky_candidates_test.go:419: message "session-bound account is temporarily unavailable; retry shortly" must state that no upstream is dispatchable
    sticky_candidates_test.go:421: message "session-bound account is temporarily unavailable; retry shortly" must state that the session was not rebound to another account
--- FAIL: TestGateway_StickyDisabledProvider_ErrorMessageIsNotTemporaryOrTransient (0.11s)
```

Both failures are pure **assertion** failures — the error **code**
assertions (`upstream_unavailable`) passed at baseline, which is the
proof that the wire code was already right and only the text was wrong.

##### 4.24.3 GREEN

Command #2 — exit code **0**:

```
--- PASS: TestGateway_DisabledProvider_ErrorMessageIsNotCapacityOrTransient (0.08s)
--- PASS: TestGateway_StickyDisabledProvider_ErrorMessageIsNotTemporaryOrTransient (0.06s)
```

Command #3 (the whole T3 case set, `-p 1`) — exit code **0**: 16/16 pass
(14 earlier T3 cases + the 2 new message regressions).

> **Harness note (honest).** An earlier run of command #3 omitted `-p 1`
> while listing four database-touching packages: `go test` then runs the
> package binaries **in parallel**, and the `httpapi` fixture's
> `TRUNCATE` wiped the gateway package's rows mid-test. Two gateway cases
> failed for that reason alone (`-p 1` → all green; the same two cases
> also pass in isolation). It was a **procedure** error in the ad-hoc
> command, not a code defect — the standing rule of §3.2 is `-p 1` for
> every DB-touching suite, and the acceptance command itself uses it.

##### 4.24.4 The change

`internal/inference/gateway/service.go` — three message strings, one
helper, one corrected doc comment. **No logic, no code, no status, no
field changed.**

| Site | Before | After |
|---|---|---|
| normal path — empty candidate set | `no compatible deployment with capacity for model <id>` | `no dispatchable upstream for model <id>` |
| sticky path — the bound account left the candidate set | `session-bound account is temporarily unavailable; retry shortly` | `session-bound account is not dispatchable for model <id>; the session was not rebound to another account` |
| sticky refresh path — the refreshed candidate set is empty | `no compatible deployment with capacity for model <id>` | `no dispatchable upstream for model <id>` (same helper) |

`noDispatchableUpstreamMessage(modelID)` is the single source for the two
"no upstream can take this call" sites, so they cannot drift.

The `pinSessionCandidates` doc comment was corrected too: the old text
claimed the bound-account failure is "RETRYABLE" (transient fullness),
which is wrong for a disabled provider — the branch now reads "the call
fails and the session is NOT rebound".

##### 4.24.5 Compatibility

- **The wire contract is unchanged**: error code `upstream_unavailable`,
  HTTP **503**, the same `V1Error` shape, no new field, no new enum. Only
  the free-form `message` string changed.
- **No OpenAPI / integration-guide change is required**: the spec pins the
  503 by its **code** (`description: upstream_unavailable`,
  `docs/api/kaya-coding-plan.openapi.yaml:917`/`:1006`) and declares
  `message` as a plain `type: string` — it never pins the message text.
  Neither does `docs/api-integration-guide.md` nor the rollout runbook
  (they cite the code, not the sentence).
- **Preserved behaviour**: the sticky contract still forbids a silent
  rebind (the message now says so explicitly), the active-provider
  fallback still works for a *new* session, the in-flight request still
  finishes on its pinned snapshot, and the disabled provider still never
  receives an outbound call.
- **The new text is accurate for the other cause too**: an upstream
  account whose quota is exhausted (`quota_remaining=0`, see
  `docs/runbooks/kaya-coding-plan-rollout.md`) is likewise *not
  dispatchable* — the old "with capacity" wording happened to fit that
  case but misdescribed the disabled case; the new wording covers both
  without claiming either.

##### 4.24.6 Skips

Unchanged from §4.20 — `TestQuotaWorkerProcess` (`KAYA_QUOTA_WORKER=1`),
`TestLoadDrill_Throughput` and `TestLoadDrill_LongConnections`
(`KAYA_LOAD=1`). **Database-test skips: 0.**


#### 4.25 T3 fix-round commit

- One independent local commit on `fix/model-gateway-completion`, parent
  `d9a5ad3`; subject:
  `fix(inference): stop calling a provider-disable rejection a capacity gap`;
  3 files changed (1 production, 1 test, 1 runbook). The commit SHA is
  recorded in the task report
  (`.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-3-report.md`).
- Staged by explicit path (`git add <path> …`); `git add .` was never used.
- No push, no merge, no deploy, no database change on any real system.
- The dry-run/probe finding was **not** touched — it stays with T9.


### T4 — PostgreSQL-backed RPM/TPM

Recorded on 2026-11-06, base SHA `cdb2cdb` (the T3 branch tip; T0 base
`35cf836`). Task report:
`.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-4-report.md`.

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the instance
down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`; every
database-backed command used `-p 1`; `go.mod` / `go.sum` were never modified
(`git diff --stat go.mod go.sum` → empty after every run).

#### 4.26 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1, `t4_app`) | **0** | `applied=52 skipped=0` (51 baseline + 052) |
| 4 | `go run ./cmd/migrate` (run 2, `t4_app`) | **0** | `applied=0 skipped=52` (idempotent) |
| 5 | `go run ./cmd/migrate -status` | **0** | 52 × `✅` |
| 6 | `DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate` | **0** | e2e-side DB migrated before the e2e job |
| 7 | **RED** `go test -count=1 -run 'TestRateLimit_' -v ./internal/inference/gateway` | **1** | the leak reproduced — see §4.27 |
| 8 | **GREEN** the same command as #7 | **0** | all 13 T4 gateway cases pass |
| 9 | **GREEN** `go test -count=1 -run 'TestReserveRateTx_|TestRateSettle_|TestRateRelease_|TestCorrectRateTokens_|TestPurgeRateReservations_|TestRateTPM_' -v ./internal/inference/postgres` | **0** | 9 store-level cases pass |
| 10 | **GREEN** `go test -count=1 ./internal/inference/access` (pure rules) | **0** | 6 pure-rule cases pass |
| 11 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/... ./internal/router ./internal/config ./cmd/server` | **0** | 17 packages `ok` |
| 12 | `go test -race -count=1 -p 1 -v ./internal/inference/...` | **0** | 14 packages `ok`, 0 FAIL |
| 13 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | 52 PASS / 2 SKIP / 0 FAIL |
| 14 | `go test -race -count=1 ./tests/e2e/...` (`E2E_DATABASE_URL` → `t4_e2e`) | **0** | `ok github.com/yunhou/users/tests/e2e 417.807s` (125 PASS / 1 SKIP) |
| 15 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 16 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.1%` ≥ the 80% floor (T3 80.5%, T2 80.3%, T1 80.5%, T0 80.4%) |

#### 4.27 RED evidence (before any production edit)

Command #7 ran against the **unmodified** production code at `cdb2cdb`. The
new cases were written against the *existing* API surface only (the gateway's
`ChatCompletions`, the real PostgreSQL store, the existing policy/Key
columns), so every failure is a behavioural assertion failure — never a
compile error, never a skipped database test.

```
=== RUN   TestRateLimit_PolicyRPM_CapsConcurrentAdmissions
    rate_limits_test.go:211: admitted = 3, want 2 (RPM=2 是硬上限，第三次必须被拒)
    rate_limits_test.go:214: rate-limited = 0, want 1
--- FAIL: TestRateLimit_PolicyRPM_CapsConcurrentAdmissions (0.09s)
=== RUN   TestRateLimit_TwoInstancesTwoPools_ShareTheAccountCounter
    rate_limits_test.go:255: admitted = 3, want <= 2 (两个实例必须共享同一个账户计数)
--- FAIL: TestRateLimit_TwoInstancesTwoPools_ShareTheAccountCounter (0.10s)
=== RUN   TestRateLimit_CrossKeyAndEntrypointShareTheAccountCounter
    rate_limits_test.go:282: call 3 err = <nil>, want rate_limited (/chat 与 /v1 共用账户计数)
--- FAIL: TestRateLimit_CrossKeyAndEntrypointShareTheAccountCounter (0.11s)
=== RUN   TestRateLimit_KeyRPMIsEnforcedPerKey
    rate_limits_test.go:302: call 2 err = <nil>, want rate_limited (Key RPM=1)
--- FAIL: TestRateLimit_KeyRPMIsEnforcedPerKey (0.09s)
=== RUN   TestRateLimit_PolicyChangeDoesNotResetTheMinute
    rate_limits_test.go:341: after policy change err = <nil>, want rate_limited (换套餐不得重置一分钟消费)
--- FAIL: TestRateLimit_PolicyChangeDoesNotResetTheMinute (0.12s)
=== RUN   TestRateLimit_UpstreamFailureStillCountsTheRequest
    rate_limits_test.go:383: call 3 err = <nil>, want rate_limited (上游失败后仍计 RPM)
--- FAIL: TestRateLimit_UpstreamFailureStillCountsTheRequest (0.13s)
=== RUN   TestRateLimit_TPM_InFlightHoldBlocksAndSettlementReleases
    rate_limits_test.go:413: call 2 err = <nil>, want rate_limited (60 在途 + 50 预占 > TPM 100)
--- FAIL: TestRateLimit_TPM_InFlightHoldBlocksAndSettlementReleases (0.11s)
=== RUN   TestRateLimit_TPM_OverHoldCountsTheActualTotal
    rate_limits_test.go:440: call 2 err = <nil>, want rate_limited (实际 100 token 全额计入 TPM)
--- FAIL: TestRateLimit_TPM_OverHoldCountsTheActualTotal (0.11s)
=== RUN   TestRateLimit_TPM_UnknownUsageKeepsTheHold
    rate_limits_test.go:475: call 2 err = <nil>, want rate_limited (unknown 用量不得释放预占)
--- FAIL: TestRateLimit_TPM_UnknownUsageKeepsTheHold (0.12s)
=== RUN   TestRateLimit_WalletPathAppliesTheSameLimits
    rate_limits_test.go:517: call 3 err = <nil>, want rate_limited (钱包路径同样受 RPM 限制)
--- FAIL: TestRateLimit_WalletPathAppliesTheSameLimits (0.11s)
=== RUN   TestRateLimit_DBOutageFailsClosed
--- PASS: TestRateLimit_DBOutageFailsClosed (0.09s)
FAIL
FAIL	github.com/yunhou/users/internal/inference/gateway	2.024s
```

**Root cause (one defect, ten surfaces).** The business rate limits did not
exist in the gateway at all: the policy's `rpm_limit`/`tpm_limit` and the
Key's `rpm_limit` were persisted and administered, but **nothing read them
on the inference path**. The only counter was the process-local
`access.RPMCounter` inside `APIKeyAuth` — per instance, therefore unable to
hold a global limit, and never applied to `/chat` (which never passes through
that middleware).

**Protection evidence (already-correct behaviour, recorded instead of a forced
change).** `TestRateLimit_DBOutageFailsClosed` **passed** at the pre-fix
baseline: a database outage already refused the admission (fail-closed) and
was not reported as a quota exhaustion. T4 kept that behaviour and made the
error shape explicit (503 `service_unavailable` instead of a generic 500).

The store-level cases (command #9) and the pure-rule cases (command #10)
could not be written before the implementation: they exercise the persistent
rate reservation itself (idempotency, settle/release/correction, retention),
which has no pre-T4 API surface. They were written together with the
implementation and are GREEN-only by construction; the behaviour they pin is
the brief's own wording (TPM lifecycle, net-delta correction, retention).

#### 4.28 GREEN evidence

Command #8 — exit code **0**:

```
--- PASS: TestRateLimit_PolicyRPM_CapsConcurrentAdmissions (0.16s)
--- PASS: TestRateLimit_TwoInstancesTwoPools_ShareTheAccountCounter (0.19s)
--- PASS: TestRateLimit_CrossKeyAndEntrypointShareTheAccountCounter (0.35s)
--- PASS: TestRateLimit_KeyRPMIsEnforcedPerKey (0.21s)
--- PASS: TestRateLimit_PolicyChangeDoesNotResetTheMinute (0.16s)
--- PASS: TestRateLimit_UpstreamFailureStillCountsTheRequest (0.27s)
--- PASS: TestRateLimit_TPM_InFlightHoldBlocksAndSettlementReleases (0.14s)
--- PASS: TestRateLimit_TPM_OverHoldCountsTheActualTotal (0.16s)
--- PASS: TestRateLimit_TPM_UnknownUsageKeepsTheHold (0.18s)
--- PASS: TestRateLimit_WalletPathAppliesTheSameLimits (0.21s)
--- PASS: TestRateLimit_DBOutageFailsClosed (0.10s)
--- PASS: TestRateLimit_WalletFreezeFailureRollsBackTheWholeAdmission (0.08s)
--- PASS: TestRateLimit_RetryAfterOnlyWhenTheRecoveryIsDeterminable (0.18s)
```

Command #9 (store level) — exit code **0**: 9/9 cases pass
(`TestReserveRateTx_CountsOneLogicalRequestOnce`, `…_RejectionRollsBackEverything`,
`TestRateSettle_ConvertsTheHoldIntoTheActualTokens`,
`TestRateSettle_DuplicateSettlementDoesNotDoubleCountTheTokens`,
`TestRateRelease_DropsTheHoldButKeepsTheCount`,
`TestCorrectRateTokens_NetDeltaKeepsTheOriginalSettlementTime`,
`TestCorrectRateTokens_OutOfWindowCorrectionDoesNotConsumeTheCurrentMinute`,
`TestPurgeRateReservations_NeverDeletesUnsettledReservations`,
`TestRateTPM_InFlightHoldSurvivesTheWindow`).

Command #10 (pure rules + reaper) — exit code **0**: 6 pure-rule cases and 3
reaper cases pass.

Command #9 (store level) — exit code **0**: `TestReserveRateTx_CountsOneLogicalRequestOnce`,
`…_RejectionRollsBackEverything`, `TestRateSettle_ConvertsTheHoldIntoTheActualTokens`,
`TestRateSettle_DuplicateSettlementDoesNotDoubleCountTheTokens`,
`TestRateRelease_DropsTheHoldButKeepsTheCount`,
`TestCorrectRateTokens_NetDeltaKeepsTheOriginalSettlementTime`,
`TestCorrectRateTokens_OutOfWindowCorrectionDoesNotConsumeTheCurrentMinute`,
`TestPurgeRateReservations_NeverDeletesUnsettledReservations`,
`TestRateTPM_InFlightHoldSurvivesTheWindow` — 9/9 pass.

Command #11 (the brief's acceptance command) — exit code **0**:

```
ok  	github.com/yunhou/users/internal/inference/access	1.709s
ok  	github.com/yunhou/users/internal/inference/accounting	1.681s
ok  	github.com/yunhou/users/internal/inference/catalog	5.835s
ok  	github.com/yunhou/users/internal/inference/credentials	3.254s
ok  	github.com/yunhou/users/internal/inference/domain	1.679s
ok  	github.com/yunhou/users/internal/inference/gateway	27.465s
ok  	github.com/yunhou/users/internal/inference/httpapi	110.179s
ok  	github.com/yunhou/users/internal/inference/management	3.203s
ok  	github.com/yunhou/users/internal/inference/postgres	31.509s
ok  	github.com/yunhou/users/internal/inference/providers	1.782s
ok  	github.com/yunhou/users/internal/inference/providers/connector	1.672s
ok  	github.com/yunhou/users/internal/inference/quota	1.635s
ok  	github.com/yunhou/users/internal/inference/routing	1.897s
ok  	github.com/yunhou/users/internal/inference/workers	9.798s
ok  	github.com/yunhou/users/internal/router	2.625s
ok  	github.com/yunhou/users/internal/config	1.659s
ok  	github.com/yunhou/users/cmd/server	2.797s
```

(The final verification run above is the one recorded in this section; the
wall-clock numbers vary with the machine load — the *results* were identical
in three consecutive runs.)

#### 4.29 Skips

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; needs `KAYA_QUOTA_WORKER=1` (`quota_concurrency_test.go:773`). The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and passes with real contention output. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1`. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |
| 4 | `TestUsageTokenAcceptance_TenPagesTwoMinutes` | `tests/e2e` | Explicit non-default gate (usage-stats acceptance drill). | No |

**Database-test skips: 0** — identical to the T0 (§2.6), T1 (§4.4), T2
(§4.12) and T3 (§4.20) inventories; grepping the skip lines for `postgres` /
`no database` / `database unavailable` returns nothing.

#### 4.30 Migration

**One appended migration: `migrations/052_inference_rate_reservations.sql`**
(52 = the re-checked latest number; 051 was the T2-fix-round baseline). It
creates `inference_rate_reservations`:

| Column | Meaning |
|---|---|
| `request_id` | the logical request id — the **idempotency identity** (`UNIQUE (request_id, scope)`) |
| `billing_account_id` / `api_key_id` | the counting key (account-level and per-Key) |
| `scope` | `account` (account RPM counter **and** the account-level TPM reservation) or `key` (the per-Key RPM counter) |
| `counted_at` | the successful-admission instant (**database time**) — the rolling-60s window attribution |
| `reserved_tokens` / `settled_tokens` | the account-level TPM: the in-flight hold and the settled actual |
| `state` | `reserved` (in flight) / `settled` / `released` |

Indexes: `(billing_account_id, counted_at DESC) WHERE scope='account'`,
`(api_key_id, counted_at DESC) WHERE scope='key'`,
`(billing_account_id) WHERE state='reserved'` (the in-flight TPM sum),
`(billing_account_id, settled_at DESC) WHERE state='settled'` (the settled
TPM window sum), `(counted_at) WHERE state <> 'reserved'` (the retention
sweep). The counting key deliberately does **not** contain the policy
version — switching plans never resets the minute.

Evidence: `go run ./cmd/migrate` (run 1) → `applied=52 skipped=0`;
(run 2) → `applied=0 skipped=52`; `go run ./cmd/migrate -status` → 52 × `✅`.
The e2e-side disposable database was migrated the same way.

**Retention / cleanup.** `workers.RateReservationReaper` sweeps hourly and
deletes the **terminal** rate reservations (`state <> 'reserved'`) older than
**24 hours**. An unsettled reservation is **never** deleted by the cleanup —
it still occupies the TPM budget (a long stream keeps its hold beyond the
60s window), so a TTL must not release it. The reaper has its own narrow
store contract (`workers.RateReservationStore`) so the settlement-recovery
store contract stays untouched.

#### 4.31 Files changed

| File | Change |
|---|---|
| `migrations/052_inference_rate_reservations.sql` | **new** — the persistent rate reservation table + indexes |
| `internal/inference/domain/rate_limits.go` | **new** — the shared shapes (`RateLimits`, `RateReserveCommand`, `RateUsage`, `RateBlock`) and the typed rejection `RateLimitError` |
| `internal/inference/domain/errors.go` | `CodeOf` recognises the typed rate rejection; new `CodeServiceUnavailable` |
| `internal/inference/access/inference_limits.go` | **new** — the pure rules (`NormalizedTokens`, `EvaluateRateLimits`, `RecoverySeconds`) |
| `internal/inference/access/ratelimit.go` (+ its two test files) | **deleted** — the process-local counter was a duplicate, per-instance business limit (see §4.32) |
| `internal/inference/access/principal_authenticate_edges_test.go` | **new** (renamed from `ratelimit_principal_edges_test.go`) — keeps the `Resolver.Authenticate` branch coverage |
| `internal/inference/postgres/inference_limit_repo.go` | **new** — `ReserveRateTx` / `CorrectRateTokensTx` / `PurgeRateReservations` + the settle/release writers |
| `internal/inference/postgres/ledger_repo.go` | the settlement converts the TPM hold into the **actual** normalized tokens (plan and wallet paths) |
| `internal/inference/postgres/reservation_repo.go` | the release drops the TPM hold (the RPM count stays) |
| `internal/inference/quota/service.go` | the rate gate runs **inside the admission transaction** (plan and wallet paths); `Service.AccountRPM`; a storage outage is a service error |
| `internal/inference/gateway/service.go` | one logical request id for the whole call (the plan→wallet fallback reuses it); a rate rejection is never masked by the plan's `quota_exceeded` |
| `internal/inference/httpapi/apikey_auth.go` | the duplicate business decrement is **removed** (authentication + the per-IP guard stay) |
| `internal/inference/httpapi/chat_completions.go`, `messages.go`, `admin_models.go` | the typed rate rejection → 429 (+ `Retry-After` only when determinable); `service_unavailable` → 503 |
| `internal/inference/httpapi/user_api_keys.go`, `internal/router/router.go`, `cmd/server/main.go` | the counter wiring is gone; `quotaSvc.AccountRPM = cfg.InferenceAccountRPM` |
| `internal/service/chat_facade.go` | the `/chat` facade maps a storage outage to 503 (never a fake quota exhaustion) |
| `internal/inference/workers/rate_reservation_reaper.go` | **new** — the retention sweep (never deletes an unsettled reservation) |
| `internal/inference/gateway/rate_limits_test.go` | **new** — 11 T4 gateway cases (two instances/two pools, cross-Key, cross-entrypoint, policy change, TPM lifecycle, wallet path, DB outage, Retry-After) |
| `internal/inference/postgres/inference_limit_test.go` | **new** — 9 store-level cases (idempotency, rollback, settle/release/correction, retention) |
| `internal/inference/access/inference_limits_test.go` | **new** — the pure-rule cases (token normalisation, simultaneous limits, Retry-After determinability) |
| `internal/inference/workers/rate_reservation_reaper_test.go` | **new** — the retention sweep cases |
| `internal/inference/quota/service_test.go` | the fake store grows `ReserveRateTx` |
| `internal/inference/httpapi/user_api_keys_test.go` | the auth-layer rate-limit cases are replaced by `TestV1_AuthLayerOnlyAuthenticates` (the business limit moved to the admission) |
| `tests/e2e/chat_gateway_test.go`, `tests/e2e/testhelpers.go` | the e2e wipe lists the new table (FK-safe order) |
| `tests/e2e/kaya_coding_plan_test.go`, `tests/integration/*.go`, `internal/inference/httpapi/*_test.go` | the `APIKeyAuth` signature change (the counter argument is gone) |
| `docs/api/kaya-coding-plan.openapi.yaml`, `docs/api-integration-guide.md`, `CLAUDE.md`, `.env.example`, `internal/config/config.go` | the new 429/503 semantics, the business-rate-limit contract, and the `INFERENCE_ACCOUNT_RPM` meaning |
| `docs/superpowers/plans/2026-11-05-users-model-gateway-completion.md` | the T4 **Interfaces** paragraph now carries the real signatures (required by the plan) |

#### 4.32 Notes (lock order, overhead, retention, compatibility, honesty)

- **Lock order.** The rate gate is part of the **existing** admission
  transaction, so it inherits the fixed lock order: account (`FOR UPDATE`) →
  request row → windows (five_hour→weekly→monthly) → Key budget →
  reservation/lease rows → **rate rows**. The rate reads are plain `SELECT`s
  (no row locks) and the rate writes touch only the new request's own rows,
  so the gate can never form a deadlock cycle with the settlement path
  (request row → rate rows). Same-account admissions still serialize on the
  account anchor lock, which is what makes the read-then-insert of the
  counters atomic across instances.
- **Per-account overhead.** One aggregate statement (5 sub-queries: the
  account count, the Key count, the in-flight TPM sum, the settled TPM window
  sum and the Key's `rpm_limit`) plus 1–2 inserts per admission; the
  recovery-instant window scans run **only on a rejection**. Measured on the
  disposable PostgreSQL 16 with ~200 rows in the rolling window: **≈0.87 ms
  per admission** (empty-transaction baseline ≈0.09 ms, with the gate
  ≈0.97 ms). All scans are index-only over the window's rows (bounded by the
  account's RPM), not table scans.
- **Time semantics.** Every timestamp is the **database** clock (`now()`),
  so instances agree on the window attribution; the settlement instant is
  recorded once and a later correction never moves it.
- **Token normalisation.** The TPM counts exactly what the price list bills
  (`accounting.BillableBuckets` afterwards): input + cache read + cache
  write + output. The reasoning detail is already inside the output total
  and the cache buckets are already subtracted from the input — neither is
  counted twice.
- **Compatibility.** The wire contract only gained clarity: a rate
  rejection is the already-documented 429 `rate_limited`; a storage outage
  is a new, explicit 503 `service_unavailable` (never a fake quota
  exhaustion). `quota_exceeded` keeps its meaning (money). The OpenAPI spec,
  the integration guide, `CLAUDE.md` and `.env.example` were updated for
  both.
- **Behaviour change for operators.** The account/Key RPM buckets moved from
  the auth middleware into the business admission: a request that never
  reaches the admission (invalid shape, model not allowed) no longer
  consumes a rate token, and the limit is now genuinely global across
  instances instead of "120 per instance".
- **Honest limitation.** The **money-side** settlement correction (the
  ledger reversal/adjustment) is still the reconciliation flow's job; T4 adds
  the **rate-side** counterpart (`Store.CorrectRateTokensTx`: net delta per
  request, original settlement instant kept, idempotent) and pins it with
  tests, but no production flow calls it yet — the evidence window is still
  driven by the existing reconciliation queue.

#### 4.33 T4 fix round — the correction composition (money + token budget in one transaction)

Recorded on 2026-11-06, after the T4 commit `cde1c5e`. One **Important**
review finding was fixed with TDD; the fix round is one extra commit
(one extra commit on top of `cde1c5e`; its SHA is
recorded in the task report). Nothing else was touched — the deferred
minors stay deferred.

**The finding (verbatim, condensed).** "The correction composition the report
prescribes is unsafe, and the estimated→reported lifecycle is not realized
end-to-end. `CorrectRateTokensTx` has no production caller; money-side
`CorrectSettlement` is also externally unwired. `CorrectRateTokensTx` returns
`CodeConflict` when no settled rate row exists, so following the report's
instruction to call it with money correction rolls back money corrections for
pre-migration-052 requests and for terminal rows removed by retention. The
same-transaction consistency is only a convention: `CorrectionCommand` carries
no corrected bucket/token field, so nothing forces or enables correcting TPM
with the ledger in one transaction."

**Environment.** Same recipe as §4.26: a fresh trap-cleaned disposable
PostgreSQL per run, explicit `DATABASE_URL` / `E2E_DATABASE_URL`, `-p 1`,
`-race`, `GOPROXY=off` + `GOFLAGS=-mod=readonly`, `go.mod` / `go.sum`
untouched.

##### 4.33.1 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | **RED** `go test -count=1 -run 'TestCorrectSettlement_' -v ./internal/inference/postgres` | **1** | the composition was missing — see §4.33.2 |
| 2 | `go run ./cmd/migrate` (run 1 / run 2 / `-status`) | **0** / **0** / **0** | `applied=52 skipped=0` → `applied=0 skipped=52` → 52 × `✅` |
| 3 | **GREEN** the same command as #1 | **0** | 11/11 correction cases pass (9 new + 2 pre-existing) |
| 4 | **GREEN** `go test -count=1 ./internal/inference/postgres/...` (whole package) | **0** | the package is green (the pre-existing correction tests were updated for the new contract) |
| 5 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/... ./internal/router ./internal/config ./cmd/server` | **0** | 17 packages `ok` (re-run on the final tree: 17 `ok`, exit 0) |
| 6 | `go test -race -count=1 -p 1 -v ./internal/inference/... ./tests/integration/...` | **0** | 0 FAIL; the skip inventory is unchanged (3 skips: `TestQuotaWorkerProcess`, `TestLoadDrill_Throughput`, `TestLoadDrill_LongConnections` — **0 database-test skips**) |
| 7 | CI `go-ci` gate + coverage floor | **0** | 33 packages `ok`, `total coverage: 80.1%` ≥ the 80% floor |
| 8 | `go test -race -count=1 ./tests/e2e/...` (`E2E_DATABASE_URL` → `t4_e2e`) | **0** | `ok github.com/yunhou/users/tests/e2e 291.264s` |

##### 4.33.2 RED evidence (production code still at `cde1c5e`)

The new cases were written against the **existing** API surface
(`postgres.Store.CorrectSettlement` + the real store), so every failure is a
behavioural assertion — never a compile error, never a skipped DB test.

```
=== RUN   TestCorrectSettlement_UpdatesTheLedgerAndTheTokenBudgetOnce
    correction_rate_test.go:171: settled tokens = 1000, want 1200 (the token budget must be corrected with the money)
--- FAIL: TestCorrectSettlement_UpdatesTheLedgerAndTheTokenBudgetOnce (0.06s)
=== RUN   TestCorrectSettlement_MissingRateRowIsABenignAbsence
--- PASS: TestCorrectSettlement_MissingRateRowIsABenignAbsence (0.05s)
=== RUN   TestCorrectSettlement_PresentButMalformedRateRowIsNotABenignAbsence/not_settled_row
    correction_rate_test.go:231: err = <nil>, want conflict (a malformed rate row is not a benign absence)
=== RUN   TestCorrectSettlement_PresentButMalformedRateRowIsNotABenignAbsence/key_row_without_account_row
    correction_rate_test.go:251: err = <nil>, want conflict (a key row without its account row is drift)
--- FAIL: TestCorrectSettlement_PresentButMalformedRateRowIsNotABenignAbsence (0.11s)
=== RUN   TestCorrectSettlement_LedgerHalfFailureRollsBackTheTokenCorrection
    correction_rate_test.go:274: settled tokens = 1000, want 1200 after the first correction
--- FAIL: TestCorrectSettlement_LedgerHalfFailureRollsBackTheTokenCorrection (0.05s)
=== RUN   TestCorrectSettlement_RateHalfFailureRollsBackTheLedgerCorrection
    correction_rate_test.go:311: err = <nil>, want conflict (a malformed rate row must fail the correction)
--- FAIL: TestCorrectSettlement_RateHalfFailureRollsBackTheLedgerCorrection (0.05s)
=== RUN   TestCorrectSettlement_RepeatedCorrectionIsIdempotent
--- PASS: TestCorrectSettlement_RepeatedCorrectionIsIdempotent (0.05s)
=== RUN   TestCorrectSettlement_CorrectionKeepsTheOriginalSettlementInstant
    correction_rate_test.go:374: settled tokens = 1000, want 5000 (the request fact is corrected)
--- FAIL: TestCorrectSettlement_CorrectionKeepsTheOriginalSettlementInstant (0.05s)
=== RUN   TestCorrectSettlement_UnknownTokenEvidenceKeepsThePreviousTotal
--- PASS: TestCorrectSettlement_UnknownTokenEvidenceKeepsThePreviousTotal (0.05s)
=== RUN   TestCorrectSettlement_KnownTokenEvidenceRequiresTheCorrectedTotal
    correction_rate_test.go:426: err = <nil>, want invalid_input (the token total must not be omitted)
--- FAIL: TestCorrectSettlement_KnownTokenEvidenceRequiresTheCorrectedTotal (0.04s)
--- PASS: TestCorrectSettlement_ReversalAndDelta (0.05s)
FAIL	github.com/yunhou/users/internal/inference/postgres	1.209s
```

**Root cause.** `postgres.CorrectSettlement` corrected only the **money**
(ledger reversal/adjustment + the window/budget aggregates + the request's
settled amount). The **token budget** was never touched: the rate
reservation kept the estimated total, so the TPM and the ledger disagreed
after every correction — and the T4 report told the reader to wire the two
together by calling `CorrectRateTokensTx`, whose "no settled rate row ⇒
`CodeConflict`" behaviour would have rolled back valid money corrections for
pre-052 requests and for retention-purged terminal rows.

**Protection evidence (already-correct behaviour, recorded instead of a forced
change).** Three cases passed at the pre-fix baseline:

| Case | Guard that already held |
|---|---|
| `…_MissingRateRowIsABenignAbsence` | a money correction is never rolled back by a missing rate row (trivially true before the rate half was wired; a real guard from now on) |
| `…_RepeatedCorrectionIsIdempotent` | the idempotency key makes a replayed correction a conflict and changes nothing |
| `…_UnknownTokenEvidenceKeepsThePreviousTotal` | unknown token evidence never collapses into zero |

##### 4.33.3 The fix

* `postgres.CorrectionCommand` gained **`CorrectedTokens *int64`** — the
  **absolute** corrected normalized token total (the TPM measure: input +
  cache read + cache write + output, after the overlap normalization the
  settlement path bills; the caller computes it with
  `access.NormalizedTokens` over the same normalized buckets it carries in
  `Corrected`). `nil` = the corrected evidence carries **no known** token
  total (unknown) → the token-budget correction is a deliberate no-op and
  the previous total is kept — **unknown never collapses into zero**.
* `Store.CorrectSettlement` now performs the **token-budget correction in
  the same transaction** as the ledger correction (step 5b, between the
  ledger entries and the window/budget deltas): the rate row's total is
  **replaced** by the absolute corrected total (the storage effect is a
  **net delta** — one authoritative total, never an accumulation across
  revisions) and the **original settlement instant is kept** (a correction
  never resets the 60-second window attribution, so an out-of-window
  correction does not create consumption in the current minute).
* **The correction entrypoint can no longer silently omit the TPM**: known
  token buckets without a corrected total → `CodeInvalidInput` ("the token
  budget correction must not be omitted").
* **A missing rate row is a benign absence**, not an anomaly: the request may
  predate migration 052 or its terminal row may have been purged by the
  retention sweep → the money correction still applies. **A row that is
  present but malformed** (a settled request whose rate row is still
  in-flight, or a key row without its account row) is an **anomaly** and
  fails loudly with `CodeConflict` — it is never mistaken for an absence.
* `Store.CorrectRateTokensTx` (the standalone rate-only correction) now
  shares that core and therefore has the same semantics; it still takes an
  **absolute corrected total** whose storage effect is a **net delta**.
* **Recorded boundary (not expanded):** wallet-charged requests are still
  explicitly rejected by `CorrectSettlement` (`CodeInvalidInput` — the
  micromoney unit and the wallet compensation entries are not adapted), so a
  wallet request's token budget is **not** corrected through this channel
  yet. The money-correction channel still has **no production caller** (the
  "estimate → later real evidence" flow stays in the reconciliation queue);
  the fix makes the entrypoint safe, it does not add a new endpoint, a new
  job or an auto-correction sweep.

##### 4.33.4 GREEN evidence

Command #3 — exit code **0**:

```
--- PASS: TestCorrectSettlement_UpdatesTheLedgerAndTheTokenBudgetOnce (0.06s)
--- PASS: TestCorrectSettlement_MissingRateRowIsABenignAbsence (0.07s)
--- PASS: TestCorrectSettlement_PresentButMalformedRateRowIsNotABenignAbsence (0.09s)
    --- PASS: …/not_settled_row (0.05s)
    --- PASS: …/key_row_without_account_row (0.05s)
--- PASS: TestCorrectSettlement_LedgerHalfFailureRollsBackTheTokenCorrection (0.07s)
--- PASS: TestCorrectSettlement_RateHalfFailureRollsBackTheLedgerCorrection (0.05s)
--- PASS: TestCorrectSettlement_RepeatedCorrectionIsIdempotent (0.07s)
--- PASS: TestCorrectSettlement_CorrectionKeepsTheOriginalSettlementInstant (0.05s)
--- PASS: TestCorrectSettlement_UnknownTokenEvidenceKeepsThePreviousTotal (0.05s)
--- PASS: TestCorrectSettlement_KnownTokenEvidenceRequiresTheCorrectedTotal (0.05s)
--- PASS: TestCorrectSettlement_ReversalAndDelta (0.06s)   (pre-existing)
```

##### 4.33.5 Files changed (fix round)

| File | Change |
|---|---|
| `internal/inference/postgres/reconciliation_repo.go` | `CorrectionCommand.CorrectedTokens *int64`; the guard; the token-budget correction inside the same transaction; the doc (the composition, the benign absence, the malformed row, the wallet boundary, the absolute total / net delta) |
| `internal/inference/postgres/inference_limit_repo.go` | the shared core `correctRateTokensTx` (benign absence vs malformed row); `CorrectRateTokensTx` delegates to it |
| `internal/inference/access/inference_limits.go` | the pure rule `KnownTokenBuckets` (does the normalized measure have a known amount?) |
| `internal/inference/postgres/correction_rate_test.go` | **new** — 9 correction × TPM cases |
| `internal/inference/postgres/repo_misc_test.go`, `recovery_test.go`, `customer_read_repo_test.go` | the pre-existing correction cases now state the corrected token total (the contract changed) |
| `docs/superpowers/plans/2026-11-05-users-model-gateway-completion.md` | the T4 **Interfaces** paragraph carries the final signatures (`CorrectionCommand.CorrectedTokens`, `CorrectSettlement`, `correctRateTokensTx`, `KnownTokenBuckets`) and the correction-channel notes |

##### 4.33.6 Notes (compatibility, honesty)

* **No client-visible change**: the correction channel has no HTTP surface
  and no production caller, so the OpenAPI spec and the integration guide
  need no update for this round.
* **Preserved:** the net-delta effect (one authoritative total), the
  original settlement instant, the idempotency key semantics, the
  out-of-window behaviour, `unknown ≠ zero`, the fixed-point money
  arithmetic and the one-charge-per-request rule (a correction appends an
  adjustment/reversal, never a second charge).
* **Atomicity:** the two halves share one `UnitOfWork` — an injected failure
  in either half rolls back both (pinned by the two "half failure" cases).
* **Honest limitation:** the estimated→reported lifecycle is still not
  realized end-to-end in production (no caller drives `CorrectSettlement`);
  the fix makes the entrypoint correct and safe so that whoever wires the
  evidence flow cannot make the ledger and the TPM disagree.

### T5 — Per-key concurrency

Recorded on 2026-11-06, base SHA `2cf99da` (the T4 branch tip; T0 base `35cf836`).
**T5 commit:** `feat(inference): enforce a per-Key concurrency limit at
admission` — the commit that carries this section (19 files; **not pushed,
not merged, not deployed**). The SHA is recorded in the task report (the
report lives outside version control).
Task report: `.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-5-report.md`.

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the
instance down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`;
every database-backed command used `-p 1`; `go.mod` / `go.sum` were never
modified (`git diff --stat go.mod go.sum` → empty after every run).

#### 4.34 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1, `t5_app`) | **0** | `applied=52 skipped=0` (T4 baseline 52; **T5 adds no migration**) |
| 4 | `go run ./cmd/migrate` (run 2, `t5_app`) | **0** | `applied=0 skipped=52` (idempotent) |
| 5 | `go run ./cmd/migrate -status` | **0** | 52 × `✅` |
| 6 | `DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate` | **0** | e2e-side DB migrated before the e2e job |
| 7 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/...` | **0** | 14 packages `ok` — the brief's exact acceptance command |
| 8 | `go test -race -count=1 -p 1 -v ./internal/inference/... ./tests/integration/...` | **0** | skip inventory — see §4.37 |
| 9 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 10 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.2%` ≥ the 80% floor (T0 80.4%, T1 80.5%, T2 80.3%, T3 80.4%, T4 80.1%) |
| 11 | `go test -race -count=1 ./tests/e2e/...` (`E2E_DATABASE_URL` → `t5_e2e`) | **0** | `ok github.com/yunhou/users/tests/e2e 314.854s` |

#### 4.35 RED evidence (before any production edit)

**RED-1a — compile-level RED for the two packages whose new cases must
reference the new type surface.** `access.CreateParams.ConcurrencyLimit` and
`quota.AdmissionResult.Leases` did not exist at `2cf99da`, so those two test
files cannot compile against the unmodified production code. The compile
errors (verbatim) are the RED for that part:

```
$ go vet ./internal/inference/access/ ./internal/inference/quota/
# github.com/yunhou/users/internal/inference/quota
vet: internal/inference/quota/key_lease_test.go:35:13: res.Leases undefined (type *AdmissionResult has no field or method Leases)
# github.com/yunhou/users/internal/inference/access
vet: internal/inference/access/apikey_concurrency_test.go:31:59: unknown field ConcurrencyLimit in struct literal of type CreateParams
```

Those two files were then moved aside (`mv` to `/tmp`) so the remaining
packages could be run against the **unmodified** production tree; the
behavioural RED below is from that state.

**RED-1b — `httpapi` (management surface).** The `concurrency_limit` field did
not exist on the wire, so every request that carried it was rejected as an
unknown field:

```
=== RUN   TestUserAPIKeys_ConcurrencyLimitCreateReadUpdate
    user_api_keys_concurrency_test.go:52: concurrency_limit = <nil> (present=false), want null (缺省不加 Key 限制)
    user_api_keys_concurrency_test.go:60: create key: 400 {"code":400,"data":null,"message":"invalid request body (unknown fields rejected): json: unknown field \"concurrency_limit\""}
--- FAIL: TestUserAPIKeys_ConcurrencyLimitCreateReadUpdate (0.26s)
=== RUN   TestUserAPIKeys_ConcurrencyLimitRejectsNonPositive
    user_api_keys_concurrency_test.go:150: create key: 400 {"code":400,"data":null,"message":"invalid request body (unknown fields rejected): json: unknown field \"concurrency_limit\""}
--- FAIL: TestUserAPIKeys_ConcurrencyLimitRejectsNonPositive (0.20s)
=== RUN   TestUserAPIKeys_ConcurrencyLimitKeepsOwnerChecks
    user_api_keys_concurrency_test.go:170: create key: 400 {"code":400,"data":null,"message":"invalid request body (unknown fields rejected): json: unknown field \"concurrency_limit\""}
--- FAIL: TestUserAPIKeys_ConcurrencyLimitKeepsOwnerChecks (0.11s)
FAIL	github.com/yunhou/users/internal/inference/httpapi	4.177s
```

**RED-1c — `postgres` (the admission).** A Key with `concurrency_limit = 1`
was **not** limited at all: the second concurrent admission was **accepted**
(`err = <nil>`) and no `api_key`-scope lease row was ever written.

```
=== RUN   TestKeyConcurrencyLimit_SameKeySecondAdmissionIsRejected
    key_concurrency_test.go:77: held key leases = 0, want 1 (the admitted request holds the Key slot)
--- FAIL: TestKeyConcurrencyLimit_SameKeySecondAdmissionIsRejected (0.19s)
=== RUN   TestKeyConcurrencyLimit_DifferentKeysAreIndependent_AccountCapStillApplies
    key_concurrency_test.go:157: key leases = 0/0, want 1/1 (each key holds its own slot)
    key_concurrency_test.go:162: second admission on the same key: err = <nil>, want insufficient_capacity
--- FAIL: TestKeyConcurrencyLimit_DifferentKeysAreIndependent_AccountCapStillApplies (0.39s)
=== RUN   TestKeyConcurrencyLimit_UnlimitedKeyTakesNoLease
--- PASS: TestKeyConcurrencyLimit_UnlimitedKeyTakesNoLease (0.27s)
=== RUN   TestAPIKeyLeaseScope_IsolatesKeysAndReclaimsAfterTTL
--- PASS: TestAPIKeyLeaseScope_IsolatesKeysAndReclaimsAfterTTL (0.25s)
FAIL	github.com/yunhou/users/internal/inference/postgres	2.579s
```

The two PASSes are **protection evidence** (§4.37), not new behaviour: the
generic lease protocol already supported the `api_key` scope (per-scope
isolation + TTL reclamation), and a key without a limit must take no lease.

**RED-1d — `gateway` (the lease lifecycle).** No Key lease was ever taken,
so the second concurrent request was **not** rejected and none of the five
terminal paths released a Key lease:

```
=== RUN   TestGateway_KeyConcurrencyLimit_BlocksConcurrentRequestAndReleasesSlot
    key_concurrency_test.go:88: held key leases during the request = 0, want 1
    key_concurrency_test.go:96: second concurrent request: err = <nil>, want insufficient_capacity
--- FAIL: TestGateway_KeyConcurrencyLimit_BlocksConcurrentRequestAndReleasesSlot (0.25s)
=== RUN   TestGateway_KeyLeaseReleasedOnStreamCompletion
    key_concurrency_test.go:132: key lease states = map[], want one released
--- FAIL: TestGateway_KeyLeaseReleasedOnStreamCompletion (0.38s)
=== RUN   TestGateway_KeyLeaseReleasedOnClientCancel
    key_concurrency_test.go:175: released key leases = 0, want 1 (客户端断开必须归还 Key 槽)
--- FAIL: TestGateway_KeyLeaseReleasedOnClientCancel (0.21s)
=== RUN   TestGateway_KeyLeaseReleasedOnFailover
    key_concurrency_test.go:198: released key leases = 0, want 1 (失败切换必须归还 Key 槽)
--- FAIL: TestGateway_KeyLeaseReleasedOnFailover (0.28s)
=== RUN   TestGateway_KeyLeaseReleasedOnStreamAssemblyPanic
    key_concurrency_test.go:232: released key leases = 0, want 1 (panic 守卫必须归还 Key 槽)
--- FAIL: TestGateway_KeyLeaseReleasedOnStreamAssemblyPanic (0.26s)
FAIL	github.com/yunhou/users/internal/inference/gateway	2.609s
```

**Root cause (one missing link, four surfaces).** The column
`inference_api_keys.concurrency_limit` (migration 025) and the field
`domain.APIKey.ConcurrencyLimit` existed, but (1) the management surface
neither exposed nor persisted the field (`postgres.Store.UpdateAPIKey` did not
even write the column — a PATCH silently dropped it), (2) the admission never
read it, so no `api_key`-scope lease was ever taken, and (3) the finalizer
only knew **one** request-scoped lease (`AdmissionResult.AccountLease`), i.e.
a hardcoded "two locks" shape that would have released a Key lease as if it
were the attempt-scoped upstream lease.

#### 4.36 GREEN evidence

The new cases after the implementation — exit code **0**:

```
--- PASS: TestCreateKey_ConcurrencyLimitSemantics (0.00s)
--- PASS: TestUpdateKey_ConcurrencyLimitPresenceSemantics (0.00s)
ok  	github.com/yunhou/users/internal/inference/access	0.646s

--- PASS: TestAdmitTakesKeyLeaseWhenTheKeyCapsConcurrency (0.00s)
--- PASS: TestAdmitAccountAndKeyLeasesCoexist (0.00s)
--- PASS: TestAdmitTakesNoKeyLeaseWhenTheKeyHasNoLimit (0.00s)
--- PASS: TestAdmitKeyLeaseFailureRollsBackEverything (0.00s)
ok  	github.com/yunhou/users/internal/inference/quota	0.622s

--- PASS: TestUserAPIKeys_ConcurrencyLimitCreateReadUpdate (0.23s)
--- PASS: TestUserAPIKeys_ConcurrencyLimitRejectsNonPositive (0.13s)
--- PASS: TestUserAPIKeys_ConcurrencyLimitKeepsOwnerChecks (1.82s)
ok  	github.com/yunhou/users/internal/inference/httpapi	3.598s

--- PASS: TestKeyConcurrencyLimit_SameKeySecondAdmissionIsRejected (0.19s)
--- PASS: TestKeyConcurrencyLimit_DifferentKeysAreIndependent_AccountCapStillApplies (0.15s)
--- PASS: TestKeyConcurrencyLimit_UnlimitedKeyTakesNoLease (0.11s)
--- PASS: TestAPIKeyLeaseScope_IsolatesKeysAndReclaimsAfterTTL (0.13s)
--- PASS: TestKeyConcurrencyLimit_CrashedRequestDoesNotFreezeTheSlot (0.07s)
ok  	github.com/yunhou/users/internal/inference/postgres	1.290s

--- PASS: TestGateway_KeyConcurrencyLimit_BlocksConcurrentRequestAndReleasesSlot (0.17s)
--- PASS: TestGateway_KeyLeaseReleasedOnStreamCompletion (0.13s)
--- PASS: TestGateway_KeyLeaseReleasedOnClientCancel (0.12s)
--- PASS: TestGateway_KeyLeaseReleasedOnFailover (0.12s)
--- PASS: TestGateway_KeyLeaseReleasedOnStreamAssemblyPanic (0.12s)
--- PASS: TestStreamBodyFinish_FinalizeBeforeLeaseRelease (0.00s)
--- PASS: TestStreamBodyFinish_ReleasesEveryRequestLease (0.00s)
--- PASS: TestStreamBodyFinish_PanicStillReleasesLeases (0.00s)
ok  	github.com/yunhou/users/internal/inference/gateway	1.688s
```

The acceptance command (#7) — exit code **0** (final trap-protected run):

```
ok  	github.com/yunhou/users/internal/inference/access	1.615s
ok  	github.com/yunhou/users/internal/inference/accounting	1.590s
ok  	github.com/yunhou/users/internal/inference/catalog	3.009s
ok  	github.com/yunhou/users/internal/inference/credentials	1.866s
ok  	github.com/yunhou/users/internal/inference/domain	1.581s
ok  	github.com/yunhou/users/internal/inference/gateway	10.966s
ok  	github.com/yunhou/users/internal/inference/httpapi	54.484s
ok  	github.com/yunhou/users/internal/inference/management	1.972s
ok  	github.com/yunhou/users/internal/inference/postgres	18.262s
ok  	github.com/yunhou/users/internal/inference/providers	2.126s
ok  	github.com/yunhou/users/internal/inference/providers/connector	1.647s
ok  	github.com/yunhou/users/internal/inference/quota	1.992s
ok  	github.com/yunhou/users/internal/inference/routing	2.215s
ok  	github.com/yunhou/users/internal/inference/workers	6.801s
```

The CI `go-ci` gate (#9) — exit code **0**, 33 packages `ok`, 0 `FAIL`
(including `tests/integration`), and the coverage floor (#10):
`total coverage: 80.2%` ≥ 80%. The e2e job (#11) — exit code **0**:
`ok github.com/yunhou/users/tests/e2e 314.854s`.

#### 4.37 Skips

The verbose inventory run (#8) — exit code **0**:

```
PASS lines: 1277
FAIL lines: 0
1967:--- SKIP: TestQuotaWorkerProcess (0.00s)
3128:--- SKIP: TestLoadDrill_Throughput (0.00s)
3131:--- SKIP: TestLoadDrill_LongConnections (0.00s)
--- skips mentioning postgres/no database ---
(NONE)
```

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; needs `KAYA_QUOTA_WORKER=1` (`quota_concurrency_test.go:773`). The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and passes with real contention output. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1`. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |

**Database-test skips: 0** — identical to the T0 (§2.6), T1 (§4.4), T2
(§4.12), T3 (§4.20) and T4 (§4.29) inventories; the skip lines were grepped
for `postgres` / `no database` / `database unavailable` and returned nothing.

**One transient failure, honestly recorded.** During an *ad-hoc* re-run of
`go test -race -count=1 -p 1 ./internal/inference/gateway ./internal/inference/quota
./internal/inference/access` (a three-package spot check, **not** part of the
battery above) the command reported `FAIL`. The failing test's name was
**lost**: that command's output was piped through `tail` twice, and the outer
`tail -10` cut exactly the lines that carried the failure detail. The command
was then re-run **11 times** (4 × the same three-package combination, 3 × the
gateway package alone, 3 × the gateway package under artificial CPU load with
4 busy loops) — **0 failures** — and the **new** T5 gateway cases were
re-run **20 times** in isolation — **0 failures**. The three battery runs
that include the gateway package (steps 7, 8, 9) were all green. The
transient is therefore recorded as **unreproduced and unattributed**; it was
**not** observed in any run of the T5 acceptance command, of the CI gate or
of the e2e job.

#### 4.38 Migration

**None.** T5 adds no migration: `inference_api_keys.concurrency_limit` has
existed since migration 025 (`CHECK (concurrency_limit IS NULL OR
concurrency_limit > 0)`) — T5 only wires the existing column up to the
management surface and to the admission. No historical migration was
rewritten and no historical ledger row was touched. The disposable databases
were migrated with `cmd/migrate` exactly as in §2.3 (52 files, idempotent on
the second run).

#### 4.39 Files changed

| File | Change |
|---|---|
| `internal/inference/access/apikey.go` | `CreateParams.ConcurrencyLimit`; `KeyPatch.ConcurrencyLimit` + `ClearConcurrency`; the create/update validation and assignment |
| `internal/inference/httpapi/user_api_keys.go` | `keyJSON.ConcurrencyLimit`; `createKeyRequest.ConcurrencyLimit`; the create response; `updateKeyAllowed`; the PATCH presence handling |
| `internal/inference/postgres/apikey_repo.go` | `UpdateAPIKey` now persists `concurrency_limit`; new `GetAPIKeyConcurrencyLimitTx` |
| `internal/inference/quota/service.go` | `Store.GetAPIKeyConcurrencyLimitTx`; `AdmissionResult.Leases` (+ `AccountLease()` / `KeyLease()`); `AccountLeaseTTL` → `RequestLeaseTTL`; new `acquireKeyLeaseTx` used by **both** the plan and the wallet admission |
| `internal/inference/gateway/service.go` | `releaseAdmission(requestID, leases)`; `releaseRequestLeases`; `leaseIDSet`; the dispatch keeper carries every request-scoped lease; the non-stream path splits attempt-scoped vs request-scoped leases by ID |
| `internal/inference/access/apikey_concurrency_test.go` | **new** — create/patch semantics of `concurrency_limit` |
| `internal/inference/httpapi/user_api_keys_concurrency_test.go` | **new** — the wire contract (create/read/patch/null/non-positive/owner) |
| `internal/inference/quota/key_lease_test.go` | **new** — the Key lease in the admission, the account+Key coexistence, the rollback |
| `internal/inference/quota/service_test.go`, `internal/inference/quota/admit_wallet_test.go` | the fake store gained `GetAPIKeyConcurrencyLimitTx` / `keyConcurrencyLimit` / `leaseErrs`; `AccountLease` → `AccountLease()` |
| `internal/inference/postgres/key_concurrency_test.go` | **new** — same-Key rejection + full rollback, per-Key independence with the account cap, no lease for an unlimited key, the `api_key` lease protocol, crash → TTL reclaim |
| `internal/inference/postgres/quota_concurrency_test.go` | `AccountLeaseTTL` → `RequestLeaseTTL`; `AccountLease` → `AccountLease()` |
| `internal/inference/gateway/key_concurrency_test.go` | **new** — the gateway lease lifecycle (concurrent rejection, stream, cancel, failover, panic) |
| `internal/inference/gateway/gateway_test.go` | the fixture gained `withKeyConcurrencyLimit` |
| `internal/inference/gateway/finish_order_test.go` | **new case** `TestStreamBodyFinish_ReleasesEveryRequestLease` (three locks); the two named cases are unchanged |
| `docs/api/kaya-coding-plan.openapi.yaml` | `concurrency_limit` on `ApiKey` / `ApiKeyCreateRequest` / `ApiKeyUpdateRequest`; the three 429 descriptions now name `insufficient_capacity` |
| `docs/api-integration-guide.md` | the Key field table, a new "Key 级并发限制" section, and the `/chat` note |
| `docs/superpowers/plans/2026-11-05-users-model-gateway-completion.md` | the T5 **Interfaces** paragraph now carries the final signatures |
| `docs/runbooks/2026-11-05-model-gateway-verification.md` | this section |

#### 4.40 Notes (root cause, compatibility, honesty)

- **Root cause: the Key's concurrency cap existed in the database and in the
  domain model but was never wired.** The management surface neither exposed
  nor persisted it (`UpdateAPIKey` did not even write the column), the
  admission never read it, and the finalizer knew only **one** request-scoped
  lease — a hardcoded "two locks" shape. The fix wires all three and
  replaces the single field with a **request-level lease list**.
- **The Key limit is a DATABASE fact read inside the admission transaction**
  — exactly like T4's Key RPM limit — so the cap that applies is always the
  one in force at admission time, never a stale authentication snapshot.
- **The Key limit only narrows.** The account-level (policy) cap and the
  upstream-account cap still apply; all of them must hold simultaneously. A
  key without a limit takes **no** lease (no `api_key` row is written at
  all). The shared "one billing account per user" entitlement is unchanged.
- **`budget_micros` keeps its meaning**: a package microcredit **sub-budget**,
  not a cash budget, and unrelated to the concurrency cap.
- **Finalize before lease release is preserved**, including under panic: the
  two named cases (`TestStreamBodyFinish_FinalizeBeforeLeaseRelease`,
  `TestStreamBodyFinish_PanicStillReleasesLeases`) are **unchanged** and
  still pass; the new `TestStreamBodyFinish_ReleasesEveryRequestLease`
  pins the three-lock shape (upstream + account + Key).
- **No lease leaks on any terminal path**: non-stream success, stream
  completion, client cancel, failover exhaustion, and the pre-handoff
  assembly panic all leave **zero** held leases (asserted on the DB rows,
  not on an in-process counter).
- **Recorded boundary — the two "execution state unknown" paths differ in
  their lease disposition** (corrected in the T5 review-fix round; the earlier
  wording "the unknown-execution path keeps its lease" was **too broad**):

  | Path | Trigger | Request | Request-scoped leases (account + Key) |
  |---|---|---|---|
  | **A — dispatch-level unknown** | `providers.DispatchError{ExecutedUnknown: true}` (transport timeout / connection reset / 200-body parse failure) → `dispatchLoop` → `holdForReconciliation` | parked for reconciliation (`reconciliation_required`), the monetary hold is retained | **stay held** — the request is not terminal and the upstream may still be running; the slot is reclaimed by the lease TTL (10 min) |
  | **B — stream interrupted without any usage** | `!tap.SawUsage && !tap.Terminal` → `StreamBody.Finish` → `finalize` → `markReconciliation` | parked for reconciliation (`reconciliation_required`), the monetary hold is retained | **all released** — `Finish`'s `keeper.Stop` releases every lease the keeper holds (upstream + account + Key) |

  Both paths are pinned by tests
  (`TestGateway_DispatchUnknownExecution_KeepsRequestLeases`,
  `TestGateway_StreamInterruptedWithoutUsage_ReleasesRequestLeases`).
  The recovery-side reasoning holds for both: the lease TTL (10 min) is
  shorter than the recovery sweep's grace period (15 min), so a request old
  enough to be scanned has either an expired lease (path A — the live-lease
  guard then lets it through) or no lease at all (path B) — the slot is never
  permanently frozen. The **worker-level** assertion for that is
  `TestRecover_KeyLeaseExpired_ThenSubsequentAdmissionSucceeds` (§4.41). If
  the grace period is ever configured **below** the lease TTL, path A's
  reasoning stops holding and the recovery worker would have to release the
  leases explicitly.
- **No new client-visible error shape.** A Key-level concurrency rejection
  reuses the existing `insufficient_capacity` 429 (`rate_limit_error` on the
  OpenAI/Anthropic protocol surfaces, the management envelope on `/chat`);
  only the OpenAPI 429 descriptions were extended to name it.
- **Compatibility**: `concurrency_limit` is an **optional** field (absent /
  `null` = no Key-level limit) — a client that never sends it sees exactly
  the previous behaviour, including no additional `api_key` lease rows.
  The OpenAPI spec and the integration guide were updated for the new field
  and for the newly reachable `insufficient_capacity` code.

#### 4.41 T5 fix round — the two "unknown execution" paths and a worker-level recovery assertion

Recorded on 2026-11-06, after the T5 commit. The T5 review raised two
**Important** findings; **neither is a behaviour defect** — one is an
over-broad statement, one is missing evidence. This round therefore
**changed no production code**: it corrected the wording, pinned the two
paths with tests, and added the worker-level recovery assertion.

**Environment.** Same recipe as §4.34: a fresh trap-cleaned disposable
PostgreSQL per run, explicit `DATABASE_URL` / `E2E_DATABASE_URL`, `-p 1`,
`-race`, `GOPROXY=off` + `GOFLAGS=-mod=readonly`, `go.mod` / `go.sum`
untouched. **Every command's COMPLETE output was written to
`/tmp/t5-fix-logs/<step>.log` — outside the repository and untruncated** —
so that a failure can always be attributed to a named test (the attribution
gap of §4.37's transient failure can no longer recur).

##### 4.41.1 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no production change, no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1 / run 2 / `-status`) | **0** / **0** / **0** | `applied=52 skipped=0` → `applied=0 skipped=52` → 52 × `✅` (**no migration added**) |
| 4 | **new** `go test -race -count=1 -p 1 -v -run TestRecover_KeyLeaseExpired_ThenSubsequentAdmissionSucceeds ./internal/inference/workers` | **0** | the new worker-level recovery case passes **on the unchanged production code** |
| 5 | **new** `go test -race -count=1 -p 1 -v -run "TestGateway_DispatchUnknownExecution_KeepsRequestLeases\|TestGateway_StreamInterruptedWithoutUsage_ReleasesRequestLeases" ./internal/inference/gateway` | **0** | both path-pinning cases pass on the unchanged production code |
| 6 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/...` | **0** | 14 packages `ok` |
| 7 | **ad-hoc** `go test -race -count=1 -p 1 -v ./internal/inference/gateway ./internal/inference/quota ./internal/inference/access` | **0** | 3 packages `ok`; **no FAIL / panic / DATA RACE**; the complete output is in `/tmp/t5-fix-logs/adhoc_gateway_quota_access.log` (555 lines, untruncated) |
| 8 | `go test -race -count=1 -p 1 -v ./internal/inference/... ./tests/integration/...` | **0** | skip inventory — see §4.41.3 |

Command #6 output:

```
ok  	github.com/yunhou/users/internal/inference/access	1.754s
ok  	github.com/yunhou/users/internal/inference/accounting	2.155s
ok  	github.com/yunhou/users/internal/inference/catalog	9.518s
ok  	github.com/yunhou/users/internal/inference/credentials	3.652s
ok  	github.com/yunhou/users/internal/inference/domain	1.724s
ok  	github.com/yunhou/users/internal/inference/gateway	39.268s
ok  	github.com/yunhou/users/internal/inference/httpapi	185.895s
ok  	github.com/yunhou/users/internal/inference/management	4.220s
ok  	github.com/yunhou/users/internal/inference/postgres	67.461s
ok  	github.com/yunhou/users/internal/inference/providers	2.479s
ok  	github.com/yunhou/users/internal/inference/providers/connector	1.762s
ok  	github.com/yunhou/users/internal/inference/quota	1.700s
ok  	github.com/yunhou/users/internal/inference/routing	1.928s
ok  	github.com/yunhou/users/internal/inference/workers	13.362s
```

(The wall-clock numbers are inflated — `httpapi` 185 s against the usual
~55 s — because an unrelated `cargo test --workspace` was running on the same
machine during this run; the results are unaffected.)

Command #7 output (the ad-hoc three-package check):

```
ok  	github.com/yunhou/users/internal/inference/gateway	17.489s
ok  	github.com/yunhou/users/internal/inference/quota	1.624s
ok  	github.com/yunhou/users/internal/inference/access	1.671s
```

##### 4.41.2 Finding 1 — the two "execution state unknown" paths

The previous wording ("the unknown-execution path keeps its lease") was
**too broad**. The code has **two** distinct paths and their lease
dispositions differ:

| Path | Trigger | Request | Request-scoped leases (account + Key) |
|---|---|---|---|
| **A — dispatch-level unknown** | `providers.DispatchError{ExecutedUnknown: true}` (transport timeout / connection reset / 200-body parse failure) → `dispatchLoop` → `holdForReconciliation` | parked for reconciliation (`reconciliation_required`), the monetary hold is retained | **stay held** — the request is not terminal and the upstream may still be running; the slot is reclaimed by the lease TTL (10 min) |
| **B — stream interrupted without any usage** | `!tap.SawUsage && !tap.Terminal` → `StreamBody.Finish` → `finalize` → `markReconciliation` | parked for reconciliation (`reconciliation_required`), the monetary hold is retained | **all released** — `Finish`'s `keeper.Stop` releases every lease the keeper holds (upstream + account + Key) |

Both are now **pinned** (command #5), and the wording in §4.40, in the task
report (§2.4 / §5.1 / §10) was corrected to match:

```
=== RUN   TestGateway_DispatchUnknownExecution_KeepsRequestLeases
--- PASS: TestGateway_DispatchUnknownExecution_KeepsRequestLeases (1.22s)
=== RUN   TestGateway_StreamInterruptedWithoutUsage_ReleasesRequestLeases
--- PASS: TestGateway_StreamInterruptedWithoutUsage_ReleasesRequestLeases (0.43s)
ok  	github.com/yunhou/users/internal/inference/gateway	3.529s
```

**No production behaviour was changed** — the two cases describe what the
code already does; the finding was about the statement, not the behaviour.

##### 4.41.3 Finding 2 — the worker-level recovery assertion

The brief's "恢复" requirement was previously only **argued** (lease TTL
10 min < recovery grace 15 min ⇒ a scanned request's lease has expired).
It is now **pinned at the worker level** by
`internal/inference/workers/key_concurrency_recovery_test.go`
(`TestRecover_KeyLeaseExpired_ThenSubsequentAdmissionSucceeds`), against the
**real recovery worker + the real store + a disposable PostgreSQL**:

1. a real admission (the `quota` service over the real store) commits the
   request row, the window / Key-budget holds and **one `api_key`-scope
   lease** (the Key's `concurrency_limit = 1`; the lease TTL is compressed to
   50 ms so that "the crashed request's lease aged out" needs no wall-clock
   wait);
2. the lease is waited out → it is **no longer a live authorization**;
3. the **real** `SettlementRecovery` worker runs one pass — the live-lease
   guard (`state='held' AND expires_at > now()`) lets the request through
   and it is recovered;
4. a **subsequent admission on the same Key succeeds** — the expired lease
   is reclaimed at acquisition time (`state='expired'`) and the new lease is
   `held`.

```
=== RUN   TestRecover_KeyLeaseExpired_ThenSubsequentAdmissionSucceeds
2026/11/06 13:16:15 inference recovery: request 651c5da3-... released — zero upstream consumption proven (audit: reservations released)
2026/11/06 13:16:15 inference_recovery pass: scanned=1 released_not_sent=1 settled_conservative=0 settled_verified=0 racing_skipped=0 jobs_processed=0 jobs_settled=0 jobs_awaiting_evidence=0 jobs_no_evidence=0 escalated=0 backlog=0 unknown_usage=0 dangling_holds=0 open_requests=0 settlement_lag_s=0 ledger_mismatches=0 errors=0
--- PASS: TestRecover_KeyLeaseExpired_ThenSubsequentAdmissionSucceeds (0.58s)
ok  	github.com/yunhou/users/internal/inference/workers	6.105s
```

**This case passes against the unchanged production code** — the existing
TTL/grace mechanism already guarantees it. It is therefore recorded as
**protection evidence**, exactly as the finding asked: *no code change was
manufactured for it*.

##### 4.41.4 Skips

The verbose inventory run (command #8) — exit code **0**:

```
PASS lines: 1280
FAIL lines: 0
1971:--- SKIP: TestQuotaWorkerProcess (0.00s)
3135:--- SKIP: TestLoadDrill_Throughput (0.00s)
3138:--- SKIP: TestLoadDrill_LongConnections (0.00s)
--- skips mentioning postgres / no database ---
(NONE)
```

**Database-test skips: 0** — unchanged from §4.37 (the three skips are the
same `KAYA_QUOTA_WORKER=1` / `KAYA_LOAD=1` gates as in the T0 baseline).

##### 4.41.5 Files changed (fix round)

| File | Change |
|---|---|
| `internal/inference/workers/key_concurrency_recovery_test.go` | **new** — the worker-level recovery assertion (real worker + real store + disposable PG) |
| `internal/inference/gateway/key_concurrency_test.go` | **new cases** — the two "execution state unknown" paths' lease disposition |
| `docs/runbooks/2026-11-05-model-gateway-verification.md` | §4.40's over-broad bullet corrected into the two-path table; this section |
| `.superpowers/sdd/…/task-5-report.md` | §2.4 / §5.1 / §10 corrected; §11 added (the report is outside version control) |

**Production code: unchanged.** The fix round touched **tests and
documentation only**.

### T6 — Protocol error shape and Retry-After

Recorded on 2026-11-06, base SHA `6daf677` (T5 branch tip). Task report:
`.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-6-report.md`.

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the
instance down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`; every
database-backed command used `-p 1` and `-race`; `go.mod` / `go.sum` were
never modified (`git diff --stat go.mod go.sum` → empty after every run).

#### 4.24 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1 / run 2, `t6_app`) | **0** / **0** | `applied=52 skipped=0` → `applied=0 skipped=52` (idempotent) |
| 4 | **RED** `go test -count=1 -run 'TestSetup_V1PeripheralRateLimitUsesProtocolNativeError' -v ./internal/router` | **1** | the management envelope leaked onto `/v1` — see §4.25 |
| 5 | **RED** `go test -count=1 -run 'TestMapGatewayError' -v ./internal/service` | **1** | `/chat` degraded local limits to upstream wording / generic 502 — see §4.25 |
| 6 | **RED** `DATABASE_URL=<disposable> go test -count=1 -run 'TestV1ProtocolMatrix' -v ./internal/inference/httpapi` | **1** | the permanent Key budget produced a fabricated `Retry-After` — see §4.25 |
| 7 | **GREEN** the same commands as #4/#5/#6 | **0** | all T6 cases pass |
| 8 | **GREEN** `go test -count=1 -run 'TestAdmitQuotaExceeded' -v ./internal/inference/quota` | **0** | the Key-budget probe cases pass |
| 9 | **GREEN** `go test -count=1 -run 'TestChatHandler_LocalLimitEnvelope' -v ./internal/handler` | **0** | the `/chat` envelope case passes |
| 10 | **GREEN** `DATABASE_URL=<disposable> go test -count=1 -run 'TestV1AuthChain_NativeErrorShapePerEntrypoint' -v ./internal/inference/httpapi` | **0** | the auth chain's native shape per entrypoint passes |
| 11 | **acceptance（任务书原文）** `go test -race -count=1 -p 1 ./internal/middleware ./internal/router ./internal/inference/httpapi ./internal/service` | **0** | 4 packages `ok` (13.119s / 2.979s / 75.317s / 25.596s) — and re-run on the **committed** tree in the resumed session: exit **0** (router 3.318s / httpapi 68.150s / service 29.549s); see §4.24.1 |
| 12 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 13 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.3%` ≥ the 80% floor (T5 80.2%, T4 80.1%, T3 80.4%, T0 80.4%) |
| 14 | `go test -race -count=1 ./tests/e2e/...` (`E2E_DATABASE_URL` → `t6_e2e`) | **1** → **0** | the `go-e2e` job found ONE contract conflict — the facade's money-quota 429 still wore the upstream wording (§4.25.5); after the deliberate contract update it is green |

#### 4.24.1 Run provenance — T6 ran across two sessions

T6 was implemented across **two sessions**: the first was cut off by a usage
limit (2056) **before** the commit, the second resumed it. This note records
which session produced which evidence, so that no result is attributed to a
command that did not run.

| Phase | Session | What actually ran | Result |
|---|---|---|---|
| RED | A (interrupted) | `go test -count=1 -run 'TestSetup_V1PeripheralRateLimitUsesProtocolNativeError' -v ./internal/router`; `go test -count=1 -run 'TestMapGatewayError' -v ./internal/service`; `DATABASE_URL=<disposable> go test -count=1 -run 'TestV1ProtocolMatrix' -v ./internal/inference/httpapi` | exit **1** each — the behavioural failures of §4.25 (the DB-backed run on a fresh trap-cleaned disposable PostgreSQL) |
| GREEN (targeted) | A | the same three commands after the fix, plus `TestAdmitQuotaExceeded*` (quota), `TestChatHandler_LocalLimitEnvelope` (handler) and `TestV1AuthChain_NativeErrorShapePerEntrypoint` (httpapi) | exit **0** each |
| acceptance | A | `go test -race -count=1 -p 1 ./internal/middleware ./internal/router ./internal/inference/httpapi ./internal/service` — on the tree that already carried **every** production change | exit **0** (4 packages `ok`) |
| CI gate (1st) | A | `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | exit **0**, 33 packages `ok`, coverage **80.2%** |
| `go-e2e` (1st + diagnostic) | A | `go test -race -count=1 ./tests/e2e/...`, then the same with `-v` and a full log | exit **1** — the client-contract conflict of §4.25.5; the diagnostic run confirmed **exactly one** failing test (`TestChatContract_FacadeQuotaExceededIs429`), the rest of the suite passed |
| **interruption** | — | the usage limit stopped the session **before** the commit: the contract conflict was diagnosed but **not yet fixed**, and the final tree's CI / e2e completion was **not** confirmed at that point | — |
| contract update | B (resumed) | `tests/e2e/chat_gateway_test.go` expectation updated (§4.25.5) | — |
| `go-e2e` (final) | B | `go test -race -count=1 ./tests/e2e/...` on the **final** tree | exit **0**, `ok … tests/e2e 286.763s` |
| CI gate (final) | B | the same `go-ci` command on the **final** tree (the only later edits were a comment inside a test file and documentation) | exit **0**, 33 packages `ok`, coverage **80.3%** |
| acceptance (final) | B | the exact acceptance command on the **committed** tree | exit **0** (router 3.318s / httpapi 68.150s / service 29.549s) |
| post-commit targeted | B | `go test -race -count=1 -run 'TestV1ProtocolMatrix|TestV1AuthChain' ./internal/inference/httpapi` and `go test -race -count=1 -run 'TestSetup_V1PeripheralRateLimitUsesProtocolNativeError' ./internal/router` on the committed tree | exit **0** each |
| migrate idempotency | A + B | `go run ./cmd/migrate` twice per disposable instance | exit **0**, `applied=52 skipped=0` → `applied=0 skipped=52` |
| lint / build | A + B | `go vet ./internal/... ./cmd/...`, `go build ./...` | exit **0** |

**What is *not* claimed.** The pre-interruption session's numbers above are
the ones it actually printed; the final tree's numbers are the ones session B
produced after the contract update. No pre-interruption command is reported
as having completed on the final tree, and the final acceptance invocation
on the committed tree **was** actually re-run (row "acceptance (final)"), so
the committed state is verified by the acceptance command itself, by the
final CI gate, by the `go-e2e` job and by the post-commit targeted cases.

#### 4.25 RED evidence (before any production edit)

Command #4 ran against the **unmodified** production code at `6daf677`; the
new tests are written against the existing API surface only, so every failure
is a behavioural assertion failure, never a compile error.

##### 4.25.1 The perimeter IP 429 forced the management envelope onto `/v1`

```
=== RUN   TestSetup_V1PeripheralRateLimitUsesProtocolNativeError//v1/chat/completions
    router_test.go:1165: management envelope leaked into /v1/chat/completions: {"code":429,"error":"rate_limited","message":"too many requests"}
    router_test.go:1169: /v1/chat/completions: the native error object is missing: {"code":429,"error":"rate_limited","message":"too many requests"}
=== RUN   TestSetup_V1PeripheralRateLimitUsesProtocolNativeError//v1/messages
    router_test.go:1165: management envelope leaked into /v1/messages: {"code":429,"error":"rate_limited","message":"too many requests"}
    router_test.go:1169: /v1/messages: the native error object is missing: {"code":429,"error":"rate_limited","message":"too many requests"}
=== RUN   TestSetup_V1PeripheralRateLimitUsesProtocolNativeError//v1/responses
    router_test.go:1165: management envelope leaked into /v1/responses: {"code":429,"error":"rate_limited","message":"too many requests"}
    router_test.go:1169: /v1/responses: the native error object is missing: {"code":429,"error":"rate_limited","message":"too many requests"}
--- FAIL: TestSetup_V1PeripheralRateLimitUsesProtocolNativeError (0.00s)
    --- FAIL: TestSetup_V1PeripheralRateLimitUsesProtocolNativeError//v1/chat/completions (0.00s)
    --- FAIL: TestSetup_V1PeripheralRateLimitUsesProtocolNativeError//v1/messages (0.00s)
    --- FAIL: TestSetup_V1PeripheralRateLimitUsesProtocolNativeError//v1/responses (0.00s)
FAIL	github.com/yunhou/users/internal/router	3.481s
```

The generic limiter wrote `{"code":429,"error":"rate_limited",…}` — a
top-level management `code` and a **string** `error` where the OpenAI shape
needs an error **object**, and where the Anthropic shape needs the
`{"type":"error",…}` envelope.

##### 4.25.2 `/chat` degraded the local limits

```
=== NAME  TestMapGatewayError_LocalLimitsAreNotUpstreamErrors/insufficient_balance
    chat_facade_test.go:755: a local usage limit must not be degraded to the generic upstream 502: chat upstream error
    chat_facade_test.go:758: mapGatewayError = chat upstream error, want a 429-class limit error
    chat_facade_test.go:761: the message must not blame the upstream for a local limit: "chat upstream error"
=== NAME  TestMapGatewayError_LocalLimitsAreNotUpstreamErrors/quota_exceeded
    chat_facade_test.go:761: the message must not blame the upstream for a local limit: "chat upstream rate limit exceeded"
=== NAME  TestMapGatewayError_LocalLimitsAreNotUpstreamErrors/quota_exceeded_window
    chat_facade_test.go:761: the message must not blame the upstream for a local limit: "chat upstream rate limit exceeded"
=== NAME  TestMapGatewayError_LocalLimitsAreNotUpstreamErrors/rate_limited
    chat_facade_test.go:761: the message must not blame the upstream for a local limit: "chat upstream rate limit exceeded"
=== NAME  TestMapGatewayError_LocalLimitsAreNotUpstreamErrors/typed_rate_limited
    chat_facade_test.go:761: the message must not blame the upstream for a local limit: "chat upstream rate limit exceeded"
=== NAME  TestMapGatewayError_LocalLimitsAreNotUpstreamErrors/insufficient_capacity
    chat_facade_test.go:761: the message must not blame the upstream for a local limit: "chat upstream rate limit exceeded"
--- FAIL: TestMapGatewayError_LocalLimitsAreNotUpstreamErrors (0.00s)
FAIL	github.com/yunhou/users/internal/service	0.514s
```

`insufficient_balance` was a **502 `chat upstream error`**; the other four
local classes were 429 but with the upstream wording.

**Protection evidence.** The pre-existing case
`upstream_429_exhausted → ErrChatRateLimited` (a real upstream 429) passed at
the baseline and still passes — the upstream wording is reserved for upstream
rejections.

##### 4.25.3 The permanent Key budget produced a fabricated countdown

```
=== RUN   TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown/chat_completions
    protocol_error_edges_test.go:403: the permanent blocker must be named: {"error":{"code":"quota_exceeded","message":"quota exceeded: blocked by five_hour; deficit_micros=16404","type":"rate_limit_error"}}
    protocol_error_edges_test.go:406: Retry-After = "18000", want none (Key 预算耗尽是永久阻断项)
=== RUN   TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown/messages
    protocol_error_edges_test.go:403: the permanent blocker must be named: {"error":{"message":"quota exceeded: blocked by five_hour; deficit_micros=148","type":"rate_limit_error"},"type":"error"}
    protocol_error_edges_test.go:406: Retry-After = "18000", want none (Key 预算耗尽是永久阻断项)
=== RUN   TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown/responses
    protocol_error_edges_test.go:403: the permanent blocker must be named: {"error":{"code":"quota_exceeded","message":"quota exceeded: blocked by five_hour; deficit_micros=16404","type":"rate_limit_error"}}
    protocol_error_edges_test.go:406: Retry-After = "18000", want none (Key 预算耗尽是永久阻断项)
--- FAIL: TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown (0.90s)
    --- FAIL: TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown/chat_completions (0.27s)
    --- FAIL: TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown/messages (0.23s)
    --- FAIL: TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown/responses (0.40s)
FAIL	github.com/yunhou/users/internal/inference/httpapi	8.665s
```

`Retry-After: 18000` (five hours — the window's reset) for a request whose
Key budget is permanently exhausted, and the message never mentioned the
permanent blocker.

##### 4.25.4 Protection evidence — the other four limit classes were already right

In the same RED run, the business rate / TPM / concurrency / money-quota
matrix cells **passed at the baseline** on all three entrypoints (native
shape, business code, zero upstream calls, `Retry-After` rules). They are
recorded as guards, not as new behaviour:

```
--- PASS: TestV1ProtocolMatrix_BusinessRateLimit (1.22s)
    --- PASS: …/chat_completions (0.41s)   --- PASS: …/messages (0.39s)   --- PASS: …/responses (0.42s)
--- PASS: TestV1ProtocolMatrix_BusinessTokenLimit (1.48s)
--- PASS: TestV1ProtocolMatrix_ConcurrencyLimit (1.79s)
--- PASS: TestV1ProtocolMatrix_MoneyQuota (1.47s)
```

So the defects were exactly three: the perimeter limiter's shape, the
`/chat` mapping, and the permanent blocker's countdown.

#### 4.25.5 The e2e job caught a contract conflict the unit tests could not

The first `go-e2e` run (command #14, exit **1**) failed with exactly one
test — and it is the `go-e2e` job's value: it pins the **client-visible**
`/chat` envelope byte-exactly, which no unit test does.

```
=== RUN   TestChatContract_FacadeQuotaExceededIs429
2026/11/06 14:21:31 chat: request rejected by a usage limit: chat request rejected by a usage limit (quota_exceeded)
    chat_gateway_test.go:1657: quota envelope = {"code":429,"data":null,"message":"chat request rejected by a usage limit (quota_exceeded)"}, want byte-exact legacy 429 shape {"code":429,"data":null,"message":"chat upstream rate limit exceeded"}
--- FAIL: TestChatContract_FacadeQuotaExceededIs429 (1.17s)
```

Every other e2e case passed (the full inventory is in the run log).

**The conflict.** The pre-existing e2e contract pinned the facade's *money
quota* 429 to the byte-exact legacy **upstream** wording
(`chat upstream rate limit exceeded`) — its own comment said "同一 client
envelope，不同触发源" (same envelope, different trigger). T6 requires the
`/chat` limits to carry an **accurate** business code/description (设计 §9.1):
a quota exhaustion is a **local** rejection whose request never left, and
telling the client "upstream rate limit exceeded" invites a back-off retry
loop that can never succeed.

**The resolution (deliberate, minimal, documented).** The facade's local
limit classes now answer `{"code":429,"data":null,"message":"chat request
rejected by a usage limit (<业务码>)"}`, and the single e2e expectation was
updated to match. What did **not** change:

- the envelope **shape** (`{code,data,message}`) and the status code (429);
- the **upstream** 429 wording — `chat upstream rate limit exceeded` stays
  byte-identical, and `caseChatContractUpstreamErrors/upstream_rate_limited`
  (which runs for the legacy **and** the facade backend) still pins it;
- the legacy backend entirely;
- the upstream-429 path — the only error class the facade shares with the
  legacy backend.

**Scope of the change.** The e2e conflict is about the *wording* of the
facade's **money-quota** class. The facade's local-limit classes changed
**four** in total — `rate_limited`, `insufficient_capacity`,
`quota_exceeded` (message only) and `insufficient_balance` (which also
moved from a generic **502** to **429**); the full table is in §4.31.

The integration guide's `/chat` error table was updated in the same change
(§4.30), so the documentation now matches the code: the message
`chat upstream rate limit exceeded` is documented as the **upstream** 429
only.

#### 4.26 GREEN evidence

Commands #7–#10 re-run after the fix — exit code **0**:

```
--- PASS: TestSetup_V1PeripheralRateLimitUsesProtocolNativeError (0.00s)
    --- PASS: …//v1/chat/completions (0.00s)
    --- PASS: …//v1/messages (0.00s)
    --- PASS: …//v1/responses (0.00s)
--- PASS: TestMapGatewayError_LocalLimitsAreNotUpstreamErrors (0.00s)
    --- PASS: …/insufficient_balance (0.00s)   --- PASS: …/quota_exceeded (0.00s)
    --- PASS: …/quota_exceeded_window (0.00s)  --- PASS: …/rate_limited (0.00s)
    --- PASS: …/typed_rate_limited (0.00s)     --- PASS: …/insufficient_capacity (0.00s)
--- PASS: TestV1ProtocolMatrix_BusinessRateLimit (0.46s)
--- PASS: TestV1ProtocolMatrix_BusinessTokenLimit (0.40s)
--- PASS: TestV1ProtocolMatrix_ConcurrencyLimit (0.50s)
--- PASS: TestV1ProtocolMatrix_MoneyQuota (0.43s)
--- PASS: TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown (0.42s)
--- PASS: TestAdmitQuotaExceededReportsExhaustedKeyBudget (0.00s)
--- PASS: TestAdmitQuotaExceededKeepsWindowOnlyWhenKeyBudgetIsHealthy (0.00s)
--- PASS: TestChatHandler_LocalLimitEnvelope (0.00s)
--- PASS: TestV1AuthChain_NativeErrorShapePerEntrypoint (0.20s)
```

Acceptance command #11 — exit code **0**:

```
ok  	github.com/yunhou/users/internal/middleware	13.119s
ok  	github.com/yunhou/users/internal/router	2.979s
ok  	github.com/yunhou/users/internal/inference/httpapi	75.317s
ok  	github.com/yunhou/users/internal/service	25.596s
```

CI `go-ci` gate (command #12) — exit code **0**, 33 packages `ok`, 0 `FAIL`
(including `tests/integration`); coverage floor `total coverage: 80.3%` ≥ 80%.

#### 4.27 The protocol matrix (T6 deliverable)

Every cell asserts: status, JSON `Content-Type`, the **protocol-native
shape**, the business code, **zero upstream calls** and the `Retry-After`
rule.

| Limit class | `/v1/chat/completions`, `/v1/responses` (OpenAI) | `/v1/messages` (Anthropic) | Upstream calls |
|---|---|---|---|
| Perimeter per-IP 429 | `{"error":{"type":"rate_limit_error","code":"rate_limited","message":"too many requests"}}` | `{"type":"error","error":{"type":"rate_limit_error","message":"too many requests"}}` | 0 (rejected before auth) |
| Business RPM 429 | `error.code=rate_limited`, `error.type=rate_limit_error`, `Retry-After` present | `error.type=rate_limit_error`, message `rate limit exceeded: blocked by key_rpm` | 0 |
| Business TPM 429 | `error.code=rate_limited`, message names `policy_tpm`, **no** `Retry-After` (recovery not determinable) | same | 0 |
| Concurrency 429 | `error.code=insufficient_capacity` | message names `concurrency limit` | 0 (the second request never leaves) |
| Money quota 429 | `error.code=quota_exceeded`, message names the window, `Retry-After` present | same (no `code` field in the native shape) | 0 |
| Permanent Key budget **+** window | `error.code=quota_exceeded`, message contains `key budget exhausted`, **no** `Retry-After` | same | 0 |
| Auth 401 | `error.type=authentication_error`, `error.code=invalid_api_key` | `error.type=authentication_error` | 0 |

`/chat` keeps its existing `{code,data,message}` envelope; the local limits
become 429 with the business code in the `message` (the envelope has no
separate symbol field):

| Limit class | `/chat` answer |
|---|---|
| Business rate / concurrency / quota / balance | `{"code":429,"data":null,"message":"chat request rejected by a usage limit (<rate_limited|insufficient_capacity|quota_exceeded|insufficient_balance>)"}` |
| Upstream 429 | `{"code":429,"data":null,"message":"chat upstream rate limit exceeded"}` — **byte-identical to legacy** |

#### 4.27.1 One run failed for an environmental reason (recorded, not hidden)

A **later** re-run of the same suite (the one that re-measured the skip
inventory) reported `20` `--- FAIL` lines **and** the trap printed
`[cleanup] pg_ctl stop non-zero` — i.e. the disposable PostgreSQL instance
was **gone** while the tests were running. The host was under heavy
memory/swap pressure at that moment (a concurrent `cargo test --workspace`
from an unrelated project; `vm.swapusage` showed 17.3 GiB of 18.9 GiB used
and the data volume at 98%), and the disposable instance died with it.

The suite was immediately re-run on a **fresh** instance (this time keeping
the PostgreSQL server log for diagnosis):

```
ok  	github.com/yunhou/users/internal/inference/access	1.743s
ok  	github.com/yunhou/users/internal/inference/accounting	1.693s
ok  	github.com/yunhou/users/internal/inference/catalog	3.835s
ok  	github.com/yunhou/users/internal/inference/credentials	2.116s
ok  	github.com/yunhou/users/internal/inference/domain	1.748s
ok  	github.com/yunhou/users/internal/inference/gateway	13.919s
ok  	github.com/yunhou/users/internal/inference/httpapi	79.741s
ok  	github.com/yunhou/users/internal/inference/management	2.080s
ok  	github.com/yunhou/users/internal/inference/postgres	21.073s
ok  	github.com/yunhou/users/internal/inference/providers	2.414s
ok  	github.com/yunhou/users/internal/inference/providers/connector	1.754s
ok  	github.com/yunhou/users/internal/inference/quota	1.677s
ok  	github.com/yunhou/users/internal/inference/routing	2.189s
ok  	github.com/yunhou/users/internal/inference/workers	10.304s
[cleanup] pg_ctl stop ok
```

**0 `FAIL` lines.** The preserved `server.log` contains only the expected
constraint violations of the tests that exercise error paths (duplicate
`inference_entitlements`, `inference_wallet_entries`, a provider FK
violation, …) — no crash, no restart, no `PANIC`. Conclusion: the 20 FAIL
lines were the **database dying**, not a code regression. The authoritative
green numbers for T7 are the ones in §4.25 (command #14) and in this
re-run.

#### 4.27.2 Count reconciliation — the T7 numbers vs the T0–T6 baseline

**The discrepancy was the counting method, not missing tests.** The baseline
numbers recorded by T0–T6 ("1109 PASS / 1 SKIP", "1154 PASS", "1168 PASS",
"1171 PASS" for the inference suite and "52 PASS / 2 SKIP" for the
integration suite) are **line counts of the verbose output**: every line that
carries a `--- PASS` marker (top-level test lines **plus** subtest lines).
T7's first run reported "1032 PASS / 1 SKIP" (inference) and "35 PASS /
2 SKIP" (integration) because that run's filter was `^--- PASS` — **top-level
lines only**. Same suites, narrower filter.

To replace the assertion with a measurement, both trees were run **in one
session, on one fresh disposable PostgreSQL, with one documented counting
method**: the base `9ffbe85` (a separate `git worktree`, pristine — no T7
code) and the T7 tree.

| Suite | Metric | Base `9ffbe85` | T7 (fixed tree) | Delta |
|---|---|---|---|---|
| `./internal/inference/...` | top-level PASS lines (`^--- PASS`) | 1015 | 1033 | **+18** |
| | subtest PASS lines (`^[[:space:]]+--- PASS`) | 241 | 244 | **+3** |
| | **PASS lines (the baseline method)** | **1256** | **1277** | **+21** |
| | PASS occurrences (`grep -o -- '--- PASS'`) | 1256 | 1277 | +21 (no line carries two markers) |
| | FAIL | **0** | **0** | 0 |
| | SKIP | 1 (`TestQuotaWorkerProcess`) | 1 (same one) | 0 |
| | packages `ok` | 14 | 14 | 0 |
| `./tests/integration/...` | top-level PASS lines | 35 | 35 | 0 |
| | subtest PASS lines | 17 | 17 | 0 |
| | **PASS lines (the baseline method)** | **52** | **52** | **0** |
| | FAIL / SKIP | 0 / 2 (the two load drills) | 0 / 2 (the same two) | 0 |

**The integration suite reproduces the recorded baseline number `52`
exactly** (35 top-level + 17 subtest lines) on **both** trees — that is the
proof that the baseline method is the "all PASS lines" one, and that T7's
earlier "35" was the top-level-only filter rather than a lost suite.

**The inference delta is +21 lines, and every one of them is a T7 case** —
so no pre-existing test was lost, renamed or skipped (T1–T6 preservation is
*measured*, not asserted):

| # | New top-level test | Package |
|---|---|---|
| 1 | `TestValidateRoute_PoolStrategySupportedSet` | catalog |
| 2 | `TestPublishedSnapshot_LegacyPoolStrategyStaysReadableButUnavailable` | catalog |
| 3 | `TestPublish_RejectsUnsupportedPoolStrategyAndNamesTheObject` | catalog |
| 4 | `TestPublish_SupportedPoolStrategyStillPublishes` | catalog |
| 5 | `TestImportEnvCatalog_DerivedRouteUsesSupportedPoolStrategy` | catalog |
| 6 | `TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause` (fix round) | catalog |
| 7 | `TestQuotaPolicy_ClampIfDeclaredRejectedOnWrite` | management |
| 8 | `TestQuotaPolicy_LegacyClampIfDeclaredStaysQueryable` | management |
| 9 | `TestBulkImport_DerivedRouteStaysInsideTheSupportedPoolStrategySet` | management |
| 10 | `TestApplyBulkImportTx_DerivedRouteUsesSupportedPoolStrategy` | postgres |
| 11 | `TestCandidates_UnsupportedPoolStrategyIsNotACandidate` | routing |
| 12 | `TestCandidates_WeightOrdersWithinEqualPriorityAndNeverSplits` | routing |
| 13 | `TestDispatch_ConnectTimeoutBoundsConnectionEstablishment` | providers |
| 14 | `TestDispatch_ConnectTimeoutDoesNotTruncateEstablishedStream` | providers |
| 15 | `TestDispatch_ConnectTimeoutIsUnknownExecution` | providers |
| 16 | `TestGateway_UnsupportedPoolStrategyFailsBeforeAdmission` | gateway |
| 17 | `TestGateway_DisabledLegacyRouteDoesNotBlockTheCall` | gateway |
| 18 | `TestGateway_ClampIfDeclaredPolicyFailsClearly` | gateway |

plus **3 subtest lines** — the three cases of
`TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause`
(`weight_zero_keeps_its_own_cause`, `empty_model_keeps_its_own_cause`,
`unsupported_strategy_names_the_replacement`).

18 + 3 = **21 = the measured delta exactly**. (The new
`mapGatewayError` case lives in `internal/service`, which is **not** part of
`./internal/inference/...` — it is covered by the CI gate, which runs
`./internal/...`.)

For reference, the recorded baseline progression on the same method is T0
1088 → T1 1109 → T2 1154 → T3 1168 → T4 1171 → (T5 + T6) **1256 at the
base `9ffbe85`** → **1277 with T7**.

#### 4.28 Skips

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; needs `KAYA_QUOTA_WORKER=1` (`quota_concurrency_test.go:773`). The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and passes with real contention output. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1`. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |
| 4 | `TestUsageTokenAcceptance_TenPagesTwoMinutes` | `tests/e2e` | Explicit non-default gate `USAGE_ACCEPTANCE=1` (`usage_tokens_test.go:226`) — a two-minute dashboard load drill, not a correctness test. | No |

**Database-test skips: 0** — identical to the T0 (§2.6), T1 (§4.4), T2
(§4.12), T3 (§4.20) and T4/T5 inventories. The `go-e2e` job's only skip is
the `USAGE_ACCEPTANCE=1` drill above. One honest note: running the new
auth-chain case **without** `DATABASE_URL` produced `--- SKIP` (the fixture's
silent fallback) — that run was discarded and the case re-run on the
disposable instance, where it passed. A skipped database test is not a pass.

#### 4.29 Migration

**None.** T6 adds no migration: the only schema facts it reads
(`inference_api_keys.budget_limit_micros` / `budget_used_micros`) have been
there since migration 025. The disposable databases were migrated with
`cmd/migrate` exactly as in §2.3 (52 files, idempotent on the second run).

#### 4.30 Files changed

| File | Change |
|---|---|
| `internal/middleware/ratelimit.go` | new `RateLimitResponder` + `RateLimitWithResponder` / `RateLimitWithKeyResponder`; `RateLimit` / `RateLimitWithKey` become the default (management envelope) combinations |
| `internal/inference/httpapi/apikey_auth.go` | new `V1RateLimitResponder` (reuses the existing `v1Error` / `anthropicError` native helpers) + the shared `isAnthropicV1Path` predicate |
| `internal/router/router.go` | the `/v1` group's perimeter limiter is injected with `httpapi.V1RateLimitResponder` (rate/burst/bucket unchanged) |
| `internal/inference/httpapi/chat_completions.go` | `quotaExceededDetails`: an exhausted Key budget ⇒ **no** `Retry-After` (it is a permanent blocker) |
| `internal/inference/quota/service.go` | `Store` gains `GetAPIKeyBudgetTx`; new `withKeyBudgetBlock` reports the permanent blocker together with the window blocks |
| `internal/inference/postgres/apikey_repo.go` | implements `GetAPIKeyBudgetTx` (a database fact read inside the admission transaction) |
| `internal/service/errors.go` | new `ErrChatRequestLimited` + `ChatLimitError` (unwraps to both the new sentinel and `ErrChatRateLimited`) |
| `internal/service/chat_facade.go` | `mapGatewayError`: local limits → `ChatLimitError` (429 + business code); the upstream 429 keeps `ErrChatRateLimited` |
| `internal/handler/chat.go` | `chatErrorMapping` gains the `ErrChatRequestLimited` branch (429 + accurate wording) |
| `internal/router/router_test.go` | the perimeter-429 native-shape case + the `setupV1Router` helper |
| `internal/inference/httpapi/protocol_error_edges_test.go` | the protocol matrix (5 groups × 3 entrypoints) + assertion helpers |
| `internal/inference/httpapi/apikey_auth_edges_test.go` | the auth chain's native shape per entrypoint |
| `internal/inference/httpapi/protocol_surfaces_test.go` | **fixture repair**: `newProtoFixture` now mounts `/v1/chat/completions` too (the matrix needs the three entrypoints on one engine) |
| `internal/inference/quota/service_test.go` | the Key-budget probe cases (exhausted / sufficient / no budget) + the fakeStore's new method |
| `internal/service/chat_facade_test.go` | the `/chat` local-limit mapping case |
| `internal/handler/chat_test.go` | the `/chat` local-limit envelope case |
| `tests/e2e/chat_gateway_test.go` | **contract update** (§4.25.5): the facade's money-quota 429 expectation now carries the accurate business code; the upstream-429 wording and the legacy backend are untouched |
| `docs/api/kaya-coding-plan.openapi.yaml` | the three `/v1` 429 descriptions (perimeter bucket, `Retry-After` trustworthiness, the permanent Key budget), `/v1/models` gains a 429, `AnthropicError` states that the native shape has no `code` field |
| `docs/api-integration-guide.md` | a new `/v1` error-shape + `Retry-After` section; the `/chat` error table gains the four local-limit rows and the retry guidance |
| `CLAUDE.md` | the API-response-format exceptions now name the `/v1/*` native shapes (including the generic middleware's rejection) |
| `docs/superpowers/plans/2026-11-05-users-model-gateway-completion.md` | the T6 section backfills the actual signatures (same convention as T4/T5) |

#### 4.30.1 Commit

| Item | Value |
|---|---|
| Subject | `fix(inference): converge the configuration support sets and wire the connect timeout` |
| Parent | `9ffbe85bb51f9775193adb4a45f1faf287ecef20` (the T6 branch tip) |
| Change set | 34 files changed, 1700 insertions(+), 49 deletions(-) |
| Staging | `git add <explicit file list>` — **never** `git add .` |
| Push / merge / deploy | **none** (local commit only; no adjacent repository touched) |
| Own SHA | recorded in the task report (`.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-7-report.md`) — a runbook naming its own commit SHA would be circular, and the report file is not tracked |

The acceptance command (#13) was re-run **on the committed tree** after the
commit (worktree clean, `HEAD` = the T7 commit): 6 packages `ok`, exit **0**
(`catalog` 3.518s / `management` 1.936s / `providers` 2.587s /
`providers/connector` 1.693s / `routing` 2.032s / `gateway` 13.085s).

#### 4.31 Notes (root cause, compatibility, honesty)

- **Root cause (three defects, one symptom).** (1) The generic limiter
  middleware **hard-coded** the management envelope and had no injection
  point, while the router mounted it in front of the whole `/v1` group — so
  the perimeter rejection was the only `/v1` answer that was not
  protocol-native. (2) The `/chat` facade mapped every internal limit class
  onto the upstream-429 sentinel (wrong wording) and had **no** mapping at
  all for `insufficient_balance` (a generic 502). (3) The money-side 429
  computed "is the recovery instant trustworthy?" from the **windows only**,
  and the pure-rule evaluation short-circuited the admission **before** the
  Key's cumulative budget was ever checked — so a permanently exhausted Key
  budget was invisible and the window's reset was promised as the recovery
  time.
- **The fix reuses what already existed**: the native error helpers
  (`v1Error` / `anthropicError`), T4's typed `RateLimitError`/`RateBlock`,
  T5's `insufficient_capacity` semantics and the
  `GetAPIKeyConcurrencyLimitTx` pattern (a database fact read inside the
  admission transaction, never an authentication snapshot).
- **Admission semantics unchanged**: the pass/reject conditions of RPM, TPM,
  concurrency and the money windows are untouched — only the *report* of a
  rejection gained one fact (whether the Key budget is also a blocker).
- **The countdown is now strictly more conservative**: a permanent blocker
  removes the `Retry-After` instead of inventing one. The TPM case (an
  in-flight hold's real consumption is unknown) already behaved that way and
  is unchanged.
- **Compatibility.** No new response field, no new enum, no migration, no new
  dependency. The `/v1` perimeter 429 changes shape (it was unparseable by
  any OpenAI/Anthropic SDK before); the envelope shape `{code,data,message}`
  is unchanged everywhere. **The `/chat` client-visible change set is four
  facade classes** — all of them *local* limits whose request never left:

  | business code | before | after | kind of change |
  |---|---|---|---|
  | `rate_limited` (business RPM/TPM) | 429 `chat upstream rate limit exceeded` | 429 `chat request rejected by a usage limit (rate_limited)` | **message only** (status unchanged) |
  | `insufficient_capacity` (concurrency cap) | 429 `chat upstream rate limit exceeded` | 429 `chat request rejected by a usage limit (insufficient_capacity)` | **message only** (status unchanged) |
  | `quota_exceeded` (money quota / Key budget) | 429 `chat upstream rate limit exceeded` | 429 `chat request rejected by a usage limit (quota_exceeded)` | **message only** (status unchanged) |
  | `insufficient_balance` (balance / spend cap) | **502** `chat upstream error` | **429** `chat request rejected by a usage limit (insufficient_balance)` | **status 502 → 429 AND message** |

  The upstream 429 wording (`chat upstream rate limit exceeded`) is
  byte-identical, in the legacy **and** the facade backend, and every other
  `/chat` message is unchanged. The 502 → 429 of `insufficient_balance` is
  the **only** status-code change: it used to fall through to the generic
  `default` branch (502), which is exactly what "不能全部降为 generic 502"
  forbids. Only the `quota_exceeded` row had a byte-exact e2e contract
  (`TestChatContract_FacadeQuotaExceededIs429`, updated in §4.25.5); the
  other three rows had no byte-exact contract, but their existing wording was
  still rewritten — so the change set is **four classes**, not one. The
  integration guide's `/chat` error table was updated in the same change
  (all four rows).
- **The Anthropic native shape has no `code` field** — the business class is
  carried by `error.message` there. The OpenAPI previously described the
  three codes as if they were a `code` field; the document now matches the
  implementation (the protocol shape was **not** changed to satisfy a
  document).
- **Fail-closed preserved**: a storage failure while reading the Key budget
  answers 503 `service_unavailable`, never a fake quota exhaustion.
- **`quota.Store` grew one required method** — the same conscious trade-off
  T5 made: an optional upgrade would silently skip the permanent-blocker
  probe (and therefore re-introduce a fabricated countdown).
- **Concurrency still carries no `Retry-After`**: the lease's "earliest slot
  frees at" instant lives in the message, not in a structured field; making
  it a structured, trustworthy recovery instant is T7/T10 work. T6 does not
  invent one.

### T7 — Configuration-behaviour convergence and connect timeouts

Recorded on 2026-11-06, base SHA `9ffbe85` (T6 branch tip). Task report:
`.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-7-report.md`.

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the
instance down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`; every
database-backed command used `-p 1` and `-race`; `go.mod` / `go.sum` were
never modified (`git diff --stat go.mod go.sum` → empty after every run).

#### 4.25 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1 / run 2, `t7_app`) | **0** / **0** | `applied=53 skipped=0` → `applied=0 skipped=53` (idempotent) |
| 4 | `go run ./cmd/migrate -status` | **0** | 53 × `✅` |
| 5 | `DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate` | **0** | e2e-side DB migrated before the e2e job (`applied=53 skipped=0`) |
| 6 | **RED** `go test -count=1 -run 'TestValidateRoute_PoolStrategySupportedSet' -v ./internal/inference/catalog` | **1** | the write path accepted `least_loaded` / `session_sticky` — see §4.26 |
| 7 | **RED** `go test -count=1 -run 'TestPublishedSnapshot_LegacyPoolStrategy\|TestPublish_RejectsUnsupportedPoolStrategy' -v ./internal/inference/catalog` | **1** | the legacy route was a dispatchable deployment and publish accepted it |
| 8 | **RED** `go test -count=1 -run 'TestCandidates_UnsupportedPoolStrategyIsNotACandidate' -v ./internal/inference/routing` | **1** | the legacy route produced candidates (silently round-robin) |
| 9 | **RED** `go test -count=1 -run 'TestQuotaPolicy_ClampIfDeclaredRejectedOnWrite' -v ./internal/inference/management` | **1** | the write path accepted `clamp_if_declared` |
| 10 | **RED** `go test -count=1 -run 'TestDispatch_ConnectTimeout' -v ./internal/inference/providers` | **1** | `connect_timeout` did not bound the dial at all |
| 11 | **RED** `DATABASE_URL=<disposable> go test -count=1 -run 'TestGateway_UnsupportedPoolStrategy\|TestGateway_ClampIfDeclaredPolicy' -v ./internal/inference/gateway` | **1** | both configuration errors were silently ignored (the call succeeded) |
| 12 | **GREEN** the same commands as #6–#11 | **0** | all T7 cases pass |
| 13 | **acceptance（任务书原文）** `go test -race -count=1 -p 1 ./internal/inference/catalog ./internal/inference/management ./internal/inference/providers/... ./internal/inference/routing ./internal/inference/gateway` | **0** | 6 packages `ok` (catalog 17.459s / management 2.515s / providers 2.676s / providers/connector 2.500s / routing 3.052s / gateway 41.447s) |
| 14 | `go test -race -count=1 -p 1 -v ./internal/inference/...` | **0** | 14 packages `ok`, **1277 PASS lines / 1 SKIP / 0 FAIL**（baseline 口径：含子测试行；与基线 `9ffbe85` 的 1256 相比 **+21**，全部是 T7 新增用例 —— 见 §4.27.2 的对账） |
| 15 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | **52 PASS lines / 2 SKIP / 0 FAIL**（与基线口径与数字**逐字相同**；T7 未改集成套件） |
| 16 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 17 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.3%` ≥ the 80% floor (T6 80.3%, T0 80.4%) |
| 18 | `E2E_DATABASE_URL=<disposable> go test -race -count=1 ./tests/e2e/...` | **0** | `ok github.com/yunhou/users/tests/e2e 449.247s` |
| 19 | `go test -race -count=1 -run 'TestMapGatewayError' -v ./internal/service` | **0** | the legacy `/chat` facade maps the configuration error to its own class |

#### 4.26 RED evidence (before any production edit)

Command #6 ran against the **unmodified** production code at `9ffbe85`; the
new tests were written against the *existing* API surface only, so every
failure is a behavioural assertion failure, never a compile error.

```
=== RUN   TestValidateRoute_PoolStrategySupportedSet
    validate_edges_test.go:176: write path accepted "least_loaded" (unsupported pool strategy must be refused, not stored as if it worked)
    validate_edges_test.go:176: write path accepted "session_sticky" (unsupported pool strategy must be refused, not stored as if it worked)
    validate_edges_test.go:183: "chaos": the refusal must name the supported replacement: inference/invalid_input: unknown pool strategy: chaos
--- FAIL: TestValidateRoute_PoolStrategySupportedSet (0.00s)
FAIL	github.com/yunhou/users/internal/inference/catalog	0.820s
```

```
=== RUN   TestPublishedSnapshot_LegacyPoolStrategyStaysReadableButUnavailable
    pool_strategy_test.go:60: least_loaded: ActiveDeployments = [{ID:d1 ... Status:active ...}], want 0 (unsupported pool strategy is not a dispatchable deployment)
    pool_strategy_test.go:60: session_sticky: ActiveDeployments = [{ID:d1 ... Status:active ...}], want 0 (unsupported pool strategy is not a dispatchable deployment)
--- FAIL: TestPublishedSnapshot_LegacyPoolStrategyStaysReadableButUnavailable (0.00s)
=== RUN   TestPublish_RejectsUnsupportedPoolStrategyAndNamesTheObject
    pool_strategy_test.go:103: publish must refuse an unsupported pool strategy
--- FAIL: TestPublish_RejectsUnsupportedPoolStrategyAndNamesTheObject (0.05s)
```

```
=== RUN   TestCandidates_UnsupportedPoolStrategyIsNotACandidate
    service_test.go:579: candidates = [prov-a/acct-1,prov-a/acct-1,prov-a/acct-1], want only the round_robin route (不支持的 pool_strategy 不得静默当 RR)
--- FAIL: TestCandidates_UnsupportedPoolStrategyIsNotACandidate (0.00s)
```

```
=== RUN   TestQuotaPolicy_ClampIfDeclaredRejectedOnWrite
    quota_policies_test.go:391: create must refuse the unimplemented clamp_if_declared overage policy
--- FAIL: TestQuotaPolicy_ClampIfDeclaredRejectedOnWrite (0.00s)
```

```
=== RUN   TestDispatch_ConnectTimeoutBoundsConnectionEstablishment
    adapter_dispatch_test.go:476: a connect timeout must fail the dispatch
--- FAIL: TestDispatch_ConnectTimeoutBoundsConnectionEstablishment (0.15s)
```

(0.15 s = the dialer's deliberate 150 ms sleep: `connect_timeout=30ms` had no
effect whatsoever — the field was a dead letter.)

```
=== RUN   TestGateway_UnsupportedPoolStrategyFailsBeforeAdmission
    config_errors_test.go:66: an unsupported pool strategy must fail the call with a configuration error
--- FAIL: TestGateway_UnsupportedPoolStrategyFailsBeforeAdmission (0.07s)
=== RUN   TestGateway_ClampIfDeclaredPolicyFailsClearly
    config_errors_test.go:107: a policy with the unimplemented clamp_if_declared mode must fail the call
--- FAIL: TestGateway_ClampIfDeclaredPolicyFailsClearly (0.06s)
```

**Protection evidence (already-correct behaviour, recorded instead of a
forced change).** These passed at the pre-fix baseline and are guards, not
new behaviour:

| Case | Guard that already held |
|---|---|
| `TestImportEnvCatalog_DerivedRouteUsesSupportedPoolStrategy` | the env import already derives `round_robin` routes |
| `TestPublish_SupportedPoolStrategyStillPublishes` | a supported catalog still publishes (the new gate must not block it) |
| `TestCandidates_WeightOrdersWithinEqualPriorityAndNeverSplits` | `weight` is **ordering within one priority**, never a probabilistic split — every candidate stays in the list |
| `TestBulkImport_DerivedRouteStaysInsideTheSupportedPoolStrategySet` | the bulk import's derived route is valid |
| `TestQuotaPolicy_LegacyClampIfDeclaredStaysQueryable` | a legacy policy row stays queryable (the read side never rewrites it) |
| `TestDispatch_ConnectTimeoutDoesNotTruncateEstablishedStream` | an established SSE stream is not truncated by the connect budget |
| `TestSlowUpstream_TimeoutHoldsForReconciliation` (pre-existing) | the **overall request timeout** (`request_timeout`) still bounds the whole request: unknown execution → the hold is kept and the request is parked |
| `TestExecutedUnknown_NoBlindReplay` (pre-existing) | an **unknown execution state is never blindly replayed** on the next candidate |

#### 4.27 GREEN evidence

Command #12 (the same cases, production code fixed) — exit code **0**:

```
--- PASS: TestValidateRoute_PoolStrategySupportedSet (0.00s)
--- PASS: TestImportEnvCatalog_DerivedRouteUsesSupportedPoolStrategy (0.11s)
--- PASS: TestPublishedSnapshot_LegacyPoolStrategyStaysReadableButUnavailable (0.00s)
--- PASS: TestPublish_RejectsUnsupportedPoolStrategyAndNamesTheObject (0.16s)
--- PASS: TestPublish_SupportedPoolStrategyStillPublishes (0.16s)
ok  	github.com/yunhou/users/internal/inference/catalog	2.176s
--- PASS: TestCandidates_UnsupportedPoolStrategyIsNotACandidate (0.00s)
--- PASS: TestCandidates_WeightOrdersWithinEqualPriorityAndNeverSplits (0.00s)
ok  	github.com/yunhou/users/internal/inference/routing	1.787s
--- PASS: TestBulkImport_DerivedRouteStaysInsideTheSupportedPoolStrategySet (0.00s)
--- PASS: TestQuotaPolicy_ClampIfDeclaredRejectedOnWrite (0.00s)
--- PASS: TestQuotaPolicy_LegacyClampIfDeclaredStaysQueryable (0.00s)
ok  	github.com/yunhou/users/internal/inference/management	1.956s
--- PASS: TestDispatch_ConnectTimeoutBoundsConnectionEstablishment (0.20s)
--- PASS: TestDispatch_ConnectTimeoutDoesNotTruncateEstablishedStream (0.31s)
--- PASS: TestDispatch_ConnectTimeoutIsUnknownExecution (0.04s)
ok  	github.com/yunhou/users/internal/inference/providers	2.355s
--- PASS: TestGateway_UnsupportedPoolStrategyFailsBeforeAdmission (0.15s)
--- PASS: TestGateway_DisabledLegacyRouteDoesNotBlockTheCall (0.17s)
--- PASS: TestGateway_ClampIfDeclaredPolicyFailsClearly (0.19s)
ok  	github.com/yunhou/users/internal/inference/gateway	2.324s
--- PASS: TestApplyBulkImportTx_DerivedRouteUsesSupportedPoolStrategy (0.07s)
ok  	github.com/yunhou/users/internal/inference/postgres	1.801s
--- PASS: TestMapGatewayError_UpstreamRejectionMatchesLegacyClassification (0.00s)
--- PASS: TestMapGatewayError_LocalLimitsAreNotUpstreamErrors (0.00s)
--- PASS: TestMapGatewayError_UnpricedCapabilityIsUpstreamError (0.00s)
ok  	github.com/yunhou/users/internal/service	3.286s
```

Acceptance command #13 (the brief's exact command) — exit code **0**:

```
ok  	github.com/yunhou/users/internal/inference/catalog	17.459s
ok  	github.com/yunhou/users/internal/inference/management	2.515s
ok  	github.com/yunhou/users/internal/inference/providers	2.676s
ok  	github.com/yunhou/users/internal/inference/providers/connector	2.500s
ok  	github.com/yunhou/users/internal/inference/routing	3.052s
ok  	github.com/yunhou/users/internal/inference/gateway	41.447s
```

#### 4.28 Skips

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; needs `KAYA_QUOTA_WORKER=1`. The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and passes with real contention output. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1`. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |

**Database-test skips: 0** — identical to the T0 (§2.6), T1 (§4.4), T2
(§4.12), T3 (§4.20), T5 and T6 inventories; grepping the skip lines for
`postgres` / `no database` / `database unavailable` returns nothing.

#### 4.29 Migration

**One appended migration:
`migrations/053_inference_route_pool_strategy_comments.sql`** (53 = the
re-checked latest number; 052 was the T6 baseline). It only re-states the
`COMMENT ON COLUMN` of `inference_model_routes.priority` /
`.weight` / `.account_pool_strategy` — the old comment said
「同优先级按 weight 加权」, which reads as a *probabilistic* split, while the
implementation is a deterministic ordering within one priority group. No
column type, no CHECK and no historical row is touched: the DB CHECK still
admits the legacy `least_loaded` / `session_sticky` values (those rows must
remain storable — they are the legacy data the read path has to survive).

Evidence: `go run ./cmd/migrate` (run 1) → `applied=53 skipped=0`;
(run 2) → `applied=0 skipped=53`; `go run ./cmd/migrate -status` → 53 × `✅`;
the e2e-side disposable database was migrated the same way.

#### 4.30 Files changed

| File | Change |
|---|---|
| `internal/inference/domain/connect_timeout.go` | **new** — the cross-package carrier of the connect budget (a context **value**, because `net/http` strips the request deadline before dialing but keeps the values) |
| `internal/inference/domain/model.go` | `ModelRoute.Priority` / `.Weight` / `.PoolStrategy` semantics in comments |
| `internal/inference/catalog/service.go` | the supported set (`SupportedPoolStrategy`, `PoolStrategySupported`, `poolStrategyRefusal`), `ValidateRoute` (write) vs `ValidateRouteForRead` (read), and the publish gate now validates the routes and names the object ID + the replacement |
| `internal/inference/catalog/revision.go` | `ParseSnapshot` uses the read validator; `ActiveDeployments` skips unsupported routes; `UnsupportedPoolStrategyRoutes` / `PoolStrategyDispatchBlock` |
| `internal/inference/catalog/import.go` | the env-import derived route goes through the shared validator |
| `internal/inference/management/bulk_import.go` | the bulk-import derived route goes through the shared validator |
| `internal/inference/management/quota_policies.go` | the write side refuses `clamp_if_declared` (create **and** update) and names the migration targets |
| `internal/inference/management/pricing_preview.go` | the preview shares the same overage support set |
| `internal/inference/routing/service.go` | the candidate filter skips unsupported routes; comments aligned |
| `internal/inference/providers/adapter.go` | the connect budget is wired into the shared transport (one transport, one connection pool — never a per-request transport); `Dispatch` carries the budget; the `classifyTransport` comment records the conservative verdict |
| `internal/inference/credentials/egress.go` | the outbound dialer reads the same budget (the DNS/IP validation itself is unchanged) |
| `internal/inference/gateway/service.go` | the configuration error before admission/dispatch + the alarm + the `clamp_if_declared` refusal + the typed `ErrConfigUnavailable` |
| `internal/inference/gateway/config_alert.go` | **new** — the legacy-dirty-config alarm channel (structured log + `AuditAlertHook`) |
| `internal/service/errors.go`, `internal/service/chat_facade.go`, `internal/handler/chat.go` | the legacy `/chat` surface reports the configuration error as 503 `chat configuration unavailable` (no `Retry-After` promise) instead of a generic upstream error |
| `internal/inference/postgres/pricing_repo.go` | the overage-policy comment |
| `migrations/053_inference_route_pool_strategy_comments.sql` | **new** (appended) — the three column comments |
| `docs/api/kaya-coding-plan.openapi.yaml` | the 503 semantics, the bulk-import / quota-policy-preview supported sets; version 0.13.0 → 0.14.0 |
| `docs/api-integration-guide.md` | the customer-visible configuration errors (`/v1/*` and `/chat`) |
| `docs/runbooks/kaya-coding-plan-operations.md` | the new「配置支持集（T7）」section (pool_strategy / weight / overage_policy / the deployment timeouts, incl. the manual migration steps) |
| `docs/api/kaya-coding-plan-website-handoff.md` | the version reference (0.13.0 → 0.14.0) |
| `internal/inference/catalog/validate_edges_test.go`, `catalog/pool_strategy_test.go` (**new**), `catalog/import_test.go` | the pool-strategy support-set cases |
| `internal/inference/management/quota_policies_test.go`, `management/bulk_import_test.go`, `postgres/bulk_import_repo_test.go` | the overage-mode and the bulk-import cases |
| `internal/inference/routing/service_test.go` | the routing cases (unsupported route / weight ordering) |
| `internal/inference/providers/adapter_dispatch_test.go` | the connect-timeout cases (distinct timeouts, long stream, unknown execution) |
| `internal/inference/gateway/config_errors_test.go` (**new**), `gateway/gateway_test.go` | the gateway cases (configuration error before admission, the disabled legacy route, the overage-mode refusal) + one fixture option |

#### 4.31 Notes (root cause, compatibility, honesty)

- **Root cause (three「配置收下但没兑现」+ one「配置没接线」).**
  (1) `catalog.ValidateRoute` accepted `least_robin`/`least_loaded`/
  `session_sticky` while **no runtime code ever read `route.PoolStrategy`** —
  an unimplemented strategy was saved, published and then silently executed
  as round-robin. (2) `overage_policy=clamp_if_declared` was accepted by the
  write side while the runtime treats it as `reject` — the customer's quota
  semantics changed silently. (3) the DB column comment said「同优先级按
  weight 加权」while the implementation is a deterministic ordering.
  (4) `deployment.connect_timeout` was validated, stored, published and
  imported — and **never used**: the shared client hard-coded a 10 s dial
  timeout, so the field was a dead letter.
- **One supported set, five write paths.** Operator CRUD, bulk import, env
  import, publish and publish-preview all funnel through
  `catalog.ValidateRoute`; the two import paths derive `round_robin` routes
  and validate them through the same gate. The Responses
  `previous_response_id` binding is untouched — it rides the session binder
  (`routing.SessionBinder`), not the route's pool strategy.
- **Legacy published snapshots stay readable.** The read path accepts the
  known-but-unsupported values (a truly unknown value still fails the
  parse — 「不得加载半个版本」). Such a route is marked **unavailable**:
  it is not a candidate, it is not an active deployment (the model list and
  the dispatch path agree), it raises an alarm, and when it is the model's
  only **enabled** route the call fails with a configuration error **before
  the reservation** (zero hold, zero upstream call, zero billing).
- **`weight` keeps its ordering semantics** — the code comments, the schema
  comment (migration 053) and the operations runbook now say the same thing:
  ordering inside one priority group, never a probabilistic split.
- **The connect budget covers connection establishment only.** It travels as
  a context **value** (net/http drops the request deadline before dialing
  but keeps the values — Go 1.27 `transport.go` `getConn`), the dialer turns
  it into a deadline that covers DNS + TCP connect, and the deadline is
  cancelled as soon as the dial returns — an established SSE stream is never
  truncated by it. The **transport is shared** (one connection pool): a
  per-request transport would leak a pool per request. The outbound DNS/IP
  validation and the cross-origin redirect ban are untouched.
- **A connect timeout is still classified as unknown-execution** (recorded,
  not forced): the transport cannot tell "the connection was never
  established" from "the request left the box", and the repository's core
  money-safety rule is「不确定是否已执行的上游请求不得盲目重放」. The
  consequence is documented for operators (a too-small `connect_timeout`
  parks the request for reconciliation instead of failing over) — changing
  the execution verdict is a separate task, not a timeout change.
- **The effective connect budget of EXISTING deployments shrank from 10 s to
  5 s (corrected after the review).** The stored default is 5 s
  (`inference_deployments.connect_timeout_ms DEFAULT 5000`, and the
  create / env-import / bulk-import defaults are 5 s too), while the shared
  client used a **hard-coded 10 s** dial timeout — so a deployment that never
  declared a value actually got 10 s. Now that the stored value is honoured,
  every such deployment gets **5 s**. The controller ruled: keep the stored
  5 s (the field means what it says), change **no** default and migrate
  **no** live configuration. Consequences an operator must know:
  (a) explicitly configured values now really control DNS + TCP
  establishment (before: ignored);
  (b) deployments on the default lost 5 s of connect budget — a slow-but-
  reachable upstream may now hit the connect timeout;
  (c) a connect timeout lands in **reconciliation** (the hold is kept, no
  failover), not in a clean candidate switch — so the operational remedy is
  to raise `connect_timeout` (or the upstream's own connect latency), not
  to wait for a retry.
- **Compatibility.** Two new customer-visible 503s (the `/v1/*` surfaces and
  the legacy `/chat` surface), 400s on the operator writes, one appended
  comment-only migration, and the OpenAPI bumped 0.13.0 → 0.14.0. No
  client-visible field, enum or request shape changed; no new dependency;
  T1 (auth), T2 (settlement), T3 (provider filtering), T4 (rate admission),
  T5 (leases) and T6 (protocol errors) are untouched.
- **Pre-existing flaky test (not introduced by T7, not fixed here).**
  `internal/inference/gateway`'s `TestNonStreamDispatch_RenewsAccountLease`
  sets the lease TTL to 50 ms, so the acquire→check database round trip can
  exceed it (`lease: expired`). It reproduced on the **unmodified** base
  `9ffbe85` as well (a separate `git worktree` of the base commit, the
  gateway package three times: 1 failure), therefore it is not a T7
  regression — recorded per the「不修无关缺陷」rule.

#### 4.42 T7 fix round — the three review findings

Recorded on 2026-11-06, on top of the T7 commit (`d1b6ec8`). The review
raised **three Important** findings; all three are fixed here, no deferred
minor was touched.

**Environment.** Same recipe as §4.25: a fresh trap-cleaned disposable
PostgreSQL per run, explicit `DATABASE_URL` / `E2E_DATABASE_URL`, `-p 1`,
`-race`, `GOPROXY=off` + `GOFLAGS=-mod=readonly`, `go.mod` / `go.sum`
untouched.

##### 4.42.1 Finding 1 — the publish gate reported the wrong cause

`validateCatalogForPublish` wrapped **every** `ValidateRoute` error with
`poolStrategyRefusal(...)`, so a route with `weight <= 0` or an empty
`model_id` was refused with "pool_strategy round_robin is not supported …
set pool_strategy to round_robin" — self-contradictory and pointing the
operator at the wrong knob.

**RED** (the new focused cases, before the fix):

```
=== RUN   TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause/weight_zero_keeps_its_own_cause
    validate_edges_test.go:321: error = inference/invalid_input: route route-1 is not publishable: unknown pool strategy:  (supported: round_robin), want it to name "weight"
=== RUN   TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause/empty_model_keeps_its_own_cause
    validate_edges_test.go:321: error = inference/invalid_input: route route-2 is not publishable: unknown pool strategy:  (supported: round_robin), want it to name "model_id"
--- FAIL: TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause (0.00s)
    --- FAIL: TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause/weight_zero_keeps_its_own_cause (0.00s)
    --- FAIL: TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause/empty_model_keeps_its_own_cause (0.00s)
    --- PASS: TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause/unsupported_strategy_names_the_replacement (0.00s)
```

The third case passed **before** the fix as well — protection evidence: an
unsupported strategy must still name the replacement.

**Fix.** The publish gate now checks the two invariants separately and lets
each report its own cause: `validateRouteShape` (weight / model_id /
deployment_id) keeps its own message; only `validatePoolStrategy` produces
the "pool_strategy X is not supported — set it to round_robin" text. The
underlying validation error is preserved (`routeRefusalMessage` renders the
`domain.Error` message without the code prefix).

**GREEN** (the same command, after the fix) — exit code **0**:

```
--- PASS: TestValidateRoute_PoolStrategySupportedSet (0.00s)
--- PASS: TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause (0.00s)
    --- PASS: TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause/weight_zero_keeps_its_own_cause (0.00s)
    --- PASS: TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause/empty_model_keeps_its_own_cause (0.00s)
    --- PASS: TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause/unsupported_strategy_names_the_replacement (0.00s)
--- PASS: TestPublish_RejectsUnsupportedPoolStrategyAndNamesTheObject (0.05s)
ok  	github.com/yunhou/users/internal/inference/catalog	2.534s
```

##### 4.42.2 Finding 2 — the `connect_timeout` compatibility claim was wrong

The stored default is **5 s** (`inference_deployments.connect_timeout_ms
DEFAULT 5000`; the create / env-import / bulk-import defaults are 5 s too),
while the shared client used a **hard-coded 10 s** dial timeout — the field
was never read. So the deployments that never declared a value actually got
**10 s** and now get **5 s**: the effective budget **shrank**, it did not
"stay unchanged".

Controller ruling: **keep the stored 5 s** (the field means what it says),
change **no** default and migrate **no** live configuration. Therefore the
fix is a documentation correction, not a behaviour change:

| Where | Correction |
|---|---|
| task report §2.4.1 (new) + §8 + §12 | the fact table (DB default 5 s / write-path default 5 s / old hard-coded 10 s / new = stored value) and the three consequences |
| this runbook §4.31 | the same, incl. "explicitly configured values now really control DNS + TCP establishment" and "a connect timeout lands in reconciliation, not in a failover" |
| `docs/runbooks/kaya-coding-plan-operations.md` | the default values, the 10 s → 5 s effect for existing deployments and the operational remedy (raise `connect_timeout`, do not wait for a retry) |
| `internal/inference/domain/connect_timeout.go` | `DefaultConnectTimeout` (10 s) is documented as the "caller declared no budget at all" fallback, with the stored 5 s default named |

##### 4.42.3 Finding 3 — the verification evidence was not comparable

See **§4.27.2** for the full reconciliation (base `9ffbe85` vs the T7 tree,
one session, one disposable PostgreSQL, one counting method):

| Suite | Metric | Base `9ffbe85` | T7 (fixed tree) | Delta |
|---|---|---|---|---|
| `./internal/inference/...` | PASS lines (baseline method) | 1256 | **1277** | **+21** (18 new top-level + 3 new subtests — every one listed in §4.27.2) |
| | top-level / subtest split | 1015 / 241 | 1033 / 244 | +18 / +3 |
| | FAIL / SKIP | 0 / 1 | **0 / 1** (the same `TestQuotaWorkerProcess`) | 0 |
| | packages `ok` | 14 | 14 | 0 |
| `./tests/integration/...` | PASS lines (baseline method) | **52** | **52** | 0 |
| | FAIL / SKIP | 0 / 2 | 0 / 2 (the same two load drills) | 0 |

The integration suite reproduces the recorded baseline number **52 exactly**
on both trees — that is the proof that the baseline method is the "all PASS
lines" one and that T7's earlier "35" was a top-level-only filter. The
inference delta of **+21** is accounted for case by case, so no pre-existing
test was lost, renamed or skipped: **T1–T6's preservation is measured, not
asserted**.

##### 4.42.4 Fix-round commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1 / run 2) | **0** / **0** | `applied=53 skipped=0` → `applied=0 skipped=53`; `-status` → 53 × `✅` |
| 4 | `DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate` | **0** | the e2e-side DB migrated before the e2e job (`applied=53 skipped=0`) |
| 5 | **acceptance（任务书原文）** `go test -race -count=1 -p 1 ./internal/inference/catalog ./internal/inference/management ./internal/inference/providers/... ./internal/inference/routing ./internal/inference/gateway` | **0** | 6 packages `ok` (catalog 2.513s / management 1.501s / providers 1.770s / providers/connector 2.641s / routing 2.397s / gateway 9.402s) |
| 6 | **GREEN** the Finding-1 cases (`-v -run 'TestValidateCatalogForPublish_RouteRefusalNamesTheRealCause\|TestValidateRoute_PoolStrategySupportedSet\|TestPublish_RejectsUnsupportedPoolStrategyAndNamesTheObject'`) | **0** | 3 test funcs / 5 cases pass (see §4.42.1) |
| 7 | `go test -race -count=1 -p 1 -v ./internal/inference/...` | **0** | 14 packages `ok`; **1277 PASS lines / 1033 top-level / 244 subtest / 0 FAIL / 1 SKIP** |
| 8 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | **52 PASS lines / 35 top-level / 17 subtest / 0 FAIL / 2 SKIP** |
| 9 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 10 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.3%` ≥ the 80% floor |
| 11 | `E2E_DATABASE_URL=<disposable> go test -race -count=1 ./tests/e2e/...` | **0** | `ok github.com/yunhou/users/tests/e2e 142.635s` |

**Skip inventory (fix round)** — unchanged from §4.28: `TestQuotaWorkerProcess`
(`KAYA_QUOTA_WORKER=1` child-process entry point), `TestLoadDrill_Throughput`
and `TestLoadDrill_LongConnections` (`KAYA_LOAD=1`). **Database-test skips: 0.**

**Files changed in the fix round** (5):

| File | Change |
|---|---|
| `internal/inference/catalog/service.go` | the publish gate reports each invariant's own cause; `routeRefusalMessage` helper |
| `internal/inference/catalog/validate_edges_test.go` | the focused Finding-1 cases (3 subtests + the write-path assertions) |
| `internal/inference/domain/connect_timeout.go` | the `DefaultConnectTimeout` comment (10 s = the "no budget declared" fallback; the stored default is 5 s) |
| `docs/runbooks/2026-11-05-model-gateway-verification.md` | §4.27.2 (the count reconciliation), §4.31 (the corrected `connect_timeout` compatibility), this section |
| `docs/runbooks/kaya-coding-plan-operations.md` | the deployment-timeout section: the defaults, the 10 s → 5 s effect and the remedy |

**Commit.** An independent fix commit on top of `d1b6ec8` (explicit paths
only, no `git add .`, no push / merge / deploy) — see the task report §11.1.

### T8 — Capability routing and protocol truthfulness

Recorded on 2026-11-06, base SHA `0fc5afb` (T7 branch tip; T0 base
`35cf836`). Task report:
`.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-8-report.md`.

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the
instance down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`;
every database-backed command used `-p 1`; `go.mod` / `go.sum` were never
modified (`git diff --stat go.mod go.sum` → empty after every run).

#### 4.32 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1 / run 2, `t8_app`) | **0** / **0** | `applied=53 skipped=0` → `applied=0 skipped=53` (idempotent) |
| 4 | `go run ./cmd/migrate -status` | **0** | 53 × `✅` |
| 5 | **RED** `go test -count=1 -run 'TestGateway_ImageRequest\|TestGateway_TextRequestKeepsOriginalCandidates' -v ./internal/inference/gateway` | **1** | the image leak reproduced — see §4.33 |
| 6 | **RED** `go test -count=1 -run 'TestCapabilityMatrix\|TestV1ChatCompletions_ReasoningEffort\|TestV1ChatCompletions_UnsupportedReasoningEffort\|TestV1Responses_ReasoningEffort\|TestV1ChatCompletions_ServerSideTool\|TestV1Messages_ThinkingRequest\|TestV1Responses_ReasoningItem\|TestV1Messages_NoFakedThinkingSignature' -v ./internal/inference/httpapi` | **1** | the protocol-fidelity leaks reproduced — see §4.34 |
| 7 | **GREEN** the same two commands after the fix | **0** | all T8 cases pass |
| 8 | **acceptance (brief, verbatim)** `go test -race -count=1 -p 1 ./internal/inference/providers/... ./internal/inference/httpapi ./internal/inference/gateway` | **0** | 4 packages `ok` (`providers` 4.916s / `providers/connector` 1.313s / `httpapi` 121.509s / `gateway` 19.967s) |
| 9 | `go test -race -count=1 -p 1 -v ./internal/inference/...` | **0** | 14 packages `ok`, PASS/SKIP/FAIL inventory in §4.36 |
| 10 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | 52 PASS lines / 2 SKIP / 0 FAIL (the T8 fixture case was rewritten, not removed — §4.37) |
| 11 | `E2E_DATABASE_URL=<disposable> go test -race -count=1 ./tests/e2e/...` | **0** | `ok github.com/yunhou/users/tests/e2e 215.218s` — see §4.36 |
| 12 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 13 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.3%` ≥ the 80% floor (T0 80.4%, T1 80.5%, T2 80.3%, T3 80.4%) |

#### 4.33 RED evidence — capability routing (gateway layer)

Command #5 ran against the **unmodified** production code at `0fc5afb`; the
new tests were written against the *existing* API surface only (the gateway's
`ChatCompletions`, the published snapshot, the real store), so every failure
is a behavioural assertion failure, never a compile error.

```
=== RUN   TestGateway_ImageRequestSkipsIncompatibleDeployment
    t8_capability_routing_test.go:141: ChatCompletions (image): inference/invalid_input: providers: image input is not supported by this adapter
--- FAIL: TestGateway_ImageRequestSkipsIncompatibleDeployment (0.06s)
=== RUN   TestGateway_ImageRequestWithoutCompatibleCandidateRejectsBeforeAdmission
    t8_capability_routing_test.go:176: requests = 1, want 0 (rejection must precede admission)
    t8_capability_routing_test.go:179: attempts = 1, want 0 (rejection must precede dispatch)
    t8_capability_routing_test.go:182: leases = 1, want 0 (rejection must leave no lease)
    t8_capability_routing_test.go:185: reservations = 4, want 0 (rejection must leave no hold)
--- FAIL: TestGateway_ImageRequestWithoutCompatibleCandidateRejectsBeforeAdmission (0.06s)
=== RUN   TestGateway_TextRequestKeepsOriginalCandidates
--- PASS: TestGateway_TextRequestKeepsOriginalCandidates (0.06s)
```

**Root cause (one defect, two surfaces).** The image capability existed
**only in the adapter's `BuildPayload`** (`openai_responses` refused an image
with `CodeInvalidInput`) and in the *model* declaration
(`input_modalities` / `AcceptsImages`). The **routing layer had no image
dimension at all** (`routing.Needs` had only `Tools`/`Reasoning`), so:

1. an image-bearing request selected the first candidate **by failover order**
   — the incompatible `openai_responses` deployment — and only failed when
   the adapter tried to build the payload (after admission, after the attempt
   row and the upstream lease were committed);
2. when *no* candidate could carry the image, the request was still **admitted**
   (money + TPM holds, a request row, an attempt row, a lease — 4 reservation
   rows in the RED run) before the shape error surfaced.

The third case (`TestGateway_TextRequestKeepsOriginalCandidates`) **passed at
the pre-fix baseline**: it is protection evidence that the text candidate set
must stay exactly as it was.

#### 4.34 RED evidence — protocol fidelity (HTTP entry layer)

Command #6 ran against the same unmodified tree. The failures are the
"accept-and-drop" / "silently ignore" behaviours the brief names:

```
--- FAIL: TestCapabilityMatrix_ClientEntryByUpstreamProtocol (4.30s)
    --- FAIL: .../openai_chat/messages/thinking_replay (0.20s)
    --- FAIL: .../openai_chat/responses/thinking_replay (0.12s)
    --- FAIL: .../anthropic_messages/messages/thinking_replay (0.12s)
    --- FAIL: .../anthropic_messages/responses/thinking_replay (0.13s)
    --- FAIL: .../openai_responses/messages/thinking_replay (0.11s)
    --- FAIL: .../openai_responses/responses/thinking_replay (0.11s)
        t8_capability_matrix_test.go:386: code = 200, want 400（明确拒绝）: {"content":[{"text":"Hello","type":"text"}], ...}
--- FAIL: TestV1ChatCompletions_ReasoningEffortIsPreservedUpstream (0.50s)
    --- FAIL: .../low (0.14s)   upstream body does not carry the requested effort "low":
          {"max_tokens":8192,"messages":[{"role":"user","content":"hi"}],"model":"matrix-upstream","stream":false}
    --- FAIL: .../medium ... --- FAIL: .../high ... --- FAIL: .../xhigh ...
--- FAIL: TestV1ChatCompletions_UnsupportedReasoningEffortIsRejected (0.07s)
    code = 200, want 400: {"id":"chatcmpl-1", ... }          ← the effort was silently ignored
--- FAIL: TestV1Responses_ReasoningEffortIsPreservedUpstream (0.41s)
    upstream body does not carry the requested effort "low":
    {"max_tokens":8192,"messages":[...],"stream":false,"thinking":{"type":"enabled"}}   ← folded to a boolean
--- FAIL: TestV1ChatCompletions_ServerSideToolIsRejected (0.07s)
    code = 200, want 400                                        ← a server-side tool was relayed upstream
--- FAIL: TestV1Messages_ThinkingRequestIsRejected (0.07s)
    code = 200, want 400                                        ← thinking was accepted
--- FAIL: TestV1Responses_ReasoningItemIsRejected (0.19s)
    code = 200, want 400                                        ← the reasoning item was accepted (and dropped)
--- FAIL: TestV1Messages_NoFakedThinkingSignature (0.12s)
    the gateway faked an empty thinking signature:
    {"content":[{"signature":"","thinking":"step-1","type":"thinking"},{"text":"Hello","type":"text"}], ...}
```

The **other 30 matrix cells passed at the pre-fix baseline** (text / tools /
images for every entry × upstream protocol) and the whole SSE matrix
(`TestCapabilityMatrix_SSETermination`, 18 cases: complete end + truncated
EOF for 3 entries × 3 upstream protocols) passed too — protection evidence
for the behaviour T8 must **not** change.

#### 4.35 GREEN evidence

Commands #7 (the same two commands, production code fixed) — exit code **0**:

```
--- PASS: TestGateway_ImageRequestSkipsIncompatibleDeployment (0.06s)
--- PASS: TestGateway_ImageRequestWithoutCompatibleCandidateRejectsBeforeAdmission (0.05s)
--- PASS: TestGateway_TextRequestKeepsOriginalCandidates (0.06s)
--- PASS: TestCapabilityMatrix_ClientEntryByUpstreamProtocol (4.5s)      (36 cells)
--- PASS: TestCapabilityMatrix_SSETermination (2.3s)                      (18 cells)
--- PASS: TestV1ChatCompletions_ReasoningEffortIsPreservedUpstream (0.5s)
--- PASS: TestV1ChatCompletions_UnsupportedReasoningEffortIsRejected (0.07s)
--- PASS: TestV1Responses_ReasoningEffortIsPreservedUpstream (0.4s)
--- PASS: TestV1ChatCompletions_ServerSideToolIsRejected (0.07s)
--- PASS: TestV1Messages_ThinkingRequestIsRejected (0.07s)
--- PASS: TestV1Responses_ReasoningItemIsRejected (0.19s)
--- PASS: TestV1Messages_NoFakedThinkingSignature (0.12s)
```

Acceptance command #8 (the brief's verbatim command) — exit code **0**:

```
ok  	github.com/yunhou/users/internal/inference/providers	5.384s
ok  	github.com/yunhou/users/internal/inference/providers/connector	2.331s
ok  	github.com/yunhou/users/internal/inference/httpapi	69.606s
ok  	github.com/yunhou/users/internal/inference/gateway	14.489s
```

#### 4.36 Test inventory and skips

| Suite | Result |
|---|---|
| `go test -race -count=1 -p 1 -v ./internal/inference/...` | 14 packages `ok`; **1052 top-level PASS + 314 subtest PASS, 1 SKIP, 0 FAIL**. The only SKIP is `TestQuotaWorkerProcess` (child-process entry point, needs `KAYA_QUOTA_WORKER=1`) |
| `go test -race -count=1 -p 1 -v ./tests/integration/...` | 35 top-level PASS + 18 subtest PASS / **2 SKIP** (`TestLoadDrill_Throughput`, `TestLoadDrill_LongConnections` — both behind the explicit `KAYA_LOAD=1` gate) / 0 FAIL |
| `go test -race -count=1 ./tests/e2e/...` | `ok github.com/yunhou/users/tests/e2e 215.218s` (exit **0**) |

**Database-test skips: 0** — identical to the T0 inventory (§2.6); grepping
the skip lines for `postgres` / `no database` / `database unavailable`
returns nothing.

**Harness note (not a code defect).** The first e2e attempt in this task
failed with `pq: relation "inference_audit_log" does not exist` — the
e2e-side disposable database had not been migrated. `tests/e2e` does **not**
apply migrations itself (it reads `E2E_DATABASE_URL` and expects a migrated
schema, exactly as §1.2/§2.3 prescribe). After migrating the e2e database in
the harness, the same command exits **0**. The failure was a harness
omission, not a T8 regression.

#### 4.37 Migration

**None.** T8 adds no migration: the fix lives entirely in the capability
layers (domain / routing / gateway), the adapters and the protocol client
surfaces. The disposable databases were migrated with `cmd/migrate` exactly
as in §2.3 (53 files, idempotent on the second run).

#### 4.38 Files changed

| File | Change |
|---|---|
| `internal/inference/domain/model.go` | the reasoning-effort vocabulary (`ReasoningEffort`, `SupportedReasoningEfforts`, `ValidReasoningEffort`) |
| `internal/inference/domain/usage.go` | `AdapterCapabilities.Images` + `AdapterCapabilities.ReasoningEfforts` |
| `internal/inference/providers/adapter.go` | `ChatRequest.ReasoningEffort` (the explicit level travels with the request) |
| `internal/inference/providers/openai_chat.go` | declares image + effort support; relays `reasoning_effort` verbatim |
| `internal/inference/providers/openai_responses.go` | declares **no** image support; relays the requested effort instead of folding it to `medium` |
| `internal/inference/providers/anthropic.go` | declares image support, **no** effort knob; refuses an effort level defensively |
| `internal/inference/routing/service.go` | `Needs.Images` + `Needs.ReasoningEffort`; the single `CompatibleDeployments` capability filter (route + deployment + adapter) shared by the candidate builder and the pre-admission gate |
| `internal/inference/gateway/service.go` | the capability needs; the pre-admission `rejectUnservableNeed` gate (+ the typed `ErrCapabilityUnsupported`); the image-token estimate comment (estimate, not a universal upper bound) |
| `internal/inference/httpapi/chat_completions.go` | `reasoning_effort` parsing + validation; the server-side tool rejection |
| `internal/inference/providers/responses_client.go` | the effort is preserved; a `reasoning` input item is now an explicit 400 |
| `internal/inference/providers/messages_client.go` | `thinking.type=enabled` and signed/redacted thinking history are explicit 400; the surface no longer renders a thinking block (no faked signature) |
| `internal/service/{errors.go,chat_facade.go}`, `internal/handler/chat.go` | the legacy /chat surface maps the capability rejection onto a 400-class sentinel (not "upstream rejected" 502) |
| `internal/inference/gateway/sticky_candidates_test.go` | the T8 gateway cases (the capability-routing section appended) |
| `internal/inference/httpapi/protocol_surfaces_test.go` | the executable capability matrix + the SSE termination matrix (appended) |
| `internal/inference/providers/multimodal_guard_test.go` | the image/effort capability declarations vs the adapters' actual behaviour |
| `internal/inference/providers/{messages_client,responses_client}_test.go` | the thinking/reasoning rejection cases; the old accept-and-drop fixtures were rewritten (compatibility correction) |
| `internal/inference/routing/service_test.go` | the routing-level image/effort candidate cases |
| `internal/inference/gateway/gateway_test.go` | fixture-only: the Responses adapter is registered so the capability tests can publish one |
| `tests/integration/kaya_client_contract_test.go` | the thinking-history fixture case now expects the explicit 400; a new case proves the tool history without the thinking block still works |
| `docs/api-integration-guide.md`, `docs/api/kaya-coding-plan.openapi.yaml`, `docs/api/kaya-coding-plan-website-handoff.md`, `docs/runbooks/kaya-coding-plan-rollout.md`, `docs/api/fixtures/client/client-request-anthropic-messages-thinking-history.json` | the capability matrices, the new field/error, and the compatibility corrections |

#### 4.39 T8 fix round — four review findings (OpenAPI semantics, picker vs dispatch, capability vocabulary, emit-vs-echo)

Recorded on 2026-11-06, after the T8 commit `059a904`. Four **Important**
review findings were fixed with focused TDD; the fix round is one independent
commit on top of `059a904`. Nothing else was touched — deferred minors stayed
deferred.

**Environment.** Same recipe as §4.32: a fresh trap-cleaned disposable
PostgreSQL per run, explicit `DATABASE_URL` / `E2E_DATABASE_URL`, `-p 1`,
`-race`, `GOPROXY=off` + `GOFLAGS=-mod=readonly`, `go.mod` / `go.sum`
untouched.

##### 4.39.1 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | **RED** `go test -count=1 -run 'TestOpenAPI_ReasoningEffortContract' -v ./internal/inference/httpapi` | **1** | the stale `minimal` semantics were still documented — §4.39.2 |
| 4 | **RED** `go test -count=1 -run 'TestKayaModels_ImageModalitiesFollowDispatchCapability' -v ./internal/inference/httpapi` | **1** | the picker still advertised a refused image — §4.39.2 |
| 5 | **RED** `go test -count=1 -run 'TestChatFacade_AllowedModels_ImageModalitiesFollowDispatchCapability' -v ./internal/service` | **1** | the legacy picker had the same defect — §4.39.2 |
| 6 | **RED** `go test -count=1 -run 'TestValidateRoute_CapabilityVocabulary\|TestValidateCatalogForPublish_CapabilityVocabulary' -v ./internal/inference/catalog` | **1** | the capability vocabulary was unvalidated — §4.39.2 |
| 7 | **RED/protection** `go test -count=1 -run 'TestRouteCovers_LegacyCapabilityLists\|TestParseSnapshot_UnknownRouteCapabilityStaysLoadable' -v ./internal/inference/routing ./internal/inference/catalog` | **0** | the legacy empty-vs-restrictive semantics and the read-path tolerance already held — protection evidence |
| 8 | **protection** `go test -count=1 -run 'TestV1Responses_ReasoningOutputItemDoesNotBreakTheChain' -v ./internal/inference/httpapi` | **0** | the chain already worked (the finding was a documentation gap, not a code gap) |
| 9 | **acceptance (brief, verbatim)** `go test -race -count=1 -p 1 ./internal/inference/providers/... ./internal/inference/httpapi ./internal/inference/gateway` | **0** | 4 packages `ok` (final tree: `providers` 4.777s / `providers/connector` 1.179s / `httpapi` 66.711s / `gateway` 14.134s) |
| 10 | `go test -race -count=1 -p 1 ./internal/inference/... ./internal/service/... ./internal/handler/... ./cmd/...` | **0** | 19 packages `ok`, 0 `FAIL` |
| 11 | `go test -race -count=1 -p 1 -v ./tests/integration/...` | **0** | `ok github.com/yunhou/users/tests/integration 39.001s`, 2 SKIP (the `KAYA_LOAD=1` gate) |
| 12 | `E2E_DATABASE_URL=<disposable> go test -race -count=1 ./tests/e2e/...` | **0** | `ok github.com/yunhou/users/tests/e2e 396.435s` |
| 13 | CI `go-ci` gate + coverage floor | **0** | 33 packages `ok`, 0 `FAIL`, `total coverage: 80.2%` ≥ the 80% floor |

##### 4.39.2 RED evidence (before the production edits)

Every new case was written against the **existing** API surface (the OpenAPI
document, the handler/facade constructors, `catalog.ValidateRoute`, the
routing filters) — so every failure is a behavioural assertion failure, never
a compile error.

```
--- FAIL: TestOpenAPI_ReasoningEffortContract (0.00s)
    openapi_contract_test.go:246: openapi still documents the stale minimal semantics
    (none/minimal → 关闭推理); minimal is the lowest reasoning level, only none turns reasoning off
--- FAIL: TestKayaModels_ImageModalitiesFollowDispatchCapability (0.06s)
    kaya_models_test.go:NNN: glm-vision-v input_modalities = [text image], want [text]
    (没有能承载图片的部署时不得 advertise image)
--- FAIL: TestChatFacade_AllowedModels_ImageModalitiesFollowDispatchCapability (0.00s)
    chat_facade_test.go:837: glm-v InputModalities = [text image], want [text]
    (只有 openai_responses 部署时不得 advertise image)
--- FAIL: TestValidateRoute_CapabilityVocabulary (0.00s)
    validate_edges_test.go:372: unknown: accepted an unsupported capability [vision]
    validate_edges_test.go:372: case: accepted an unsupported capability [Tools]
    validate_edges_test.go:372: plural: accepted an unsupported capability [images]
    validate_edges_test.go:372: mixed: accepted an unsupported capability [tools vision]
    validate_edges_test.go:372: blank: accepted an unsupported capability [ ]
--- FAIL: TestValidateCatalogForPublish_CapabilityVocabulary (0.00s)
    validate_edges_test.go:396: publish accepted a route with an unknown capability
```

**Protection evidence (already-correct behaviour, recorded instead of forced
changes).** Three cases passed at the pre-fix baseline and pin behaviour the
fix must NOT change:

| Case | Guard that already held |
|---|---|
| `TestRouteCovers_LegacyCapabilityLists` | an empty capability list still means "everything"; a restrictive list grants exactly what it declares |
| `TestParseSnapshot_UnknownRouteCapabilityStaysLoadable` | an already-published revision with an unknown capability name stays loadable (no catalog bricking) and the stored row is not rewritten |
| `TestV1Responses_ReasoningOutputItemDoesNotBreakTheChain` | the Responses surface emits a reasoning output item, the stored transcript excludes it, `previous_response_id` chaining works, and a manual echo of a reasoning item is an explicit 400 |

##### 4.39.3 The fix

| Finding | Fix | Files |
|---|---|---|
| 1 — OpenAPI contradicted the shipped `minimal` semantics | the Responses `reasoning.effort` description now states that `minimal` is the lowest **reasoning level** and only `none` turns reasoning off; the OpenAPI contract test pins `reasoning_effort` and **fails** on the stale `none/minimal` wording | `docs/api/kaya-coding-plan.openapi.yaml`, `internal/inference/httpapi/openapi_contract_test.go` |
| 2 — the picker advertised images dispatch refuses | the advertised modalities are now "declaration ∩ dispatch capability", computed by the **same** decision the dispatcher uses (`routing.CompatibleDeployments`); the shared helper is `domain.AdvertisedModalities` and the oracle is `routing.ImageCapabilityOracle` (implemented by `routing.Service`, defaulting to the production adapter registry; cmd/server wires the routing service so both sides share one adapter set) | `internal/inference/routing/service.go`, `internal/inference/domain/model.go`, `internal/inference/httpapi/kaya_models.go`, `internal/service/chat_facade.go`, `cmd/server/main.go` |
| 3 — the route capability vocabulary was free-form | the vocabulary is closed (`tools`/`reasoning`/`image`, one definition in `domain`); the **write** path (operator CRUD) and the **publish** gate reject an unknown name with a clear error, the write path normalizes (trim/drop-empty/dedupe, no case folding); the **read** path tolerates unknown names (an old published revision must stay loadable) and the stored row is never rewritten | `internal/inference/domain/model.go`, `internal/inference/catalog/service.go`, `internal/inference/routing/service.go` |
| 4 — the emit-vs-echo asymmetry and the Anthropic-only effort correction | documentation only (the behaviour itself is spec-compliant and already pinned by a focused regression test): the rollout runbook now states the emit-vs-echo asymmetry and the real second-turn risk for clients that echo output items, the Anthropic-only `reasoning.effort` 400, and the deliberate narrowing of the published non-empty capability lists | `docs/runbooks/kaya-coding-plan-rollout.md`, `docs/runbooks/kaya-coding-plan-operations.md`, `docs/api-integration-guide.md` |

##### 4.39.4 Compatibility notes (the fix round)

- **The published non-empty capability lists are narrowed on purpose.** A
  stored list such as `["tools","reasoning"]` does not mention `image`, so
  that route no longer serves image-bearing requests, and a model whose
  routes are all of that shape no longer advertises the image modality. The
  empty list (the storage default) is unchanged. The remedy (add `"image"`
  explicitly) is in the operations runbook; the narrowing is called out in
  the rollout runbook and the compatibility report.
- **Old rows are never rewritten.** The write path normalizes only what is
  written from now on; the read path ignores an unknown capability name
  instead of failing the whole revision (an upgrade must not brick the
  catalog) and the publish gate refuses a draft that carries one — loudly,
  with the route id and the offending name.
- **`previous_response_id` chains are unchanged** and stay covered by
  `TestV1Responses_ReasoningOutputItemDoesNotBreakTheChain`.

#### 4.39 Notes (root cause, compatibility, honesty, unresolved boundaries)

- **Root cause (three classes).** (1) The image capability existed only in the
  adapter's payload builder and the model declaration — the **routing layer
  had no image dimension**, so the incompatible candidate was *selected* and
  the failure came after admission. (2) The reasoning effort was modelled as
  a **boolean** (`ThinkingEnabled`): the client's level was dropped on the
  floor and the adapter sent its own default (`medium`) — the "fold
  everything to medium" behaviour. (3) The protocol surfaces treated
  "cannot replay faithfully" as "accept and drop": a signed thinking block, a
  redacted thinking block and a Responses reasoning item were silently
  swallowed, and the gateway's own Messages output faked `signature: ""`.
- **The capability contract is now three layers deep and single-sourced**:
  model declaration (`input_modalities`) → route capabilities
  (`capabilities`) → adapter capabilities (`AdapterCapabilities.Images` /
  `.ReasoningEfforts`). The routing package owns the **one** filter
  (`CompatibleDeployments`), and both the candidate builder and the
  gateway's pre-admission gate call it — they cannot drift apart.
- **The pre-admission gate is a capability statement, not a capacity
  statement.** When the model has **no** compatible deployment at all, the
  request keeps its previous semantics (`upstream_unavailable`, 503). Only
  the case "the base demand has a candidate but the extra demand (image /
  effort) has none" is the new explicit 400.
- **Compatibility corrections (all documented in the integration guide and
  the OpenAPI spec):**
  1. `/v1/messages`: `thinking.type=enabled` → **400**; a signed/redacted
     thinking block in the history → **400**; the surface no longer renders a
     `thinking` block (the old `signature: ""` was a fake). Ordinary
     text/tool flows are unchanged.
  2. `/v1/responses`: a `reasoning` input item → **400** (it has no
     replayable encrypted payload; `include=reasoning.encrypted_content` was
     already rejected). The gateway's own `previous_response_id` chain is
     unaffected: the stored transcript never contains reasoning items, so a
     chained call still works.
  3. `/v1/chat/completions`: a **server-side** tool type
     (`web_search`/`computer`/`code_execution`/`mcp`…) → **400** (the matrix
     already promised it; the code did not enforce it).
  4. `reasoning.effort=minimal` is now the lowest **reasoning level**
     (OpenAI semantics) instead of "reasoning off" — a documented
     compatibility correction; only `none` turns reasoning off.
  5. The legacy `/chat` surface answers the new capability rejection with a
     400-class sentinel instead of the "upstream rejected" 502 (nothing was
     sent; blaming the upstream and inviting a doomed retry would be wrong).
- **The image token estimate is an estimate, not a universal upper bound.**
  Admission reserves 1105 tokens per image (OpenAI's high-detail worst case,
  85 + 170×6 tiles) and settlement uses the **upstream-reported** usage
  (`source=reported`); the estimate is used only on the estimated path
  (`source=estimated`, correctable by later evidence). **Unresolved
  supplier boundary:** the visual-token metering convention is the
  **supplier's** (tiling, scaling, whether cache/reasoning buckets are
  included differ per vendor, so the same image can be metered several times
  over). This gateway **cannot** verify a supplier's convention — it can only
  relay the reported value faithfully, and the hold-vs-settled difference is
  booked honestly by the settlement overage path.
- **Preserved (T1–T7):** the OAuth refusal boundaries (T1), the accounting /
  one-charge-per-request semantics (T2), the scheduling and provider-disable
  rules (T3), the RPM/TPM admission (T4), the concurrency leases (T5), the
  protocol-native error shapes (T6) and the connect-timeout / SSRF
  behaviour (T7) are untouched — the same suites pass, and the T8 change
  adds no transaction, no migration and no new dependency.
- **Not built (deliberately):** no generic lossless native protocol
  passthrough layer, and `/v1` is still a **text subset** — images, files,
  audio and server-side tools are rejected natively on all three entries with
  zero admission and zero dispatch.

### T9 — Probe, capability description and operational visibility

Recorded on 2026-11-06, base SHA `63bdbd3` (the T8 branch tip; T0 base
`35cf836`). Task report:
`.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-9-report.md`.

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the
instance down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`;
every database-backed command used `-p 1` and `-race`; `go.mod` / `go.sum`
were never modified (`git diff --stat go.mod go.sum` → empty after every
run).

#### 4.40 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 2 | `go build ./...` | **0** | build green (no new dependency) |
| 3 | `go run ./cmd/migrate` (run 1, `t9_app`) | **0** | `applied=53 skipped=0` |
| 4 | `go run ./cmd/migrate` (run 2, `t9_app`) | **0** | `applied=0 skipped=53` (idempotent) |
| 5 | **RED** `go test -count=1 -run 'TestProbeDeployment_\|TestPublishDryRun_' -v ./internal/inference/management` | **1** | the leaks reproduced — see §4.41 |
| 6 | **RED** `go test -count=1 -run 'TestReserveRateTx_' -v ./internal/inference/quota` | **1** | the rate-store failure was silent — see §4.41 |
| 7 | **RED** `go test -count=1 -run 'TestRecover_UnknownUsageBacklogAlarms\|TestRecover_NoUnknownUsageBacklogNoAlarm' -v ./internal/inference/workers` | **1** | the backlog was counted but never alarmed — see §4.41 |
| 8 | **RED** `DATABASE_URL=<disposable> go test -count=1 -run 'TestKayaModels_CapabilityDescriptionIsAccurate' -v ./internal/inference/httpapi` | **1** | the capability fields did not exist — see §4.41 |
| 9 | **GREEN** the same four commands after the fix | **0** | all T9 cases pass |
| 10 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/management ./internal/inference/httpapi ./internal/config ./cmd/server` | **0** | 4 packages `ok` (2.440s / 82.775s / 1.661s / 2.331s) |
| 11 | `go test -race -count=1 -p 1 -v ./internal/inference/... ./tests/integration/...` | **0** | 15 packages `ok`; **1113 PASS / 3 SKIP / 0 FAIL** (see §4.45) |
| 12 | `go test -race -count=1 ./tests/e2e/...` (`E2E_DATABASE_URL` → `t9_e2e`) — **first run** | **1** | the e2e byte-exact `/chat/models` contract caught the appended fields — see §4.49 |
| 12b | the same e2e command after the contract fix | **0** | `ok github.com/yunhou/users/tests/e2e` (see §4.49) |
| 13 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (`GATE_EXIT=0`; the generated `coverage.out` was deleted afterwards) |
| 14 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.3%` ≥ the 80% floor (T0 80.4%, T1 80.5%, T2 80.3%) |

#### 4.41 RED evidence (before any production edit)

The four RED commands ran against the **unmodified** production tree at
`63bdbd3`. Every new test was written against the **existing** API surface
only — the probe/dry-run results are asserted through their **JSON shape**,
the capability fields through the **HTTP response body**, and the alarms
through the **standard log stream** — so every failure is a behavioural
assertion failure, never a compile error.

**Probe (management)** — the probe was protocol-blind, credential-blind and
outcome-blind:

```
=== RUN   TestProbeDeployment_AnthropicUsesProtocolAuthAndVersionHeaders
    probe_t9_test.go:104: anthropic probe must use the x-api-key slot, got ""
    probe_t9_test.go:107: anthropic probe must send the protocol version header
    probe_t9_test.go:110: anthropic probe must not send an Authorization bearer header, got "Bearer sk-anthropic-secret"
--- FAIL: TestProbeDeployment_AnthropicUsesProtocolAuthAndVersionHeaders (0.00s)
=== RUN   TestProbeDeployment_AnthropicOAuthRefusedBeforeDispatch
    probe_t9_test.go:137: an oauth credential must not be dispatched to anthropic, hits=1
--- FAIL: TestProbeDeployment_AnthropicOAuthRefusedBeforeDispatch (0.00s)
=== RUN   TestProbeDeployment_VendorWithoutModelsIsUnsupportedNotFault
    probe_t9_test.go:197: probe_outcome = "", want unsupported (供应商未提供 /models)
    probe_t9_test.go:206: description "" must state that this is not evidence of an unhealthy model
--- FAIL: TestProbeDeployment_VendorWithoutModelsIsUnsupportedNotFault (0.00s)
=== RUN   TestProbeDeployment_SeparatesCredentialProbeAndInference
    probe_t9_test.go:229: credential_state = "", want decrypted (材料可解密)
    probe_t9_test.go:232: probe_method = "", want connectivity
    probe_t9_test.go:235: probe_outcome = "", want verified (指定探测成功)
    probe_t9_test.go:238: probe_supported must be true when the endpoint answered
    probe_t9_test.go:244: description must be present (old fields stay, the description is appended)
--- FAIL: TestProbeDeployment_SeparatesCredentialProbeAndInference (0.00s)
=== RUN   TestProbeDeployment_MissingCredentialIsUnconfirmed
    probe_t9_test.go:271: credential_state = "", want missing
    probe_t9_test.go:274: probe_method = "", want none
    probe_t9_test.go:277: probe_outcome = "", want unconfirmed
--- FAIL: TestProbeDeployment_MissingCredentialIsUnconfirmed (0.00s)
=== RUN   TestProbeDeployment_UnusableCredentialIsUnconfirmed
    probe_t9_test.go:297: credential_state = "", want unusable
    probe_t9_test.go:300: probe_outcome = "", want unconfirmed
--- FAIL: TestProbeDeployment_UnusableCredentialIsUnconfirmed (0.00s)
--- PASS: TestProbeDeployment_OpenAIOAuthUsesBearer (0.00s)      ← protection evidence
--- PASS: TestProbeDeployment_OK / _MissingV1Shape / _ConnectionRefused / _Timeout /
          _SecretNeverLeaks / _ResolveFailureNoLeak / _NoCredential /
          _RevokedSkippedRotatingUsed / _EgressRejected / _EgressNilFailsClosed /
          _NilClientFailsClosed / _EmptyBaseURL / _NotFound           ← protection evidence
FAIL	github.com/yunhou/users/internal/inference/management
```

Root cause: the probe sent `Authorization: Bearer <token>` to **every**
protocol (no version header, no API-key slot), ignored the credential's
`auth_type` entirely (an OAuth credential aimed at `anthropic_messages` was
dispatched), and reported a single boolean — so a vendor without `/models`
looked exactly like a broken model, and "the credential decrypted", "the
upstream answered" and "the model answered" were indistinguishable.

**Publish dry-run (management)** — the carried T3 finding:

```
=== RUN   TestPublishDryRun_DisabledProviderDeploymentIsNotCountedNorProbed
    publish_dryrun_t9_test.go:134: a deployment behind a disabled provider must not be probed: [dep-off dep-on]
--- FAIL: TestPublishDryRun_DisabledProviderDeploymentIsNotCountedNorProbed (0.00s)
=== RUN   TestPublishDryRun_IssuesSeparateBlockersFromNetworkHints
    publish_dryrun_t9_test.go:211: issues must report deployment_no_account, got map[]
    publish_dryrun_t9_test.go:216: issues must report the network hint probe_unsupported, got map[]
    publish_dryrun_t9_test.go:221: summary probe buckets = {ProbesTotal:0 …} (want total=1 ok=0 unsupported=1)
    publish_dryrun_t9_test.go:227: summary.hints = 0, want ≥1
    publish_dryrun_t9_test.go:230: account_inventory_checked must be true when the inventory was read
--- FAIL: TestPublishDryRun_IssuesSeparateBlockersFromNetworkHints (0.00s)
=== RUN   TestPublishDryRun_AccountInventoryUnknownIsNotGuessed
    publish_dryrun_t9_test.go:279: issues must report account_inventory_unavailable, got map[]
--- FAIL: TestPublishDryRun_AccountInventoryUnknownIsNotGuessed (0.00s)
=== RUN   TestPublishDryRun_ModelWithoutDispatchableRouteIsABlocker
    publish_dryrun_t9_test.go:306: issues must report model_not_dispatchable, got map[]
--- FAIL: TestPublishDryRun_ModelWithoutDispatchableRouteIsABlocker (0.00s)
=== RUN   TestPublishDryRun_RouteCapabilityNarrowingIsReported
    publish_dryrun_t9_test.go:346: issues must report route_capabilities_narrowed, got map[]
--- FAIL: TestPublishDryRun_RouteCapabilityNarrowingIsReported (0.00s)
--- PASS: TestPublishDryRun_ProbesActiveDeploymentsOnly / _ProbeFailureNeverBlocks /
          _ValidationFailurePropagates / _NilProberFailsClosed /
          _NextRevisionFollowsHistory                                     ← protection evidence
```

Root cause: `PublishDryRun` filtered the preview on `deployment.status`
**only** — it never read the provider's published status (the T3 rule), so a
deployment behind a disabled provider was counted as "will take traffic" and
was **live-probed** (an outbound call to an upstream that can never be
scheduled). The report had no issue list and no summary at all: HTTP 200
carried no verdict.

**Rate-store failure alarm (quota)**:

```
=== RUN   TestReserveRateTx_StoreFailureAlarms
    rate_alarm_t9_test.go:45: a rate-store failure must raise an ALARM line, log = ""
--- FAIL: TestReserveRateTx_StoreFailureAlarms (0.00s)
--- PASS: TestReserveRateTx_RateRejectionIsNotAnAlarm (0.00s)       ← protection evidence
```

Root cause: the rate reservation failure was surfaced only as the admission's
error code (`service_unavailable`). The distinction between "the rate store
is down" (an outage) and "the account is rate limited" (normal traffic) was
invisible to the operator — the fail-closed behaviour was right, but it was
silent.

**Unknown-usage backlog alarm (workers)**:

```
=== RUN   TestRecover_UnknownUsageBacklogAlarms
    backlog_alarm_t9_test.go:71: an unknown-usage backlog must raise an ALARM line, log = "… inference_recovery pass: scanned=0 … backlog=0 unknown_usage=11 dangling_holds=0 …"
--- FAIL: TestRecover_UnknownUsageBacklogAlarms (0.00s)
--- PASS: TestRecover_NoUnknownUsageBacklogNoAlarm (0.00s)          ← protection evidence
```

Root cause: the recovery pass counted the unknown-usage requests into its
structured metrics line but never **alarmed** — a backlog nobody is told
about is a backlog nobody handles.

**Capability description (httpapi)**:

```
=== RUN   TestKayaModels_CapabilityDescriptionIsAccurate
    kaya_models_t9_test.go:149: vision-chat supports_images = <nil>, want true (openai_chat 承载图片)
    kaya_models_t9_test.go:152: vision-chat supports_tools = <nil>, want true
    kaya_models_t9_test.go:155: vision-chat supports_thinking = <nil>, want true
    kaya_models_t9_test.go:158: vision-chat context_tokens = <nil>, want 200000
    kaya_models_t9_test.go:161: vision-chat max_output_tokens = <nil>, want 8192
    kaya_models_t9_test.go:164: capability_description missing: map[default:true display_name:vision-chat id:vision-chat input_modalities:[text image] provider:zhipu]
    kaya_models_t9_test.go:173: text-responses supports_images = <nil>, want false (该协议不承载图片)
    kaya_models_t9_test.go:176: text-responses supports_tools = <nil>, want true
--- FAIL: TestKayaModels_CapabilityDescriptionIsAccurate (0.12s)
```

Root cause: the picker only knew the **declared** input modalities; it could
not say whether a model could carry tools or reasoning, and it never published
the context/output hard limits the admission enforces.

#### 4.42 GREEN evidence

Command #9 (the same four commands, production code fixed) — exit code **0**:

```
ok  	github.com/yunhou/users/internal/inference/management	0.998s
ok  	github.com/yunhou/users/internal/inference/quota	0.935s
ok  	github.com/yunhou/users/internal/inference/workers	0.713s
ok  	github.com/yunhou/users/internal/inference/httpapi	1.529s
```

Acceptance command #10 — exit code **0**:

```
ok  	github.com/yunhou/users/internal/inference/management	2.440s
ok  	github.com/yunhou/users/internal/inference/httpapi	82.775s
ok  	github.com/yunhou/users/internal/inference/config	1.661s
ok  	github.com/yunhou/users/cmd/server	2.331s
```

#### 4.43 Probe semantics (the contract T9 pins)

| 维度 | 字段 | 取值 |
|---|---|---|
| 凭据**材料**测试 | `credential_state` | `decrypted` / `missing` / `unusable` |
| **指定**连通性探测 | `probe_method` + `probe_outcome` | `connectivity`+`verified` / `failed` / `unsupported` / `unconfirmed` |
| 本次**没有**做真实推理 | `inference_verified` | 恒为 `false` |

- `ok=true` **只**表示「这条部署的上游连通」；`inference_verified=false`
  显式发布出来，免得 `ok` 被读成「模型答过」。
- **供应商不提供 `/models`**（404/405/501）→ `unsupported`：此探测方式
  **无法确认**；**不是**模型故障（`base_url` 缺一段路径也会得到同样的 404，
  摘要与 `description` 都点明这一点）。
- 鉴权/版本头**按协议**设置，与派发路径**同一个适配器**：
  `openai_chat`/`openai_responses` → `Authorization: Bearer`；
  `anthropic_messages` → `x-api-key` + `anthropic-version`。
  `anthropic_messages` + OAuth 是**明确不支持**的组合 → 出站前拒绝，绝不
  把 OAuth 令牌塞进 API-key 槽（T1 的明确拒绝在探测面同样成立）。
- 探测**只读**、零写库、**默认零付费**（读的是供应商的模型列表端点）。

#### 4.44 Dry-run semantics (the contract T9 pins)

- **停用 provider 的部署既不算 active，也不被探测**（T3 携带的必修发现）；
  `would_publish.disabled_providers` /
  `deployments_behind_disabled_providers` 如实汇总。
- `issues[]` 分两类：`severity=blocker`（`provider_disabled` /
  `deployment_no_account` / `route_capabilities_narrowed` /
  `model_not_dispatchable`）与 `severity=hint`（`account_inventory_unavailable`
  / `probe_failed` / `probe_unsupported` / `probe_unconfirmed`）。
- 账号清单**读不到**时判定被**跳过**（`account_inventory_unavailable`
  hint），报告说「未知」，**绝不**谎称「无账号」。
- **策略/路由不支持**的配置由**发布闸**拒绝（预演与正式发布同一错误），
  因此它出现在**错误**里而不是 200 报告里。
- **HTTP 200 不等于每个 `probe.ok` 都是 true**：`summary.all_probes_ok` 只在
  每个探测都 `verified` 时才为 true。

#### 4.45 Skips

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; needs `KAYA_QUOTA_WORKER=1` (`quota_concurrency_test.go:773`). The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and passes with real contention output. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1`. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |

The verbose inventory (command #11, `-v`, **full untruncated output**):
**1113 PASS / 3 SKIP / 0 FAIL** across the 15 packages
(`access`, `accounting`, `catalog`, `credentials`, `domain`, `gateway`,
`httpapi`, `management`, `postgres`, `providers`, `providers/connector`,
`quota`, `routing`, `workers`, `tests/integration`), every package `ok`.

**Database-test skips: 0** — identical to the T0 (§2.6), T1 (§4.4), T2
(§4.12), T3 (§4.20), T5 (§4.37), T6 (§4.28) and T8 inventories; the skip
lines were grepped for `postgres` / `no database` / `database unavailable`
and returned **nothing**:

```
[inventory] any skip mentioning postgres / no database / database unavailable
  (NONE — no database-backed test was skipped)
```

#### 4.46 Migration

**None.** T9 adds no migration: everything lives in the probe/dry-run
projections, the capability oracle and the log channels. The disposable
databases were migrated with `cmd/migrate` exactly as in §2.3 (53 files,
idempotent on the second run).

#### 4.47 Files changed

| File | Change |
|---|---|
| `internal/inference/management/probe.go` | protocol-aware probe (the adapter's own auth/version headers, the explicit `anthropic_messages`+OAuth refusal); `ProbeResult` gains `credential_state` / `probe_method` / `probe_outcome` / `probe_supported` / `inference_verified` / `description`; `ProbeStore` gains the account inventory; `ProbeService` implements `AccountInventory` |
| `internal/inference/management/catalog.go` | the publish preview filters on the **published provider status** (a disabled provider's deployments are neither counted nor probed), aggregates the determinable issues (`PublishIssue`) and the verdict summary (`PublishDryRunSummary`) |
| `internal/inference/providers/adapter.go` | `AdapterForProtocol` — the registry lookup the probe uses to pick the protocol's headers |
| `internal/inference/routing/service.go` | `CapabilityOracle` (images + tools + reasoning, all answered by the dispatch decision) and `ProductionCapabilities()` |
| `internal/inference/httpapi/kaya_models.go` | `/chat/models` appends `supports_images` / `supports_tools` / `supports_thinking` / `context_tokens` / `max_output_tokens` / `capability_description` |
| `internal/inference/quota/service.go` | the rate-store failure alarm (a rate **rejection** is not an alarm) |
| `internal/inference/workers/settlement_recovery.go` | the unknown-usage backlog alarm (`RecoveryConfig.UnknownUsageAlarmThreshold`, default 10) |
| `internal/inference/management/probe_test.go` | the fake resolver now carries the credential's `auth_type`; the fake store gains the account inventory |
| `internal/inference/management/probe_t9_test.go` | **new** — the T9 probe cases |
| `internal/inference/management/publish_dryrun_t9_test.go` | **new** — the T9 dry-run cases (incl. the carried disabled-provider finding) |
| `internal/inference/httpapi/kaya_models_t9_test.go` | **new** — the T9 capability-description case |
| `internal/inference/quota/rate_alarm_t9_test.go` | **new** — the rate-store alarm case (+ the "a rejection is not an alarm" guard) |
| `internal/inference/workers/backlog_alarm_t9_test.go` | **new** — the unknown-usage backlog alarm case (+ the "no backlog, no alarm" guard) |
| `docs/api/kaya-coding-plan.openapi.yaml` | the probe result, the publish preview report and `/chat/models` (with the capability fields) are now documented; the new enums are pinned |
| `docs/api-integration-guide.md` | the `/chat/models` capability fields; the new "目录发布与上游探测" operator section |
| `docs/runbooks/kaya-coding-plan-operations.md` | the publish-preview semantics, the probe semantics and the four operational alarms (with their greppable log prefixes) |
| `docs/runbooks/kaya-coding-plan-rollout.md` | the stale "凭据 test 端点当前为结构化占位" pre-launch todo corrected (T9 made it a real protocol-aware probe) |

#### 4.48 Notes (root cause, compatibility, honesty, operational limits)

- **Root cause (four independent gaps, one theme):** every operational
  surface reported a **boolean** where the truth is a **distinction**. The
  probe reported "ok" for "the credential decrypted", "the upstream answered"
  and "the model answered"; the dry-run reported "200" for "the report was
  produced"; the picker reported a declaration where the dispatch decision
  was the truth; the rate-store outage and the unknown-usage backlog were
  counted but never announced. T9 replaces each boolean with the
  distinction it was standing in for, **without removing any old field**.
- **Compatibility.** No old field was removed or renamed: `ProbeResult`
  keeps `deployment_id` / `ok` / `status` / `latency_ms` / `error_summary`
  with their exact meaning, and the publish preview keeps `would_publish` /
  `probes`. Everything new is **appended** (`credential_state`,
  `probe_method`, `probe_outcome`, `probe_supported`, `inference_verified`,
  `description`, `issues`, `summary`, `disabled_providers`,
  `deployments_behind_disabled_providers`). The `/chat/models` entries gain
  fields but keep `id` / `display_name` / `provider` / `default` /
  `input_modalities`. **`/v1/models` is byte-for-byte unchanged** (native
  OpenAI `object=list` shape, no capability fields).
- **One behavioural change the operator must know:** a disabled provider's
  deployments are no longer counted in `would_publish.active_deployments`
  and are no longer probed. That is the carried T3 finding; the count now
  means "will actually carry traffic" instead of "the deployment row says
  active". `deployments_behind_disabled_providers` keeps the old number
  visible.
- **The publish gate is unchanged.** The dry-run still validates exactly
  like the formal publish and returns the **same error** for an invalid
  draft; the issues list covers the classes the gate does not catch.
  The formal publish still runs **no probe** and gains **no network
  dependency**.
- **Operational limits.** (1) The probe reads a **vendor model-list
  endpoint**: a vendor without one yields `unsupported` — the method
  cannot confirm, and the report says so. (2) A 404 is **ambiguous** by
  nature (no `/models`, or a `base_url` missing a path segment); the
  summary and the description name both possibilities instead of picking
  one. (3) The account inventory check is skipped (never guessed) when the
  read fails. (4) The alarms are **log lines** — the repository has no
  metrics facility, and T9 deliberately introduced none.
- **The credential resolver: a deliberate choice, stated plainly.** The
  probe consumes the T1 outbound-auth **material** through
  `credentials.Service.ResolveBearerToken` — the probe-side twin of
  `ResolveOutboundAuth`, which the T1 fix round made share ONE decode helper
  (`decodeOAuthAccessToken`), so an `oauth` credential yields the **access
  token alone** and an unusable bundle is rejected identically on both public
  paths. The probe additionally needs the credential's **`auth_type`** (it
  selects the protocol's header slot), and `ResolveBearerToken` already
  returns the complete `*domain.Credential` alongside the token. Calling
  `ResolveOutboundAuth` directly would have required moving the
  `OutboundAuth` type into `domain` (a type-level refactor of the hot
  path's contract) purely to satisfy the RED-first constraint, for no
  behavioural gain — the two resolvers return the same material for every
  credential type. The hot path (`gateway.SecretResolver`) is **untouched**
  and still calls `ResolveOutboundAuth`.
- **Not built (deliberately):** no generic lossless native protocol
  passthrough layer, no per-vendor "capability probe" beyond the model
  list, and no new monitoring platform.


#### 4.49 T9 fix round — the e2e byte-exact `/chat/models` contract

Recorded on 2026-11-06, after the **first** full T9 verification run.

**Finding.** The `go-e2e` CI job (a **separate** job with its own Postgres
service, §1.3) failed while everything else was green: the e2e contract test
pins `GET /chat/models` **byte-exactly**, and T9 legitimately appended the
capability fields to that body.

```
--- FAIL: TestChatContract_DualBackend (4.94s)
    --- FAIL: TestChatContract_DualBackend/facade/models (2.51s)
        --- FAIL: TestChatContract_DualBackend/facade/models (0.00s)
            chat_gateway_test.go:1497: /chat/models body = {"code":0,"data":{"models":[{"id":"deepseek-chat","display_name":"DeepSeek Chat","provider":"deepseek","default":true,"input_modalities":["text"],"supports_images":false,"supports_tools":true,"supports_thinking":true,"context_tokens":128000,"max_output_tokens":8192,"capability_description":"images: no; tools: yes; thinking: yes; context: 128000 tokens; max output: 8192 tokens"}]}}, want byte-exact {"code":0,"data":{"models":[{"id":"deepseek-chat","display_name":"DeepSeek Chat","provider":"deepseek","default":true,"input_modalities":["text"]}]}}
FAIL	github.com/yunhou/users/tests/e2e	192.503s
```

**Why this is a contract update, not a defect.** The brief authorises exactly
this: "/chat/models may only append accurate image/tool/thinking/context/
output limit fields". The **gateway** surface (`httpapi.KayaModelsHandler`)
now carries the six appended fields; the **legacy** picker
(`service.ChatGatewayFacade`) is **unchanged** and keeps its original five
fields. The e2e contract case runs against **both** backends (the strongest
way to keep the two client views identical), so the expectation now carries
the per-backend tail.

**The fix — test-only, no production change:**

| File | Change |
|---|---|
| `tests/e2e/chat_gateway_test.go` | `chatContractEnv` gains `modelsCapabilitySuffix` (the T9 capability tail). The **legacy** variant keeps it empty (its byte-exact five-field shape is unchanged); the **facade** variant pins the six appended fields with the values its fixture really produces (`openai_chat` adapter + empty route capability list + a text-only model → `supports_images=false`, `supports_tools=true`, `supports_thinking=true`, `context_tokens=128000`, `max_output_tokens=8192`). |

**Re-verification** — command #12b, exit code **0**:

```
ok  	github.com/yunhou/users/tests/e2e	308.985s
```

(The re-run is slower than the first because an unrelated Rust/Playwright
suite from another project was saturating the machine at the time; the
result is the same.)

**Compatibility.** The customer-visible `/chat/models` body **grew** (six
appended fields) — that is the sanctioned T9 change, and it is now pinned
byte-exactly on both backends. The legacy picker is byte-for-byte unchanged.

#### 4.50 T9 fix round — the four review findings

Recorded on 2026-11-06, after the T9 review of `a6a7226` raised four
**Important** findings. The fix round touches **one** production file
(`internal/inference/management/catalog.go`) plus the documentation and the
focused tests; **no** new prefix, gate or env knob was manufactured.

| # | Finding | Fix | Kind |
|---|---|---|---|
| 1 | `summary.account_inventory_checked` was `true` even when an inventory read **failed** — it misreported the very case it was built for | the flag now turns **false** on any failed provider read (it means "the no-account determination was made for **every** provider", not "we read once") | production + docs + test |
| 2 | the operations runbook advertised two settlement-alarm prefixes the gateway **never emits** (`ALARM gateway: reservation_drift` / `ALARM gateway: missing_reservation`) | the runbook now lists only the three real gateway prefixes and documents the **parked** family (no `ALARM gateway:` prefix, no reason token in the line) with its real lookup (the reconciliation job's `reason`, via `GET /admin/model-usage/exceptions?kind=settlement_backlog`) | docs + test |
| 3 | the unknown-usage threshold was documented as operator-adjustable, but the server never populates it and no env knob exists | stated plainly as a **code-only** constant (default 10); the server always runs 10; the "negative disables it" path only applies to a custom embedding or a test. **No env knob was added.** | docs + code comment |
| 4 | the `blocker` class overstated what it does (it is advisory; the formal publish never reads the report) and the OpenAPI did not correct the reader | the OpenAPI, the integration guide and the operations runbook now say: the whole issue list is **advisory**; a `blocker` is an **actionable configuration / usable-traffic finding**; **HTTP 200 + a blocker does not itself refuse the publish**; the things that **do** refuse it are the **shared publish gate's** hard validation failures (unsupported `pool_strategy` / unknown route capability names), which make the dry-run return the **same error** as the formal publish. No hard gate was added for the blocker class. | docs + test |

##### 4.50.1 RED (the fixes stashed, the new tests kept)

The four fixed files were stashed (`git stash push -- internal/inference/management/catalog.go
docs/api/kaya-coding-plan.openapi.yaml docs/api-integration-guide.md
docs/runbooks/kaya-coding-plan-operations.md`) while the new tests stayed in
place — the same "tests in, fixes out" shape the T2 fix round used.

```
=== RUN   TestPublishDryRun_AccountInventoryUnknownIsNotGuessed
    publish_dryrun_t9_test.go:284: account_inventory_checked must be false when the inventory read failed (the no-account determination was skipped)
--- FAIL: TestPublishDryRun_AccountInventoryUnknownIsNotGuessed (0.00s)
FAIL	github.com/yunhou/users/internal/inference/management	1.496s

--- httpapi doc guards ---
=== RUN   TestOpenAPI_T9PublishPreviewAdvisorySemantics
    openapi_contract_test.go:333: openapi missing the advisory-semantics wording "共享发布闸"
    openapi_contract_test.go:333: openapi missing the advisory-semantics wording "HTTP 200 + 一条 `blocker` 也不等于正式发布会被拒"
    openapi_contract_test.go:348: account_inventory_checked description must say "读失败"
--- FAIL: TestOpenAPI_T9PublishPreviewAdvisorySemantics (0.00s)
=== RUN   TestOpsRunbook_AlarmPrefixesMatchTheGateway
    openapi_contract_test.go:373: the runbook advertises the non-existent alarm prefix "ALARM gateway: reservation_drift"
    openapi_contract_test.go:373: the runbook advertises the non-existent alarm prefix "ALARM gateway: missing_reservation"
    openapi_contract_test.go:393: the runbook must document the parked-reason lookup "parking in reconciliation"
    openapi_contract_test.go:402: the runbook must state the threshold is code-only (missing "代码内常量")
--- FAIL: TestOpsRunbook_AlarmPrefixesMatchTheGateway (0.00s)
FAIL	github.com/yunhou/users/internal/inference/httpapi	0.799s
```

Every failure is a behavioural/doc assertion failure — never a compile error.

##### 4.50.2 GREEN

```
=== GREEN run ===
ok  	github.com/yunhou/users/internal/inference/management	0.578s
ok  	github.com/yunhou/users/internal/inference/httpapi	1.099s
```

##### 4.50.3 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `git diff --check` | **0** | no whitespace errors |
| 2 | `go run ./cmd/migrate` (run 1, `t9_app`) | **0** | `applied=53 skipped=0` |
| 3 | `go run ./cmd/migrate` (run 2, `t9_app`) | **0** | `applied=0 skipped=53` (idempotent) |
| 4 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/management ./internal/inference/httpapi ./internal/config ./cmd/server` | **0** | 4 packages `ok` (2.014s / 69.800s / 2.587s / 2.666s) |
| 5 | the same four packages with `-v` (skip inventory) | **0** | **333 PASS / 0 SKIP / 0 FAIL**; the skip lines were grepped for `postgres` / `no database` / `database unavailable` → **nothing** |
| 6 | `go test -race -count=1 -p 1 ./internal/inference/... ./tests/integration/...` | **0** | 15 packages `ok` (regression sweep) |

##### 4.50.4 Files changed (the fix round)

| File | Change |
|---|---|
| `internal/inference/management/catalog.go` | a failed provider-account inventory read now turns `summary.account_inventory_checked` **false** (Finding 1) + the field's doc comment |
| `internal/inference/management/publish_dryrun_t9_test.go` | the RED assertion for Finding 1 (the flag must be false when the read failed) |
| `internal/inference/httpapi/openapi_contract_test.go` | two focused drift guards: `TestOpenAPI_T9PublishPreviewAdvisorySemantics` and `TestOpsRunbook_AlarmPrefixesMatchTheGateway` (Findings 2–4) |
| `internal/inference/workers/settlement_recovery.go` | the threshold's doc comment now says plainly that there is **no** env / config knob and that the server always runs the default 10 (Finding 3) |
| `docs/api/kaya-coding-plan.openapi.yaml` | the advisory semantics, the shared publish gate as the only hard rejection, and the corrected `account_inventory_checked` meaning (Findings 1 + 4) |
| `docs/api-integration-guide.md` | the same two corrections in the operator section (Findings 1 + 4) |
| `docs/runbooks/kaya-coding-plan-operations.md` | the real gateway/worker alarm prefixes and the parked-reason lookup (Finding 2), the code-only threshold (Finding 3), the advisory semantics (Finding 4) |

##### 4.50.5 Notes (compatibility, honesty)

- **No new prefix was manufactured.** The gateway's alarm family is unchanged:
  `ALARM gateway: settlement_mismatch` / `ledger_drift` /
  `late_usage_after_release` are the only ones that carry a reason token;
  `reservation_drift` / `missing_reservation` / `unknown_usage` are
  **parked** (`gateway: settlement failed for … — parking in reconciliation`,
  no reason token) and are looked up by the reconciliation job's `reason`.
- **No hard gate was added.** The controller ruling stands: the shared
  publish gate owns the hard rejections (T7's shared validation); the
  dry-run's `blocker` class is an **actionable configuration / usable-traffic
  finding**, and HTTP 200 plus a blocker-labelled issue does not by itself
  refuse the formal publish.
- **No env knob was added** for the unknown-usage threshold (explicitly out
  of scope); it is a code-level constant of 10.
- **Compatibility.** `account_inventory_checked` keeps its name and type; it
  is now *accurate* rather than optimistic (it can flip from `true` to
  `false` when a read fails). The OpenAPI's field list is unchanged.
- **Deferred minors were not touched.**

### T10 — Integration acceptance and implementation report

Recorded on 2026-11-07, base SHA `babc2c8` (the T9 branch tip; T0 base
`35cf836`). Task report:
`.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-10-report.md`.

**Environment for every row below.** Same discipline as T1–T9: one **fresh
disposable PostgreSQL per run**, created with the trap-protected recipe of
§1.2 (free port proven free before the instance exists, `initdb` + `pg_ctl` +
`createdb`, `DATABASE_URL` and `E2E_DATABASE_URL` exported explicitly,
`EXIT/INT/TERM` trap tears the instance down). Every run used `GOPROXY=off`
and `GOFLAGS=-mod=readonly`; every database-backed command used `-p 1` and
`-race`; `go.mod` / `go.sum` were never modified (`git diff --stat go.mod
go.sum` → empty after every run).

**T10 added no production code.** The two new files are test-only
(`tests/integration/gateway_acceptance_test.go`,
`tests/integration/gateway_multinstance_test.go`); no `internal/` file, no
migration and no OpenAPI line changed. That is deliberate: T10 is the
acceptance task, and the plan puts the acceptance **fixtures** in
`tests/integration/` (§ "Create/Modify").

#### 4.51 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go build ./...` | **0** | build green (no new dependency) |
| 2 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 3 | `go run ./cmd/migrate` (run 1, `t10_app`) | **0** | `applied=53 skipped=0` |
| 4 | `go run ./cmd/migrate` (run 2, `t10_app`) | **0** | `applied=0 skipped=53` (idempotent) |
| 5 | `go run ./cmd/migrate -status` | **0** | `001`…`053` all ✅ |
| 6 | **named-case gate** `go test -race -count=1 -p 1 -v -run 'TestExecutedUnknown_NoBlindReplay\|TestFailoverOn429_SettlesOnce\|TestSettleOverHold_OverageJobAndRealCharge\|TestZeroUsageSettle_ChargeRowDistinguishable\|TestListStaleOpenRequests_LiveLeaseGuard\|TestCorrectSettlement' ./internal/inference/gateway/... ./internal/inference/postgres/...` | **0** | **20 PASS / 0 FAIL / 0 SKIP** (4 gateway + 16 postgres) — §4.53 |
| 7 | `go test -race -count=1 -p 1 ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (`tests/integration` 47.899s) |
| 8 | `go test -race -count=1 -p 1 -v ./internal/... ./cmd/... ./tests/integration/...` (skip inventory) | **0** | 33 packages `ok`; **2637 top-level PASS + 1470 sub-test PASS / 3 SKIP / 0 FAIL** — §4.54 |
| 9 | `go test -race -count=1 -p 1 ./tests/e2e/...` — **first attempt** | **1** | **procedure error on my side**: the e2e job had its **own** database (`t10_e2e`) and I had migrated only `DATABASE_URL` (`t10_app`) → `relation "inference_audit_log" does not exist` in every e2e case. Not a product failure; re-run in row 10 with the e2e database migrated. The failing output is kept verbatim in §4.55. |
| 10 | `DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate` ×2, then `go test -race -count=1 -p 1 ./tests/e2e/...` | **0** / **0** / **0** | `applied=53 skipped=0`, then `applied=0 skipped=53`, then `ok github.com/yunhou/users/tests/e2e 139.323s` |
| 10b | `go test -race -count=1 -p 1 -v ./tests/e2e/...` (e2e skip inventory) | **0** | `ok … 136.614s`; **125 top-level PASS + 55 sub-test PASS / 1 SKIP / 0 FAIL** — §4.54 |
| 11 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was **deleted** afterwards — it is not a deliverable, see §4.57) |
| 12 | coverage floor: `go tool cover -func=coverage.out` → `awk 'BEGIN { exit !(t+0 >= 80) }'` | **0** | `total coverage: 80.3%` ≥ the 80% floor (T0 80.4%, T2 80.3%, T9 80.3%) |
| 13 | `git diff --check` | **0** | no whitespace error |
| 14 | `git diff --stat go.mod go.sum` | **0** | empty — the module files were never touched |

#### 4.52 The acceptance matrix T10 added

The T1–T9 evidence is per-package and per-behaviour. What was missing is a
**single matrix over the client entry points** with **database facts** as the
assertion, not HTTP bodies. T10 adds exactly that, in two files, reusing the
existing fixtures (`setupDB`, `writeOpenAIChunks`, `staticSnapshot`, `microc`)
— no second server stack was built.

`tests/integration/gateway_acceptance_test.go` — four client entry points
(`/chat` → `kaya_chat`, `/v1/chat/completions` → `openai_chat`,
`/v1/messages` → `anthropic_messages`, `/v1/responses` → `openai_responses`)
× seven scenarios. Every assertion reads the persisted facts
(`inference_requests`, `inference_attempts`, `inference_usage_records`,
`inference_ledger_entries`, `inference_reservations`,
`inference_rate_reservations`, `inference_concurrency_leases`,
`inference_reconciliation_jobs`, `inference_quota_windows`):

| Scenario | Test | What is asserted |
|---|---|---|
| success | `TestAcceptance_SuccessSettlesExactlyOncePerEntrypoint` | 1 request row `settled`/`reported`, **exactly one** ledger charge = `settled_micros`, 1 usage record (7/5 tokens), 3 window holds (4 with a Key) all `settled`, **0** held holds, **0** held leases, **0** reconciliation jobs, the three windows' `used_micros` equal the charge |
| rejection | `TestAcceptance_RejectionLeavesNoTrace` | an unentitled model → 403 on all four entry points with **zero** rows in all eight accounting tables and **zero** upstream calls (pre-admission rejection) |
| broken stream | `TestAcceptance_BrokenStreamParksWithoutSilentZero` | no usage read → `reconciliation_required`/`unknown`, **no** charge, a `unknown_usage` job, the hold **retained**; usage read before the break → `settled`/`estimated` with the unread bucket **NULL** (never 0) |
| interruption | `TestAcceptance_ClientInterruptionSettlesAndReleases` | the client write fails mid-stream → attempt `cancelled`, the same two settlement outcomes as above, and the **concurrency slots are given back** (`held` leases = 0) |
| retry / failover | `TestAcceptance_FailoverAfterRetrySettlesOnce` | candidate 1 answers 429 → candidate 2 serves → **2 attempts** (`failed`/`completed`) and **exactly one** charge + one usage record (a retry never bills twice) |
| unknown execution | `TestAcceptance_UnknownExecutionParksWithoutReplay` | the upstream hangs past the deployment timeout → the attempt is `unknown`/`transport`, the request parks (`reconciliation_required`, hold retained), and the **healthy backup candidate is never called** (no blind replay) |
| recovery | `TestAcceptance_RecoverySettlesParkedRequestOnce` | the recovery worker (clock +2 h) settles the parked request **once** at the conservative estimate (charge > 0 — never silently free), the hold becomes `settled`, the **second pass is a no-op**, and the upstream is **not** replayed |

`tests/integration/gateway_multinstance_test.go` — **two independent Service
instances over two independent DB pools**. The second instance has its own
`*sqlx.DB` pool, its own `postgres.Store`, its own `quota.Service` /
`routing.Service` / `gateway.Service` and its own gin engine; the only thing
they share is PostgreSQL. A process-local goroutine counter would fail every
one of these:

| Test | What is asserted |
|---|---|
| `TestAcceptance_TwoInstancesShareAccountRPM` | policy RPM = 2, three concurrent calls split across the two instances → at most 2 admitted, at least one `429 rate_limited`, and the **account rate rows in PostgreSQL are exactly 2** |
| `TestAcceptance_TwoInstancesShareAccountTPM` | policy TPM = 100; an in-flight stream on instance 1 holds 60 tokens → instance 2's 50-token request is refused `429 rate_limited` |
| `TestAcceptance_TwoInstancesShareAccountConcurrency` | policy concurrency = 1 → instance 2 is refused `429 insufficient_capacity` while instance 1 holds the slot |
| `TestAcceptance_TwoInstancesShareKeyConcurrency` | **Key scope**: `concurrency_limit = 1` on the customer Key → instance 2 is refused `429 insufficient_capacity`, and the held `api_key` lease is visible in `inference_concurrency_leases` |
| `TestAcceptance_TwoInstancesShareKeyRPM` | **Key scope**: Key RPM = 1 → the same Key on the *other* instance is refused; a **different** Key is unaffected; the key-scope rate rows are 1 and 1 |
| `TestAcceptance_TwoInstancesShareOneLedger` | two instances write **one** ledger: 2 charge rows, 2 request rows, and the three windows' `used_micros` = 2 × the charge |

#### 4.53 The named cases (row 6) — kept and green

All six names from the brief exist and pass; none was modified or deleted:

```
--- PASS: TestFailoverOn429_SettlesOnce (0.07s)                       [gateway]
--- PASS: TestSettleOverHold_OverageJobAndRealCharge (0.05s)          [gateway]
--- PASS: TestZeroUsageSettle_ChargeRowDistinguishable (0.05s)        [gateway]
--- PASS: TestExecutedUnknown_NoBlindReplay (0.35s)                   [gateway]
--- PASS: TestListStaleOpenRequests_LiveLeaseGuard (0.04s)            [postgres]
--- PASS: TestCorrectSettlement_UpdatesTheLedgerAndTheTokenBudgetOnce (0.05s)
--- PASS: TestCorrectSettlement_MissingRateRowIsABenignAbsence (0.05s)
--- PASS: TestCorrectSettlement_PresentButMalformedRateRowIsNotABenignAbsence (0.09s)
--- PASS: TestCorrectSettlement_LedgerHalfFailureRollsBackTheTokenCorrection (0.05s)
--- PASS: TestCorrectSettlement_RateHalfFailureRollsBackTheLedgerCorrection (0.04s)
--- PASS: TestCorrectSettlement_RepeatedCorrectionIsIdempotent (0.05s)
--- PASS: TestCorrectSettlement_CorrectionKeepsTheOriginalSettlementInstant (0.04s)
--- PASS: TestCorrectSettlement_UnknownTokenEvidenceKeepsThePreviousTotal (0.04s)
--- PASS: TestCorrectSettlement_KnownTokenEvidenceRequiresTheCorrectedTotal (0.05s)
--- PASS: TestCorrectSettlementDebitPreservesHistory (0.05s)           [postgres]
--- PASS: TestCorrectSettlementCreditAndIdempotency (0.05s)           [postgres]
--- PASS: TestCorrectSettlementOverageAndJobResolution (0.04s)        [postgres]
--- PASS: TestCorrectSettlementOverageStaysVisible (0.04s)            [postgres]
--- PASS: TestCorrectSettlementRejectsWallet (0.04s)                  [postgres]
--- PASS: TestCorrectSettlement_ReversalAndDelta (0.05s)              [postgres]
ok  	github.com/yunhou/users/internal/inference/gateway	4.797s
ok  	github.com/yunhou/users/internal/inference/postgres	2.000s
```

#### 4.54 Skip inventory — audited, and **no database test skipped**

The verbose app-side run (row 8) reported exactly **three** skips, and the
verbose e2e run (row 10b) exactly **one**. **None of them is a database
skip** — every one is an explicitly opt-in drill or a manual entry point:

| Skip | Reason | Verdict |
|---|---|---|
| `TestLoadDrill_Throughput` | `load drill: set KAYA_LOAD=1 to run (非默认门禁)` | An **opt-in load gate**, not a missing database. It is a throughput drill with its own `KAYA_LOAD=1` switch; it is excluded from the default gate on purpose. |
| `TestLoadDrill_LongConnections` | `load drill: set KAYA_LOAD=1 to run (非默认门禁)` | Same. |
| `TestQuotaWorkerProcess` | `worker process entry point` | A **manual entry point** (it is a worker-process launcher, not an assertion). |
| `TestUsageTokenAcceptance_TenPagesTwoMinutes` (e2e) | `usage acceptance drill: set USAGE_ACCEPTANCE=1 to run (非默认门禁)` | An **opt-in acceptance drill** with its own switch, in the kaya usage-token area (not the model gateway). |

Every database-backed package ran against the disposable instance: the
`internal/inference/{access,accounting,catalog,credentials,domain,gateway,httpapi,management,postgres,providers,quota,routing,workers}` suites, `tests/integration` and `tests/e2e` all executed their DB cases — the acceptance matrix would have failed loudly otherwise, since `setupDB` uses `t.Fatalf` (a missing database is a **failure**, never a skip). The skip counts above are the complete inventory: **4 skips in total, 0 of them database-backed**.

#### 4.55 The one failing run, in full — and why it is not a product failure

Row 9 failed with `exit=1`. The **entire** failure mode was one sentence,
repeated by every e2e case:

```
--- FAIL: TestAdminUpstreamAccounts_FullLifecycleE2E (0.01s)
    admin_upstream_accounts_test.go:242: wipe inference_audit_log: pq: relation "inference_audit_log" does not exist at column 13 (42P01)
--- FAIL: TestE2E_AdminOpsAuthRequired (0.00s)
    admin_users_test.go:43: cleanup inference tables: pq: relation "inference_audit_log" does not exist (42P01)
--- FAIL: TestChatLegacy_RelayAndErrorContract (0.00s)
    chat_gateway_test.go:571: wipe inference_audit_log: pq: relation "inference_audit_log" does not exist at column 13 (42P01)
... (every e2e case failed the same way)
FAIL	github.com/yunhou/users/tests/e2e	1.526s
```

Root cause: **my procedure**, not the code. The e2e suite runs against
`E2E_DATABASE_URL` — a *different* database from the app-side one (§1.2
creates `t10_app` **and** `t10_e2e` precisely so the two suites cannot
contaminate each other). I had applied the migrations only to `DATABASE_URL`
(`t10_app`), so the e2e database had **no schema at all** and every e2e case
died in its own wipe step before touching the product.

The fix is the procedure, and it is exactly what CI's `go-e2e` job does
(§1.3: "apply migrations" is gate 1 of that job):

```
DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate   → applied=53 skipped=0
DATABASE_URL=$E2E_DATABASE_URL go run ./cmd/migrate   → applied=0 skipped=53
go test -race -count=1 -p 1 ./tests/e2e/...            → ok  github.com/yunhou/users/tests/e2e 139.323s
```

Nothing in the repository was changed to make the e2e suite pass; **no
production file was touched to "fix" this**. The lesson is recorded because it
is the exact hazard §1.2 warns about: a silent fallback DSN, or a migration
applied to the wrong database, looks like a product failure and is not.

#### 4.56 Migration

No new migration. T10 changed **no** schema: the acceptance fixtures only read
and assert the tables T1–T9 delivered.

- `migrations/050_inference_request_expected_hold.sql` — `inference_requests.expected_hold_micros` (the durable estimate upper bound) + the dedicated reconciliation reasons.
- `migrations/051_inference_reconciliation_reasons.sql` — the reason vocabulary (`unknown_usage`, `crash_recovery`, `ledger_mismatch`, `manual`, `settlement_overage`, `missing_reservation`, `late_usage_after_release`, `settlement_mismatch`, `insufficient_evidence`).
- `migrations/052_inference_rate_reservations.sql` — `inference_rate_reservations` (the persistent RPM/TPM coordination: `scope='account'|'key'`, `reserved_tokens` / `settled_tokens`, `UNIQUE(request_id, scope)`).
- `migrations/053_inference_route_pool_strategy_comments.sql` — comments only (no DDL).

Idempotency was verified twice on **both** databases (`applied=53 skipped=0`
then `applied=0 skipped=53`), and `go run ./cmd/migrate -status` printed ✅
for `001`…`053`.

#### 4.57 Files changed

| File | Change |
|---|---|
| `tests/integration/gateway_acceptance_test.go` | **new** — the acceptance fixture (two simulated upstreams, two models, one published policy, one entitlement, one customer Key, four client entry points) + the seven scenarios + the DB-fact reader (`readAccFacts`) |
| `tests/integration/gateway_multinstance_test.go` | **new** — the second independent instance (own pool / store / services / engine) + the six shared-limit cases (account RPM/TPM/concurrency, Key RPM/concurrency, one ledger) |
| `docs/runbooks/2026-11-05-model-gateway-verification.md` | this section |
| `docs/superpowers/plans/2026-11-05-users-model-gateway-completion.md` | the T0–T10 checkboxes + the "实际交付" appendix (tables / interfaces / compatibility / unsupported legacy configuration) |
| `.superpowers/sdd/2026-11-05-users-model-gateway-completion/task-10-report.md` | the task report |

**No production file changed.** `git status --short` after the run showed
exactly the two new test files (plus the two documentation files above).

#### 4.58 Notes (compatibility, boundaries, honesty, known limits)

- **What T10 proved.** Every client entry point ends in **one** charge, or
  in an **explicit** non-settled terminal state (`reconciliation_required`
  with a retained hold and an open job) — never a silent zero. The
  concurrency slots, the monetary holds and the TPM holds are all released
  or converted when the request ends; the rate counters are shared through
  PostgreSQL, so **two instances** see one account RPM/TPM/concurrency
  budget and one per-Key budget.
- **Compatibility.** T10 changed **no** interface: no new HTTP route, no new
  JSON field, no new error code, no new table, no OpenAPI line. What the new
  suite actually asserts about the error contract is **narrow**, and it is
  worth stating exactly: the **OpenAI `/v1` native shape**
  (`{"error":{"code":…}}`, decoded by `errCode`) for **two** of the three
  limit codes — `rate_limited` and `insufficient_capacity` — plus the HTTP
  status of a pre-admission rejection (403) on all four entry points and the
  `reconciliation_required` terminal state. It does **not** decode the
  Anthropic native shape, the `/chat` `{code,data,message}` envelope, or
  `quota_exceeded`; those belong to the **T6 protocol matrix** (§4.27), which
  is the contract's owner. The new files assert the **accounting** facts
  (request / attempt / usage / ledger / holds / leases), not the whole
  protocol matrix.
- **Unsupported legacy configuration — how it is handled today.** Three
  legacy shapes are *readable but not schedulable*, and the gateway says so
  instead of silently pretending: (1) a published route with
  `pool_strategy` outside the supported set (`least_loaded`, generic
  `session_sticky`) → the route is excluded from the candidate set, an
  alarm is raised, and if **no** candidate remains the call fails with an
  explicit configuration error *before* any hold or dispatch; (2) a policy
  with `overage_policy = clamp_if_declared` → the policy is still queryable,
  but a new inference against it is refused with
  `service_unavailable` ("not implemented") — it is **not** silently
  downgraded to `reject`, and the operator migrates it by publishing a new
  revision with `reject` or `allow_overage`; (3) a pre-`050` request row
  with no `expected_hold_micros` → it carries no recoverable estimate, so it
  is **parked for manual reconciliation**, never settled as free. The
  Responses `previous_response_id` session binding remains supported.
- **Known limits — stated, not glossed over.**
  1. **No real provider acceptance.** Every upstream in this run is an
     `httptest` server. Nothing here proves that a real OpenAI / Anthropic /
     Moonshot deployment bills the way the fixtures assume, and **no real
     Claude Code / Codex client was exercised** — the protocol surfaces were
     driven with hand-written request bodies. "Local simulation passes" is
     not "the real client is accepted".
  2. **No production deployment.** Nothing was deployed, migrated against a
     production database, or merged. The production prerequisites are listed
     in the task report (§ 上线前置).
  3. **Correction boundaries — one of them is a real remaining limitation.**
     The correction channel (`CorrectSettlement`) covers the **plan** path
     only; the wallet path is refused with `invalid_input`
     (`TestCorrectSettlementRejectsWallet`: "钱包更正通道未接线"). Two facts
     must be kept apart:
     - the **persistent rate reservation** *is* settled / released on the
       wallet terminal and recovery paths, exactly like the plan path —
       `ledger_repo.go` settles it in the wallet branch
       (`settleWallet` → `settleRateTx`) and `reservation_repo.go` releases
       it (`releaseRateTx`), so T4's literal "套餐与钱包的终态/恢复都释放或
       校正同一持久 rate reservation" is met for the rate side;
     - the **post-settlement correction of a wallet request** — the money
       reversal/adjustment *and* the token-budget correction — is **not
       wired**. An estimated wallet settlement that later gets real evidence
       cannot be corrected. The original T4 text asks for plan **and** wallet
       to release-or-correct the same persistent rate reservation, and the
       estimated→reported correction is part of that semantics, so this is a
       **real remaining limitation that needs a final review decision** — not
       something the plan excludes. (The "已知边界（本期不扩大）" note inside
       the T4 section of the plan is **branch-era text written by T4**, not a
       plan-level waiver, and must not be cited as one. Related and already
       recorded in §4.32: the money-side correction channel has **no
       production caller** at all — `CorrectRateTokensTx` is pinned by tests
       but driven only by the reconciliation queue's evidence window.)
     A settlement that exceeds its hold is **not** silently clamped — the real
     charge settles and the difference is filed as a `settlement_overage`
     reconciliation job. The recovery worker settles unknown consumption at
     the **conservative estimate** (the admission-priced upper bound); only a
     provider that supports per-request execution lookup could settle at the
     real figure, and no such capability is configured in production, so
     every recovered request waits for a human to confirm the real usage.
  4. **Protocol restrictions that remain.** The three `/v1` entry points are
     a **text subset**: images / files / audio and server-side tools are
     refused with a native 400 (zero hold, zero dispatch). `/chat` carries
     the image path, but only to models whose published deployment can
     carry it. Non-empty Responses `reasoning` / `encrypted` content and
     Anthropic signed / redacted thinking histories are **refused**, not
     accepted-and-dropped. There is no lossless native protocol passthrough
     layer yet.
  5. **Supplier metering boundaries.** The token figures the gateway bills
     are the **upstream's** reported usage when it reports any; when it
     reports none, the gateway falls back to its own estimate (input bound +
     content bytes), which is auditable (`source = 'estimated'`) but is
     **not** the supplier's real number. The image token estimate is an
     OpenAI-style worst case, **not** a mathematical upper bound for every
     model. The Anthropic `cache_read` / `cache_creation` buckets and the
     reasoning-token overlap are normalised by the adapter's inclusion
     declaration; a supplier that changes its usage semantics changes the
     bill.
  6. **Measured constraints, not guesses.** The whole
     `internal/... + cmd/... + tests/integration` suite takes ≈ 4 minutes
     under `-race -p 1` on this machine, of which `internal/inference/httpapi`
     alone is 55 s and `internal/util` 27 s; `tests/e2e` takes ≈ 140 s. The
     coverage floor has thin headroom (**80.3%** against 80%) — a change
     that adds code without tests fails CI even when every test passes.
     **No flake was observed in this run** (three consecutive full-suite
     runs of the acceptance matrix were green), but the load drills
     (`TestLoadDrill_*`) are excluded from the default gate and therefore
     carry **no** measured performance baseline — the numbers in
     `docs/runbooks/kaya-coding-plan-rollout.md` come from a separate
     campaign.
  7. **What the acceptance matrix does *not* cover.** It does not exercise
     the JWT/OAuth path (the e2e suite owns that), the management surfaces,
     the payment/refund flows, the `/v1/models` and `/chat/models` shapes
     (T9's e2e contract owns those), nor the **rate path across processes**.
     The repository **does** have a cross-process harness —
     `TestTwoProcessContendLastCredit`
     (`internal/inference/postgres/quota_concurrency_test.go:880`) re-execs
     the test binary through the `TestQuotaWorkerProcess` child entry point
     (`KAYA_QUOTA_WORKER=1`), each process with its own connection pool on the
     shared disposable database — but it exercises the **quota-window**
     contention path (the worker's policy carries only the three window
     limits: no RPM, no TPM, no concurrency cap). The **T4/T5 rate path**
     (account/Key RPM, account TPM, account and Key concurrency) is covered by
     T10's suite with **two in-process Service instances over two pools**, and
     **that suite was not itself exercised cross-process or cross-host**.
     The gap is actionable: re-use the same `KAYA_QUOTA_WORKER` re-exec
     pattern and give the worker an RPM/TPM/concurrency-limited policy.


### 4.59 Final-review fix wave — the parked-anomaly TPM leak, the rollout runbook, the wallet follow-up

Recorded on 2026-11-07, base SHA `cb6866e` (the final review's base). The
final review raised three findings; this section records the evidence for the
fix wave that closed them.

**Environment for every row below.** One **fresh disposable PostgreSQL per
run**, created with the trap-protected recipe of §1.2 (free port proven free
before the instance exists, `initdb`+`pg_ctl`+`createdb`, `DATABASE_URL` and
`E2E_DATABASE_URL` exported explicitly, `EXIT/INT/TERM` trap tears the
instance down). Every run used `GOPROXY=off` and `GOFLAGS=-mod=readonly`;
every database-backed command used `-p 1`; `go.mod` / `go.sum` were never
modified (`git diff --stat go.mod go.sum` → empty after the run).

#### 4.59.1 Commands / exit codes

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go build ./...` | **0** | build green (no new dependency) |
| 2 | `go vet ./internal/... ./cmd/...` | **0** | lint gate green |
| 3 | `go run ./cmd/migrate` (run 1, `ff_app`) | **0** | `applied=54 skipped=0` (53 baseline + 054) |
| 4 | `go run ./cmd/migrate` (run 2, `ff_app`) | **0** | `applied=0 skipped=54` (idempotent) |
| 5 | `go run ./cmd/migrate -status` (`ff_app`) | **0** | 54 × `✅` |
| 6 | `go run ./cmd/migrate` ×2 + `-status` (`ff_e2e`, `DATABASE_URL=$E2E_DATABASE_URL`) | **0** / **0** / **0** | the e2e-side database is migrated the same way |
| 7 | **RED** `go test -count=1 -run 'TestParkedAnomaly\|TestEvidenceDrivenPark' -v ./internal/inference/postgres` | **1** | the leak reproduced — see §4.59.2 |
| 8 | **GREEN** the same command | **0** | the store-level regressions pass |
| 9 | **GREEN** `go test -count=1 -run 'TestRecover_ParkedAnomaly' -v ./internal/inference/workers` | **0** | the recovery-level regressions pass |
| 10 | **GREEN** `go test -count=1 -run 'TestGateway_ParkedAnomaly' -v ./internal/inference/gateway` | **0** | the gateway-level regression passes |
| 11 | **acceptance** `go test -race -count=1 -p 1 ./internal/inference/postgres ./internal/inference/gateway ./internal/inference/workers ./internal/inference/quota ./internal/inference/access` | **0** | 5 packages `ok` |
| 12 | CI `go-ci` gate: `go test -race -count=1 -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | **0** | 33 packages `ok`, 0 `FAIL` (the generated `coverage.out` was deleted afterwards) |
| 13 | coverage floor: `go tool cover -func=coverage.out` | **0** | `total coverage: 80.3%` ≥ the 80% floor |
| 14 | `go test -race -count=1 -p 1 -v ./internal/inference/... ./tests/integration/...` (skip audit) | **0** | 1136 top-level PASS + 360 subtest PASS, **3 SKIP**, 0 FAIL — the three skips are the known non-database ones and **no** skip mentions postgres / no database |
| 15 | `go test -race -count=1 -p 1 -v ./tests/e2e/...` (`E2E_DATABASE_URL` → `ff_e2e`) | **0** | `ok github.com/yunhou/users/tests/e2e 137.597s` — 125 top-level PASS + 55 subtest PASS, **1 SKIP** (the pre-existing non-default gate `USAGE_ACCEPTANCE=1`), 0 FAIL |
| 16 | `git diff --check` | **0** | no whitespace errors |
| 17 | `git status --short` after the run | **0** | only the files of this wave (no generated artefact left behind) |

#### 4.59.2 RED — a parked anomaly permanently consumed the account's TPM

The RED run used the **unmodified production code at `cb6866e`**; the new
tests were written against the *existing* API surface only
(`Store.MarkReconciliationRequired`, `Store.ReserveRateTx`,
`Store.PurgeRateReservations`, the gateway's `ChatCompletions` and the
recovery worker's `RunPass`), so every failure is a behavioural assertion
failure, never a compile error.

```
=== RUN   TestParkedAnomaly_RateHoldLeavesTheLiveTPM
    rate_anomaly_test.go:49: scope account state = "reserved", want anomaly (停放必须显式处置速率预占，不得停在 reserved)
    rate_anomaly_test.go:49: scope key state = "reserved", want anomaly (停放必须显式处置速率预占，不得停在 reserved)
    rate_anomaly_test.go:55: in-flight tokens = 60, want 0 (停放的异常不得永久占用 TPM)
    rate_anomaly_test.go:102: a parked anomaly must not block the account TPM: inference/rate_limited: rate limit exceeded: blocked by policy_tpm; tpm_used=110 tpm_limit=100
--- FAIL: TestParkedAnomaly_RateHoldLeavesTheLiveTPM (0.06s)
=== RUN   TestParkedAnomaly_IsPurgedByTheRetentionSweep
    rate_anomaly_test.go:140: purged = 0, want 2 (处置后的速率行不再占额度，必须可被保留期清理)
    rate_anomaly_test.go:143: rate rows = 2, want 0 (处置后的速率行必须可被清理)
--- FAIL: TestParkedAnomaly_IsPurgedByTheRetentionSweep (0.05s)
=== RUN   TestEvidenceDrivenPark_KeepsTheRateHold
--- PASS: TestEvidenceDrivenPark_KeepsTheRateHold (0.05s)
FAIL	github.com/yunhou/users/internal/inference/postgres	0.825s
```

**Why this failure is the expected one.** The third line is the finding
verbatim: after a request was parked with the manual-only reason
`missing_reservation`, the account's in-flight TPM was still **60**, and the
next admission was rejected with `rate_limited … tpm_used=110 tpm_limit=100`
— the account is blocked by a request that can never settle. The retention
sweep removed **0** rows, because a `state='reserved'` row is never deleted.
`TestEvidenceDrivenPark_KeepsTheRateHold` already passed at the baseline: it
is **protection evidence** — an evidence-driven park (`unknown_usage`) must
keep its hold, because the recovery will settle that request at the
conservative estimate.

#### 4.59.3 GREEN

Command #8 (same tests, production code fixed) — exit code **0**:

```
--- PASS: TestParkedAnomaly_RateHoldLeavesTheLiveTPM (0.06s)
--- PASS: TestParkedAnomaly_IsPurgedByTheRetentionSweep (0.04s)
--- PASS: TestParkedAnomaly_LaterSettlementBooksTheActualTokens (0.04s)
--- PASS: TestParkedAnomaly_CorrectionIsExplicit (0.04s)
--- PASS: TestEvidenceDrivenPark_KeepsTheRateHold (0.05s)
ok  	github.com/yunhou/users/internal/inference/postgres	2.687s
```

Command #9 (the recovery level) — exit code **0**:

```
--- PASS: TestRecover_ParkedAnomaly_DisposesTheRateHoldAndDoesNotReDrive (0.07s)
--- PASS: TestRecover_ParkedAnomaly_KeepsTheMonetaryHoldsAndTheLedgerEmpty (0.05s)
ok  	github.com/yunhou/users/internal/inference/workers	0.796s
```

Command #10 (the gateway level) — exit code **0**:

```
--- PASS: TestGateway_ParkedAnomaly_DisposesTheRateReservation (0.07s)
ok  	github.com/yunhou/users/internal/inference/gateway	0.685s
```

Acceptance command #11 — exit code **0**:

```
ok  	github.com/yunhou/users/internal/inference/postgres	13.706s
ok  	github.com/yunhou/users/internal/inference/gateway	9.327s
ok  	github.com/yunhou/users/internal/inference/workers	7.010s
ok  	github.com/yunhou/users/internal/inference/quota	1.625s
ok  	github.com/yunhou/users/internal/inference/access	1.659s
```

#### 4.59.4 The fix — an explicit rate-reservation disposition

The disposition is a **new state on the rate reservation row**
(`inference_rate_reservations.state = 'anomaly'`), applied in the **same
transaction** as the park:

* it **leaves the live TPM budget** (the in-flight sum counts only
  `state='reserved'` rows) — one anomaly can no longer permanently block an
  account;
* it is **neither `released` (confirmed zero) nor `settled` (known usage)**:
  the consumption stays **unknown** and is never recorded as zero. The
  request keeps `usage_status='unknown'`, the reconciliation job keeps its
  reason, and the row keeps `anomaly_reason` + `anomaly_at`;
* the **RPM fact is preserved** (the row and its `counted_at` stay; the RPM
  count looks at the age, not the state) — one logical request still counts
  once;
* the **money is untouched** — the window / Key monetary holds stay held, the
  ledger gets no charge, nothing is settled and nothing is released;
* **only manual-only reasons dispose** — the shared
  `ManualOnlyReconciliationReasons` vocabulary is the single switch (the
  evidence-driven `unknown_usage` / `crash_recovery` park keeps its hold,
  because the recovery will settle that request at the conservative
  estimate);
* **retention converges** — an `anomaly` row is terminal, so the retention
  sweep deletes it like `settled` / `released` rows: the stuck rows no longer
  accumulate forever;
* the **later evidence path is explicit** — when the request really settles,
  `settleRateTx` promotes the row to `settled` with the **actual** normalized
  tokens (and `releaseRateTx` promotes it to `released` on a confirmed-zero
  release); the anomaly trace is kept. Until then a token-budget correction
  against the row fails **loudly** (an explicit conflict: that row holds no
  settled token total — correcting it would silently keep a total that was
  never booked).

Implementation: `Store.parkRateReservationTx` +
`Store.MarkReconciliationRequired` (the single funnel every park path goes
through — the gateway's `parkSettleFailure` and the recovery worker's
`parkForManual`), plus `settleRateTx` / `releaseRateTx` /
`correctRateTokensTx` in `internal/inference/postgres/inference_limit_repo.go`.

#### 4.59.5 Migration

**One appended migration: `migrations/054_inference_rate_reservation_anomaly_state.sql`**
(54 = the re-checked latest number; 053 was the branch baseline). Nothing was
rewritten and no historical ledger row was touched:

1. `inference_rate_reservations.state` — the CHECK is widened to
   `('reserved','settled','released','anomaly')` (drop + re-add, the
   `migrations/README.md` idempotent-DDL idiom, exactly as migration 051 did
   for the reconciliation reasons);
2. `inference_rate_reservations.anomaly_at` / `.anomaly_reason` — the
   disposition trace, with a one-directional CHECK
   (`state <> 'anomaly' OR anomaly_reason IS NOT NULL`): an anomaly row always
   carries its reason, while the trace survives the later promotion to
   `settled` / `released`;
3. no index churn — `idx_inference_rate_reservations_open`
   (`WHERE state = 'reserved'`) already excludes the new state and
   `idx_inference_rate_reservations_terminal` (`WHERE state <> 'reserved'`)
   already includes it.

#### 4.59.6 Operator / audit semantics

* A `missing_reservation` / `reservation_drift` / `insufficient_evidence`
  reconciliation job means: *the request's TPM hold was taken out of the live
  budget*; the money hold is **still held**; nothing was charged; the anomaly
  is still yours to resolve. **Freeing the budget is not a statement that the
  request consumed nothing.**
* To resolve: repair the monetary reservation and settle the request (the
  rate row then books the actual tokens), or correct it once it is settled.
* The evidence lives in `inference_rate_reservations` (state + `anomaly_reason`
  + `anomaly_at`), `inference_reconciliation_jobs` (reason + detail +
  `reason_history`), `inference_requests.last_error` and
  `usage_status='unknown'`. The rate row is a **budget artefact** and is
  purged with the other terminal rate rows by the retention sweep; the
  reconciliation job and the request row are **not** purged by it.
* The operational signals are unchanged: the unknown-usage backlog alarm
  (threshold 10) and the reconciliation deadline escalation still fire.

**Residual risk (stated, not hidden).**

1. A parked anomaly's real consumption is **not** counted against the
   account's TPM budget — the deliberate trade-off (keeping the hold would
   block the account forever). The consumption is *not* recorded as zero; it
   stays an open anomaly. The exposure is bounded by (open anomalies ×
   per-request hold) and by the reconciliation deadline (default 24 h).
2. The rate-side anomaly trace is purged with the retention window (default
   24 h); the audit evidence survives in the reconciliation job and the
   request row.
3. A **terminal** request is never disposed by the park path
   (`ReportTerminalAnomaly`): in every normal flow a terminal request's rate
   row is already `settled` / `released`, because the rate update and the
   terminal state are written in the **same** transaction. The residual risk
   is structural, not observed.
4. The correction channel is still unwired (Finding 3) — an `anomaly` row has
   no *automated* path back to `settled` until that follow-up lands.

#### 4.59.7 Files changed

| File | Change |
|---|---|
| `migrations/054_inference_rate_reservation_anomaly_state.sql` | **new** — the `anomaly` state + the disposition trace columns + the widened CHECK |
| `internal/inference/postgres/inference_limit_repo.go` | `rateStateAnomaly` + `parkRateReservationTx` (the disposition); `settleRateTx` / `releaseRateTx` promote an `anomaly` row; `correctRateTokensTx` refuses an `anomaly` row explicitly; the TPM/RPM aggregate documentation |
| `internal/inference/postgres/ledger_repo.go` | `MarkReconciliationRequired` disposes the in-flight rate reservation **in the same transaction** when the park reason is manual-only |
| `internal/inference/postgres/rate_anomaly_test.go` | **new** — the store-level regressions |
| `internal/inference/workers/settlement_recovery_rate_anomaly_test.go` | **new** — the recovery-level regressions |
| `internal/inference/gateway/rate_anomaly_test.go` | **new** — the gateway-level regression |
| `docs/runbooks/kaya-coding-plan-rollout.md` | **Finding 2** — the process-local / N×instance RPM statements swept (§2 switch table, §6 conclusion + a dated correction note, §9 legacy list) |
| `docs/superpowers/plans/2026-11-05-users-model-gateway-completion.md` | §1.1.1 (the 054 row + the widened state vocabulary) and §1.1.6 item 1 (the named follow-up) |
| `docs/runbooks/2026-11-05-model-gateway-verification.md` | this section + §6 "Named follow-ups (post-delivery)" |

#### 4.59.8 Finding 2 — the rollout runbook

`docs/runbooks/kaya-coding-plan-rollout.md` was the one operator-facing
document still stating the pre-T4 semantics ("进程内滑动窗口（多实例≈配置×
实例数）"). Every stale statement was updated to the delivered semantics:

| Location | After |
|---|---|
| §2 switch table | "**PostgreSQL 共享**" — rolling 60 s window on DB time, one account counter shared by instances / Keys / `/chat` and `/v1`; fail closed (503) when the database is unavailable |
| §6 conclusion item 3 | the rate path is **PostgreSQL-shared**, not process-local; the effective multi-instance allowance is the configured value itself; the account-anchor lock serialises the read-then-write so a read-modify-write cannot over-admit; only the upstream account cooldown remains process-local |
| §6 conclusion item 4 | the **cross-process rate-path acceptance gap** is stated explicitly (evidence-completeness, not correctness), with the actionable next step; **no cross-host test was run** |
| §6 header | a dated note: the measured numbers still come from the 2026-09-09 campaign (**not re-run**), only the conclusion wording was corrected |
| §9 legacy list | "多实例 RPM 精确化" split into the **cross-process acceptance** item; the Redis/独立网关层 row drops RPM centralization from its candidate scope; the "Key scope 租约 / TPM / session_sticky" row corrected to **已接** |
| §0 pre-release checklist | the branch's migrations 050–054 (incl. 052 and 054) must be applied before this branch's traffic (035–049 are baseline) |

#### 4.59.9 Finding 3 — the named follow-up

The accurate limitation wording is **kept** (plan §1.1.6 item 1, the T4
branch-era note, §5 open concerns): the wallet post-settlement correction is
still refused and the money-correction channel still has **no production
caller**. A **named follow-up** was added with an owner role and a schedule
(§6 "Named follow-ups (post-delivery)"):

| Follow-up | Scope | Owner (role) | Schedule |
|---|---|---|---|
| `inference-correction-trigger-and-wallet` | (a) the evidence-driven correction **trigger**; (b) the **wallet money-correction semantics** | Users 模型网关维护者 | 未排期 |

**No payment / wallet product change was implemented.**


---

### 4.60 Review convergence and regression verification (2026-11-09)

This review covers the current worktree on `fix/model-gateway-completion`,
based on `144e8722e5c372eab97d258a865c5f20e73c90bd` (original plan base
`299fc49`). The changes remain uncommitted; no push, merge, deployment,
production database access or real-provider inference was performed.

#### 4.60.1 Corrections reviewed

| Area | Correction and regression coverage |
|---|---|
| Wallet release and recovery | Validate the expected wallet freeze before any release/settlement writes. A missing freeze is typed `missing_reservation`; an existing non-held freeze is `reservation_drift`, not a benign race. Both direct release and reconciliation release preserve request, money and job facts on failure. Recovery parks the anomaly once, disposes its in-flight rate reservation through the existing anomaly path, and never fabricates zero usage or a charge. |
| Dispatch cancellation and fencing | A pre-cancelled dispatch sends nothing. Once a connection has been obtained, cancellation is conservatively `ExecutedUnknown`, never retryable. Lease fencing cancels upstream I/O blocked on headers, a nonstream body or silent SSE. The same keeper and fencing state survive stream handoff. |
| Blocked lease renewal cleanup | `Stop` and `Pause` cancel the renewal context before waiting for the loop, so an in-flight database renewal cannot indefinitely block cleanup. Shutdown cancellation does not report fencing or cancel dispatch. `Stop` releases the lease; `Pause` keeps it held. |
| Finalization cleanup | Nonstream settlement and confirmed-no-consumption release give request-lease cleanup a fresh detached deadline, even if finalization exhausted its own deadline. Money and TPM holds remain for recovery when finalization fails. Existing finalize-before-release and panic-cleanup tests remain in force. |
| Unknown-execution lease contract | Dispatch-level unknown execution stops renewal and retains request-level account/Key leases until TTL, while releasing the attempt-level lease. A handed-off interrupted stream releases its leases through `Finish`. These are intentionally different paths; neither replays the upstream request. |
| Wallet fallback errors | Only expected wallet spend gates retain the original plan-quota rejection. Storage, rate and concurrency errors retain their real cause rather than being disguised as plan exhaustion. Failed fallback leaves no request, rate reservation, wallet freeze, debit or dispatch. |
| Provider protocol handling | Group consecutive assistant text/function-call items into one turn, preserve parallel tool history, retain Messages content when usage is inline, and emit terminal usage only after content blocks close. Native SSE accepts the optional space after `data:`. Responses maps `content_filter` to `incomplete` for both stream and nonstream output. |
| Metering and privacy | Generated tool-argument bytes contribute to fallback output estimates across adapters and stream/nonstream paths. Stored raw usage contains the usage object, not the surrounding conversation/content chunk. Case-insensitive reserved headers cannot override gateway authentication or tracing. |
| Management and catalog | Route PATCH preserves absent/null fields, applies explicit zero/false/empty values subject to validation, requires the client's nonzero `updated_at`, rejects stale writes and immutable identity fields, and avoids audit writes on rejection. Role grants validate reason and UUID. Probe uses the same validated deployment headers as dispatch. Kaya model listing uses one pinned snapshot while preserving price/access fail-closed checks. |

The disabled-provider picker regression advances the injected clock by two
seconds after publish. This respects the existing one-second entitlement
cache contract; no production cache was disabled to make the test pass.

#### 4.60.2 Latest RED/GREEN evidence

Database-backed runs use fresh loopback-only PostgreSQL instances created under
`/private/tmp/yunhou-review-20261109.3NuMWy/`, explicit disposable
`DATABASE_URL` and `E2E_DATABASE_URL`, `GOPROXY=off`,
`GOFLAGS=-mod=readonly`, `-race`, `-count=1` and `-p 1`. The runner is
`run.zsh` in that temporary directory. It migrates separate app/e2e databases
twice and trap-stops only its own instance.

| Run | Result | Evidence |
|---|---|---|
| `run.8tWHGo` | RED, exit 1 | Before removing the unconditional unknown-path request-lease cleanup, `TestGateway_DispatchUnknownExecution_KeepsRequestLeases` and fencing `headers`/`body` failed because the Key lease was released prematurely. The interrupted handed-off stream and `silent_sse` path passed. |
| `run.BN476i` | GREEN, exit 0 | After removing that cleanup, 40 test/subtest passes across gateway, providers, postgres, workers and httpapi; zero failures/skips. Includes unknown/interrupted lease contracts, three fencing phases, Finish ordering/panic, cancellation execution boundary, wallet fallback, freeze anomalies/recovery, expired-Key recovery, lifecycle gates and disabled-provider picker. |
| `run.9OBLsq` | GREEN, exit 0 | `TestCleanup_UsesFreshDeadlineAfterFinalizationTimeout` and its two subtests pass; zero failures/skips. This separate run was necessary because the first focused regex did not select the actual `TestCleanup_` name. |
| `lease-keeper-red.jsonl` | RED, exit 1 | Before the renewal-context change, both `TestLeaseKeeper_CleanupInterruptsBlockedRenewal` subtests (`stop` and `pause`) failed with `lease cleanup stayed blocked on renewal`. This routing-only run uses a blocking fake store, not PostgreSQL. |
| `lease-keeper-green.jsonl` | GREEN, exit 0 | After the change, `go test -race -count=1 -run '^TestLeaseKeeper_' -json ./internal/inference/routing` has five test/subtest passes and one package pass, with zero failures/skips. Covers blocked Stop/Pause, normal renewal/release and fencing. |

The unknown-execution production correction is deliberately one line:
remove unconditional request-lease cleanup from the dispatch-level
`ExecutedUnknown` branch. The fencing test expects one held Key lease for
headers/body and zero for a handed-off silent stream, with one
reconciliation request, zero charges, one dispatch and cancelled upstream
I/O in every phase.

The blocked-renewal root cause was `RenewLease(context.Background(), ...)`
combined with Stop/Pause waiting on the loop's `done` channel. PostgreSQL's
renewal uses `ExecContext`, so the minimal fix gives the loop a separate
cancellable renewal context and checks its cancellation before treating an
error as fencing. The upstream dispatch cancellation callback remains
separate and is invoked only for a real fencing conflict. The regression
waits for renewal to actually start before requesting cleanup.

Independent text-only reviews found no actionable issue in the supplied
unknown-execution and renewal-cancellation corrections. Their evidence is
limited to exact snippets and reported test behavior: neither review
inspected the final worktree or ran commands. The latest review explicitly
excluded stream-handoff call sites, lease TTL policy and independent
verification of the full suite. Main-session source inspection checked the
same-keeper handoff and actual test output; reviewer verdicts do not replace
those checks.

#### 4.60.3 Full verification

The pre-renewal-fix full run `run.JRlsR7` completed with exit 0:
34 package passes, 4,252 test/subtest passes and three skips in the
race/coverage suite; 80.5% coverage; one package pass, 180 test/subtest
passes and one skip in E2E. Build and vet passed. Its cleanup log and
`pg_ctl status` confirm that its PostgreSQL instance stopped. This evidence
predates the renewal-context fix and is not the final post-fix verdict.

The post-fix full run `run.iOgFJR` completed with exit 0 and printed
`SUCCESS RUN=/private/tmp/yunhou-review-20261109.3NuMWy/run.iOgFJR`.
It used a new owned PostgreSQL instance on port 55489. Production and test
source remained frozen throughout this run.

| Gate | Current evidence |
|---|---|
| App/e2e migrations, twice each | Both first runs applied 54 migrations; both second runs applied zero and skipped 54. |
| `go build ./...` | Passed; empty `build.log`. |
| `go vet ./internal/... ./cmd/...` | Passed; empty `vet.log`. |
| Full race/coverage suite | Passed: 34 package passes, 4,255 test/subtest passes, three skips and zero failures. |
| Coverage floor | Passed: 80.5% statement coverage against the 80% floor. |
| E2E race suite | Passed: one package pass, 180 test/subtest passes, one skip and zero failures. |
| OpenAPI structural/contract check | Passed: 29 paths, 30 unique operation IDs, 226 resolved internal references, no duplicate YAML keys, required route version/reason, nullable mutable fields and no writable immutable identity fields. |
| Scope/format | All 42 changed/new Go files pass `gofmt -l`; `git diff --check` passes; `go.mod`, `go.sum` and migrations are unchanged by this review. |

Focused-run cleanup logs and `pg_ctl status` confirm that the RED and both
GREEN instances have stopped, as has the pre-fix full-run instance.
The post-fix full-run cleanup log reports `server stopped`; a subsequent
`pg_ctl status` reports `no server running` with exit 3. Its runner exited
with status 0 after completing the EXIT trap.

The main-suite skips are the child-only `TestQuotaWorkerProcess` entry
point and the opt-in `TestLoadDrill_Throughput` and
`TestLoadDrill_LongConnections`. E2E skips the opt-in
`TestUsageTokenAcceptance_TenPagesTwoMinutes`. These skips are not counted
as successful acceptance tests.

The final review of the corrections in this worktree found no further
actionable issue within the reviewed scope. This is a local review and
regression result, not a claim that the code has no defects or is ready
for production rollout.

#### 4.60.4 Boundaries retained

`inference-correction-trigger-and-wallet` remains a named, unimplemented
follow-up, not a completed correction channel. Cross-process rate-path
acceptance remains unperformed (two-Service/two-pool coverage is not renamed
cross-process coverage). Real provider and real client acceptance, product
sales, payments/refunds and production rollout remain outside this local
review. No existing limitation is waived by a green local test run.

---

### 4.61 Independent review #12: money and quota fixes (2026-11-09)

**Scope.** Only findings 1 and 2 from the independent review handoff are fixed.
Findings 3-5 are recorded in section 6, not implemented. Work stays in this
worktree. No production database, real provider, secret, adjacent repository,
Kaya restart/termination, push, merge, or deployment is part of this work.

#### 4.61.1 Retention follows admission and settlement

`PurgeRateReservations` now requires expired admission time and, for settled
rows, expired settlement time. Each cutoff is capped at the live 60-second
window boundary. Reserved rows remain unconditionally protected.

The regression admits a request 48 hours ago, settles **90 tokens now**,
purges with 24-hour retention, and attempts another **20 tokens at TPM 100**.
Before the fix the 90-token fact disappeared and admission succeeded; after
the fix admission is `rate_limited` and the live 90 tokens remain. A separate
short-retention regression protects live RPM. The old expired-terminal-row
test now expires both timestamps instead of treating a live settlement as
expired data.

#### 4.61.2 Equal money does not imply equal metering

`CorrectSettlement` now accepts changed usage evidence or a changed explicit
token total even when the monetary amount is unchanged. It appends the usage
revision, corrects TPM without moving the original settlement instant, and
updates request usage status in the same transaction. Equal money creates
neither an adjustment row nor a financial ledger entry, and leaves quota
window/key monetary aggregates untouched. Identical money and evidence remain
an invalid no-op; nullable token buckets and JSON evidence compare semantically.

New migration **055** adds `inference_usage_corrections`, an audit receipt for
every new correction (money-changing or metering-only), with a shared unique
idempotency key, usage reference, corrected amounts/tokens, reason and operator.
Financial adjustments retain their strictly positive amount constraint. Legacy
correction keys remain recognized in `inference_adjustments`. Apply migration
055 before using the new correction code; no migration was applied to any
production database in this task.

Regressions cover equal charges **60,000 and zero**, estimated-to-reported
token changes (50 to 90), source-only/bucket-only/raw-only evidence changes,
unknown versus reported zero, absence of zero-value financial entries,
unchanged money aggregates, replay, shared-key concurrent monetary/metering
corrections, and usage/receipt rollback on rate failure. A deterministic
same-request concurrency test also exposed a stale pre-lock idempotency check;
the check now runs after acquiring the request lock.

#### 4.61.3 Local verification evidence

- `run.H9MU4p`: RED for both original findings and short-retention RPM.
- `run.NjM72t`: purge regressions GREEN; expanded equal-money tests RED.
- `run.59s7n2`: complete PostgreSQL package under race, **205** test/subtest
  passes, zero failures, one child-process-entry skip.
- `run.DzVDAF`: deterministic concurrent replay RED (`invalid_input` instead
  of already-applied `conflict`).
- `run.rYbv08`: final focused race tests, **37** passes, zero failures/skips.
  Command: `go test -race -count=1 -p 1 -json ./internal/inference/postgres
  -run 'TestCorrectSettlement|TestCorrectRateTokens|TestPurgeRateReservations|TestParkedAnomaly'`.
- `run.C46zgl`: `go build ./...` and
  `go vet ./internal/... ./cmd/...` passed.
- Full race/coverage command:
  `go test -race -count=1 -p 1 -coverprofile="$RUN/coverage.out" -json
  ./internal/... ./cmd/... ./tests/integration/...`.
  **4,268** test/subtest passes across **34** packages, zero failures, three
  skips: `TestQuotaWorkerProcess` is a child-process entry point;
  `TestLoadDrill_Throughput` and `TestLoadDrill_LongConnections` require
  `KAYA_LOAD=1` and are outside the default gate. The parent
  `TestTwoProcessContendLastCredit` passed; it covers monetary quota, not the
  cross-process rate path.
- `go tool cover -func="$RUN/coverage.out"`: total statement coverage
  **80.5%**, satisfying the **80%** floor. `PurgeRateReservations` is **80.0%**
  and `CorrectSettlement` is **79.0%**. The separate E2E command is not
  included in this coverage profile.
- Race E2E command:
  `go test -race -count=1 -p 1 -json ./tests/e2e/...`.
  **180** test/subtest passes, zero failures, one skip:
  `TestUsageTokenAcceptance_TenPagesTwoMinutes` requires
  `USAGE_ACCEPTANCE=1` and is outside the default gate.
- The full runner exited **0**. Both suites together report passing package
  results for **all 35 Go packages**, checked against `go list ./...`.
  No missing packages or race failures were reported.
- Each disposable database applied all **55** migrations, including 055;
  the second migration invocation reported `applied=0 skipped=55`.
  `cleanup.log` confirms `server stopped` for this run's owned cluster.
- Final independent review found no confirmed defects in scope.
  `gofmt -l` and `git diff --check` were clean. Nine deliverable files remain
  uncommitted in the requested worktree; no push, merge, or deployment occurred.

All run directories above are under
`/private/tmp/yunhou-review-20261109.3NuMWy/`. Each invocation owns a fresh
PostgreSQL cluster and two disposable databases with explicit `DATABASE_URL`
and `E2E_DATABASE_URL`; its exit trap stops only its own data directory.
An initial test-only compilation error (`run.JNpJCu`, Microcredit/int64
comparison) was fixed before the behavior RED run; it is not defect evidence.

#### 4.61.4 Boundaries retained

The correction channel still has no production caller and still rejects
wallet-charged requests. This work completes the repository contract for
plan corrections, not the production evidence trigger or wallet compensation.
Provider E2E remains simulated; the cross-process rate-path gap in section 5
remains open. Findings 3-5 remain known behavioral risks, not waived defects.

#### 4.61.5 Independent review and delivery decisions

The independent read-only review found no confirmed Critical, Important, or
Minor defects in the scoped diff, including migration 055 and the new tests.
The reviewer ran formatting/diff checks, not database tests; runtime evidence
belongs to the main verification run above.

- The requested current worktree is retained, without a new commit or any
  publishing operation. Integration and production acceptance remain separate.
- Correction receipts are separate from strictly positive financial entries.
  Migration 055 is required before this code is invoked. Historical keys are
  recognized without rewriting accounting history, but concurrent mixed-version
  safety with legacy-only adjustment writers is not established.
- Findings 3-5 remain deferred by explicit scope: history ordering, conflicting
  reasoning parameters, and missing `/chat` retry metadata remain behavioral
  risks until their follow-ups are implemented.
- Production correction triggering and wallet compensation remain unsupported
  here; completing the repository contract does not make those paths available.
- No claim is made about live providers, cross-process rate admission,
  production rollout, or migration performance. Local simulated E2E and
  disposable migrations do not prove those runtime properties.

Additional test opportunities from review, not demonstrated defects: replay of
a legacy financial key without a receipt, equivalent reordered JSON evidence
as a no-op, and a direct short-cutoff settled-TPM regression. Callers must still
roll back their transaction on every correction error.

### 4.62 Review #12 findings 3-5: protocol intent and retry metadata

The user's November 9, 2026 follow-up supersedes the delivery decisions in
section 4.61.5: findings 1-2 were committed first as
`5f74ee8ee3bad49572c9a9fbecb5b591fa83275d`
(`fix(inference): preserve live TPM and metering corrections`), including
migration 055, regressions and documentation. Findings 3-5 are now implemented
and packaged separately as `fix(inference): preserve protocol intent and retry
metadata`. No push, merge, deployment, neighboring-checkout change or Kaya
process operation is part of either delivery.

#### 4.62.1 Behavior and compatibility

- **Responses history:** text before function calls, consecutive parallel
  calls and text after tool results remain representable in the internal
  Chat turn. Nonempty assistant text after a function call, including text
  between parallel calls before any results, now returns `invalid_input`
  rather than silently moving text before the call. Request parsing and
  stored-transcript replay use the same conversion.
- **Reasoning precedence:** an explicit `reasoning_effort` wins over the
  boolean `thinking_enabled`, including false, in both OpenAI adapters.
  The ingress regression proves false/high remains high for Chat and
  Responses; the adapter matrix covers every supported effort with true,
  false and unset toggles. No-effort defaults and capability routing are
  unchanged.
- **Local `/chat` retry metadata:** wrapped typed rate errors retain
  `RetryAfterSeconds` through the facade's `ChatLimitError`. The handler
  emits integer `Retry-After` only for `rate_limited` with positive known
  seconds. Unknown/nonpositive recovery omits the header. The existing
  429 envelope, both compatibility sentinels and cold-catalog
  `Retry-After: 5` are unchanged.

These are deliberate compatibility decisions: formerly reordered histories
must now use a representable sequence, and contradictory reasoning flags
request the explicit effort rather than returning a new ingress 400.

#### 4.62.2 Regression evidence

All artifacts below are under
`/private/tmp/yunhou-review-20261109.3NuMWy/`:

- `protocol-baseline-red.jsonl`: running the new provider regressions with
  Go's read-only source overlay of the two production files at `5f74ee8`
  fails as expected. The baseline accepts before/call/after history and
  turns false plus explicit effort into none. The worktree was not reverted.
- `run.N1821v`: facade and handler regressions RED; known 17-second recovery
  is lost and `Retry-After` is absent.
- `run.D4VkoH`: real local RPM admission through `/chat` is RED; the 429
  omits the computable recovery header.
- `run.6cnAkZ`: focused race regressions GREEN, **50** test/subtest passes
  across **five** packages, zero failures/skips. Coverage includes parse
  and replay ordering rejection, cross-adapter effort precedence, ingress
  false/high, wrapped errors, known/unknown/negative recovery, and real
  RPM versus permanently-over-cap TPM rejection without upstream dispatch.
- Full provider packages also passed
  `go test -race -count=1 ./internal/inference/providers/...`.
- `run.ObuVVi`: `go build ./...` and
  `go vet ./internal/... ./cmd/...` passed.
- Full race/coverage command:
  `go test -race -count=1 -p 1 -coverprofile="$RUN/coverage.out" -json
  ./internal/... ./cmd/... ./tests/integration/...`.
  **4,291** test/subtest passes across **34** packages, zero failures and
  three skips: `TestQuotaWorkerProcess` is the subprocess entry point
  (parent `TestTwoProcessContendLastCredit` passed);
  `TestLoadDrill_Throughput` and `TestLoadDrill_LongConnections` require
  `KAYA_LOAD=1` and are outside the default gate.
- `go tool cover -func="$RUN/coverage.out"`: total statement coverage
  **80.5%**, satisfying the **80%** floor. The separate E2E command is
  not included in this coverage profile.
- Race E2E command:
  `go test -race -count=1 -p 1 -json ./tests/e2e/...`.
  **181** test/subtest passes, zero failures, one skip:
  `TestUsageTokenAcceptance_TenPagesTwoMinutes` requires
  `USAGE_ACCEPTANCE=1` and is outside the default gate. The new
  `TestChatFacade_RateRetryAfter` passed in this full run.
- The final runner exited **0**. Both suites report passing results for all
  **35 Go packages**, checked against `go list ./...`, with no missing
  packages or race failures.
- Both disposable databases applied all **55** migrations, including 055;
  each repeat invocation reported `applied=0 skipped=55`. `cleanup.log`
  confirms `server stopped` for this run's owned cluster.
- Final `gofmt -l` and `git diff --check` were clean.

The database runner creates an owned PostgreSQL cluster and two
temporary databases, exports both test DSNs explicitly and stops only that
cluster on exit. No default or production database is used.

#### 4.62.3 Independent review and boundaries

The fresh-context read-only reviewer found no confirmed bugs or blockers in
the scoped code and tests. Selected database-free race tests and
`git diff --check` passed during review; the main runner owns all database
test evidence.

Rulings on intentionally unexpanded review scope:

- Upstream output folding/SSE ordering and system/developer hoisting are
  unchanged, not validated by this incoming parse/replay fix; their fidelity
  is not newly certified.
- Complete tool-history validation (duplicate IDs and missing/partial results)
  is outside this ordering fix; malformed histories may still fail upstream.
- Boolean-only Chat behavior and provider-specific effort vocabulary remain
  unchanged; this fixes explicit-effort conflicts, not every protocol dialect.
- Upstream 429 recovery headers remain outside the local domain-rate path;
  no promise of forwarding supplier retry metadata is made.
- Prior money/quota internals were excluded from this second review; their
  evidence is section 4.61, not a new audit of those internals.

Deferred nonblocking test opportunities, not confirmed defects: empty-text
acceptance, whitespace-only rejection and a stored-call/new-input boundary
case; positive retry seconds attached to non-rate codes should omit the header.
The correction trigger still has no production caller, wallet money correction
is unsupported, provider E2E is simulated and cross-process rate-path
acceptance remains open (section 5).

#### 4.62.4 Deliverable file manifest

First commit (findings 1-2), nine files:

- `internal/inference/postgres/inference_limit_repo.go`
- `internal/inference/postgres/reconciliation_repo.go`
- `internal/inference/postgres/inference_limit_test.go`
- `internal/inference/postgres/correction_rate_test.go`
- `internal/inference/postgres/metering_correction_test.go`
- `internal/inference/postgres/recovery_test.go`
- `migrations/055_inference_usage_corrections.sql`
- `docs/superpowers/plans/2026-11-09-model-gateway-money-quota-review-fixes.md`
- `docs/runbooks/2026-11-05-model-gateway-verification.md`

Second commit (findings 3-5), fourteen files:

- `internal/inference/providers/responses_client.go`
- `internal/inference/providers/responses_client_test.go`
- `internal/inference/providers/openai_responses.go`
- `internal/inference/providers/openai_responses_test.go`
- `internal/inference/providers/adapter.go`
- `internal/inference/httpapi/reasoning_precedence_test.go`
- `internal/service/errors.go`
- `internal/service/chat_facade.go`
- `internal/service/chat_facade_test.go`
- `internal/handler/chat.go`
- `internal/handler/chat_test.go`
- `tests/e2e/chat_retry_after_test.go`
- `docs/superpowers/plans/2026-11-09-model-gateway-protocol-review-fixes.md`
- `docs/runbooks/2026-11-05-model-gateway-verification.md`

---

### 4.63 Provider stream fidelity review checkpoint (2026-11-09)

This checkpoint follows the user's local-commit request. It is not a claim
that the open-ended branch review has converged. The earlier findings 1-2
and 3-5 are already committed as `5f74ee8` and `1bec4c4`, respectively.

#### 4.63.1 Behavior and regression evidence

- Incoming Messages assistant blocks with nonempty text after `tool_use`
  now return `invalid_input`, rather than moving that text ahead of the
  call. Text before parallel calls, empty text blocks and results followed
  by a new user turn remain supported.
- Responses streaming terminal output and the replay transcript now follow
  the assigned `output_index`, not item closing order. The transcript
  omits reasoning without moving earlier calls behind later messages.
- OpenAI SSE usage tracking and client translation accept lines up to
  1 MiB, matching the existing native-adapter bound. A 70 KiB text or
  tool delta with inline usage retains both content and usage. An over-limit
  line fails explicitly and cannot resume from a suffix or later DONE.
  The tee latches the failure and does not expose the failing read;
  translated Messages/Responses emit an error, not a successful terminal.

All three defects were reproduced before the production fixes. The new
regressions passed after the fixes, and the complete providers package
passed `go test -race -count=1 ./internal/inference/providers` (2.480s).
The tests cover fragmented/whole reads, large text/tool deltas, prior usage
retention after overflow, and completed/incomplete output ordering.

Compatibility costs: previously reordered Messages histories now fail
explicitly; callers must place later text after tool results. Providers
emitting SSE lines larger than 1 MiB must send smaller deltas. Malformed
JSON event handling is unchanged by this checkpoint.

#### 4.63.2 Review status and remaining findings

Read-only reviewers examined money/quota/recovery, gateway/facade
boundaries at `1bec4c4`, and the providers working diff. They reported
five additional issues, which are not repaired or dynamically reproduced
by this commit:

- **P1, entitlement-sync stale plan race:** a pre-refund grant plan can
  insert an active entitlement after another worker has read cancellation
  and delivered both pending messages as noops. `planSync` runs outside
  the apply transaction; noop delivery and insert do not guard source
  freshness. Next verification needs a disposable-PostgreSQL barrier
  test fixing this interleaving and checking final authorization.
- **P2, local capability errors on `/chat`:** model-level tools/reasoning
  rejection returns untyped `invalid_input`; the facade maps it to a 502
  upstream rejection instead of the existing typed capability 400 path.
  Next verification must distinguish local zero-dispatch rejection from
  real upstream 4xx classification.
- **P1, error followed by DONE:** client renderers and the Chat tracker
  can treat a later DONE as success after an in-band error. Native
  Anthropic/Responses translators also lack a sticky failed state.
  Next verification must cover same-read and split-read error/terminal
  pairs, omit successful terminal frames and keep Responses unpersistable.
- **P1, native non-stream output order:** Anthropic and Responses decoders
  collect text and calls separately into Chat, potentially moving text
  around tool calls when rebuilding the client response. Request-history
  rejection and streaming output-index fixes do not repair this response
  path. Next verification must reproduce the native output arrangements
  and preserve them or explicitly reject unrepresentable output.
- **P2, interrupted Anthropic initial usage:** `message_start` input
  usage is retained but its output usage is not, so an error/EOF before
  `message_delta` can discard known output tokens. Next verification
  must preserve the initial 7/1 usage when no later usage arrives.

These findings prevent a claim that the branch review is complete.
The providers reviewer passed the package race suite and found no
confirmed defect in the three checkpoint fixes, but did not complete
all native item-boundary/refusal/multiturn combinations. All five new
findings are static-review reports, not new failing-test evidence.
The correction-trigger/wallet, real-provider/client and cross-process
rate-path limitations in sections 5-6 remain open.

#### 4.63.3 Deliverable file manifest

- `internal/inference/providers/messages_client.go`
- `internal/inference/providers/messages_client_order_test.go`
- `internal/inference/providers/responses_client.go`
- `internal/inference/providers/responses_stream_order_test.go`
- `internal/inference/providers/client_surface.go`
- `internal/inference/providers/stream_usage.go`
- `internal/inference/providers/stream_usage_test.go`
- `internal/inference/providers/openai_responses_stream.go`
- `internal/inference/providers/sse_limits_test.go`
- `docs/superpowers/plans/2026-11-09-model-gateway-review-convergence.md`
- `docs/runbooks/2026-11-05-model-gateway-verification.md`

#### 4.63.4 Full verification

Runner completed with exit 0 and `SUCCESS`:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.dY8W6i`.
This is fresh evidence for the checkpoint source, not reused HEAD results.

| Gate | Result |
|---|---|
| `go build ./...` | exit 0 |
| `go vet ./internal/... ./cmd/...` | exit 0 |
| `go test -race -count=1 -p 1 -coverprofile=... -json ./internal/... ./cmd/... ./tests/integration/...` | 4,306 test/subtest passes, 34 package passes, zero failures |
| `go tool cover -func=...` and 80% floor | 80.6%, passed |
| `go test -race -count=1 -p 1 -json ./tests/e2e/...` | 181 test/subtest passes, zero failures |
| Migrations on each disposable database | first pass applied 55; second pass skipped 55 |
| `gofmt -l` on all changed Go files; `git diff --cached --check` | no output, passed |

Expected skips, not counted as passes: `TestQuotaWorkerProcess` is the
child-process entry point; `TestLoadDrill_Throughput` and
`TestLoadDrill_LongConnections` require `KAYA_LOAD=1`;
`TestUsageTokenAcceptance_TenPagesTwoMinutes` requires `USAGE_ACCEPTANCE=1`.
No database-dependent test was skipped for a missing DSN.

The owned cluster bound only to loopback port 55489 and used separate
`review_app`/`review_e2e` databases with explicit `DATABASE_URL` and
`E2E_DATABASE_URL`. `GOPROXY=off` and `GOFLAGS=-mod=readonly` were set.
The runner's cleanup log confirms its own PostgreSQL server stopped.
No kaya process, production service, database or provider was accessed.

---

### 4.64 Refund versus stale entitlement-sync checkpoint (2026-11-09)

This local checkpoint addresses the first remaining finding from section
4.63.2. It is not a claim that the until-clean review has finished.

#### 4.64.1 Behavior and regression evidence

Previously the worker planned from nontransactional reads, then began its
applying transaction. A paid-message or direct sync paused between those
steps could resume after refund/cancellation and after another worker
noop-delivered both messages, leaving a usable open-ended entitlement
without any pending message to revoke it.

`TestEntitlementSync_RefundBeforeApplyCannotLeaveStaleGrant` reproduced
this on both paths before the fix. RED logs:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.wI6xQY`.
Both subtests failed with a refunded subscription regaining an active
entitlement, not with setup or compilation failures.

Source reads, convergence, account creation and optional delivery now use
one transaction. The selected subscription and paid order are held with
`FOR SHARE` until commit, so refund/cancellation cannot invalidate a
grant's source facts while it is applied. Account creation uses the same
connection. Existing entitlement revision/unique guards remain; direct
optimistic conflicts return `conflict` instead of a successful count.
No payment/refund product behavior or schema is changed.

Focused GREEN logs:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.4poINd`.
`go test -race -count=1 -p 1 -json ./internal/inference/workers ./internal/inference/postgres`
completed with exit 0: 314 test/subtest passes, two package passes, zero
failures, including the outbox and direct refund regressions. Only the
`TestQuotaWorkerProcess` child entry point was skipped. Its owned
PostgreSQL cluster was stopped by the runner.

#### 4.64.2 Deliverable file manifest

- `internal/inference/postgres/billing_account_repo.go`
- `internal/inference/postgres/entitlement_repo.go`
- `internal/inference/postgres/outbox_repo.go`
- `internal/inference/workers/entitlement_sync.go`
- `internal/inference/workers/entitlement_sync_edges_test.go`
- `internal/inference/workers/entitlement_sync_test.go`
- `internal/inference/workers/entitlement_sync_refund_race_test.go`
- `docs/superpowers/plans/2026-11-09-model-gateway-review-convergence.md`
- `docs/runbooks/2026-11-05-model-gateway-verification.md`

#### 4.64.3 Remaining risks

The other four section 4.63.2 findings remain unresolved: model capability
refusal can become `/chat` 502; a failed stream can later signal success;
native nonstream text/tool output can be reordered; interrupted Anthropic
initial output usage can be lost. Their regressions and fixes are Tasks
6-9 in the review-convergence plan, not part of this checkpoint.

Shared source locks can delay concurrent source updates; database errors
continue to roll back and reschedule the outbox message. The regression
proves the reproduced stale-plan interleaving, not every lock-contention
schedule. No real-provider/client acceptance was run. Existing correction
trigger/wallet, cross-process rate-path and toolchain limitations remain
as recorded in sections 5-6.

#### 4.64.4 Full verification

Runner completed with exit 0 and `SUCCESS`:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.wgodFl`.
This run verifies the checkpoint source, not an earlier commit.

| Gate | Result |
|---|---|
| `go build ./...` | exit 0 |
| `go vet ./internal/... ./cmd/...` | exit 0 |
| `go test -race -count=1 -p 1 -coverprofile=... -json ./internal/... ./cmd/... ./tests/integration/...` | 4,309 test/subtest passes, 34 package passes, zero failures |
| `go tool cover -func=...` and 80% floor | 80.5%, passed |
| `go test -race -count=1 -p 1 -json ./tests/e2e/...` | 181 test/subtest passes, zero failures |
| Migrations on each disposable database | first pass applied 55; second pass skipped 55 |

Expected skips, not counted as passes: `TestQuotaWorkerProcess` is the
child-process entry point; `TestLoadDrill_Throughput` and
`TestLoadDrill_LongConnections` require `KAYA_LOAD=1`;
`TestUsageTokenAcceptance_TenPagesTwoMinutes` requires `USAGE_ACCEPTANCE=1`.
No database-dependent test was skipped for a missing DSN.

The owned cluster used loopback port 55489 and separate `review_app` and
`review_e2e` databases with explicit `DATABASE_URL` and `E2E_DATABASE_URL`.
`GOPROXY=off` and `GOFLAGS=-mod=readonly` were set. Cleanup confirmed that
the runner stopped its own PostgreSQL server. No kaya process, production
service, database or provider was accessed. No push, merge or deployment
was performed.

---

### 4.65 Model capability error classification checkpoint (2026-11-09)

This local checkpoint addresses Task 6 from the review-convergence plan.
It does not claim that the until-clean review has finished.

#### 4.65.1 Behavior and regression evidence

Model-level protocol, image, tools and reasoning checks previously
returned ordinary `invalid_input`. The legacy `/chat` facade treated
that error as an upstream rejection and returned 502, even though the
request had never left the gateway.

RED logs:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.CU2MzY`.
`TestGateway_ModelCapabilityRefusalBeforeAdmission` failed on tools,
thinking, effort, images and protocol because the errors lacked the
capability type. `TestChatFacade_ModelCapabilityRefusalBeforeAdmission`
failed on tools, thinking and protocol with HTTP 502 instead of 400.
These were behavioral assertion failures, not compilation or setup errors.

The four model-level checks now reuse `ErrCapabilityUnsupported`, just
like the existing deployment-level capability checks. `/chat` returns its
existing capability 400 envelope; `/v1` retains native `invalid_input`.
All checks remain before admission. There is no schema, new error code
or accounting change, and generic invalid-input mapping is not widened.

Focused GREEN logs:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.8l1Q7Q`.
The focused race run completed with exit 0: 11 test/subtest passes across
gateway and E2E, zero failures and zero skips. Refused requests leave
zero request, attempt, monetary/rate reservation, lease or ledger rows
and make no upstream call. The real-upstream control,
`TestChatFacade_UpstreamInvalidInputRemainsStructuredRejection`, still
returns HTTP 502 with `upstream_status:400` and
`upstream_code:context_length_exceeded`.

#### 4.65.2 Deliverable file manifest

- `internal/inference/gateway/service.go`
- `internal/inference/gateway/model_capability_test.go`
- `tests/e2e/chat_gateway_test.go`
- `tests/e2e/chat_capability_test.go`
- `docs/superpowers/plans/2026-11-09-model-gateway-review-convergence.md`
- `docs/runbooks/2026-11-05-model-gateway-verification.md`

#### 4.65.3 Remaining risks

Tasks 7-9 remain unresolved: a failed stream can later signal success;
native nonstream text/tool output can be reordered; interrupted
Anthropic initial output usage can be lost. They are not part of this
commit checkpoint. The remaining provider/client acceptance, correction
trigger/wallet, cross-process rate-path and toolchain limitations stay
as recorded in sections 5-6.

#### 4.65.4 Full verification

Full checkpoint verification completed with exit 0 in
`/private/tmp/yunhou-review-20261109.3NuMWy/run.aqkdPZ`.

- `go build ./...`: passed.
- `go vet ./internal/... ./cmd/...`: passed.
- `go test -race -count=1 -p 1 -coverprofile=... -json ./internal/... ./cmd/... ./tests/integration/...`:
  4,315 test/subtest passes across 34 packages, zero failures.
- `go tool cover -func=...`: total statement coverage 80.5%, above the
  required 80% floor.
- `go test -race -count=1 -p 1 -json ./tests/e2e/...`:
  186 test/subtest passes, zero failures.

The only skips were the `TestQuotaWorkerProcess` child-process entry
point, the two `KAYA_LOAD=1` load drills
(`TestLoadDrill_Throughput`, `TestLoadDrill_LongConnections`), and the
`USAGE_ACCEPTANCE=1` drill
(`TestUsageTokenAcceptance_TenPagesTwoMinutes`). No database-unavailable
skip was accepted as verification.

The runner created its own PostgreSQL cluster on loopback port 55489
and separate `review_app` and `review_e2e` databases, with explicit
`DATABASE_URL` and `E2E_DATABASE_URL`. Each database applied 55 migrations;
the idempotent rerun applied none and skipped all 55. Runs used
`GOPROXY=off` and `GOFLAGS=-mod=readonly`. The runner's `cleanup.log`
confirms `server stopped`. No production database, provider, configuration
or kaya process was touched; no push, merge or deployment was performed.

---

### 4.66 Sticky failed stream checkpoint (2026-11-09)

This local checkpoint addresses Task 7 from the review-convergence plan.
It does not claim that the until-clean review has finished.

#### 4.66.1 Behavior and regression evidence

An in-band error followed by DONE could previously restore a successful
terminal in the Chat usage tracker. Messages/Responses client renderers
then emitted success, and native Anthropic/Responses upstream adapters
had the same failed-state gap.

Provider RED logs:
`.superpowers/sdd/2026-11-09-model-gateway-review-convergence/task-7-red.jsonl`.
`TestUsageTracker_ErrorCannotBecomeTerminal`,
`TestClientStreams_ErrorCannotBecomeTerminal` and
`TestNativeStreams_ErrorCannotBecomeTerminal` failed on behavior, not
compilation or setup. Tests cover reads bounded to 1 byte, 23 bytes and
the full fixture, with error followed by EOF or a later terminal.

Database-backed RED logs:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.I0n7d5`.
`TestV1Streams_ErrorThenDoneRemainsInterrupted` reproduced all three
surfaces settling as reported with completed attempts. Messages emitted
message_stop; Responses emitted response.completed and persisted one
response chain for the failed stream.

The Chat tracker now latches non-null error payloads, clears Terminal
and ignores later DONE for success classification. Native upstream
adapters continue metering later cumulative usage/content but suppress
subsequent content and successful terminal events after the first error.
Messages/Responses client renderers likewise suppress later output and
synthetic completion. Known usage
is preserved without making failed Responses eligible for persistence.

Focused GREEN: all providers passed
`go test -race -count=1 ./internal/inference/providers` (2.660s).
`/private/tmp/yunhou-review-20261109.3NuMWy/run.t5Kh2u` passed all four
HTTP test/subtest cases, with no skips or failures. All three surfaces
record estimated 12/9 input/output usage, failed stream_interrupted
attempts and zero response chains. The runner stopped its owned
PostgreSQL cluster.

#### 4.66.2 Deliverable file manifest

- `internal/inference/providers/stream_usage.go`
- `internal/inference/providers/messages_client.go`
- `internal/inference/providers/responses_client.go`
- `internal/inference/providers/anthropic_stream.go`
- `internal/inference/providers/openai_responses_stream.go`
- `internal/inference/providers/stream_failure_test.go`
- `internal/inference/httpapi/stream_failure_test.go`
- `docs/superpowers/plans/2026-11-09-model-gateway-review-convergence.md`
- `docs/runbooks/2026-11-05-model-gateway-verification.md`

#### 4.66.3 Compatibility boundary and remaining risks

Raw OpenAI Chat remains byte passthrough. If an upstream emits error
then DONE, its raw DONE is not stripped; the authoritative usage tap
and settlement still classify the attempt as interrupted. Synthetic
Messages/Responses/native-adapter success is suppressed. Changing the
raw Chat filtering contract is outside this checkpoint; clients must
honor its in-band error rather than treating a later DONE as success.

Tasks 8-9 remain unresolved: native nonstream text/tool output can be
reordered, and interrupted Anthropic message_start output usage can be
lost without a later message_delta. No final whole-branch clean review
has been claimed. The provider/client acceptance, correction trigger
and wallet, cross-process rate-path and toolchain limitations remain
as recorded in sections 5-6.

#### 4.66.4 Full verification

Full race verification ran in
`/private/tmp/yunhou-review-20261109.3NuMWy/run.kBQlZC`.

- `go build ./...`: passed.
- `go vet ./internal/... ./cmd/...`: passed.
- `go test -race -count=1 -p 1 -coverprofile=... -json ./internal/... ./cmd/... ./tests/integration/...`:
  4,358 test/subtest passes across 34 packages, zero failures.
- `go tool cover -func=...`: total statement coverage 80.6%, above the
  required 80% floor.
- `go test -race -count=1 -p 1 -json ./tests/e2e/...`: 186 test/subtest
  passes, zero failures.

The only skips were the child-process entry point
(`TestQuotaWorkerProcess`), the opt-in `KAYA_LOAD=1` drills
(`TestLoadDrill_Throughput`, `TestLoadDrill_LongConnections`) and the
`USAGE_ACCEPTANCE=1` drill
(`TestUsageTokenAcceptance_TenPagesTwoMinutes`). No database-unavailable
skip was accepted as verification.

The runner used its own PostgreSQL cluster on loopback port 55489 and
separate `review_app` and `review_e2e` databases, with explicit
`DATABASE_URL` and `E2E_DATABASE_URL`. Each database applied 55 migrations;
the idempotent rerun applied none and skipped all 55. Runs used
`GOPROXY=off` and `GOFLAGS=-mod=readonly`. The runner exited 0 and its
`cleanup.log` confirms `server stopped`. No production database, provider,
configuration or kaya process was touched; no push, merge or deployment
was performed.

---

### 4.67 Native nonstream output-order checkpoint (2026-11-09)

This local commit checkpoint addresses Task 8 only. Task 9 and the
until-clean whole-branch review remain unfinished.

#### 4.67.1 Behavior and regression evidence

Anthropic and Responses nonstream output previously folded nonempty text
after a tool call into text before that call. Provider regressions cover
text/tool/text and tool/text/tool with reported and absent usage; the
HTTP matrix covers both native upstream protocols and all three client
surfaces. Their RED versions were committed in `8dc497c` and `3a81718`.
The disposable-database RED logs are in
`/private/tmp/yunhou-review-20261109.3NuMWy/run.SdIwzR` and
`/private/tmp/yunhou-review-20261109.3NuMWy/run.etndK6`.

The decoders now scan the complete response for metering, reject
unrepresentable order, and return metadata without a client payload.
Dispatch preserves that executed result alongside the translation error.
The gateway records a failed `response_translation` attempt and settles
once, using reported usage or a complete-content estimate. It returns a
finalized identity plus error, so dispatch does not fail over or release
the request as unexecuted. Malformed response bodies still take the
unknown-execution reconciliation path.

The dedicated `upstream_response_invalid` domain code maps to HTTP 502
on the OpenAI surfaces; Messages returns 502 `api_error`, not the
overload/retry-oriented 529 shape. No reordered answer or Responses chain
is served or persisted. Text before calls, call order and empty text
between calls remain supported.

Focused GREEN evidence:
`/private/tmp/yunhou-review-20261109.3NuMWy/run.gVp6cA` exited 0 with
24 test/subtest passes across gateway and HTTP, no failures or skips.
Assertions include one upstream call, zero fallback calls, one charge,
settled TPM, no held money/active leases/reconciliation jobs and no
response chain. All providers passed
`go test -race -count=1 ./internal/inference/providers` (5.076s).
The existing mixed tool-argument metering test still asserts all 23
generated bytes, including on rejected native output.

#### 4.67.2 Deliverable file manifest

| Files | Change |
|---|---|
| `internal/inference/providers/{adapter.go,anthropic.go,openai_responses.go}` | Reject reordered native output while retaining executed usage, output byte counts and upstream identity |
| `internal/inference/gateway/service.go` | Finalize the rejected execution exactly once and stop failover |
| `internal/inference/domain/errors.go`, `internal/inference/httpapi/{chat_completions.go,messages.go}` | Distinguish response translation failure from unavailable/overloaded upstreams and return native 502 errors |
| `internal/inference/{providers,gateway,httpapi}/nonstream_order_test.go`, `internal/inference/providers/tool_argument_metering_test.go` | Native order, supported controls, dispatch, no-fallback, HTTP/accounting and complete metering regressions; order tests originated in the two preceding test commits |
| `docs/api-integration-guide.md`, `docs/api/kaya-coding-plan.openapi.yaml` | Document rejection, incurred-usage settlement, no replay and no response-chain persistence |
| `docs/superpowers/plans/2026-11-09-model-gateway-review-convergence.md`, this runbook | Checkpoint scope, evidence and remaining work |

#### 4.67.3 Compatibility boundary and remaining risks

- Previously reordered native answers now fail explicitly with 502.
  Consumption already incurred still settles; a client retry is a new,
  potentially billable request, not a transparent gateway replay.
- This change preserves the internal Chat representation; it does not
  introduce a lossless native-protocol passthrough or additional block types.
- Interrupted Anthropic `message_start` output usage (Task 9), the existing
  nonstream upstream-lease release ordering, cross-process rate acceptance
  and the original wallet-correction limitation still require their
  separate final audit. This checkpoint does not claim a clean branch.
- Verification uses local simulated upstreams and owned disposable
  PostgreSQL databases, not real supplier/client acceptance.
- The independent read-only Task 8 review found no confirmed blocker.
  Its nonblocking coverage note remains explicit: the HTTP regression
  matrix checks status 502 but not the native error body's type/code;
  those mappings were verified statically.

#### 4.67.4 Full race verification: non-green

The full runner
`/private/tmp/yunhou-review-20261109.3NuMWy/run.yj5MJY` exited 1.
Build and vet passed. The full race command was:

```sh
go test -race -count=1 -p 1 -coverprofile=coverage.out -json \
  ./internal/... ./cmd/... ./tests/integration/...
```

The run had 4,404 test/subtest passes and two failed tests. Of 34
packages, 33 passed and `internal/relay` failed:

- `TestWSSlowConsumer`: writing the large test frames exceeded its
  context deadline (`conn_test.go:589`).
- `TestWSConnectionCaps`: the third connection hit the 300 ms hello
  timeout before the expected connection-cap rejection.

No relay source or dependency on the changed inference packages was
modified. A separate unchanged relay race run, logged in
`run.yj5MJY/relay-recheck.jsonl`, also exited 1: 57 tests/subtests passed,
including the connection-cap test, but the slow-consumer write still
timed out. The cause remains unresolved; host load was high, but that
observation does not establish the cause or waive either failure.

`go tool cover -func` measured total statement coverage at 80.6%, above
the 80% numerical floor. This profile comes from a non-green run and is
not a passing verification gate. The three full-suite skips were the
child-process entry point `TestQuotaWorkerProcess` and the two opt-in
`KAYA_LOAD=1` drills. No database-unavailable skip occurred.

The runner used its owned PostgreSQL cluster on loopback port 55489 and
explicit disposable `review_app` / `review_e2e` DSNs. Both databases
applied all 55 migrations and skipped all 55 on the idempotent rerun.
`cleanup.log` confirms `server stopped`. Its early exit prevented the
runner's subsequent E2E command from running.

The separate E2E attempt in
`/private/tmp/yunhou-review-20261109.3NuMWy/run.DAuAnw` exited 1 after
140 test/subtest passes: the package reached Go's default 10-minute
timeout while generating a test RSA key, without a reported test
assertion failure. Its independent PostgreSQL cluster on port 55490
was stopped by the runner. This attempt is incomplete, not an E2E pass.

#### 4.67.5 E2E retry and local commit checkpoint

The E2E retry completed with exit 0 in
`/private/tmp/yunhou-review-20261109.3NuMWy/run.jJtp92`:

```sh
env GOMAXPROCS=2 REVIEW_PG_PORT=55490 \
  zsh /private/tmp/yunhou-review-20261109.3NuMWy/run.zsh \
  ./tests/e2e/... -timeout=30m
```

The runner used `go test -race -count=1 -p 1 -json` and completed in
397.442 seconds, with 186 test/subtest passes and no failures. The only
skip was the opt-in `TestUsageTokenAcceptance_TenPagesTwoMinutes`
(`USAGE_ACCEPTANCE=1`); there were no database-unavailable skips.
No assertion or individual test deadline was changed. Both new,
independent disposable databases applied all 55 migrations and skipped
all 55 on the idempotent rerun. `cleanup.log` confirms `server stopped`.

The complete providers race suite was rerun before commit with
`GOMAXPROCS=2 GOPROXY=off GOFLAGS=-mod=readonly`; it passed in 2.349
seconds. `git diff --check` and gofmt checks were clean.

Task 8's scoped implementation and regression checkpoint is complete,
but the full-suite gate remains non-green because of the unresolved
relay failures in section 4.67.4. The 80.6% coverage figure belongs to
that non-green run. This local checkpoint does not resolve Task 9 or
the until-clean review, and performs no push, merge, deployment or
kaya process operation.

### 4.68 Interrupted Anthropic initial usage checkpoint (2026-11-09)

This local commit checkpoint addresses Task 9 only. It does not claim
that the whole-branch review has converged.

#### 4.68.1 Behavior and regression evidence

The Anthropic translator parsed `message_start.usage.output_tokens` but
never copied it into its merged usage state. An interrupted stream with
no later output counter therefore lost output usage in the authoritative
tap, raw usage and translated SSE. The fix retains that initial pointer,
including a reported zero. Later `message_delta` output counters remain
cumulative replacements, not increments; an absent counter stays unknown.

The provider regression covers EOF, read error, error followed by
`message_stop`, reported zero, absent output, later cumulative output and
a delta without output. All seven cases run with one-byte, 23-byte and
whole-stream reads. Before the fix, the initial-output cases lost their
tap/raw/wire counts. The complete providers race suite passed after the
fix (5.438 seconds, `GOMAXPROCS=2`, `GOPROXY=off`,
`GOFLAGS=-mod=readonly`).

The disposable-database RED run
`/private/tmp/yunhou-review-20261109.3NuMWy/run.RwICUb` reproduced a charge
of 25 instead of 59 for known input/output usage 25/17. The gateway
regression covers EOF and protocol error followed by stop. It asserts
estimated 25/17 usage, a failed interrupted attempt, settled TPM 42, one
charge of 59, one dispatch and no held money or concurrency leases.
A duplicate completion cannot change that settlement. The fixture
reports `EndUpstreamBroke` when no terminal marker is observed, matching
the existing relay contract.

The corrected gateway regression passed both endings in `run.q2GTJf`
(three test/subtest passes, 0.33 seconds). Independent E2E
`/private/tmp/yunhou-review-20261109.3NuMWy/run.U9pgJY` exited 0:
186 test/subtest passes, one expected `USAGE_ACCEPTANCE=1` opt-in skip,
no failures or database-unavailable skips. It ran with `GOMAXPROCS=2`,
`GOPROXY=off`, `GOFLAGS=-mod=readonly` and
`go test -race -count=1 -p 1 -json ./tests/e2e/... -timeout=30m`.
Its owned disposable PostgreSQL cluster used port 55490 and separate
`review_app`/`review_e2e` databases; all 55 migrations applied and skipped
all 55 on rerun. Cleanup confirmed server stopped.

The full current-worktree run
`/private/tmp/yunhou-review-20261109.3NuMWy/run.q2GTJf` exited 1.
Build and vet passed. The race command was
`go test -race -count=1 -p 1 -coverprofile=.../coverage.out -json ./internal/... ./cmd/... ./tests/integration/...`.
It recorded 4,424 test/subtest passes, seven failure events and three
expected skips (the quota child-process entry point and two `KAYA_LOAD=1`
opt-in drills); 32 packages passed and two failed. There were no
database-unavailable skips. Coverage was 80.6%, measured from this
non-green run, not a passing gate. Both disposable databases applied 55
migrations and skipped all 55 on rerun; cleanup stopped the owned cluster.

The failure events were `TestWSConnectionCaps` plus three Messages
integration subtests and their parents:
`TestAcceptance_BrokenStreamParksWithoutSilentZero`,
`TestAcceptance_ClientInterruptionSettlesAndReleases` and
`TestProtocols_BrokenStreamSequence`. Each integration fixture reported
initial `output_tokens:1`, but its assertion expected NULL. The missing
counter assignment had hidden that contradiction. The EOF acceptance
fixture now genuinely omits output, retaining the unknown-bucket guard;
client interruption and protocol error assertions retain the reported 1.
No additional production behavior was changed for these assertions.

The complete integration race recheck
`/private/tmp/yunhou-review-20261109.3NuMWy/run.KdvIFK` exited 0 after
those test corrections: 82 test/subtest passes, two expected opt-in load
drill skips, no failures or database-unavailable skips (143.433 seconds).
It used a new owned disposable PostgreSQL cluster on port 55490 with
separate explicit application/E2E database URLs, `GOMAXPROCS=2`,
`GOPROXY=off`, `GOFLAGS=-mod=readonly` and
`go test -race -count=1 -p 1 -json ./tests/integration/... -timeout=30m -coverprofile=.../task9-integration-final-coverage.out`.
Both databases applied all 55 migrations and skipped all 55 on rerun;
cleanup confirmed server stopped. This recheck clears the three stale
integration assertions; it does not turn the earlier full run into a
passing gate or complete the excluded relay work.

#### 4.68.2 Deliverable file manifest and remaining risks

| Files | Change |
|---|---|
| `internal/inference/providers/anthropic_stream.go` | Retain initial output usage without changing later cumulative replacement |
| `internal/inference/providers/anthropic_start_usage_test.go` | Fragmentation, interruption, known-zero/absent and cumulative-counter regressions |
| `internal/inference/gateway/anthropic_start_usage_test.go` | Database-backed exactly-once charge, usage, TPM and lease assertions |
| `tests/integration/gateway_acceptance_test.go`, `tests/integration/inference_protocols_test.go` | Align interrupted-stream fixtures and assertions with reported versus absent initial output |
| `docs/superpowers/plans/2026-11-09-model-gateway-review-convergence.md`, this runbook | Checkpoint scope, evidence, manifest and remaining risks |

The unfinished `internal/relay/conn_test.go` changes are deliberately
excluded from this commit and left intact in the worktree. Their owner
reported a passing targeted deterministic slow-consumer check, but the
connection-cap test still contains an intentionally delayed hello RED
reproduction and no production repair. These changes are not a completed
relay fix. Full-worktree verification must report that boundary.

Nonstream upstream-lease release ordering, cross-process rate acceptance,
the original wallet-correction boundary and the final independent branch
review remain separate unfinished work. Local simulated upstreams are
not real supplier acceptance.

## 4.69 Nonstream Finalization Lease Order (2026-11-09)

Task 10 closes the confirmed gap between nonstream finalization and the
original finalize-before-release invariant. A legal dispatch timeout just
below the 15m recovery grace can leave too little time for settlement;
this is not evidence that default 10m requests are immediately recoverable.

RED `run.caxKW6` (local artifacts under
`/private/tmp/yunhou-review-20261109.3NuMWy/`) reproduced 0/1 or 2/3 live
leases before finalization. With only an upstream lease, real recovery
settled conservatively at 16413 instead of the known reported charge 20.
The panic regression also observed only 2/3 leases before finalization.

The fix keeps the existing keeper renewing until nonstream finalization
ends and defers `Stop` with its own cleanup budget, matching stream
finalization. This covers success, exhausted settlement deadlines and
panic. Failed-dispatch retries retain their existing scoped-release path.

| File | Change |
|---|---|
| `internal/inference/gateway/service.go` | Keep renewal through finalization; remove early release and obsolete lease-ID split |
| `internal/inference/gateway/nonstream_finish_order_test.go` | Previously committed RED regressions: recovery exclusion, renewal, exact reported settlement/TPM and panic cleanup |
| `internal/inference/gateway/nonstream_order_test.go` | Check real `held` state instead of nonexistent `active` |
| `docs/superpowers/plans/2026-11-09-model-gateway-review-convergence.md`, this runbook | Progress, evidence and remaining verification boundaries |

Focused race `run.NYjMkE` exited 0: 20 test/subtest passes, no skips.
Full gateway race `run.2Pn4qs` exited 0: 142 passes, no skips/failures
(42.169s). Success asserts one charge of 20, reported usage, settled TPM
15, no reconciliation job and no held leases. Both runs used independent
owned disposable PostgreSQL clusters and stopped them on exit.

Whole-branch race coverage, E2E and final independent review remain open.
Lease cleanup is still best-effort under a database outage; fenced TTL
recovery remains the fallback. No production/provider acceptance is claimed.
No push, merge, deployment or kaya process operation.

## 4.70 Cross-Process Rate And Concurrency Acceptance (2026-11-09)

Task 11 extends the existing quota-worker re-exec harness to the rate and
concurrency paths. Two real OS processes, each with its own connection
pool, wait at a ready/start file barrier before admitting to one owned
disposable PostgreSQL database. Distinct worker PIDs are asserted.

The six independent bottlenecks are global account RPM, policy RPM,
Key RPM, account TPM, account concurrency and Key concurrency. Account
cases use sibling Keys; Key cases share a Key. Each case admits exactly
3 of 16 requests and rejects 13 with `rate_limited` or
`insufficient_capacity`. Persisted assertions check three requests,
window reservations of 90, twelve monetary holds (three windows and a
Key budget per request), six rate rows, expected concurrency leases and
no ledger entries. Rejected admissions leave no partial state.

Files changed: `internal/inference/postgres/quota_concurrency_test.go`,
`internal/inference/postgres/rate_process_test.go`, the original completion
plan, convergence plan and this runbook. No production behavior changes.
The worker adds typed rejection counters, optional synchronized startup
and bounded admission; the new harness reaps only its own child tests.

Local artifacts under `/private/tmp/yunhou-review-20261109.3NuMWy/`:

| Run | Command after disposable database setup | Result |
|---|---|---|
| `run.MhRoxm` | `go test -race -count=1 -p 1 -json ./internal/inference/postgres -run 'TestTwoProcessContend\|TestQuotaWorkerProcess' -timeout=5m` | Exit 0; 8 test/subtest passes, 12.642s |
| `run.16SlWX` | `go test -race -count=1 -p 1 -json ./internal/inference/postgres -coverprofile=... -timeout=10m` | Exit 0; 213 passes, 24.805s; package statement coverage 77.5% |

Both runs use `GOMAXPROCS=2` and explicit `DATABASE_URL` /
`E2E_DATABASE_URL`, apply all 55 migrations plus idempotent reruns and stop
their owned clusters on exit. Each has one expected skip: the parent
invocation of the child-only `TestQuotaWorkerProcess`. Its re-executed
children ran; the skip is not missing database acceptance.

This closes the cross-process rate-path evidence gap recorded in section
4.58 and original plan section 1.1.6 item 2. Historical checkpoint limits
remain historical. Cross-host, real-provider/client acceptance and the
wallet correction limitation remain open. Whole-branch race/coverage and
E2E have not been refreshed at this commit checkpoint; 77.5% is the
PostgreSQL package coverage, not the whole-branch 80% gate result.
No push, merge, deployment or kaya process operation.

### 4.71 Whole-branch Verification Refresh (2026-11-09)

Code under test: `814d120`. The latest user request is a local commit
checkpoint. All implementation changes were already committed and the
tracked worktree was clean before this documentation refresh.

Artifacts: `/private/tmp/yunhou-review-20261109.3NuMWy/run.3fEfYH`.
The existing runner creates its own PostgreSQL cluster on an unoccupied
port, with separate `review_app` and `review_e2e` databases. Both explicit
test DSNs point only to that cluster. Each database applied all 55
migrations, then skipped all 55 on its idempotent rerun.

Verification uses `GOMAXPROCS=2`, `GOPROXY=off`, `GOFLAGS=-mod=readonly`
and serial package execution:

| Check | Result |
|---|---|
| `go build ./...` | Passed; `build.log` |
| `go vet ./internal/... ./cmd/...` | Passed; `vet.log` |
| `go test -race -count=1 -p 1 -timeout=30m -coverprofile=... -json ./internal/... ./cmd/... ./tests/integration/...` | Passed: 4,442 tests/subtests, 34 packages, zero failures; `tests.jsonl` |
| `go tool cover -func=...` and 80% floor | Passed: 80.6% total statement coverage; `coverage.out`, `coverage-summary.log` |
| `go test -race -count=1 -p 1 -timeout=30m -json ./tests/e2e/...` | Passed: 186 tests/subtests, zero failures, 189.043s; `e2e.jsonl` |
| Owned PostgreSQL cleanup | Passed: `cleanup.log` confirms server stopped; runner exited 0 |

The three full-race skips are expected: parent invocation of the
child-only `TestQuotaWorkerProcess`, plus opt-in
`TestLoadDrill_Throughput` and `TestLoadDrill_LongConnections`.
There are no database-unavailable skips. The re-executed worker children
actually ran, including all six cross-process rate/concurrency modes.
The 30-minute package timeout only changes the test runner budget, not
assertions or individual test deadlines.

E2E has one expected skip: `TestUsageTokenAcceptance_TenPagesTwoMinutes`
requires the opt-in `USAGE_ACCEPTANCE=1` load scenario. No database
acceptance skipped. All four skips across the two suites are explicit
child-entry or opt-in cases, not hidden failures.

The original #12 findings are committed as `5f74ee8` (1/2, including
migration 055) and `1bec4c4` (3/4/5). This race run passes their key
regressions: old admission/recent settlement still rejects 90+20 against
TPM 100; equal-money estimated-to-reported corrections update usage and
TPM without zero-value financial entries; unrepresentable tool history
is explicitly rejected; explicit reasoning effort has consistent
precedence; `/chat` retains known Retry-After and omits unknown recovery.
The E2E `TestChatFacade_RateRetryAfter` also passed the complete HTTP
known-recovery/unknown-recovery path.

Independent bounded reviews by Noether and Tesla, including the Task
10/11 increments at `814d120`, report no confirmed unresolved important
defects in their respective reviewed scopes. These were read-only
reviews, not independent executions of this full gate.

Requirement audit retains the genuine original-plan gap in section
1.1.6 item 1: wallet post-settlement correction is explicitly rejected,
and the evidence-driven production correction trigger is not wired.
The named follow-up `inference-correction-trigger-and-wallet` remains
open; this checkpoint does not claim the entire original T4 delivered.
Cross-host, real-provider/client acceptance, Go 1.25 CI parity and the
documented raw Chat passthrough boundary remain unverified or unchanged.

Documentation changes are limited to this runbook, the original plan's
actual migration/interface inventory and the convergence plan's current
verification status. No business code, production configuration, adjacent
repository, remote Git state or kaya process is changed.

### 4.72 Correction Evidence Integrity Checkpoint (2026-11-09)

This local commit checkpoint follows the user's `commit` request. It
finishes two already reproduced correction-boundary defects; it does not
expand the review to a new settlement API or implement the wallet and
production-trigger follow-up.

**Changed files:**

- `internal/inference/postgres/reconciliation_repo.go`: verify that the
  corrected attempt belongs to the locked request before any write.
  When normalized token buckets are known, require their declared total
  to match input + cache read + cache write + output. Subtract buckets
  from the total rather than adding them, so int64 overflow cannot
  disguise inconsistent evidence. Reasoning is not counted twice.
- `internal/inference/postgres/correction_identity_test.go`: cover
  foreign-account attempts for monetary and metering-only corrections.
  Both requests' usage, settlement, TPM, ledger, windows and Key budgets
  stay unchanged; no receipt is consumed, and the right attempt can
  retry with the same idempotency key.
- `internal/inference/postgres/correction_evidence_test.go`: reject
  90-token evidence paired with total zero for paid and free models,
  and reject overflow evidence that would wrap to 1. The matching
  90-token retry succeeds, without adding reasoning again, and makes
  the next 20-token admission fail against TPM 100.
- `internal/inference/postgres/correction_rate_test.go`: keep the
  shared correction fixture's buckets consistent with its declared
  total; the nil-total fixture still carries known buckets for the
  existing missing-total rejection.
- `internal/inference/postgres/recovery_test.go`: correct the final
  overage-resolution fixture to declare 80 tokens for its 40/40 buckets.
  Monetary and reconciliation assertions are unchanged.
- This runbook and the convergence plan: record the findings,
  verification evidence and remaining scope.

**RED/GREEN evidence:**

All paths below are under
`/private/tmp/yunhou-review-20261109.3NuMWy/`.

| Run | Result |
|---|---|
| `run.rAVmUu/focused.jsonl` | RED: a foreign attempt changed the target request's money and TPM and appended mismatched usage; the metering-only variant returned `not_found` instead of `invalid_input`. |
| `run.BMonGO/focused.jsonl` | RED: both paid/free 90-token corrections accepted total zero and consumed a receipt; overflowing buckets were accepted with TPM 1. |
| `run.kBbcB3/focused.jsonl` | GREEN: `go test -race -count=1 -p 1 -json ./internal/inference/postgres -run '^TestCorrectSettlement' -timeout=5m`; 35 tests/subtests, zero failures or skips, 5.163s. |

**Fresh full gate:** `run.ZWo4XY` exited 0. It used
`GOMAXPROCS=2`, `GOPROXY=off`, `GOFLAGS=-mod=readonly`, explicit
`DATABASE_URL`/`E2E_DATABASE_URL` and serial package execution:

| Check | Result |
|---|---|
| `go build ./...` | Passed; `build.log` |
| `go vet ./internal/... ./cmd/...` | Passed; `vet.log` |
| `go test -race -count=1 -p 1 -timeout=30m -coverprofile=... -json ./internal/... ./cmd/... ./tests/integration/...` | Passed: 4,449 tests/subtests across 34 packages, zero failures; `tests.jsonl` |
| `go tool cover -func=...` and 80% floor | Passed: 80.6% total statement coverage; `coverage.out`, `coverage-summary.log` |
| `go test -race -count=1 -p 1 -timeout=30m -json ./tests/e2e/...` | Passed: 186 tests/subtests, zero failures, 237.008s; `e2e.jsonl` |
| Migrations | Both owned disposable databases applied 55 migrations and skipped all 55 on rerun; `migrate-app.log`, `migrate-e2e.log` |
| Owned PostgreSQL cleanup | Passed: `cleanup.log` confirms server stopped |

The full-race skips are the child-only `TestQuotaWorkerProcess` parent
invocation and the opt-in `TestLoadDrill_Throughput` and
`TestLoadDrill_LongConnections`. E2E skips only the opt-in
`TestUsageTokenAcceptance_TenPagesTwoMinutes`. Worker children actually
ran; no database-unavailable case skipped. Completed focused runs also
have cleanup logs confirming their owned PostgreSQL servers stopped.

McClintock's read-only bounded review found no confirmed Critical or
Important defects in the five Go files. It verified the ownership check
precedes writes, subtraction prevents overflow, reasoning is not added
again, and fixture changes preserve accounting assertions. This is not
a whole-repository clean-review claim or an independent gate execution.
Minor test gaps remain: the rollback helper does not independently
prove pre-write rejection, and there are no dedicated 90/91-total or
valid-MaxInt64 boundary cases. The current implementation was judged
correct for those paths; this checkpoint does not claim those extra
cases were tested.

**Remaining scope:** this patch has no migration and does not rewrite
historical rows. `CorrectSettlement` still has no production evidence
trigger and explicitly rejects wallet corrections. Those original-plan
requirements remain open. Initial `Settle`/shared usage-insert identity
validation has not been claimed covered by this narrow checkpoint.
Real-provider/client and cross-host acceptance remain unverified.
No push, merge, deployment, adjacent-repository change or kaya process
operation is part of this work.

### 4.73 Initial Settlement Identity Checkpoint (2026-11-09)

This local commit checkpoint follows the user's `commit` request and
completes the already reproduced initial-settlement ownership fix from
Task 13. It does not reopen the wider review or implement correction
triggers or wallet corrections.

**Changed files:**

- `internal/inference/postgres/ledger_repo.go`: require usage to identify
  the locked request; parse usage/cost attempt UUIDs before SQL and verify
  both attempts belong to that request. Validation precedes writes,
  plan/wallet dispatch and terminal duplicate classification. Distinct
  usage/cost attempts from the same request remain valid.
- `internal/inference/postgres/settlement_identity_test.go`: extend the
  existing RED regressions with empty/malformed UUIDs on both funding
  paths and distinct valid usage/cost attempts. Inspect the still-open
  transaction before retry, preserve the foreign request and attempt,
  verify one valid settlement, reject foreign duplicate evidence and
  retain typed duplicate classification for valid evidence.
- `internal/inference/postgres/inference_limit_test.go` and
  `internal/inference/postgres/recovery_test.go`: replay persisted usage
  in the TPM and zero-charge duplicate fixtures, rather than supplying
  a random, nonexistent attempt. Preserve the typed duplicate/unchanged
  TPM and single zero-charge assertions.
- This runbook and the convergence plan: record evidence and boundaries.

**RED/GREEN evidence:** paths are under
`/private/tmp/yunhou-review-20261109.3NuMWy/`.

| Run | Result |
|---|---|
| `run.RYh2s8/focused.jsonl` | RED: all 10 original plan/wallet leaf cases failed; foreign evidence was accepted, while a missing usage attempt poisoned the transaction. |
| `run.jvq0Tg/focused.jsonl` | GREEN: `go test -race -count=1 -p 1 -json ./internal/inference/postgres -run '^TestSettle_RejectsForeignEvidenceBeforeWriting$' -timeout=5m`; 21 tests/subtests, zero failures or skips, 4.890s. |
| `run.6Txzg7/tests.jsonl` | Non-green whole-branch attempt: build/vet passed; 4,468 tests/subtests passed, but `TestRateSettle_DuplicateSettlementDoesNotDoubleCountTheTokens` and `TestZeroChargeSettleAppendsChargeRow` failed because their duplicate fixtures used nonexistent attempts. The runner exited 1 before the coverage summary and E2E. No production validation was relaxed. |
| `run.NfcvZF/focused.jsonl` | GREEN after fixture corrections: identity, TPM duplicate and zero-charge duplicate race regressions; 23 tests/subtests, no failures or skips, 7.258s. |
| `run.N9MTPk/tests.jsonl` | Fresh whole-branch GREEN after all Go edits: build/vet passed; `go test -race -count=1 -p 1 -timeout=30m -coverprofile=... -json ./internal/... ./cmd/... ./tests/integration/...` passed 4,470 tests/subtests across 34 packages, zero failures. |
| `run.N9MTPk/coverage-summary.log` | Total statement coverage 80.6%; the 80% floor passed. |
| `run.N9MTPk/e2e.jsonl` | `go test -race -count=1 -p 1 -timeout=30m -json ./tests/e2e/...` passed 186 tests/subtests, zero failures, 236.625s. The runner exited 0. |

The GREEN run covers nine identity-error cases on each funding path.
Both owned disposable databases applied all 55 migrations and skipped
all 55 on rerun. Completed runs' cleanup logs confirm their owned
PostgreSQL servers stopped, including the final `run.N9MTPk`.
The full-race skips were `TestQuotaWorkerProcess` (child-process entry)
and `TestLoadDrill_Throughput` / `TestLoadDrill_LongConnections`
(`KAYA_LOAD=1` opt-in). E2E skipped only
`TestUsageTokenAcceptance_TenPagesTwoMinutes` (`USAGE_ACCEPTANCE=1`
opt-in). No database acceptance was skipped.

Euler's read-only review of the delta against `593946d` found no
confirmed Critical or Important defects and checked gateway/recovery
caller compatibility. Its follow-up review of both duplicate-fixture
corrections also found no Critical or Important defects or relevant
assertion weakening. It did not execute the verification gates.
Two nonblocking test gaps remain: the usage-request mismatch also
changes the usage attempt, and the pre-retry inspection does not
independently assert the legitimate cost attempt is unchanged.
The production checks precede both writes, but this checkpoint does
not claim isolated regression coverage for those two assertions.
The existing duplicate fixtures' rollback-before-state-check limitation
is unchanged.

**Scope rulings:** unchanged amount-only duplicate compatibility,
standalone usage insertion and correction behavior remain separate
review surfaces. The production correction-evidence trigger and wallet
corrections are still open requirements. This patch has no migration
or historical-data repair; real-provider/client and cross-host
acceptance remain unverified. No push, merge, deployment, adjacent
repository modification or kaya process operation is part of this work.

## 5. Open concerns

- The Go module proxy is unreachable from this environment; the cached module
  set is currently sufficient. A task that introduces a **new** external
  dependency cannot be verified here — report the blocker instead of
  weakening the verification. `GOPROXY=off` + `GOFLAGS=-mod=readonly` make
  that failure explicit rather than mutating `go.mod` / `go.sum`.
- `internal/inference/httpapi` takes ~52 s under `-race` and dominates the
  inference baseline runtime; budget for it when running the full suite.
- The coverage floor has thin headroom: T0 measures **80.4%** against a floor
  of 80% (§2.8). A task that adds code without tests can drop below the
  floor and fail CI even when every test passes. Re-run the floor check
  (gate 6 in §1.3) after adding packages.
- `tests/e2e` has the same silent-fallback hazard as the inference
  `TestMain`s: `tests/e2e/testhelpers.go` reads `E2E_DATABASE_URL` and falls
  back to `postgres://postgres@localhost/yunhou_users` when it is unset.
  Always export it explicitly.
- The CI runner pins Go **1.25** while the local toolchain is **1.27.1**.
  The `go.mod` `go` directive is the authority — do not bump it as a side
  effect of a task.
- `make ci-test` / `make ci-local` under-cover relative to `pr-ci.yml`
  because they omit `./tests/integration/...` (§1.3). Use the workflow
  commands, not the Makefile targets, to reproduce a merge gate.
- **The e2e job needs its own migrated database.** T10's first e2e run
  failed with `relation "inference_audit_log" does not exist` on **every**
  case because the migrations had been applied to `DATABASE_URL` only
  (§4.55). The `go-e2e` CI job has its own Postgres service and runs
  `go run ./cmd/migrate` as its first gate (§1.3); a local reproduction must
  do the same **against `E2E_DATABASE_URL`** — otherwise the failure looks
  like a product regression and is not one.
- **The acceptance evidence is a simulation, not a provider acceptance.**
  Every upstream in §4.52 is an `httptest` server; no real OpenAI /
  Anthropic / Moonshot deployment was billed and **no real Claude Code /
  Codex client was run** (§4.58, limits 1). The matrix proves the gateway's
  own accounting and limit semantics, not that a real supplier reports the
  usage the gateway assumes.
- **The recovery path bills a conservative estimate, not the real usage.**
  With no provider-side execution lookup configured, a request whose
  consumption is unknown settles at the admission-priced upper bound and
  waits for a human to confirm it (§4.58, limit 3). The unknown-usage
  backlog alarm fires at 10 parked requests; the backlog needs an operator,
  not a retry loop.
- **Cross-process rate-path acceptance is now covered; cross-host is not.**
  Section 4.70 adds two real worker processes and independent pools for
  account/policy/Key RPM, account TPM and account/Key concurrency. The full
  PostgreSQL race suite passed; section 4.71 also refreshes the whole-branch
  race/coverage and E2E gates successfully. Neither proves
  cross-host/provider/client behavior.
- **The wallet's post-settlement correction is still unwired — a real
  remaining limitation, not a plan exclusion.** The persistent rate
  reservation *is* settled/released on the wallet terminal and recovery
  paths (`settleWallet` → `settleRateTx`, `releaseRateTx`), but
  `CorrectSettlement` refuses the wallet path with `invalid_input`
  (`TestCorrectSettlementRejectsWallet`), so an estimated wallet settlement
  that later gets real evidence **cannot** be corrected. The original T4 text
  asks for plan **and** wallet to release-or-correct the same persistent rate
  reservation; this needs a **final review decision**. The "已知边界（本期
  不扩大）" note inside the plan's T4 section is branch-era text written by
  T4 — it is **not** a plan-level waiver and must not be cited as one
  (§4.58, limit 3).


---

## 6. Named follow-ups (post-delivery)

Named limitations and their delivery status are retained so they are not
silently dropped. Open items are **not** "done", and no plan-level waiver is
implied by their existence here. Closed review findings point to their evidence.

| # | Follow-up | Scope | Owner (role) | Schedule |
|---|---|---|---|---|
| 1 | `inference-correction-trigger-and-wallet` | (a) The **evidence-driven correction trigger**: who calls `Store.CorrectSettlement` / `Store.CorrectRateTokensTx` when real usage evidence lands (the recovery verifier path, or an operator-facing correction entry point) — today the money-correction channel has **no production caller**; (b) the **wallet money-correction semantics**: `CorrectSettlement` refuses wallet-charged requests with `invalid_input` (micromoney unit, wallet compensation entries and the token-budget sync are not adapted), so an estimated wallet settlement that later gets real evidence cannot be corrected. **No payment / wallet product change is implied** — the scope is the correction channel only. | Users 模型网关维护者 | 未排期 |
| 2 | `responses-history-order-preservation` (review #12 finding 3) | Representable history stays ordered; nonempty text after a call before results explicitly returns `invalid_input` in parse and replay. See section 4.62 for regressions and compatibility cost. | Users 模型网关维护者 | 2026-11-09 本轮已修 |
| 3 | `reasoning-parameter-conflict-policy` (review #12 finding 4) | Explicit reasoning effort takes precedence over the toggle in both OpenAI adapters; false/high remains high. See section 4.62 for the ingress and adapter matrix. | Users 模型网关维护者 | 2026-11-09 本轮已修 |
| 4 | `chat-rate-limit-retry-after` (review #12 finding 5) | Facade retains known local rate recovery; handler emits positive `Retry-After` and omits unknown recovery. See section 4.62 for real RPM/TPM E2E evidence. | Users 模型网关维护者 | 2026-11-09 本轮已修 |

The limitation wording in the plan (§1.1.6 item 1), the T4 branch-era note
"已知边界（本期不扩大）" and §4.58 limit 3 stay **as they are**: they are
accurate. The T4 note is branch-era text and is still **not** a plan-level
waiver.

**Why it is not a merge blocker.** The rate-side of the original T4 sentence is
satisfied for the wallet path (`settleWallet` → `settleRateTx`,
`releaseRateTx` on release); the missing pieces are the *trigger* and the
*money* semantics, both of which are pre-existing and outside this branch's
rate-gate deliverable. No money or rate fact produced by this branch is wrong
because of them: a recovery-settled request is charged the conservative
estimate and stays that way.
