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

