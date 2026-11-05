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

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T2 — Window-less settlement and conflict classification

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T3 — Provider disable, model state and sticky consistency

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T4 — PostgreSQL-backed RPM/TPM

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T5 — Per-key concurrency

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T6 — Protocol error shape and Retry-After

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T7 — Configuration-behaviour convergence and connect timeouts

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T8 — Capability routing and protocol truthfulness

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T9 — Probe, capability description and operational visibility

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T10 — Integration acceptance and implementation report

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

---

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

