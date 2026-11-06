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

